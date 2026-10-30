package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ReconcileReport counts what one pass examined and could not finish.
type ReconcileReport struct {
	Examined int
	Failed   int
}

// Reconcile is the periodic, NATS-independent repair pass. For every tenant it
// handles, in bounded batches, the workspaces with unfinished durable work:
// provisioning that stopped, publications whose outcome is unknown, archive
// requests that were not settled, retained references with unconfirmed
// creation or deletion. Failures are counted and retried by the next pass.
func (s *Service) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	tenants, err := s.d.Tenants.TenantIDs(ctx)
	if err != nil {
		return rep, unavailable("tenant enumeration", err)
	}
	for _, t := range tenants {
		var after ID
		for {
			cands, err := s.d.Store.Reconcilable(ctx, t, after, s.o.ReconcileBatch)
			if err != nil {
				return rep, err
			}
			for _, c := range cands {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				rep.Examined++
				if err := s.ReconcileWorkspace(ctx, c.Scope, c.ID); err != nil {
					rep.Failed++
					s.d.Logger.Warn("workspace reconcile incomplete", "workspace", c.ID, "error", err.Error())
				}
			}
			if len(cands) < s.o.ReconcileBatch {
				break
			}
			after = cands[len(cands)-1].ID
		}
	}
	return rep, nil
}

// RunReconciler repeats Reconcile every interval until ctx ends.
func (s *Service) RunReconciler(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return ErrInvalidConfiguration
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.d.Logger.Warn("workspace reconcile pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ReconcileWorkspace finishes whatever durable work one workspace has.
func (s *Service) ReconcileWorkspace(ctx context.Context, scope domain.Scope, id ID) error {
	agg, err := s.d.Store.View(ctx, scope, id, Load{Pins: true})
	if err != nil {
		return err
	}
	w := agg.Workspace
	if w.Internal() {
		return s.reconcileInternal(ctx, scope, id)
	}
	var errs []error
	if w.State == StateProvisioning {
		if w, err = s.completeProvisioning(ctx, system, w); err != nil {
			errs = append(errs, err)
		}
		if w == nil {
			return errors.Join(errs...)
		}
	}
	if w.Pending != nil {
		if err := s.advance(ctx, scope, id, nil); err != nil {
			errs = append(errs, err)
		}
	}
	cur, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if cur.State == StateAvailable && cur.ArchiveRequestedAt != nil && cur.Pending == nil {
		if _, err := s.finishArchive(ctx, system, scope, id); err != nil && !errors.Is(err, ErrArchiveUncertain) {
			errs = append(errs, err)
		}
	}
	for _, pin := range agg.Pins {
		switch pin.Status {
		case PinIntent:
			if _, err := s.confirmPin(ctx, system, cur, pin); err != nil {
				errs = append(errs, err)
			}
		case PinRemoving:
			if err := s.finishRemoval(ctx, system, cur, pin); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
