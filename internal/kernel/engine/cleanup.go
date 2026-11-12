package engine

// Cleanup is independent of the terminal execution outcome. A failed or
// cancelled execution can still owe workload removal and workspace release,
// and a cleanup attempt that fails stays visible with its attempt count and
// last actionable error until a later pass succeeds.
//
// Retries are bounded by CleanupPolicy. The bound is installation policy; the
// Kernel stops issuing cleanup effects once the persisted attempt count reaches
// it and leaves the record pending with an exhaustion diagnostic. Durable
// backoff scheduling remains installation/scan cadence, and exhaustion is never
// a successful cleanup or permission to release an unconfirmed workspace.
//
// A retiring identity is launch-terminal: its persisted never-started proof lets
// cleanup remove it directly, without Observe or Stop, and terminal cleanup never
// creates a replacement identity.
//
// A never-started proof that only an observation of this pass supplied is
// committed before the identity is removed, for the same reason the retiring
// protocol commits its proof first: after removal the runtime reports absence,
// and absence is not proof.

import (
	"context"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// advanceTerminal drives the separate cleanup record of a terminal execution.
// It never changes the terminal outcome or its failure diagnostic.
func (e *Engine) advanceTerminal(ctx context.Context, execution kernel.Execution) (bool, bool, error) {
	if execution.Cleanup.State == kernel.CleanupComplete {
		return false, true, nil
	}

	// Removal and workspace release are external effects, so the cleanup intent
	// must be durable first. Cancellation, expiry, workspace loss and
	// preparation failure already commit a pending record; a terminal success
	// (or a seeded terminal record with no cleanup state) gets one here.
	if execution.Cleanup.State != kernel.CleanupPending {
		workloads := effectWorkloadIndexes(execution)
		firstWorkload := id.WorkloadID("")
		if len(workloads) > 0 {
			firstWorkload = execution.Actions[workloads[0]].Container.WorkloadID
		}
		if _, err := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, firstWorkload, execution.Cleanup.Attempts, ""); err != nil {
			return false, false, err
		}
		return true, false, nil
	}

	// The attempt bound is checked before any further stop, removal or release
	// effect. Exhaustion is never success: the record stays pending with an
	// actionable last error, so the remaining resources stay visible instead of
	// being reported clean.
	exhausted, err := e.enforceCleanupBound(ctx, execution)
	if err != nil {
		return false, false, err
	}
	if exhausted {
		return false, true, nil
	}

	workloads := effectWorkloadIndexes(execution)

	// Confirm stop for every identity whose start may have happened before
	// removing anything. A prepared but affirmatively never-started identity
	// needs no stop; defer its removal until every possibly-started identity is
	// confirmed, so the workspace is never released while a live workload may
	// still own it.
	//
	// absenceProven records identities for which the barrier established
	// confirmed stop or affirmative never-prepared evidence. Only those may
	// treat a generic NotFound from Remove as "already gone"; a possibly-started
	// identity without that evidence stays pending.
	neverStarted := make([]int, 0, len(workloads))
	absenceProven := make(map[int]bool, len(workloads))
	for _, index := range workloads {
		action := execution.Actions[index]
		if action.Stop.IsConfirmed() {
			absenceProven[index] = true
			continue
		}
		if neverStartedProof(execution, index) {
			// The identity carries affirmative proof that its command never
			// ran: either a recorded never-started observation (including a
			// retiring identity) or the persisted provenance of a preparation
			// failure that happened before Start was requested. Removal is
			// sufficient: never Observe or Stop it, never fabricate stop
			// evidence, and never create a replacement.
			neverStarted = append(neverStarted, index)
			continue
		}
		identity := effectIdentity(execution.ID, action)
		observation, err := e.runtime.Observe(ctx, ObserveRequest{
			Identity:    identity,
			WorkspaceID: execution.WorkspaceID,
		})
		if err != nil {
			if isNeverPreparedError(err) {
				// The runtime affirmatively never prepared this identity, so it
				// cannot own the workspace. There is no stop to request and no
				// stop evidence to fabricate.
				absenceProven[index] = true
				continue
			}
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			if isNotFoundError(err) {
				// A generic NotFound is not proof that a possibly-started
				// identity was never prepared or stopped. Keep the workspace
				// held and attribute the failure instead of skipping Stop.
				attempts := execution.Cleanup.Attempts + 1
				if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, err.Error()); commitErr != nil {
					return false, false, commitErr
				}
				return true, true, nil
			}
			return false, false, err
		}
		if !observation.Valid() {
			observation = kernel.UnknownObservation()
		}
		if observation.ProofOfNonexecution() {
			// Affirmative nonexecution is not a stop. Removal is sufficient and
			// must not fabricate stop evidence.
			if action.Container.Observation.Kind != kernel.ObservationExited {
				// The proof exists only in this pass, and removal destroys the
				// object that supplied it: once the identity is gone the
				// runtime can only report absence, which proves nothing. Make
				// the proof durable before any removal, so a failure of a
				// later commit of this pass resumes from the proof. A recorded
				// exit is the evidence of the terminal outcome and is never
				// overwritten.
				if err := e.commitNeverStartedEvidence(ctx, execution, action, observation); err != nil {
					return false, false, err
				}
				return true, false, nil
			}
			neverStarted = append(neverStarted, index)
			continue
		}
		progress, stop, absent, err := e.confirmStop(ctx, execution, index, observation)
		if err != nil {
			return false, false, err
		}
		if absent {
			absenceProven[index] = true
			continue
		}
		return progress, stop, nil
	}

	// Every possibly-started identity is confirmed. Remove never-started
	// identities directly without a Stop, then remove the remaining workloads
	// and release the workspace in the same pass so a removed identity is never
	// re-observed as ambiguous.
	removed := make(map[int]bool, len(neverStarted))
	for _, index := range neverStarted {
		action := execution.Actions[index]
		identity := effectIdentity(execution.ID, action)
		removal, err := e.runtime.Remove(ctx, RemoveRequest{
			Identity:    identity,
			WorkspaceID: execution.WorkspaceID,
		})
		if err != nil {
			if isRemovalAbsentError(err) {
				// Never-started proof already established that this identity
				// needs no stop, so a missing object is already clean.
				removed[index] = true
				continue
			}
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			attempts := execution.Cleanup.Attempts + 1
			if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, err.Error()); commitErr != nil {
				return false, false, commitErr
			}
			return true, true, nil
		}
		if !removal.Removed {
			attempts := execution.Cleanup.Attempts + 1
			detail := strings.TrimSpace(removal.Detail)
			if detail == "" {
				detail = "runtime did not confirm removal of a never-started workload"
			}
			if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, detail); commitErr != nil {
				return false, false, commitErr
			}
			return true, true, nil
		}
		removed[index] = true
	}

	return e.removeOwned(ctx, execution, workloads, removed, absenceProven)
}

// cleanupExhaustedPrefix marks a pending cleanup record whose retry budget is
// spent. It is persisted once so later scans stop issuing effects without
// rewriting the same record.
const cleanupExhaustedPrefix = "cleanup retry exhausted"

// enforceCleanupBound reports whether the persisted attempt count has reached
// the installation bound. On the transition to exhaustion it commits the
// visible pending record with an actionable diagnostic; after that it is a
// read-only stop.
func (e *Engine) enforceCleanupBound(ctx context.Context, execution kernel.Execution) (bool, error) {
	policy := e.cleanupPolicy()
	if execution.Cleanup.Attempts < policy.MaxAttempts {
		return false, nil
	}
	if strings.HasPrefix(execution.Cleanup.LastError, cleanupExhaustedPrefix) {
		return true, nil
	}
	if _, err := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, execution.Cleanup.WorkloadID, execution.Cleanup.Attempts, cleanupExhaustedError(execution.Cleanup.LastError)); err != nil {
		return false, err
	}
	return true, nil
}

func cleanupExhaustedError(lastError string) string {
	detail := strings.TrimSpace(lastError)
	if detail == "" {
		return cleanupExhaustedPrefix
	}
	return cleanupExhaustedPrefix + ": " + detail
}

// confirmStop observes the stable identity and then requests termination. An
// acknowledgment is never promoted to confirmation: only affirmative evidence
// completes the barrier, and absence or lease expiry is not termination proof.
//
// The observation is used to decide the stop barrier but is not written over a
// terminal action's recorded observation. The terminal outcome is immutable;
// the separate stop evidence and cleanup record are the only progress values a
// terminal execution may still gain.
func (e *Engine) confirmStop(ctx context.Context, execution kernel.Execution, index int, observation kernel.Observation) (bool, bool, bool, error) {
	action := execution.Actions[index]
	identity := effectIdentity(execution.ID, action)

	// An exited observation carries its own stop barrier. A confirmed barrier
	// completes cleanup; a pending one is recorded without a redundant request.
	if observation.Valid() && observation.Kind == kernel.ObservationExited && observation.Stop != nil {
		if _, err := e.commitStopEvidence(ctx, execution, action, *observation.Stop); err != nil {
			return false, false, false, err
		}
		return true, !observation.Stop.IsConfirmed(), false, nil
	}

	evidence, stopErr := e.runtime.Stop(ctx, StopRequest{
		Identity:    identity,
		WorkspaceID: execution.WorkspaceID,
	})
	if stopErr != nil {
		if isNeverPreparedError(stopErr) {
			// The runtime affirmatively never prepared this identity, so there
			// is nothing to stop and no stop evidence to fabricate. Treat it as
			// already absent and keep checking the other owned identities.
			return false, false, true, nil
		}
		if ctx.Err() != nil {
			return false, false, false, ctx.Err()
		}
		attempts := execution.Cleanup.Attempts + 1
		if _, err := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, stopErr.Error()); err != nil {
			return false, false, false, err
		}
		return true, true, false, nil
	}

	if !evidence.IsConfirmed() {
		// A successful Stop call is still only an acknowledgment, never a
		// failed attempt. A terminating workload (for example a pod inside its
		// termination grace period) acknowledges every scan pass, so counting
		// acknowledgments would spend the installation bound in a few passes
		// and strand its removal and the workspace release after it stops.
		// Record the barrier with the exact identity it waits on and the
		// pending detail, but leave the attempt count unchanged: only failed
		// Stop, Remove and Release effects are attempts.
		detail := strings.TrimSpace(evidence.Detail)
		if detail == "" {
			detail = "runtime acknowledged the stop without termination evidence"
		}
		if _, err := e.commitStopProgress(ctx, execution, action, evidence, execution.Cleanup.Attempts, detail); err != nil {
			return false, false, false, err
		}
		return true, true, false, nil
	}

	if _, err := e.commitStopEvidence(ctx, execution, action, evidence); err != nil {
		return false, false, false, err
	}
	return true, false, false, nil
}

// removeOwned removes every effect-owning workload after its stop is
// confirmed, then releases the retained workspace. A failure is attributed
// before returning so a crash between attempts stays actionable. A generic
// NotFound is accepted only for identities whose barrier already established
// confirmed stop or affirmative never-prepared evidence; absenceProven carries
// that evidence for the current pass.
func (e *Engine) removeOwned(ctx context.Context, execution kernel.Execution, workloads []int, alreadyRemoved, absenceProven map[int]bool) (bool, bool, error) {
	attempts := execution.Cleanup.Attempts
	lastWorkload := execution.Cleanup.WorkloadID
	for _, index := range workloads {
		action := execution.Actions[index]
		identity := effectIdentity(execution.ID, action)
		// Attribution names the last owned identity in action order, including
		// a never-started identity removed earlier in this pass, so a completed
		// record never drops the identity cleanup acted on.
		lastWorkload = identity.WorkloadID
		if alreadyRemoved[index] {
			continue
		}

		removal, err := e.runtime.Remove(ctx, RemoveRequest{
			Identity:    identity,
			WorkspaceID: execution.WorkspaceID,
		})
		if err != nil {
			if isNeverPreparedError(err) {
				// Affirmative never-prepared evidence: there is nothing left to
				// remove and no reason to block workspace release.
				continue
			}
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			if isNotFoundError(err) && (action.Stop.IsConfirmed() || absenceProven[index]) {
				// The barrier already established confirmed stop or
				// never-prepared evidence, so a vanished object is already
				// clean. A possibly-started identity without that evidence
				// stays pending with attribution.
				continue
			}
			attempts++
			if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, err.Error()); commitErr != nil {
				return false, false, commitErr
			}
			return true, true, nil
		}
		if !removal.Removed {
			attempts++
			detail := strings.TrimSpace(removal.Detail)
			if detail == "" {
				detail = "runtime did not confirm resource removal"
			}
			if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, identity.WorkloadID, attempts, detail); commitErr != nil {
				return false, false, commitErr
			}
			return true, true, nil
		}
	}

	if err := e.workspace.Release(ctx, execution.WorkspaceID); err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.NotFound {
			attempts++
			if _, commitErr := e.commitCleanupProgress(ctx, execution, kernel.CleanupPending, lastWorkload, attempts, err.Error()); commitErr != nil {
				return false, false, commitErr
			}
			return true, true, nil
		}
		// An unknown workspace has nothing left to release. Ownership is
		// already gone, so cleanup may complete.
	}

	if _, err := e.commitCleanupComplete(ctx, execution, lastWorkload, attempts); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// effectWorkloadIndexes returns the container actions that own an external
// workload identity whose start may have been applied. A never-issued identity
// has no command effect to stop or remove; issued, unknown and retiring
// identities stay tracked until removal is confirmed.
func effectWorkloadIndexes(execution kernel.Execution) []int {
	var indexes []int
	for index, action := range execution.Actions {
		if action.Kind != definition.ActionKindContainer || action.Container == nil {
			continue
		}
		if action.Container.WorkloadID == "" {
			continue
		}
		if action.Container.Launch.State == kernel.LaunchNotIssued {
			continue
		}
		indexes = append(indexes, index)
	}
	return indexes
}

// neverStartedProof reports whether terminal cleanup already holds affirmative,
// durable evidence that this action's command never ran. A recorded
// never-started observation (a retiring identity or one observed before the
// terminal settlement) qualifies, and so does the persisted provenance of a
// preparation failure that happened before Start was requested. Absence and an
// unconfirmed Stop never qualify.
func neverStartedProof(execution kernel.Execution, index int) bool {
	if index < 0 || index >= len(execution.Actions) {
		return false
	}
	action := execution.Actions[index]
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return false
	}
	if action.Container.Observation.ProofOfNonexecution() {
		return true
	}
	return preparationProvenNotStarted(execution, index)
}

// preparationProvenNotStarted derives pre-Start nonexecution evidence from the
// persisted preparation failure of the exact failed action. The engine records
// that failure before Start is ever called, so the pair (failed action,
// preparation failure) is affirmative evidence, including for records persisted
// before the evidence was stored as an observation. An unknown launch outcome
// is genuinely ambiguous and is never promoted to never-started.
func preparationProvenNotStarted(execution kernel.Execution, index int) bool {
	if execution.Failure == nil || execution.Failure.Kind != kernel.FailurePreparation {
		return false
	}
	if index < 0 || index >= len(execution.Actions) {
		return false
	}
	action := execution.Actions[index]
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return false
	}
	if action.State != kernel.ActionFailed {
		return false
	}
	if action.Container.WorkloadID == "" {
		return false
	}
	return action.Container.Launch.State == kernel.LaunchIssued
}

// stopEvidenceEqual reports whether two stop barriers record the same state and
// detail. The barrier's workload identity lives on the cleanup record and is
// compared separately.
func stopEvidenceEqual(persisted, candidate kernel.StopEvidence) bool {
	return persisted.State == candidate.State && persisted.Detail == candidate.Detail
}

// cleanupProgressEqual reports whether the candidate cleanup progress rewrites
// exactly the persisted record. An identical write would only bump the
// execution and action row versions, which is not a durable fact: it makes a
// concurrent compare-and-commit (for example a user task.reply) lose with
// version_conflict, so the reconciler skips it.
func cleanupProgressEqual(persisted kernel.Cleanup, state kernel.CleanupState, workloadID id.WorkloadID, workspaceID id.WorkspaceID, attempts int, lastError string) bool {
	return persisted.State == state &&
		persisted.WorkloadID == workloadID &&
		persisted.WorkspaceID == workspaceID &&
		persisted.Attempts == attempts &&
		persisted.LastError == lastError
}

// commitNeverStartedEvidence persists a never-started proof that terminal
// cleanup obtained from the runtime. It is an evidence-only commit: the
// terminal execution and action states, the failure, outputs and the cleanup
// record are unchanged.
func (e *Engine) commitNeverStartedEvidence(ctx context.Context, execution kernel.Execution, action kernel.Action, observation kernel.Observation) error {
	proof := observation
	_, err := e.state.Commit(ctx, flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       execution.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Observation:              &proof,
		At:                       e.now(),
	})
	return err
}

func (e *Engine) commitStopEvidence(ctx context.Context, execution kernel.Execution, action kernel.Action, evidence kernel.StopEvidence) (kernel.Execution, error) {
	cloned := evidence
	if stopEvidenceEqual(action.Stop, cloned) {
		return execution, nil
	}
	return e.state.Commit(ctx, flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       execution.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		StopEvidence:             &cloned,
		At:                       e.now(),
	})
}

// commitStopProgress records an unconfirmed stop barrier together with the
// exact identity it waits on and the pending detail. The barrier stays pending
// and attributable; attempts carries the existing failed-effect count unchanged,
// because an acknowledgment is not a failed cleanup attempt.
func (e *Engine) commitStopProgress(ctx context.Context, execution kernel.Execution, action kernel.Action, evidence kernel.StopEvidence, attempts int, lastError string) (kernel.Execution, error) {
	cloned := evidence
	identity := effectIdentity(execution.ID, action)
	if stopEvidenceEqual(action.Stop, cloned) &&
		cleanupProgressEqual(execution.Cleanup, kernel.CleanupPending, identity.WorkloadID, execution.WorkspaceID, attempts, lastError) {
		return execution, nil
	}
	return e.state.Commit(ctx, flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       execution.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		StopEvidence:             &cloned,
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  identity.WorkloadID,
			WorkspaceID: execution.WorkspaceID,
			Attempts:    attempts,
			LastError:   lastError,
		},
		At: e.now(),
	})
}

func (e *Engine) commitCleanupProgress(ctx context.Context, execution kernel.Execution, state kernel.CleanupState, workloadID id.WorkloadID, attempts int, lastError string) (kernel.Execution, error) {
	if cleanupProgressEqual(execution.Cleanup, state, workloadID, execution.WorkspaceID, attempts, lastError) {
		return execution, nil
	}
	return e.state.Commit(ctx, flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       execution.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		CleanupUpdate: &flow.CleanupUpdate{
			State:       state,
			WorkloadID:  workloadID,
			WorkspaceID: execution.WorkspaceID,
			Attempts:    attempts,
			LastError:   lastError,
		},
		At: e.now(),
	})
}

func (e *Engine) commitCleanupComplete(ctx context.Context, execution kernel.Execution, workloadID id.WorkloadID, attempts int) (kernel.Execution, error) {
	return e.commitCleanupProgress(ctx, execution, kernel.CleanupComplete, workloadID, attempts, "")
}
