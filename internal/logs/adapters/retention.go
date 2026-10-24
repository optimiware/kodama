package adapters

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/logs"
)

// Retention implements artifacts.Retention for execution logs: diagnostic
// files (command logs, controller captures and their sidecars) become eligible
// for deletion after the 30-day O-11 window unless a protection applies. Every
// other artifact role is not this policy's to delete. The artifact module's
// reconciliation remains the only mechanism that deletes bytes and remains
// serialized against pins; this policy only answers eligibility.
//
// Composition chains policies for other roles; this one never makes a
// non-diagnostic artifact eligible.
type Retention struct{ policy logs.RetentionPolicy }

var _ artifacts.Retention = Retention{}

// NewRetention requires a protection policy: with none, nothing is eligible.
func NewRetention(window time.Duration, protection logs.Protection) (Retention, error) {
	p, err := logs.NewRetentionPolicy(window, protection)
	if err != nil {
		return Retention{}, errors.New("logs/adapters: a retention protection policy is required")
	}
	return Retention{policy: p}, nil
}

// Eligible implements artifacts.Retention. An unestablishable protection state
// is an error (the reconciler deletes nothing for that artifact), never
// eligibility.
func (r Retention) Eligible(ctx context.Context, s artifacts.RetentionSubject, now time.Time) (bool, error) {
	if s.Kind != artifacts.KindFile || s.Role != artifacts.RoleDiagnostic {
		return false, nil
	}
	return r.policy.Eligible(ctx, s.Scope, s.ID, s.VerifiedAt, now)
}
