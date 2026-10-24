package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// CleanupBoundary is the delivery use case execution's cleanup admission
// consumes. *delivery.Service implements it.
type CleanupBoundary interface {
	AuthorizeCleanup(ctx context.Context, scope domain.Scope, deployment string) error
}

// Cleanup implements execution.DeploymentCleanup: a cleanup run may be admitted
// only for a recorded deployment of the scope whose cleanup was requested.
type Cleanup struct{ svc CleanupBoundary }

var _ execution.DeploymentCleanup = (*Cleanup)(nil)

// NewCleanup requires the delivery service.
func NewCleanup(svc CleanupBoundary) (*Cleanup, error) {
	if svc == nil {
		return nil, errors.New("delivery/adapters: a delivery service is required")
	}
	return &Cleanup{svc: svc}, nil
}

// AuthorizeCleanup implements execution.DeploymentCleanup. A missing, foreign
// or not-requested deployment is denied; an outage is unavailable.
func (c *Cleanup) AuthorizeCleanup(ctx context.Context, scope domain.Scope, deployment string) error {
	err := c.svc.AuthorizeCleanup(ctx, scope, deployment)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrDenied):
		return execution.ErrDenied
	}
	return execution.Unavailablef(err, "the delivery record is unavailable")
}
