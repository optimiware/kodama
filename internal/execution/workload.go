package execution

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// requirements applies the installation's admission policy to one step.
func (s *Service) requirements(step pipeline.Step) Guarantees {
	if s.d.Requirements != nil {
		return s.d.Requirements(step)
	}
	return DefaultRequirements(step)
}

// bind admits a definition on the configured runtime and returns the binding
// to pin with the run. A runtime lacking a required guarantee refuses the run
// before any state exists. Without a runtime the run is admitted unbound; an
// unbound run is never claimed, started, observed, stopped or removed, because
// no backend was ever promised to run it.
func (s *Service) bind(doc pipeline.Document) (*RuntimeBinding, error) {
	if s.d.Runtime == nil {
		return nil, nil
	}
	var required Guarantees
	for _, step := range doc.Spec.Steps {
		required |= s.requirements(step)
	}
	caps := s.d.Runtime.Capabilities()
	if missing := caps.Guarantees.Missing(required); missing != 0 {
		return nil, Refusedf("the %s runtime does not provide %s", caps.Backend, missing)
	}
	return &RuntimeBinding{Backend: caps.Backend, Requires: required}, nil
}

// runtimeFor returns the runtime that may act on run's workloads: the
// configured backend must be the one the run was admitted for and still meet
// what the run requires. A run admitted without a runtime has no backend to
// act on its workloads. Otherwise nothing is started, stopped or removed:
// another backend would report a workload it never ran as absent.
func (s *Service) runtimeFor(run *Run) (Runtime, error) {
	if s.d.Runtime == nil {
		return nil, fmt.Errorf("%w: no runtime is configured", ErrNotConfigured)
	}
	b := run.Snapshot.Runtime
	if b == nil {
		return nil, fmt.Errorf("%w: the run was admitted without a runtime", ErrNotConfigured)
	}
	caps := s.d.Runtime.Capabilities()
	if caps.Backend != b.Backend {
		return nil, fmt.Errorf("%w: the run is bound to the %s runtime", ErrNotConfigured, b.Backend)
	}
	if missing := caps.Guarantees.Missing(b.Requires); missing != 0 {
		return nil, fmt.Errorf("%w: the %s runtime no longer provides %s", ErrNotConfigured, b.Backend, missing)
	}
	return s.d.Runtime, nil
}

// launchable returns the runtime that may start step now: the run's binding
// holds and the current policy's requirements for the step are met.
func (s *Service) launchable(run *Run, step *StepRun) (Runtime, error) {
	rt, err := s.runtimeFor(run)
	if err != nil {
		return nil, err
	}
	caps := rt.Capabilities()
	if missing := caps.Guarantees.Missing(s.requirements(step.Spec)); missing != 0 {
		return nil, fmt.Errorf("%w: the %s runtime does not provide %s for step %s", ErrNotConfigured, caps.Backend, missing, step.ID)
	}
	return rt, nil
}

// observeWorkload asks the runtime about a running, dispatch-confirmed
// attempt and applies the answer. An affirmatively absent workload is lost:
// the attempt fails as an infrastructure failure and keeps whatever launch
// evidence is already known, so an uncertain effect still blocks replay.
func (s *Service) observeWorkload(ctx context.Context, rt Runtime, at *Attempt, c *Control) error {
	obs, err := rt.Observe(ctx, at.Workload())
	switch {
	case errors.Is(err, ErrNotFound):
		obs = Observation{Kind: ObservedFailed, Class: ClassInfrastructure, Reason: ReasonWorkloadLost, Launch: c.Launch}
		if at.StartedAt != nil {
			obs.Launch = LaunchStarted
		}
	case err != nil:
		return Unavailablef(err, "the runtime could not observe the workload")
	case obs.Kind == ObservedTerminated:
		return Unavailablef(nil, "the runtime reported a running workload terminated")
	case !news(at, obs):
		return nil
	}
	obs.WorkloadName = at.WorkloadName
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = s.now()
	}
	return s.ObserveAttempt(ctx, at.Scope, at.ID, obs)
}

// news reports whether an observation of a running, dispatch-confirmed
// attempt can change anything: a repeated answer is not applied, so polling
// writes nothing while a workload just runs.
func news(at *Attempt, obs Observation) bool {
	differs := obs.WorkloadUID != "" && obs.WorkloadUID != at.WorkloadUID
	switch obs.Kind {
	case ObservedDispatched:
		return differs
	case ObservedStarted:
		return differs || at.StartedAt == nil
	}
	return true
}

// RemoveSettled releases the workloads of the tenant's settled attempts and
// records each confirmed removal. Removal is bookkept apart from settlement,
// so a failed or retained removal is retried on a later pass, for terminal
// runs too. It reports how many workloads were removed.
func (s *Service) RemoveSettled(ctx context.Context, tenant domain.TenantID) (int, error) {
	if s.d.Runtime == nil {
		return 0, ErrNotConfigured
	}
	caps := s.d.Runtime.Capabilities()
	removed := 0
	var first error
	var after RemovalCursor
	for {
		page, err := s.d.Store.Removable(ctx, tenant, caps.Backend, after, s.o.Batch)
		if err != nil {
			return removed, err
		}
		for _, r := range page {
			err := s.d.Runtime.Remove(ctx, r.Workload)
			if err == nil {
				err = s.d.Store.RecordRemoval(ctx, r.Workload.Scope, r.Workload.AttemptID)
			}
			switch {
			case err == nil:
				removed++
			case errors.Is(err, ErrBusy):
			default:
				s.log(ctx, "remove workload", err, "attempt", string(r.Workload.AttemptID))
				if first == nil {
					first = err
				}
			}
		}
		if len(page) < s.o.Batch {
			return removed, first
		}
		after = page[len(page)-1].Cursor()
	}
}
