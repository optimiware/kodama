package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ReconcileReport counts what one pass did.
type ReconcileReport struct {
	// Unsent requests were abandoned before any send and released their slot.
	Unsent int
	// Unknown requests were abandoned after a send may have started: they were
	// charged their bound and keep the run's slot.
	Unknown int
	// Skipped requests were settled by their worker first.
	Skipped int
	// Failed reconciliations (retried by the next pass).
	Failed int
}

// minQuiet is how many Recheck intervals a request must be silent before it is
// reconciled: several missed heartbeats, never a single late one.
const minQuiet = 3

// Reconcile abandons requests whose worker stopped heartbeating for at least
// quiet. It is idempotent and safe to run on every replica concurrently: the
// store settles each request once, and a request settled meanwhile by its
// worker is skipped.
//
// A request that never started forwarding is charged nothing and frees its
// slot. A forwarding request is charged its bound and becomes unknown; it keeps
// holding the run's slot, because a lost worker is not evidence that the
// provider stopped. Only SettleUnknown, which needs trusted evidence and
// management authority, releases that slot.
func (s *Service) Reconcile(ctx context.Context, quiet time.Duration) (ReconcileReport, error) {
	var rep ReconcileReport
	if s.cfg.Tenants == nil {
		return rep, errors.New("gateway: a tenant source is required to reconcile")
	}
	if quiet < minQuiet*s.cfg.Timeouts.Recheck {
		return rep, fail(ErrInvalid, "the quiet period must cover several heartbeat intervals")
	}
	tenants, err := s.cfg.Tenants.TenantIDs(ctx)
	if err != nil {
		return rep, FailWith(ErrUnavailable, err, "tenants could not be listed")
	}
	var first error
	for _, tenant := range tenants {
		if err := s.reconcileTenant(ctx, tenant, quiet, &rep); err != nil && first == nil {
			first = err
		}
	}
	return rep, first
}

func (s *Service) reconcileTenant(ctx context.Context, tenant domain.TenantID, quiet time.Duration, rep *ReconcileReport) error {
	before := s.now().Add(-quiet)
	list, err := s.cfg.Store.Abandoned(ctx, tenant, before, 100)
	if err != nil {
		return Classify(err)
	}
	for _, m := range list {
		u, err := s.cfg.Store.Abandon(ctx, m.Scope, m.ID, before, s.now())
		switch {
		case err == nil:
			if u.Result == StateUnknown {
				rep.Unknown++
			} else {
				rep.Unsent++
			}
		case errors.Is(err, ErrSuperseded):
			rep.Skipped++
		default:
			rep.Failed++
			s.cfg.Logger.Warn("reconciliation failed", "requestId", string(m.ID))
		}
	}
	return nil
}

// SettleUnknown records an authorized operator's attestation that an unknown
// request's provider call ended, and releases the run's slot. The gateway does
// not establish remote completion itself. The charge stays. The actor needs
// project:settings, checked here and again inside the settlement transaction:
// lease expiry or a missing worker never does this.
func (s *Service) SettleUnknown(ctx context.Context, actor Actor, scope domain.Scope, id RequestID) error {
	if err := s.authorize(ctx, actor, scope, PermSettings); err != nil {
		return err
	}
	if err := id.Validate(); err != nil {
		return err
	}
	return Classify(s.cfg.Store.Settle(ctx, SettleCommand{Actor: actor, Scope: scope, RequestID: id, Now: s.now()}))
}
