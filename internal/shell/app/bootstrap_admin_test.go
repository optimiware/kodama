package app

// Guarded first-admin bootstrap tests.
//
// The bootstrap is a one-shot composition helper: it reads one OIDC access
// token from a file, verifies it through the identity.OIDCVerifier seam,
// derives the principal with oidc.PrincipalIDFor (the verifier's derivation),
// mints the initial encrypted tenant derivation key, and commits the tenant,
// key, ADMIN-only tenant-wide membership, audit row and config.changed event
// intent as ONE store.Unit. The tenant's unique insert is the one-shot guard:
// every existing tenant is refused, including one whose last Admin was revoked.
//
// The tests below pin that contract. They are deliberately written against the
// public surface (NewBootstrapAdmin/Run) and the pure unit builder.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

const (
	bootstrapTestIssuer      = "https://issuer.example"
	bootstrapTestAudience    = "kodama-api"
	bootstrapTestTenantClaim = "https://kodama/tenant"
	bootstrapTestSubject     = "auth0|bootstrap-admin"
	bootstrapTestTenant      = id.TenantID("tenant-bootstrap")
	bootstrapTestKeyID       = "bootstrap-test-key"
)

var bootstrapTestNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// bootstrapTestIDs is a deterministic BootstrapAdminIDs. The unit builder
// allocates the membership row id first, then the audit event id, then the
// config event id; every test pins those identities.
type bootstrapTestIDs struct {
	mu       sync.Mutex
	rowID    string
	eventIDs []id.EventID
	next     int
}

func (ids *bootstrapTestIDs) NewRowID() (string, error) {
	if ids.rowID == "" {
		return "", errors.New("bootstrap test id source has no row id")
	}
	return ids.rowID, nil
}

func (ids *bootstrapTestIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.next >= len(ids.eventIDs) {
		return "", errors.New("bootstrap test id source has no event id")
	}
	value := ids.eventIDs[ids.next]
	ids.next++
	return value, nil
}

// bootstrapRecordingVerifier is a fake identity.OIDCVerifier that records the
// exact request the bootstrap presented and returns one prepared principal.
type bootstrapRecordingVerifier struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	token     string
	now       time.Time
}

func (verifier *bootstrapRecordingVerifier) Verify(_ context.Context, request identity.OIDCRequest) (identity.Principal, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.calls++
	verifier.token = request.AccessToken
	verifier.now = request.Now
	return verifier.principal, verifier.err
}

// bootstrapFailingCipher fails every seal, carrying a marker that must never be
// rendered by a public error.
type bootstrapFailingCipher struct {
	marker string
}

func (cipher bootstrapFailingCipher) Seal(context.Context, secret.AssociatedData, []byte) (secret.Envelope, error) {
	return secret.Envelope{}, errors.New(cipher.marker)
}

func (cipher bootstrapFailingCipher) Open(context.Context, secret.AssociatedData, secret.Envelope) ([]byte, error) {
	return nil, errors.New(cipher.marker)
}

// bootstrapRoundTrip serves the fixed test JWKS without any network.
type bootstrapRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip bootstrapRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// bootstrapOIDCFixture builds the real oidc.Verifier with an injected ECDSA
// key and a fixed JWKS. It proves the bootstrap consumes the real
// verifier (issuer, audience, tenant claim, signature and oidc.PrincipalIDFor),
// not a token-claim shortcut.
type bootstrapOIDCFixture struct {
	verifier    identity.OIDCVerifier
	key         *ecdsa.PrivateKey
	issuer      string
	audience    string
	tenantClaim string
}

func newBootstrapOIDCFixture(t *testing.T, issuer, audience string) *bootstrapOIDCFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate OIDC test key: %v", err)
	}
	jwks, err := json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "EC",
			"kid": bootstrapTestKeyID,
			"use": "sig",
			"alg": oidc.AlgorithmES256,
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
			"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
		}},
	})
	if err != nil {
		t.Fatalf("encode OIDC test JWKS: %v", err)
	}
	client := &http.Client{Transport: bootstrapRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(jwks))),
		}, nil
	})}
	verifier, err := oidc.NewVerifier(oidc.Config{
		Issuer: issuer, Audience: audience,
		JWKSURL:       "https://issuer.example/.well-known/jwks.json",
		Algorithms:    []string{oidc.AlgorithmES256},
		HTTPClient:    client,
		MaxJWKSBytes:  1 << 20,
		MaxTokenBytes: 1 << 20,
		CacheTTL:      time.Hour,
		Clock:         func() time.Time { return bootstrapTestNow },
		SubjectClaim:  "sub",
		TenantClaim:   bootstrapTestTenantClaim,
	})
	if err != nil {
		t.Fatalf("oidc.NewVerifier: %v", err)
	}
	return &bootstrapOIDCFixture{
		verifier:    verifier,
		key:         key,
		issuer:      issuer,
		audience:    audience,
		tenantClaim: bootstrapTestTenantClaim,
	}
}

// token signs one access token. The caller can override any claim to exercise
// an issuer, audience or tenant mismatch.
func (fixture *bootstrapOIDCFixture) token(t *testing.T, issuer, audience, subject string, tenant id.TenantID) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{
		"alg": oidc.AlgorithmES256,
		"typ": "JWT",
		"kid": bootstrapTestKeyID,
	})
	if err != nil {
		t.Fatalf("encode token header: %v", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss":               issuer,
		"aud":               audience,
		"sub":               subject,
		"exp":               bootstrapTestNow.Add(time.Hour).Unix(),
		"nbf":               bootstrapTestNow.Add(-time.Minute).Unix(),
		"iat":               bootstrapTestNow.Add(-time.Minute).Unix(),
		fixture.tenantClaim: string(tenant),
	})
	if err != nil {
		t.Fatalf("encode token claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// bootstrapHumanPrincipal is the verified human identity a fake verifier
// returns. Its id is derived exactly as the OIDC adapter derives it.
func bootstrapHumanPrincipal(t *testing.T, tenant id.TenantID, issuer, subject string) identity.Principal {
	t.Helper()
	principalID, err := oidc.PrincipalIDFor(issuer, subject)
	if err != nil {
		t.Fatalf("oidc.PrincipalIDFor: %v", err)
	}
	return identity.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: tenant},
		Enabled: true,
	}
}

func bootstrapTestConfig(tenant id.TenantID, tokenFile string) BootstrapAdminConfig {
	return BootstrapAdminConfig{
		InstallationID: "installation-bootstrap",
		TenantID:       tenant,
		TenantName:     "Bootstrap Tenant",
		TokenFile:      tokenFile,
		KeyVersion:     secret.MinKeyVersion,
	}
}

func writeBootstrapToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access-token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

// writeBootstrapAdminRawToken writes the exact bytes of a token file, without
// the trailing newline helper, so the size bound can be exercised at and past
// its edge.
func writeBootstrapAdminRawToken(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access-token")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write raw token file: %v", err)
	}
	return path
}

// bootstrapInertCipher seals without consulting the context, so a mutant that
// skips the post-verify cancellation check really reaches the commit unit
// instead of stopping at the key mint.
type bootstrapInertCipher struct{}

func (bootstrapInertCipher) Seal(context.Context, secret.AssociatedData, []byte) (secret.Envelope, error) {
	return secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     "test:inert",
		KeyVersion: secret.MinKeyVersion,
		Nonce:      make([]byte, secret.ContentNonceBytes),
		Ciphertext: []byte("ciphertext"),
		WrappedKey: make([]byte, secret.WrappedKeyBytes),
		WrapNonce:  make([]byte, secret.WrapNonceBytes),
	}, nil
}

func (bootstrapInertCipher) Open(context.Context, secret.AssociatedData, secret.Envelope) ([]byte, error) {
	return nil, errors.New("inert cipher cannot open")
}

// bootstrapCancellingVerifier cancels the run's context and still returns a
// valid principal, proving the bootstrap does not act on a verification whose
// context was already cancelled.
type bootstrapCancellingVerifier struct {
	cancel    context.CancelFunc
	principal identity.Principal
}

func (verifier bootstrapCancellingVerifier) Verify(context.Context, identity.OIDCRequest) (identity.Principal, error) {
	verifier.cancel()
	return verifier.principal, nil
}

// bootstrapCountingExecutor records whether the single commit unit was reached.
type bootstrapCountingExecutor struct {
	inner store.Executor
	mu    sync.Mutex
	calls int
}

func (executor *bootstrapCountingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.mu.Lock()
	executor.calls++
	executor.mu.Unlock()
	return executor.inner.Execute(ctx, scope, unit)
}

func (executor *bootstrapCountingExecutor) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error) {
	return executor.inner.Read(ctx, scope, table, id)
}

func (executor *bootstrapCountingExecutor) commitCalls() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.calls
}

func newBootstrapForTest(
	t *testing.T,
	executor store.Executor,
	cipher secret.EnvelopeCipher,
	verifier identity.OIDCVerifier,
	ids BootstrapAdminIDs,
	clock Clock,
	config BootstrapAdminConfig,
) *BootstrapAdmin {
	t.Helper()
	bootstrap, err := NewBootstrapAdmin(config, BootstrapAdminDependencies{
		Executor: executor,
		IDs:      ids,
		Cipher:   cipher,
		Verifier: verifier,
		Clock:    clock,
	})
	if err != nil {
		t.Fatalf("NewBootstrapAdmin: %v", err)
	}
	if bootstrap == nil {
		t.Fatal("NewBootstrapAdmin returned nil")
	}
	return bootstrap
}

func mustExecuteBootstrapUnit(t *testing.T, executor store.Executor, scope tenancy.Scope, unit store.Unit) {
	t.Helper()
	if _, err := executor.Execute(context.Background(), scope, unit); err != nil {
		t.Fatalf("seed commit unit: %v", err)
	}
}

func bootstrapMembershipRows(t *testing.T, memoryStore *memory.Store, tenant id.TenantID, principal id.PrincipalID) []store.Row {
	t.Helper()
	page, err := memoryStore.Query(context.Background(), tenancy.Scope{TenantID: tenant}, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("query memberships: %v", err)
	}
	return page.Rows
}

func bootstrapAllMembershipRows(t *testing.T, memoryStore *memory.Store, tenant id.TenantID) []store.Row {
	t.Helper()
	page, err := memoryStore.Query(context.Background(), tenancy.Scope{TenantID: tenant}, store.ScanQuery{
		Table:   "memberships",
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("query memberships: %v", err)
	}
	return page.Rows
}

func bootstrapRowVersion(t *testing.T, row store.Row) uint64 {
	t.Helper()
	switch value := row.Values["version"].(type) {
	case int64:
		return uint64(value)
	case uint64:
		return value
	case int:
		return uint64(value)
	default:
		t.Fatalf("row version has unexpected type %T (%v)", row.Values["version"], row.Values["version"])
		return 0
	}
}

func bootstrapRoles(t *testing.T, row store.Row) []tenancy.Role {
	t.Helper()
	raw, ok := row.Values["roles"].([]byte)
	if !ok {
		t.Fatalf("membership roles are %T; want stored JSON bytes", row.Values["roles"])
	}
	var roles []tenancy.Role
	if err := json.Unmarshal(raw, &roles); err != nil {
		t.Fatalf("decode membership roles: %v", err)
	}
	return roles
}

func bootstrapTenantKey(t *testing.T, row store.Row) token.TenantKey {
	t.Helper()
	raw, ok := row.Values["derivation_key"].([]byte)
	if !ok {
		t.Fatalf("derivation_key is %T; want stored JSON bytes", row.Values["derivation_key"])
	}
	var key token.TenantKey
	if err := json.Unmarshal(raw, &key); err != nil {
		t.Fatalf("decode derivation key: %v", err)
	}
	return key
}

func openBootstrapTenantKey(t *testing.T, cipher secret.EnvelopeCipher, installationID string, key token.TenantKey) ([]byte, error) {
	t.Helper()
	if len(key.Ciphertext) <= secret.ContentNonceBytes {
		t.Fatalf("tenant key ciphertext is %d bytes; want nonce plus ciphertext", len(key.Ciphertext))
	}
	envelope := secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef(key.KeyRef),
		KeyVersion: secret.KeyVersion(key.KeyVersion),
		Nonce:      append([]byte(nil), key.Ciphertext[:secret.ContentNonceBytes]...),
		Ciphertext: append([]byte(nil), key.Ciphertext[secret.ContentNonceBytes:]...),
		WrappedKey: append([]byte(nil), key.WrappedKey...),
		WrapNonce:  append([]byte(nil), key.WrapNonce...),
	}
	associated := secret.AssociatedData{
		InstallationID: installationID,
		TenantID:       key.TenantID,
		SecretID:       secret.SecretID("tenant-derivation-key"),
		Name:           id.SecretName("TENANT_DERIVATION_KEY"),
		KeyVersion:     secret.KeyVersion(key.KeyVersion),
	}
	return cipher.Open(context.Background(), associated, envelope)
}

func assertBootstrapNoWrites(t *testing.T, memoryStore *memory.Store, tenant id.TenantID) {
	t.Helper()
	ctx := context.Background()
	scope := tenancy.Scope{TenantID: tenant}
	if _, err := memoryStore.Read(ctx, scope, "tenants", string(tenant)); !isNotFound(err) {
		t.Fatalf("tenant row read = %v; want not_found (no bootstrap write)", err)
	}
	if rows := bootstrapAllMembershipRows(t, memoryStore, tenant); len(rows) != 0 {
		t.Fatalf("bootstrap left %d membership rows; want none", len(rows))
	}
	if audits := memoryStore.Audit(scope); len(audits) != 0 {
		t.Fatalf("bootstrap left %d audit rows; want none", len(audits))
	}
	if events := memoryStore.Events(scope); len(events) != 0 {
		t.Fatalf("bootstrap left %d event rows; want none", len(events))
	}
}

// TestBootstrapAdminRequiresVerifiedExplicitIdentity proves the bootstrap
// refuses every unverified or wrongly scoped identity before any write, and
// that a real verified token derives the membership principal with the OIDC
// adapter's PrincipalIDFor.
func TestBootstrapAdminRequiresVerifiedExplicitIdentity(t *testing.T) {
	t.Run("verified token derives the OIDC principal", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		fixture := newBootstrapOIDCFixture(t, bootstrapTestIssuer, bootstrapTestAudience)
		tokenValue := fixture.token(t, bootstrapTestIssuer, bootstrapTestAudience, bootstrapTestSubject, bootstrapTestTenant)
		tokenFile := writeBootstrapToken(t, tokenValue)
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, fixture.verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		result, err := bootstrap.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		wantPrincipal, err := oidc.PrincipalIDFor(bootstrapTestIssuer, bootstrapTestSubject)
		if err != nil {
			t.Fatalf("oidc.PrincipalIDFor: %v", err)
		}
		if result.PrincipalID != wantPrincipal {
			t.Fatalf("bootstrap principal = %q; want OIDC-derived %q", result.PrincipalID, wantPrincipal)
		}
		rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, wantPrincipal)
		if len(rows) != 1 {
			t.Fatalf("memberships for the verified principal = %d; want exactly 1", len(rows))
		}
	})

	t.Run("token file content reaches the verifier", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "token-from-the-file")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		if _, err := bootstrap.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if verifier.calls != 1 {
			t.Fatalf("verifier calls = %d; want exactly 1", verifier.calls)
		}
		if verifier.token != "token-from-the-file" {
			t.Fatalf("verified token = %q; want the trimmed file content", verifier.token)
		}
		if !verifier.now.Equal(clock.Now()) {
			t.Fatalf("verification time = %s; want the injected clock %s", verifier.now, clock.Now())
		}
	})

	t.Run("wrong issuer", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		fixture := newBootstrapOIDCFixture(t, bootstrapTestIssuer, bootstrapTestAudience)
		tokenValue := fixture.token(t, "https://other-issuer.example", bootstrapTestAudience, bootstrapTestSubject, bootstrapTestTenant)
		tokenFile := writeBootstrapToken(t, tokenValue)
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, fixture.verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a token from another issuer")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("wrong-issuer error class = %v (ok=%v); want unauthenticated", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("wrong audience", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		fixture := newBootstrapOIDCFixture(t, bootstrapTestIssuer, bootstrapTestAudience)
		tokenValue := fixture.token(t, bootstrapTestIssuer, "other-audience", bootstrapTestSubject, bootstrapTestTenant)
		tokenFile := writeBootstrapToken(t, tokenValue)
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, fixture.verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a token for another audience")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("wrong-audience error class = %v (ok=%v); want unauthenticated", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("wrong tenant", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		fixture := newBootstrapOIDCFixture(t, bootstrapTestIssuer, bootstrapTestAudience)
		tokenValue := fixture.token(t, bootstrapTestIssuer, bootstrapTestAudience, bootstrapTestSubject, id.TenantID("tenant-other"))
		tokenFile := writeBootstrapToken(t, tokenValue)
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, fixture.verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a token for another tenant")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("wrong-tenant error class = %v (ok=%v); want forbidden", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("service principal", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{principal: identity.Principal{
			ID: "service-1", Kind: identity.KindService,
			Scope: tenancy.Scope{TenantID: bootstrapTestTenant}, Enabled: true,
		}}
		tokenFile := writeBootstrapToken(t, "service-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a service principal")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("service-principal error class = %v (ok=%v); want forbidden", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("disabled principal", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		principal.Enabled = false
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "disabled-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a disabled principal")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("disabled-principal error class = %v (ok=%v); want unauthenticated", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("project-scoped principal", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		principal.Scope.ProjectID = "project-1"
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "project-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a project-scoped principal")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("project-scoped error class = %v (ok=%v); want forbidden", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("missing token file", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{principal: bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, filepath.Join(t.TempDir(), "absent-token")))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a missing token file")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("missing-token error class = %v (ok=%v); want invalid_input", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("empty token file", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{principal: bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)}
		tokenFile := writeBootstrapToken(t, "   ")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted an empty token file")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("empty-token error class = %v (ok=%v); want invalid_input", class, ok)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("token file content is trimmed", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapAdminRawToken(t, []byte(" \t padded-token \r\n"))
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		if _, err := bootstrap.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if verifier.token != "padded-token" {
			t.Fatalf("verified token = %q; want the trimmed file content", verifier.token)
		}
	})

	t.Run("token file at the token bound", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		// Exactly the verifier's bound: the trailing newline is trimmed, so the
		// presented token fits the bound.
		tokenFile := writeBootstrapAdminRawToken(t, []byte(strings.Repeat("a", bootstrapAdminMaxTokenBytes-1)+"\n"))
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		if _, err := bootstrap.Run(context.Background()); err != nil {
			t.Fatalf("Run with a token file at the bound: %v", err)
		}
		if len(verifier.token) != bootstrapAdminMaxTokenBytes-1 {
			t.Fatalf("verified token length = %d; want %d", len(verifier.token), bootstrapAdminMaxTokenBytes-1)
		}
	})

	t.Run("token file beyond the token bound", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		const marker = "oversized-token-material-must-not-leak"
		raw := marker + strings.Repeat("a", bootstrapAdminMaxTokenBytes+1-len(marker))
		tokenFile := writeBootstrapAdminRawToken(t, []byte(raw))
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a token file beyond the token bound")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("oversized-token error class = %v (ok=%v); want invalid_input", class, ok)
		}
		if verifier.calls != 0 {
			t.Fatalf("verifier calls = %d; want none for an oversized token file", verifier.calls)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("oversized-token error rendered the file content: %v", err)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("token file is a FIFO", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		fifo := filepath.Join(t.TempDir(), "access-token-fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatalf("create FIFO token file: %v", err)
		}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, fifo))

		done := make(chan error, 1)
		go func() {
			_, err := bootstrap.Run(context.Background())
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("bootstrap accepted a FIFO token file")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("FIFO-token error class = %v (ok=%v); want invalid_input", class, ok)
			}
		case <-time.After(5 * time.Second):
			// Unblock a reader that is stuck in open so a mutant run does not
			// leak a goroutine past the timeout.
			go func() {
				writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err == nil {
					writer.Close()
				}
			}()
			t.Fatal("bootstrap blocked reading a FIFO token file")
		}
		if verifier.calls != 0 {
			t.Fatalf("verifier calls = %d; want none for a FIFO token file", verifier.calls)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("token file is a directory", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{principal: bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)}
		directory := filepath.Join(t.TempDir(), "access-token-directory")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create token directory: %v", err)
		}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, directory))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a directory token file")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("directory-token error class = %v (ok=%v); want invalid_input", class, ok)
		}
		if verifier.calls != 0 {
			t.Fatalf("verifier calls = %d; want none for a directory token file", verifier.calls)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("token file is a device", func(t *testing.T) {
		if _, err := os.Stat("/dev/null"); err != nil {
			t.Skip("no /dev/null on this host")
		}
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{principal: bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, "/dev/null"))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a device token file")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("device-token error class = %v (ok=%v); want invalid_input", class, ok)
		}
		if verifier.calls != 0 {
			t.Fatalf("verifier calls = %d; want none for a device token file", verifier.calls)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("token file is a symlink to a regular file", func(t *testing.T) {
		// Kubernetes Secret volumes are a relative symlink chain, so a regular
		// file reached through symlinks must still be accepted.
		directory := t.TempDir()
		dataDirectory := filepath.Join(directory, "..2026_11_07_00_00_00")
		if err := os.Mkdir(dataDirectory, 0o700); err != nil {
			t.Fatalf("create secret data directory: %v", err)
		}
		target := filepath.Join(dataDirectory, "access-token")
		if err := os.WriteFile(target, []byte("symlinked-token\n"), 0o600); err != nil {
			t.Fatalf("write symlink target: %v", err)
		}
		if err := os.Symlink(filepath.Base(dataDirectory), filepath.Join(directory, "..data")); err != nil {
			t.Fatalf("create ..data symlink: %v", err)
		}
		symlink := filepath.Join(directory, "access-token")
		if err := os.Symlink(filepath.Join("..data", "access-token"), symlink); err != nil {
			t.Fatalf("create token symlink: %v", err)
		}

		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, symlink))

		if _, err := bootstrap.Run(context.Background()); err != nil {
			t.Fatalf("Run with a symlinked token file: %v", err)
		}
		if verifier.token != "symlinked-token" {
			t.Fatalf("verified token = %q; want the symlink target content", verifier.token)
		}
	})

	t.Run("malformed verified principal id", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		principal.ID = id.PrincipalID("malformed principal id")
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "malformed-principal-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap accepted a verified principal with a malformed id")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("malformed-principal error class = %v (ok=%v); want unauthenticated", class, ok)
		}
		if verifier.calls != 1 {
			t.Fatalf("verifier calls = %d; want exactly 1", verifier.calls)
		}
		if strings.Contains(err.Error(), "malformed principal id") {
			t.Fatalf("malformed-principal error rendered the invalid id: %v", err)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("explicit installation and tenant configuration", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*BootstrapAdminConfig)
		}{
			{"missing installation", func(config *BootstrapAdminConfig) { config.InstallationID = "" }},
			{"missing tenant", func(config *BootstrapAdminConfig) { config.TenantID = "" }},
			{"missing tenant name", func(config *BootstrapAdminConfig) { config.TenantName = "" }},
			{"missing token file", func(config *BootstrapAdminConfig) { config.TokenFile = "" }},
			{"zero key version", func(config *BootstrapAdminConfig) { config.KeyVersion = 0 }},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				config := bootstrapTestConfig(bootstrapTestTenant, "/tmp/token")
				testCase.mutate(&config)
				_, err := NewBootstrapAdmin(config, BootstrapAdminDependencies{
					Executor: memory.New(),
					IDs:      &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}},
					Cipher:   newTestCipher(t),
					Verifier: &bootstrapRecordingVerifier{},
					Clock:    newFakeClock(),
				})
				if err == nil {
					t.Fatal("NewBootstrapAdmin accepted an incomplete configuration")
				}
				if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
					t.Fatalf("configuration error class = %v (ok=%v); want invalid_input", class, ok)
				}
			})
		}
	})

	t.Run("missing dependencies", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*BootstrapAdminDependencies)
		}{
			{"missing executor", func(deps *BootstrapAdminDependencies) { deps.Executor = nil }},
			{"missing ids", func(deps *BootstrapAdminDependencies) { deps.IDs = nil }},
			{"missing cipher", func(deps *BootstrapAdminDependencies) { deps.Cipher = nil }},
			{"missing verifier", func(deps *BootstrapAdminDependencies) { deps.Verifier = nil }},
			{"missing clock", func(deps *BootstrapAdminDependencies) { deps.Clock = nil }},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				deps := BootstrapAdminDependencies{
					Executor: memory.New(),
					IDs:      &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}},
					Cipher:   newTestCipher(t),
					Verifier: &bootstrapRecordingVerifier{},
					Clock:    newFakeClock(),
				}
				testCase.mutate(&deps)
				_, err := NewBootstrapAdmin(bootstrapTestConfig(bootstrapTestTenant, "/tmp/token"), deps)
				if err == nil {
					t.Fatal("NewBootstrapAdmin accepted an incomplete dependency set")
				}
				if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
					t.Fatalf("dependency error class = %v (ok=%v); want invalid_input", class, ok)
				}
			})
		}
	})
}

// TestBootstrapAdminGrantsOnlyAdmin proves the single membership is exactly the
// tenant-wide ADMIN grant and that the derivation key is stored encrypted and
// bound to the installation and tenant identity.
func TestBootstrapAdminGrantsOnlyAdmin(t *testing.T) {
	clock := newFakeClock()
	memoryStore := memory.NewWithClock(clock.Now)
	cipher := newTestCipher(t)
	principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
	verifier := &bootstrapRecordingVerifier{principal: principal}
	tokenFile := writeBootstrapToken(t, "admin-token")
	ids := &bootstrapTestIDs{
		rowID:    "row-bootstrap-admin",
		eventIDs: []id.EventID{"event-bootstrap-audit", "event-bootstrap-config"},
	}
	config := bootstrapTestConfig(bootstrapTestTenant, tokenFile)
	bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock, config)

	result, err := bootstrap.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.TenantID != bootstrapTestTenant {
		t.Fatalf("result tenant = %q; want %q", result.TenantID, bootstrapTestTenant)
	}
	if result.PrincipalID != principal.ID {
		t.Fatalf("result principal = %q; want %q", result.PrincipalID, principal.ID)
	}
	if result.MembershipID != "row-bootstrap-admin" {
		t.Fatalf("result membership = %q; want the allocated row id", result.MembershipID)
	}
	if result.AuditID != "event-bootstrap-audit" || result.EventID != "event-bootstrap-config" {
		t.Fatalf("result audit/event = %q/%q; want the allocated event ids", result.AuditID, result.EventID)
	}

	scope := tenancy.Scope{TenantID: bootstrapTestTenant}
	tenantRow, err := memoryStore.Read(context.Background(), scope, "tenants", string(bootstrapTestTenant))
	if err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	if tenantRow.Values["name"] != config.TenantName {
		t.Fatalf("tenant name = %v; want %q", tenantRow.Values["name"], config.TenantName)
	}
	if version := bootstrapRowVersion(t, tenantRow); version != 1 {
		t.Fatalf("tenant version = %d; want 1", version)
	}
	key := bootstrapTenantKey(t, tenantRow)
	if key.TenantID != bootstrapTestTenant || key.Version != 1 {
		t.Fatalf("tenant key identity = %q/%d; want %q/1", key.TenantID, key.Version, bootstrapTestTenant)
	}
	if key.KeyRef == "" || key.KeyVersion != uint64(secret.MinKeyVersion) {
		t.Fatalf("tenant key provider identity = %q/%d; want a key reference at version %d", key.KeyRef, key.KeyVersion, secret.MinKeyVersion)
	}
	plaintext, err := openBootstrapTenantKey(t, cipher, config.InstallationID, key)
	if err != nil {
		t.Fatalf("open stored derivation key: %v", err)
	}
	if len(plaintext) != 32 {
		t.Fatalf("derivation key plaintext = %d bytes; want 32", len(plaintext))
	}
	for name, plaintextValue := range map[string]string{"ciphertext": string(key.Ciphertext), "wrapped": string(key.WrappedKey)} {
		if strings.Contains(plaintextValue, string(plaintext)) {
			t.Fatalf("stored %s contains the plaintext derivation key", name)
		}
	}
	if _, err := openBootstrapTenantKey(t, cipher, "other-installation", key); err == nil {
		t.Fatal("derivation key opened under a different installation id")
	}
	wrongTenant := key
	wrongTenant.TenantID = "tenant-other"
	if _, err := openBootstrapTenantKey(t, cipher, config.InstallationID, wrongTenant); err == nil {
		t.Fatal("derivation key opened under a different tenant id")
	}

	rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, principal.ID)
	if len(rows) != 1 {
		t.Fatalf("memberships for the bootstrap principal = %d; want exactly 1", len(rows))
	}
	roles := bootstrapRoles(t, rows[0])
	if !reflect.DeepEqual(roles, []tenancy.Role{tenancy.RoleAdmin}) {
		t.Fatalf("bootstrap roles = %v; want exactly [admin]", roles)
	}
	if rows[0].Values["project_id"] != nil {
		t.Fatalf("bootstrap membership project_id = %v; want tenant-wide NULL", rows[0].Values["project_id"])
	}
	if version := bootstrapRowVersion(t, rows[0]); version != 1 {
		t.Fatalf("membership version = %d; want 1", version)
	}

	audits := memoryStore.Audit(scope)
	if len(audits) != 1 {
		t.Fatalf("audit rows = %d; want exactly 1", len(audits))
	}
	if audits[0].ID != result.AuditID || audits[0].Actor != principal.ID ||
		audits[0].Operation != BootstrapAdminOperation || !audits[0].At.Equal(clock.Now()) {
		t.Fatalf("audit row = %+v; want the bootstrap audit shape", audits[0])
	}

	events := memoryStore.Events(scope)
	if len(events) != 1 {
		t.Fatalf("event rows = %d; want exactly 1", len(events))
	}
	envelope := events[0].Envelope
	if envelope.ID != result.EventID || envelope.Type != event.TypeConfigChanged {
		t.Fatalf("event identity/type = %q/%q; want %q/config.changed", envelope.ID, envelope.Type, result.EventID)
	}
	if envelope.Source.Kind != event.SourceShell || envelope.Source.PrincipalID != principal.ID {
		t.Fatalf("event source = %+v; want shell/%q", envelope.Source, principal.ID)
	}
	if envelope.Scope != scope || envelope.SubjectID != string(bootstrapTestTenant) || envelope.SubjectVersion != 1 {
		t.Fatalf("event scope/subject = %+v/%q/%d; want %+v/%q/1", envelope.Scope, envelope.SubjectID, envelope.SubjectVersion, scope, bootstrapTestTenant)
	}
	if !envelope.At.Equal(clock.Now()) {
		t.Fatalf("event time = %s; want %s", envelope.At, clock.Now())
	}
}

// TestBootstrapAdminRefusesExistingTenant proves the unique tenant insert is
// the one-shot guard: every existing tenant is refused, including a tenant
// whose last Admin was revoked. Bootstrap is not recovery.
func TestBootstrapAdminRefusesExistingTenant(t *testing.T) {
	t.Run("existing tenant without an admin", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		scope := tenancy.Scope{TenantID: bootstrapTestTenant}
		mustExecuteBootstrapUnit(t, memoryStore, scope, store.Unit{Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(bootstrapTestTenant),
			Insert: true,
			Values: map[string]any{
				"name":           "Existing Tenant",
				"version":        int64(1),
				"derivation_key": []byte(`{"tenant_id":"tenant-bootstrap","version":1,"ciphertext":"AA==","wrapped_key":"AA==","wrap_nonce":"AA==","key_ref":"test:key","key_version":1}`),
				"created_at":     clock.Now(),
			},
		}}})

		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "existing-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		if _, err := bootstrap.Run(context.Background()); err == nil {
			t.Fatal("bootstrap succeeded for an existing tenant")
		}
		if _, err := memoryStore.Read(context.Background(), scope, "tenants", string(bootstrapTestTenant)); err != nil {
			t.Fatalf("existing tenant disappeared: %v", err)
		}
		if rows := bootstrapAllMembershipRows(t, memoryStore, bootstrapTestTenant); len(rows) != 0 {
			t.Fatalf("bootstrap added %d memberships to an existing tenant; want none", len(rows))
		}
		if audits := memoryStore.Audit(scope); len(audits) != 0 {
			t.Fatalf("bootstrap added %d audit rows to an existing tenant; want none", len(audits))
		}
		if events := memoryStore.Events(scope); len(events) != 0 {
			t.Fatalf("bootstrap added %d events to an existing tenant; want none", len(events))
		}
	})

	t.Run("last admin revoked is not recovery", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		scope := tenancy.Scope{TenantID: bootstrapTestTenant}
		mustExecuteBootstrapUnit(t, memoryStore, scope, store.Unit{Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(bootstrapTestTenant),
			Insert: true,
			Values: map[string]any{
				"name":           "Existing Tenant",
				"version":        int64(1),
				"derivation_key": []byte(`{"tenant_id":"tenant-bootstrap","version":1,"ciphertext":"AA==","wrapped_key":"AA==","wrap_nonce":"AA==","key_ref":"test:key","key_version":1}`),
				"created_at":     clock.Now(),
			},
		}}})
		mustExecuteBootstrapUnit(t, memoryStore, scope, store.Unit{Writes: []store.Write{{
			Table:  "memberships",
			ID:     "membership-revoked",
			Insert: true,
			Values: map[string]any{
				"principal_id": "principal-revoked",
				"roles":        []byte(`["admin"]`),
				"version":      int64(1),
				"created_at":   clock.Now(),
			},
		}}})
		mustExecuteBootstrapUnit(t, memoryStore, scope, store.Unit{Writes: []store.Write{{
			Table:         "memberships",
			ID:            "membership-revoked",
			Delete:        true,
			ExpectVersion: 1,
		}}})
		if rows := bootstrapAllMembershipRows(t, memoryStore, bootstrapTestTenant); len(rows) != 0 {
			t.Fatalf("revoked admin fixture still has %d memberships; want none", len(rows))
		}

		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		tokenFile := writeBootstrapToken(t, "recovery-token")
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, tokenFile))

		if _, err := bootstrap.Run(context.Background()); err == nil {
			t.Fatal("bootstrap treated a tenant with its last Admin revoked as recovery")
		}
		if _, err := memoryStore.Read(context.Background(), scope, "tenants", string(bootstrapTestTenant)); err != nil {
			t.Fatalf("existing tenant disappeared: %v", err)
		}
		if rows := bootstrapAllMembershipRows(t, memoryStore, bootstrapTestTenant); len(rows) != 0 {
			t.Fatalf("bootstrap recreated %d memberships; want none", len(rows))
		}
		if audits := memoryStore.Audit(scope); len(audits) != 0 {
			t.Fatalf("bootstrap added %d audit rows; want none", len(audits))
		}
		if events := memoryStore.Events(scope); len(events) != 0 {
			t.Fatalf("bootstrap added %d events; want none", len(events))
		}
	})

	t.Run("second run is refused", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		firstPrincipal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		firstVerifier := &bootstrapRecordingVerifier{principal: firstPrincipal}
		firstIDs := &bootstrapTestIDs{rowID: "row-bootstrap-first", eventIDs: []id.EventID{"event-audit-first", "event-config-first"}}
		first := newBootstrapForTest(t, memoryStore, cipher, firstVerifier, firstIDs, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "first-token")))
		if _, err := first.Run(context.Background()); err != nil {
			t.Fatalf("first Run: %v", err)
		}

		secondPrincipal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, "auth0|another-admin")
		secondVerifier := &bootstrapRecordingVerifier{principal: secondPrincipal}
		secondIDs := &bootstrapTestIDs{rowID: "row-bootstrap-second", eventIDs: []id.EventID{"event-audit-second", "event-config-second"}}
		second := newBootstrapForTest(t, memoryStore, cipher, secondVerifier, secondIDs, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "second-token")))
		if _, err := second.Run(context.Background()); err == nil {
			t.Fatal("a second bootstrap succeeded for the same tenant")
		}
		if rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, secondPrincipal.ID); len(rows) != 0 {
			t.Fatalf("second bootstrap created %d memberships; want none", len(rows))
		}
		if rows := bootstrapMembershipRows(t, memoryStore, bootstrapTestTenant, firstPrincipal.ID); len(rows) != 1 {
			t.Fatalf("first bootstrap memberships = %d; want exactly 1", len(rows))
		}
		scope := tenancy.Scope{TenantID: bootstrapTestTenant}
		if audits := memoryStore.Audit(scope); len(audits) != 1 {
			t.Fatalf("audit rows after the second attempt = %d; want exactly 1", len(audits))
		}
		if events := memoryStore.Events(scope); len(events) != 1 {
			t.Fatalf("event rows after the second attempt = %d; want exactly 1", len(events))
		}
	})
}

// TestBootstrapAdminAtomicUnit proves the tenant, encrypted key, membership,
// audit and config.changed event are one commit unit: a post-staging failure
// leaves none of them visible.
func TestBootstrapAdminAtomicUnit(t *testing.T) {
	clock := newFakeClock()
	memoryStore := memory.NewWithClock(clock.Now)
	cipher := newTestCipher(t)
	principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
	verifier := &bootstrapRecordingVerifier{principal: principal}
	ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
	bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
		bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "atomic-token")))

	injected := errors.New("injected post-staging commit failure")
	memoryStore.FailNextCommit(injected)
	if _, err := bootstrap.Run(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Run error = %v; want the injected post-staging failure", err)
	}
	assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
}

// TestBootstrapAdminDependencyFailureHasNoWrites proves a KMS, OIDC or
// cancellation failure happens before the single commit unit and never leaks
// the token or key material into the public error.
func TestBootstrapAdminDependencyFailureHasNoWrites(t *testing.T) {
	t.Run("KMS seal failure", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		const marker = "kms-material-must-not-leak"
		bootstrap := newBootstrapForTest(t, memoryStore, bootstrapFailingCipher{marker: marker}, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "kms-token")))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap succeeded despite a KMS seal failure")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("KMS failure class = %v (ok=%v); want dependency_unavailable", class, ok)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("KMS failure leaked its private cause: %v", err)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("OIDC verification failure", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		const marker = "oidc-provider-cause-must-not-leak"
		verifier := &bootstrapRecordingVerifier{err: fmt.Errorf("%s for token %s", marker, "secret-token-value")}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "secret-token-value")))

		_, err := bootstrap.Run(context.Background())
		if err == nil {
			t.Fatal("bootstrap succeeded despite an OIDC verification failure")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("OIDC failure class = %v (ok=%v); want unauthenticated", class, ok)
		}
		for _, leaked := range []string{marker, "secret-token-value"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("OIDC failure leaked %q: %v", leaked, err)
			}
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("cancelled context", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "cancelled-token")))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := bootstrap.Run(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Run error = %v; want context.Canceled", err)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("cancellation after verification", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := bootstrapInertCipher{}
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		ids := &bootstrapTestIDs{rowID: "row-bootstrap", eventIDs: []id.EventID{"event-audit", "event-config"}}
		executor := &bootstrapCountingExecutor{inner: memoryStore}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		verifier := bootstrapCancellingVerifier{cancel: cancel, principal: principal}
		bootstrap := newBootstrapForTest(t, executor, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "post-verify-cancel-token")))

		_, err := bootstrap.Run(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("post-verification cancellation error = %v; want context.Canceled", err)
		}
		if calls := executor.commitCalls(); calls != 0 {
			t.Fatalf("bootstrap reached the commit unit %d time(s) after cancellation; want none", calls)
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})

	t.Run("identity allocation failure", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		principal := bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject)
		verifier := &bootstrapRecordingVerifier{principal: principal}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, &bootstrapTestIDs{}, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "ids-token")))

		if _, err := bootstrap.Run(context.Background()); err == nil {
			t.Fatal("bootstrap succeeded without an identity source")
		}
		assertBootstrapNoWrites(t, memoryStore, bootstrapTestTenant)
	})
}
