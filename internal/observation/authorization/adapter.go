// Package authorization adapts the identity module's effective-access Authority
// to the observation module's Access port. It copies no role grants and has no
// fallback: the installation's selected authority answers every question, and an
// authority that cannot answer is an outage, never access.
package authorization

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/observation"
)

// Permissions maps each observed family to the existing permission that opens
// it. Work observations need work_item:read; run and step-attempt observations
// need run:read. The mapping is explicit so a new family cannot become
// observable by accident.
var Permissions = map[observation.Family]identity.Permission{
	observation.FamilyWork:        identity.PermWorkRead,
	observation.FamilyRun:         identity.PermRunRead,
	observation.FamilyStepAttempt: identity.PermRunRead,
}

// Adapter implements observation.Access over identity.Authority.
type Adapter struct {
	authority identity.Authority
}

var _ observation.Access = (*Adapter)(nil)

// NewAdapter requires the authority selected for the installation (see
// identity.SelectAuthority).
func NewAdapter(a identity.Authority) (*Adapter, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: an identity authority is required", observation.ErrInvalidConfiguration)
	}
	return &Adapter{authority: a}, nil
}

// Authorize implements observation.Access. The target is the scope's project;
// the authority reads current bindings on every call.
func (a *Adapter) Authorize(ctx context.Context, actor observation.Actor, scope observation.Scope, family observation.Family) error {
	perm, ok := Permissions[family]
	if !ok {
		return observation.ErrDenied // an unmapped family is never observable
	}
	err := a.authority.Authorize(ctx, identity.UserActor(identity.UserID(actor.ID)),
		identity.Target{TenantID: scope.Tenant, ProjectID: scope.Project}, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return observation.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return observation.ErrDenied
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return observation.Unavailablef(err, "the authority could not answer")
}
