package logs

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RetentionWindow is the approved O-11 baseline for ordinary execution logs.
// Active references (pins, recovery policy) extend it; it never shortens an
// explicit protection.
const RetentionWindow = 30 * 24 * time.Hour

// Protection answers whether something other than the artifact's own pins
// requires its content to be kept: a retained-evidence reference, a recovery
// hold or a legal/operational hold. It is injected by composition; an error or
// a missing port means protection cannot be established.
type Protection interface {
	Protected(ctx context.Context, scope domain.Scope, artifact domain.ArtifactID) (bool, error)
}

// RetentionPolicy decides log-specific retention eligibility: an ordinary
// execution log or its completeness sidecar becomes eligible for deletion once
// its verified age reaches the window and no protection applies. It only
// decides eligibility. Deleting bytes remains the artifact reconciler's sole
// responsibility, serialized against pins there.
type RetentionPolicy struct {
	Window     time.Duration
	Protection Protection
}

// NewRetentionPolicy requires a protection policy: without one nothing may be
// deleted, so a missing wiring fails closed at construction.
func NewRetentionPolicy(window time.Duration, p Protection) (RetentionPolicy, error) {
	if p == nil {
		return RetentionPolicy{}, ErrProtectionUnknown
	}
	if window <= 0 {
		window = RetentionWindow
	}
	return RetentionPolicy{Window: window, Protection: p}, nil
}

// Eligible reports whether the artifact verified at verifiedAt may be deleted
// at now. A zero policy, a missing protection policy, an unknown protection
// state and a protected artifact are all "not eligible".
func (p RetentionPolicy) Eligible(ctx context.Context, scope domain.Scope, artifact domain.ArtifactID, verifiedAt, now time.Time) (bool, error) {
	if p.Protection == nil || p.Window <= 0 {
		return false, ErrProtectionUnknown
	}
	if verifiedAt.IsZero() || now.Sub(verifiedAt) < p.Window {
		return false, nil
	}
	protected, err := p.Protection.Protected(ctx, scope, artifact)
	if err != nil {
		return false, ErrProtectionUnknown
	}
	return !protected, nil
}
