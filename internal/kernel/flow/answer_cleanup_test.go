package flow_test

// Answer-terminal cleanup.
//
// Pending cleanup progress moves in the same atomic payload as the terminal
// outcome, and the commit shape carries both a cleanup intent and cleanup
// progress. S02 §2 says a
// cancellation/timeout may make the outcome terminal while cleanup remains
// pending, and §5 says resource removal and workspace release are owed after a
// terminal settlement.
//
// A request answer is settled through the public SettleRequest boundary, not
// through a Reconcile loop. Unlike the container-exit and activation-failure
// terminal transitions, which are followed by advanceTerminal in the same
// Reconcile pass, an answer commit is the last thing the Kernel does for that
// execution. The store's ScanUnfinished only selects executions with a pending
// cleanup row, a running container action, or a non-terminal execution with a
// pending action. Therefore an answered request that becomes terminal with
// cleanup "none" is never selected again: the earlier succeeded container is
// never removed and the retained workspace is never released.
//
// These tests pin the reducer half: an answer that makes the
// execution terminal commits the pending cleanup intent/progress atomically
// with the terminal outcome. They use the deterministic reducer fixtures from
// reducer_test.go and never read a clock or perform I/O.

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// succeededContainerAction builds an earlier action that already ran to
// success: it owns a real workload identity, its stop barrier is confirmed, and
// its resources therefore still need removal after the execution settles.
func succeededContainerAction(actionID, name string, index int, workloadID id.WorkloadID, workspaceID id.WorkspaceID, outputs ...string) kernel.Action {
	action := containerAction(actionID, name, index, time.Hour, outputs...)
	action.State = kernel.ActionSucceeded
	action.Version = 3
	action.ActivatedAt = baseTime.Add(-2 * time.Hour)
	action.Deadline = baseTime.Add(-time.Hour)
	action.Stop = kernel.ConfirmedStop("termination observed")
	action.Container.WorkloadID = workloadID
	action.Container.WorkspaceID = workspaceID
	action.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     workloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "start-" + string(workloadID),
	}
	action.Container.Observation = kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed"))
	return action
}

// answerTerminalFixture is the durable shape after an earlier container
// succeeded and its stop was confirmed, the request is the current running
// action, and (optionally) one pending successor remains.
func answerTerminalFixture(withSuccessor bool) kernel.Execution {
	actions := []kernel.Action{
		succeededContainerAction("act-0", "build", 0, id.WorkloadID("wl-0"), id.WorkspaceID("ws-1"), "candidate"),
		requestAction("act-1", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"),
	}
	if withSuccessor {
		actions = append(actions, containerAction("act-2", "test", 2, time.Hour))
	}
	exec := newExecution(actions...)
	exec.State = kernel.ExecutionRunning
	exec.CurrentAction = 1
	exec.Version = 7
	exec.Actions[1].State = kernel.ActionRunning
	exec.Actions[1].Version = 2
	exec.Actions[1].ActivatedAt = baseTime
	exec.Actions[1].Deadline = baseTime.Add(exec.Actions[1].Timeout)
	subject := literalSubject(map[string]string{"operation": "review"})
	exec.Actions[1].Request.Subject = &subject
	return exec
}

func terminalAnswer(decision kernel.Decision, value string) kernel.Answer {
	return kernel.Answer{
		Decision:   decision,
		Outputs:    kernel.Outputs{id.OutputKey("decision"): value},
		Actor:      id.PrincipalID("principal-1"),
		At:         baseTime.Add(time.Minute),
		Provenance: []byte("signed-shell-fact"),
	}
}

// assertExactCleanup pins the intent and the projected progress together,
// including the empty-workload case that assertCleanup intentionally skips. A
// non-empty reason pins the exact actionable reason; an empty one only requires
// an attributable reason, because the contract fixes the intent but not the
// answer-specific reason text.
func assertExactCleanup(t *testing.T, before, next kernel.Execution, commit *flow.Commit, workload id.WorkloadID, reason string) {
	t.Helper()
	assertCleanup(t, before, commit, workload)
	if commit.Cleanup.WorkloadID != workload {
		t.Fatalf("cleanup intent workload = %q, want %q", commit.Cleanup.WorkloadID, workload)
	}
	if commit.CleanupUpdate.WorkloadID != workload {
		t.Fatalf("cleanup update workload = %q, want %q", commit.CleanupUpdate.WorkloadID, workload)
	}
	if reason != "" && commit.Cleanup.Reason != reason {
		t.Fatalf("cleanup reason = %q, want %q", commit.Cleanup.Reason, reason)
	}
	if next.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("projected cleanup state = %q, want pending", next.Cleanup.State)
	}
	if next.Cleanup.WorkloadID != workload || next.Cleanup.WorkspaceID != before.WorkspaceID {
		t.Fatalf("projected cleanup = %+v, want workload %q workspace %q", next.Cleanup, workload, before.WorkspaceID)
	}
}

// TestAnsweredRequestFailCommitsCleanupAtomically pins the failure mode: a
// failing answer makes the execution terminal, so the earlier succeeded
// container and the retained workspace are owed removal/release. The pending
// cleanup must travel in the same commit as ExecutionFailed; otherwise the
// unfinished scan never selects the execution and the resources leak.
func TestAnsweredRequestFailCommitsCleanupAtomically(t *testing.T) {
	at := baseTime.Add(2 * time.Minute)
	exec := answerTerminalFixture(true)
	answer := terminalAnswer(kernel.DecisionFail, "reject")
	fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-1", Answer: answer, At: at}

	next, commit := mustReduce(t, exec, fact)

	// The terminal outcome, the successor cancellation, the credential
	// invalidation and the cleanup intent are one payload.
	if commit.NextExecutionState != kernel.ExecutionFailed {
		t.Fatalf("commit next execution state = %q, want failed", commit.NextExecutionState)
	}
	if next.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].State != kernel.ActionSucceeded {
		t.Fatalf("earlier workload state = %q, want succeeded", next.Actions[0].State)
	}
	if next.Actions[1].State != kernel.ActionFailed {
		t.Fatalf("request state = %q, want failed", next.Actions[1].State)
	}
	if next.Actions[2].State != kernel.ActionCancelled {
		t.Fatalf("pending successor state = %q, want cancelled", next.Actions[2].State)
	}
	assertPrimaryAndPendingSettlement(t, exec, commit, kernel.ActionFailed, kernel.ActionCancelled)
	assertFailure(t, commit, kernel.FailureRuntime)
	if commit.Failure.Reason != "answer_rejected" {
		t.Fatalf("failure reason = %q, want answer_rejected", commit.Failure.Reason)
	}
	if !commit.InvalidateCredential {
		t.Fatal("failing answer did not invalidate the execution credential")
	}
	if commit.Answer == nil || !reflect.DeepEqual(*commit.Answer, answer) {
		t.Fatalf("commit answer = %+v, want the retained failing answer", commit.Answer)
	}
	if commit.Output.Action != flow.OutputDiscard {
		t.Fatalf("output action = %q, want discard", commit.Output.Action)
	}
	assertNoPending(t, next)
	assertSingleActiveAction(t, next)

	// The earlier container is already stop-confirmed, so the terminal answer
	// must not fabricate a new stop request; it owes removal and release.
	if commit.Stop != nil {
		t.Fatalf("failing answer fabricated a stop intent: %+v", commit.Stop)
	}
	assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
	if next.Actions[0].Container == nil || next.Actions[0].Container.WorkloadID != id.WorkloadID("wl-0") {
		t.Fatalf("cleanup attribution lost the earlier workload identity: %+v", next.Actions[0].Container)
	}
}

// TestAnsweredRequestPassOnLastActionCommitsCleanup pins the success branch: a
// passing answer on the last action makes the execution succeed, and the
// earlier workload and workspace are still owed cleanup in the same commit.
func TestAnsweredRequestPassOnLastActionCommitsCleanup(t *testing.T) {
	at := baseTime.Add(2 * time.Minute)
	exec := answerTerminalFixture(false)
	answer := terminalAnswer(kernel.DecisionPass, "approve")
	fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-1", Answer: answer, At: at}

	next, commit := mustReduce(t, exec, fact)

	if commit.NextExecutionState != kernel.ExecutionSucceeded {
		t.Fatalf("commit next execution state = %q, want succeeded", commit.NextExecutionState)
	}
	if next.State != kernel.ExecutionSucceeded {
		t.Fatalf("execution state = %q, want succeeded", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[0].State != kernel.ActionSucceeded || next.Actions[1].State != kernel.ActionSucceeded {
		t.Fatalf("actions = %q/%q, want succeeded/succeeded", next.Actions[0].State, next.Actions[1].State)
	}
	if !commit.InvalidateCredential {
		t.Fatal("terminal success did not invalidate the execution credential")
	}
	if commit.Output.Action != flow.OutputPromote {
		t.Fatalf("output action = %q, want promote", commit.Output.Action)
	}
	if len(commit.PendingCancellations) != 0 {
		t.Fatalf("last-action pass cancelled successors: %+v", commit.PendingCancellations)
	}
	if commit.Stop != nil {
		t.Fatalf("last-action pass fabricated a stop intent: %+v", commit.Stop)
	}
	assertExactCleanup(t, exec, next, commit, id.WorkloadID("wl-0"), "")
}

// TestAnsweredRequestPassBeforeLastKeepsNoCleanup pins the boundary: when a
// passing answer leaves pending work, the execution continues, so the retained
// workspace is still in use and no cleanup may be emitted.
func TestAnsweredRequestPassBeforeLastKeepsNoCleanup(t *testing.T) {
	at := baseTime.Add(2 * time.Minute)
	exec := answerTerminalFixture(true)
	answer := terminalAnswer(kernel.DecisionPass, "approve")
	fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-1", Answer: answer, At: at}

	next, commit := mustReduce(t, exec, fact)

	if commit.NextExecutionState != kernel.ExecutionRunning {
		t.Fatalf("commit next execution state = %q, want running", commit.NextExecutionState)
	}
	if next.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want running", next.State)
	}
	if next.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", next.CurrentAction)
	}
	if next.Actions[1].State != kernel.ActionSucceeded {
		t.Fatalf("request state = %q, want succeeded", next.Actions[1].State)
	}
	if next.Actions[2].State != kernel.ActionPending {
		t.Fatalf("pending successor state = %q, want pending", next.Actions[2].State)
	}
	if commit.Cleanup != nil || commit.CleanupUpdate != nil {
		t.Fatalf("non-terminal answer carried cleanup: intent=%+v update=%+v", commit.Cleanup, commit.CleanupUpdate)
	}
	if next.Cleanup.State != kernel.CleanupNone {
		t.Fatalf("projected cleanup state = %q, want none", next.Cleanup.State)
	}
	if commit.InvalidateCredential {
		t.Fatal("non-terminal answer invalidated the execution credential")
	}
	if commit.Failure != nil {
		t.Fatalf("passing non-terminal answer carried a failure: %+v", commit.Failure)
	}
}

// TestFirstActionRequestAnswerReleasesWorkspaceOnly pins the no-workload
// boundary: a request that never had an earlier container still owns the
// retained workspace. Its terminal answer must commit workspace-only cleanup
// with no workload identity, exactly as cancelling a queued/first-action
// execution does.
func TestFirstActionRequestAnswerReleasesWorkspaceOnly(t *testing.T) {
	at := baseTime.Add(2 * time.Minute)

	t.Run("fail", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		answer := terminalAnswer(kernel.DecisionFail, "reject")
		fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at}

		next, commit := mustReduce(t, exec, fact)
		if next.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", next.State)
		}
		if commit.Stop != nil {
			t.Fatalf("request-only failure fabricated a stop intent: %+v", commit.Stop)
		}
		assertExactCleanup(t, exec, next, commit, id.WorkloadID(""), "")
		if commit.Cleanup.WorkspaceID != exec.WorkspaceID || commit.CleanupUpdate.WorkspaceID != exec.WorkspaceID {
			t.Fatalf("workspace-only cleanup = %+v / %+v, want %q", commit.Cleanup, commit.CleanupUpdate, exec.WorkspaceID)
		}
	})

	t.Run("pass as last", func(t *testing.T) {
		exec := runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"))
		answer := terminalAnswer(kernel.DecisionPass, "approve")
		fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at}

		next, commit := mustReduce(t, exec, fact)
		if next.State != kernel.ExecutionSucceeded {
			t.Fatalf("execution state = %q, want succeeded", next.State)
		}
		if commit.Stop != nil {
			t.Fatalf("request-only success fabricated a stop intent: %+v", commit.Stop)
		}
		assertExactCleanup(t, exec, next, commit, id.WorkloadID(""), "")
		if commit.Cleanup.WorkspaceID != exec.WorkspaceID || commit.CleanupUpdate.WorkspaceID != exec.WorkspaceID {
			t.Fatalf("workspace-only cleanup = %+v / %+v, want %q", commit.Cleanup, commit.CleanupUpdate, exec.WorkspaceID)
		}
	})
}

// TestAnsweredRequestCleanupProjectionAndReplay pins that the returned
// projection is derived from the same commit (pending state with the exact
// identities) and that replaying the identical answer converges without
// rewriting the pending cleanup record.
func TestAnsweredRequestCleanupProjectionAndReplay(t *testing.T) {
	at := baseTime.Add(2 * time.Minute)
	exec := answerTerminalFixture(true)
	answer := terminalAnswer(kernel.DecisionFail, "reject")
	fact := flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-1", Answer: answer, At: at}

	next, commit := mustReduce(t, exec, fact)

	if commit.CleanupUpdate == nil {
		t.Fatal("commit cleanup update = nil")
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
	if next.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("projected cleanup state = %q, want pending", next.Cleanup.State)
	}

	// The identical answer retry is a converged no-op and must not clear or
	// rewrite the pending cleanup record.
	retried := mustNoop(t, next, fact)
	if retried.Cleanup != next.Cleanup {
		t.Fatalf("identical answer retry rewrote cleanup: got %+v, want %+v", retried.Cleanup, next.Cleanup)
	}

	// Reducing the same revision again is deterministic: the same atomic
	// payload, including cleanup, is produced.
	_, again := mustReduce(t, exec, fact)
	if !reflect.DeepEqual(commit, again) {
		t.Fatalf("repeat reduction produced a different commit\nfirst:  %+v\nsecond: %+v", commit, again)
	}
}

// TestExistingTerminalCleanupCommitsAreUnchanged pins the cancel, expiry and
// workspace-loss cleanup intents that already existed, so fixing the answer
// terminals cannot silently change their attribution or reason.
func TestExistingTerminalCleanupCommitsAreUnchanged(t *testing.T) {
	at := baseTime
	cases := []struct {
		name     string
		exec     kernel.Execution
		fact     flow.Fact
		workload id.WorkloadID
		reason   string
	}{
		{
			name:     "cancel running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(time.Minute)},
			workload: id.WorkloadID("wl-0"),
			reason:   "cancelled",
		},
		{
			name:     "cancel running request",
			exec:     runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at.Add(time.Minute)},
			workload: id.WorkloadID(""),
			reason:   "cancelled",
		},
		{
			name:     "cancel queued execution",
			exec:     newExecution(containerAction("act-0", "build", 0, time.Hour), requestAction("act-1", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")),
			fact:     flow.CancelFact{ExecutionID: "exec-1", Reason: flow.CancelRequested, At: at},
			workload: id.WorkloadID(""),
			reason:   "cancelled",
		},
		{
			name:     "expire running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(2 * time.Hour)},
			workload: id.WorkloadID("wl-0"),
			reason:   "deadline_expired",
		},
		{
			name:     "expire running request",
			exec:     runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.ExpireFact{ExecutionID: "exec-1", ActionID: "act-0", At: at.Add(2 * time.Hour)},
			workload: id.WorkloadID(""),
			reason:   "deadline_expired",
		},
		{
			name:     "workspace lost running container",
			exec:     runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at.Add(time.Minute)},
			workload: id.WorkloadID("wl-0"),
			reason:   "workspace_lost",
		},
		{
			name:     "workspace lost running request",
			exec:     runningRequestExecution(requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision"), containerAction("act-1", "test", 1, time.Hour)),
			fact:     flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at.Add(time.Minute)},
			workload: id.WorkloadID(""),
			reason:   "workspace_lost",
		},
		{
			name:     "workspace lost queued execution",
			exec:     newExecution(containerAction("act-0", "build", 0, time.Hour), requestAction("act-1", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")),
			fact:     flow.WorkspaceLostFact{ExecutionID: "exec-1", WorkspaceID: "ws-1", At: at.Add(time.Minute)},
			workload: id.WorkloadID(""),
			reason:   "workspace_lost",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			next, commit := mustReduce(t, test.exec, test.fact)
			assertExactCleanup(t, test.exec, next, commit, test.workload, test.reason)
		})
	}
}
