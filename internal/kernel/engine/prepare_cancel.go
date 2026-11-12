package engine

// A Prepare call is an external effect that happens after the issued launch
// marker is durable and before Start is requested. The invocation that performed
// it is the only place that knows whether that exact attempt ever called Start.
// When a concurrent cancellation, expiry or workspace loss wins the commit
// boundary while Prepare is in flight, the invocation must persist affirmative
// NeverStarted evidence for its identity before terminal cleanup may remove
// anything. The proof is never inferred from a cancelled execution, an issued
// launch marker, Job absence or an empty pod list: a reconstructed engine that
// sees only those values stays uncertain.
//
// The witness is one evidence-only compare-and-commit on the existing versioned
// payload. It preserves the winning execution/action states, the failure or
// timeout diagnostic, outputs, credential revocation, the persisted deadline and
// the cleanup attribution; it writes only the observation. A competing cleanup
// commit is handled by bounded reload/revalidation, so the local knowledge is
// retried onto the fresh revision instead of being dropped.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// prepareCancelNeverStartedProof is the affirmative evidence emitted by the
// Start-less Prepare invocation that lost the commit boundary. It records the
// source of the knowledge (the engine's own effect ordering), not an inference
// from any later observation.
const prepareCancelNeverStartedProof = "preparation completed without a start request after a concurrent terminal transition won"

// maxPrepareWitnessAttempts bounds the reload/revalidation of the witness
// commit after competing version conflicts.
const maxPrepareWitnessAttempts = 4

// failPrepareAttempt settles a failed Prepare for the exact issued attempt. A
// concurrent terminal fact may already have won while Prepare was in flight; in
// that case the failure must not overwrite the winner and the invocation must
// still persist its locally known nonexecution proof. Version conflicts are
// retried from a fresh read.
func (e *Engine) failPrepareAttempt(ctx context.Context, executionID id.ExecutionID, index int, identity EffectIdentity, cause error) (bool, bool, error) {
	var lastErr error
	for range maxPrepareWitnessAttempts {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		current, err := e.state.Load(ctx, executionID)
		if err != nil {
			return false, false, err
		}
		action, ok := engineActionAt(current, index)
		if !ok {
			return false, true, nil
		}
		if sameIssuedAttempt(current, index, identity) {
			if _, _, err := e.failPreparation(ctx, current, action, "preparation_failed", cause); err != nil {
				if !isVersionConflict(err) {
					return false, false, err
				}
				lastErr = err
				continue
			}
			return true, false, nil
		}
		// The attempt is no longer the current issued attempt, so a terminal
		// winner already settled it. Persist the witness instead of writing a
		// preparation failure over that outcome.
		return e.persistPrepareCancellationWitness(ctx, executionID, index, identity)
	}
	return false, false, lastErr
}

// persistPrepareCancellationWitness records the Start-less outcome of one exact
// launch attempt after a terminal transition won the boundary. It re-reads the
// authoritative state, matches execution, action, workload and launch key to the
// invocation's identity, and commits nonempty NeverStarted evidence. A proof
// that is already durable is left untouched, and an attempt that no longer
// matches is never promoted.
func (e *Engine) persistPrepareCancellationWitness(ctx context.Context, executionID id.ExecutionID, index int, identity EffectIdentity) (bool, bool, error) {
	var lastErr error
	for range maxPrepareWitnessAttempts {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		current, err := e.state.Load(ctx, executionID)
		if err != nil {
			return false, false, err
		}
		action, ok := engineActionAt(current, index)
		if !ok {
			return false, true, nil
		}
		if prepareWitnessPersisted(action, identity) {
			// The exact identity already carries its proof (for example after a
			// retried pass); continue to the proof-driven cleanup.
			return true, false, nil
		}
		if !prepareWitnessApplies(current, action, identity) {
			// The loaded state no longer presents the same unstarted issued
			// attempt, so this invocation must not claim its nonexecution.
			// Reconcile re-reads the winner and decides from persisted state.
			return true, false, nil
		}
		commit := prepareWitnessCommit(current, action, e.now())
		if _, err := e.state.Commit(ctx, commit); err != nil {
			if isVersionConflict(err) {
				// A competing cleanup or transition commit changed a guarded
				// row. Keep the local witness and revalidate it on the fresh
				// revision.
				lastErr = err
				continue
			}
			return false, false, err
		}
		return true, false, nil
	}
	return false, false, lastErr
}

// prepareWitnessCommit builds the evidence-only commit of one witness. It keeps
// the winning execution and action states and writes no failure, output,
// deadline, credential or cleanup change, so the cancellation (or failure or
// timeout) and its attribution are preserved byte for byte.
func prepareWitnessCommit(execution kernel.Execution, action kernel.Action, at time.Time) flow.Commit {
	proof := kernel.NeverStarted(prepareCancelNeverStartedProof)
	return flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       execution.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Observation:              &proof,
		At:                       at,
	}
}

// prepareWitnessApplies reports whether the loaded execution still presents the
// exact issued identity of this invocation in a settled terminal shape, so the
// invocation's local knowledge can be committed. A canceled or failed action
// with the same workload and launch key is the only representable winner; a
// running attempt can still issue Start, and any other identity is foreign.
func prepareWitnessApplies(execution kernel.Execution, action kernel.Action, identity EffectIdentity) bool {
	if !execution.State.Terminal() {
		return false
	}
	if action.Container == nil || action.Container.Launch.State != kernel.LaunchIssued {
		return false
	}
	switch action.State {
	case kernel.ActionCancelled, kernel.ActionFailed:
	default:
		return false
	}
	return prepareIdentityMatches(action, identity)
}

// prepareWitnessPersisted reports whether this exact identity already carries
// affirmative nonexecution evidence, so a retried pass converges without a
// redundant version bump.
func prepareWitnessPersisted(action kernel.Action, identity EffectIdentity) bool {
	if action.Container == nil {
		return false
	}
	if !prepareIdentityMatches(action, identity) {
		return false
	}
	return action.Container.Observation.ProofOfNonexecution()
}

// prepareIdentityMatches binds the witness to the exact invocation: the same
// action, stable workload identity and launch key. A reused or replaced attempt
// is never eligible.
func prepareIdentityMatches(action kernel.Action, identity EffectIdentity) bool {
	if action.ID != identity.ActionID || action.Container == nil {
		return false
	}
	if identity.WorkloadID == "" || identity.IdempotencyKey == "" {
		return false
	}
	return action.Container.WorkloadID == identity.WorkloadID &&
		action.Container.Launch.IdempotencyKey == identity.IdempotencyKey
}

// engineActionAt returns one action by its stable position.
func engineActionAt(execution kernel.Execution, index int) (kernel.Action, bool) {
	if index < 0 || index >= len(execution.Actions) {
		return kernel.Action{}, false
	}
	return execution.Actions[index], true
}

func isVersionConflict(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.VersionConflict
}
