package execution

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// advance completes a mutation: skip propagation, settlement of the listed
// attempts, run completion and secret release. Settlement hands its peer effects
// (capacity release, writer settlement) to the store as After hooks, which run
// in the documented lock order. A blocked (unknown-effect) attempt is not an
// error: its bookkeeping is saved and it stays blocked.
func advance(env Env, agg *Aggregate, settle ...*Attempt) ([]func(context.Context, Env) error, error) {
	now := env.Now()
	var after []func(context.Context, Env) error
	for _, at := range settle {
		out, err := agg.Settle(at, now)
		if err != nil && !errors.Is(err, ErrOutcomeUnknown) {
			return nil, err
		}
		if out != SettledNow {
			continue
		}
		at := at
		after = append(after, func(ctx context.Context, env Env) error { return env.ReleaseCapacity(ctx, at) })
		if at.Writer() {
			outcome, source := WriterFailed, ""
			switch at.Status {
			case AttemptSucceeded:
				outcome = WriterSucceeded
				if at.Result != nil && at.Result.SourceCommit != nil {
					source = string(*at.Result.SourceCommit)
				}
			case AttemptCancelled:
				outcome = WriterCancelled
			}
			after = append(after, func(ctx context.Context, env Env) error { return env.SettleWriter(ctx, at, outcome, source) })
		}
	}
	agg.Propagate(now)
	if _, err := agg.Finish(now); err != nil {
		return nil, err
	}
	if agg.Run.Draining() || agg.Run.Status.Terminal() {
		run := agg.Run
		after = append(after, func(ctx context.Context, env Env) error { return env.ReleaseSecrets(ctx, run) })
	}
	if agg.Run.Shared != nil && agg.Run.Status.Terminal() {
		// The run-level reservation is released only once the run is terminal,
		// which requires every resource verified stopped and removed.
		run := agg.Run
		after = append(after, func(ctx context.Context, env Env) error {
			if len(agg.Resources) == 0 {
				return nil
			}
			se, err := sharedEnvOf(env)
			if err != nil {
				return err
			}
			return se.ReleaseRunCapacity(ctx, run)
		})
	}
	return after, nil
}

// claimOutcome is the result of one claim transaction.
type claimOutcome struct {
	Intent *AttemptSpec
}

// ClaimRunnable claims one runnable step of the tenant and persists its attempt
// as durable dispatch intent. It never calls the runtime: the returned spec
// authorizes it to create or adopt the workload after the commit. A run whose
// pinned runtime is not the configured one is not claimed. Runs are
// scanned oldest first in bounded pages; contention, exhausted capacity and
// stale writer evidence skip to the next candidate without consuming an
// attempt ordinal or leaking a reservation, because each claim is one
// transaction. ErrNothingRunnable means no run could dispatch.
func (s *Service) ClaimRunnable(ctx context.Context, tenant domain.TenantID) (*AttemptSpec, error) {
	if err := tenant.Validate(); err != nil {
		return nil, ErrNotFound
	}
	var after RunRef
	var firstErr error
	for {
		refs, err := s.d.Store.Dispatchable(ctx, tenant, after, s.o.Batch)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			intent, err := s.claimOne(ctx, ref)
			switch {
			case err == nil && intent != nil:
				return intent, nil
			case err == nil:
			case errors.Is(err, ErrBusy), errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrStale):
			case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotConfigured):
				// A dependency outage denies dispatch rather than skipping a check.
				return nil, err
			default:
				s.log(ctx, "claim", err, "run", string(ref.ID))
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(refs) < s.o.Batch {
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, ErrNothingRunnable
		}
		after = refs[len(refs)-1]
	}
}

func (s *Service) claimOne(ctx context.Context, ref RunRef) (*AttemptSpec, error) {
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return nil, err
	}
	if view.Run.Shared != nil {
		return s.claimShared(ctx, ref, view)
	}
	now := s.now()
	step := view.NextRunnable(now, s.d.Hooks)
	if step == nil {
		return nil, nil
	}
	if _, err := s.launchable(view.Run, step); err != nil {
		// Claiming would create an attempt this runtime must not start; the run
		// waits for the runtime it was admitted for.
		s.log(ctx, "claim", err, "run", string(ref.ID), "step", step.ID)
		return nil, nil
	}
	lim, err := s.d.Limits.Limits(ctx, ref.Scope)
	if err != nil {
		return nil, Unavailablef(err, "execution limits are unavailable")
	}
	if err := lim.Validate(); err != nil {
		return nil, Unavailablef(err, "execution limits are invalid")
	}
	var src SourceInfo
	if binding, err := view.SourceFor(step); err == nil && binding.Access != AccessNone {
		// Refresh writer-tip evidence outside the transaction; the claim
		// transaction rechecks it under the workspace lock.
		src, err = s.d.Sources.Describe(ctx, ref.Scope, view.Run.WorkspaceID, binding.Access == AccessWrite)
		if err != nil {
			return nil, mapLookup(err, "source")
		}
	}
	opts := UpdateOptions{Quota: true, LockWork: view.Run.Purpose == PurposeWork, SkipLocked: true}
	_, res, err := s.d.Store.Update(ctx, ref.Scope, ref.ID, opts, s.claimMutation(step.ID, src, lim))
	if err != nil {
		return nil, err
	}
	if out, ok := res.(claimOutcome); ok {
		return out.Intent, nil
	}
	return nil, nil
}

// claimMutation is the dispatch decision, rechecked under every lock: current
// authorization, work cancellation, run cancellation/failure intent, due time,
// dependencies, source readiness, capacity and writer eligibility.
func (s *Service) claimMutation(stepID string, src SourceInfo, lim Limit) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		now := env.Now()
		run := agg.Run
		if run.Purpose == PurposeWork {
			if _, err := env.Work(); err != nil {
				if errors.Is(err, ErrWorkCancelled) {
					agg.RequestCancel(ReasonWorkCancelled, now)
					after, aerr := advance(env, agg)
					return Effects{After: after, Result: claimOutcome{}}, aerr
				}
				return Effects{}, err
			}
			if err := env.Authorize(ctx, run.RequestedBy, run.Scope, PermRunManage); err != nil {
				return Effects{}, normalizeAuth(err)
			}
		} else if run.DeploymentID == "" {
			return Effects{}, ErrDenied
		}
		agg.Propagate(now)
		step := agg.NextRunnable(now, s.d.Hooks)
		if step == nil || step.ID != stepID {
			after, err := advance(env, agg)
			return Effects{After: after, Result: claimOutcome{}}, err
		}
		binding, err := agg.SourceFor(step)
		if err != nil {
			if errors.Is(err, ErrStale) {
				return Effects{}, err
			}
			return s.failStep(env, agg, step, ReasonInvalidRef)
		}
		rs, err := agg.Resolve(step)
		if err != nil {
			return s.failStep(env, agg, step, ReasonInvalidRef)
		}
		attemptID := domain.AttemptID(env.NewID("att"))
		c, err := BuildContext(run, step, attemptID, rs, binding, src, lim)
		if err != nil {
			if errors.Is(err, ErrAdmissionRefused) {
				return s.failStep(env, agg, step, ReasonStepFailed)
			}
			return Effects{}, err
		}
		plan, err := s.d.Planner.Plan(run.Scope.TenantID, c)
		if err != nil {
			return Effects{}, err
		}
		if err := env.CapacityAvailable(ctx, run.Scope, lim); err != nil {
			return Effects{}, err
		}
		timeout := time.Duration(c.Limits.TimeoutSeconds) * time.Second
		// A step that consumes no source is bound to no workspace: the attempt
		// row's shape is all-or-nothing (step_attempts_workspace_shape).
		workspaceID := run.WorkspaceID
		if binding.Access == AccessNone {
			workspaceID = ""
		}
		at, err := agg.Claim(ClaimParams{
			StepID: step.ID, AttemptID: attemptID, Plan: plan, Access: binding.Access, Workspace: workspaceID,
			Input: binding.Commit, Tip: src.AcceptedTip, Timeout: timeout, Startup: lim.StartupBudget, Lease: s.o.ClaimLease,
		}, now)
		if err != nil {
			return Effects{}, err
		}
		after := []func(context.Context, Env) error{func(ctx context.Context, env Env) error { return env.ReserveCapacity(ctx, at) }}
		if at.Writer() {
			after = append(after, func(ctx context.Context, env Env) error { return env.ReserveWriter(ctx, at) })
		}
		spec := at.spec(run.Purpose, false)
		return Effects{After: after, Result: claimOutcome{Intent: &spec}}, nil
	}
}

func (s *Service) failStep(env Env, agg *Aggregate, step *StepRun, reason Reason) (Effects, error) {
	if err := agg.FailPending(step, reason, env.Now()); err != nil {
		return Effects{}, err
	}
	after, err := advance(env, agg)
	return Effects{After: after, Result: claimOutcome{}}, err
}

func normalizeAuth(err error) error {
	switch {
	case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden):
		return ErrDenied
	case IsBusinessError(err):
		return err
	}
	return Unavailablef(err, "the authority is unavailable")
}

// DispatchNext claims one runnable step and starts its workload outside any
// transaction. A failed or lost start leaves the committed attempt
// unconfirmed; reconciliation redrives the same attempt and workload identity,
// never a new attempt. It reports whether anything was claimed.
func (s *Service) DispatchNext(ctx context.Context, tenant domain.TenantID) (bool, error) {
	if s.d.Runtime == nil {
		return false, ErrNotConfigured
	}
	spec, err := s.ClaimRunnable(ctx, tenant)
	if errors.Is(err, ErrNothingRunnable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.drive(ctx, s.d.Runtime, *spec); err != nil {
		s.log(ctx, "dispatch", err, "attempt", string(spec.AttemptID))
	}
	return true, nil
}

// drive starts one committed attempt and records the confirmation together
// with the instance the runtime reports, so every later observation, stop and
// removal is bound to that instance. The runtime verifies ownership and the
// expected spec before adopting an existing workload.
func (s *Service) drive(ctx context.Context, rt Runtime, spec AttemptSpec) error {
	// A run_shared attempt is created or adopted only while the run model, backend,
	// pins, policy, capabilities and the provisioning barrier all still hold:
	// first launch and redrive alike.
	launch, err := s.sharedDriveCheck(ctx, spec)
	if err != nil {
		return Unavailablef(err, "the run_shared launch checks do not hold")
	}
	spec.Shared = launch
	w, err := rt.Start(ctx, spec)
	if err != nil {
		return Unavailablef(err, "the runtime could not start the workload")
	}
	return s.ObserveAttempt(ctx, spec.Scope, spec.AttemptID, Observation{
		Kind: ObservedDispatched, WorkloadName: w.Name, WorkloadUID: w.UID, SpecFingerprint: spec.SpecFingerprint, ObservedAt: s.now(),
	})
}
