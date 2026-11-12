package factory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// OIDCAdminSubject is the fixture subject of the bootstrapped OIDC
// administrator. Its authority comes from a committed membership row, never
// from the token or from a static development token mapped to the same
// identity.
const OIDCAdminSubject = "oidc-admin-i04"

// OIDCFixture is the opt-in harness OIDC material: the in-process HTTPS
// identity provider, the real verifier built against it, and the derived
// administrator identity whose committed membership the harness bootstraps.
//
// Tokens are always minted on the fixture's deterministic clock and carry the
// factory tenant, so the real verifier's expiry and scope checks are exercised
// with the same scope the served composition uses.
type OIDCFixture struct {
	// Provider is the HTTPS identity fixture.
	Provider *identityfixture.Provider
	// Issuer is the provider's HTTPS issuer URL.
	Issuer string
	// AdminSubject is the subject of the bootstrapped administrator.
	AdminSubject string
	// AdminPrincipal is the principal id derived from issuer+subject. The
	// harness seeds this identity's tenant-wide Admin membership through the
	// same committed store seam the composition reads.
	AdminPrincipal id.PrincipalID

	tenantID     id.TenantID
	projectID    id.ProjectID
	projectClaim string
}

// newOIDCFixture starts one HTTPS OIDC provider anchored at now and derives the
// administrator identity from its issuer. The provider is closed through the
// harness cleanup; close is idempotent.
//
// The provider keeps its own clock frozen at now. It never advances with the
// harness TestClock, so a minted token's iat/exp and the verifier's expiry
// decision are independent of test-clock advances; a harness test that moves
// TestClock must not expect the OIDC lifetime to move with it. This is
// deliberate and is not a behaviour to "fix" when pinning timestamps.
func newOIDCFixture(t testing.TB, now time.Time) *OIDCFixture {
	t.Helper()
	provider, err := identityfixture.NewProvider(identityfixture.OIDCOptions{
		Clock: identityfixture.NewClock(now.UTC()),
	})
	if err != nil {
		t.Fatalf("factory: new OIDC provider: %v", err)
	}
	t.Cleanup(provider.Close)
	adminPrincipal, err := shelloidc.PrincipalIDFor(provider.Issuer(), OIDCAdminSubject)
	if err != nil {
		t.Fatalf("factory: derive OIDC administrator principal: %v", err)
	}
	return &OIDCFixture{
		Provider:       provider,
		Issuer:         provider.Issuer(),
		AdminSubject:   OIDCAdminSubject,
		AdminPrincipal: adminPrincipal,
		tenantID:       id.TenantID(TenantID),
		projectID:      id.ProjectID(ProjectID),
		projectClaim:   provider.ProjectClaim(),
	}
}

// Mint mints one access token for the harness tenant and project. Empty spec
// fields take the factory scope so a project-scoped membership is read in the
// same scope the composition resolves; an explicit claim still wins.
func (fixture *OIDCFixture) Mint(t testing.TB, spec identityfixture.TokenSpec) string {
	t.Helper()
	return fixture.mint(t, spec, false)
}

// AdminToken mints the bootstrapped administrator's tenant-wide access token.
// The token is only an identity hint: the administrator's authority is the
// committed tenant-wide Admin membership the harness seeded.
func (fixture *OIDCFixture) AdminToken(t testing.TB) string {
	t.Helper()
	return fixture.mint(t, identityfixture.TokenSpec{Subject: fixture.AdminSubject}, true)
}

// mint mints one token, filling the factory scope and, for the tenant-wide
// administrator, removing the project claim so the verified principal is
// tenant-scoped.
func (fixture *OIDCFixture) mint(t testing.TB, spec identityfixture.TokenSpec, tenantWide bool) string {
	t.Helper()
	if spec.Tenant == "" {
		spec.Tenant = string(fixture.tenantID)
	}
	if tenantWide {
		previous := spec.Mutate
		spec.Mutate = func(claims map[string]any) {
			delete(claims, fixture.projectClaim)
			if previous != nil {
				previous(claims)
			}
		}
	} else if spec.Project == "" {
		spec.Project = string(fixture.projectID)
	}
	minted, err := fixture.Provider.Mint(spec)
	if err != nil {
		t.Fatalf("factory: mint OIDC access token: %v", err)
	}
	return minted.Value
}

// oidcResolverConfig binds the opt-in OIDC principal resolver.
type oidcResolverConfig struct {
	Verifier   identity.OIDCVerifier
	Static     authorize.CurrentPrincipal
	Principals identity.PrincipalReader
	Now        func() time.Time
}

// oidcPrincipalResolver verifies JWT-shaped credentials through the real OIDC
// verifier and then re-reads the current committed membership. Verification is
// only an identity hint: the returned roles, enablement and version always come
// from the current reader, so token role claims are never authority.
//
// A credential that is not JWT-shaped (the generated static development tokens
// and the derived kodex/kodsvc execution and service credentials) is delegated
// to the existing authenticator. A failed JWT never falls back: a malformed,
// forged, expired or out-of-scope token is a refusal even when the static
// authenticator would accept the same string.
type oidcPrincipalResolver struct {
	verifier   identity.OIDCVerifier
	static     authorize.CurrentPrincipal
	principals identity.PrincipalReader
	now        func() time.Time
}

var _ authorize.CurrentPrincipal = (*oidcPrincipalResolver)(nil)

// newOIDCPrincipalResolver builds the resolver with every dependency present.
func newOIDCPrincipalResolver(config oidcResolverConfig) (*oidcPrincipalResolver, error) {
	switch {
	case config.Verifier == nil:
		return nil, fault.New(fault.InvalidInput, "OIDC principal resolver requires a verifier")
	case config.Static == nil:
		return nil, fault.New(fault.InvalidInput, "OIDC principal resolver requires a static authenticator")
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "OIDC principal resolver requires a principal reader")
	case config.Now == nil:
		return nil, fault.New(fault.InvalidInput, "OIDC principal resolver requires a clock")
	}
	return &oidcPrincipalResolver{
		verifier:   config.Verifier,
		static:     config.Static,
		principals: config.Principals,
		now:        config.Now,
	}, nil
}

// Resolve authenticates one presented credential.
func (resolver *oidcPrincipalResolver) Resolve(ctx context.Context, presented string) (authorize.Principal, error) {
	if err := ctx.Err(); err != nil {
		return authorize.Principal{}, fault.Wrap(fault.Unauthenticated, "credential authentication was canceled", err)
	}
	if !isJWTCredential(presented) {
		return resolver.static.Resolve(ctx, presented)
	}
	now := resolver.now()
	if now.IsZero() {
		return authorize.Principal{}, oidcUnavailable()
	}
	verified, err := resolver.verifier.Verify(ctx, identity.OIDCRequest{AccessToken: presented, Now: now})
	if err != nil {
		return authorize.Principal{}, oidcRefusal(err)
	}
	if err := verified.Validate(); err != nil {
		return authorize.Principal{}, oidcRefusal(err)
	}
	if !verified.Enabled || verified.Kind != identity.KindHuman {
		return authorize.Principal{}, oidcUnauthenticated()
	}
	current, err := identity.Current(ctx, resolver.principals, identity.PrincipalRequest{
		Scope:       verified.Scope,
		PrincipalID: verified.ID,
	})
	if err != nil {
		return authorize.Principal{}, oidcRefusal(err)
	}
	if err := identity.CheckCurrent(verified, current); err != nil {
		return authorize.Principal{}, oidcRefusal(err)
	}
	if current.Kind != identity.KindHuman {
		return authorize.Principal{}, oidcUnauthenticated()
	}
	principal := current
	principal.Roles = append([]tenancy.Role(nil), current.Roles...)
	if err := principal.Validate(); err != nil {
		return authorize.Principal{}, oidcUnauthenticated()
	}
	return principal, nil
}

// isJWTCredential reports whether a presented value must be decided by the OIDC
// verifier instead of the static authenticator. The two derived credential
// prefixes carry a dotted MAC segment but are never JWTs, so they are excluded
// before the three-segment JWT shape is tested.
func isJWTCredential(presented string) bool {
	if strings.HasPrefix(presented, token.ExecutionPrefix) || strings.HasPrefix(presented, token.ServicePrefix) {
		return false
	}
	return strings.Count(presented, ".") == 2
}

// oidcRefusal maps a verifier or reader error to the uniform unauthenticated
// refusal while keeping a dependency failure distinct. The private cause stays
// reachable through errors.Is/errors.As but the token is never rendered.
func oidcRefusal(err error) error {
	if class, ok := fault.ClassOf(err); ok {
		switch class {
		case fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.InternalFailure:
			return fault.Wrap(fault.DependencyUnavailable, "credential verification is unavailable", err)
		}
	}
	return fault.Wrap(fault.Unauthenticated, "presented credential is invalid", err)
}

// oidcUnauthenticated returns the uniform credential refusal.
func oidcUnauthenticated() error {
	return fault.New(fault.Unauthenticated, "presented credential is invalid")
}

// oidcUnavailable returns the fail-closed dependency refusal.
func oidcUnavailable() error {
	return fault.New(fault.DependencyUnavailable, "credential verification is unavailable")
}
