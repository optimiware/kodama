package execution

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
)

type observeOutcome struct {
	late     bool
	mismatch bool
}

// ObserveAttempt applies one trusted runtime observation under the run lock.
// Observations of another workload, instance or spec are refused and kept as
// evidence; a late observation of a terminal attempt rewrites nothing.
// Observing the workload stopped settles the attempt (releasing its capacity
// and writer slot once) and may complete the run. Attempts that lost authority
// are stopped through the runtime after the commit.
func (s *Service) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, obs Observation) error {
	if err := scope.Validate(); err != nil {
		return ErrNotFound
	}
	if err := attempt.Validate(); err != nil {
		return ErrNotFound
	}
	run, err := s.d.Store.RunOf(ctx, scope, attempt)
	if err != nil {
		return err
	}
	agg, res, err := s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		at := agg.Attempt(attempt)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		now := env.Now()
		r, err := agg.Observe(at, obs, now)
		if errors.Is(err, ErrConflict) {
			// The mismatch evidence must commit although the call fails.
			return Effects{Result: observeOutcome{mismatch: true}}, nil
		}
		if err != nil {
			return Effects{}, err
		}
		var targets []*Attempt
		if obs.Kind == ObservedTerminated {
			targets = append(targets, at)
		}
		after, err := advance(env, agg, targets...)
		return Effects{After: after, Result: observeOutcome{late: r.Late}}, err
	})
	if err != nil {
		return err
	}
	s.terminateRevoked(ctx, agg)
	if out, ok := res.(observeOutcome); ok && out.mismatch {
		return ErrConflict
	}
	return nil
}

// terminateRevoked stops the workloads of attempts that lost authority in the
// committed mutation. It runs outside any transaction; a failure leaves the
// durable termination request for reconciliation, and a confirmed stop is
// recorded through ObserveAttempt.
func (s *Service) terminateRevoked(ctx context.Context, agg *Aggregate) {
	if agg == nil || len(agg.Revoked) == 0 || s.d.Runtime == nil {
		return
	}
	rt, err := s.runtimeFor(agg.Run)
	if err != nil {
		s.log(ctx, "terminate", err, "run", string(agg.Run.ID))
		return
	}
	seen := map[domain.AttemptID]bool{}
	for _, at := range agg.Revoked {
		if seen[at.ID] {
			continue
		}
		seen[at.ID] = true
		s.terminate(ctx, rt, at)
	}
}

func (s *Service) terminate(ctx context.Context, rt Runtime, at *Attempt) {
	ev, err := rt.Stop(ctx, at.Workload())
	if err != nil {
		s.log(ctx, "terminate", err, "attempt", string(at.ID))
		return
	}
	if !ev.Stopped {
		return
	}
	if err := s.ObserveAttempt(ctx, at.Scope, at.ID, Observation{Kind: ObservedTerminated, WorkloadName: at.WorkloadName, Launch: ev.Launch, ObservedAt: s.now()}); err != nil {
		s.log(ctx, "terminated", err, "attempt", string(at.ID))
	}
}
