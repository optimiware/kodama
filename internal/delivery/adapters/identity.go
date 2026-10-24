// Package adapters binds the delivery module's consumer-owned ports to the
// identity, secrets, execution, artifact and runtime-authority modules. Nothing
// here holds business rules: each adapter translates identifiers and failure
// classes between two owners and keeps peer types out of the domain.
package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// Access implements delivery.Access over the selected identity authority. It
// reads current bindings on every call; an unknown permission, a non-human actor
// and a missing binding deny, and an outage is never an allow.
type Access struct{ authority identity.Authority }

var _ delivery.Access = (*Access)(nil)

// NewAccess requires the authority; there is no default grant.
func NewAccess(a identity.Authority) (*Access, error) {
	if a == nil {
		return nil, errors.New("delivery/adapters: an identity authority is required")
	}
	return &Access{authority: a}, nil
}

var permissions = map[delivery.Permission]identity.Permission{
	delivery.PermRunRead:          identity.PermRunRead,
	delivery.PermDeploymentManage: identity.PermDeploymentManage,
}

// Authorize implements delivery.Access.
func (a *Access) Authorize(ctx context.Context, actor delivery.Actor, scope domain.Scope, perm delivery.Permission) error {
	p, ok := permissions[perm]
	if !ok {
		return delivery.Fail(delivery.ErrDenied, nil, "the permission is not known")
	}
	ia := identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)}
	err := a.authority.Authorize(ctx, ia, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return delivery.Fail(delivery.ErrNotFound, err, "not found")
	case errors.Is(err, identity.ErrDenied):
		return delivery.Fail(delivery.ErrDenied, err, "the caller lacks the permission")
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "the identity authority is unavailable")
}
