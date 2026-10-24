package gates

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
)

// Report counts what one reconciliation pass did. It is for operators and
// tests; nothing else reads it.
type Report struct {
	Requested   int
	Completed   int
	Rejected    int
	Invalidated int
	// Waiting counts gates that remain pending or whose approval cannot be
	// consumed yet.
	Waiting int
	// Blocked counts gate steps that could not get a gate because their
	// evidence is unverifiable.
	Blocked int
	// Failed counts gates whose processing met an outage or an unexpected
	// error; they are retried by the next pass.
	Failed int
}

// ReconcileAll reconciles every tenant the injected enumeration lists and
// returns the first failure after visiting all of them.
func (s *Service) ReconcileAll(ctx context.Context) (Report, error) {
	var total Report
	if s.d.Tenants == nil {
		return total, ErrNotConfigured
	}
	tenants, err := s.d.Tenants.TenantIDs(ctx)
	if err != nil {
		return total, Unavailablef(err, "tenant enumeration failed")
	}
	var first error
	for _, t := range tenants {
		r, err := s.Reconcile(ctx, t)
		total.Requested += r.Requested
		total.Completed += r.Completed
		total.Rejected += r.Rejected
		total.Invalidated += r.Invalidated
		total.Waiting += r.Waiting
		total.Blocked += r.Blocked
		total.Failed += r.Failed
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

// Reconcile scans the durable records of one tenant in bounded pages and
// converges them: ready gate steps get their gate, decided gates progress or
// fail their step, and definitively stale gates are invalidated once. It never
// trusts an event: a missing, duplicated or reordered notification only delays
// it. Every action is idempotent under the run/step identity, so a repeat
// observes the same decision and allocates no attempt.
func (s *Service) Reconcile(ctx context.Context, tenant domain.TenantID) (Report, error) {
	var rep Report
	if err := tenant.Validate(); err != nil {
		return rep, ErrNotFound
	}
	var after StepRef
	for {
		refs, err := s.d.Store.ReadyGateSteps(ctx, tenant, after, s.o.Batch)
		if err != nil {
			return rep, err
		}
		for _, ref := range refs {
			_, created, err := s.RequestForStep(ctx, ref.Scope, ref.RunID, ref.StepID)
			switch {
			case err == nil:
				if created {
					rep.Requested++
				}
			case errors.Is(err, ErrInvalidTransition):
				// Not ready yet (a dependency is unfinished) or no longer waiting.
			case errors.Is(err, ErrEvidence), errors.Is(err, ErrInvalid):
				rep.Blocked++
				s.log(ctx, "request", err, "run", string(ref.RunID), "step", ref.StepID)
			default:
				rep.Failed++
				s.log(ctx, "request", err, "run", string(ref.RunID), "step", ref.StepID)
			}
		}
		if len(refs) < s.o.Batch {
			break
		}
		after = refs[len(refs)-1]
	}
	var gafter GateRef
	for {
		refs, err := s.d.Store.OpenGates(ctx, tenant, gafter, s.o.Batch)
		if err != nil {
			return rep, err
		}
		for _, ref := range refs {
			s.processGate(ctx, ref, &rep)
		}
		if len(refs) < s.o.Batch {
			break
		}
		gafter = refs[len(refs)-1]
	}
	return rep, nil
}

// ProcessGate converges one gate and reports whether its step progressed. It
// is the same action reconciliation takes, exposed for event-driven wakeups.
func (s *Service) ProcessGate(ctx context.Context, scope domain.Scope, id ID) (Report, error) {
	var rep Report
	s.processGate(ctx, GateRef{Scope: scope, ID: id}, &rep)
	if rep.Failed > 0 {
		return rep, Unavailablef(nil, "the gate could not be processed now")
	}
	return rep, nil
}

func (s *Service) processGate(ctx context.Context, ref GateRef, rep *Report) {
	g, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		rep.Failed++
		s.log(ctx, "load", err, "gate", string(ref.ID))
		return
	}
	switch g.Status {
	case StatusRejected:
		out, err := s.d.Store.RejectStep(ctx, ref.Scope, ref.ID)
		if err != nil {
			rep.Failed++
			s.log(ctx, "reject", err, "gate", string(ref.ID))
			return
		}
		if out.Rejected {
			rep.Rejected++
		}
		return
	case StatusApproved:
		out, err := s.d.Store.CompleteStep(ctx, ref.Scope, ref.ID)
		switch {
		case err == nil:
			if out.Completed {
				rep.Completed++
			}
			// A consumed approval can still become stale: later steps need it
			// to stay applicable, so record the staleness once.
			if rerr := s.d.Store.Reevaluate(ctx, ref.Scope, ref.ID); isDefinitiveStale(rerr) {
				s.invalidate(ctx, ref, rerr, rep)
			}
			return
		case isDefinitiveStale(err):
			s.invalidate(ctx, ref, err, rep)
			return
		case errors.Is(err, ErrStaleSubject), errors.Is(err, ErrConformance), errors.Is(err, ErrReviewerAuthority):
			// Not applicable right now (expired, authority revoked): the step
			// keeps waiting. No invalidation is fabricated for an authority
			// check or a derived expiry.
			rep.Waiting++
			return
		}
		rep.Failed++
		s.log(ctx, "complete", err, "gate", string(ref.ID))
		return
	}
	if g.Invalidation != nil {
		return
	}
	// Pending: record a definitive staleness once; otherwise keep waiting.
	err = s.d.Store.Reevaluate(ctx, ref.Scope, ref.ID)
	switch {
	case err == nil:
		rep.Waiting++
	case isDefinitiveStale(err):
		s.invalidate(ctx, ref, err, rep)
	case errors.Is(err, ErrStaleSubject):
		rep.Waiting++
	default:
		rep.Failed++
		s.log(ctx, "reevaluate", err, "gate", string(ref.ID))
	}
}

func isDefinitiveStale(err error) bool {
	var se *StaleError
	return errors.As(err, &se) && se.Definitive
}

func (s *Service) invalidate(ctx context.Context, ref GateRef, err error, rep *Report) {
	var se *StaleError
	if !errors.As(err, &se) {
		return
	}
	_, created, ierr := s.d.Store.Invalidate(ctx, ref.Scope, ref.ID, se, "")
	if ierr != nil {
		rep.Failed++
		s.log(ctx, "invalidate", ierr, "gate", string(ref.ID))
		return
	}
	if created {
		rep.Invalidated++
	}
}
