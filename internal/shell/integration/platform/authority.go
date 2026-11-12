// Package platform implements the OAuth-shaped hosted-authority adapter
// declared by the authorize.HostedAuthority seam.
//
// One Authority is configured with an application source. The source supplies
// the current application list and the verified token's current claim values;
// the declared authorize.HostedAuthorityRequest carries the operation's
// configured claim match, not the token values. The adapter matches exactly one
// application, then checks that application's own RBAC table. RBAC is never
// merged across applications and never cached.
package platform

import (
	"context"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
)

// Application is one OAuth application's current configuration.
type Application struct {
	Name   string
	Client identity.OAuthClientConfig
	Claims identity.OAuthClaims
	RBAC   authorize.RBACTable
}

// Snapshot is one current hosted-authority view: the configured applications
// plus the verified token's current claim values.
type Snapshot struct {
	Applications []Application
	ClaimValues  map[string][]string
}

// Source reads the current hosted-authority snapshot for one check.
type Source interface {
	Snapshot(context.Context, authorize.HostedAuthorityRequest) (Snapshot, error)
}

// Config is the hosted-authority adapter configuration.
type Config struct {
	Source Source
	Logf   func(format string, args ...any)
}

// Authority is the concrete authorize.HostedAuthority adapter.
type Authority struct {
	source Source
	logf   func(format string, args ...any)
}

// NewAuthority validates the configuration and builds an authority.
//
// The declared authorize.HostedAuthority seam is the constructor result so an
// invalid configuration returns a genuinely nil seam value rather than a typed
// nil pointer.
func NewAuthority(config Config) (authorize.HostedAuthority, error) {
	if config.Source == nil {
		return nil, fault.New(fault.InvalidInput, "hosted authority source is nil")
	}
	return &Authority{source: config.Source, logf: config.Logf}, nil
}

// Check validates the declared request, reads one current snapshot and returns
// a normal refusal or an allowed decision.
//
// A policy refusal is a response with Allowed=false and no error. Invalid input
// is invalid_input; every source or snapshot failure is dependency_unavailable.
// A failure is never an allowed decision and never falls back to standalone
// grants.
func (authority *Authority) Check(ctx context.Context, request authorize.HostedAuthorityRequest) (authorize.HostedAuthorityResponse, error) {
	if err := request.Validate(); err != nil {
		return authorize.HostedAuthorityResponse{}, err
	}

	response := authorize.HostedAuthorityResponse{Protocol: request.Protocol}

	if err := ctx.Err(); err != nil {
		return response, fault.Wrap(fault.DependencyUnavailable, "hosted authority is unavailable", err)
	}

	snapshot, err := authority.source.Snapshot(ctx, request)
	if err != nil {
		return response, fault.Wrap(fault.DependencyUnavailable, "hosted authority is unavailable", err)
	}
	if err := ctx.Err(); err != nil {
		return response, fault.Wrap(fault.DependencyUnavailable, "hosted authority is unavailable", err)
	}

	if err := validateSnapshot(snapshot); err != nil {
		return response, err
	}

	tenantValues, ok := snapshot.ClaimValues[request.Claims.TenantClaim]
	if !ok || len(tenantValues) != 1 || tenantValues[0] == "" || tenantValues[0] != request.Claims.Tenant {
		return response, nil
	}

	roleValues, ok := snapshot.ClaimValues[request.Claims.RoleClaim]
	if !ok || !containsValue(roleValues, request.Claims.Role) {
		return response, nil
	}

	application, ok := matchApplication(snapshot.Applications, request)
	if !ok {
		return response, nil
	}

	access, ok := application.RBAC.Lookup(request.Claims.Role)
	if !ok {
		return response, nil
	}
	for _, required := range request.RequiredAccess {
		if !containsValue(access, required) {
			return response, nil
		}
	}

	response.Decision = authorize.HostedAuthorityDecision{
		Allowed: true,
		Role:    request.Claims.Role,
		Access:  access,
	}
	return response, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if len(snapshot.Applications) == 0 {
		return fault.New(fault.DependencyUnavailable, "hosted authority snapshot has no applications")
	}
	if snapshot.ClaimValues == nil {
		return fault.New(fault.DependencyUnavailable, "hosted authority snapshot has no claim values")
	}
	for _, application := range snapshot.Applications {
		if err := application.Client.Validate(); err != nil {
			return fault.New(fault.DependencyUnavailable, "hosted authority application client is invalid")
		}
		if err := application.Claims.Validate(); err != nil {
			return fault.New(fault.DependencyUnavailable, "hosted authority application claims are invalid")
		}
		if err := application.RBAC.Validate(); err != nil {
			return fault.New(fault.DependencyUnavailable, "hosted authority application RBAC is invalid")
		}
	}
	return nil
}

func matchApplication(applications []Application, request authorize.HostedAuthorityRequest) (Application, bool) {
	var matched Application
	count := 0
	for _, application := range applications {
		if application.Client.Protocol != request.Protocol {
			continue
		}
		if application.Claims.TenantClaim != request.Claims.TenantClaim ||
			application.Claims.Tenant != request.Claims.Tenant ||
			application.Claims.RoleClaim != request.Claims.RoleClaim ||
			application.Claims.Role != request.Claims.Role {
			continue
		}
		matched = application
		count++
	}
	if count != 1 {
		return Application{}, false
	}
	return matched, true
}

func containsValue(values []string, wanted string) bool {
	return slices.Contains(values, wanted)
}
