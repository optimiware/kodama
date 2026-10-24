package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Resolver is the delivery use case that retains an immutable environment
// snapshot. *delivery.Service implements it.
type Resolver interface {
	ResolveSnapshot(ctx context.Context, scope domain.Scope, actor delivery.Actor, id domain.EnvironmentID) (delivery.ConfigurationSnapshot, error)
}

// Environments implements execution.EnvironmentResolver. It checks the actor's
// current access, resolves the environment's credential references through the
// secrets owner, retains the immutable snapshot and returns its pin and exact
// credential bindings. It is the only place current credential versions are
// read; existing snapshots are never replaced.
type Environments struct{ svc Resolver }

var _ execution.EnvironmentResolver = (*Environments)(nil)

// NewEnvironments requires the delivery service.
func NewEnvironments(svc Resolver) (*Environments, error) {
	if svc == nil {
		return nil, errors.New("delivery/adapters: a delivery service is required")
	}
	return &Environments{svc: svc}, nil
}

// ResolveEnvironment implements execution.EnvironmentResolver. Denied, hidden,
// missing or unavailable environments and credentials refuse the admission;
// anything else is unavailable so the admission can be retried.
func (e *Environments) ResolveEnvironment(ctx context.Context, req execution.EnvironmentRequest) (execution.EnvironmentSnapshot, error) {
	if err := req.Scope.Validate(); err != nil {
		return execution.EnvironmentSnapshot{}, execution.Refusedf("the environment request is not valid")
	}
	if err := req.Actor.Validate(); err != nil {
		return execution.EnvironmentSnapshot{}, execution.Refusedf("the environment request is not valid")
	}
	actor := delivery.Actor{Type: delivery.ActorType(req.Actor.Type), ID: req.Actor.ID}
	snap, err := e.svc.ResolveSnapshot(ctx, req.Scope, actor, req.EnvironmentID)
	if err != nil {
		switch {
		case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrDenied), errors.Is(err, delivery.ErrInvalid),
			errors.Is(err, delivery.ErrCredentialUnavailable):
			return execution.EnvironmentSnapshot{}, execution.Refusedf("the declared environment is not admissible")
		}
		return execution.EnvironmentSnapshot{}, execution.Unavailablef(err, "environment resolution is unavailable")
	}
	return execution.EnvironmentSnapshot{Scope: snap.Scope, Pin: snap.Pin(), Secrets: append(snap.Secrets[:0:0], snap.Secrets...)}, nil
}
