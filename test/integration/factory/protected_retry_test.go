//go:build integration

package factorytest

// Failed-execution retry and cross-execution approval refusal on the protected
// first-factory path.
//
// TestProtectedFactoryFailedExecutionRetry drives one task through two real
// executions. The first execution implements and tests a candidate, receives a
// genuine human role/reviewer pass, and then fails in its integrate action
// before moving the protected ref. The operator discovers the failed task in
// its inbox and starts a fresh attempt. The retry can present the first
// execution's exact candidate to the managed hook, but that pass belongs to the
// earlier execution and is refused; the retry then records its own fresh pass
// and integrates a different candidate produced by its own workspace.
//
// TestProtectedFactoryForeignAndPriorExecutionApprovalRefused pins the two
// execution-scoping cases directly. A foreign execution holds a genuine pass
// for the exact protected effect the victim tries; the victim's push is refused.
// An earlier attempt of the same task holds a genuine pass for the exact
// candidate the retry presents; the retry's push is refused. In both cases the
// refusal is the managed hook's durable refused git.ref.update fact, the
// protected ref does not move, and the live credential still writes a granted
// work ref.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestProtectedFactoryFailedExecutionRetry proves a protected pipeline that
// fails after a genuine pass is retried as a fresh execution, the earlier
// approval is never reused, and the retry integrates its own fresh candidate
// only after its own human pass.
func TestProtectedFactoryFailedExecutionRetry(t *testing.T) {
	fixture := startGitHarness(t)
	reviewerToken, reviewerPrincipal := grantProtectedReviewer(t, fixture, protectedGoldenReviewerSubject)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	applied := fixture.applyPipeline(protectedRetryPipelineName,
		protectedRetryPipeline(t, fixture, base))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline", applied)
	}
	taskID, lifecycle, _ := protectedRetryCreateTask(t, fixture, applied.PipelineID)

	// First attempt: implement, test, a genuine pass, then a real failure in
	// the integrate action before the protected push.
	first := protectedRetryRunAttempt(t, fixture, reviewerToken, taskID,
		fixture.engineerToken, "start-protected-retry-first", &lifecycle, base,
		protectedRetryAttemptOptions{retainPrior: true, failAfterPass: true}, "")
	if first.finished.State != "failed" {
		t.Fatalf("first execution state = %q; want failed after the genuine pass\n%s",
			first.finished.State, first.logs)
	}
	if first.finished.Failure == nil || first.finished.Failure.Kind != "runtime" ||
		first.finished.Failure.Reason != "workload_exited" {
		t.Fatalf("first execution failure = %+v; want runtime/workload_exited", first.finished.Failure)
	}
	if first.integrateAction.State != "failed" {
		t.Fatalf("first integrate action state = %q; want failed", first.integrateAction.State)
	}
	assertProtectedRetryApproval(t, fixture, "first approval", first.reviewAction,
		reviewerPrincipal, base, first.candidate)

	// The pass is durable, but no protected push happened: main is still the
	// base and there is no accepted or reported entry for the candidate.
	if count := protectedRetryJournalCount(t, fixture); count != 0 {
		t.Fatalf("reported protected journal entries after the injected failure = %d; want 0", count)
	}
	assertProtectedRetryProtectedRefUnchanged(t, fixture, "first failed attempt", base, 0, first.candidate)
	if got := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRetryWorkRef); got != first.candidate {
		t.Fatalf("first work ref = %q; want the first candidate %q", got, first.candidate)
	}
	if got := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRetryPriorRef); got != first.candidate {
		t.Fatalf("retained prior ref = %q; want the first candidate %q", got, first.candidate)
	}

	firstTask := waitProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID,
		"task open with the failed first execution", func(read api.TaskReadResult) bool {
			return read.State == "open" && read.CurrentExecution == first.executionID
		})
	if len(firstTask.ExecutionHistory) != 1 || firstTask.ExecutionHistory[0] != first.executionID {
		t.Fatalf("first task history = %v; want exactly [%s]", firstTask.ExecutionHistory, first.executionID)
	}

	// The operator discovers the failed task through its own inbox item and
	// retries at exactly that lifecycle version.
	failedItem := protectedRetryFailedTask(t, fixture, taskID)
	failedTask := readProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID)
	if failedItem.Version != failedTask.LifecycleVersion {
		t.Fatalf("failed_task Version = %d; want the task lifecycle version %d",
			failedItem.Version, failedTask.LifecycleVersion)
	}
	if failedItem.ExecutionID != "" || failedItem.ActionID != "" || failedItem.Assignment != "" {
		t.Fatalf("failed_task item carries execution/action/assignment identity: %+v", failedItem)
	}

	retry := protectedRetryRunAttempt(t, fixture, reviewerToken, taskID,
		fixture.OperatorToken, "start-protected-retry-operator", &failedItem.Version, base,
		protectedRetryAttemptOptions{probePrior: true, finish: true}, first.candidate)
	if retry.executionID == first.executionID {
		t.Fatalf("operator retry reused the failed execution %q", first.executionID)
	}
	if retry.candidate == "" || retry.candidate == first.candidate {
		t.Fatalf("retry candidate = %q; want the retry's own candidate distinct from %q",
			retry.candidate, first.candidate)
	}

	// The retry can present the first execution's exact candidate, and the
	// first approval is durable for exactly that effect, yet the managed hook
	// refuses the retry: approval evidence is scoped to its own execution.
	assertProtectedRetryRefusedEffect(t, fixture, "prior-execution approval",
		retry.probeRefused, retry.probeRefusedApproval, retry.executionID,
		retry.testAction.ActionID, base, first.candidate)
	if got := retry.probeRefusedApproval.String(); got != "role/reviewer" {
		t.Fatalf("prior-execution refusal requested approval = %q; want role/reviewer", got)
	}

	if retry.finished.State != "succeeded" || retry.finished.Cleanup.State != "complete" {
		t.Fatalf("retry execution state=%q cleanup=%q; want succeeded and complete\n%s",
			retry.finished.State, retry.finished.Cleanup.State, retry.logs)
	}
	assertProtectedRetryApproval(t, fixture, "retry approval", retry.reviewAction,
		reviewerPrincipal, base, retry.candidate)
	if retry.reviewAction.ActionID == first.reviewAction.ActionID {
		t.Fatalf("retry reused the first approval action %q", first.reviewAction.ActionID)
	}
	if retry.integrateAction.State != "succeeded" {
		t.Fatalf("retry integrate action state = %q; want succeeded", retry.integrateAction.State)
	}

	// Exactly the retry's own candidate landed, and its verification names the
	// retry execution and integrate action.
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != retry.candidate {
		t.Fatalf("protected main = %q; want the retry candidate %q", main, retry.candidate)
	}
	if count := protectedRetryJournalCount(t, fixture); count != 1 {
		t.Fatalf("reported protected journal entries after the retry = %d; want exactly 1", count)
	}
	journal := protectedRetryUserlandJournalEntry(t, fixture, gitMainRef, retry.candidate)
	if journal.ExpectedBase != base || journal.Candidate != retry.candidate ||
		journal.State != "reported" {
		t.Fatalf("protected journal = %+v; want a reported %s -> %s entry",
			journal, base, retry.candidate)
	}
	verification := fixture.verificationByID(t, journal.VerificationID)
	if verification.ExecutionID != retry.executionID ||
		verification.ActionID != retry.integrateAction.ActionID ||
		verification.Result != "approved" {
		t.Fatalf("protected verification = %+v; want the retry execution/action approved",
			verification)
	}
	approval, err := fixture.git.RequestedApproval(t.Context(), verification.ID)
	if err != nil {
		t.Fatalf("read the retry verification's stored requested approval: %v", err)
	}
	if got := approval.String(); got != "role/reviewer" {
		t.Fatalf("stored requested approval = %q; want role/reviewer", got)
	}
	event := fixture.waitReportEvent(t, verification.ID)
	if event.CorrelationID != retry.executionID || event.CausationID != verification.ID {
		t.Fatalf("retry verified event correlation/causation = %q/%q; want %q/%q",
			event.CorrelationID, event.CausationID, retry.executionID, verification.ID)
	}

	// The retry did not touch the failed execution or its approval. The
	// execution is terminal and its integrate action is still the same failed
	// action; cleanup-record progress is not an execution change.
	unchanged := waitProtectedGoldenTerminal(t, fixture, first.executionID)
	if unchanged.State != "failed" {
		t.Fatalf("retry changed the failed execution state: before=%q after=%q",
			first.finished.State, unchanged.State)
	}
	if unchanged.Failure == nil || first.finished.Failure == nil ||
		*unchanged.Failure != *first.finished.Failure {
		t.Fatalf("retry changed the failed execution failure: before=%+v after=%+v",
			first.finished.Failure, unchanged.Failure)
	}
	if unchangedAction := actionByName(t, unchanged, "integrate"); unchangedAction.ActionID != first.integrateAction.ActionID ||
		unchangedAction.State != "failed" {
		t.Fatalf("retry changed the first integrate action: before=%+v after=%+v",
			first.integrateAction, unchangedAction)
	}

	finalTask := waitProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID,
		"task done after the retry integrated its own candidate", func(read api.TaskReadResult) bool {
			return read.State == "done" && read.CurrentExecution == retry.executionID
		})
	if len(finalTask.ExecutionHistory) != 2 ||
		finalTask.ExecutionHistory[0] != first.executionID ||
		finalTask.ExecutionHistory[1] != retry.executionID {
		t.Fatalf("retried task history = %v; want [%s %s]",
			finalTask.ExecutionHistory, first.executionID, retry.executionID)
	}
}

// TestProtectedFactoryForeignAndPriorExecutionApprovalRefused proves the
// execution scope of approval evidence at the protected Git boundary. A genuine
// pass for the exact effect from an unrelated execution, and a genuine pass
// from an earlier execution of the same task, are both refused.
func TestProtectedFactoryForeignAndPriorExecutionApprovalRefused(t *testing.T) {
	t.Run("foreign execution", func(t *testing.T) {
		testProtectedRetryForeignApprovalRefused(t)
	})
	t.Run("prior execution", func(t *testing.T) {
		testProtectedRetryPriorApprovalRefused(t)
	})
}

// testProtectedRetryForeignApprovalRefused creates a genuine pass for exactly
// the victim's effect in an unrelated task-free execution, then proves the
// victim's protected push is refused and its live credential still writes a
// granted work ref.
func testProtectedRetryForeignApprovalRefused(t *testing.T) {
	t.Helper()
	fixture := startGitHarness(t)
	reviewerToken, reviewerPrincipal := grantProtectedReviewer(t, fixture, protectedGoldenReviewerSubject)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	applied := fixture.applyPipeline("protected-retry-foreign-victim",
		protectedRetryForeignPipeline(t, fixture, base))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline", applied)
	}
	taskID, lifecycle, _ := protectedRetryCreateTask(t, fixture, applied.PipelineID)
	victimID := protectedRetryStartTask(t, fixture, fixture.engineerToken, taskID,
		"start-protected-retry-foreign-victim", &lifecycle)

	// Implement and test for real.
	waitProtectedActionRunning(t, fixture, victimID, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, victimID, "implement")
	implementToken := waitProtectedGoldenCredential(t, fixture, victimID, "implement")
	requireProtectedGoldenCredentialUsable(t, fixture, victimID, implementToken)
	releaseProtectedGoldenGate(t, fixture, implementContainer, protectedRetryImplementGate)

	waitProtectedActionRunning(t, fixture, victimID, "test")
	testContainer := waitProtectedGoldenContainer(t, fixture, victimID, "test")
	_ = waitProtectedGoldenCredential(t, fixture, victimID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, victimID)
	candidate := protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if candidate == "" || candidate == base {
		t.Fatalf("victim implement candidate = %q; want a commit different from the base %q",
			candidate, base)
	}
	requireProtectedGoldenHead(t, fixture, testContainer, candidate)
	releaseProtectedGoldenGate(t, fixture, testContainer, protectedRetryTestGate)

	// The victim's protected action is live and gated before its push.
	integrate := waitProtectedActionRunning(t, fixture, victimID, "integrate")
	integrateAction := actionByName(t, integrate, "integrate")
	integrateContainer := waitProtectedGoldenContainer(t, fixture, victimID, "integrate")
	_ = waitProtectedGoldenCredential(t, fixture, victimID, "integrate")

	// The foreign execution holds a genuine pass for exactly this effect.
	foreignID, foreignAction := protectedRetryRunForeignApproval(t, fixture, reviewerToken, base, candidate)
	if foreignID == victimID {
		t.Fatalf("foreign approval execution equals the victim execution %q", victimID)
	}
	assertProtectedRetryApproval(t, fixture, "foreign approval", foreignAction,
		reviewerPrincipal, base, candidate)

	journalBefore := protectedRetryJournalCount(t, fixture)
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("protected main before the victim push = %q; want the base %q", main, base)
	}
	releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedRetryIntegrateGate)

	finished := waitProtectedGoldenTerminal(t, fixture, victimID)
	logs := fixture.executionLogs(t, finished)
	if finished.State != "failed" {
		t.Fatalf("victim execution state = %q after the protected push; want failed\n%s",
			finished.State, logs)
	}
	if !strings.Contains(logs, "subject-protected-push-refused") ||
		!strings.Contains(logs, "subject-control-pushed") {
		t.Fatalf("victim logs are missing the refused/control push markers:\n%s", logs)
	}
	if got := actionByName(t, finished, "integrate").State; got != "failed" {
		t.Fatalf("victim integrate action state = %q; want failed", got)
	}

	row, approval := protectedRetryWireEvidence(t, fixture, victimID, integrateAction.ActionID, candidate)
	assertProtectedRetryRefusedEffect(t, fixture, "foreign-execution approval",
		row, approval, victimID, integrateAction.ActionID, base, candidate)
	assertProtectedRetryProtectedRefUnchanged(t, fixture, "foreign-execution refusal",
		base, journalBefore, candidate)
	assertProtectedRetryControlPush(t, fixture, protectedRetryControlRef, candidate)
}

// testProtectedRetryPriorApprovalRefused fails an execution after its genuine
// pass, retries the task, and proves the retry cannot reuse the earlier
// execution's approval even when it presents the earlier execution's exact
// candidate.
func testProtectedRetryPriorApprovalRefused(t *testing.T) {
	t.Helper()
	fixture := startGitHarness(t)
	reviewerToken, reviewerPrincipal := grantProtectedReviewer(t, fixture, protectedGoldenReviewerSubject)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	applied := fixture.applyPipeline(protectedRetryPipelineName,
		protectedRetryPipeline(t, fixture, base))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline", applied)
	}
	taskID, lifecycle, _ := protectedRetryCreateTask(t, fixture, applied.PipelineID)

	first := protectedRetryRunAttempt(t, fixture, reviewerToken, taskID,
		fixture.engineerToken, "start-protected-retry-prior-first", &lifecycle, base,
		protectedRetryAttemptOptions{retainPrior: true, failAfterPass: true}, "")
	if first.finished.State != "failed" {
		t.Fatalf("prior execution state = %q; want failed after the genuine pass\n%s",
			first.finished.State, first.logs)
	}
	assertProtectedRetryApproval(t, fixture, "prior approval", first.reviewAction,
		reviewerPrincipal, base, first.candidate)
	if count := protectedRetryJournalCount(t, fixture); count != 0 {
		t.Fatalf("reported protected journal entries after the prior failure = %d; want 0", count)
	}
	if got := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRetryPriorRef); got != first.candidate {
		t.Fatalf("retained prior ref = %q; want the prior candidate %q", got, first.candidate)
	}

	firstTask := waitProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID,
		"task open with the failed prior execution", func(read api.TaskReadResult) bool {
			return read.State == "open" && read.CurrentExecution == first.executionID
		})
	if len(firstTask.ExecutionHistory) != 1 || firstTask.ExecutionHistory[0] != first.executionID {
		t.Fatalf("prior task history = %v; want exactly [%s]", firstTask.ExecutionHistory, first.executionID)
	}
	failedItem := protectedRetryFailedTask(t, fixture, taskID)
	failedTask := readProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID)
	if failedItem.Version != failedTask.LifecycleVersion {
		t.Fatalf("failed_task Version = %d; want the task lifecycle version %d",
			failedItem.Version, failedTask.LifecycleVersion)
	}

	retry := protectedRetryRunAttempt(t, fixture, reviewerToken, taskID,
		fixture.OperatorToken, "start-protected-retry-prior-operator", &failedItem.Version, base,
		protectedRetryAttemptOptions{probePrior: true, stopAfterProbe: true}, first.candidate)
	if retry.executionID == first.executionID {
		t.Fatalf("retry reused the prior execution %q", first.executionID)
	}

	// The earlier pass is genuine for exactly the candidate the retry tried to
	// integrate. The retry's attempt is refused because the approval belongs to
	// the earlier execution.
	assertProtectedRetryRefusedEffect(t, fixture, "prior-execution approval",
		retry.probeRefused, retry.probeRefusedApproval, retry.executionID,
		retry.testAction.ActionID, base, first.candidate)
	assertProtectedRetryProtectedRefUnchanged(t, fixture, "prior-execution refusal",
		base, 0, first.candidate)

	if retry.finished.State != "failed" {
		t.Fatalf("retry execution state = %q after the prior-approval refusal; want failed",
			retry.finished.State)
	}
	if got := actionByName(t, retry.finished, "test").State; got != "failed" {
		t.Fatalf("retry test action state = %q; want failed after the refused probe", got)
	}
	if review := actionByName(t, retry.finished, "review"); review.State == "running" ||
		review.State == "succeeded" || review.Decision == "pass" {
		t.Fatalf("retry review action advanced despite the prior-approval refusal: %+v", review)
	}
}
