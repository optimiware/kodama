package flow

// The reducer is the only place where flow facts become durable state
// changes. It is pure: it never reads a clock, allocates randomness, performs
// I/O or mutates its input. Every successful transition returns one complete
// compare-and-commit payload plus the exact post-commit projection that the
// state port would produce from that payload.
//
// Three outcomes are possible:
//
//   - (execution, nil, nil) is a converged no-op: the fact is a duplicate of
//     already recorded evidence, or it records nothing new.
//   - (execution, nil, err) is a refusal: the fact is illegal for the current
//     state, stale, malformed, late or conflicts with settled history. The
//     execution is returned unchanged and nothing is persisted.
//   - (projection, commit, nil) is one atomic transition. The projection is
//     derived from the commit alone, so a state port that applies the same
//     commit cannot diverge from the reducer.
//
// Time, identity and evidence are explicit inputs. A fact time is never used
// to re-derive a persisted deadline, and only the authoritative transaction
// time decides answer timeliness.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// Reduce applies one explicit fact to one execution.
func Reduce(execution kernel.Execution, fact Fact) (kernel.Execution, *Commit, error) {
	switch typed := fact.(type) {
	case nil:
		return execution, nil, refuseInvalid("flow: fact is nil")
	case ActivateFact:
		return reduceActivate(execution, typed)
	case ObserveFact:
		return reduceObserve(execution, typed)
	case OutputFact:
		return reduceOutput(execution, typed)
	case AnswerFact:
		return reduceAnswer(execution, typed)
	case CancelFact:
		return reduceCancel(execution, typed)
	case ExpireFact:
		return reduceExpire(execution, typed)
	case WorkspaceLostFact:
		return reduceWorkspaceLost(execution, typed)
	case RetiredFact:
		return reduceRetired(execution, typed)
	default:
		return execution, nil, refuseInvalid("flow: unsupported fact %T", fact)
	}
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

func reduceActivate(execution kernel.Execution, fact ActivateFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	if execution.State.Terminal() {
		return execution, nil, refuseInvalid("flow: execution %q is terminal and cannot activate new work", execution.ID)
	}

	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]
	if action.State != kernel.ActionPending {
		return execution, nil, refuseInvalid("flow: action %q is %q, not pending", action.ID, action.State)
	}

	// At most one action may be active, and no activation may overlap it.
	if running, err := runningActionIndex(execution); err != nil {
		return execution, nil, err
	} else if running >= 0 {
		return execution, nil, refuseInvalid("flow: action %q is already running", execution.Actions[running].ID)
	}

	// Ordered lifecycle: every earlier action must have succeeded, and every
	// earlier container workload must have confirmed its stop barrier before a
	// successor may take over the retained workspace.
	for index := range target {
		previous := execution.Actions[index]
		if previous.State != kernel.ActionSucceeded {
			return execution, nil, refuseInvalid("flow: action %q cannot activate before action %q succeeds", action.ID, previous.ID)
		}
		if previous.Kind == definition.ActionKindContainer && !previous.Stop.IsConfirmed() {
			return execution, nil, refuseInvalid("flow: action %q cannot activate before the workload of %q is confirmed stopped", action.ID, previous.ID)
		}
	}

	at := fact.At
	deadline := at.UTC().Add(action.Timeout)
	nextCurrent := target
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionRunning,
		NextCurrentAction:        &nextCurrent,
		Output:                   OutputChange{Action: OutputNone},
		Deadline:                 deadline,
		At:                       at,
	}

	switch action.Kind {
	case definition.ActionKindContainer:
		if action.Container == nil {
			return execution, nil, refuseInvalid("flow: container action %q has no container value", action.ID)
		}
		// Persist the stable effect identity and idempotency key before any
		// start request can be issued.
		launch := newLaunchIntent(execution.ID, action.ID, "activation")
		commit.Launch = &launch
	case definition.ActionKindRequest:
		if action.Request == nil {
			return execution, nil, refuseInvalid("flow: request action %q has no request value", action.ID)
		}
		subject, err := resolveSubject(execution, target, action.Request.Definition.Subject)
		if err != nil {
			// Never expose an answerable request with an incomplete subject.
			return activationFailure(execution, action, at, err)
		}
		commit.Subject = &subject
		commit.InvalidateCredential = true
	default:
		return execution, nil, refuseInvalid("flow: action %q has unsupported kind %q", action.ID, action.Kind)
	}

	next := project(execution, *commit)
	return next, commit, nil
}

// activationFailure settles the selected action as failed before any workload
// or request became usable. The failed action, the typed preparation failure
// and every pending successor cancellation travel in one commit.
func activationFailure(execution kernel.Execution, action kernel.Action, at time.Time, cause error) (kernel.Execution, *Commit, error) {
	nextCurrent := -1
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionFailed,
		PendingCancellations:     pendingCancellations(execution, action.ID),
		NextCurrentAction:        &nextCurrent,
		Output:                   OutputChange{Action: OutputNone},
		InvalidateCredential:     true,
		Failure: &kernel.Failure{
			Kind:   kernel.FailurePreparation,
			Reason: "subject_resolution",
			Detail: cause.Error(),
		},
		At: at,
	}
	// A preparation failure settles the execution before any new workload or
	// request becomes usable, but earlier container workloads and the retained
	// workspace are still owed removal/release. No later Reconcile pass is
	// guaranteed, so the pending cleanup intent/progress travels in this same
	// commit, attributed exactly as the engine's effect-workload selection is.
	workload := firstEffectWorkload(execution)
	commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "activation_failed"}
	commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}

	next := project(execution, *commit)
	return next, commit, nil
}

// resolveSubject materializes one request subject from admitted inputs and the
// settled outputs of earlier succeeded actions only. A forward or self
// reference, a missing input or a missing earlier output fails resolution.
func resolveSubject(execution kernel.Execution, target int, subject definition.Subject) (definition.Subject, error) {
	fields := subject.Fields()
	resolved := make(map[string]definition.SubjectValue, len(fields))
	var inputs map[string]string
	if execution.Snapshot != nil {
		inputs = execution.Snapshot.Inputs()
	}
	for name, value := range fields {
		if err := value.Validate(); err != nil {
			return definition.Subject{}, fmt.Errorf("subject field %q is malformed", name)
		}
		switch value.Kind {
		case definition.SubjectLiteral:
			resolved[name] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: value.Literal}
		case definition.SubjectReference:
			reference := value.Reference
			if reference.Input {
				raw, ok := inputs[reference.Key]
				if !ok {
					return definition.Subject{}, fmt.Errorf("subject field %q references missing input %q", name, reference.Key)
				}
				resolved[name] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: raw}
				continue
			}
			source := -1
			for index, action := range execution.Actions {
				if index >= target {
					break
				}
				if string(action.Name) == reference.Action {
					source = index
					break
				}
			}
			if source < 0 {
				return definition.Subject{}, fmt.Errorf("subject field %q references %q, which is not an earlier action", name, reference.Action)
			}
			earlier := execution.Actions[source]
			if earlier.State != kernel.ActionSucceeded {
				return definition.Subject{}, fmt.Errorf("subject field %q references action %q, which has not succeeded", name, reference.Action)
			}
			raw, ok := earlier.Outputs[id.OutputKey(reference.Key)]
			if !ok {
				return definition.Subject{}, fmt.Errorf("subject field %q references missing output %q of action %q", name, reference.Key, reference.Action)
			}
			resolved[name] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: raw}
		default:
			return definition.Subject{}, fmt.Errorf("subject field %q has unsupported kind %q", name, value.Kind)
		}
	}
	return definition.NewSubject(resolved), nil
}

// newLaunchIntent allocates the stable workload identity and idempotency key
// of one launch attempt. The derivation is deterministic for one input, so a
// repeated reduction of the same revision yields the same identity, while a
// fresh attempt is seeded by the previous identity and therefore differs.
func newLaunchIntent(executionID id.ExecutionID, actionID id.ActionID, seed string) kernel.LaunchIntent {
	identity := sha256.Sum256([]byte(string(executionID) + "\x00" + string(actionID) + "\x00workload\x00" + seed))
	key := sha256.Sum256([]byte(string(executionID) + "\x00" + string(actionID) + "\x00start\x00" + seed))
	return kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-" + hex.EncodeToString(identity[:16])),
		State:          kernel.LaunchNotIssued,
		IdempotencyKey: "start-" + hex.EncodeToString(key[:16]),
	}
}

// relaunchSeed binds a fresh attempt to the current identity so a relaunch
// never reuses the workload identity of the attempt it replaces.
func relaunchSeed(container *kernel.ContainerAction) string {
	if container == nil {
		return "relaunch"
	}
	return string(container.WorkloadID) + "\x00" + container.Launch.IdempotencyKey
}

// ---------------------------------------------------------------------------
// Observation
// ---------------------------------------------------------------------------

func reduceObserve(execution kernel.Execution, fact ObserveFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	if !fact.Observation.Valid() {
		return execution, nil, refuseInvalid("flow: observation is not internally consistent")
	}
	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return execution, nil, refuseInvalid("flow: action %q is not a container action", action.ID)
	}

	if execution.State.Terminal() {
		// A duplicate observation of the settled evidence converges; any other
		// observation would rewrite terminal history and is refused.
		if observationsEqual(action.Container.Observation, fact.Observation) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: observation conflicts with the settled state of action %q", action.ID)
	}
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != target || action.State != kernel.ActionRunning {
		return execution, nil, refuseInvalid("flow: action %q is not the running action", action.ID)
	}

	// A retiring identity is launch-terminal: it accepts no observation for a
	// new decision. The identical proof converges because removal is still
	// owed; every other observation would rewrite the evidence that justified
	// retirement and is refused.
	if action.Container.Launch.State == kernel.LaunchRetiring {
		if observationsEqual(action.Container.Observation, fact.Observation) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: observation conflicts with the retiring identity of action %q", action.ID)
	}

	observation := cloneObservation(fact.Observation)
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionRunning,
		Output:                   OutputChange{Action: OutputNone},
		Observation:              &observation,
		At:                       fact.At,
	}

	switch observation.Kind {
	case kernel.ObservationNeverStarted:
		// Retire-then-replace: affirmative proof of nonexecution only marks the
		// current identity as retiring. The same workload identity and
		// idempotency key are retained with the proof, and no replacement
		// identity exists until removal is confirmed by a retired fact.
		launch := kernel.LaunchIntent{
			WorkloadID:     action.Container.WorkloadID,
			State:          kernel.LaunchRetiring,
			IdempotencyKey: action.Container.Launch.IdempotencyKey,
		}
		commit.Launch = &launch
		// commit.Observation already retains the never-started proof verbatim;
		// removal retries depend on it until the replacement is committed.
	case kernel.ObservationExited:
		commit.StopEvidence = cloneStop(observation.Stop)
		code, ok := observation.ExitStatus()
		if !ok {
			return execution, nil, refuseInvalid("flow: exited observation of action %q has no exit status", action.ID)
		}
		// Settlement and workspace handoff wait for affirmative stop evidence.
		// An exit with a pending stop barrier records evidence and advances
		// nothing.
		if observation.StopConfirmed() {
			nextCurrent := -1
			commit.NextCurrentAction = &nextCurrent
			if code == 0 {
				commit.NextActionState = kernel.ActionSucceeded
				commit.Output = promotedOutput(action)
				if allOtherActionsSucceeded(execution, target) {
					commit.NextExecutionState = kernel.ExecutionSucceeded
					commit.InvalidateCredential = true
					// The confirmed exit is the last thing the Kernel does for
					// this execution. The terminal commit therefore carries
					// the pending cleanup intent/progress itself, or the
					// earlier workloads and retained workspace would never be
					// selected for removal and release again.
					workload := firstEffectWorkload(execution)
					commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "execution_succeeded"}
					commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
				}
			} else {
				commit.NextActionState = kernel.ActionFailed
				commit.NextExecutionState = kernel.ExecutionFailed
				commit.Output = OutputChange{Action: OutputDiscard}
				commit.PendingCancellations = pendingCancellations(execution, action.ID)
				commit.InvalidateCredential = true
				commit.Failure = &kernel.Failure{
					Kind:   kernel.FailureRuntime,
					Reason: "workload_exited",
					Detail: fmt.Sprintf("workload exited with status %d", code),
				}
				// A failed exit settles the execution without a later
				// Reconcile pass, so the pending cleanup intent/progress must
				// travel in this same commit. The just-exited workload counts
				// as an effect workload: its launch was durably issued before
				// the command ran, and an unknown identity may have applied it.
				workload := firstEffectWorkload(execution)
				commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "workload_exited"}
				commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
			}
		}
	}

	next := project(execution, *commit)
	return next, commit, nil
}

// promotedOutput promotes immutable staging after a runtime-confirmed success.
// No submission means nothing to promote.
func promotedOutput(action kernel.Action) OutputChange {
	if !action.StagedOutputsSet && action.StagedOutputs == nil {
		return OutputChange{Action: OutputNone}
	}
	return OutputChange{Action: OutputPromote, Values: cloneOutputs(action.StagedOutputs)}
}

// ---------------------------------------------------------------------------
// Container output submission
// ---------------------------------------------------------------------------

func reduceOutput(execution kernel.Execution, fact OutputFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return execution, nil, refuseInvalid("flow: action %q is not a container action", action.ID)
	}
	if err := fact.Outputs.Validate(action.Container.Definition.Outputs); err != nil {
		return execution, nil, err
	}

	// A settled action's identical retry returns the recorded result, whether or
	// not the execution itself has already reached a terminal state. New content
	// never rewrites settled history.
	if action.State == kernel.ActionSucceeded {
		if outputsEqual(action.Outputs, fact.Outputs) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: output submission conflicts with the settled outputs of action %q", action.ID)
	}
	if action.StagedOutputsSet || action.StagedOutputs != nil {
		if outputsEqual(action.StagedOutputs, fact.Outputs) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: staged output content for action %q changed", action.ID)
	}
	if execution.State.Terminal() {
		return execution, nil, refuseChanged("flow: output submission conflicts with the settled state of action %q", action.ID)
	}
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != target || action.State != kernel.ActionRunning {
		return execution, nil, refuseInvalid("flow: action %q is not the running action", action.ID)
	}

	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionRunning,
		Output:                   OutputChange{Action: OutputStage, Values: cloneOutputs(fact.Outputs)},
		At:                       fact.At,
	}
	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Request answer settlement
// ---------------------------------------------------------------------------

func reduceAnswer(execution kernel.Execution, fact AnswerFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]
	if action.Kind != definition.ActionKindRequest || action.Request == nil {
		return execution, nil, refuseInvalid("flow: action %q is not a request action", action.ID)
	}
	if !fact.Answer.Decision.Valid() {
		return execution, nil, refuseInvalid("flow: answer decision %q is not supported", fact.Answer.Decision)
	}
	if err := fact.Answer.Outputs.Validate(action.Request.Definition.Outputs); err != nil {
		return execution, nil, err
	}

	// An identical retry returns the recorded result, whether or not the
	// execution has already reached a terminal state. Replacement content never
	// rewrites a settled answer.
	if action.Request.Answer != nil {
		if answersEqual(action.Request.Answer, fact.Answer) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: answer for action %q conflicts with the recorded decision", action.ID)
	}
	if execution.State.Terminal() {
		return execution, nil, refuseChanged("flow: answer conflicts with the settled state of action %q", action.ID)
	}
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != target || action.State != kernel.ActionRunning {
		return execution, nil, refuseInvalid("flow: action %q is not the running action", action.ID)
	}
	if action.Request.Subject == nil {
		return execution, nil, refuseInvalid("flow: request action %q has no resolved subject", action.ID)
	}
	// Timeliness is decided by the authoritative transaction time only; the
	// sender timestamp is retained but never used. Equality expires.
	if !fact.At.Before(action.Deadline) {
		return execution, nil, fault.New(fault.DeadlineExpired, fmt.Sprintf("flow: action %q is at or past its deadline", action.ID))
	}

	answer := cloneAnswer(fact.Answer)
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionRunning,
		Output:                   OutputChange{Action: OutputNone},
		Answer:                   &answer,
		At:                       fact.At,
	}

	nextCurrent := -1
	if answer.Decision == kernel.DecisionPass {
		commit.NextActionState = kernel.ActionSucceeded
		commit.NextCurrentAction = &nextCurrent
		commit.Output = OutputChange{Action: OutputPromote, Values: cloneOutputs(answer.Outputs)}
		if allOtherActionsSucceeded(execution, target) {
			commit.NextExecutionState = kernel.ExecutionSucceeded
			commit.InvalidateCredential = true
			// The answer is the last thing the Kernel does for this
			// execution: no later Reconcile pass follows it. The terminal
			// commit therefore carries the pending cleanup intent/progress
			// itself, or the earlier workloads and retained workspace would
			// never be selected for removal and release again.
			workload := firstEffectWorkload(execution)
			commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "answer_succeeded"}
			commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
		}
	} else {
		commit.NextActionState = kernel.ActionFailed
		commit.NextExecutionState = kernel.ExecutionFailed
		commit.NextCurrentAction = &nextCurrent
		commit.Output = OutputChange{Action: OutputDiscard}
		commit.PendingCancellations = pendingCancellations(execution, action.ID)
		commit.InvalidateCredential = true
		commit.Failure = &kernel.Failure{
			Kind:   kernel.FailureRuntime,
			Reason: "answer_rejected",
			Detail: "request action was answered with a failing decision",
		}
		// A failing answer settles the execution without a later Reconcile
		// pass, so the pending cleanup intent/progress must travel in this
		// same commit. The answered action is a request, so this commit
		// requests no new stop; earlier container workloads keep their own
		// persisted stop evidence and cleanup confirms termination before
		// removal.
		workload := firstEffectWorkload(execution)
		commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "answer_rejected"}
		commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
	}

	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func reduceCancel(execution kernel.Execution, fact CancelFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	if !fact.Reason.Valid() {
		return execution, nil, refuseInvalid("flow: cancel reason %q is not supported", fact.Reason)
	}
	if execution.State.Terminal() {
		// Explicit cancellation is the only reason recorded at execution scope,
		// so an identical requested cancel converges and every other fact
		// conflicts with terminal history.
		if execution.State == kernel.ExecutionCancelled && fact.Reason == CancelRequested {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: cancellation conflicts with the settled state %q", execution.State)
	}
	if execution.State != kernel.ExecutionQueued && execution.State != kernel.ExecutionRunning {
		return execution, nil, refuseInvalid("flow: execution %q cannot be cancelled from state %q", execution.ID, execution.State)
	}

	running, err := runningActionIndex(execution)
	if err != nil {
		return execution, nil, err
	}

	nextCurrent := -1
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionCancelled,
		NextCurrentAction:        &nextCurrent,
		Output:                   OutputChange{Action: OutputDiscard},
		InvalidateCredential:     true,
		At:                       fact.At,
	}
	if running >= 0 {
		primary := execution.Actions[running]
		commit.ActionID = primary.ID
		commit.ExpectedActionVersion = primary.Version
		commit.NextActionState = kernel.ActionCancelled
	}
	commit.PendingCancellations = pendingCancellations(execution, commit.ActionID)

	workload := id.WorkloadID("")
	if running >= 0 {
		primary := execution.Actions[running]
		workload = containerWorkload(primary)
		if containerNeedsStop(primary) {
			stop := StopIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
			commit.Stop = &stop
		}
	}
	commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "cancelled"}
	commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}

	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Deadline expiry
// ---------------------------------------------------------------------------

func reduceExpire(execution kernel.Execution, fact ExpireFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]

	if execution.State.Terminal() {
		// Re-observing the same expired deadline converges. Any other expiry
		// would replace the recorded terminal cause and is refused.
		if execution.State == kernel.ExecutionFailed &&
			execution.Failure != nil && execution.Failure.Kind == kernel.FailureTimeout &&
			action.State == kernel.ActionFailed && !action.Deadline.IsZero() &&
			!fact.At.Before(action.Deadline) {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: expiry conflicts with the settled state of action %q", action.ID)
	}
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != target || action.State != kernel.ActionRunning {
		return execution, nil, refuseInvalid("flow: action %q is not the running action", action.ID)
	}
	if action.Deadline.IsZero() {
		return execution, nil, refuseInvalid("flow: running action %q has no persisted deadline", action.ID)
	}
	// The persisted deadline is authoritative and is never recalculated from a
	// changed timeout. Reaching it expires; it is not strictly-before.
	if fact.At.Before(action.Deadline) {
		return execution, nil, refuseInvalid("flow: deadline %s of action %q has not been reached", action.Deadline.UTC().Format(time.RFC3339Nano), action.ID)
	}

	nextCurrent := -1
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionFailed,
		PendingCancellations:     pendingCancellations(execution, action.ID),
		NextCurrentAction:        &nextCurrent,
		Output:                   OutputChange{Action: OutputDiscard},
		InvalidateCredential:     true,
		Failure: &kernel.Failure{
			Kind:   kernel.FailureTimeout,
			Reason: "deadline_reached",
			Detail: fmt.Sprintf("action %q reached its persisted deadline", action.ID),
		},
		Deadline: action.Deadline,
		At:       fact.At,
	}

	workload := containerWorkload(action)
	if containerNeedsStop(action) {
		stop := StopIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
		commit.Stop = &stop
	}
	commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "deadline_expired"}
	commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}

	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Workspace loss
// ---------------------------------------------------------------------------

func reduceWorkspaceLost(execution kernel.Execution, fact WorkspaceLostFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	if fact.WorkspaceID != execution.WorkspaceID {
		return execution, nil, refuseInvalid("flow: workspace %q is not the retained workspace of execution %q", fact.WorkspaceID, execution.ID)
	}
	if execution.State.Terminal() {
		if execution.State == kernel.ExecutionFailed &&
			execution.Failure != nil && execution.Failure.Kind == kernel.FailureWorkspaceLost {
			return execution, nil, nil
		}
		return execution, nil, refuseChanged("flow: workspace loss conflicts with the settled state %q", execution.State)
	}
	if execution.State != kernel.ExecutionQueued && execution.State != kernel.ExecutionRunning {
		return execution, nil, refuseInvalid("flow: execution %q cannot observe workspace loss from state %q", execution.ID, execution.State)
	}

	running, err := runningActionIndex(execution)
	if err != nil {
		return execution, nil, err
	}

	nextCurrent := -1
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionFailed,
		NextCurrentAction:        &nextCurrent,
		Output:                   OutputChange{Action: OutputDiscard},
		InvalidateCredential:     true,
		Failure: &kernel.Failure{
			Kind:   kernel.FailureWorkspaceLost,
			Reason: "workspace_missing",
			Detail: fmt.Sprintf("retained workspace %q is no longer available", execution.WorkspaceID),
		},
		At: fact.At,
	}
	if running >= 0 {
		primary := execution.Actions[running]
		commit.ActionID = primary.ID
		commit.ExpectedActionVersion = primary.Version
		commit.NextActionState = kernel.ActionFailed
	}
	commit.PendingCancellations = pendingCancellations(execution, commit.ActionID)

	// Losing the workspace is not evidence that the workload stopped. When the
	// failed action owns a live container workload whose stop is unconfirmed,
	// the same failure commit must request the stop so termination stays
	// attributable; the workspace is never reconstructed. Cleanup progress is
	// recorded as pending exactly as cancellation and expiry do, with the
	// workload identity left empty when no container workload is involved.
	workload := id.WorkloadID("")
	if running >= 0 {
		primary := execution.Actions[running]
		workload = containerWorkload(primary)
		if containerNeedsStop(primary) {
			stop := StopIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID}
			commit.Stop = &stop
		}
	}
	commit.Cleanup = &CleanupIntent{WorkloadID: workload, WorkspaceID: execution.WorkspaceID, Reason: "workspace_lost"}
	commit.CleanupUpdate = &CleanupUpdate{State: kernel.CleanupPending, WorkloadID: workload, WorkspaceID: execution.WorkspaceID}

	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Retirement
// ---------------------------------------------------------------------------

// reduceRetired consumes the confirmed removal of the retiring identity and
// commits its deterministic replacement. Removal is confirmed separately from
// the retirement decision, so this is the only place a replacement identity is
// allocated. A terminal execution never receives one, and nothing but the
// launch identity and the new identity's unknown observation changes: the
// persisted deadline, activation time, outputs and stop evidence are untouched.
func reduceRetired(execution kernel.Execution, fact RetiredFact) (kernel.Execution, *Commit, error) {
	if err := checkExecutionID(execution, fact.ExecutionID); err != nil {
		return execution, nil, err
	}
	if execution.State.Terminal() {
		return execution, nil, refuseChanged("flow: retired fact conflicts with the settled state %q of execution %q", execution.State, execution.ID)
	}
	target := actionIndex(execution, fact.ActionID)
	if target < 0 {
		return execution, nil, refuseInvalid("flow: action %q does not exist", fact.ActionID)
	}
	action := execution.Actions[target]
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return execution, nil, refuseInvalid("flow: action %q is not a container action", action.ID)
	}
	if execution.State != kernel.ExecutionRunning || execution.CurrentAction != target || action.State != kernel.ActionRunning {
		return execution, nil, refuseInvalid("flow: action %q is not the running action", action.ID)
	}
	if action.Container.Launch.State != kernel.LaunchRetiring {
		return execution, nil, refuseInvalid("flow: action %q is not retiring a workload identity", action.ID)
	}
	if fact.WorkloadID != action.Container.WorkloadID {
		return execution, nil, refuseInvalid("flow: workload %q is not the retiring identity %q of action %q", fact.WorkloadID, action.Container.WorkloadID, action.ID)
	}
	// A replacement is justified only by the persisted affirmative proof that
	// made removal safe. A malformed aggregate without it is refused.
	if !action.Container.Observation.ProofOfNonexecution() {
		return execution, nil, refuseInvalid("flow: retiring action %q has no persisted proof of nonexecution", action.ID)
	}

	// The replacement is derived deterministically from the retired identity,
	// so a retried retired fact on the same revision yields the same identity
	// and never reuses the retired workload id or key.
	launch := newLaunchIntent(execution.ID, action.ID, relaunchSeed(action.Container))
	unknown := kernel.UnknownObservation()
	commit := &Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionRunning,
		Output:                   OutputChange{Action: OutputNone},
		Launch:                   &launch,
		Observation:              &unknown,
		At:                       fact.At,
	}
	next := project(execution, *commit)
	return next, commit, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func checkExecutionID(execution kernel.Execution, executionID id.ExecutionID) error {
	if executionID != execution.ID {
		return refuseInvalid("flow: fact execution %q does not match execution %q", executionID, execution.ID)
	}
	return nil
}

func actionIndex(execution kernel.Execution, actionID id.ActionID) int {
	for index, action := range execution.Actions {
		if action.ID == actionID {
			return index
		}
	}
	return -1
}

// runningActionIndex returns the single active action index, or -1. Two running
// actions are an illegal aggregate and are refused rather than guessed.
func runningActionIndex(execution kernel.Execution) (int, error) {
	found := -1
	for index, action := range execution.Actions {
		if action.State != kernel.ActionRunning {
			continue
		}
		if found >= 0 {
			return -1, refuseInvalid("flow: execution %q has more than one running action", execution.ID)
		}
		found = index
	}
	return found, nil
}

// pendingCancellations cancels every pending action except the primary one.
// A failure, cancellation, timeout or workspace loss therefore never leaves a
// pending successor behind.
func pendingCancellations(execution kernel.Execution, primary id.ActionID) []ActionUpdate {
	var updates []ActionUpdate
	for _, action := range execution.Actions {
		if action.ID == primary || action.State != kernel.ActionPending {
			continue
		}
		updates = append(updates, ActionUpdate{
			ActionID:        action.ID,
			ExpectedVersion: action.Version,
			NextState:       kernel.ActionCancelled,
		})
	}
	return updates
}

func allOtherActionsSucceeded(execution kernel.Execution, target int) bool {
	for index, action := range execution.Actions {
		if index == target {
			continue
		}
		if action.State != kernel.ActionSucceeded {
			return false
		}
	}
	return true
}

func containerWorkload(action kernel.Action) id.WorkloadID {
	if action.Kind != definition.ActionKindContainer || action.Container == nil {
		return ""
	}
	return action.Container.WorkloadID
}

// containerNeedsStop reports whether settling an active action requires a stop
// intent. Only an unconfirmed issued or unknown launch may have started a
// command; a not_issued or retiring identity has no live workload to stop.
func containerNeedsStop(action kernel.Action) bool {
	if action.Kind != definition.ActionKindContainer ||
		action.Container == nil ||
		action.Container.WorkloadID == "" ||
		action.Stop.IsConfirmed() {
		return false
	}
	switch action.Container.Launch.State {
	case kernel.LaunchIssued, kernel.LaunchUnknown:
		return true
	default:
		return false
	}
}

// firstEffectWorkload returns the workload identity that a terminal answer must
// attribute cleanup to. It selects the first container action whose start may
// have been applied, exactly as the engine's cleanup selection does: a
// container action owning a recorded identity whose launch is not affirmatively
// never-issued. Issued, unknown and retiring identities all stay owed removal
// after the execution settles, including an earlier action that already
// succeeded. A request-only execution owns no workload, so the cleanup intent
// releases the retained workspace alone.
func firstEffectWorkload(execution kernel.Execution) id.WorkloadID {
	for _, action := range execution.Actions {
		if action.Kind != definition.ActionKindContainer || action.Container == nil {
			continue
		}
		if action.Container.WorkloadID == "" {
			continue
		}
		if action.Container.Launch.State == kernel.LaunchNotIssued {
			continue
		}
		return action.Container.WorkloadID
	}
	return ""
}

// project applies one commit to an execution exactly as the state port
// must: the expected versions are incremented once, the primary action and
// every pending cancellation are settled, the current-action index, evidence,
// output change, failure and cleanup progress move in the same step. The
// reducer returns this projection so a caller never needs to re-derive it.
func project(execution kernel.Execution, commit Commit) kernel.Execution {
	next := execution
	next.Version = commit.ExpectedExecutionVersion + 1
	next.State = commit.NextExecutionState
	next.Actions = append([]kernel.Action(nil), execution.Actions...)
	if commit.Failure != nil {
		failure := *commit.Failure
		next.Failure = &failure
	}
	if update := commit.CleanupUpdate; update != nil {
		next.Cleanup = kernel.Cleanup{
			State:       update.State,
			WorkloadID:  update.WorkloadID,
			WorkspaceID: update.WorkspaceID,
			Attempts:    update.Attempts,
			LastError:   update.LastError,
		}
	}
	if commit.NextCurrentAction != nil {
		next.CurrentAction = *commit.NextCurrentAction
	}
	if commit.ActionID != "" {
		index := actionIndex(next, commit.ActionID)
		if index >= 0 {
			action := &next.Actions[index]
			if action.Container != nil {
				container := *action.Container
				action.Container = &container
			}
			if action.Request != nil {
				request := *action.Request
				action.Request = &request
			}
			action.Version = commit.ExpectedActionVersion + 1
			action.State = commit.NextActionState
			if !commit.Deadline.IsZero() {
				action.Deadline = commit.Deadline
			}
			if action.State == kernel.ActionRunning && action.ActivatedAt.IsZero() {
				action.ActivatedAt = commit.At
			}
			switch commit.Output.Action {
			case OutputStage:
				action.StagedOutputs = cloneOutputs(commit.Output.Values)
				action.StagedOutputsSet = true
			case OutputPromote:
				action.Outputs = cloneOutputs(commit.Output.Values)
				action.StagedOutputs = nil
				action.StagedOutputsSet = false
			case OutputDiscard:
				action.StagedOutputs = nil
				action.StagedOutputsSet = false
			}
			if commit.Subject != nil {
				if action.Request == nil {
					action.Request = &kernel.RequestAction{}
				}
				subject := *commit.Subject
				action.Request.Subject = &subject
			}
			if commit.Answer != nil {
				if action.Request == nil {
					action.Request = &kernel.RequestAction{}
				}
				answer := cloneAnswer(*commit.Answer)
				action.Request.Answer = &answer
			}
			if commit.Observation != nil {
				if action.Container == nil {
					action.Container = &kernel.ContainerAction{}
				}
				action.Container.Observation = cloneObservation(*commit.Observation)
			}
			if commit.StopEvidence != nil {
				action.Stop = *commit.StopEvidence
			}
			if commit.Launch != nil {
				if action.Container == nil {
					action.Container = &kernel.ContainerAction{}
				}
				action.Container.Launch = *commit.Launch
				action.Container.WorkloadID = commit.Launch.WorkloadID
				if action.Container.WorkspaceID == "" {
					action.Container.WorkspaceID = next.WorkspaceID
				}
			}
		}
	}
	for _, update := range commit.PendingCancellations {
		index := actionIndex(next, update.ActionID)
		if index < 0 {
			continue
		}
		next.Actions[index].Version = update.ExpectedVersion + 1
		next.Actions[index].State = update.NextState
	}
	return next
}

// ---------------------------------------------------------------------------
// Value helpers
// ---------------------------------------------------------------------------

func refuseInvalid(format string, args ...any) error {
	return fault.New(fault.InvalidInput, fmt.Sprintf(format, args...))
}

func refuseChanged(format string, args ...any) error {
	return fault.New(fault.ChangedIdempotencyContent, fmt.Sprintf(format, args...))
}

func cloneOutputs(values kernel.Outputs) kernel.Outputs {
	if values == nil {
		return nil
	}
	cloned := make(kernel.Outputs, len(values))
	maps.Copy(cloned, values)
	return cloned
}

func outputsEqual(left, right kernel.Outputs) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || other != value {
			return false
		}
	}
	return true
}

func cloneAnswer(answer kernel.Answer) kernel.Answer {
	cloned := answer
	cloned.Outputs = cloneOutputs(answer.Outputs)
	cloned.Provenance = append([]byte(nil), answer.Provenance...)
	return cloned
}

// answersEqual compares the settlement content a retry must reproduce: the
// decision, the declared outputs and the authorized principal. Sender
// timestamps and transport provenance are ignored metadata, so an identical
// retry carrying a different Answer.At or different Provenance still converges
// on the recorded result. A different decision, output map or principal remains
// a replacement settlement and is refused.
func answersEqual(recorded *kernel.Answer, candidate kernel.Answer) bool {
	if recorded == nil {
		return false
	}
	return recorded.Decision == candidate.Decision &&
		outputsEqual(recorded.Outputs, candidate.Outputs) &&
		recorded.Actor == candidate.Actor
}

func cloneObservation(observation kernel.Observation) kernel.Observation {
	cloned := observation
	if observation.ExitCode != nil {
		code := *observation.ExitCode
		cloned.ExitCode = &code
	}
	cloned.Stop = cloneStop(observation.Stop)
	return cloned
}

func cloneStop(stop *kernel.StopEvidence) *kernel.StopEvidence {
	if stop == nil {
		return nil
	}
	cloned := *stop
	return &cloned
}

func observationsEqual(recorded, candidate kernel.Observation) bool {
	if recorded.Kind != candidate.Kind || recorded.Proof != candidate.Proof {
		return false
	}
	if (recorded.ExitCode == nil) != (candidate.ExitCode == nil) {
		return false
	}
	if recorded.ExitCode != nil && *recorded.ExitCode != *candidate.ExitCode {
		return false
	}
	if (recorded.Stop == nil) != (candidate.Stop == nil) {
		return false
	}
	if recorded.Stop != nil && *recorded.Stop != *candidate.Stop {
		return false
	}
	return true
}
