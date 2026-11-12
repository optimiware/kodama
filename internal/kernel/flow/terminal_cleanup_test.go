package flow_test

// Container-exit and activation-failure terminal cleanup.
//
// Pending cleanup progress moves in the same atomic payload as the terminal
// outcome, and the commit shape carries both a cleanup intent and cleanup
// progress. The store's
// unfinished scan (internal/shell/store/units/execution/engine.go) selects only
// executions with a pending cleanup row, a running container action, or a
// non-terminal execution with a pending action. A terminal execution committed
// with cleanup "none" is therefore never selected again: a crash between the
// terminal commit and the engine's later advanceTerminal seed leaves the
// stopped workload and the retained workspace leaked.
//
// These tests pin reducer-level cleanup on the container-exit terminal
// transitions and the reducer-level activation-failure terminal, the exact
// first-effect-workload attribution that mirrors
// engine.effectWorkloadIndexes, the non-terminal and stop-barrier boundaries,
// and the other terminal commits. They use
// the deterministic reducer fixtures from reducer_test.go and never read a
// clock or perform I/O.

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// unresolvedSubject references a declared input that the execution does not
// carry, so subject resolution fails before a request becomes answerable.
func unresolvedSubject() definition.Subject {
	return definition.NewSubject(map[string]definition.SubjectValue{
		"repository": {
			Kind:      definition.SubjectReference,
			Reference: definition.Reference{Input: true, Key: "repository"},
		},
	})
}

// runningLastContainerExecution builds a running execution whose last action
// is the current container action and every earlier action has already
// succeeded with its stop barrier confirmed.
func runningLastContainerExecution(actions ...kernel.Action) kernel.Execution {
	exec := newExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = len(actions) - 1
	exec.Version = 9
	last := &exec.Actions[len(actions)-1]
	last.State = kernel.ActionRunning
	last.Version = 2
	last.ActivatedAt = baseTime
	last.Deadline = baseTime.Add(last.Timeout)
	last.Container.WorkloadID = id.WorkloadID("wl-last")
	last.Container.WorkspaceID = exec.WorkspaceID
	last.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     last.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "start-wl-last",
	}
	return exec
}

// assertProjectedCleanupMatchesUpdate pins the returned projection to the
// commit's cleanup progress: the engine's advanceTerminal must observe a
// pending record instead of seeding a second one, and no attribution may be
// dropped or double counted.
func assertProjectedCleanupMatchesUpdate(t *testing.T, next kernel.Execution, commit *flow.Commit) {
	t.Helper()
	if commit.CleanupUpdate == nil {
		t.Fatal("terminal commit carries no cleanup progress")
	}
	if next.Cleanup.State != commit.CleanupUpdate.State {
		t.Fatalf("projected cleanup state = %q, want %q", next.Cleanup.State, commit.CleanupUpdate.State)
	}
	if next.Cleanup.WorkloadID != commit.CleanupUpdate.WorkloadID {
		t.Fatalf("projected cleanup workload = %q, want %q", next.Cleanup.WorkloadID, commit.CleanupUpdate.WorkloadID)
	}
	if next.Cleanup.WorkspaceID != commit.CleanupUpdate.WorkspaceID {
		t.Fatalf("projected cleanup workspace = %q, want %q", next.Cleanup.WorkspaceID, commit.CleanupUpdate.WorkspaceID)
	}
	if next.Cleanup.Attempts != commit.CleanupUpdate.Attempts || next.Cleanup.LastError != commit.CleanupUpdate.LastError {
		t.Fatalf("projected cleanup attribution = %+v, want %+v", next.Cleanup, *commit.CleanupUpdate)
	}
	if next.Cleanup.State != kernel.CleanupPending || !next.Cleanup.IsPending() {
		t.Fatalf("projected cleanup state = %q, want pending", next.Cleanup.State)
	}
	if next.Cleanup.Attempts != 0 || next.Cleanup.LastError != "" {
		t.Fatalf("projected cleanup attribution = %+v, want a fresh pending record", next.Cleanup)
	}
}

// TestContainerExitSuccessTerminalCommitsCleanupAtomically pins the success
// terminal: the last action exits with code 0 and a confirmed stop, so the
// execution succeeds, the credential is invalidated and the pending cleanup
// intent/progress travel in the same commit. Without that record the store's
// unfinished scan never selects the terminal execution again.
func TestContainerExitSuccessTerminalCommitsCleanupAtomically(t *testing.T) {
	at := baseTime.Add(time.Minute)
	observation := kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))

	t.Run("single last container attributes the exited workload", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
		exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
		exec.Actions[0].StagedOutputsSet = true
		next, commit := mustReduce(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: observation,
			At:          at,
		})

		if commit.NextExecutionState != kernel.ExecutionSucceeded || next.State != kernel.ExecutionSucceeded {
			t.Fatalf("terminal state = %q/%q, want succeeded", commit.NextExecutionState, next.State)
		}
		if commit.NextActionState != kernel.ActionSucceeded || next.Actions[0].State != kernel.ActionSucceeded {
			t.Fatalf("terminal action state = %q/%q, want succeeded", commit.NextActionState, next.Actions[0].State)
		}
		// Output settlement still travels in the same terminal payload as the
		// cleanup intent: promotion is not displaced by the new record.
		if commit.Output.Action != flow.OutputPromote || next.Actions[0].Outputs["candidate"] != "abc" {
			t.Fatalf("terminal output = %+v/%+v, want the promoted staged outputs", commit.Output, next.Actions[0].Outputs)
		}
		if next.Actions[0].StagedOutputsSet {
			t.Fatal("terminal success did not clear the staging presence marker")
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if !commit.InvalidateCredential {
			t.Fatal("terminal success did not invalidate the execution credential")
		}
		if commit.Stop != nil {
			t.Fatalf("terminal success fabricated a new stop intent: %+v", commit.Stop)
		}
		if commit.StopEvidence == nil || !commit.StopEvidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v, want the confirmed exit barrier", commit.StopEvidence)
		}

		// The just-exited workload counts as an effect workload: its launch was
		// durably issued before the command ran, so cleanup must attribute it.
		assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
		assertProjectedCleanupMatchesUpdate(t, next, commit)
	})

	t.Run("unknown launch identity still owes removal", func(t *testing.T) {
		exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
		exec.Actions[0].Container.Launch.State = kernel.LaunchUnknown
		next, commit := mustReduce(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: observation,
			At:          at,
		})

		// An ambiguous launch may have applied its effect, so it stays owed
		// removal exactly like an issued identity.
		assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
	})

	t.Run("earlier succeeded container is the first effect workload", func(t *testing.T) {
		exec := runningLastContainerExecution(
			succeededContainerAction("act-0", "build", 0, id.WorkloadID("wl-0"), id.WorkspaceID("ws-1"), "candidate"),
			containerAction("act-1", "test", 1, time.Hour, "result"),
		)
		next, commit := mustReduce(t, exec, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-1",
			Observation: observation,
			At:          at,
		})

		if next.State != kernel.ExecutionSucceeded {
			t.Fatalf("state = %q, want succeeded", next.State)
		}
		// The engine re-derives every effect workload, but the visible record
		// must name the same first effect workload its seed would choose.
		assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
		if commit.Cleanup.WorkloadID != id.WorkloadID("wl-0") || commit.CleanupUpdate.WorkloadID != id.WorkloadID("wl-0") {
			t.Fatalf("cleanup attribution = %+v/%+v, want the earlier workload wl-0", commit.Cleanup, commit.CleanupUpdate)
		}
	})
}

// TestContainerExitFailureTerminalCommitsCleanupAtomically pins the runtime
// failure terminal: a non-zero exit settles the action and execution failed,
// cancels every pending successor and carries the pending cleanup record in
// that same commit.
func TestContainerExitFailureTerminalCommitsCleanupAtomically(t *testing.T) {
	at := baseTime.Add(time.Minute)
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	exec.Actions[0].StagedOutputs = kernel.Outputs{id.OutputKey("candidate"): "abc"}
	exec.Actions[0].StagedOutputsSet = true
	next, commit := mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(2, kernel.ConfirmedStop("termination observed")),
		At:          at,
	})

	if commit.NextExecutionState != kernel.ExecutionFailed || next.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q/%q, want failed", commit.NextExecutionState, next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if commit.Stop != nil {
		t.Fatalf("runtime failure fabricated a new stop intent: %+v", commit.Stop)
	}
	if commit.StopEvidence == nil || !commit.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v, want the confirmed exit barrier", commit.StopEvidence)
	}
	assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
	assertFailure(t, commit, kernel.FailureRuntime)
	if commit.Failure.Reason != "workload_exited" {
		t.Fatalf("failure reason = %q, want workload_exited", commit.Failure.Reason)
	}
	if !commit.InvalidateCredential {
		t.Fatal("runtime failure did not invalidate the execution credential")
	}
	if next.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("pending successor state = %q, want cancelled", next.Actions[1].State)
	}
	// A failed exit discards staging in the same payload that carries cleanup.
	if commit.Output.Action != flow.OutputDiscard || next.Actions[0].StagedOutputsSet || next.Actions[0].StagedOutputs != nil {
		t.Fatalf("terminal output = %+v/%+v, want discarded staging", commit.Output, next.Actions[0].StagedOutputs)
	}

	assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
	assertProjectedCleanupMatchesUpdate(t, next, commit)
}

// TestNonTerminalContainerSuccessCommitsNoCleanup pins the boundary: a
// confirmed success with pending work still ahead leaves the execution running
// and the retained workspace in use, so no cleanup may be emitted yet.
func TestNonTerminalContainerSuccessCommitsNoCleanup(t *testing.T) {
	at := baseTime.Add(time.Minute)
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour),
	)
	next, commit := mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          at,
	})

	if commit.NextExecutionState != kernel.ExecutionRunning || next.State != kernel.ExecutionRunning {
		t.Fatalf("state = %q/%q, want running", commit.NextExecutionState, next.State)
	}
	if commit.NextActionState != kernel.ActionSucceeded || next.Actions[0].State != kernel.ActionSucceeded {
		t.Fatalf("settled action state = %q/%q, want succeeded", commit.NextActionState, next.Actions[0].State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1 until the successor activates", next.CurrentAction)
	}
	if next.Actions[1].State != kernel.ActionPending {
		t.Fatalf("successor state = %q, want pending", next.Actions[1].State)
	}
	if commit.Cleanup != nil || commit.CleanupUpdate != nil {
		t.Fatalf("non-terminal success carried cleanup: intent=%+v update=%+v", commit.Cleanup, commit.CleanupUpdate)
	}
	if next.Cleanup.State != kernel.CleanupNone {
		t.Fatalf("projected cleanup state = %q, want none", next.Cleanup.State)
	}
	if commit.InvalidateCredential {
		t.Fatal("non-terminal success invalidated the credential")
	}
}

// TestActivationFailureTerminalCommitsCleanupAtomically pins the reducer-level
// activation failure: subject resolution fails before the request becomes
// usable, the execution fails, and any earlier container workload plus the
// retained workspace must still be released through the same commit. A
// request-first execution owns no workload, so it releases the workspace only.
func TestActivationFailureTerminalCommitsCleanupAtomically(t *testing.T) {
	at := baseTime.Add(time.Minute)

	t.Run("after an earlier succeeded container", func(t *testing.T) {
		exec := newExecution(
			succeededContainerAction("act-0", "build", 0, id.WorkloadID("wl-0"), id.WorkspaceID("ws-1"), "candidate"),
			requestAction("act-1", "review", 1, time.Hour, unresolvedSubject(), "decision"),
			containerAction("act-2", "test", 2, time.Hour),
		)
		exec.State = kernel.ExecutionRunning
		exec.CurrentAction = -1
		exec.Version = 9

		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-1", At: at})

		if commit.NextExecutionState != kernel.ExecutionFailed || next.State != kernel.ExecutionFailed {
			t.Fatalf("terminal state = %q/%q, want failed", commit.NextExecutionState, next.State)
		}
		if next.CurrentAction != -1 {
			t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
		}
		if commit.ActionID != "act-1" || commit.NextActionState != kernel.ActionFailed {
			t.Fatalf("primary settlement = %q/%q, want act-1/failed", commit.ActionID, commit.NextActionState)
		}
		if next.Actions[1].State != kernel.ActionFailed {
			t.Fatalf("failed action state = %q, want failed", next.Actions[1].State)
		}
		if len(commit.PendingCancellations) != 1 ||
			commit.PendingCancellations[0].ActionID != "act-2" ||
			commit.PendingCancellations[0].NextState != kernel.ActionCancelled {
			t.Fatalf("pending cancellations = %+v, want act-2 cancelled", commit.PendingCancellations)
		}
		if next.Actions[2].State != kernel.ActionCancelled {
			t.Fatalf("pending successor state = %q, want cancelled", next.Actions[2].State)
		}
		assertFailure(t, commit, kernel.FailurePreparation)
		if commit.Failure.Reason != "subject_resolution" {
			t.Fatalf("failure reason = %q, want subject_resolution", commit.Failure.Reason)
		}
		if !commit.InvalidateCredential {
			t.Fatal("activation failure did not invalidate the execution credential")
		}
		if commit.Stop != nil {
			t.Fatalf("activation failure fabricated a stop intent: %+v", commit.Stop)
		}

		// The failed action is a request, so the earlier container is the first
		// effect workload and remains owed removal.
		assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
		assertProjectedCleanupMatchesUpdate(t, next, commit)
	})

	t.Run("request-first execution releases the workspace only", func(t *testing.T) {
		exec := newExecution(requestAction("act-0", "review", 0, time.Hour, unresolvedSubject(), "decision"))
		next, commit := mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})

		if commit.NextExecutionState != kernel.ExecutionFailed || next.State != kernel.ExecutionFailed {
			t.Fatalf("terminal state = %q/%q, want failed", commit.NextExecutionState, next.State)
		}
		assertFailure(t, commit, kernel.FailurePreparation)
		assertExactCleanup(t, exec, next, commit, id.WorkloadID(""), "")
		if commit.Cleanup.WorkspaceID != exec.WorkspaceID || commit.CleanupUpdate.WorkspaceID != exec.WorkspaceID {
			t.Fatalf("workspace-only cleanup = %+v/%+v, want %q", commit.Cleanup, commit.CleanupUpdate, exec.WorkspaceID)
		}
		if next.Cleanup.WorkloadID != "" {
			t.Fatalf("workspace-only cleanup named workload %q", next.Cleanup.WorkloadID)
		}
	})
}

// TestContainerExitWithPendingStopBarrierCommitsNoCleanup pins the stop
// barrier regression: an exit whose stop is not confirmed records evidence but
// advances neither the action nor the execution, so no cleanup is committed.
func TestContainerExitWithPendingStopBarrierCommitsNoCleanup(t *testing.T) {
	at := baseTime.Add(time.Minute)
	exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	next, commit := mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    "act-0",
		Observation: kernel.ExitedObservation(0, kernel.PendingStop("stop not confirmed")),
		At:          at,
	})

	if commit.NextExecutionState != kernel.ExecutionRunning || next.State != kernel.ExecutionRunning {
		t.Fatalf("state = %q/%q, want running", commit.NextExecutionState, next.State)
	}
	if commit.NextActionState != kernel.ActionRunning || next.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("action state = %q/%q, want running", commit.NextActionState, next.Actions[0].State)
	}
	if next.CurrentAction != 0 {
		t.Fatalf("CurrentAction = %d, want the running action 0", next.CurrentAction)
	}
	if commit.StopEvidence == nil || commit.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v, want the unconfirmed barrier", commit.StopEvidence)
	}
	if commit.Cleanup != nil || commit.CleanupUpdate != nil {
		t.Fatalf("pending stop barrier carried cleanup: intent=%+v update=%+v", commit.Cleanup, commit.CleanupUpdate)
	}
	if next.Cleanup.State != kernel.CleanupNone {
		t.Fatalf("projected cleanup state = %q, want none", next.Cleanup.State)
	}
}

// TestTerminalCleanupProjectionAndReplay pins the engine-visible invariant: the
// returned projection already carries a pending cleanup record with the
// commit's fields and fresh attribution, so advanceTerminal's seeding branch is
// skipped rather than duplicating the attempt count or losing the last error.
// Replay of the same terminal fact is a converged no-op.
func TestTerminalCleanupProjectionAndReplay(t *testing.T) {
	at := baseTime.Add(time.Minute)
	cases := []struct {
		name        string
		observation kernel.Observation
	}{
		{name: "success", observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))},
		{name: "failure", observation: kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed"))},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			exec := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
			fact := flow.ObserveFact{ExecutionID: exec.ID, ActionID: "act-0", Observation: test.observation, At: at}

			next, commit := mustReduce(t, exec, fact)
			assertProjectedCleanupMatchesUpdate(t, next, commit)
			if next.Cleanup.State != kernel.CleanupPending {
				t.Fatalf("projected cleanup state = %q, want pending", next.Cleanup.State)
			}
			if next.Cleanup.WorkloadID != id.WorkloadID("wl-0") || next.Cleanup.WorkspaceID != exec.WorkspaceID {
				t.Fatalf("projected cleanup = %+v, want wl-0/%s", next.Cleanup, exec.WorkspaceID)
			}

			// The same fact on the same revision reproduces the identical
			// payload, so a retry cannot publish a second cleanup record.
			_, again := mustReduce(t, exec, fact)
			if !reflect.DeepEqual(commit, again) {
				t.Fatalf("repeated reduction produced a different commit\nfirst:  %+v\nsecond: %+v", commit, again)
			}

			// Once the terminal revision is committed, the same fact converges
			// without a second write and the cleanup record is unchanged.
			mustNoop(t, next, fact)
		})
	}
}

// TestExistingTerminalCleanupCommitShapesUnchanged pins the terminal cleanup
// commits that already carried intent and progress, so adding the
// container-exit and activation-failure terminals cannot silently change their
// attribution or reason.
func TestExistingTerminalCleanupCommitShapesUnchanged(t *testing.T) {
	at := baseTime.Add(time.Minute)
	answer := func(decision kernel.Decision, value string) flow.AnswerFact {
		return flow.AnswerFact{
			ExecutionID: "exec-1",
			ActionID:    "act-1",
			Answer:      terminalAnswer(decision, value),
			At:          at,
		}
	}

	cases := []struct {
		name     string
		exec     kernel.Execution
		fact     flow.Fact
		workload id.WorkloadID
		reason   string
	}{
		{
			name:     "cancel running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")),
			fact:     flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at},
			workload: id.WorkloadID("wl-0"),
			reason:   "cancelled",
		},
		{
			name:     "expire running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")),
			fact:     flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: baseTime.Add(2 * time.Hour)},
			workload: id.WorkloadID("wl-0"),
			reason:   "deadline_expired",
		},
		{
			name:     "workspace lost with a running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate")),
			fact:     flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at},
			workload: id.WorkloadID("wl-0"),
			reason:   "workspace_lost",
		},
		{
			name:     "answer fail",
			exec:     answerTerminalFixture(true),
			fact:     answer(kernel.DecisionFail, "reject"),
			workload: id.WorkloadID("wl-0"),
			reason:   "",
		},
		{
			name:     "answer pass on the last action",
			exec:     answerTerminalFixture(false),
			fact:     answer(kernel.DecisionPass, "approve"),
			workload: id.WorkloadID("wl-0"),
			reason:   "",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			next, commit := mustReduce(t, test.exec, test.fact)
			assertExactCleanup(t, test.exec, next, commit, test.workload, test.reason)
			assertProjectedCleanupMatchesUpdate(t, next, commit)
		})
	}
}
