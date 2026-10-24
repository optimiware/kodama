package httpapi

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RateKey identifies the budget a request draws from: the verified principal,
// the claimed scope and the action. Scopes are never merged across tenants.
type RateKey struct {
	Principal Principal
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	Action    Action
}

// RateDecision is the limiter's answer. RetryAfter is meaningful only when
// Allowed is false and is rounded up to at least one second.
type RateDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RateLimiter is the throttling port (AC-11). Implementations decide the
// algorithm and storage. An error is treated as a refusal (fail closed) so a
// limiter outage cannot become unbounded load.
type RateLimiter interface {
	Allow(ctx context.Context, key RateKey) (RateDecision, error)
}

// Unlimited returns a limiter that always allows. Using it is a deliberate,
// visible choice in the composition root: the router has no default limiter.
func Unlimited() RateLimiter { return unlimited{} }

type unlimited struct{}

func (unlimited) Allow(context.Context, RateKey) (RateDecision, error) {
	return RateDecision{Allowed: true}, nil
}
