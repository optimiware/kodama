package app

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/app/bridges"
	natsdriver "github.com/optimiware/kodema/internal/shell/app/drivers/nats"
	pgdriver "github.com/optimiware/kodema/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	natsbroker "github.com/optimiware/kodema/internal/shell/integration/nats"
	"github.com/optimiware/kodema/internal/shell/integration/oidc"
	"github.com/optimiware/kodema/internal/shell/secret"
	"github.com/optimiware/kodema/internal/shell/service"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodema/internal/shell/store/units/identity"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/token"
)

type fixedValidityClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (clock *fixedValidityClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.now
}

func (clock *fixedValidityClock) Sleep(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}

type recordingStoreValidity struct {
	mu     sync.Mutex
	calls  int
	expiry time.Time
	err    error
}

func (validity *recordingStoreValidity) ValidUntil(_ context.Context, _ identity.Principal) (time.Time, error) {
	validity.mu.Lock()
	defer validity.mu.Unlock()
	validity.calls++
	if validity.err != nil {
		return time.Time{}, validity.err
	}
	return validity.expiry, nil
}

func (validity *recordingStoreValidity) callCount() int {
	validity.mu.Lock()
	defer validity.mu.Unlock()
	return validity.calls
}

// TestPersistentCredentialValidityUnit pins the unit behavior of
// persistentCredentialValidity: humans use the verified OIDC expiry and fail
// closed when missing, zero or expired; service and execution principals
// delegate to the store-backed reader.
func TestPersistentCredentialValidityUnit(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := &fixedValidityClock{now: now}
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}

	if _, err := newPersistentCredentialValidity(nil, clock); err == nil {
		t.Fatal("newPersistentCredentialValidity accepted nil store")
	}
	if _, err := newPersistentCredentialValidity(&recordingStoreValidity{}, nil); err == nil {
		t.Fatal("newPersistentCredentialValidity accepted nil clock")
	}

	t.Run("human_with_future_verified_expiry", func(t *testing.T) {
		storeValidity := &recordingStoreValidity{expiry: now.Add(24 * time.Hour)}
		validity, err := newPersistentCredentialValidity(storeValidity, clock)
		if err != nil {
			t.Fatalf("newPersistentCredentialValidity: %v", err)
		}
		want := now.Add(30 * time.Minute)
		human := identity.Principal{
			ID:      "human-1",
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
		}.WithVerifiedExpiry(want)

		got, err := validity.ValidUntil(context.Background(), human)
		if err != nil {
			t.Fatalf("ValidUntil(human): %v", err)
		}
		if !got.Equal(want) {
			t.Fatalf("ValidUntil(human) = %v; want %v", got, want)
		}
		if storeValidity.callCount() != 0 {
			t.Fatalf("store validity was consulted %d times for a human principal; want 0", storeValidity.callCount())
		}
	})

	t.Run("human_missing_or_expired_fails_closed", func(t *testing.T) {
		storeValidity := &recordingStoreValidity{expiry: now.Add(24 * time.Hour)}
		validity, err := newPersistentCredentialValidity(storeValidity, clock)
		if err != nil {
			t.Fatalf("newPersistentCredentialValidity: %v", err)
		}
		base := identity.Principal{
			ID:      "human-1",
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
		}
		cases := []struct {
			name      string
			principal identity.Principal
		}{
			{"missing_expiry", base},
			{"at_now_boundary", base.WithVerifiedExpiry(now)},
			{"past_expiry", base.WithVerifiedExpiry(now.Add(-time.Second))},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := validity.ValidUntil(context.Background(), tc.principal)
				if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
					t.Fatalf("ValidUntil(%s) err = %v (class=%v); want unauthenticated", tc.name, err, class)
				}
			})
		}
		if storeValidity.callCount() != 0 {
			t.Fatalf("store validity was consulted %d times for human failures; want 0", storeValidity.callCount())
		}
	})

	t.Run("service_and_execution_delegate_to_store", func(t *testing.T) {
		storeExp := now.Add(2 * time.Hour)
		storeValidity := &recordingStoreValidity{expiry: storeExp}
		validity, err := newPersistentCredentialValidity(storeValidity, clock)
		if err != nil {
			t.Fatalf("newPersistentCredentialValidity: %v", err)
		}

		svc := identity.Principal{
			ID:      "service-1",
			Kind:    identity.KindService,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
		}
		got, err := validity.ValidUntil(context.Background(), svc)
		if err != nil || !got.Equal(storeExp) {
			t.Fatalf("ValidUntil(service) = %v, %v; want %v, nil", got, err, storeExp)
		}

		exec := identity.Principal{
			ID:          "exec-principal-1",
			Kind:        identity.KindExecution,
			Scope:       scope,
			Enabled:     true,
			ExecutionID: "exec-1",
		}
		got, err = validity.ValidUntil(context.Background(), exec)
		if err != nil || !got.Equal(storeExp) {
			t.Fatalf("ValidUntil(execution) = %v, %v; want %v, nil", got, err, storeExp)
		}
		if storeValidity.callCount() != 2 {
			t.Fatalf("store validity calls = %d; want 2", storeValidity.callCount())
		}
	})

	t.Run("canceled_context_fails_closed", func(t *testing.T) {
		validity, err := newPersistentCredentialValidity(&recordingStoreValidity{}, clock)
		if err != nil {
			t.Fatalf("newPersistentCredentialValidity: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		human := identity.Principal{
			ID:      "human-1",
			Kind:    identity.KindHuman,
			Scope:   scope,
			Enabled: true,
		}.WithVerifiedExpiry(now.Add(time.Hour))
		_, err = validity.ValidUntil(ctx, human)
		if class, ok := fault.ClassOf(err); !ok || class != fault.DeadlineExpired {
			t.Fatalf("ValidUntil(canceled) err = %v (class=%v); want deadline_expired", err, class)
		}
	})
}

// oidcTestFixture is a hermetic RS256 OIDC issuer serving one static JWKS over
// an in-memory RoundTripper so tests exercise the real oidc.Verifier without
// network sockets.
type oidcTestFixture struct {
	issuer   string
	audience string
	kid      string
	key      *rsa.PrivateKey
}

func newOIDCTestFixture(t *testing.T) *oidcTestFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return &oidcTestFixture{
		issuer:   "https://issuer.test.kodema.invalid",
		audience: "kodema",
		kid:      "rsa-key-1",
		key:      key,
	}
}

func (fixture *oidcTestFixture) newVerifier(t *testing.T, clock Clock, skew time.Duration) identity.OIDCVerifier {
	t.Helper()
	jwksBody, err := json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": oidc.AlgorithmRS256,
			"kid": fixture.kid,
			"n":   base64.RawURLEncoding.EncodeToString(fixture.key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fixture.key.E)).Bytes()),
		}},
	})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(jwksBody))),
			Request:    request,
		}, nil
	})
	verifier, err := oidc.NewVerifier(oidc.Config{
		OIDCConfig: identity.OIDCConfig{
			Issuer:    fixture.issuer,
			Audience:  fixture.audience,
			ClockSkew: skew,
		},
		JWKSURL:       fixture.issuer + "/.well-known/jwks.json",
		Algorithms:    []string{oidc.AlgorithmRS256},
		HTTPClient:    &http.Client{Transport: transport},
		MaxJWKSBytes:  1 << 20,
		MaxTokenBytes: 1 << 20,
		TenantClaim:   "tenant",
		ProjectClaim:  "project",
		Clock:         clock.Now,
	})
	if err != nil {
		t.Fatalf("oidc.NewVerifier: %v", err)
	}
	return verifier
}

func (fixture *oidcTestFixture) signToken(t *testing.T, subject string, scope tenancy.Scope, now, exp time.Time) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]any{
		"alg": oidc.AlgorithmRS256,
		"typ": "JWT",
		"kid": fixture.kid,
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claims := map[string]any{
		"iss":    fixture.issuer,
		"aud":    fixture.audience,
		"sub":    subject,
		"tenant": string(scope.TenantID),
		"iat":    now.Add(-time.Minute).Unix(),
		"exp":    exp.Unix(),
	}
	if scope.ProjectID != "" {
		claims["project"] = string(scope.ProjectID)
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, fixture.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("rsa.SignPKCS1v15: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type staticDerivationKey struct {
	key []byte
}

func (resolver staticDerivationKey) ResolveDerivationKey(context.Context, token.TenantKey) ([]byte, error) {
	return append([]byte(nil), resolver.key...), nil
}

type noopDeliverer struct{}

func (noopDeliverer) Deliver(context.Context, token.Delivery) error { return nil }

type noopInvalidator struct{}

func (noopInvalidator) Invalidate(context.Context, token.Invalidation) error { return nil }

func seedPersistentSubscribeFixture(
	t *testing.T,
	clock *fixedValidityClock,
	mem *memory.Store,
	scope tenancy.Scope,
	tenantAdminID id.PrincipalID,
	projectHumanID id.PrincipalID,
	serviceID service.ServiceID,
	serviceNonce []byte,
	serviceExpiry time.Time,
) []byte {
	t.Helper()
	ctx := context.Background()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}

	derivationKey := make([]byte, 32)
	if _, err := rand.Read(derivationKey); err != nil {
		t.Fatalf("rand derivation key: %v", err)
	}
	encodedKey, err := json.Marshal(token.TenantKey{
		TenantID:   scope.TenantID,
		Version:    1,
		Ciphertext: []byte("sealed-derivation-key"),
		WrappedKey: make([]byte, secret.WrappedKeyBytes),
		WrapNonce:  make([]byte, secret.WrapNonceBytes),
		KeyRef:     "kek-v1",
		KeyVersion: uint64(secret.MinKeyVersion),
	})
	if err != nil {
		t.Fatalf("marshal tenant key: %v", err)
	}
	adminRoles, err := json.Marshal([]tenancy.Role{tenancy.RoleAdmin})
	if err != nil {
		t.Fatalf("marshal admin roles: %v", err)
	}
	engineerRoles, err := json.Marshal([]tenancy.Role{tenancy.RoleEngineer})
	if err != nil {
		t.Fatalf("marshal engineer roles: %v", err)
	}
	if _, err := mem.Execute(ctx, tenantScope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "tenants",
				ID:     string(scope.TenantID),
				Insert: true,
				Values: map[string]any{
					"name":           "Tenant One",
					"version":        uint64(1),
					"derivation_key": encodedKey,
					"created_at":     clock.Now(),
				},
			},
			{
				Table:  "projects",
				ID:     string(scope.ProjectID),
				Insert: true,
				Values: map[string]any{
					"name":       "Project One",
					"version":    uint64(1),
					"created_at": clock.Now(),
				},
			},
			{
				Table:  "memberships",
				ID:     "mem-tenant-admin",
				Insert: true,
				Values: map[string]any{
					"principal_id": string(tenantAdminID),
					"roles":        adminRoles,
					"version":      uint64(1),
					"created_at":   clock.Now(),
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed tenant/project/admin membership: %v", err)
	}

	if _, err := mem.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "memberships",
				ID:     "mem-project-engineer",
				Insert: true,
				Values: map[string]any{
					"principal_id": string(projectHumanID),
					"roles":        engineerRoles,
					"version":      uint64(1),
					"created_at":   clock.Now(),
				},
			},
			{
				Table:  "services",
				ID:     string(serviceID),
				Insert: true,
				Values: map[string]any{
					"state":            string(service.StateEnabled),
					"roles":            engineerRoles,
					"version":          uint64(1),
					"token_nonce":      append([]byte(nil), serviceNonce...),
					"token_expires_at": serviceExpiry,
					"created_at":       clock.Now(),
				},
			},
		},
		Events: []event.Envelope{{
			ID:        "evt-seeded-1",
			Type:      event.TypeCustomObservation,
			Source:    event.Source{Kind: event.SourceService, PrincipalID: id.PrincipalID(serviceID)},
			At:        clock.Now(),
			Scope:     scope,
			SubjectID: "subject-1",
		}},
	}); err != nil {
		t.Fatalf("seed project membership/service/event: %v", err)
	}
	return derivationKey
}

// TestPersistentEventSubscribeAndCalloutHonorHumanOIDCExpiry wires the real
// oidc.Verifier, token.Resolver, readers.CredentialValidity,
// persistentCredentialValidity, event.Subscriber and natsbroker.Callout
// together and proves:
//   - Human OIDC tokens (both tenant-wide admin and project-scoped engineer)
//     succeed on event.subscribe with ExpiresAt equal to the verified JWT exp.
//   - Human OIDC tokens verified inside ClockSkew whose exp <= now fail closed
//     on both event.subscribe and NATS auth callout.
//   - Human OIDC tokens succeed on NATS auth callout with publish denied and
//     user JWT exp bounded by min(oidcExp, now + grantLifetime).
//   - Service credentials continue to read authoritative token_expires_at from
//     the store in both event.subscribe and NATS auth callout.
func TestPersistentEventSubscribeAndCalloutHonorHumanOIDCExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := &fixedValidityClock{now: now}
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	tenantScope := tenancy.Scope{TenantID: "tenant-1"}

	oidcFixture := newOIDCTestFixture(t)
	verifier := oidcFixture.newVerifier(t, clock, 5*time.Second)

	tenantAdminID, err := oidc.PrincipalIDFor(oidcFixture.issuer, "auth0|admin-1")
	if err != nil {
		t.Fatalf("PrincipalIDFor admin: %v", err)
	}
	projectHumanID, err := oidc.PrincipalIDFor(oidcFixture.issuer, "auth0|engineer-1")
	if err != nil {
		t.Fatalf("PrincipalIDFor engineer: %v", err)
	}

	serviceID := service.ServiceID("svc-subscriber-1")
	serviceNonce := make([]byte, token.MinNonceBytes)
	if _, err := rand.Read(serviceNonce); err != nil {
		t.Fatalf("rand service nonce: %v", err)
	}
	serviceExpiry := now.Add(90 * time.Minute)

	mem := memory.NewWithClock(clock.Now)
	derivationKey := seedPersistentSubscribeFixture(t, clock, mem, scope, tenantAdminID, projectHumanID, serviceID, serviceNonce, serviceExpiry)

	identityAdapter, err := identitystore.New(identitystore.Config{
		Executor: mem,
		Scanner:  mem,
		Clock:    clock,
		IDs:      bridges.NewSecureIDs(),
	})
	if err != nil {
		t.Fatalf("identitystore.New: %v", err)
	}
	receiptLookup, err := readers.NewReceiptLookup(mem)
	if err != nil {
		t.Fatalf("readers.NewReceiptLookup: %v", err)
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          identityAdapter,
		Resolver:      staticDerivationKey{key: derivationKey},
		Credentials:   identityAdapter,
		Principals:    identityAdapter,
		Services:      identityAdapter,
		Mutations:     identityAdapter,
		Receipts:      receiptLookup,
		Deliverer:     noopDeliverer{},
		Invalidations: noopInvalidator{},
		Clock:         clock,
		Random:        rand.Reader,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	resolver, err := token.NewResolver(token.ResolverConfig{
		Lookup:      mem,
		Credentials: manager,
		OIDC:        verifier,
		Principals:  identityAdapter,
		Clock:       clock,
	})
	if err != nil {
		t.Fatalf("token.NewResolver: %v", err)
	}
	storeValidity, err := readers.NewCredentialValidity(mem)
	if err != nil {
		t.Fatalf("readers.NewCredentialValidity: %v", err)
	}
	validity, err := newPersistentCredentialValidity(storeValidity, clock)
	if err != nil {
		t.Fatalf("newPersistentCredentialValidity: %v", err)
	}
	cursorReader, err := readers.NewCursorReader(mem)
	if err != nil {
		t.Fatalf("readers.NewCursorReader: %v", err)
	}
	subscriber, err := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Cursor:     cursorReader,
		Validity:   validity,
		Clock:      clock,
	})
	if err != nil {
		t.Fatalf("event.NewSubscriber: %v", err)
	}

	ctx := context.Background()
	wantSubject := "kodema.v1.tenant-1.project-1.events"

	t.Run("tenant_wide_human_oidc_token_subscribes_to_project", func(t *testing.T) {
		exp := now.Add(25 * time.Minute)
		jwt := oidcFixture.signToken(t, "auth0|admin-1", tenantScope, now, exp)
		principal, err := resolver.Resolve(ctx, jwt)
		if err != nil {
			t.Fatalf("Resolve(admin OIDC): %v", err)
		}
		grant, err := subscriber.Setup(ctx, event.SubscribeCommand{
			Actor: principal,
			Scope: scope,
			Setup: event.SubscribeSetup{Scope: scope},
		})
		if err != nil {
			t.Fatalf("event.subscribe(admin OIDC): %v", err)
		}
		if !slices.Equal(grant.Subjects, []string{wantSubject}) {
			t.Fatalf("subjects = %v; want [%s]", grant.Subjects, wantSubject)
		}
		if grant.Cursor != 1 {
			t.Fatalf("cursor = %d; want 1", grant.Cursor)
		}
		if !grant.ExpiresAt.Equal(exp) {
			t.Fatalf("expires_at = %v; want JWT exp %v", grant.ExpiresAt, exp)
		}
	})

	t.Run("project_scoped_human_oidc_token_subscribes_to_project", func(t *testing.T) {
		exp := now.Add(15 * time.Minute)
		jwt := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, exp)
		principal, err := resolver.Resolve(ctx, jwt)
		if err != nil {
			t.Fatalf("Resolve(engineer OIDC): %v", err)
		}
		grant, err := subscriber.Setup(ctx, event.SubscribeCommand{
			Actor: principal,
			Scope: scope,
			Setup: event.SubscribeSetup{Scope: scope},
		})
		if err != nil {
			t.Fatalf("event.subscribe(engineer OIDC): %v", err)
		}
		if !slices.Equal(grant.Subjects, []string{wantSubject}) {
			t.Fatalf("subjects = %v; want [%s]", grant.Subjects, wantSubject)
		}
		if !grant.ExpiresAt.Equal(exp) {
			t.Fatalf("expires_at = %v; want JWT exp %v", grant.ExpiresAt, exp)
		}
	})

	t.Run("human_without_verified_expiry_fails_closed", func(t *testing.T) {
		forged := identity.Principal{
			ID:      projectHumanID,
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		}
		_, err := subscriber.Setup(ctx, event.SubscribeCommand{
			Actor: forged,
			Scope: scope,
			Setup: event.SubscribeSetup{Scope: scope},
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("event.subscribe(missing verified expiry) err = %v (class=%v); want unauthenticated", err, class)
		}
	})

	t.Run("human_exp_within_skew_but_not_after_now_fails_closed", func(t *testing.T) {
		// Token exp is 1s in the past: Verify accepts it because ClockSkew=5s,
		// yet persistentCredentialValidity must refuse because !exp.After(now).
		skewExp := now.Add(-time.Second)
		jwt := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, skewExp)
		principal, err := resolver.Resolve(ctx, jwt)
		if err != nil {
			t.Fatalf("Resolve(skew token): %v", err)
		}
		_, err = subscriber.Setup(ctx, event.SubscribeCommand{
			Actor: principal,
			Scope: scope,
			Setup: event.SubscribeSetup{Scope: scope},
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("event.subscribe(exp<=now) err = %v (class=%v); want unauthenticated", err, class)
		}
	})

	t.Run("service_credential_reads_store_expiry", func(t *testing.T) {
		derived, err := token.DeriveServiceToken(derivationKey, token.ServiceClaims{
			TenantID:  scope.TenantID,
			ServiceID: serviceID,
			Nonce:     serviceNonce,
			Expiry:    serviceExpiry,
		})
		if err != nil {
			t.Fatalf("DeriveServiceToken: %v", err)
		}
		principal, err := resolver.Resolve(ctx, derived)
		if err != nil {
			t.Fatalf("Resolve(service): %v", err)
		}
		grant, err := subscriber.Setup(ctx, event.SubscribeCommand{
			Actor: principal,
			Scope: scope,
			Setup: event.SubscribeSetup{Scope: scope},
		})
		if err != nil {
			t.Fatalf("event.subscribe(service): %v", err)
		}
		if !grant.ExpiresAt.Equal(serviceExpiry) {
			t.Fatalf("service expires_at = %v; want store token_expires_at %v", grant.ExpiresAt, serviceExpiry)
		}
	})

	t.Run("execution_credential_reads_store_expiry", func(t *testing.T) {
		execExpiry := now.Add(40 * time.Minute)
		execNonce := make([]byte, token.MinNonceBytes)
		if _, err := rand.Read(execNonce); err != nil {
			t.Fatalf("rand execution nonce: %v", err)
		}
		if _, err := mem.Execute(ctx, scope, store.Unit{
			Writes: []store.Write{{
				Table:  "executions",
				ID:     "exec-validity-1",
				Insert: true,
				Values: map[string]any{
					"pipeline_id":      nil,
					"task_id":          nil,
					"principal_id":     "exec-principal-1",
					"state":            "running",
					"version":          int64(1),
					"yaml_sha256":      []byte("sha256"),
					"snapshot":         []byte(`{}`),
					"token_nonce":      execNonce,
					"token_expires_at": execExpiry,
					"workspace_id":     "ws-validity-1",
					"created_at":       clock.Now(),
				},
			}},
		}); err != nil {
			t.Fatalf("seed execution: %v", err)
		}
		execPrincipal := identity.Principal{
			ID:          "exec-principal-1",
			Kind:        identity.KindExecution,
			Scope:       scope,
			Enabled:     true,
			Version:     1,
			ExecutionID: "exec-validity-1",
		}
		got, err := validity.ValidUntil(ctx, execPrincipal)
		if err != nil {
			t.Fatalf("ValidUntil(execution): %v", err)
		}
		if !got.Equal(execExpiry) {
			t.Fatalf("execution ValidUntil = %v; want store token_expires_at %v", got, execExpiry)
		}
	})

	t.Run("nats_callout_honors_human_oidc_expiry_and_service_store_expiry", func(t *testing.T) {
		issuerSeed := testNKeyAccountSeed(t)
		conn := &testCalloutConnection{}
		callout, err := natsbroker.NewCallout(natsbroker.CalloutConfig{
			Connection: conn,
			IssuerSeed: issuerSeed,
			Account:    "KODEMA",
			Principal:  resolver,
			Authorizer: authorize.NewMatrix(),
			Validity:   validity,
			Clock:      clock,
		})
		if err != nil {
			t.Fatalf("natsbroker.NewCallout: %v", err)
		}
		if err := callout.Start(ctx); err != nil {
			t.Fatalf("callout.Start: %v", err)
		}
		t.Cleanup(func() { _ = callout.Close(context.Background()) })

		// 1. Human OIDC token with exp shorter than DefaultGrantLifetime (60s)
		// keeps its exact JWT exp and receives read-only scope permissions.
		shortExp := now.Add(25 * time.Second)
		shortJWT := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, shortExp)
		userClaims := invokeCalloutExpectUserJWT(t, conn, now, shortJWT)
		if got := int64(userClaims["exp"].(float64)); got != shortExp.Unix() {
			t.Fatalf("human short user jwt exp = %d; want %d", got, shortExp.Unix())
		}
		natsClaims := userClaims["nats"].(map[string]any)
		subAllow := natsClaims["sub"].(map[string]any)["allow"].([]any)
		if !slices.Contains(subAllow, any(wantSubject)) {
			t.Fatalf("human user jwt sub allow = %v; want %s", subAllow, wantSubject)
		}
		pubDeny := natsClaims["pub"].(map[string]any)["deny"].([]any)
		if !slices.Contains(pubDeny, any(">")) {
			t.Fatalf("human user jwt pub deny = %v; want explicit deny of '>'", pubDeny)
		}

		// 2. Human OIDC token with exp longer than DefaultGrantLifetime is
		// capped at now + DefaultGrantLifetime.
		longExp := now.Add(10 * time.Minute)
		longJWT := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, longExp)
		longClaims := invokeCalloutExpectUserJWT(t, conn, now, longJWT)
		wantCapped := now.Add(natsbroker.DefaultGrantLifetime).Unix()
		if got := int64(longClaims["exp"].(float64)); got != wantCapped {
			t.Fatalf("human capped user jwt exp = %d; want %d", got, wantCapped)
		}

		// 3. Human OIDC token inside ClockSkew with exp <= now is refused by
		// the callout (consistent with event.subscribe).
		skewJWT := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, now.Add(-time.Second))
		invokeCalloutExpectError(t, conn, now, skewJWT)

		// 4. Service credential continues to work through the same callout
		// using PostgreSQL-backed token_expires_at.
		serviceToken, err := token.DeriveServiceToken(derivationKey, token.ServiceClaims{
			TenantID:  scope.TenantID,
			ServiceID: serviceID,
			Nonce:     serviceNonce,
			Expiry:    serviceExpiry,
		})
		if err != nil {
			t.Fatalf("DeriveServiceToken: %v", err)
		}
		svcClaims := invokeCalloutExpectUserJWT(t, conn, now, serviceToken)
		if got := int64(svcClaims["exp"].(float64)); got != wantCapped {
			t.Fatalf("service capped user jwt exp = %d; want %d", got, wantCapped)
		}
	})
}

// TestPersistentProfilesWireComposedValidityAndCallout proves both the
// production profile and the durable test profile install
// persistentCredentialValidity on environment.validity and wire it into the
// NATS auth callout when configured.
func TestPersistentProfilesWireComposedValidityAndCallout(t *testing.T) {
	now := time.Now().UTC()
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	futureExp := now.Add(30 * time.Minute).Truncate(time.Second)
	human := identity.Principal{
		ID:      "human-1",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
	}.WithVerifiedExpiry(futureExp)

	checkProfile := func(t *testing.T, name string, build func(t *testing.T, dial natsDial) Config) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			previousPool := newPostgresPool
			newPostgresPool = func(config pgdriver.Config) (postgresPool, error) {
				return &recordingPool{dsn: config.ConnString}, nil
			}
			t.Cleanup(func() { newPostgresPool = previousPool })

			calloutServer := &fakeCalloutNATSServer{}
			config := build(t, calloutServer.dial)
			config.NATSAuth = &NATSAuthConfig{
				User:     "kodema-publisher",
				Password: "publisher-password",
				Callout: &NATSCalloutConfig{
					User:       "kodema-callout",
					Password:   "callout-password",
					IssuerSeed: testNKeyAccountSeed(t),
					Account:    "KODEMA",
				},
			}

			builder := &builder{cfg: config, logger: config.logger()}
			environment, err := buildEnvironment(context.Background(), builder)
			if err != nil {
				t.Fatalf("buildEnvironment(%s): %v", name, err)
			}
			t.Cleanup(func() { builder.cleanup(context.Background()) })

			if _, ok := environment.validity.(*persistentCredentialValidity); !ok {
				t.Fatalf("%s environment.validity = %T; want *persistentCredentialValidity", name, environment.validity)
			}
			got, err := environment.validity.ValidUntil(context.Background(), human)
			if err != nil {
				t.Fatalf("%s ValidUntil(human): %v", name, err)
			}
			if !got.Equal(futureExp) {
				t.Fatalf("%s ValidUntil(human) = %v; want %v", name, got, futureExp)
			}
			if _, err := environment.validity.ValidUntil(context.Background(), human.WithVerifiedExpiry(time.Time{})); err == nil {
				t.Fatalf("%s ValidUntil(human without expiry) unexpectedly succeeded", name)
			}
			if !calloutServer.subscribedTo(natsbroker.CalloutSubject) {
				t.Fatalf("%s did not subscribe the NATS auth callout to %s", name, natsbroker.CalloutSubject)
			}
		})
	}

	checkProfile(t, "production", func(t *testing.T, dial natsDial) Config {
		config := validProductionConfig(t)
		config.Production.dialNATS = dial
		return config
	})
	checkProfile(t, "durable_test", func(t *testing.T, dial natsDial) Config {
		server := &fakeNATSServer{}
		config := durableTestConfig(t, server)
		config.Test.dialNATS = dial
		return config
	})
}

type testCalloutConnection struct {
	mu      sync.Mutex
	subject string
	handler func(context.Context, natsbroker.CalloutRequest) error
	replies [][]byte
}

func (conn *testCalloutConnection) SubscribeCallout(_ context.Context, subject string, handler func(context.Context, natsbroker.CalloutRequest) error) (natsbroker.Subscription, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.subject = subject
	conn.handler = handler
	return conn, nil
}

func (conn *testCalloutConnection) PublishCallout(_ context.Context, _ string, data []byte) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.replies = append(conn.replies, append([]byte(nil), data...))
	return nil
}

func (conn *testCalloutConnection) Close(context.Context) error { return nil }

func (conn *testCalloutConnection) deliver(t *testing.T, now time.Time, credential string) map[string]any {
	t.Helper()
	conn.mu.Lock()
	handler := conn.handler
	before := len(conn.replies)
	conn.mu.Unlock()
	if handler == nil {
		t.Fatal("callout handler was not registered")
	}
	reqJWT := buildTestCalloutRequestJWT(t, now, credential)
	if err := handler(context.Background(), natsbroker.CalloutRequest{
		Subject: natsbroker.CalloutSubject,
		Reply:   "_INBOX.reply",
		Data:    []byte(reqJWT),
	}); err != nil {
		t.Fatalf("callout handler returned error: %v", err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.replies) != before+1 {
		t.Fatalf("callout published %d replies; want 1", len(conn.replies)-before)
	}
	return decodeTestJWTClaims(t, string(conn.replies[len(conn.replies)-1]))
}

func invokeCalloutExpectUserJWT(t *testing.T, conn *testCalloutConnection, now time.Time, credential string) map[string]any {
	t.Helper()
	resp := conn.deliver(t, now, credential)
	natsClaims := resp["nats"].(map[string]any)
	inner, ok := natsClaims["jwt"].(string)
	if !ok || inner == "" {
		t.Fatalf("callout response had no user jwt: %v", natsClaims)
	}
	return decodeTestJWTClaims(t, inner)
}

func invokeCalloutExpectError(t *testing.T, conn *testCalloutConnection, now time.Time, credential string) {
	t.Helper()
	resp := conn.deliver(t, now, credential)
	natsClaims := resp["nats"].(map[string]any)
	if natsClaims["error"] == nil || natsClaims["jwt"] != nil {
		t.Fatalf("callout response = %v; want explicit error and no user jwt", natsClaims)
	}
}

func buildTestCalloutRequestJWT(t *testing.T, now time.Time, credential string) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"typ": "JWT", "alg": "ed25519-nkey"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claims, err := json.Marshal(map[string]any{
		"aud": "nats-authorization-request",
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
		"iss": "NSERVERKEYTEST1234567890",
		"sub": "nats_user_auth_request",
		"nats": map[string]any{
			"user_nkey": "UUSERKEYTEST1234567890",
			"server_id": map[string]any{"id": "NSERVERKEYTEST1234567890"},
			"connect_opts": map[string]any{
				"auth_token": credential,
			},
			"type":    "authorization_request",
			"version": 2,
		},
	})
	if err != nil {
		t.Fatalf("marshal callout request claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func decodeTestJWTClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts; want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal jwt claims: %v", err)
	}
	return claims
}

func testNKeyAccountSeed(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	const (
		prefixSeed    byte = 18 << 3
		prefixAccount byte = 0
	)
	seedBytes := priv.Seed()
	payload := make([]byte, 0, 2+len(seedBytes)+2)
	payload = append(payload, prefixSeed|(prefixAccount>>5), (prefixAccount&31)<<3)
	payload = append(payload, seedBytes...)
	var crc uint16
	for _, value := range payload {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	var checksum [2]byte
	binary.LittleEndian.PutUint16(checksum[:], crc)
	payload = append(payload, checksum[:]...)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(payload)
}

type fakeCalloutNATSServer struct {
	fakeNATSServer
	calloutMu       sync.Mutex
	calloutSubjects []string
}

func (server *fakeCalloutNATSServer) dial(ctx context.Context, config natsdriver.Config) (natsClient, error) {
	base, err := server.fakeNATSServer.dial(ctx, config)
	if err != nil {
		return nil, err
	}
	return &fakeCalloutNATSClient{fakeNATSClient: base.(*fakeNATSClient), owner: server}, nil
}

func (server *fakeCalloutNATSServer) subscribedTo(subject string) bool {
	server.calloutMu.Lock()
	defer server.calloutMu.Unlock()
	return slices.Contains(server.calloutSubjects, subject)
}

type fakeCalloutNATSClient struct {
	*fakeNATSClient
	owner *fakeCalloutNATSServer
}

func (client *fakeCalloutNATSClient) SubscribeCallout(_ context.Context, subject string, _ func(context.Context, natsbroker.CalloutRequest) error) (natsbroker.Subscription, error) {
	client.owner.calloutMu.Lock()
	client.owner.calloutSubjects = append(client.owner.calloutSubjects, subject)
	client.owner.calloutMu.Unlock()
	return client, nil
}

func (client *fakeCalloutNATSClient) PublishCallout(context.Context, string, []byte) error {
	return nil
}
