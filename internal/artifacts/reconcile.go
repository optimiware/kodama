package artifacts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ObjectDeleter is the part of the object store the reconciler needs.
type ObjectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// ReconcilerConfig wires the bounded orphan and retention sweeps.
type ReconcilerConfig struct {
	Store   CleanupStore
	Objects ObjectDeleter
	// Retention is optional. Without it, usable content is never deleted.
	Retention Retention
	Policy    CleanupPolicy
	IDs       IDs
	Clock     func() time.Time
}

// Reconciler recovers unfinished object deletions, abandons expired
// reservations and applies an injected retention policy, all under durable
// leases and fencing: a crashed worker's claims expire and are retried, and
// repeating a sweep converges.
type Reconciler struct {
	cfg ReconcilerConfig
	now func() time.Time

	mu      sync.Mutex
	cursors map[domain.TenantID]domain.ArtifactID
}

// NewReconciler validates cfg. The module defines no production values: the
// caller injects every duration (O-11 belongs to operations).
func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	switch {
	case cfg.Store == nil || cfg.Objects == nil:
		return nil, errors.New("artifacts: a cleanup store and an object deleter are required")
	case cfg.Policy.StagingGrace <= 0 || cfg.Policy.DeletionLease <= 0 || cfg.Policy.Batch <= 0 || cfg.Policy.AbandonAfter < 0:
		return nil, errors.New("artifacts: cleanup policy needs positive StagingGrace, DeletionLease and Batch (AbandonAfter may be zero to disable abandonment)")
	}
	if cfg.IDs == nil {
		cfg.IDs = RandomIDs{}
	}
	r := &Reconciler{cfg: cfg, now: cfg.Clock, cursors: map[domain.TenantID]domain.ArtifactID{}}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	return r, nil
}

// SweepReport counts what one sweep did.
type SweepReport struct {
	Tenants   int
	Abandoned int
	Deleted   int
	Removed   int
	// Retryable counts deletions left pending for a later sweep.
	Retryable int
}

func (r *SweepReport) add(o SweepReport) {
	r.Tenants += o.Tenants
	r.Abandoned += o.Abandoned
	r.Deleted += o.Deleted
	r.Removed += o.Removed
	r.Retryable += o.Retryable
}

// Sweep runs one bounded pass over every tenant. A failure in one tenant does
// not stop the others; the first error is returned with the report.
func (r *Reconciler) Sweep(ctx context.Context) (SweepReport, error) {
	tenants, err := r.cfg.Store.Tenants(ctx)
	if err != nil {
		return SweepReport{}, unavailable("list tenants", err)
	}
	var total SweepReport
	var first error
	for _, t := range tenants {
		rep, err := r.SweepTenant(ctx, t)
		total.add(rep)
		if err != nil && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return total, first
}

// SweepTenant runs one bounded pass for one tenant.
func (r *Reconciler) SweepTenant(ctx context.Context, tenant domain.TenantID) (SweepReport, error) {
	rep := SweepReport{Tenants: 1}
	now := r.now().UTC()
	p := r.cfg.Policy

	if p.AbandonAfter > 0 {
		n, err := r.cfg.Store.AbandonExpired(ctx, tenant, now, p)
		rep.Abandoned = n
		if err != nil {
			return rep, unavailable("abandon expired reservations", err)
		}
	}
	if r.cfg.Retention != nil {
		n, err := r.retain(ctx, tenant, now)
		rep.Removed = n
		if err != nil {
			return rep, err
		}
	}

	claims, err := r.cfg.Store.ClaimDeletions(ctx, tenant, now, p, r.cfg.IDs.NewToken())
	if err != nil {
		return rep, unavailable("claim deletions", err)
	}
	var first error
	for _, c := range claims {
		// Deletion intent was persisted by ClaimDeletions before this call.
		dctx, cancel := context.WithTimeout(ctx, p.DeletionLease/2+time.Millisecond)
		derr := r.cfg.Objects.Delete(dctx, c.Key)
		cancel()
		if derr != nil {
			// A timeout or provider failure leaves the work pending; the lease
			// expires and a later sweep retries. Missing objects were already
			// reported as success by the adapter.
			rep.Retryable++
			if first == nil {
				first = unavailable("delete object", derr)
			}
			continue
		}
		if err := r.cfg.Store.AckDeletion(ctx, c, r.now().UTC(), p); err != nil {
			rep.Retryable++ // the deletion repeats idempotently
			if errors.Is(err, ErrClaimLost) {
				continue // another worker or a renewed transfer owns the record now
			}
			if first == nil {
				first = unavailable("acknowledge deletion", err)
			}
			continue
		}
		rep.Deleted++
	}
	return rep, first
}

// retain applies the injected policy to one batch after the tenant's cursor.
// Eligibility is advisory: the store re-checks pins under the artifact's row
// lock, so a concurrently pinned artifact survives.
func (r *Reconciler) retain(ctx context.Context, tenant domain.TenantID, now time.Time) (int, error) {
	r.mu.Lock()
	after := r.cursors[tenant]
	r.mu.Unlock()
	subjects, err := r.cfg.Store.RetentionCandidates(ctx, tenant, after, r.cfg.Policy.Batch)
	if err != nil {
		return 0, unavailable("list retention candidates", err)
	}
	removed := 0
	for _, sub := range subjects {
		ok, err := r.cfg.Retention.Eligible(ctx, sub, now)
		if err != nil {
			return removed, unavailable("retention policy", err)
		}
		if !ok {
			continue
		}
		err = r.cfg.Store.BeginRetentionDeletion(ctx, sub.Scope, sub.ID, now, Actor{Type: "system", ID: "artifact-reconciler"})
		switch {
		case err == nil:
			removed++
		case errors.Is(err, ErrPinned), errors.Is(err, ErrNotFound):
			// Protected or already gone: the policy decision no longer applies.
		default:
			return removed, unavailable("begin retention deletion", err)
		}
	}
	r.mu.Lock()
	if len(subjects) < r.cfg.Policy.Batch {
		delete(r.cursors, tenant) // wrap around next time
	} else {
		r.cursors[tenant] = subjects[len(subjects)-1].ID
	}
	r.mu.Unlock()
	return removed, nil
}

// Run sweeps every interval until ctx ends. Errors are reported to onError
// (which may be nil) and never stop the loop.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration, onError func(error)) error {
	if interval <= 0 {
		return fmt.Errorf("artifacts: sweep interval must be positive")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.Sweep(ctx); err != nil && onError != nil && ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
