package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// HumanAuthority implements mcpproxy.HumanAuthority over the selected identity
// authority and the current-binding reader. Only users hold roles, so a
// workload, service or operator principal can never satisfy a management
// permission.
//
// Mapping, using existing permissions only (no new role or permission):
//
//	mcp_server.list   -> project:read
//	mcp_server.mutate -> project:settings AND credential:manage
//
// A mutation binds a project credential to an endpoint, so it needs both.
type HumanAuthority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ mcpproxy.HumanAuthority = (*HumanAuthority)(nil)

// NewHumanAuthority requires both collaborators; there is no default grant.
func NewHumanAuthority(a identity.Authority, r identity.AccessReader) (*HumanAuthority, error) {
	if a == nil || r == nil {
		return nil, errors.New("mcpproxy/adapters: an authority and an access reader are required")
	}
	return &HumanAuthority{authority: a, reader: r}, nil
}

var permissions = map[mcpproxy.Permission][]identity.Permission{
	mcpproxy.PermList:   {identity.PermProjectRead},
	mcpproxy.PermMutate: {identity.PermProjectSettings, identity.PermCredentialManage},
}

// Authorize implements mcpproxy.HumanAuthority. A principal with no binding in
// the project is hidden (not found), exactly as for other project resources.
func (a *HumanAuthority) Authorize(ctx context.Context, principalID string, scope domain.Scope, perm mcpproxy.Permission) error {
	needed, ok := permissions[perm]
	if !ok {
		return mcpproxy.ErrDenied // unknown permissions deny by default
	}
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return mcpproxy.ErrNotFound
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return mcpproxy.Unavailablef(err, "the identity authority is unavailable")
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	applies := false
	for _, b := range bindings {
		if b.Applies(target) {
			applies = true
			break
		}
	}
	if !applies {
		return mcpproxy.ErrNotFound
	}
	actor := identity.Actor{ID: principalID, Type: identity.ActorUser}
	for _, p := range needed {
		switch err := a.authority.Authorize(ctx, actor, target, p); {
		case err == nil:
		case errors.Is(err, identity.ErrHidden):
			return mcpproxy.ErrNotFound
		case errors.Is(err, identity.ErrDenied):
			return mcpproxy.ErrDenied
		default:
			return mcpproxy.Unavailablef(err, "the identity authority is unavailable")
		}
	}
	return nil
}
