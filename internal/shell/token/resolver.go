// Pre-authentication credential bootstrap.
//
// This is the one authorize.CurrentPrincipal implementation. It lives in the
// token package because that package already owns derived-credential
// authentication (Manager.Authenticate and the tenant-key/credential readers);
// identity stays the OIDC access-token verifier and current-principal reader.
// Keeping the orchestration beside the verifier it drives avoids a second
// authentication boundary and an identity -> token import cycle.
//
// A presented derived token carries only a kind and an identity hint. The
// resolver first asks the store's read-only credential lookup for a CANDIDATE
// tenant/project/principal scope, then hands the candidate scope and the
// presented token to Manager.Authenticate, which verifies the MAC against the
// candidate tenant's current derivation key and re-reads the current
// nonce/expiry, principal and service state inside that scope. The candidate is
// never authority: a lookup miss, a wrong kind, a bad MAC, an expired or revoked
// credential, a disabled service, a rotated nonce and a candidate scope the MAC
// cannot confirm are all the same public unauthenticated refusal.
//
// A token that is not a kodex/kodsvc credential is a human bearer token and is
// verified through the existing OIDC verifier plus the current-principal reader.
// Token material is never logged, returned or rendered by an error, and a
// lookup miss still performs one dummy HMAC verification so the refusal is not
// structured around whether the identity exists.
package token

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// CredentialAuthenticator is the derived-credential authentication boundary the
// resolver drives. *Manager implements it; a test can substitute a recording
// fake.
type CredentialAuthenticator interface {
	Authenticate(context.Context, AuthenticateRequest) (Authenticated, error)
}

// ResolverConfig binds every pre-authentication dependency.
type ResolverConfig struct {
	Lookup      store.CredentialLookup
	Credentials CredentialAuthenticator
	OIDC        identity.OIDCVerifier
	Principals  identity.PrincipalReader
	Clock       Clock
}

// Resolver authenticates one presented bearer credential. It holds no cached
// authority and no mutable authentication state, so concurrent calls re-read
// current state independently.
type Resolver struct {
	lookup      store.CredentialLookup
	credentials CredentialAuthenticator
	oidc        identity.OIDCVerifier
	principals  identity.PrincipalReader
	clock       Clock
	dummyKey    []byte
	dummyInput  []byte
}

// The two public refusals of this boundary. Every authentication failure uses
// the same class and message so the boundary cannot be used to discover whether
// a token identity exists, and dependency failures fail closed without exposing
// their private cause.
const (
	resolverRefusalMessage     = "presented credential is invalid"
	resolverUnavailableMessage = "credential authentication is unavailable"
)

var _ authorize.CurrentPrincipal = (*Resolver)(nil)

// NewResolver builds a resolver with every dependency present.
func NewResolver(config ResolverConfig) (*Resolver, error) {
	switch {
	case config.Lookup == nil:
		return nil, fault.New(fault.InvalidInput, "credential resolver requires a credential lookup")
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "credential resolver requires a credential authenticator")
	case config.OIDC == nil:
		return nil, fault.New(fault.InvalidInput, "credential resolver requires an OIDC verifier")
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "credential resolver requires a principal reader")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "credential resolver requires a clock")
	}
	// The dummy key and canonical input exist only to spend the same HMAC work
	// on a lookup miss as on a hit. No token can authenticate against them and
	// the comparison result is discarded.
	sum := sha256.Sum256([]byte("kodama-credential-resolver-dummy-key-v1"))
	dummyInput, err := encodeMACInput(ExecutionDomain, "tenant-dummy", "execution-dummy",
		make([]byte, MinNonceBytes), time.Unix(1, 0))
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "credential resolver dummy input is invalid", err)
	}
	return &Resolver{
		lookup:      config.Lookup,
		credentials: config.Credentials,
		oidc:        config.OIDC,
		principals:  config.Principals,
		clock:       config.Clock,
		dummyKey:    sum[:],
		dummyInput:  dummyInput,
	}, nil
}

// Resolve authenticates one presented credential. A kodex/kodsvc token is
// bootstrapped through the candidate lookup and then verified in that candidate
// scope; anything else is a human bearer token verified through OIDC. The
// returned principal carries current roles, enablement and version, never token
// claims.
func (resolver *Resolver) Resolve(ctx context.Context, presented string) (authorize.Principal, error) {
	if presented == "" {
		resolver.dummyWork(presented)
		return authorize.Principal{}, resolverRefusal()
	}
	switch {
	case strings.HasPrefix(presented, ExecutionPrefix):
		credentialID, ok := presentedID(strings.TrimPrefix(presented, ExecutionPrefix))
		if !ok {
			resolver.dummyWork(presented)
			return authorize.Principal{}, resolverRefusal()
		}
		return resolver.resolveDerived(ctx, presented, store.CredentialExecution, credentialID)
	case strings.HasPrefix(presented, ServicePrefix):
		credentialID, ok := presentedID(strings.TrimPrefix(presented, ServicePrefix))
		if !ok {
			resolver.dummyWork(presented)
			return authorize.Principal{}, resolverRefusal()
		}
		return resolver.resolveDerived(ctx, presented, store.CredentialService, credentialID)
	default:
		return resolver.resolveBearer(ctx, presented)
	}
}

// resolveDerived bootstraps a candidate scope from the lookup and verifies the
// derived credential inside it. No scoped read happens before the lookup.
func (resolver *Resolver) resolveDerived(
	ctx context.Context,
	presented string,
	kind store.CredentialKind,
	credentialID string,
) (authorize.Principal, error) {
	candidate, err := resolver.lookup.LookupCredential(ctx, kind, credentialID)
	if err != nil {
		resolver.dummyWork(presented)
		if class, ok := fault.ClassOf(err); ok {
			switch class {
			case fault.Unauthenticated, fault.Forbidden, fault.InvalidInput:
				return authorize.Principal{}, resolverRefusal()
			}
		}
		return authorize.Principal{}, resolverUnavailable()
	}
	if err := candidate.Validate(); err != nil {
		resolver.dummyWork(presented)
		return authorize.Principal{}, resolverUnavailable()
	}
	scope := tenancy.Scope{TenantID: candidate.TenantID, ProjectID: candidate.ProjectID}
	if err := scope.Validate(); err != nil {
		resolver.dummyWork(presented)
		return authorize.Principal{}, resolverUnavailable()
	}
	now := resolver.clock.Now()
	if now.IsZero() {
		return authorize.Principal{}, resolverUnavailable()
	}
	request := AuthenticateRequest{
		Scope:       scope,
		Presented:   presented,
		PrincipalID: candidate.PrincipalID,
		Now:         now,
	}
	switch kind {
	case store.CredentialExecution:
		request.ExecutionID = id.ExecutionID(credentialID)
	case store.CredentialService:
		request.ServiceID = service.ServiceID(credentialID)
	default:
		resolver.dummyWork(presented)
		return authorize.Principal{}, resolverUnavailable()
	}
	authenticated, err := resolver.credentials.Authenticate(ctx, request)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok {
			switch class {
			case fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.InternalFailure:
				return authorize.Principal{}, resolverUnavailable()
			}
		}
		return authorize.Principal{}, resolverRefusal()
	}
	return authenticatedPrincipal(authenticated, kind, credentialID, candidate, scope)
}

// authenticatedPrincipal re-checks the Manager's result against the candidate
// scope. The Manager already verified the MAC and current state; this is the
// defense-in-depth confirmation that the returned identity is exactly the
// candidate the MAC was verified in.
func authenticatedPrincipal(
	authenticated Authenticated,
	kind store.CredentialKind,
	credentialID string,
	candidate store.CandidateScope,
	scope tenancy.Scope,
) (authorize.Principal, error) {
	if !authenticated.Enabled {
		return authorize.Principal{}, resolverRefusal()
	}
	if authenticated.Scope != scope || authenticated.PrincipalID != candidate.PrincipalID {
		return authorize.Principal{}, resolverRefusal()
	}
	switch kind {
	case store.CredentialExecution:
		if authenticated.Kind != identity.KindExecution ||
			authenticated.ExecutionID != id.ExecutionID(credentialID) ||
			authenticated.ServiceID != "" {
			return authorize.Principal{}, resolverRefusal()
		}
	case store.CredentialService:
		if authenticated.Kind != identity.KindService ||
			authenticated.ServiceID != service.ServiceID(credentialID) ||
			authenticated.ExecutionID != "" ||
			authenticated.PrincipalID != id.PrincipalID(credentialID) {
			return authorize.Principal{}, resolverRefusal()
		}
	default:
		return authorize.Principal{}, resolverRefusal()
	}
	principal := authorize.Principal{
		ID:          authenticated.PrincipalID,
		Kind:        authenticated.Kind,
		Scope:       authenticated.Scope,
		Roles:       append([]tenancy.Role(nil), authenticated.Roles...),
		Enabled:     authenticated.Enabled,
		Version:     authenticated.Version,
		ExecutionID: authenticated.ExecutionID,
	}
	if err := principal.Validate(); err != nil {
		return authorize.Principal{}, resolverRefusal()
	}
	return principal, nil
}

// resolveBearer verifies an opaque human bearer token and re-reads current
// authority. The token's verified scope is a hint for the current read; a
// removed, disabled or changed principal fails closed.
func (resolver *Resolver) resolveBearer(ctx context.Context, presented string) (authorize.Principal, error) {
	now := resolver.clock.Now()
	if now.IsZero() {
		return authorize.Principal{}, resolverUnavailable()
	}
	verified, err := resolver.oidc.Verify(ctx, identity.OIDCRequest{AccessToken: presented, Now: now})
	if err != nil {
		return authorize.Principal{}, resolverFailure(err)
	}
	if err := verified.Validate(); err != nil {
		return authorize.Principal{}, resolverRefusal()
	}
	if !verified.Enabled || verified.Kind != identity.KindHuman {
		return authorize.Principal{}, resolverRefusal()
	}
	current, err := identity.Current(ctx, resolver.principals, identity.PrincipalRequest{
		Scope:       verified.Scope,
		PrincipalID: verified.ID,
	})
	if err != nil {
		return authorize.Principal{}, resolverFailure(err)
	}
	if err := identity.CheckCurrent(verified, current); err != nil {
		return authorize.Principal{}, resolverRefusal()
	}
	if current.Kind != identity.KindHuman {
		return authorize.Principal{}, resolverRefusal()
	}
	principal := current
	principal.Roles = append([]tenancy.Role(nil), current.Roles...)
	return principal, nil
}

// resolverFailure maps a dependency error to the fail-closed unavailable class
// and every authentication-shaped error to the one uniform refusal. The private
// cause is never rendered.
func resolverFailure(err error) error {
	if class, ok := fault.ClassOf(err); ok {
		switch class {
		case fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.InternalFailure:
			return resolverUnavailable()
		}
	}
	return resolverRefusal()
}

// dummyWork spends one HMAC and one constant-time comparison on a lookup miss or
// malformed token. The result is discarded: it exists only so a refusal does not
// branch on whether the identity was found.
func (resolver *Resolver) dummyWork(presented string) {
	mac, err := deriveMAC(resolver.dummyKey, resolver.dummyInput)
	if err != nil {
		return
	}
	expected := base64.RawURLEncoding.EncodeToString(mac)
	observed := presented
	if index := strings.LastIndexByte(observed, '.'); index >= 0 {
		observed = observed[index+1:]
	}
	_ = subtle.ConstantTimeCompare([]byte(observed), []byte(expected))
}

// presentedID extracts the identity hint from the token body. The MAC segment
// is not interpreted here; Manager.Authenticate verifies it.
func presentedID(body string) (string, bool) {
	segments := strings.Split(body, ".")
	if len(segments) != 2 || segments[0] == "" {
		return "", false
	}
	return segments[0], true
}

func resolverRefusal() error {
	return fault.New(fault.Unauthenticated, resolverRefusalMessage)
}

func resolverUnavailable() error {
	return fault.New(fault.DependencyUnavailable, resolverUnavailableMessage)
}
