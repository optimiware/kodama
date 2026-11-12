//go:build integration

package factorytest

// Terminal decisions on the protected first-factory path.
//
// The protected execution reaches the real role/reviewer wait with candidate C
// on the granted work ref, main still at B and the integrate action unstarted.
// Each subtest then settles that wait differently and asserts the durable
// consequence for the real candidate, the protected ref, the receive journal
// and the report path:
//
//   - a failing decision fails the execution with the declared runtime/
//     answer_rejected vocabulary, retains the reviewer's answer, cancels the
//     integrate successor, leaves main at B with no journal entry or protected
//     verification for C, clears the pre-wait execution credential and leaves
//     the task open with the one execution in its history;
//   - advancing the authoritative clock past the persisted request deadline
//     fails the execution with kind timeout and no answer, cancels the
//     successor, leaves the same no-effect facts, refuses the late answer with
//     the deterministic version_conflict and commits no resolution receipt;
//   - execution.cancel settles the request without an answer, cancels the
//     successor, leaves the same no-effect facts and refuses the stale answer.
//
// Each subtest starts its own fixture because the expiry subtest advances the
// authoritative clock; no subtest can expire another. A human work-ref push is
// the positive control that the served Git edge and current membership still
// work after the terminal decision.

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

const (
	protectedRefRejectPipeline = "protected-terminal-reject"
	protectedRefExpiryPipeline = "protected-terminal-expiry"
	protectedRefCancelPipeline = "protected-terminal-cancel"

	protectedRefRejectWorkRef = "refs/heads/work/protected-terminal-reject"
	protectedRefExpiryWorkRef = "refs/heads/work/protected-terminal-expiry"
	protectedRefCancelWorkRef = "refs/heads/work/protected-terminal-cancel"

	protectedRefRejectTitle = "protected terminal rejection"
	protectedRefExpiryTitle = "protected terminal expiry"
	protectedRefCancelTitle = "protected terminal cancellation"

	// protectedRefExpiryTimeout is the one-minute request minimum the
	// expiry subtest uses; the clock advance is one second past it.
	protectedRefExpiryTimeout = "1m"
)

// TestProtectedFactoryNonPassNeverIntegrates proves no non-pass settlement of
// the real review wait integrates the candidate or moves the protected ref.
func TestProtectedFactoryNonPassNeverIntegrates(t *testing.T) {
	t.Run("rejected decision fails the execution", func(t *testing.T) {
		testProtectedRefRejected(t)
	})
	t.Run("deadline expiry fails the execution", func(t *testing.T) {
		testProtectedRefExpired(t)
	})
	t.Run("cancel settles the request without an answer", func(t *testing.T) {
		testProtectedRefCancelled(t)
	})
}

// protectedRefTerminalExpect is the terminal shape one settlement must
// produce.
type protectedRefTerminalExpect struct {
	state         string
	failureKind   string
	failureReason string
	reviewState   string
	decision      string
}

// testProtectedRefRejected settles the wait with a failing decision.
func testProtectedRefRejected(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: protectedRefRejectPipeline,
		taskTitle:    protectedRefRejectTitle,
		workRef:      protectedRefRejectWorkRef,
	})
	before := protectedEdgeVerificationCount(t, fixture)
	resolveProtectedRefRequest(t, fixture, evidence.reviewerToken, evidence.executionID,
		protectedRefReviewAction, "fail", "resolve-"+protectedRefRejectPipeline, nil)

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	requireProtectedRefTerminal(t, fixture, &evidence, finished, protectedRefTerminalExpect{
		state:         "failed",
		failureKind:   "runtime",
		failureReason: "answer_rejected",
		reviewState:   "failed",
		decision:      "fail",
	})
	requireProtectedRefNoEffect(t, fixture, evidence.executionID, evidence.base, evidence.candidate)
	if after := protectedEdgeVerificationCount(t, fixture); after != before {
		t.Fatalf("a rejected request committed %d new verification rows; want none", after-before)
	}
	requireProtectedEdgeShellRefusal(t, fixture, evidence.testToken, evidence.executionID,
		"after the rejected decision")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.testToken, "after-the-rejection")
	protectedRefHumanWorkControl(t, fixture, "after-reject")
}

// testProtectedRefExpired advances the isolated fixture clock past the persisted
// deadline and proves the timeout settlement and the stale-answer refusal.
func testProtectedRefExpired(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName:   protectedRefExpiryPipeline,
		taskTitle:      protectedRefExpiryTitle,
		workRef:        protectedRefExpiryWorkRef,
		requestTimeout: protectedRefExpiryTimeout,
	})
	if evidence.review.TimeoutSeconds != int64(time.Minute/time.Second) {
		t.Fatalf("review timeout_seconds = %d; want the declared 60", evidence.review.TimeoutSeconds)
	}
	if evidence.review.Deadline == nil {
		t.Fatalf("the waiting review carries no persisted deadline")
	}
	before := protectedEdgeVerificationCount(t, fixture)

	fixture.Clock.Advance(61 * time.Second)

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	requireProtectedRefTerminal(t, fixture, &evidence, finished, protectedRefTerminalExpect{
		state:       "failed",
		failureKind: "timeout",
		reviewState: "failed",
	})
	requireProtectedRefNoEffect(t, fixture, evidence.executionID, evidence.base, evidence.candidate)
	if after := protectedEdgeVerificationCount(t, fixture); after != before {
		t.Fatalf("an expired request committed %d new verification rows; want none", after-before)
	}

	// A late answer quotes the versions observed while the request waited. The
	// controller has already expired the action, so the refusal is the
	// deterministic version_conflict and nothing settles.
	_, publicError := fixture.REST(evidence.reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           evidence.executionID,
		ActionID:              evidence.review.ActionID,
		ExpectedActionVersion: evidence.review.Version,
		Decision:              "pass",
	}, &evidence.pendingVersion, "late-resolve-"+protectedRefExpiryPipeline)
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("late resolve after expiry code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeVersionConflict)
	}
	after := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	if after.State != "failed" {
		t.Fatalf("late resolve changed the execution state to %q; want failed", after.State)
	}
	lateReview := actionByName(t, after, protectedRefReviewAction)
	if lateReview.Decision != "" || lateReview.Actor != "" || lateReview.AnsweredAt != nil {
		t.Fatalf("late resolve after expiry recorded an answer: %+v", lateReview)
	}
	requireProtectedRefReceipts(t, fixture, evidence.executionID, evidence.review.ActionID, 0)

	requireProtectedEdgeShellRefusal(t, fixture, evidence.testToken, evidence.executionID,
		"after the request expired")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.testToken, "after-the-expiry")
	protectedRefHumanWorkControl(t, fixture, "after-expiry")
}

// testProtectedRefCancelled cancels the execution during the wait and proves
// the request settles without an answer and the stale answer is refused.
func testProtectedRefCancelled(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: protectedRefCancelPipeline,
		taskTitle:    protectedRefCancelTitle,
		workRef:      protectedRefCancelWorkRef,
	})
	before := protectedEdgeVerificationCount(t, fixture)
	cancelled := cancelProtectedRefExecution(t, fixture, evidence.executionID)
	if cancelled.ExecutionVersion <= evidence.pendingVersion {
		t.Fatalf("cancel version = %d; want an advance past the waiting version %d",
			cancelled.ExecutionVersion, evidence.pendingVersion)
	}

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	requireProtectedRefTerminal(t, fixture, &evidence, finished, protectedRefTerminalExpect{
		state:       "cancelled",
		reviewState: "cancelled",
	})
	requireProtectedRefNoEffect(t, fixture, evidence.executionID, evidence.base, evidence.candidate)
	if after := protectedEdgeVerificationCount(t, fixture); after != before {
		t.Fatalf("a cancelled request committed %d new verification rows; want none", after-before)
	}

	_, publicError := fixture.REST(evidence.reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           evidence.executionID,
		ActionID:              evidence.review.ActionID,
		ExpectedActionVersion: evidence.review.Version,
		Decision:              "pass",
	}, &evidence.pendingVersion, "late-resolve-"+protectedRefCancelPipeline)
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("late resolve after cancel code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeVersionConflict)
	}
	after := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	if after.State != "cancelled" {
		t.Fatalf("late resolve changed the execution state to %q; want cancelled", after.State)
	}
	lateReview := actionByName(t, after, protectedRefReviewAction)
	if lateReview.Decision != "" || lateReview.Actor != "" || lateReview.AnsweredAt != nil {
		t.Fatalf("late resolve after cancel recorded an answer: %+v", lateReview)
	}
	requireProtectedRefReceipts(t, fixture, evidence.executionID, evidence.review.ActionID, 0)

	requireProtectedEdgeShellRefusal(t, fixture, evidence.testToken, evidence.executionID,
		"after the execution was cancelled")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.testToken, "after-the-cancellation")
	protectedRefHumanWorkControl(t, fixture, "after-cancel")
}

// requireProtectedRefTerminal asserts the declared settlement shape: execution
// outcome and failure vocabulary, the review decision/actor/history facts, the
// cancelled successor and the open task that kept the execution in history.
func requireProtectedRefTerminal(t *testing.T, fixture *gitHarness, evidence *protectedRefEvidence, finished api.ExecutionInspectResult, want protectedRefTerminalExpect) {
	t.Helper()
	if finished.ExecutionID != evidence.executionID || finished.TaskID != evidence.taskID {
		t.Fatalf("terminal execution = %q task = %q; want %q/%q",
			finished.ExecutionID, finished.TaskID, evidence.executionID, evidence.taskID)
	}
	if finished.State != want.state {
		t.Fatalf("execution state = %q; want %q\n%s",
			finished.State, want.state, fixture.executionLogs(t, finished))
	}
	switch {
	case want.failureKind == "":
		if finished.Failure != nil {
			t.Fatalf("terminal execution carries failure %+v; want none", finished.Failure)
		}
	default:
		if finished.Failure == nil || finished.Failure.Kind != want.failureKind {
			t.Fatalf("execution failure = %+v; want kind %q", finished.Failure, want.failureKind)
		}
		if want.failureReason != "" && finished.Failure.Reason != want.failureReason {
			t.Fatalf("execution failure reason = %q; want %q", finished.Failure.Reason, want.failureReason)
		}
	}
	if finished.Cleanup.State != "complete" {
		t.Fatalf("execution cleanup = %q; want complete", finished.Cleanup.State)
	}

	review := actionByName(t, finished, protectedRefReviewAction)
	if review.State != want.reviewState {
		t.Fatalf("review action state = %q; want %q", review.State, want.reviewState)
	}
	if review.Assignment != "role/reviewer" {
		t.Fatalf("review assignment = %q; want role/reviewer", review.Assignment)
	}
	if review.Decision != want.decision {
		t.Fatalf("review decision = %q; want %q", review.Decision, want.decision)
	}
	if want.decision == "" {
		if review.Actor != "" || review.AnsweredAt != nil {
			t.Fatalf("unanswered review action carries actor/answer: %+v", review)
		}
	} else {
		if review.Actor != evidence.reviewerPrincipal || review.AnsweredAt == nil {
			t.Fatalf("review answer actor/time = %q/%v; want the reviewer and a time",
				review.Actor, review.AnsweredAt)
		}
		requireProtectedGoldenSubject(t, "terminal review action", review.Subject, protectedGoldenEvidence{
			fixture:   fixture,
			base:      evidence.base,
			candidate: evidence.candidate,
		})
	}
	if successor := actionByName(t, finished, protectedRefIntegrateAction); successor.State != "cancelled" {
		t.Fatalf("integrate successor state = %q; want cancelled", successor.State)
	}

	task := readProtectedGoldenTask(t, fixture, evidence.engineerToken, evidence.taskID)
	if task.State != "open" {
		t.Fatalf("task state = %q after the terminal decision; want open", task.State)
	}
	if task.CurrentExecution != evidence.executionID {
		t.Fatalf("task current execution = %q; want %q", task.CurrentExecution, evidence.executionID)
	}
	if len(task.ExecutionHistory) != 1 || task.ExecutionHistory[0] != evidence.executionID {
		t.Fatalf("task execution history = %v; want exactly [%s]",
			task.ExecutionHistory, evidence.executionID)
	}

	// The decision/history facts must be durable, not a response-body echo.
	persisted := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	persistedReview := actionByName(t, persisted, protectedRefReviewAction)
	if persistedReview.State != want.reviewState || persistedReview.Decision != want.decision {
		t.Fatalf("persisted review state/decision = %q/%q; want %q/%q",
			persistedReview.State, persistedReview.Decision, want.reviewState, want.decision)
	}
}
