package factory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// fakePrincipalReader is the current-membership reader the OIDC resolver unit
// test controls. It reproduces the store reader's contract: it returns the
// current roles/version for the requested identity in the requested scope, and
// it is consulted on every resolution.
type fakePrincipalReader struct {
	mu        sync.Mutex
	roles     []tenancy.Role
	version   uint64
	err       error
	reads     int
	lastScope tenancy.Scope
}

// ReadPrincipal implements identity.PrincipalReader.
func (reader *fakePrincipalReader) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.reads++
	reader.lastScope = request.Scope
	if reader.err != nil {
		return identity.Principal{}, reader.err
	}
	return identity.Principal{
		ID:      request.PrincipalID,
		Kind:    identity.KindHuman,
		Scope:   request.Scope,
		Roles:   append([]tenancy.Role(nil), reader.roles...),
		Enabled: true,
		Version: reader.version,
	}, nil
}

// set replaces the current stored membership the fake reader returns.
func (reader *fakePrincipalReader) set(roles []tenancy.Role, version uint64) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.roles = append([]tenancy.Role(nil), roles...)
	reader.version = version
}

// setError makes the next reads fail the way a missing or disabled principal
// does at the store boundary.
func (reader *fakePrincipalReader) setError(err error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.err = err
}

// readCount returns how many current-membership reads the resolver performed.
func (reader *fakePrincipalReader) readCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reads
}

// staticPrincipalFixture stands in for the development static-token resolver
// the OIDC resolver delegates to for non-OIDC credentials.
type staticPrincipalFixture struct {
	principal identity.Principal
	calls     int
}

// Resolve implements authorize.CurrentPrincipal.
func (fixture *staticPrincipalFixture) Resolve(context.Context, string) (authorize.Principal, error) {
	fixture.calls++
	return fixture.principal, nil
}

// countingVerifier records how many credentials reach the JWT verifier while
// delegating the decision to the real verifier.
type countingVerifier struct {
	delegate identity.OIDCVerifier
	calls    int
}

// Verify implements identity.OIDCVerifier.
func (verifier *countingVerifier) Verify(ctx context.Context, request identity.OIDCRequest) (identity.Principal, error) {
	verifier.calls++
	return verifier.delegate.Verify(ctx, request)
}

// newOIDCFixtureForTest builds the HTTPS OIDC fixture and its real
// verifier against one deterministic clock.
func newOIDCFixtureForTest(t *testing.T) (*identityfixture.Provider, identity.OIDCVerifier) {
	t.Helper()
	provider, err := identityfixture.NewProvider(identityfixture.OIDCOptions{
		Clock: identityfixture.NewClock(time.Unix(1_700_000_000, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("new OIDC provider: %v", err)
	}
	t.Cleanup(provider.Close)
	verifier, err := provider.NewVerifier()
	if err != nil {
		t.Fatalf("new OIDC verifier: %v", err)
	}
	return provider, verifier
}

// requireFaultClass asserts one public fault class.
func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error = %v (class=%v ok=%v); want class %v", err, class, ok, want)
	}
}

// TestOIDCPrincipalResolverReadsCurrentMembership proves a verified OIDC token
// only supplies the identity hint: every resolution re-reads the current stored
// membership, so role claims in the token are never authority and a changed
// membership changes the very next decision with the same token.
func TestOIDCPrincipalResolverReadsCurrentMembership(t *testing.T) {
	provider, verifier := newOIDCFixtureForTest(t)
	reader := &fakePrincipalReader{roles: []tenancy.Role{tenancy.RoleReviewer}, version: 3}
	static := &staticPrincipalFixture{principal: identity.Principal{
		ID:      "static-human",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-a"},
		Enabled: true,
	}}
	resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
		Verifier:   verifier,
		Static:     static,
		Principals: reader,
		Now:        provider.Clock().Now,
	})
	if err != nil {
		t.Fatalf("new OIDC principal resolver: %v", err)
	}

	const subject = "human-current-membership"
	minted, err := provider.Mint(identityfixture.TokenSpec{
		Subject: subject,
		// A stale or forged role claim must not become authority.
		Roles: []string{string(tenancy.RoleAdmin)},
	})
	if err != nil {
		t.Fatalf("mint OIDC access token: %v", err)
	}
	wantID, err := shelloidc.PrincipalIDFor(provider.Issuer(), subject)
	if err != nil {
		t.Fatalf("derive OIDC principal id: %v", err)
	}

	principal, err := resolver.Resolve(context.Background(), minted.Value)
	if err != nil {
		t.Fatalf("resolve verified token: %v", err)
	}
	if principal.ID != wantID {
		t.Fatalf("resolved principal id = %q; want OIDC-derived %q", principal.ID, wantID)
	}
	if !principal.HasRole(tenancy.RoleReviewer) || principal.HasRole(tenancy.RoleAdmin) {
		t.Fatalf("resolved roles = %v; want only the current reviewer membership", principal.Roles)
	}
	if principal.Version != 3 {
		t.Fatalf("resolved membership version = %d; want 3", principal.Version)
	}
	if static.calls != 0 {
		t.Fatalf("OIDC credential was delegated to the static resolver %d times", static.calls)
	}

	// The same token resolves to the new current membership after an upgrade.
	reader.set([]tenancy.Role{tenancy.RoleAdmin}, 4)
	upgraded, err := resolver.Resolve(context.Background(), minted.Value)
	if err != nil {
		t.Fatalf("resolve after membership upgrade: %v", err)
	}
	if !upgraded.HasRole(tenancy.RoleAdmin) || upgraded.Version != 4 {
		t.Fatalf("resolved roles/version = %v/%d; want admin/4", upgraded.Roles, upgraded.Version)
	}
	if reader.readCount() < 2 {
		t.Fatalf("current membership was read %d times; want one read per resolution", reader.readCount())
	}
	if provider.JWKSFetchCount() == 0 {
		t.Fatalf("token was resolved without the real verifier fetching the JWKS")
	}
	if static.calls != 0 {
		t.Fatalf("OIDC credential was delegated to the static resolver %d times", static.calls)
	}

	// A current-read failure after a successful resolution is a refusal, never
	// a replay of the last good principal. The same token must not keep working
	// from cached authority while the committed row is gone or unreadable.
	reader.setError(fault.New(fault.NotFound, "no committed principal exists in the requested scope"))
	if _, err := resolver.Resolve(context.Background(), minted.Value); err == nil {
		t.Fatalf("resolve after a failed current read replayed the last good principal")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if static.calls != 0 {
		t.Fatalf("failed current read fell back to the static resolver")
	}
}

// TestOIDCPrincipalResolverRequiresCommittedMembership proves verification is
// not authority: a correctly signed token for an identity with no committed
// membership is refused as unauthenticated and never falls back to a static
// token map.
func TestOIDCPrincipalResolverRequiresCommittedMembership(t *testing.T) {
	provider, verifier := newOIDCFixtureForTest(t)
	reader := &fakePrincipalReader{}
	static := &staticPrincipalFixture{principal: identity.Principal{
		ID:      "static-human",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-a"},
		Enabled: true,
	}}
	resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
		Verifier:   verifier,
		Static:     static,
		Principals: reader,
		Now:        provider.Clock().Now,
	})
	if err != nil {
		t.Fatalf("new OIDC principal resolver: %v", err)
	}

	minted, err := provider.Mint(identityfixture.TokenSpec{Subject: "human-without-membership"})
	if err != nil {
		t.Fatalf("mint OIDC access token: %v", err)
	}

	reader.setError(fault.New(fault.NotFound, "no committed principal exists in the requested scope"))
	if _, err := resolver.Resolve(context.Background(), minted.Value); err == nil {
		t.Fatalf("verified token without a committed membership was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if static.calls != 0 {
		t.Fatalf("verified-but-unauthorized token fell back to the static resolver")
	}
}

// TestOIDCPrincipalResolverRejectsIssuerAudienceExpiry proves the real verifier,
// not a token map, refuses a correctly signed token whose issuer, audience or
// expiry is outside the configured contract.
func TestOIDCPrincipalResolverRejectsIssuerAudienceExpiry(t *testing.T) {
	provider, verifier := newOIDCFixtureForTest(t)
	reader := &fakePrincipalReader{roles: []tenancy.Role{tenancy.RoleAdmin}, version: 1}
	static := &staticPrincipalFixture{principal: identity.Principal{
		ID:      "static-human",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-a"},
		Enabled: true,
	}}
	resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
		Verifier:   verifier,
		Static:     static,
		Principals: reader,
		Now:        provider.Clock().Now,
	})
	if err != nil {
		t.Fatalf("new OIDC principal resolver: %v", err)
	}

	valid, err := provider.Mint(identityfixture.TokenSpec{Subject: "human-verifier"})
	if err != nil {
		t.Fatalf("mint valid token: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), valid.Value); err != nil {
		t.Fatalf("valid token was refused: %v", err)
	}

	cases := []struct {
		name string
		spec identityfixture.TokenSpec
	}{
		{"issuer", identityfixture.TokenSpec{Subject: "human-verifier", Issuer: "https://issuer.invalid"}},
		{"audience", identityfixture.TokenSpec{Subject: "human-verifier", Audience: "another-audience"}},
		{"expiry", identityfixture.TokenSpec{
			Subject:   "human-verifier",
			ExpiresAt: provider.Clock().Now().Add(-time.Minute),
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			minted, err := provider.Mint(testCase.spec)
			if err != nil {
				t.Fatalf("mint %s token: %v", testCase.name, err)
			}
			if _, err := resolver.Resolve(context.Background(), minted.Value); err == nil {
				t.Fatalf("%s token was accepted", testCase.name)
			} else {
				requireFaultClass(t, err, fault.Unauthenticated)
			}
			if static.calls != 0 {
				t.Fatalf("%s token fell back to the static resolver", testCase.name)
			}
		})
	}
}

// TestOIDCPrincipalResolverDelegatesStaticCredentials proves the opt-in
// resolver keeps the composed development credentials working, and that a
// malformed JWT-shaped credential is refused instead of being retried against
// the static token map.
func TestOIDCPrincipalResolverDelegatesStaticCredentials(t *testing.T) {
	provider, verifier := newOIDCFixtureForTest(t)
	static := &staticPrincipalFixture{principal: identity.Principal{
		ID:      "static-human",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-a"},
		Enabled: true,
	}}
	resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
		Verifier:   verifier,
		Static:     static,
		Principals: &fakePrincipalReader{roles: []tenancy.Role{tenancy.RoleReviewer}},
		Now:        provider.Clock().Now,
	})
	if err != nil {
		t.Fatalf("new OIDC principal resolver: %v", err)
	}

	principal, err := resolver.Resolve(context.Background(), "fixture-static-development-token")
	if err != nil {
		t.Fatalf("static credential was refused: %v", err)
	}
	if principal.ID != "static-human" || static.calls != 1 {
		t.Fatalf("static credential resolved to %q after %d static calls", principal.ID, static.calls)
	}

	if _, err := resolver.Resolve(context.Background(), "not.a.jwt"); err == nil {
		t.Fatalf("malformed JWT-shaped credential was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if static.calls != 1 {
		t.Fatalf("malformed JWT-shaped credential fell back to the static resolver")
	}
}

// TestOIDCPrincipalResolverRoutesDerivedCredentials pins the credential-shape
// routing: the derived kodex./kodsvc. execution and service credentials carry a
// dotted MAC segment but are always decided by the static/derived
// authenticator, while a JWT-shaped value always reaches the verifier and never
// the static path.
func TestOIDCPrincipalResolverRoutesDerivedCredentials(t *testing.T) {
	provider, verifier := newOIDCFixtureForTest(t)
	static := &staticPrincipalFixture{principal: identity.Principal{
		ID:      "derived-principal",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-a"},
		Enabled: true,
	}}
	spy := &countingVerifier{delegate: verifier}
	resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
		Verifier:   spy,
		Static:     static,
		Principals: &fakePrincipalReader{roles: []tenancy.Role{tenancy.RoleEngineer}},
		Now:        provider.Clock().Now,
	})
	if err != nil {
		t.Fatalf("new OIDC principal resolver: %v", err)
	}

	derived := []string{
		token.ExecutionPrefix + "execution-i04.bWFj",
		token.ServicePrefix + "service-i04.bWFj",
	}
	for _, presented := range derived {
		if isJWTCredential(presented) {
			t.Fatalf("derived credential %q was classified as a JWT", presented)
		}
		principal, err := resolver.Resolve(context.Background(), presented)
		if err != nil {
			t.Fatalf("derived credential %q was refused: %v", presented, err)
		}
		if principal.ID != "derived-principal" {
			t.Fatalf("derived credential %q resolved to %q; want the static principal", presented, principal.ID)
		}
		if spy.calls != 0 {
			t.Fatalf("derived credential %q reached the JWT verifier %d times", presented, spy.calls)
		}
	}
	if static.calls != len(derived) {
		t.Fatalf("static resolver calls = %d; want one per derived credential (%d)", static.calls, len(derived))
	}

	// A JWT-shaped value is decided by the verifier only. It is refused here
	// because it is not a signed token, but the static map is never consulted.
	if !isJWTCredential("not.a.jwt") {
		t.Fatalf("JWT-shaped credential was not routed to the verifier")
	}
	if _, err := resolver.Resolve(context.Background(), "not.a.jwt"); err == nil {
		t.Fatalf("malformed JWT-shaped credential was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if static.calls != len(derived) {
		t.Fatalf("JWT-shaped credential reached the static resolver")
	}
	if spy.calls != 1 {
		t.Fatalf("JWT-shaped credential reached the verifier %d times; want 1", spy.calls)
	}
}
