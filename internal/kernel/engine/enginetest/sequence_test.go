package enginetest

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// TestEssentialSequenceAcrossContainerRequestAndRestart pins the S02 §7
// essential fake sequence: container stages candidate -> confirmed success ->
// request activates without a workload or credential delivery -> controller
// restart -> authorized pass -> next container shares the retained workspace ->
// terminal success.
func TestEssentialSequenceAcrossContainerRequestAndRestart(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	review := requestAction("act-review", "review", 1, time.Hour, definition.NewSubject(map[string]definition.SubjectValue{
		"resource":  inputReference("repository"),
		"candidate": actionReference("build", "candidate"),
	}), "decision")
	test := containerAction("act-test", "test", 2, time.Hour)
	execution := newExecution("exec-essential", "ws-essential", build, review, test)
	h := newHarness(t, execution)

	// Activation persists the workspace, the absolute deadline and the launch
	// intent before materializing and starting the workload.
	activated := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	assertSingleActiveAction(t, activated)
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("activation did not retain the execution workspace")
	}
	buildAction := mustAction(t, activated, "build")
	if buildAction.Deadline.IsZero() {
		t.Fatal("activation did not persist an absolute deadline")
	}
	if buildAction.Container == nil || buildAction.Container.WorkloadID == "" {
		t.Fatalf("activation did not persist a stable workload identity: %+v", buildAction.Container)
	}
	if buildAction.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state after start = %q, want issued", buildAction.Container.Launch.State)
	}
	buildWorkload := buildAction.Container.WorkloadID
	assertWorkloadStarted(t, h.fixture.Runtime, buildWorkload)

	// Stage the candidate output.
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
		t.Fatalf("SubmitOutputs(build) error = %v", err)
	}
	staged := h.load()
	stagedBuild := mustAction(t, staged, "build")
	if !stagedBuild.StagedOutputsSet || stagedBuild.StagedOutputs["candidate"] != "abc" {
		t.Fatalf("staged outputs = %+v set=%v, want candidate=abc", stagedBuild.StagedOutputs, stagedBuild.StagedOutputsSet)
	}
	if len(stagedBuild.Outputs) != 0 {
		t.Fatalf("settled outputs before settlement = %+v, want empty", stagedBuild.Outputs)
	}

	// Exit zero is only a success with a separate confirmed stop barrier.
	h.confirmRuntimeStop(identityFor(h.executionID, stagedBuild))
	h.fixture.Runtime.QueueObservation(buildWorkload, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
	settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionSucceeded
	})
	settledBuild := mustAction(t, settled, "build")
	if settledBuild.Outputs["candidate"] != "abc" {
		t.Fatalf("settled outputs = %+v, want candidate=abc", settledBuild.Outputs)
	}
	if settledBuild.StagedOutputsSet {
		t.Fatal("successful settlement retained the staging presence marker")
	}
	if !settledBuild.Stop.IsConfirmed() {
		t.Fatalf("settled stop = %+v, want confirmed", settledBuild.Stop)
	}

	// The request activates with a resolved immutable subject, no workload and
	// no credential delivery.
	reviewRunning := h.reconcileUntil("review running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "review")
		return ok && action.State == kernel.ActionRunning
	})
	assertSingleActiveAction(t, reviewRunning)
	if h.runtime.prepareCount() != 1 {
		t.Fatalf("Prepare calls before the request = %d, want only the build workload", h.runtime.prepareCount())
	}
	if h.runtime.startCount() != 1 {
		t.Fatalf("Start calls before the request = %d, want only the build workload", h.runtime.startCount())
	}
	if h.runtime.credentialCount() != 0 {
		t.Fatalf("credential deliveries = %d, want none for a request", h.runtime.credentialCount())
	}
	if !h.fixture.Store.CredentialInvalidated(h.executionID) {
		t.Fatal("request activation did not record the credential invalidation intent")
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("request wait did not retain the workspace")
	}
	reviewAction := mustAction(t, reviewRunning, "review")
	if reviewAction.Request == nil || reviewAction.Request.Subject == nil {
		t.Fatalf("request activation exposed no resolved subject: %+v", reviewAction.Request)
	}
	fields := reviewAction.Request.Subject.Fields()
	if fields["resource"].Literal != "example/repo" {
		t.Fatalf("resolved input subject = %+v, want example/repo", fields["resource"])
	}
	if fields["candidate"].Literal != "abc" {
		t.Fatalf("resolved output subject = %+v, want abc", fields["candidate"])
	}
	if reviewAction.Deadline.IsZero() {
		t.Fatal("request activation did not persist a deadline")
	}
	deadline := reviewAction.Deadline

	// A controller restart keeps the persisted deadline, subject and workspace.
	h.restart()
	restarted := h.load()
	restartedReview := mustAction(t, restarted, "review")
	if !restartedReview.Deadline.Equal(deadline) {
		t.Fatalf("restarted deadline = %s, want %s", restartedReview.Deadline, deadline)
	}
	if restartedReview.Request == nil || restartedReview.Request.Subject == nil {
		t.Fatal("restart lost the resolved request subject")
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("restart lost workspace retention")
	}

	// An authorized pass settles the request and promotes its response outputs.
	answer := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"decision": "approved"},
		Actor:    id.PrincipalID("principal-1"),
		At:       testNow.Add(30 * time.Minute),
	}
	if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer); err != nil {
		t.Fatalf("SettleRequest(pass) error = %v", err)
	}
	passed := h.load()
	passedReview := mustAction(t, passed, "review")
	if passedReview.State != kernel.ActionSucceeded {
		t.Fatalf("review state after pass = %q, want succeeded", passedReview.State)
	}
	if passedReview.Outputs["decision"] != "approved" {
		t.Fatalf("review settled outputs = %+v, want decision=approved", passedReview.Outputs)
	}

	// The next container shares the retained workspace and sees only earlier
	// succeeded outputs.
	testRunning := h.reconcileUntil("test running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "test")
		return ok && action.State == kernel.ActionRunning
	})
	assertSingleActiveAction(t, testRunning)
	lastPrepare := h.runtime.lastPrepare()
	if lastPrepare.WorkspaceID != h.workspaceID {
		t.Fatalf("next container workspace = %q, want %q", lastPrepare.WorkspaceID, h.workspaceID)
	}
	if lastPrepare.Outputs["build"]["candidate"] != "abc" {
		t.Fatalf("next container build outputs = %+v, want candidate=abc", lastPrepare.Outputs["build"])
	}
	if lastPrepare.Outputs["review"]["decision"] != "approved" {
		t.Fatalf("next container review outputs = %+v, want decision=approved", lastPrepare.Outputs["review"])
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("next container did not reuse the retained workspace")
	}
	testAction := mustAction(t, testRunning, "test")
	if testAction.Container == nil || testAction.Container.WorkloadID == "" {
		t.Fatalf("test action has no stable workload identity: %+v", testAction.Container)
	}
	testWorkload := testAction.Container.WorkloadID
	if testWorkload == buildWorkload {
		t.Fatal("a fresh command attempt reused the previous workload identity")
	}

	// Terminal success after the final confirmed stop, followed by cleanup of
	// both workloads and release of the workspace.
	h.confirmRuntimeStop(identityFor(h.executionID, testAction))
	h.fixture.Runtime.QueueObservation(testWorkload, kernel.ExitedObservation(0, kernel.ConfirmedStop("test exited")))
	done := h.reconcileUntil("execution succeeded", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionSucceeded
	})
	assertNoPendingActions(t, done)
	if !h.fixture.Store.CredentialInvalidated(h.executionID) {
		t.Fatal("terminal success did not record credential invalidation")
	}
	final := h.reconcileUntil("terminal cleanup", 8, func(candidate kernel.Execution) bool {
		if candidate.State != kernel.ExecutionSucceeded {
			return false
		}
		if candidate.Cleanup.State != kernel.CleanupComplete && candidate.Cleanup.State != kernel.CleanupNone {
			return false
		}
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			return false
		}
		removed := h.runtime.removedWorkloads()
		return containsWorkload(removed, buildWorkload) && containsWorkload(removed, testWorkload)
	})
	if final.State != kernel.ExecutionSucceeded {
		t.Fatalf("final execution state = %q, want succeeded", final.State)
	}

	// Terminal history is immutable across another reconcile.
	stable := h.reconcile()
	if stable.State != kernel.ExecutionSucceeded {
		t.Fatalf("terminal execution reopened to %q", stable.State)
	}
}

// TestOnlyConfirmedSuccessAdvancesAndPromotesOutputs pins the replacement
// barrier: an exit without a confirmed stop records evidence but neither
// settles the action nor lets a successor take over the workspace.
func TestOnlyConfirmedSuccessAdvancesAndPromotesOutputs(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	test := containerAction("act-test", "test", 1, time.Hour)
	h := newHarness(t, newExecution("exec-barrier", "ws-barrier", build, test))

	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	buildAction := mustAction(t, running, "build")
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
		t.Fatalf("SubmitOutputs(build) error = %v", err)
	}

	// Exit zero with only a pending stop barrier is not success.
	h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(0, kernel.PendingStop("stop acknowledged")))
	pending := h.reconcileUntil("pending stop evidence", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.Stop.State == kernel.StopStateUnconfirmed
	})
	pendingBuild := mustAction(t, pending, "build")
	if pendingBuild.State != kernel.ActionRunning {
		t.Fatalf("build state with pending stop = %q, want running", pendingBuild.State)
	}
	if pendingBuild.Outputs["candidate"] != "" {
		t.Fatalf("pending stop promoted outputs: %+v", pendingBuild.Outputs)
	}
	if !pendingBuild.StagedOutputsSet {
		t.Fatal("pending stop cleared the accepted staging")
	}
	pendingTest := mustAction(t, pending, "test")
	if pendingTest.State != kernel.ActionPending {
		t.Fatalf("successor state with pending stop = %q, want pending", pendingTest.State)
	}
	if h.runtime.prepareCount() != 1 || h.runtime.startCount() != 1 {
		t.Fatalf("pending stop dispatched the successor: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("pending stop released the workspace")
	}

	// Affirmative stop evidence completes the settlement and only then may the
	// successor activate.
	h.confirmRuntimeStop(identityFor(h.executionID, pendingBuild))
	h.fixture.Runtime.QueueObservation(pendingBuild.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
	settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionSucceeded
	})
	settledBuild := mustAction(t, settled, "build")
	if settledBuild.Outputs["candidate"] != "abc" {
		t.Fatalf("confirmed success outputs = %+v, want candidate=abc", settledBuild.Outputs)
	}
	advanced := h.reconcileUntil("successor running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "test")
		return ok && action.State == kernel.ActionRunning
	})
	assertSingleActiveAction(t, advanced)
}

// TestNonzeroExitFailsExecutionAndCancelsPendingActions pins failure settlement:
// the action fails, staging is discarded, every pending successor is cancelled
// rather than skipped and no new workload starts.
func TestNonzeroExitFailsExecutionAndCancelsPendingActions(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	review := requestAction("act-review", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	h := newHarness(t, newExecution("exec-failure", "ws-failure", build, review))

	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	buildAction := mustAction(t, running, "build")
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
		t.Fatalf("SubmitOutputs(build) error = %v", err)
	}
	h.confirmRuntimeStop(identityFor(h.executionID, buildAction))
	h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(1, kernel.ConfirmedStop("build failed")))

	failed := h.reconcileUntil("execution failed", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	failedBuild := mustAction(t, failed, "build")
	if failedBuild.State != kernel.ActionFailed {
		t.Fatalf("build state = %q, want failed", failedBuild.State)
	}
	if failedBuild.StagedOutputsSet || failedBuild.Outputs["candidate"] != "" {
		t.Fatalf("failure retained outputs: settled=%+v staged_set=%v", failedBuild.Outputs, failedBuild.StagedOutputsSet)
	}
	failedReview := mustAction(t, failed, "review")
	if failedReview.State != kernel.ActionCancelled {
		t.Fatalf("pending review state = %q, want cancelled", failedReview.State)
	}
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailureRuntime {
		t.Fatalf("failure = %+v, want runtime", failed.Failure)
	}
	if !h.fixture.Store.CredentialInvalidated(h.executionID) {
		t.Fatal("failure did not revoke execution authority")
	}
	if h.runtime.prepareCount() != 1 || h.runtime.startCount() != 1 {
		t.Fatalf("failure started pending work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	assertNoPendingActions(t, failed)
}

// TestRequestPassFailRetentionAndIdenticalRetry pins authorized request
// settlement, retained failed answers and idempotent retries.
func TestRequestPassFailRetentionAndIdenticalRetry(t *testing.T) {
	t.Run("pass promotes and retries converge", func(t *testing.T) {
		review := requestAction("act-review", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
		h := newHarness(t, newExecution("exec-pass", "ws-pass", review))
		h.reconcileUntil("review running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "review")
			return ok && action.State == kernel.ActionRunning
		})

		answer := kernel.Answer{
			Decision: kernel.DecisionPass,
			Outputs:  kernel.Outputs{"decision": "approved"},
			Actor:    id.PrincipalID("principal-1"),
			At:       testNow.Add(time.Minute),
		}
		if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer); err != nil {
			t.Fatalf("SettleRequest(pass) error = %v", err)
		}
		settled := h.load()
		action := mustAction(t, settled, "review")
		if action.State != kernel.ActionSucceeded {
			t.Fatalf("review state = %q, want succeeded", action.State)
		}
		if action.Request == nil || action.Request.Answer == nil {
			t.Fatal("passing answer was not retained")
		}
		if !action.Request.Answer.DownstreamEligible() {
			t.Fatal("passing answer is not downstream eligible")
		}
		if action.Outputs["decision"] != "approved" {
			t.Fatalf("settled outputs = %+v, want decision=approved", action.Outputs)
		}

		// An identical retry converges without a new commit.
		commits := h.fixture.Store.CommitCount()
		retry := answer
		retry.At = testNow.Add(2 * time.Minute)
		if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", retry); err != nil {
			t.Fatalf("identical SettleRequest retry error = %v", err)
		}
		if h.fixture.Store.CommitCount() != commits {
			t.Fatalf("identical retry committed again: %d -> %d", commits, h.fixture.Store.CommitCount())
		}

		// A replacement decision never rewrites settled history.
		conflict := answer
		conflict.Decision = kernel.DecisionFail
		mustClass(t, h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", conflict), fault.ChangedIdempotencyContent)
		after := mustAction(t, h.load(), "review")
		if after.State != kernel.ActionSucceeded || after.Request.Answer.Decision != kernel.DecisionPass {
			t.Fatalf("conflicting retry rewrote the answer: state=%q answer=%+v", after.State, after.Request.Answer)
		}
	})

	t.Run("fail retains the answer but discards downstream eligibility", func(t *testing.T) {
		review := requestAction("act-review", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
		test := containerAction("act-test", "test", 1, time.Hour)
		h := newHarness(t, newExecution("exec-reject", "ws-reject", review, test))
		h.reconcileUntil("review running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "review")
			return ok && action.State == kernel.ActionRunning
		})

		answer := kernel.Answer{
			Decision: kernel.DecisionFail,
			Outputs:  kernel.Outputs{"decision": "rejected"},
			Actor:    id.PrincipalID("principal-1"),
			At:       testNow.Add(time.Minute),
		}
		if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer); err != nil {
			t.Fatalf("SettleRequest(fail) error = %v", err)
		}
		rejected := h.load()
		if rejected.State != kernel.ExecutionFailed {
			t.Fatalf("execution state = %q, want failed", rejected.State)
		}
		action := mustAction(t, rejected, "review")
		if action.State != kernel.ActionFailed {
			t.Fatalf("review state = %q, want failed", action.State)
		}
		if action.Request == nil || action.Request.Answer == nil {
			t.Fatal("failed answer was not retained for inspection")
		}
		if action.Request.Answer.DownstreamEligible() {
			t.Fatal("failed answer became downstream eligible")
		}
		if action.Outputs["decision"] != "" {
			t.Fatalf("failed answer leaked into downstream outputs: %+v", action.Outputs)
		}
		if successor := mustAction(t, rejected, "test"); successor.State != kernel.ActionCancelled {
			t.Fatalf("pending successor state = %q, want cancelled", successor.State)
		}
		if rejected.Failure == nil || rejected.Failure.Kind != kernel.FailureRuntime {
			t.Fatalf("failure = %+v, want runtime", rejected.Failure)
		}

		// The identical failing retry converges.
		if err := h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer); err != nil {
			t.Fatalf("identical failing retry error = %v", err)
		}
	})
}

// TestRequestActivationFailsClosedOnMissingSubject pins that a request is never
// exposed answerable with an incomplete immutable subject.
func TestRequestActivationFailsClosedOnMissingSubject(t *testing.T) {
	review := requestAction("act-review", "review", 0, time.Hour, definition.NewSubject(map[string]definition.SubjectValue{
		"repository": inputReference("missing"),
	}), "decision")
	execution := newExecutionWithInputs("exec-missing-subject", "ws-missing-subject", map[string]string{}, review)
	h := newHarness(t, execution)
	h.retainWorkspace()

	failed := h.reconcileUntil("preparation failure", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	action := mustAction(t, failed, "review")
	if action.State != kernel.ActionFailed {
		t.Fatalf("review state = %q, want failed", action.State)
	}
	if action.Request == nil || action.Request.Subject != nil {
		t.Fatalf("failed activation exposed a request subject: %+v", action.Request)
	}
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailurePreparation {
		t.Fatalf("failure = %+v, want preparation", failed.Failure)
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("failed subject resolution dispatched work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	if !h.fixture.Store.CredentialInvalidated(h.executionID) {
		t.Fatal("failed activation did not revoke execution authority")
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("terminal preparation failure did not release the workspace")
	}
}

// TestQueuedDelayAndExactDeadlineBoundary pins that queue time consumes no
// timeout and that equality with the persisted deadline expires.
func TestQueuedDelayAndExactDeadlineBoundary(t *testing.T) {
	t.Run("queued delay is not charged", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour)
		h := newHarness(t, newExecution("exec-queued-delay", "ws-queued-delay", build))
		h.fixture.Clock.Advance(10 * time.Minute)

		running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionRunning
		})
		action := mustAction(t, running, "build")
		wantDeadline := testNow.Add(10 * time.Minute).Add(time.Hour)
		if !action.Deadline.Equal(wantDeadline) {
			t.Fatalf("deadline = %s, want activation time + timeout %s", action.Deadline, wantDeadline)
		}
	})

	t.Run("one nanosecond before the deadline is still timely", func(t *testing.T) {
		build := runningContainerExecution("exec-before", "ws-before", testNow.Add(time.Hour))
		h := newHarness(t, build)
		h.retainWorkspace()
		h.prepareAndStart(identityFor(build.ID, build.Actions[0]), build.Actions[0].Container.Definition)
		h.fixture.Clock.Set(testNow.Add(time.Hour - time.Nanosecond))

		stillRunning := h.reconcile()
		if stillRunning.State != kernel.ExecutionRunning {
			t.Fatalf("execution expired before its deadline: %q", stillRunning.State)
		}
		h.fixture.Clock.Set(testNow.Add(time.Hour))
		expired := h.reconcileUntil("timeout", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionFailed
		})
		if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
			t.Fatalf("failure = %+v, want timeout", expired.Failure)
		}
	})

	t.Run("container expiry stops the workload", func(t *testing.T) {
		build := runningContainerExecution("exec-container-timeout", "ws-container-timeout", testNow.Add(time.Hour))
		h := newHarness(t, build)
		h.retainWorkspace()
		h.prepareAndStart(identityFor(build.ID, build.Actions[0]), build.Actions[0].Container.Definition)
		h.fixture.Clock.Set(testNow.Add(time.Hour))

		expired := h.reconcileUntil("container timeout", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionFailed
		})
		action := mustAction(t, expired, "build")
		if action.State != kernel.ActionFailed {
			t.Fatalf("build state = %q, want failed", action.State)
		}
		if !h.fixture.Store.CredentialInvalidated(h.executionID) {
			t.Fatal("timeout did not revoke execution authority")
		}
		if expired.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending", expired.Cleanup.State)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("timeout released the workspace before a confirmed stop")
		}
		if h.runtime.removeCount() != 0 {
			t.Fatal("timeout removed the workload before a confirmed stop")
		}
	})

	t.Run("request expiry carries no workload stop", func(t *testing.T) {
		review := runningRequestExecution("exec-request-timeout", "ws-request-timeout", testNow.Add(time.Hour))
		h := newHarness(t, review)
		h.retainWorkspace()
		h.fixture.Clock.Set(testNow.Add(time.Hour))

		expired := h.reconcileUntil("request timeout", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionFailed
		})
		if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
			t.Fatalf("failure = %+v, want timeout", expired.Failure)
		}
		if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 || h.runtime.stopCount() != 0 {
			t.Fatalf("request expiry used a workload: prepare=%d start=%d stop=%d",
				h.runtime.prepareCount(), h.runtime.startCount(), h.runtime.stopCount())
		}
		if !h.fixture.Store.CredentialInvalidated(h.executionID) {
			t.Fatal("request expiry did not revoke execution authority")
		}
	})
}

// TestRestartKeepsPersistedDeadline pins that a restart never recalculates the
// deadline from the action timeout.
func TestRestartKeepsPersistedDeadline(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-deadline-restart", "ws-deadline-restart", build))
	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	original := mustAction(t, running, "build")
	deadline := original.Deadline
	workloadID := original.Container.WorkloadID
	if deadline.IsZero() || workloadID == "" {
		t.Fatalf("activation did not persist deadline/identity: %+v", original)
	}

	h.fixture.Clock.Advance(30 * time.Minute)
	h.restart()
	restarted := mustAction(t, h.load(), "build")
	if !restarted.Deadline.Equal(deadline) {
		t.Fatalf("restarted deadline = %s, want %s", restarted.Deadline, deadline)
	}
	if restarted.Container.WorkloadID != workloadID {
		t.Fatalf("restarted workload id = %q, want %q", restarted.Container.WorkloadID, workloadID)
	}

	h.fixture.Clock.Set(deadline)
	expired := h.reconcileUntil("timeout after restart", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("failure = %+v, want timeout", expired.Failure)
	}

	t.Run("persisted deadline wins over a changed timeout", func(t *testing.T) {
		seeded := runningContainerExecution("exec-changed-timeout", "ws-changed-timeout", testNow.Add(10*time.Minute))
		seeded.Actions[0].Timeout = 24 * time.Hour
		changed := newHarness(t, seeded)
		changed.retainWorkspace()
		changed.prepareAndStart(identityFor(seeded.ID, seeded.Actions[0]), seeded.Actions[0].Container.Definition)
		changed.fixture.Clock.Set(testNow.Add(10 * time.Minute))
		expired := changed.reconcileUntil("timeout at persisted deadline", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionFailed
		})
		if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
			t.Fatalf("failure = %+v, want timeout at the persisted deadline", expired.Failure)
		}
	})
}

// TestLateAnswerAfterExpiryIsRefused pins strict-before timeliness and that a
// late answer never settles a request.
func TestLateAnswerAfterExpiryIsRefused(t *testing.T) {
	review := runningRequestExecution("exec-late", "ws-late", testNow.Add(time.Hour))
	h := newHarness(t, review)
	h.retainWorkspace()
	h.fixture.Clock.Set(testNow.Add(time.Hour))

	answer := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"decision": "approved"},
		Actor:    id.PrincipalID("principal-1"),
		At:       testNow.Add(59 * time.Minute),
	}
	mustClass(t, h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer), fault.DeadlineExpired)
	after := mustAction(t, h.load(), "review")
	if after.State != kernel.ActionRunning {
		t.Fatalf("late answer changed the action state to %q", after.State)
	}
	if after.Request != nil && after.Request.Answer != nil {
		t.Fatalf("late answer was recorded: %+v", after.Request.Answer)
	}

	expired := h.reconcileUntil("expiry", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("failure = %+v, want timeout", expired.Failure)
	}
	mustClass(t, h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer), fault.ChangedIdempotencyContent)
}

// TestOutputSubmissionBoundsAndImmutableStaging pins the output contract at the
// engine boundary: declared keys, byte bounds, UTF-8, immutable staging and
// current-action-only submission.
func TestOutputSubmissionBoundsAndImmutableStaging(t *testing.T) {
	keys := []id.OutputKey{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"}
	build := containerAction("act-build", "build", 0, time.Hour, keys...)
	test := containerAction("act-test", "test", 1, time.Hour)
	h := newHarness(t, newExecution("exec-output-bounds", "ws-output-bounds", build, test))
	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	buildAction := mustAction(t, running, "build")

	// Undeclared key.
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"undeclared": "value"}), fault.InvalidInput)
	// Byte overflow.
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"k1": strings.Repeat("a", kernel.MaxOutputValueBytes+1)}), fault.InvalidInput)
	// Invalid UTF-8.
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"k1": string([]byte{0xff})}), fault.InvalidInput)
	// More than eight values.
	nine := kernel.Outputs{"k1": "1", "k2": "2", "k3": "3", "k4": "4", "k5": "5", "k6": "6", "k7": "7", "k8": "8", "extra": "9"}
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", nine), fault.InvalidInput)
	// Not the current action.
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-test", kernel.Outputs{}), fault.InvalidInput)

	// The first accepted submission is immutable staging.
	valid := kernel.Outputs{"k1": "one", "k2": "two"}
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", valid); err != nil {
		t.Fatalf("SubmitOutputs(valid) error = %v", err)
	}
	staged := mustAction(t, h.load(), "build")
	if !staged.StagedOutputsSet || staged.StagedOutputs["k1"] != "one" || staged.StagedOutputs["k2"] != "two" {
		t.Fatalf("staged outputs = %+v set=%v, want k1=one k2=two", staged.StagedOutputs, staged.StagedOutputsSet)
	}
	commits := h.fixture.Store.CommitCount()
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", valid); err != nil {
		t.Fatalf("identical output retry error = %v", err)
	}
	if h.fixture.Store.CommitCount() != commits {
		t.Fatalf("identical output retry committed again: %d -> %d", commits, h.fixture.Store.CommitCount())
	}
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"k1": "changed"}), fault.ChangedIdempotencyContent)
	afterConflict := mustAction(t, h.load(), "build")
	if afterConflict.StagedOutputs["k1"] != "one" {
		t.Fatalf("conflicting submission changed staging: %+v", afterConflict.StagedOutputs)
	}

	// Once the action is terminal, a new submission is refused.
	h.confirmRuntimeStop(identityFor(h.executionID, buildAction))
	h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
	h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionSucceeded
	})
	mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"k1": "late"}), fault.ChangedIdempotencyContent)
}

// TestOutputSettlementOrderDeterminesPromotion pins the deterministic staging
// versus settlement orders: only a confirmed success promotes accepted staging.
func TestOutputSettlementOrderDeterminesPromotion(t *testing.T) {
	t.Run("stage before confirmed success promotes", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour, "candidate")
		h := newHarness(t, newExecution("exec-stage-first", "ws-stage-first", build))
		running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionRunning
		})
		buildAction := mustAction(t, running, "build")
		if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
			t.Fatalf("SubmitOutputs() error = %v", err)
		}
		h.confirmRuntimeStop(identityFor(h.executionID, buildAction))
		h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
		settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionSucceeded
		})
		action := mustAction(t, settled, "build")
		if action.Outputs["candidate"] != "abc" || action.StagedOutputsSet {
			t.Fatalf("settled outputs = %+v set=%v, want promoted candidate=abc", action.Outputs, action.StagedOutputsSet)
		}
	})

	t.Run("confirmed success before submission refuses late staging", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour, "candidate")
		h := newHarness(t, newExecution("exec-exit-first", "ws-exit-first", build))
		running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionRunning
		})
		buildAction := mustAction(t, running, "build")
		h.confirmRuntimeStop(identityFor(h.executionID, buildAction))
		h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
		settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionSucceeded
		})
		action := mustAction(t, settled, "build")
		if action.Outputs["candidate"] != "" {
			t.Fatalf("success without staging invented outputs: %+v", action.Outputs)
		}
		mustClass(t, h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "late"}), fault.ChangedIdempotencyContent)
	})

	t.Run("failure discards accepted staging", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour, "candidate")
		h := newHarness(t, newExecution("exec-discard", "ws-discard", build))
		running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionRunning
		})
		buildAction := mustAction(t, running, "build")
		if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
			t.Fatalf("SubmitOutputs() error = %v", err)
		}
		h.confirmRuntimeStop(identityFor(h.executionID, buildAction))
		h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(2, kernel.ConfirmedStop("build failed")))
		failed := h.reconcileUntil("build failed", 8, func(candidate kernel.Execution) bool {
			action, ok := actionByName(candidate, "build")
			return ok && action.State == kernel.ActionFailed
		})
		action := mustAction(t, failed, "build")
		if action.StagedOutputsSet || action.Outputs["candidate"] != "" {
			t.Fatalf("failure retained outputs: settled=%+v staged_set=%v", action.Outputs, action.StagedOutputsSet)
		}
	})
}

// TestConcurrentAnswersPreserveOneWinner pins that competing authorized facts
// serialize on the same commit boundary and never replace the winner.
func TestConcurrentAnswersPreserveOneWinner(t *testing.T) {
	review := requestAction("act-review", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	h := newHarness(t, newExecution("exec-competing", "ws-competing", review))
	h.reconcileUntil("review running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "review")
		return ok && action.State == kernel.ActionRunning
	})

	pass := kernel.Answer{
		Decision: kernel.DecisionPass,
		Outputs:  kernel.Outputs{"decision": "approved"},
		Actor:    id.PrincipalID("principal-1"),
		At:       testNow.Add(time.Minute),
	}
	fail := kernel.Answer{
		Decision: kernel.DecisionFail,
		Outputs:  kernel.Outputs{"decision": "rejected"},
		Actor:    id.PrincipalID("principal-1"),
		At:       testNow.Add(time.Minute),
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for index, answer := range []kernel.Answer{pass, fail} {
		wg.Add(1)
		go func(index int, answer kernel.Answer) {
			defer wg.Done()
			<-start
			results[index] = h.engine.SettleRequest(h.t.Context(), h.executionID, "act-review", answer)
		}(index, answer)
	}
	close(start)
	wg.Wait()

	winners := 0
	for index, err := range results {
		if err == nil {
			winners++
			continue
		}
		class, ok := fault.ClassOf(err)
		if !ok || (class != fault.VersionConflict && class != fault.ChangedIdempotencyContent) {
			t.Fatalf("loser %d error = %v (class %q, %v), want version_conflict or changed_idempotency_content", index, err, class, ok)
		}
	}
	if winners != 1 {
		t.Fatalf("settlement winners = %d, want exactly one: %v", winners, results)
	}

	final := h.load()
	action := mustAction(t, final, "review")
	if action.Request == nil || action.Request.Answer == nil {
		t.Fatal("competing settlements recorded no winner")
	}
	switch action.Request.Answer.Decision {
	case kernel.DecisionPass:
		if action.State != kernel.ActionSucceeded || final.State != kernel.ExecutionSucceeded {
			t.Fatalf("passing winner state = %q/%q, want succeeded/succeeded", action.State, final.State)
		}
	case kernel.DecisionFail:
		if action.State != kernel.ActionFailed || final.State != kernel.ExecutionFailed {
			t.Fatalf("failing winner state = %q/%q, want failed/failed", action.State, final.State)
		}
	default:
		t.Fatalf("recorded decision = %q", action.Request.Answer.Decision)
	}
}

// ---------------------------------------------------------------------------
// Small constructors used by deadline tests
// ---------------------------------------------------------------------------

func runningContainerExecution(executionID, workspaceID string, deadline time.Time) kernel.Execution {
	build := containerAction("act-build", "build", 0, time.Hour)
	build.State = kernel.ActionRunning
	build.Version = 2
	build.ActivatedAt = deadline.Add(-time.Hour)
	build.Deadline = deadline
	build.Container.WorkloadID = id.WorkloadID("wl-" + executionID)
	build.Container.WorkspaceID = id.WorkspaceID(workspaceID)
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-" + executionID,
	}
	execution := newExecution(executionID, workspaceID, build)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = 0
	return execution
}

func runningRequestExecution(executionID, workspaceID string, deadline time.Time) kernel.Execution {
	review := requestAction("act-review", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	review.State = kernel.ActionRunning
	review.Version = 2
	review.ActivatedAt = deadline.Add(-time.Hour)
	review.Deadline = deadline
	subject := literalSubject(map[string]string{"operation": "review"})
	review.Request.Subject = &subject
	execution := newExecution(executionID, workspaceID, review)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = 0
	return execution
}
