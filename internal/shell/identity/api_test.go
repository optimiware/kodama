// Principal-verification API pinned by these tests.
//
// OIDC is only an access-token verifier: the OIDCVerifier returns a
// structurally verified Principal, and this package re-reads current
// membership/enablement before returning authority. The implementation pinned
// here is deliberately small and has no discovery, JWKS, HTTP or provider SDK:
//
//	type Authenticator struct{ ... }
//	type AuthenticatorConfig struct {
//	    Verifier   OIDCVerifier
//	    Principals PrincipalReader
//	}
//	func NewAuthenticator(AuthenticatorConfig) (*Authenticator, error)
//	func (*Authenticator) Authenticate(context.Context, AuthenticateRequest) (Principal, error)
//
//	type AuthenticateRequest struct {
//	    AccessToken string
//	    Scope       tenancy.Scope
//	    Now         time.Time
//	}
//
//	func Current(context.Context, PrincipalReader, PrincipalRequest) (Principal, error)
//	func CheckCurrent(verified, current Principal) error
//
// Rules pinned by these tests (S05 §1–2):
//   - A successful token verification is never durable authority: the current
//     principal is re-read on every call and returned roles are the current
//     roles, not roles carried by a stale token.
//   - A disabled or removed current principal, a scope mismatch, a changed
//     kind/id/execution identity or a failed current read fails closed.
//   - The access token never appears in an error, and the verifier's private
//     cause stays unrendered through fault wrapping.
//   - An invalid request (empty token, zero time, invalid scope) is refused
//     before any verifier or reader call.
package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-a"),
		ProjectID: id.ProjectID("project-1"),
	}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-b"),
		ProjectID: id.ProjectID("project-2"),
	}
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}

func humanPrincipal(scope tenancy.Scope, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 7,
	}
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

// requireAuthRefusal accepts either authentication or authorization refusal for
// a disabled/removed principal, because the declared operation groups allow
// both unauthenticated and forbidden for an inactive identity.
func requireAuthRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want a refusal")
	}
	class, ok := fault.ClassOf(err)
	if !ok || (class != fault.Unauthenticated && class != fault.Forbidden) {
		t.Fatalf("error class = %v (ok=%v); want unauthenticated or forbidden (err=%v)", class, ok, err)
	}
}

// TestAuthenticatorReturnsCurrentRolesNotTokenRoles pins the central identity
// invariant: a token that once carried Admin does not keep Admin after the
// current membership is downgraded.
func TestAuthenticatorReturnsCurrentRolesNotTokenRoles(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleAdmin)
	current := humanPrincipal(testScope(), tenancy.RoleViewer)
	current.Version = 9
	verifier := &fakeVerifier{principal: verified}
	reader := &fakePrincipalReader{principal: current}

	authenticator := newAuthenticator(t, verifier, reader)
	principal, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: "access-token-1",
		Scope:       testScope(),
		Now:         testNow(),
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Version != 9 {
		t.Fatalf("Version = %d; want the current version 9", principal.Version)
	}
	if len(principal.Roles) != 1 || principal.Roles[0] != tenancy.RoleViewer {
		t.Fatalf("Roles = %v; want the current [viewer] role", principal.Roles)
	}
	if principal.HasRole(tenancy.RoleAdmin) {
		t.Fatal("stale Admin role survived the current-state re-read")
	}
	if verifier.calls != 1 || reader.calls != 1 {
		t.Fatalf("verifier/reader calls = %d/%d; want 1/1", verifier.calls, reader.calls)
	}
	if reader.requests[0].PrincipalID != verified.ID || reader.requests[0].Scope != verified.Scope {
		t.Fatalf("current read = %+v; want verified principal identity", reader.requests[0])
	}
}

// TestAuthenticatorRejectsDisabledCurrentPrincipal proves an otherwise valid
// token fails closed when the current membership is disabled.
func TestAuthenticatorRejectsDisabledCurrentPrincipal(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleEngineer)
	current := humanPrincipal(testScope(), tenancy.RoleEngineer)
	current.Enabled = false
	verifier := &fakeVerifier{principal: verified}
	reader := &fakePrincipalReader{principal: current}

	authenticator := newAuthenticator(t, verifier, reader)
	_, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: "access-token-1",
		Scope:       testScope(),
		Now:         testNow(),
	})
	requireAuthRefusal(t, err)
}

// TestAuthenticatorRejectsScopeMismatch proves a token verified for tenant B is
// not accepted at a tenant A call site even before the current read.
func TestAuthenticatorRejectsScopeMismatch(t *testing.T) {
	verified := humanPrincipal(otherScope(), tenancy.RoleViewer)
	reader := &fakePrincipalReader{principal: verified}
	authenticator := newAuthenticator(t, &fakeVerifier{principal: verified}, reader)

	_, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: "access-token-1",
		Scope:       testScope(),
		Now:         testNow(),
	})
	requireClass(t, err, fault.Forbidden)
	if reader.calls != 0 {
		t.Fatalf("current read happened for a cross-scope token: %d calls", reader.calls)
	}
}

// TestAuthenticatorRejectsChangedCurrentIdentity proves a token cannot be
// transplanted to a different current id, kind or execution identity.
func TestAuthenticatorRejectsChangedCurrentIdentity(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleViewer)

	cases := []struct {
		name    string
		current identity.Principal
	}{
		{
			name: "different id",
			current: identity.Principal{
				ID:      id.PrincipalID("human-2"),
				Kind:    identity.KindHuman,
				Scope:   testScope(),
				Enabled: true,
				Version: 7,
			},
		},
		{
			name: "different kind",
			current: identity.Principal{
				ID:      id.PrincipalID("human-1"),
				Kind:    identity.KindService,
				Scope:   testScope(),
				Enabled: true,
				Version: 7,
			},
		},
		{
			name: "different scope",
			current: identity.Principal{
				ID:      id.PrincipalID("human-1"),
				Kind:    identity.KindHuman,
				Scope:   otherScope(),
				Enabled: true,
				Version: 7,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			authenticator := newAuthenticator(t, &fakeVerifier{principal: verified}, &fakePrincipalReader{principal: testCase.current})
			_, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
				AccessToken: "access-token-1",
				Scope:       testScope(),
				Now:         testNow(),
			})
			requireAuthRefusal(t, err)
		})
	}
}

// TestAuthenticatorRejectsFailedCurrentRead proves a membership read failure is
// a refusal, never a fallback to the token's claims.
func TestAuthenticatorRejectsFailedCurrentRead(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleAdmin)
	reader := &fakePrincipalReader{err: errors.New("membership store unavailable")}
	authenticator := newAuthenticator(t, &fakeVerifier{principal: verified}, reader)

	principal, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: "access-token-1",
		Scope:       testScope(),
		Now:         testNow(),
	})
	if err == nil {
		t.Fatalf("Authenticate = %+v, nil; want refusal", principal)
	}
	if principal.ID != "" || len(principal.Roles) != 0 {
		t.Fatalf("failed current read returned a principal: %+v", principal)
	}
}

// TestAuthenticatorRedactsAccessToken proves a verifier error that embeds the
// raw bearer token cannot leak it through the public error.
func TestAuthenticatorRedactsAccessToken(t *testing.T) {
	const presented = "super-secret-access-token-value"
	verifier := &fakeVerifier{err: errors.New("provider rejected token " + presented)}
	authenticator := newAuthenticator(t, verifier, &fakePrincipalReader{})

	_, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: presented,
		Scope:       testScope(),
		Now:         testNow(),
	})
	if err == nil {
		t.Fatal("Authenticate accepted a verifier error")
	}
	if strings.Contains(err.Error(), presented) {
		t.Fatalf("error leaked the access token: %v", err)
	}
}

// TestAuthenticatorRejectsInvalidRequestBeforeDependencies proves malformed
// input is refused without invoking the verifier or reader.
func TestAuthenticatorRejectsInvalidRequestBeforeDependencies(t *testing.T) {
	cases := []struct {
		name    string
		request identity.AuthenticateRequest
	}{
		{
			name:    "empty token",
			request: identity.AuthenticateRequest{Scope: testScope(), Now: testNow()},
		},
		{
			name:    "zero time",
			request: identity.AuthenticateRequest{AccessToken: "token", Scope: testScope()},
		},
		{
			name:    "invalid scope",
			request: identity.AuthenticateRequest{AccessToken: "token", Scope: tenancy.Scope{}, Now: testNow()},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verifier := &fakeVerifier{principal: humanPrincipal(testScope())}
			reader := &fakePrincipalReader{principal: humanPrincipal(testScope())}
			authenticator := newAuthenticator(t, verifier, reader)
			_, err := authenticator.Authenticate(context.Background(), testCase.request)
			requireClass(t, err, fault.InvalidInput)
			if verifier.calls != 0 || reader.calls != 0 {
				t.Fatalf("invalid request reached dependencies: verifier=%d reader=%d", verifier.calls, reader.calls)
			}
		})
	}
}

// TestAuthenticatorReadsCurrentStateEveryTime proves there is no cached
// membership allow: two calls perform two current reads.
func TestAuthenticatorReadsCurrentStateEveryTime(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleViewer)
	reader := &fakePrincipalReader{principal: verified}
	authenticator := newAuthenticator(t, &fakeVerifier{principal: verified}, reader)

	for i := range 2 {
		if _, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
			AccessToken: "access-token-1",
			Scope:       testScope(),
			Now:         testNow(),
		}); err != nil {
			t.Fatalf("Authenticate call %d: %v", i+1, err)
		}
	}
	if reader.calls != 2 {
		t.Fatalf("current reads = %d; want 2 (no cached authority)", reader.calls)
	}
}

// TestAuthenticatorRejectsInvalidVerifiedPrincipal proves a verifier cannot
// smuggle a structurally invalid principal past the boundary.
func TestAuthenticatorRejectsInvalidVerifiedPrincipal(t *testing.T) {
	verified := identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.PrincipalKind("robot"),
		Scope:   testScope(),
		Enabled: true,
	}
	authenticator := newAuthenticator(t, &fakeVerifier{principal: verified}, &fakePrincipalReader{principal: verified})

	_, err := authenticator.Authenticate(context.Background(), identity.AuthenticateRequest{
		AccessToken: "access-token-1",
		Scope:       testScope(),
		Now:         testNow(),
	})
	requireAuthRefusal(t, err)
}

// TestCurrentRejectsDisabledPrincipal pins the read seam used by every
// commit-boundary check: a disabled principal is not returned as current.
func TestCurrentRejectsDisabledPrincipal(t *testing.T) {
	disabled := humanPrincipal(testScope(), tenancy.RoleViewer)
	disabled.Enabled = false
	reader := &fakePrincipalReader{principal: disabled}

	_, err := identity.Current(context.Background(), reader, identity.PrincipalRequest{
		Scope:       testScope(),
		PrincipalID: disabled.ID,
	})
	requireAuthRefusal(t, err)
}

// TestCheckCurrentRejectsCrossScopeKindAndID pins the pure current-state check
// directly, including the execution identity comparison.
func TestCheckCurrentRejectsCrossScopeKindAndID(t *testing.T) {
	verified := humanPrincipal(testScope(), tenancy.RoleViewer)

	current := humanPrincipal(testScope(), tenancy.RoleViewer)
	if err := identity.CheckCurrent(verified, current); err != nil {
		t.Fatalf("CheckCurrent(same) = %v; want nil", err)
	}

	otherID := current
	otherID.ID = id.PrincipalID("human-2")
	requireAuthRefusal(t, identity.CheckCurrent(verified, otherID))

	otherKind := current
	otherKind.Kind = identity.KindService
	requireAuthRefusal(t, identity.CheckCurrent(verified, otherKind))

	otherScope := current
	otherScope.Scope = otherScopeForTest()
	requireAuthRefusal(t, identity.CheckCurrent(verified, otherScope))

	disabled := current
	disabled.Enabled = false
	requireAuthRefusal(t, identity.CheckCurrent(verified, disabled))
}

func otherScopeForTest() tenancy.Scope {
	return otherScope()
}

// TestOAuthClaimsMatchTenantAndRole pins the explicit claim match used by the
// OAuth application seam: tenant and role must both match, and a nil claim
// never matches.
func TestOAuthClaimsMatchTenantAndRole(t *testing.T) {
	claims := identity.OAuthClaims{
		TenantClaim: "https://kodama.example.com/tenant",
		Tenant:      "tenant-a",
		RoleClaim:   "https://kodama.example.com/roles",
		Role:        "engineer",
	}
	values := map[string][]string{
		claims.TenantClaim: {"tenant-a"},
		claims.RoleClaim:   {"viewer", "engineer"},
	}
	if !claims.Matches(values) {
		t.Fatal("matching tenant and role claims were refused")
	}

	wrongTenant := map[string][]string{
		claims.TenantClaim: {"tenant-b"},
		claims.RoleClaim:   {"engineer"},
	}
	if claims.Matches(wrongTenant) {
		t.Fatal("wrong tenant claim matched")
	}

	wrongRole := map[string][]string{
		claims.TenantClaim: {"tenant-a"},
		claims.RoleClaim:   {"viewer"},
	}
	if claims.Matches(wrongRole) {
		t.Fatal("wrong role claim matched")
	}
	if claims.Matches(nil) {
		t.Fatal("nil claims matched")
	}
}

// TestOIDCConfigRefusesInsecureIssuer pins installation configuration refusal:
// a plain-HTTP issuer or empty audience never reaches a verifier.
func TestOIDCConfigRefusesInsecureIssuer(t *testing.T) {
	requireClass(t, identity.OIDCConfig{Issuer: "http://issuer.example.com", Audience: "kodama"}.Validate(), fault.InvalidInput)
	requireClass(t, identity.OIDCConfig{Issuer: "https://issuer.example.com"}.Validate(), fault.InvalidInput)
	requireClass(t, identity.OIDCConfig{Issuer: "https://issuer.example.com", Audience: "kodama", ClockSkew: -time.Second}.Validate(), fault.InvalidInput)
	if err := (identity.OIDCConfig{Issuer: "https://issuer.example.com", Audience: "kodama", ClockSkew: time.Second}).Validate(); err != nil {
		t.Fatalf("valid OIDC config refused: %v", err)
	}
}

func newAuthenticator(t *testing.T, verifier identity.OIDCVerifier, reader identity.PrincipalReader) *identity.Authenticator {
	t.Helper()
	authenticator, err := identity.NewAuthenticator(identity.AuthenticatorConfig{
		Verifier:   verifier,
		Principals: reader,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if authenticator == nil {
		t.Fatal("NewAuthenticator returned nil")
	}
	return authenticator
}
