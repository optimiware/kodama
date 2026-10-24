package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// permissions maps the profile permissions onto the existing fixed-role
// permissions: reads need run:read and registration needs run:manage. No new
// role or permission is introduced, and an unmapped permission denies.
var permissions = map[agents.Permission]identity.Permission{
	agents.PermList:     identity.PermRunRead,
	agents.PermRegister: identity.PermRunManage,
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return agents.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return agents.ErrDenied
	}
	return agents.Unavailablef(err, "the identity authority is unavailable")
}

// Authority implements agents.Authority over the selected identity authority and
// the current-binding reader (for actor resolution): a principal is a user ID
// and is hidden from tenants where it holds no binding. Only users hold roles:
// the identity policy denies every workload, service and operator actor, so an
// agent identity can never satisfy a permission, human-only or not.
type Authority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ agents.Authority = (*Authority)(nil)

// NewAuthority requires both collaborators; there is no default grant.
func NewAuthority(a identity.Authority, r identity.AccessReader) (*Authority, error) {
	if a == nil || r == nil {
		return nil, errors.New("agents/adapters: an authority and an access reader are required")
	}
	return &Authority{authority: a, reader: r}, nil
}

// ResolveActor implements agents.Authority.
func (a *Authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (agents.Actor, error) {
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return agents.Actor{}, agents.ErrHidden
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return agents.Actor{}, translate(err)
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	for _, b := range bindings {
		if b.Applies(target) {
			return agents.Actor{ID: principalID, Type: agents.ActorUser}, nil
		}
	}
	return agents.Actor{}, agents.ErrHidden
}

// Authorize implements agents.Authority.
func (a *Authority) Authorize(ctx context.Context, actor agents.Actor, scope domain.Scope, perm agents.Permission) error {
	p, ok := permissions[perm]
	if !ok {
		return agents.ErrDenied // unknown permissions deny by default
	}
	ia := identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)}
	return translate(a.authority.Authorize(ctx, ia, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p))
}
