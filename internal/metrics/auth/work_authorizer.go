// Package metricsauth adapts the work module's authority contract to the
// metrics read port. It introduces no role map and no hosted-access policy: the
// decision is the identity authority's answer for work_item:read.
package metricsauth

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	"github.com/optimiware/kodama/internal/work"
)

// WorkReadAuthorizer implements metrics.ReadAuthorizer over work.Authority.
type WorkReadAuthorizer struct{ authority work.Authority }

var _ metrics.ReadAuthorizer = WorkReadAuthorizer{}

// NewWorkReadAuthorizer requires the authority; there is no permissive default.
func NewWorkReadAuthorizer(a work.Authority) (WorkReadAuthorizer, error) {
	if a == nil {
		return WorkReadAuthorizer{}, errors.New("metricsauth: an authority is required")
	}
	return WorkReadAuthorizer{authority: a}, nil
}

// AuthorizeRead resolves the principal and checks work_item:read in the scope.
// Denied and hidden outcomes keep their classes; every other failure is an
// outage, never access.
func (a WorkReadAuthorizer) AuthorizeRead(ctx context.Context, scope domain.Scope, principalID string) error {
	if a.authority == nil {
		return metrics.Unavailablef(nil, "the authority is not configured")
	}
	actor, err := a.authority.ResolveActor(ctx, scope, principalID)
	if err != nil {
		return classify(err)
	}
	if err := actor.Validate(); err != nil {
		return metrics.Unavailablef(err, "the authority returned an unusable actor")
	}
	return classify(a.authority.Authorize(ctx, actor, scope, work.PermRead))
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, work.ErrDenied):
		return metrics.ErrDenied
	case errors.Is(err, work.ErrHidden):
		return metrics.ErrHidden
	}
	return metrics.Unavailablef(err, "the authority failed")
}
