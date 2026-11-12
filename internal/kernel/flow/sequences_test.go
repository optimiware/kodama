package flow_test

// Sequence and property tests for the pure lifecycle reducer. Every sequence
// is driven by explicit fact times and explicit observations; no test sleeps,
// starts a process, or reads a clock.

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

type kindSequence struct {
	name  string
	kinds []definition.ActionKind
}

func kindSequences() []kindSequence {
	c := definition.ActionKindContainer
	r := definition.ActionKindRequest
	return []kindSequence{
		{"container", []definition.ActionKind{c}},
		{"request", []definition.ActionKind{r}},
		{"container-container", []definition.ActionKind{c, c}},
		{"container-request", []definition.ActionKind{c, r}},
		{"request-container", []definition.ActionKind{r, c}},
		{"request-request", []definition.ActionKind{r, r}},
		{"container-request-container", []definition.ActionKind{c, r, c}},
		{"request-container-request", []definition.ActionKind{r, c, r}},
		{"container-container-container", []definition.ActionKind{c, c, c}},
		{"container-request-request", []definition.ActionKind{c, r, r}},
		{"request-container-container", []definition.ActionKind{r, c, c}},
	}
}

func buildExecution(kinds []definition.ActionKind) kernel.Execution {
	actions := make([]kernel.Action, len(kinds))
	for i, kind := range kinds {
		actionID := fmt.Sprintf("act-%d", i)
		name := fmt.Sprintf("a%d", i)
		switch kind {
		case definition.ActionKindContainer:
			actions[i] = containerAction(actionID, name, i, time.Hour, "out")
		case definition.ActionKindRequest:
			subject := literalSubject(map[string]string{"operation": fmt.Sprintf("op-%d", i)})
			actions[i] = requestAction(actionID, name, i, time.Hour, subject, "decision")
		default:
			panic("unsupported action kind " + string(kind))
		}
	}
	return newExecution(actions...)
}

func activateAction(t *testing.T, exec kernel.Execution, index int, now time.Time) (kernel.Execution, *flow.Commit) {
	t.Helper()
	return mustReduce(t, exec, flow.ActivateFact{
		ExecutionID: exec.ID,
		ActionID:    exec.Actions[index].ID,
		At:          now,
	})
}

func stageContainerOutput(t *testing.T, exec kernel.Execution, index int, now time.Time) (kernel.Execution, *flow.Commit) {
	t.Helper()
	return mustReduce(t, exec, flow.OutputFact{
		ExecutionID: exec.ID,
		ActionID:    exec.Actions[index].ID,
		Outputs:     kernel.Outputs{id.OutputKey("out"): fmt.Sprintf("value-%d", index)},
		At:          now,
	})
}

// succeedAction drives the active action to a runtime-confirmed success. It
// returns the next state and the next authoritative time.
func succeedAction(t *testing.T, exec kernel.Execution, index int, now time.Time) (kernel.Execution, time.Time) {
	t.Helper()
	action := exec.Actions[index]
	if action.Kind == definition.ActionKindContainer {
		next, commit := stageContainerOutput(t, exec, index, now)
		if commit.Output.Action != flow.OutputStage {
			t.Fatalf("staging output action = %q, want stage", commit.Output.Action)
		}
		now = now.Add(time.Minute)
		next, commit = mustReduce(t, next, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    action.ID,
			Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
			At:          now,
		})
		if commit.Output.Action != flow.OutputPromote {
			t.Fatalf("successful container output action = %q, want promote", commit.Output.Action)
		}
		return next, now.Add(time.Minute)
	}

	answer := kernel.Answer{
		Decision:   kernel.DecisionPass,
		Outputs:    kernel.Outputs{id.OutputKey("decision"): fmt.Sprintf("approve-%d", index)},
		Actor:      id.PrincipalID("principal-1"),
		At:         now,
		Provenance: []byte("signed-shell-fact"),
	}
	next, commit := mustReduce(t, exec, flow.AnswerFact{
		ExecutionID: exec.ID,
		ActionID:    action.ID,
		Answer:      answer,
		At:          now,
	})
	if commit.Answer == nil || !commit.Answer.DownstreamEligible() {
		t.Fatalf("passing answer was not retained as downstream-eligible: %+v", commit.Answer)
	}
	return next, now.Add(time.Minute)
}

func failAction(t *testing.T, exec kernel.Execution, index int, now time.Time) (kernel.Execution, *flow.Commit, time.Time) {
	t.Helper()
	action := exec.Actions[index]
	if action.Kind == definition.ActionKindContainer {
		next, commit := mustReduce(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    action.ID,
			Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")),
			At:          now,
		})
		return next, commit, now.Add(time.Minute)
	}
	answer := kernel.Answer{
		Decision: kernel.DecisionFail,
		Outputs:  kernel.Outputs{id.OutputKey("decision"): "reject"},
		Actor:    id.PrincipalID("principal-1"),
		At:       now,
	}
	next, commit := mustReduce(t, exec, flow.AnswerFact{
		ExecutionID: exec.ID,
		ActionID:    action.ID,
		Answer:      answer,
		At:          now,
	})
	return next, commit, now.Add(time.Minute)
}

func cancelExecution(t *testing.T, exec kernel.Execution, now time.Time) (kernel.Execution, *flow.Commit, time.Time) {
	t.Helper()
	next, commit := mustReduce(t, exec, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelRequested,
		At:          now,
	})
	return next, commit, now.Add(time.Minute)
}

// ---------------------------------------------------------------------------
// K-ORDER: success sequences and failure/cancel at every position
// ---------------------------------------------------------------------------

func TestSuccessSequenceAcrossKindCombinations(t *testing.T) {
	for _, sequence := range kindSequences() {
		t.Run(sequence.name, func(t *testing.T) {
			exec := buildExecution(sequence.kinds)
			now := baseTime
			for i := range sequence.kinds {
				exec, _ = activateAction(t, exec, i, now)
				if exec.CurrentAction != i {
					t.Fatalf("activation %d CurrentAction = %d, want %d", i, exec.CurrentAction, i)
				}
				if exec.Actions[i].State != kernel.ActionRunning {
					t.Fatalf("activation %d state = %q, want running", i, exec.Actions[i].State)
				}
				assertSingleActiveAction(t, exec)
				now = now.Add(time.Minute)

				exec, now = succeedAction(t, exec, i, now)
				if exec.Actions[i].State != kernel.ActionSucceeded {
					t.Fatalf("action %d state = %q, want succeeded", i, exec.Actions[i].State)
				}
				if exec.CurrentAction != -1 {
					t.Fatalf("action %d CurrentAction = %d, want -1 after success", i, exec.CurrentAction)
				}
				for j := i + 1; j < len(exec.Actions); j++ {
					if exec.Actions[j].State != kernel.ActionPending {
						t.Fatalf("action %d after success of %d = %q, want pending", j, i, exec.Actions[j].State)
					}
				}
				if i == len(sequence.kinds)-1 {
					if exec.State != kernel.ExecutionSucceeded {
						t.Fatalf("final state = %q, want succeeded", exec.State)
					}
				} else if exec.State != kernel.ExecutionRunning {
					t.Fatalf("intermediate state = %q, want running", exec.State)
				}
				assertSingleActiveAction(t, exec)
			}

			if exec.State != kernel.ExecutionSucceeded {
				t.Fatalf("execution state = %q, want succeeded", exec.State)
			}
			if exec.CurrentAction != -1 {
				t.Fatalf("final CurrentAction = %d, want -1", exec.CurrentAction)
			}
			for i, action := range exec.Actions {
				if action.State != kernel.ActionSucceeded {
					t.Fatalf("final action %d state = %q, want succeeded", i, action.State)
				}
				switch action.Kind {
				case definition.ActionKindContainer:
					if got := action.Outputs[id.OutputKey("out")]; got != fmt.Sprintf("value-%d", i) {
						t.Fatalf("container %d settled output = %q, want value-%d", i, got, i)
					}
				case definition.ActionKindRequest:
					if action.Request == nil || action.Request.Answer == nil || action.Request.Answer.Decision != kernel.DecisionPass {
						t.Fatalf("request %d retained answer = %+v, want pass", i, action.Request)
					}
					if got := action.Outputs[id.OutputKey("decision")]; got != fmt.Sprintf("approve-%d", i) {
						t.Fatalf("request %d settled output = %q, want approve-%d", i, got, i)
					}
				}
			}
		})
	}
}

func TestFailureAtEveryPositionCancelsEverySuccessor(t *testing.T) {
	for _, sequence := range kindSequences() {
		for failAt := 0; failAt < len(sequence.kinds); failAt++ {
			name := fmt.Sprintf("%s/fail-at-%d", sequence.name, failAt)
			t.Run(name, func(t *testing.T) {
				exec := buildExecution(sequence.kinds)
				now := baseTime
				for i := 0; i < failAt; i++ {
					exec, _ = activateAction(t, exec, i, now)
					now = now.Add(time.Minute)
					exec, now = succeedAction(t, exec, i, now)
				}
				exec, _ = activateAction(t, exec, failAt, now)
				now = now.Add(time.Minute)

				if exec.Actions[failAt].Kind == definition.ActionKindContainer {
					exec, _ = stageContainerOutput(t, exec, failAt, now)
					now = now.Add(time.Minute)
				}
				before := exec
				exec, commit, now := failAction(t, exec, failAt, now)

				if exec.State != kernel.ExecutionFailed {
					t.Fatalf("execution state = %q, want failed", exec.State)
				}
				if exec.CurrentAction != -1 {
					t.Fatalf("CurrentAction = %d, want -1", exec.CurrentAction)
				}
				for i := 0; i < failAt; i++ {
					if exec.Actions[i].State != kernel.ActionSucceeded {
						t.Fatalf("earlier action %d state = %q, want succeeded", i, exec.Actions[i].State)
					}
				}
				if exec.Actions[failAt].State != kernel.ActionFailed {
					t.Fatalf("failed action %d state = %q, want failed", failAt, exec.Actions[failAt].State)
				}
				for i := failAt + 1; i < len(exec.Actions); i++ {
					if exec.Actions[i].State != kernel.ActionCancelled {
						t.Fatalf("successor action %d state = %q, want cancelled", i, exec.Actions[i].State)
					}
				}
				assertPrimaryAndPendingSettlement(t, before, commit, kernel.ActionFailed, kernel.ActionCancelled)
				assertFailure(t, commit, kernel.FailureRuntime)
				assertNoPending(t, exec)
				assertSingleActiveAction(t, exec)
				if before.Actions[failAt].Kind == definition.ActionKindContainer {
					if commit.Output.Action != flow.OutputDiscard {
						t.Fatalf("failed container output action = %q, want discard", commit.Output.Action)
					}
				} else {
					if commit.Output.Action == flow.OutputPromote {
						t.Fatal("failed request promoted its answer outputs")
					}
					if commit.Answer == nil || commit.Answer.DownstreamEligible() {
						t.Fatalf("failed request answer = %+v, want retained and ineligible", commit.Answer)
					}
				}
				_ = now
			})
		}
	}
}

func TestCancellationAtEveryPositionCancelsEverySuccessor(t *testing.T) {
	for _, sequence := range kindSequences() {
		for cancelAt := 0; cancelAt < len(sequence.kinds); cancelAt++ {
			name := fmt.Sprintf("%s/cancel-at-%d", sequence.name, cancelAt)
			t.Run(name, func(t *testing.T) {
				exec := buildExecution(sequence.kinds)
				now := baseTime
				for i := 0; i < cancelAt; i++ {
					exec, _ = activateAction(t, exec, i, now)
					now = now.Add(time.Minute)
					exec, now = succeedAction(t, exec, i, now)
				}
				exec, _ = activateAction(t, exec, cancelAt, now)
				now = now.Add(time.Minute)

				if exec.Actions[cancelAt].Kind == definition.ActionKindContainer {
					unissuedNext, unissuedCommit, _ := cancelExecution(t, exec, now)
					if unissuedCommit.Stop != nil {
						t.Fatalf("cancelled not_issued container carried a workload stop intent: %+v", unissuedCommit.Stop)
					}
					assertCleanup(t, exec, unissuedCommit, exec.Actions[cancelAt].Container.WorkloadID)
					assertNoPending(t, unissuedNext)
					assertSingleActiveAction(t, unissuedNext)

					container := *exec.Actions[cancelAt].Container
					container.Launch.State = kernel.LaunchIssued
					exec.Actions[cancelAt].Container = &container
					exec, _ = stageContainerOutput(t, exec, cancelAt, now)
					now = now.Add(time.Minute)
				}
				before := exec
				exec, commit, _ := cancelExecution(t, exec, now)

				if exec.State != kernel.ExecutionCancelled {
					t.Fatalf("execution state = %q, want cancelled", exec.State)
				}
				if exec.CurrentAction != -1 {
					t.Fatalf("CurrentAction = %d, want -1", exec.CurrentAction)
				}
				for i := 0; i < cancelAt; i++ {
					if exec.Actions[i].State != kernel.ActionSucceeded {
						t.Fatalf("earlier action %d state = %q, want succeeded", i, exec.Actions[i].State)
					}
				}
				for i := cancelAt; i < len(exec.Actions); i++ {
					if exec.Actions[i].State != kernel.ActionCancelled {
						t.Fatalf("cancelled action %d state = %q, want cancelled", i, exec.Actions[i].State)
					}
				}
				assertAllActionsSettled(t, before, commit, kernel.ActionCancelled)
				if commit.Failure != nil {
					t.Fatalf("cancel carried a failure: %+v", commit.Failure)
				}
				if !commit.InvalidateCredential {
					t.Fatal("cancel did not revoke execution authority")
				}
				if before.Actions[cancelAt].Kind == definition.ActionKindContainer {
					if commit.Output.Action != flow.OutputDiscard {
						t.Fatalf("cancelled container output action = %q, want discard", commit.Output.Action)
					}
					assertStopIntent(t, commit, before.Actions[cancelAt].Container.WorkloadID, before.WorkspaceID)
					assertCleanup(t, before, commit, before.Actions[cancelAt].Container.WorkloadID)
				} else {
					if commit.Stop != nil {
						t.Fatalf("cancelled request carried a workload stop intent: %+v", commit.Stop)
					}
					assertCleanup(t, before, commit, "")
				}
				assertNoPending(t, exec)
				assertSingleActiveAction(t, exec)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// K-STOP: stop barrier blocks promotion and replacement dispatch
// ---------------------------------------------------------------------------

func TestStopBarrierSequenceBlocksWorkspaceHandoff(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "out"),
		containerAction("act-1", "test", 1, time.Hour),
	)

	next, commit := stageContainerOutput(t, exec, 0, at.Add(time.Minute))
	if commit.Output.Action != flow.OutputStage {
		t.Fatalf("staging action = %q, want stage", commit.Output.Action)
	}

	// Exit zero with a lost stop response: the exit is known but stop is not
	// confirmed, so nothing is promoted and nothing advances.
	next, commit = mustReduce(t, next, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(0, kernel.PendingStop("lost stop response")),
		At:          at.Add(2 * time.Minute),
	})
	if next.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("action state after lost stop = %q, want running", next.Actions[0].State)
	}
	if next.CurrentAction != 0 {
		t.Fatalf("CurrentAction after lost stop = %d, want 0", next.CurrentAction)
	}
	if next.Actions[0].Outputs != nil {
		t.Fatalf("settled outputs after lost stop = %v, want nil", next.Actions[0].Outputs)
	}
	if !next.Actions[0].StagedOutputsSet {
		t.Fatal("lost stop cleared the staging presence marker")
	}
	if commit.Output.Action == flow.OutputPromote {
		t.Fatal("lost stop promoted staged outputs")
	}

	// Absence alone is neither proof of nonexecution nor proof of stop.
	next, commit = mustReduce(t, next, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.AbsentObservation(),
		At:          at.Add(3 * time.Minute),
	})
	if next.Actions[0].State != kernel.ActionRunning || next.CurrentAction != 0 {
		t.Fatalf("absent workload advanced the action: state=%q current=%d", next.Actions[0].State, next.CurrentAction)
	}
	if next.Actions[0].Outputs != nil {
		t.Fatalf("absent workload promoted outputs: %v", next.Actions[0].Outputs)
	}
	if commit.Output.Action != flow.OutputNone {
		t.Fatalf("absent workload output action = %q, want none", commit.Output.Action)
	}
	if commit.Launch != nil {
		t.Fatalf("absent workload carried a launch intent: %+v", commit.Launch)
	}

	// No replacement dispatch while the stop barrier is pending.
	mustRefuse(t, next, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at.Add(4 * time.Minute)}, fault.InvalidInput)

	// Confirmed stop settles the success and promotes the staged output.
	next, commit = mustReduce(t, next, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          at.Add(5 * time.Minute),
	})
	if next.Actions[0].State != kernel.ActionSucceeded {
		t.Fatalf("action state after confirmed stop = %q, want succeeded", next.Actions[0].State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction after confirmed stop = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].Outputs[id.OutputKey("out")] != "value-0" {
		t.Fatalf("settled outputs = %v, want out=value-0", next.Actions[0].Outputs)
	}
	if commit.Output.Action != flow.OutputPromote {
		t.Fatalf("successful output action = %q, want promote", commit.Output.Action)
	}

	// The next action can activate only after the confirmed stop.
	next, _ = activateAction(t, next, 1, at.Add(6*time.Minute))
	if next.CurrentAction != 1 {
		t.Fatalf("next activation CurrentAction = %d, want 1", next.CurrentAction)
	}

	// A succeeded action whose stop barrier is still pending must not hand the
	// workspace to a replacement action.
	blocked := newExecution(
		containerAction("act-0", "build", 0, time.Hour),
		containerAction("act-1", "test", 1, time.Hour),
	)
	blocked.State = kernel.ExecutionRunning
	blocked.CurrentAction = -1
	blocked.Actions[0].State = kernel.ActionSucceeded
	blocked.Actions[0].Stop = kernel.PendingStop("stop not confirmed")
	mustRefuse(t, blocked, flow.ActivateFact{ExecutionID: blocked.ID, ActionID: "act-1", At: at}, fault.InvalidInput)
	blocked.Actions[0].Stop = kernel.ConfirmedStop("termination observed")
	next, _ = mustReduce(t, blocked, flow.ActivateFact{ExecutionID: blocked.ID, ActionID: "act-1", At: at})
	if next.CurrentAction != 1 {
		t.Fatalf("activation after confirmed stop CurrentAction = %d, want 1", next.CurrentAction)
	}

	// After cancellation, an absent workload is not a success or a replacement
	// dispatch, and the terminal outcome stays immutable.
	cancelled := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "out"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	cancelled, _ = stageContainerOutput(t, cancelled, 0, at.Add(time.Minute))
	cancelledNext, _ := mustReduce(t, cancelled, flow.CancelFact{ExecutionID: cancelled.ID, Reason: flow.CancelRequested, At: at.Add(2 * time.Minute)})
	before := fingerprint(cancelledNext)
	after, observationCommit, err := flow.Reduce(cancelledNext, flow.ObserveFact{
		ExecutionID: cancelledNext.ID,
		ActionID:    "act-0",
		Observation: kernel.AbsentObservation(),
		At:          at.Add(3 * time.Minute),
	})
	if err != nil {
		if got := fingerprint(cancelledNext); got != before {
			t.Fatalf("absent observation refusal mutated terminal state")
		}
	} else if observationCommit != nil {
		if after.State != kernel.ExecutionCancelled || after.Actions[0].State != kernel.ActionCancelled {
			t.Fatalf("absent observation reopened terminal state: %q/%q", after.State, after.Actions[0].State)
		}
		if observationCommit.Output.Action == flow.OutputPromote {
			t.Fatal("absent observation promoted outputs after cancellation")
		}
		if observationCommit.Launch != nil && observationCommit.Launch.State == kernel.LaunchNotIssued {
			t.Fatal("absent observation requested a fresh launch after cancellation")
		}
	}
}

// ---------------------------------------------------------------------------
// Generated legal/illegal sequences
// ---------------------------------------------------------------------------

func propertyExecution(rng *rand.Rand) kernel.Execution {
	count := 1 + rng.Intn(4)
	actions := make([]kernel.Action, count)
	for i := range count {
		actionID := fmt.Sprintf("act-%d", i)
		name := fmt.Sprintf("a%d", i)
		if rng.Intn(2) == 0 {
			actions[i] = containerAction(actionID, name, i, time.Hour, "out")
		} else {
			subject := literalSubject(map[string]string{"operation": fmt.Sprintf("op-%d", i)})
			actions[i] = requestAction(actionID, name, i, time.Hour, subject, "decision")
		}
	}
	return newExecution(actions...)
}

func propertyFact(rng *rand.Rand, exec kernel.Execution, now time.Time) flow.Fact {
	switch exec.State {
	case kernel.ExecutionQueued:
		if len(exec.Actions) > 0 {
			return flow.ActivateFact{ExecutionID: exec.ID, ActionID: exec.Actions[0].ID, At: now}
		}
		return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
	case kernel.ExecutionRunning:
		if exec.CurrentAction >= 0 {
			action := exec.Actions[exec.CurrentAction]
			if action.Kind == definition.ActionKindContainer {
				switch rng.Intn(9) {
				case 0:
					return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.RunningObservation(), At: now}
				case 1:
					return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.UnknownObservation(), At: now}
				case 2:
					return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.AbsentObservation(), At: now}
				case 3:
					if action.StagedOutputsSet {
						return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.RunningObservation(), At: now}
					}
					return flow.OutputFact{ExecutionID: exec.ID, ActionID: action.ID, Outputs: kernel.Outputs{id.OutputKey("out"): "value"}, At: now}
				case 4:
					return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")), At: now}
				case 5:
					return flow.ObserveFact{ExecutionID: exec.ID, ActionID: action.ID, Observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")), At: now}
				case 6:
					return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
				case 7:
					return flow.ExpireFact{ExecutionID: exec.ID, ActionID: action.ID, At: action.Deadline}
				default:
					return flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: now}
				}
			}
			switch rng.Intn(5) {
			case 0:
				return flow.AnswerFact{ExecutionID: exec.ID, ActionID: action.ID, Answer: kernel.Answer{Decision: kernel.DecisionPass, Outputs: kernel.Outputs{id.OutputKey("decision"): "pass"}, Actor: id.PrincipalID("principal-1"), At: now}, At: now}
			case 1:
				return flow.AnswerFact{ExecutionID: exec.ID, ActionID: action.ID, Answer: kernel.Answer{Decision: kernel.DecisionFail, Outputs: kernel.Outputs{id.OutputKey("decision"): "fail"}, Actor: id.PrincipalID("principal-1"), At: now}, At: now}
			case 2:
				return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
			case 3:
				return flow.ExpireFact{ExecutionID: exec.ID, ActionID: action.ID, At: action.Deadline}
			default:
				return flow.WorkspaceLostFact{ExecutionID: exec.ID, WorkspaceID: exec.WorkspaceID, At: now}
			}
		}
		for _, action := range exec.Actions {
			if action.State == kernel.ActionPending {
				return flow.ActivateFact{ExecutionID: exec.ID, ActionID: action.ID, At: now}
			}
		}
		return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
	case kernel.ExecutionSucceeded:
		if rng.Intn(2) == 0 {
			return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
		}
		return flow.ActivateFact{ExecutionID: exec.ID, ActionID: exec.Actions[0].ID, At: now}
	case kernel.ExecutionFailed:
		if rng.Intn(2) == 0 {
			return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
		}
		return flow.ExpireFact{ExecutionID: exec.ID, ActionID: exec.Actions[0].ID, At: now}
	case kernel.ExecutionCancelled:
		if rng.Intn(2) == 0 {
			return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
		}
		return flow.ActivateFact{ExecutionID: exec.ID, ActionID: exec.Actions[0].ID, At: now}
	default:
		return flow.CancelFact{ExecutionID: exec.ID, Reason: flow.CancelRequested, At: now}
	}
}

func TestGeneratedSequencesPreserveAtMostOneActiveAction(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		exec := propertyExecution(rng)
		now := baseTime
		commits := 0

		for step := range 200 {
			before := fingerprint(exec)
			fact := propertyFact(rng, exec, now)
			next, commit, err := flow.Reduce(exec, fact)

			switch {
			case err != nil:
				if commit != nil {
					t.Fatalf("seed %d step %d: refusal carried a commit: %+v", seed, step, commit)
				}
				if got := fingerprint(exec); got != before {
					t.Fatalf("seed %d step %d: refusal mutated its input\nbefore:\n%s\nafter:\n%s", seed, step, before, got)
				}
				if got := fingerprint(next); got != before {
					t.Fatalf("seed %d step %d: refusal returned a changed state\nbefore:\n%s\nafter:\n%s", seed, step, before, got)
				}
			case commit == nil:
				if got := fingerprint(next); got != before {
					t.Fatalf("seed %d step %d: no-op changed state\nbefore:\n%s\nafter:\n%s", seed, step, before, got)
				}
			default:
				commits++
				if commit.ExecutionID != exec.ID {
					t.Fatalf("seed %d step %d: commit execution = %q, want %q", seed, step, commit.ExecutionID, exec.ID)
				}
				if commit.ExpectedExecutionVersion != exec.Version {
					t.Fatalf("seed %d step %d: commit expected version = %d, want %d", seed, step, commit.ExpectedExecutionVersion, exec.Version)
				}
				if !commit.At.Equal(factTime(fact)) {
					t.Fatalf("seed %d step %d: commit.At = %v, want %v", seed, step, commit.At, factTime(fact))
				}
				if next.Version != exec.Version+1 {
					t.Fatalf("seed %d step %d: next version = %d, want %d", seed, step, next.Version, exec.Version+1)
				}
				if want := applyCommit(exec, commit); !reflect.DeepEqual(next, want) {
					t.Fatalf("seed %d step %d: next state diverges from commit\n got: %#v\nwant: %#v", seed, step, next, want)
				}
				assertSingleActiveAction(t, next)
				if exec.State.Terminal() {
					if got := fingerprint(next); got != before {
						t.Fatalf("seed %d step %d: terminal state changed\nbefore:\n%s\nafter:\n%s", seed, step, before, got)
					}
				}
				if len(commit.PendingCancellations) > 0 {
					for _, action := range next.Actions {
						if action.State == kernel.ActionPending {
							t.Fatalf("seed %d step %d: action %q remains pending after a cancellation commit", seed, step, action.ID)
						}
					}
				}
			}

			exec = next
			if exec.State.Terminal() {
				break
			}
			now = now.Add(time.Duration(rng.Intn(3)+1) * time.Minute)
		}

		if commits == 0 {
			t.Fatalf("seed %d made no progress", seed)
		}
		if !exec.State.Terminal() {
			t.Fatalf("seed %d did not reach a terminal state: %q", seed, exec.State)
		}
		assertSingleActiveAction(t, exec)
		if exec.State == kernel.ExecutionSucceeded {
			for i, action := range exec.Actions {
				if action.State != kernel.ActionSucceeded {
					t.Fatalf("seed %d: succeeded execution action %d state = %q", seed, i, action.State)
				}
			}
		}
	}
}
