// Package identityhttp is the HTTP adapter of the identity module: the bearer
// authenticator and the route authorizer that the shared router injects, and the
// discovery, membership and project handlers. It reuses the shared router,
// strict JSON decoding, ETags, scoped cursors and error envelopes of
// internal/httpapi and contains no persistence or token-verification code.
package identityhttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
)

// Authenticator implements httpapi.Authenticator: it verifies the bearer access
// token and maps the verified (issuer, subject) to the stable user ID, which is
// the principal ID every later layer sees. Membership is not consulted: a valid
// credential identifies a person and grants nothing.
type Authenticator struct {
	verifier identity.CredentialVerifier
	mapper   identity.PrincipalMapper
}

var _ httpapi.Authenticator = (*Authenticator)(nil)

// NewAuthenticator requires both collaborators.
func NewAuthenticator(v identity.CredentialVerifier, m identity.PrincipalMapper) (*Authenticator, error) {
	if v == nil || m == nil {
		return nil, errors.New("identityhttp: a credential verifier and a principal mapper are required")
	}
	return &Authenticator{verifier: v, mapper: m}, nil
}

// Authenticate reads the single Authorization: Bearer credential. Cookies, query
// parameters and other headers are never credentials. A bad credential is
// httpapi.ErrUnauthenticated; verification or mapping outages are returned as
// they are (the router renders 503, never access).
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (httpapi.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	var delegation *identity.Delegation
	var p identity.Principal
	if rich, ok := a.verifier.(identity.AccessTokenVerifier); ok {
		verified, err := rich.VerifyAccessToken(ctx, token)
		if err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				return httpapi.Principal{}, httpapi.ErrUnauthenticated
			}
			return httpapi.Principal{}, err
		}
		p, delegation = verified.Principal, verified.Delegation
	} else {
		var err error
		p, err = a.verifier.Verify(ctx, token)
		if err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				return httpapi.Principal{}, httpapi.ErrUnauthenticated
			}
			return httpapi.Principal{}, err
		}
	}
	id, err := a.mapper.MapPrincipal(ctx, p)
	if err != nil {
		return httpapi.Principal{}, err
	}
	principal := httpapi.Principal{ID: string(id)}
	if delegation != nil {
		principal.DelegatedBy = delegation.Actor
		principal.DelegatedScopes = strings.Join(delegation.Scopes, " ")
	}
	return principal, nil
}

// Level says which part of the target a rule needs.
type Level int

const (
	// LevelPrincipal needs only a verified caller (no membership).
	LevelPrincipal Level = iota
	// LevelTenant needs the permission through a tenant-wide binding.
	LevelTenant
	// LevelProject needs the permission at the target project (tenant-wide or
	// matching project binding).
	LevelProject
	// LevelAnyScope needs the permission at tenant scope or at any project of the
	// tenant; the use case then narrows what the caller may see or change.
	LevelAnyScope
)

// Rule maps one route action to the permissions that open it. A route rule is a
// coarse gate: it keeps unrelated callers out and hides the tenant from
// non-members, but never replaces a use case's field- or transition-specific
// permission check (for example work_item:complete), which the owning module
// repeats inside its transaction through the same Authority.
type Rule struct {
	Level Level
	// Any lists permissions of which one suffices.
	Any []identity.Permission
}

// Authorizer implements httpapi.Authorizer over the effective-access Authority.
// An action without a rule is denied.
type Authorizer struct {
	authority identity.Authority
	rules     map[httpapi.Action]Rule
}

var _ httpapi.Authorizer = (*Authorizer)(nil)

// NewAuthorizer requires the authority and the action rules (see DefaultRules).
func NewAuthorizer(authority identity.Authority, rules map[httpapi.Action]Rule) (*Authorizer, error) {
	if authority == nil {
		return nil, errors.New("identityhttp: an authority is required")
	}
	if len(rules) == 0 {
		return nil, errors.New("identityhttp: authorization rules are required")
	}
	cp := make(map[httpapi.Action]Rule, len(rules))
	for a, r := range rules {
		if a == "" || (r.Level != LevelPrincipal && len(r.Any) == 0) {
			return nil, errors.New("identityhttp: a rule needs an action and, above principal level, a permission")
		}
		cp[a] = r
	}
	return &Authorizer{authority: authority, rules: cp}, nil
}

// Authorize implements httpapi.Authorizer with current state on every call.
func (a *Authorizer) Authorize(ctx context.Context, p httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	rule, ok := a.rules[action]
	if !ok {
		return httpapi.ErrDenied
	}
	if rule.Level == LevelPrincipal {
		return nil
	}
	// A delegated credential is attributable to its represented user, but the
	// intermediary is not a human reviewer. A route whose only permissions are
	// human-only (for example a gate decision) is refused here. Mixed routes
	// remain open so delegated users can perform their non-human transitions;
	// the owning use case rejects the human-only branch.
	if p.Delegated() && len(rule.Any) > 0 {
		humanOnly := true
		for _, perm := range rule.Any {
			if !perm.HumanOnly() {
				humanOnly = false
				break
			}
		}
		if humanOnly {
			return httpapi.ErrDenied
		}
	}
	if (rule.Level == LevelProject) != (t.ProjectID != "") || t.TenantID == "" {
		return httpapi.ErrDenied // the route's target does not match what its action needs
	}
	target := identity.Target{TenantID: t.TenantID, ProjectID: t.ProjectID, AnyProject: rule.Level == LevelAnyScope}
	actor := identity.UserActor(identity.UserID(p.ID))
	hidden := false
	for _, perm := range rule.Any {
		err := a.authority.Authorize(ctx, actor, target, perm)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, identity.ErrDenied):
		case errors.Is(err, identity.ErrHidden):
			hidden = true
		default:
			return err // an outage is a refusal, never access
		}
	}
	if hidden {
		return httpapi.ErrHidden
	}
	return httpapi.ErrDenied
}
