package installation

// Retention enforcement. The RetentionPolicy is a pure rule; this file is its
// production caller. A sweeper walks the ownership scopes that can hold
// retained rows, asks the store for rows at or beyond a family's boundary,
// re-applies MayDelete to every candidate and only then deletes the rows the
// policy proves expired.
//
// The loop is deliberately conservative:
//
//   - a scope is validated before any statement, so a malformed sweep deletes
//     nothing;
//   - a candidate with no usable creation time is kept, never guessed old;
//   - an active reference, a disabled family, a receipt kept until project
//     deletion and a future creation time are all kept by MayDelete;
//   - a candidate-scan failure keeps every row of that family and does not stop
//     another family whose expiry is independently provable;
//   - a delete failure is reported and the row is retried on the next sweep;
//   - deletion is idempotent: a row absent on the next pass is simply not a
//     candidate, and a repeated sweep at the same clock deletes nothing.
//
// Retained logs are enforced by the log FileSink (30-day read-time expiry and
// the per-stream bound). Receipts are intentionally queried only when a store
// offers them: MayDelete always keeps them until project deletion, and the
// PostgreSQL adapter returns no receipt candidates at all.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// RetentionScope identifies one tenant and, optionally, one project whose
// retained rows may be swept. An empty ProjectID is a tenant-wide scope; it is
// how tenant-wide audit metadata (project_id NULL) is reached.
type RetentionScope struct {
	TenantID  string
	ProjectID string
}

// TenantScoped reports whether the scope covers the whole tenant.
func (scope RetentionScope) TenantScoped() bool {
	return scope.ProjectID == ""
}

// Validate refuses a scope that does not name a tenant. Identity grammar is
// checked again by the store's typed tenancy scope before any statement runs.
func (scope RetentionScope) Validate() error {
	if scope.TenantID == "" {
		return fault.New(fault.InvalidInput, "retention scope has no tenant")
	}
	return nil
}

// RetentionCandidate is one stored row the sweeper may delete. CreatedAt is the
// row's creation or occurrence time; a zero value means the store could not
// establish the age and the sweeper keeps it. ActiveReferences reports a
// reference that extends the deadline (for example a verification owned by a
// running execution).
type RetentionCandidate struct {
	ID               string
	CreatedAt        time.Time
	ActiveReferences bool
}

// RetentionStore is the persistence seam the sweeper drives. Candidates must
// return only rows at or before before (a superset of the deletable set is
// allowed; MayDelete makes the final decision). Delete must remove at most one
// row, must re-check the same expiry and active-reference proof inside the
// delete, and must treat an absent row as success so a retried sweep is
// idempotent.
type RetentionStore interface {
	Candidates(ctx context.Context, scope RetentionScope, class RetentionClass, before time.Time) ([]RetentionCandidate, error)
	// Delete reports whether it removed the row. A row already absent or a
	// concurrent change that restores a live reference is not an error and
	// reports false, so the sweep counts only proven deletions.
	Delete(ctx context.Context, scope RetentionScope, class RetentionClass, candidate RetentionCandidate, before time.Time) (bool, error)
}

// RetentionSweepResult is the evidence of one sweep. The maps are keyed by the
// family name; a family with no candidates has no entry.
type RetentionSweepResult struct {
	Scopes         int
	Candidates     int
	DeletedByClass map[RetentionClass]int
	KeptByClass    map[RetentionClass]int
}

// RetentionSweeper applies the policy to the rows a RetentionStore
// offers. It holds no clock and no mutable state, so one instance is safe for
// repeated and concurrent sweeps.
type RetentionSweeper struct {
	policy  RetentionPolicy
	store   RetentionStore
	classes []RetentionClass
}

// NewRetentionSweeper binds the policy to its store. A partially wired sweeper
// is refused instead of silently doing nothing.
func NewRetentionSweeper(policy RetentionPolicy, store RetentionStore) (*RetentionSweeper, error) {
	if store == nil {
		return nil, fault.New(fault.InvalidInput, "retention sweeper requires a store")
	}
	return &RetentionSweeper{
		policy:  policy,
		store:   store,
		classes: RetentionClasses(),
	}, nil
}

// Sweep applies the policy at one explicit time across scopes. Any uncertainty
// is reported as an error while the affected rows are left in place; the caller
// retries on the next interval.
func (sweeper *RetentionSweeper) Sweep(ctx context.Context, scopes []RetentionScope, now time.Time) (RetentionSweepResult, error) {
	result := RetentionSweepResult{
		DeletedByClass: make(map[RetentionClass]int),
		KeptByClass:    make(map[RetentionClass]int),
	}
	if ctx == nil {
		return result, fault.New(fault.InvalidInput, "retention sweep requires a context")
	}
	if now.IsZero() {
		return result, fault.New(fault.InvalidInput, "retention sweep requires a non-zero current time")
	}
	// Validate every scope before the first statement so one malformed scope
	// cannot leave a partially swept installation.
	unique := make([]RetentionScope, 0, len(scopes))
	seen := make(map[RetentionScope]bool, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return result, err
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		unique = append(unique, scope)
	}
	result.Scopes = len(unique)

	var failures []error
	for _, scope := range unique {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		for _, class := range sweeper.classes {
			window, err := sweeper.policy.Window(class)
			if err != nil {
				// An unknown family is a programming error: refuse the whole
				// sweep rather than guess which family was meant.
				return result, err
			}
			if window.Disabled || window.UntilProjectDeletion {
				// Raw content without an explicit opt-in and receipts kept until
				// project deletion have no deletable candidates by definition.
				continue
			}
			before := now.Add(-window.Duration)
			candidates, err := sweeper.store.Candidates(ctx, scope, class, before)
			if err != nil {
				failures = append(failures, fmt.Errorf("retention %s candidates in %s/%s: %w",
					class, scope.TenantID, scope.ProjectID, err))
				continue
			}
			result.Candidates += len(candidates)
			for _, candidate := range candidates {
				if candidate.ID == "" {
					failures = append(failures, fmt.Errorf("retention %s candidate in %s/%s has no identity",
						class, scope.TenantID, scope.ProjectID))
					continue
				}
				if candidate.CreatedAt.IsZero() {
					// The store could not establish the age. Keeping an item is
					// always safer than deleting one that might still be inside
					// its window.
					result.KeptByClass[class]++
					continue
				}
				decision, err := sweeper.policy.MayDelete(class, candidate.CreatedAt, now, candidate.ActiveReferences)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				if !decision.Delete {
					result.KeptByClass[class]++
					continue
				}
				deleted, err := sweeper.store.Delete(ctx, scope, class, candidate, before)
				if err != nil {
					failures = append(failures, fmt.Errorf("retention %s delete in %s/%s: %w",
						class, scope.TenantID, scope.ProjectID, err))
					continue
				}
				if !deleted {
					// A concurrent change (an added reference, a moved
					// timestamp or a row already removed) means the store could
					// not prove this deletion; it is kept, not guessed.
					result.KeptByClass[class]++
					continue
				}
				result.DeletedByClass[class]++
			}
		}
	}
	if len(failures) > 0 {
		return result, errors.Join(failures...)
	}
	return result, nil
}
