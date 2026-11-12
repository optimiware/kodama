package engine

// Runtime effects are performed only after their intent is committed. Start
// evidence is persisted before the reconciler looks for more work, so a crash
// or lost response leaves an observable stable identity rather than a reason to
// launch a replacement.
//
// A proven-never-started identity is retired before it is replaced: the
// `retiring` launch state and the affirmative proof are committed first, Remove
// follows idempotently, and only a confirmed removal commits the deterministic
// replacement identity. The engine never prepares, starts, observes or re-issues
// a retiring identity, and a crash at any point resumes from persisted state.

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// performStart materializes and starts the committed launch intent of one
// container action. The stable workload identity and idempotency key are read
// from persisted state, never derived again.
func (e *Engine) performStart(ctx context.Context, execution kernel.Execution, index int) (bool, bool, error) {
	action := execution.Actions[index]
	if action.Container == nil {
		return false, true, nil
	}

	capabilities, err := e.runtime.Capabilities(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		return false, false, err
	}
	for _, required := range execution.Snapshot.Runtime().Capabilities {
		if !capabilities.Supports(required) {
			return e.failPreparation(ctx, execution, action, "unsupported_capability", unsupportedCapability(required))
		}
	}

	identity := effectIdentity(execution.ID, action)

	// Commit the launch-attempt marker before preparing anything. From this
	// point the identity is durably issued, so cleanup can always see it and
	// stop or remove it even if Prepare or Start never completes. The marker
	// carries the same stable identity and idempotency key the effect will use.
	issued, err := e.commitLaunch(ctx, execution, action, kernel.LaunchIntent{
		WorkloadID:     identity.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: identity.IdempotencyKey,
	})
	if err != nil {
		// The marker did not become durable, so this attempt must not prepare
		// or start. Re-read so the next decision uses the winning revision:
		// Reconcile retries a version conflict and reports a storage failure.
		if _, reloadErr := e.state.Load(ctx, execution.ID); reloadErr != nil {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			return false, false, reloadErr
		}
		return false, false, err
	}
	execution = issued
	action = execution.Actions[index]

	if _, err := e.runtime.Prepare(ctx, e.prepareRequest(execution, index)); err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		// A concurrent Cancel, expiry or workspace loss may have won while
		// Prepare was in flight. This invocation never called Start, so the
		// exact attempt keeps its locally known nonexecution proof instead of
		// being settled as a preparation failure over the winner.
		return e.failPrepareAttempt(ctx, execution.ID, index, identity, err)
	}

	// Re-read after the external preparation step. A concurrent Cancel, expiry
	// or workspace loss that won the commit boundary while Prepare was in
	// flight must prevent this stale attempt from starting. The invocation
	// never called Start, so it persists affirmative NeverStarted evidence for
	// its exact identity; terminal cleanup then removes the partial resources
	// from that proof instead of waiting for an absent object or an unconfirmed
	// Stop.
	current, err := e.state.Load(ctx, execution.ID)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		return false, false, err
	}
	if !sameIssuedAttempt(current, index, identity) {
		return e.persistPrepareCancellationWitness(ctx, execution.ID, index, identity)
	}
	execution = current
	action = execution.Actions[index]

	result, err := e.runtime.Start(ctx, StartRequest{
		Identity:    identity,
		WorkspaceID: execution.WorkspaceID,
	})
	if err != nil {
		// An unclassified start error may have applied the effect. Persist the
		// ambiguous outcome and never launch this identity again. A cancelled
		// context leaves the already-issued marker in place, which cleanup can
		// still observe and stop.
		return e.persistUnknownLaunch(ctx, execution, action, identity)
	}

	switch result.Outcome {
	case StartAccepted:
		// The issued marker was committed before the effect, so acceptance
		// needs no second launch commit.
		return true, false, nil
	case StartProvenNotStarted:
		// Affirmative proof of nonexecution is the only evidence that permits a
		// fresh launch. Retire the prepared identity first: the `retiring` launch
		// state and the proof are committed before any removal effect, so a crash
		// or lost response always leaves the identity durably tracked. Removal and
		// the replacement commit happen only in the shared retirement completion.
		proof := trimDetail(result.Detail)
		if proof == "" {
			proof = "runtime reported the command never started"
		}
		_, commit, reduceErr := flow.Reduce(execution, flow.ObserveFact{
			ExecutionID: execution.ID,
			ActionID:    action.ID,
			Observation: kernel.NeverStarted(proof),
			At:          e.now(),
		})
		if reduceErr != nil || commit == nil {
			return false, true, nil
		}
		retiring, err := e.state.Commit(ctx, *commit)
		if err != nil {
			// The retirement did not become durable, so no removal or
			// replacement may happen. Reconcile re-reads a version conflict and
			// reports any other storage failure.
			return false, false, err
		}
		return e.retire(ctx, retiring, index)
	default:
		// StartUnknown, or an outcome outside the declared vocabulary. Either way
		// the effect is not provably absent.
		return e.persistUnknownLaunch(ctx, execution, action, identity)
	}
}

// persistUnknownLaunch records the ambiguous launch outcome and stops this
// reconcile pass. Observation, not relaunch, is the next step.
func (e *Engine) persistUnknownLaunch(ctx context.Context, execution kernel.Execution, action kernel.Action, identity EffectIdentity) (bool, bool, error) {
	launch := kernel.LaunchIntent{
		WorkloadID:     identity.WorkloadID,
		State:          kernel.LaunchUnknown,
		IdempotencyKey: identity.IdempotencyKey,
	}
	if _, err := e.commitLaunch(ctx, execution, action, launch); err != nil {
		// A concurrent writer may already have recorded the launch. Reload and
		// never replay the start; if the launch is still unrecorded, persist the
		// ambiguous state on the fresh revision.
		if _, reloadErr := e.markUnknownAfterStart(ctx, execution.ID, action.ID, identity); reloadErr != nil {
			return false, false, reloadErr
		}
	}
	return true, true, nil
}

// markUnknownAfterStart reloads and persists LaunchUnknown for an identity
// whose start may have happened. An already issued, unknown or retiring launch,
// a settled execution/action, or a replaced attempt identity is left untouched:
// a retiring identity is launch-terminal and a replacement belongs to a fresh
// attempt.
func (e *Engine) markUnknownAfterStart(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, identity EffectIdentity) (kernel.Execution, error) {
	var (
		last    kernel.Execution
		lastErr error
	)
	for range maxPrepareWitnessAttempts {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		current, err := e.state.Load(ctx, executionID)
		if err != nil {
			return current, err
		}
		last = current
		index := actionIndex(current, actionID)
		if index < 0 {
			return current, nil
		}
		action := current.Actions[index]
		if current.State != kernel.ExecutionRunning || action.State != kernel.ActionRunning || action.Container == nil {
			return current, nil
		}
		switch action.Container.Launch.State {
		case kernel.LaunchIssued, kernel.LaunchUnknown, kernel.LaunchRetiring:
			return current, nil
		}
		if identity.WorkloadID == "" ||
			action.Container.WorkloadID != identity.WorkloadID ||
			action.Container.Launch.WorkloadID != identity.WorkloadID ||
			action.Container.Launch.IdempotencyKey != identity.IdempotencyKey {
			return current, nil
		}
		launch := kernel.LaunchIntent{
			WorkloadID:     identity.WorkloadID,
			State:          kernel.LaunchUnknown,
			IdempotencyKey: identity.IdempotencyKey,
		}
		updated, err := e.commitLaunch(ctx, current, action, launch)
		if err != nil {
			if isVersionConflict(err) {
				lastErr = err
				continue
			}
			return updated, err
		}
		return updated, nil
	}
	return last, lastErr
}

// observe records runtime evidence about the current stable identity. A
// non-settling observation stops the pass so queued evidence is not consumed
// twice; a never-started proof commits the retirement decision and continues to
// the retiring step, which owns the removal and the replacement commit.
func (e *Engine) observe(ctx context.Context, execution kernel.Execution, index int) (bool, bool, error) {
	action := execution.Actions[index]
	if action.Container == nil {
		return false, true, nil
	}
	identity := effectIdentity(execution.ID, action)
	observation, err := e.runtime.Observe(ctx, ObserveRequest{
		Identity:    identity,
		WorkspaceID: execution.WorkspaceID,
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		if isNeverPreparedError(err) || isNotFoundError(err) {
			observation = kernel.AbsentObservation()
		} else {
			return false, false, err
		}
	}
	if !observation.Valid() {
		observation = kernel.UnknownObservation()
	}
	if observationsEqual(action.Container.Observation, observation) {
		return false, true, nil
	}

	_, commit, reduceErr := flow.Reduce(execution, flow.ObserveFact{
		ExecutionID: execution.ID,
		ActionID:    action.ID,
		Observation: observation,
		At:          e.now(),
	})
	if reduceErr != nil || commit == nil {
		return false, true, nil
	}

	// Affirmative proof of nonexecution commits the retirement decision only:
	// the `retiring` launch state and the proof travel together, and no removal
	// or replacement happens inline. The next step sees the retiring state and
	// completes removal followed by the replacement commit.
	if _, err := e.state.Commit(ctx, *commit); err != nil {
		return false, false, err
	}

	switch observation.Kind {
	case kernel.ObservationExited:
		// Only a confirmed stop barrier settles the action. An exit with a
		// pending stop records evidence and waits.
		return true, !observation.StopConfirmed(), nil
	case kernel.ObservationNeverStarted:
		// The retirement is durable; the loop continues to the retiring step.
		return true, false, nil
	default:
		return true, true, nil
	}
}

// retire completes the retirement of an identity whose `retiring` launch state
// and never-started proof are already durable. Remove is idempotent, so a crash
// before or after it resumes from persisted state. Only a confirmed removal
// reduces the `retired` fact that commits the deterministic replacement
// identity; a corrupt aggregate without the persisted proof performs no runtime
// effect and stays visibly retiring.
func (e *Engine) retire(ctx context.Context, execution kernel.Execution, index int) (bool, bool, error) {
	if index < 0 || index >= len(execution.Actions) {
		return false, true, nil
	}
	action := execution.Actions[index]
	if action.Container == nil || action.Container.Launch.State != kernel.LaunchRetiring {
		return false, true, nil
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		// The persisted proof that justified retirement is missing, so the
		// aggregate is corrupt. Removal is not safe without it: perform no
		// runtime effect and leave the visible retiring state unchanged.
		return false, true, nil
	}

	identity := effectIdentity(execution.ID, action)
	if err := e.removeNeverStarted(ctx, execution, identity); err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		// The retiring state stays durable with its proof. No replacement is
		// committed and no cleanup attempt is recorded: a later Reconcile retries
		// the idempotent removal from persisted state.
		return false, false, err
	}

	_, commit, reduceErr := flow.Reduce(execution, flow.RetiredFact{
		ExecutionID: execution.ID,
		ActionID:    action.ID,
		WorkloadID:  identity.WorkloadID,
		At:          e.now(),
	})
	if reduceErr != nil || commit == nil {
		// A refusal or nil commit means the state moved under this decision
		// (for example a Cancel or expiry won the boundary). No replacement is
		// created; Reconcile re-reads the winner.
		return false, true, nil
	}
	if _, err := e.state.Commit(ctx, *commit); err != nil {
		// A version conflict is reported so Reconcile re-reads; the confirmed
		// removal is not lost because Remove is idempotent.
		return false, false, err
	}
	return true, false, nil
}

// prepareRequest builds the activation-time materialization request from the
// immutable snapshot and the settled outputs of earlier succeeded actions.
func (e *Engine) prepareRequest(execution kernel.Execution, index int) PrepareRequest {
	action := execution.Actions[index]
	inputs := make(map[id.InputKey]string)
	for key, value := range execution.Snapshot.Inputs() {
		inputs[id.InputKey(key)] = value
	}
	outputs := make(map[id.ActionName]kernel.Outputs)
	for earlier := range index {
		candidate := execution.Actions[earlier]
		if candidate.State != kernel.ActionSucceeded {
			continue
		}
		outputs[candidate.Name] = cloneOutputs(candidate.Outputs)
	}
	task, _ := execution.Snapshot.Task()
	pipeline := execution.Snapshot.Pipeline()
	container := definition.Container{}
	if action.Container != nil {
		container = action.Container.Definition
	}
	container.Env = mergeEnv(pipeline.Env, container.Env)
	return PrepareRequest{
		Identity:    effectIdentity(execution.ID, action),
		WorkspaceID: execution.WorkspaceID,
		ActionName:  action.Name,
		TaskID:      execution.Snapshot.TaskID(),
		Container:   container,
		Inputs:      inputs,
		Outputs:     outputs,
		Task:        task,
	}
}

// mergeEnv merges pipeline-level literal defaults with one action's literal
// environment. Defaults come first in declaration order. An action entry whose
// name matches a default replaces that value in place, keeping the default's
// position; every other action entry is appended in action declaration order.
// Neither input slice is mutated, so admitted snapshot/definition state stays
// immutable.
func mergeEnv(defaults, action []definition.EnvVar) []definition.EnvVar {
	merged := make([]definition.EnvVar, 0, len(defaults)+len(action))
	merged = append(merged, defaults...)
	positions := make(map[id.EnvName]int, len(merged))
	for index, entry := range merged {
		positions[entry.Name] = index
	}
	for _, entry := range action {
		if index, ok := positions[entry.Name]; ok {
			merged[index].Value = entry.Value
			continue
		}
		positions[entry.Name] = len(merged)
		merged = append(merged, entry)
	}
	return merged
}

func effectIdentity(executionID id.ExecutionID, action kernel.Action) EffectIdentity {
	identity := EffectIdentity{ExecutionID: executionID, ActionID: action.ID}
	if action.Container != nil {
		identity.WorkloadID = action.Container.WorkloadID
		identity.IdempotencyKey = action.Container.Launch.IdempotencyKey
	}
	return identity
}

// sameIssuedAttempt reports whether the freshly loaded execution still runs the
// same issued attempt. Anything else means another writer won the boundary
// while Prepare was in flight, so this attempt must not start.
func sameIssuedAttempt(execution kernel.Execution, index int, identity EffectIdentity) bool {
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != index {
		return false
	}
	if index < 0 || index >= len(execution.Actions) {
		return false
	}
	action := execution.Actions[index]
	if action.State != kernel.ActionRunning || action.Container == nil {
		return false
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		return false
	}
	return action.Container.WorkloadID == identity.WorkloadID &&
		action.Container.Launch.IdempotencyKey == identity.IdempotencyKey
}

// removeNeverStarted removes one prepared identity whose command is
// affirmatively known never to have run. Removal is sufficient evidence; no
// stop is requested and no stop evidence is fabricated. An identity the runtime
// never prepared is already absent and needs no removal.
func (e *Engine) removeNeverStarted(ctx context.Context, execution kernel.Execution, identity EffectIdentity) error {
	removal, err := e.runtime.Remove(ctx, RemoveRequest{
		Identity:    identity,
		WorkspaceID: execution.WorkspaceID,
	})
	if err != nil {
		if isRemovalAbsentError(err) {
			return nil
		}
		return err
	}
	if !removal.Removed {
		detail := trimDetail(removal.Detail)
		if detail == "" {
			detail = "runtime did not confirm removal of a never-started workload"
		}
		return fault.New(fault.DependencyUnavailable, detail)
	}
	return nil
}

// commitLaunch persists one launch-attempt marker without changing the action
// lifecycle state. A retiring identity is launch-terminal, so a launch marker
// never rewrites its launch state.
func (e *Engine) commitLaunch(ctx context.Context, execution kernel.Execution, action kernel.Action, launch kernel.LaunchIntent) (kernel.Execution, error) {
	if action.Container != nil && action.Container.Launch.State == kernel.LaunchRetiring {
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
		Launch:                   &launch,
		At:                       e.now(),
	})
}
