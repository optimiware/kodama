package execution

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
)

// ReconcileReport counts what one reconciliation pass repaired.
type ReconcileReport struct {
	Runs       int
	Retained   int
	Observed   int
	Terminated int
	Redriven   int
	Reconciled int
	Settled    int
	Removed    int
	Errors     int
}

func (r *ReconcileReport) add(o ReconcileReport) {
	r.Runs += o.Runs
	r.Retained += o.Retained
	r.Observed += o.Observed
	r.Terminated += o.Terminated
	r.Redriven += o.Redriven
	r.Reconciled += o.Reconciled
	r.Settled += o.Settled
	r.Removed += o.Removed
	r.Errors += o.Errors
}

// Reconcile repairs every tenant: it rescans cancelled work items, confirms
// pending source retention, enforces deadlines, observes running workloads,
// requests and confirms workload termination, redrives unconfirmed dispatch
// intents with their original identity, asks the trusted effect adapter about
// uncertain outcomes, settles what can be settled and removes settled
// workloads. It tolerates missing, duplicate and reordered
// wakeups because it reads durable desired state only, and it is safe to run at
// any time and from several replicas.
func (s *Service) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var total ReconcileReport
	if s.d.Tenants == nil {
		return total, ErrNotConfigured
	}
	tenants, err := s.d.Tenants.TenantIDs(ctx)
	if err != nil {
		return total, Unavailablef(err, "tenant enumeration failed")
	}
	var first error
	for _, t := range tenants {
		r, err := s.ReconcileTenant(ctx, t)
		total.add(r)
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

// ReconcileTenant is Reconcile for one tenant.
func (s *Service) ReconcileTenant(ctx context.Context, tenant domain.TenantID) (ReconcileReport, error) {
	var rep ReconcileReport
	var first error
	if s.d.CancelledWork != nil {
		if _, err := s.ReconcileCancellations(ctx, tenant); err != nil {
			s.log(ctx, "reconcile cancellations", err)
			rep.Errors++
			first = err
		}
	}
	var after RunRef
	for {
		refs, err := s.d.Store.Active(ctx, tenant, after, s.o.Batch)
		if err != nil {
			return rep, err
		}
		for _, ref := range refs {
			r, err := s.ReconcileRun(ctx, ref)
			rep.add(r)
			if err != nil {
				s.log(ctx, "reconcile run", err, "run", string(ref.ID))
				rep.Errors++
				if first == nil {
					first = err
				}
			}
		}
		if len(refs) < s.o.Batch {
			break
		}
		after = refs[len(refs)-1]
	}
	if s.d.Runtime != nil {
		n, err := s.RemoveSettled(ctx, tenant)
		rep.Removed += n
		if err != nil {
			rep.Errors++
			if first == nil {
				first = err
			}
		}
	}
	return rep, first
}

// ReconcileRun repairs one run. Each repair is its own short transaction, so one
// blocked attempt (an unsettled publication, an unknown effect) never stalls
// the others.
func (s *Service) ReconcileRun(ctx context.Context, ref RunRef) (ReconcileReport, error) {
	rep := ReconcileReport{Runs: 1}
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return rep, err
	}
	if view.Run.Status.Terminal() {
		return rep, nil
	}
	if view.Run.SourceRetention == RetentionPending {
		if _, err := s.ConfirmSource(ctx, ref.Scope, ref.ID); err != nil {
			note(err)
		} else {
			rep.Retained++
		}
	}
	// Deadlines, skip propagation and completion.
	agg, _, err := s.d.Store.Update(ctx, ref.Scope, ref.ID, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		agg.CheckDeadlines(env.Now())
		after, err := advance(env, agg)
		return Effects{After: after}, err
	})
	if err != nil {
		note(err)
		return rep, first
	}
	s.terminateRevoked(ctx, agg)
	view = agg

	if rt, err := s.runtimeFor(view.Run); err == nil {
		for _, at := range view.Attempts {
			c := view.Control(at.ID)
			switch {
			case c.Settled():
			case at.Status == AttemptRunning && c.DispatchConfirmedAt == nil:
				if s.redrive(ctx, view, ref, at) {
					rep.Redriven++
				}
			case at.Status == AttemptRunning:
				if err := s.observeWorkload(ctx, rt, at, c); err != nil {
					note(err)
				} else {
					rep.Observed++
				}
			case c.TerminationRequestedAt != nil && c.TerminationConfirmedAt == nil:
				s.terminate(ctx, rt, at)
				rep.Terminated++
			}
		}
	} else if s.d.Runtime != nil && len(view.Unsettled()) > 0 {
		note(err)
	}
	// Uncertain outcomes and settlement, one attempt per transaction.
	view, err = s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		note(err)
		return rep, first
	}
	for _, at := range view.Attempts {
		c := view.Control(at.ID)
		if c.Settled() || !at.Status.Terminal() {
			continue
		}
		if at.Status != AttemptSucceeded && c.TerminationConfirmedAt == nil {
			continue
		}
		if c.Launch == LaunchUnknown && c.ReconciledAt == nil && at.Status != AttemptSucceeded && s.d.EffectReconciler != nil {
			if err := s.reconcileEffect(ctx, ref, at); err != nil {
				note(err)
			} else {
				rep.Reconciled++
			}
		}
		if err := s.settle(ctx, ref, at.ID); err != nil {
			if !errors.Is(err, ErrOutcomeUnknown) {
				note(err)
			}
		} else {
			rep.Settled++
		}
	}
	return rep, first
}

// redrive starts an unconfirmed attempt again under the same rule as a first
// start: the run's runtime must still be the one configured and the current
// policy's requirements for the step must still be met. Otherwise nothing is
// started and the attempt stays unconfirmed for a later pass; cancellation and
// deadlines still end it.
func (s *Service) redrive(ctx context.Context, view *Aggregate, ref RunRef, at *Attempt) bool {
	step := view.Step(at.StepID)
	if step == nil {
		return false
	}
	rt, err := s.launchable(view.Run, step)
	if err != nil {
		s.log(ctx, "redrive", err, "attempt", string(at.ID), "step", at.StepID)
		return false
	}
	var spec *AttemptSpec
	_, _, err = s.d.Store.Update(ctx, ref.Scope, ref.ID, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		cur := agg.Attempt(at.ID)
		if cur == nil || !agg.ClaimRedrive(cur, s.d.NewID("drv"), s.o.ClaimLease, env.Now()) {
			return Effects{}, nil
		}
		redriven := cur.spec(agg.Run.Purpose, true)
		spec = &redriven
		return Effects{}, nil
	})
	if err != nil || spec == nil {
		if err != nil {
			s.log(ctx, "redrive claim", err, "attempt", string(at.ID))
		}
		return false
	}
	if err := s.drive(ctx, rt, *spec); err != nil {
		s.log(ctx, "redrive", err, "attempt", string(at.ID))
		return false
	}
	return true
}

func (s *Service) reconcileEffect(ctx context.Context, ref RunRef, at *Attempt) error {
	ev, err := s.d.EffectReconciler.Reconcile(ctx, EffectQuery{Scope: ref.Scope, RunID: ref.ID, StepID: at.StepID, AttemptID: at.ID, ActionKey: at.ActionKey})
	if err != nil {
		return Unavailablef(err, "effect reconciliation is unavailable")
	}
	_, _, err = s.d.Store.Update(ctx, ref.Scope, ref.ID, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		cur := agg.Attempt(at.ID)
		if cur == nil {
			return Effects{}, ErrNotFound
		}
		return Effects{}, agg.Reconcile(cur, ev, env.Now())
	})
	return err
}

// ReconcileAttempt applies trusted reconciliation evidence to one uncertain
// attempt and settles it when the evidence is affirmative. It is the entry for
// an operator-driven adapter; without evidence the attempt stays blocked.
func (s *Service) ReconcileAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ev ReconciliationEvidence) error {
	run, err := s.d.Store.RunOf(ctx, scope, attempt)
	if err != nil {
		return err
	}
	_, _, err = s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		at := agg.Attempt(attempt)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		if err := agg.Reconcile(at, ev, env.Now()); err != nil {
			return Effects{}, err
		}
		after, err := advance(env, agg, at)
		if errors.Is(err, ErrOutcomeUnknown) {
			err = nil
		}
		return Effects{After: after}, err
	})
	return err
}

// settle accounts for one attempt in its own transaction.
func (s *Service) settle(ctx context.Context, ref RunRef, attempt domain.AttemptID) error {
	agg, _, err := s.d.Store.Update(ctx, ref.Scope, ref.ID, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		at := agg.Attempt(attempt)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		after, err := advance(env, agg, at)
		return Effects{After: after}, err
	})
	if err != nil {
		return err
	}
	if agg.Control(attempt).Effect == EffectUnknown && !agg.Control(attempt).Settled() {
		return ErrOutcomeUnknown
	}
	return nil
}
