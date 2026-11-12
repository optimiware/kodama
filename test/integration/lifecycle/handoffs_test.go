//go:build integration

package lifecycle

// Task conversation handoffs on the real factory composition.
//
// Coverage map (docs/specs/11-verification.md):
//   - AC 3 idempotency: a duplicate task.start replays to the same queued
//     execution and one receipt; task.create_and_start and task.reply replayed
//     over MCP with the same idempotency key return the same identities and
//     commit exactly one receipt each.
//   - AC 11 materialization: the fresh execution receives /kodama/task.json
//     carrying the answer and a brand-new workspace; the previous execution's
//     /workspace scratch is absent.
//   - AC 16 planned waits: asking while a workload runs stops it, clears the
//     execution credential and leaves no workload container; the waiting task
//     is visible only to the assigned role.
//   - AC 17 handoffs: ask cancels the nonterminal execution and marks the task
//     waiting; the assigned reply queues exactly one fresh execution whose
//     action reads the updated conversation; missing durable context fails the
//     restoring action; cancel settles the handoff and stale replies cannot
//     reopen it.
//   - AC 18 task progress: a failed open task retried by an engineer starts a
//     fresh execution, preserves the failed execution and its action history,
//     and does not replay the failed action. Old-event immunity is NOT credited
//     here.
//   - AC 6 recovery: NOT credited. This suite proves the durable handoff
//     commit and the lost-response-after-effect replay, not a process, broker
//     or runtime restart.
//   - AC 13 ordered flow: NOT credited here.
//
// Reviewer question discovery: the waiting_task inbox item carries question_id,
// the question body, durable references, the task lifecycle version and a
// Version equal to the outstanding question version. A reviewer-only principal
// therefore answers from its own inbox without task.read or task.history, which
// stay forbidden to that role. The tests assert both the projection and the
// reply that uses it.
//
// Failed-task discovery (KODAMA.md line 218): an authorized human operator
// discovers an open task whose current execution failed through the failed_task
// inbox kind. The item carries only task_id, its Version is the task lifecycle
// version and At is the failed execution's timestamp; it carries no
// execution/action identity and no question projection. A reviewer-only
// principal does not see it.
//
// KODAMA.md line 230 says "An authorized operator can start a fresh attempt or
// cancel". The role matrix grants task.start to engineer and operator, with the
// operator grant conditional on an OPEN task whose CURRENT execution is FAILED
// (`failed_task_operator_start`). TestOperatorRetry proves the operator retry
// of a failed open task, the engineer retry, and that the failed execution is
// preserved. The conditional matrix itself is covered by the store unit tests.
//
// The handoff cancellation reason (`requested`) is not observable through any
// public field: execution.inspect exposes the terminal execution state but not
// the cancellation reason. The tests assert the observable contract (state
// cancelled, cleanup complete, credential refused) and report the missing
// reason projection.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// handoffAnswer is the answer the restoring action of the successful
	// handoff pipeline requires in /kodama/task.json.
	handoffAnswer = "approved-context-42"
	// missingContextAnswer selects the restoring branch of the failing pipeline.
	missingContextAnswer = "restore-after-ask-7"
	// receiptAnswer keeps the reply execution running long enough that the
	// MCP replay observes the same still-open task rather than a settled one.
	receiptAnswer = "receipt-answer-9"
)

// TestTaskHandoffs proves the real task handoff lifecycle. Every subtest owns
// the task it creates; the shared harness is isolated per test process and
// releases its infrastructure on cleanup.
func TestTaskHandoffs(t *testing.T) {
	harness := factory.Start(t)

	t.Run("create starts nothing and start admits one execution", func(t *testing.T) {
		testTaskCreateAndStart(t, harness)
	})
	t.Run("ask cancels the running execution and waits for the assignee", func(t *testing.T) {
		testTaskAskHandoff(t, harness)
	})
	t.Run("assigned reply queues a fresh execution with the answer", func(t *testing.T) {
		testTaskReply(t, harness)
	})
	t.Run("missing durable context fails the restoring action", func(t *testing.T) {
		testTaskMissingContext(t, harness)
	})
	t.Run("cancel settles a waiting task and stale replies are refused", func(t *testing.T) {
		testTaskCancel(t, harness)
	})
	t.Run("REST and MCP retries share one receipt", func(t *testing.T) {
		testTaskReceiptParity(t, harness)
	})
}

// TestOperatorRetry proves the retry behavior: the operator-only token retries
// an open task whose current execution failed as a fresh execution, the
// engineer retry does the same, and neither touches the failed execution or
// replays its action.
func TestOperatorRetry(t *testing.T) {
	harness := factory.Start(t)
	applied := applyPipeline(t, harness, "retry", retryPipeline(t, harness))

	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "retry a failed task",
		PipelineID: applied.PipelineID,
	}, "retry-create")
	failed := waitExecutionSettled(t, harness, harness.ProjectToken, created.ExecutionID)
	if failed.State != "failed" {
		t.Fatalf("first attempt state = %q; want failed", failed.State)
	}
	if failed.Failure == nil || failed.Failure.Kind != "runtime" || failed.Failure.Reason != "workload_exited" {
		t.Fatalf("first attempt failure = %+v; want runtime/workload_exited", failed.Failure)
	}
	failedAction := actionByName(t, failed, "work")
	failedRow, err := harness.ActionRow(t.Context(), failed.ExecutionID, "work")
	if err != nil {
		t.Fatalf("read failed action row: %v", err)
	}

	task := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if task.State != "open" {
		t.Fatalf("failed task state = %q; want open", task.State)
	}
	if task.CurrentExecution != failed.ExecutionID {
		t.Fatalf("failed task current execution = %q; want %q", task.CurrentExecution, failed.ExecutionID)
	}
	if len(task.ExecutionHistory) != 1 || task.ExecutionHistory[0] != failed.ExecutionID {
		t.Fatalf("failed task history = %v; want [%s]", task.ExecutionHistory, failed.ExecutionID)
	}

	// The failed task belongs to the human operator's inbox. A failed_task item
	// names only the task: no execution/action identity and no question
	// projection. Version is the task lifecycle version (the value
	// task.start/task.cancel name as expected_version) and At is the failed
	// execution's timestamp.
	operatorInbox := handoffInboxResult(t, harness, harness.OperatorToken, false, []string{"failed_task"})
	discovered, found := handoffFindFailedTask(operatorInbox, created.TaskID)
	if !found {
		t.Fatalf("operator inbox has no failed_task for %q: %+v", created.TaskID, operatorInbox.Items)
	}
	handoffAssertFailedTaskItem(t, discovered, task.LifecycleVersion)

	// REST and MCP agree, and an explicit failed_task kind and the default
	// (omitted) kind list expose the same item. The reviewer-only principal
	// sees the query succeed but never sees the failed task.
	for _, transport := range []struct {
		name string
		mcp  bool
	}{
		{name: "REST", mcp: false},
		{name: "MCP", mcp: true},
	} {
		for _, kinds := range [][]string{{"failed_task"}, nil} {
			operatorPage := handoffInboxResult(t, harness, harness.OperatorToken, transport.mcp, kinds)
			item, ok := handoffFindFailedTask(operatorPage, created.TaskID)
			if !ok {
				t.Fatalf("operator %s kinds=%v inbox has no failed_task for %q: %+v",
					transport.name, kinds, created.TaskID, operatorPage.Items)
			}
			handoffAssertFailedTaskItem(t, item, task.LifecycleVersion)

			reviewerPage := handoffInboxResult(t, harness, harness.ReviewerToken, transport.mcp, kinds)
			if _, ok := handoffFindFailedTask(reviewerPage, created.TaskID); ok {
				t.Fatalf("reviewer %s kinds=%v inbox exposed the failed task %q: %+v",
					transport.name, kinds, created.TaskID, reviewerPage.Items)
			}
		}
	}

	// The role matrix grants the operator task.start only for an OPEN task
	// whose CURRENT execution is FAILED. The first attempt failed, so the
	// operator-only token is admitted as a fresh attempt. The discovered item's
	// Version is exactly the task lifecycle version this start must name. The
	// conditional authority itself (waiting/done/non-failed current execution
	// refused) is covered by the store unit tests in
	// internal/shell/store/units/work/authority_operator_test.go.
	operatorRetried := startTask(t, harness, harness.OperatorToken, created.TaskID, &discovered.Version, "retry-operator")
	if operatorRetried.ExecutionID == failed.ExecutionID {
		t.Fatalf("operator retry reused the failed execution %q", failed.ExecutionID)
	}
	operatorExecution := waitExecution(t, harness, harness.ProjectToken, operatorRetried.ExecutionID)
	if operatorExecution.State != "failed" {
		t.Fatalf("operator retry state = %q; want failed (the retry pipeline always fails)", operatorExecution.State)
	}
	if operatorExecution.Failure == nil || operatorExecution.Failure.Kind != "runtime" {
		t.Fatalf("operator retry failure = %+v; want kind runtime", operatorExecution.Failure)
	}
	operatorAction := actionByName(t, operatorExecution, "work")
	if operatorAction.ActionID == failedAction.ActionID {
		t.Fatalf("operator retry reused the failed action id %q", failedAction.ActionID)
	}
	afterOperator := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if afterOperator.State != "open" || afterOperator.CurrentExecution != operatorRetried.ExecutionID {
		t.Fatalf("operator retry changed the task unexpectedly: state=%q current=%q; want open/%s",
			afterOperator.State, afterOperator.CurrentExecution, operatorRetried.ExecutionID)
	}
	if len(afterOperator.ExecutionHistory) != 2 ||
		afterOperator.ExecutionHistory[0] != failed.ExecutionID ||
		afterOperator.ExecutionHistory[1] != operatorRetried.ExecutionID {
		t.Fatalf("operator retry history = %v; want [%s %s]",
			afterOperator.ExecutionHistory, failed.ExecutionID, operatorRetried.ExecutionID)
	}
	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after operator retry: %v", err)
	}
	if count != 2 {
		t.Fatalf("execution count after operator retry = %d; want 2", count)
	}

	// The engineer retry is admitted as a fresh execution. The operator retry
	// is terminal, so the replacement may dispatch.
	retried := startTask(t, harness, harness.ProjectToken, created.TaskID, &afterOperator.LifecycleVersion, "retry-engineer")
	if retried.ExecutionID == failed.ExecutionID {
		t.Fatalf("engineer retry reused the failed execution %q", failed.ExecutionID)
	}
	second := waitExecution(t, harness, harness.ProjectToken, retried.ExecutionID)
	if second.State != "failed" {
		t.Fatalf("retry state = %q; want failed (the retry pipeline always fails)", second.State)
	}
	if second.Failure == nil || second.Failure.Kind != "runtime" {
		t.Fatalf("retry failure = %+v; want kind runtime", second.Failure)
	}

	// The retry ran a brand-new action, not a replay of the failed one.
	secondAction := actionByName(t, second, "work")
	if secondAction.ActionID == failedAction.ActionID {
		t.Fatalf("retry reused the failed action id %q", failedAction.ActionID)
	}
	secondRow, err := harness.ActionRow(t.Context(), second.ExecutionID, "work")
	if err != nil {
		t.Fatalf("read retry action row: %v", err)
	}
	if secondRow.WorkloadID == "" || secondRow.WorkloadID == failedRow.WorkloadID {
		t.Fatalf("retry workload id = %q; want a fresh workload distinct from %q", secondRow.WorkloadID, failedRow.WorkloadID)
	}

	// The failed execution and its action are untouched by the retry. Both
	// observations are taken after the execution view is quiescent (cleanup
	// complete and the same version read twice): a single inspect can observe
	// the cleanup record from a newer commit than the execution row, so a
	// version captured at a non-quiescent instant is not a valid baseline.
	unchanged := waitExecutionSettled(t, harness, harness.ProjectToken, failed.ExecutionID)
	if unchanged.State != failed.State || unchanged.Version != failed.Version {
		t.Fatalf("retry changed the failed execution: before state=%q version=%d after state=%q version=%d",
			failed.State, failed.Version, unchanged.State, unchanged.Version)
	}
	if unchanged.Failure == nil || failed.Failure == nil || *unchanged.Failure != *failed.Failure {
		t.Fatalf("retry changed the failed execution failure: before=%+v after=%+v", failed.Failure, unchanged.Failure)
	}
	if unchanged.Cleanup.State != failed.Cleanup.State {
		t.Fatalf("retry changed the failed execution cleanup: before=%q after=%q", failed.Cleanup.State, unchanged.Cleanup.State)
	}
	unchangedAction := actionByName(t, unchanged, "work")
	if unchangedAction.ActionID != failedAction.ActionID || unchangedAction.State != "failed" || unchangedAction.Version != failedAction.Version {
		t.Fatalf("retry changed the failed action: before=%+v after=%+v", failedAction, unchangedAction)
	}

	// Task history preserves all three executions in order.
	after := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if after.State != "open" {
		t.Fatalf("retried task state = %q; want open after the retry failure", after.State)
	}
	if after.CurrentExecution != retried.ExecutionID {
		t.Fatalf("retried task current execution = %q; want %q", after.CurrentExecution, retried.ExecutionID)
	}
	if len(after.ExecutionHistory) != 3 ||
		after.ExecutionHistory[0] != failed.ExecutionID ||
		after.ExecutionHistory[1] != operatorRetried.ExecutionID ||
		after.ExecutionHistory[2] != retried.ExecutionID {
		t.Fatalf("retried task history = %v; want [%s %s %s]",
			after.ExecutionHistory, failed.ExecutionID, operatorRetried.ExecutionID, retried.ExecutionID)
	}
	count, err = harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after retry: %v", err)
	}
	if count != 3 {
		t.Fatalf("execution count after retry = %d; want 3", count)
	}

	// The operator's granted mutation path is cancellation: task.cancel is
	// granted to operator and engineer. The operator cannot read the task
	// (task.read is engineer/viewer only), so it refreshes its own failed_task
	// inbox discovery for the current failure; that item's Version is the task
	// lifecycle version the cancel must name. Cancelling the failed open task
	// must settle it without queueing another execution.
	refreshedInbox := handoffInboxResult(t, harness, harness.OperatorToken, false, []string{"failed_task"})
	refreshed, found := handoffFindFailedTask(refreshedInbox, created.TaskID)
	if !found {
		t.Fatalf("operator inbox has no failed_task for %q after the retries: %+v", created.TaskID, refreshedInbox.Items)
	}
	if refreshed.Version != after.LifecycleVersion {
		t.Fatalf("refreshed failed_task Version = %d; want the task lifecycle version %d", refreshed.Version, after.LifecycleVersion)
	}
	operatorResult := harness.MustREST(harness.OperatorToken, "task.cancel", api.TaskCancelRequest{TaskID: created.TaskID}, &refreshed.Version, "retry-operator-cancel")
	var operatorCancelled api.TaskCancelResult
	harness.Decode(operatorResult, &operatorCancelled)
	if operatorCancelled.LifecycleVersion <= after.LifecycleVersion {
		t.Fatalf("operator cancel lifecycle version = %d; want > %d", operatorCancelled.LifecycleVersion, after.LifecycleVersion)
	}
	final := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if final.State != "cancelled" {
		t.Fatalf("task state after operator cancel = %q; want cancelled", final.State)
	}
	if final.Question != nil {
		t.Fatalf("operator-cancelled task still carries a question: %+v", final.Question)
	}
	count, err = harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after operator cancel: %v", err)
	}
	if count != 3 {
		t.Fatalf("operator cancel queued executions: count=%d want=3", count)
	}
}

// testTaskCreateAndStart proves creating a task starts nothing, a start admits
// exactly one queued execution that runs, the same-key retry replays it, and a
// distinct start carrying the stale lifecycle version is refused.
func testTaskCreateAndStart(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-start", holdPipeline(t, harness))

	created := createTask(t, harness, harness.ProjectToken, api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      "start nothing on create",
		PipelineID: applied.PipelineID,
	}, "task-start-create")

	initial := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if initial.State != "open" {
		t.Fatalf("created task state = %q; want open", initial.State)
	}
	if initial.CurrentExecution != "" || len(initial.ExecutionHistory) != 0 {
		t.Fatalf("created task already has execution state: current=%q history=%v", initial.CurrentExecution, initial.ExecutionHistory)
	}
	count, err := harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after create: %v", err)
	}
	if count != 0 {
		t.Fatalf("task.create started %d executions; want 0", count)
	}

	started := startTask(t, harness, harness.ProjectToken, created.TaskID, &initial.LifecycleVersion, "task-start-once")
	running := waitExecutionRunning(t, harness, started.ExecutionID)
	if running.TaskID != created.TaskID {
		t.Fatalf("started execution task = %q; want %q", running.TaskID, created.TaskID)
	}
	count, err = harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after start: %v", err)
	}
	if count != 1 {
		t.Fatalf("task.start admitted %d executions; want exactly 1", count)
	}

	// The same idempotency key replays the original admission: same execution,
	// same lifecycle version, one receipt.
	replayed := startTask(t, harness, harness.ProjectToken, created.TaskID, &initial.LifecycleVersion, "task-start-once")
	if replayed.ExecutionID != started.ExecutionID || replayed.LifecycleVersion != started.LifecycleVersion {
		t.Fatalf("start replay = %+v; want %+v", replayed, started)
	}
	receipts, err := harness.ReceiptCount(ctx, "task-start-once")
	if err != nil {
		t.Fatalf("count start receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("start receipt count = %d; want exactly 1", receipts)
	}
	count, err = harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("start replay admitted %d executions; want 1", count)
	}

	// A distinct start carrying the now-stale lifecycle version is refused at
	// the commit boundary. The product's replacement path (a refreshed version
	// while the predecessor is nonterminal) is a separate, existing behavior
	// documented in the settlement tests; this assertion pins the
	// optimistic-concurrency vocabulary.
	_, publicError := harness.REST(harness.ProjectToken, "task.start", api.TaskStartRequest{TaskID: created.TaskID}, &initial.LifecycleVersion, "task-start-stale")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale second task.start code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	count, err = harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after stale start: %v", err)
	}
	if count != 1 {
		t.Fatalf("stale second start admitted %d executions; want 1", count)
	}

	// Settle the running execution so the harness leaves no live wait.
	cancelTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, "task-start-cancel")
	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if finished.State != "cancelled" {
		t.Fatalf("cancelled start execution state = %q; want cancelled", finished.State)
	}
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testTaskAskHandoff proves asking on a running task cancels its execution,
// revokes the credential, waits for the reviewer assignment, stops the
// workload and exposes the waiting task only to the assigned role.
func testTaskAskHandoff(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-ask", holdPipeline(t, harness))
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "ask while running",
		PipelineID: applied.PipelineID,
	}, "task-ask-create")

	waitExecutionRunning(t, harness, created.ExecutionID)
	// The execution credential is materialized for the running workload. It is
	// read before the handoff so the test can present the revoked value after.
	credential := waitExecutionCredential(t, harness, created.ExecutionID, "hold")

	// Positive control: while the execution is running, the materialized
	// credential authenticates. Without this, a post-ask refusal could be the
	// terminal execution state rather than a revoked credential.
	selfResult, selfError := harness.REST(credential, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: created.ExecutionID}, nil, "")
	if selfError.Code != "" {
		t.Fatalf("running execution credential was refused: %s (%s)", selfError.Code, selfError.Message)
	}
	var selfInspect api.ExecutionInspectResult
	harness.Decode(selfResult, &selfInspect)
	if selfInspect.ExecutionID != created.ExecutionID || selfInspect.State != "running" {
		t.Fatalf("credential self-inspect = %+v; want the running execution %q", selfInspect, created.ExecutionID)
	}
	nonceBefore, nonceErr := harness.ExecutionCredentialNoncePresent(ctx, created.ExecutionID)
	if nonceErr != nil {
		t.Fatalf("read credential nonce before ask: %v", nonceErr)
	}
	if !nonceBefore {
		t.Fatalf("positive control: running execution has no stored credential nonce")
	}

	task := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if task.State != "open" || task.CurrentExecution != created.ExecutionID {
		t.Fatalf("running task = %+v; want open with current %q", task, created.ExecutionID)
	}

	// A stale task lifecycle version is refused without committing anything.
	// This is a direct, non-retrying call: the retrying ask helper above hides a
	// product that ignores the caller's lifecycle version.
	if task.LifecycleVersion <= 1 {
		t.Fatalf("running task lifecycle version = %d; want > 1", task.LifecycleVersion)
	}
	staleLifecycle := task.LifecycleVersion - 1
	_, staleAskError := harness.REST(harness.ProjectToken, "task.ask", api.TaskAskRequest{
		TaskID:     created.TaskID,
		Question:   "stale lifecycle ask",
		Assignment: "role/reviewer",
	}, &staleLifecycle, "task-ask-stale-lifecycle")
	if staleAskError.Code != api.CodeVersionConflict {
		t.Fatalf("stale lifecycle task.ask code = %q; want %q", staleAskError.Code, api.CodeVersionConflict)
	}
	afterStaleAsk := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if afterStaleAsk.State != "open" || afterStaleAsk.Question != nil || afterStaleAsk.LifecycleVersion != task.LifecycleVersion {
		t.Fatalf("stale lifecycle task.ask changed the task: %+v", afterStaleAsk)
	}
	if stillRunning := inspectExecution(t, harness, harness.ProjectToken, created.ExecutionID); stillRunning.State != "running" {
		t.Fatalf("stale lifecycle task.ask changed the execution state to %q; want running", stillRunning.State)
	}

	asked := askTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, api.TaskAskRequest{
		Question:   "What context should the next attempt restore?",
		Assignment: "role/reviewer",
	}, "task-ask")
	if asked.QuestionID == "" || asked.QuestionVersion == 0 {
		t.Fatalf("ask result = %+v; want a question identity and version", asked)
	}

	// The ask commits the execution cancellation and the credential
	// invalidation atomically, so both are observable as soon as it returns.
	immediately := inspectExecution(t, harness, harness.ProjectToken, created.ExecutionID)
	if immediately.State != "cancelled" {
		t.Fatalf("execution state immediately after ask = %q; want cancelled", immediately.State)
	}
	_, revokedError := harness.REST(credential, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: created.ExecutionID}, nil, "")
	if revokedError.Code != api.CodeUnauthenticated {
		t.Fatalf("credential immediately after ask code = %q; want %q", revokedError.Code, api.CodeUnauthenticated)
	}
	// The public edge also disables an execution principal once it leaves the
	// running state, so the refusal above alone does not prove revocation. The
	// durable nonce probe does: a task handoff clears the stored credential.
	nonceAfter, nonceErr := harness.ExecutionCredentialNoncePresent(ctx, created.ExecutionID)
	if nonceErr != nil {
		t.Fatalf("read credential nonce after ask: %v", nonceErr)
	}
	if nonceAfter {
		t.Fatalf("ask left the execution credential nonce stored; the terminal state alone also refuses authentication")
	}

	waiting := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil || waiting.Question.QuestionID != asked.QuestionID {
		t.Fatalf("waiting task question = %+v; want %q", waiting.Question, asked.QuestionID)
	}
	if waiting.Question.Assignment != "role/reviewer" {
		t.Fatalf("question assignment = %q; want role/reviewer", waiting.Question.Assignment)
	}

	// The nonterminal execution is cancelled and its cleanup settles.
	cancelled := waitExecution(t, harness, harness.ProjectToken, created.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("execution state after ask = %q; want cancelled", cancelled.State)
	}
	assertExecutionResourcesReleased(t, harness, created.ExecutionID)

	// The waiting task is visible to the assigned reviewer and to no other
	// role-limited principal.
	reviewerInbox := inboxWaitingTasks(t, harness, harness.ReviewerToken)
	item, ok := findWaitingTask(reviewerInbox, created.TaskID)
	if !ok {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", created.TaskID, reviewerInbox.Items)
	}
	if item.Assignment != "role/reviewer" {
		t.Fatalf("waiting_task assignment = %q; want role/reviewer", item.Assignment)
	}
	// The waiting_task item carries the complete outstanding-question
	// projection, so the exact assignee can answer without any read the
	// reviewer role is not granted. Version stays the QUESTION version;
	// LifecycleVersion is the task lifecycle version.
	if item.QuestionID != waiting.Question.QuestionID {
		t.Fatalf("waiting_task question_id = %q; want %q", item.QuestionID, waiting.Question.QuestionID)
	}
	if item.Question != waiting.Question.Body {
		t.Fatalf("waiting_task question body = %q; want %q", item.Question, waiting.Question.Body)
	}
	if item.Version != waiting.Question.Version {
		t.Fatalf("waiting_task version = %d; want the question version %d", item.Version, waiting.Question.Version)
	}
	if item.LifecycleVersion == 0 || item.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("waiting_task lifecycle_version = %d; want the task lifecycle version %d",
			item.LifecycleVersion, waiting.LifecycleVersion)
	}
	if !slices.Equal(item.References, waiting.Question.References) {
		t.Fatalf("waiting_task references = %v; want the question references %v",
			item.References, waiting.Question.References)
	}
	operatorInbox := inboxWaitingTasks(t, harness, harness.OperatorToken)
	if _, found := findWaitingTask(operatorInbox, created.TaskID); found {
		t.Fatalf("operator-only inbox exposed a reviewer-assigned waiting task: %+v", operatorInbox.Items)
	}

	// The reviewer still cannot read the task directly: the inbox projection is
	// the intended discovery path, not a widening of task.read/task.history.
	if _, err := harness.REST(harness.ReviewerToken, "task.read", api.TaskReadRequest{TaskID: created.TaskID}, nil, ""); err.Code != api.CodeForbidden {
		t.Fatalf("reviewer task.read code = %q; want %q", err.Code, api.CodeForbidden)
	}
	if _, err := harness.REST(harness.ReviewerToken, "task.history", api.TaskHistoryRequest{TaskID: created.TaskID}, nil, ""); err.Code != api.CodeForbidden {
		t.Fatalf("reviewer task.history code = %q; want %q", err.Code, api.CodeForbidden)
	}
	encoded, marshalErr := json.Marshal(item)
	if marshalErr != nil {
		t.Fatalf("marshal waiting_task item: %v", marshalErr)
	}
	if !strings.Contains(string(encoded), `"question_id":"`+waiting.Question.QuestionID+`"`) {
		t.Fatalf("waiting_task wire projection lacks the question identity: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"lifecycle_version":`) {
		t.Fatalf("waiting_task wire projection lacks the lifecycle version: %s", encoded)
	}

	// Cancel the waiting task so the harness is left with no outstanding
	// handoff; the dedicated cancel subtest proves the cancel semantics.
	cancelTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, "task-ask-cancel")
}

// testTaskReply proves a stale question version and a principal outside the
// assigned role are refused, then the assigned reviewer's reply queues a fresh
// execution with a new workspace whose action reads the answer from
// /kodama/task.json.
func testTaskReply(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-reply", handoffPipeline(t, harness))
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "reply with context",
		PipelineID: applied.PipelineID,
	}, "task-reply-create")

	waitExecutionRunning(t, harness, created.ExecutionID)
	readTask(t, harness, harness.ProjectToken, created.TaskID)
	askTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, api.TaskAskRequest{
		Question:   "Which durable context should the next attempt restore?",
		Assignment: "role/reviewer",
	}, "task-reply-ask")
	waiting := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question")
	}
	// The assigned reviewer discovers the question identity through its own
	// inbox, not through the engineer's task.read: the reviewer role is not
	// granted task.read or task.history.
	reviewerInbox := inboxWaitingTasks(t, harness, harness.ReviewerToken)
	inboxItem, ok := findWaitingTask(reviewerInbox, created.TaskID)
	if !ok {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", created.TaskID, reviewerInbox.Items)
	}
	if inboxItem.QuestionID != waiting.Question.QuestionID ||
		inboxItem.Version != waiting.Question.Version ||
		inboxItem.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("reviewer waiting_task projection = %+v; want question %q version %d lifecycle %d",
			inboxItem, waiting.Question.QuestionID, waiting.Question.Version, waiting.LifecycleVersion)
	}

	// A stale task lifecycle version is refused before any admission. This is a
	// direct, non-retrying call, so a product that ignores the caller's
	// lifecycle version would queue a second execution.
	if waiting.LifecycleVersion <= 1 {
		t.Fatalf("waiting task lifecycle version = %d; want > 1", waiting.LifecycleVersion)
	}
	staleLifecycle := waiting.LifecycleVersion - 1
	_, staleLifecycleError := harness.REST(harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              inboxItem.QuestionID,
		ExpectedQuestionVersion: inboxItem.Version,
		Answer:                  handoffAnswer,
	}, &staleLifecycle, "task-reply-stale-lifecycle")
	if staleLifecycleError.Code != api.CodeVersionConflict {
		t.Fatalf("stale lifecycle task.reply code = %q; want %q", staleLifecycleError.Code, api.CodeVersionConflict)
	}
	assertWaitingUnchanged(t, harness, created.TaskID, waiting, applied.PipelineID, 1)

	// A reply naming a different well-formed question identity is refused even
	// with the current lifecycle and question versions.
	_, wrongQuestionError := harness.REST(harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              inboxItem.QuestionID + "-wrong",
		ExpectedQuestionVersion: inboxItem.Version,
		Answer:                  handoffAnswer,
	}, &waiting.LifecycleVersion, "task-reply-wrong-question")
	if wrongQuestionError.Code != api.CodeVersionConflict {
		t.Fatalf("wrong question task.reply code = %q; want %q", wrongQuestionError.Code, api.CodeVersionConflict)
	}
	assertWaitingUnchanged(t, harness, created.TaskID, waiting, applied.PipelineID, 1)

	// A stale expected question version is refused without queueing anything.
	stale := api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              inboxItem.QuestionID,
		ExpectedQuestionVersion: inboxItem.Version + 1,
		Answer:                  handoffAnswer,
	}
	_, publicError := harness.REST(harness.ReviewerToken, "task.reply", stale, &waiting.LifecycleVersion, "task-reply-stale")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale question version code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}

	// An operator-only human does not hold the assigned reviewer role.
	_, publicError = harness.REST(harness.OperatorToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              inboxItem.QuestionID,
		ExpectedQuestionVersion: inboxItem.Version,
		Answer:                  handoffAnswer,
	}, &waiting.LifecycleVersion, "task-reply-operator")
	if publicError.Code != api.CodeForbidden {
		t.Fatalf("operator-only reply code = %q; want %q", publicError.Code, api.CodeForbidden)
	}
	afterRefusals := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if afterRefusals.State != "waiting" || afterRefusals.Question == nil {
		t.Fatalf("refused replies changed the waiting task: %+v", afterRefusals)
	}
	count, err := harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after refused replies: %v", err)
	}
	if count != 1 {
		t.Fatalf("refused replies queued %d extra executions; want 0", count-1)
	}

	// The assigned reviewer answers with the exact question identity/version it
	// discovered through its own inbox; no engineer read supplies them.
	if afterRefusals.LifecycleVersion != inboxItem.LifecycleVersion {
		t.Fatalf("task lifecycle version after refusals = %d; want the inbox lifecycle version %d",
			afterRefusals.LifecycleVersion, inboxItem.LifecycleVersion)
	}
	replied := replyTask(t, harness, harness.ReviewerToken, created.TaskID, &inboxItem.LifecycleVersion, api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              inboxItem.QuestionID,
		ExpectedQuestionVersion: inboxItem.Version,
		Answer:                  handoffAnswer,
	}, "task-reply")
	if replied.ExecutionID == "" || replied.ExecutionID == created.ExecutionID {
		t.Fatalf("reply execution = %q; want a fresh execution", replied.ExecutionID)
	}
	if replied.QuestionVersion <= waiting.Question.Version {
		t.Fatalf("reply question version = %d; want > %d", replied.QuestionVersion, waiting.Question.Version)
	}

	// The fresh execution runs the restoring action, which only succeeds when
	// /kodama/task.json carries the answer.
	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	requireSucceeded(t, fresh)
	if fresh.TaskID != created.TaskID {
		t.Fatalf("fresh execution task = %q; want %q", fresh.TaskID, created.TaskID)
	}

	firstWorkspace, err := harness.ExecutionWorkspaceID(ctx, created.ExecutionID)
	if err != nil {
		t.Fatalf("read first execution workspace: %v", err)
	}
	freshWorkspace, err := harness.ExecutionWorkspaceID(ctx, replied.ExecutionID)
	if err != nil {
		t.Fatalf("read fresh execution workspace: %v", err)
	}
	if firstWorkspace == "" || freshWorkspace == "" || firstWorkspace == freshWorkspace {
		t.Fatalf("workspace ids first=%q fresh=%q; want two distinct non-empty workspaces", firstWorkspace, freshWorkspace)
	}

	// The reply reopened the task and queued the fresh execution; because that
	// current execution then succeeded while the task was open, the task
	// settles to done (KODAMA.md task-state table). The answer and both
	// executions stay in the durable history.
	after := waitTaskState(t, harness, harness.ProjectToken, created.TaskID, "done")
	if after.State != "done" {
		t.Fatalf("task state after reply success = %q; want done", after.State)
	}
	if after.Question != nil {
		t.Fatalf("answered task still carries a question: %+v", after.Question)
	}
	if after.CurrentExecution != replied.ExecutionID {
		t.Fatalf("task current execution = %q; want %q", after.CurrentExecution, replied.ExecutionID)
	}
	if len(after.ExecutionHistory) != 2 || after.ExecutionHistory[0] != created.ExecutionID || after.ExecutionHistory[1] != replied.ExecutionID {
		t.Fatalf("task history = %v; want [%s %s]", after.ExecutionHistory, created.ExecutionID, replied.ExecutionID)
	}
	foundAnswer := false
	for _, message := range after.Messages {
		if message.Body == handoffAnswer && message.Author == factory.ReviewerPrincipal {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Fatalf("task conversation does not retain the reviewer answer: %+v", after.Messages)
	}
}

// testTaskMissingContext proves a fresh execution has no inherited scratch and
// fails its restoring action when the durable context referenced by the
// previous execution is missing, while /kodama/task.json still carries the
// answer.
func testTaskMissingContext(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-missing-context", missingContextPipeline(t, harness))
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "restore missing context",
		PipelineID: applied.PipelineID,
	}, "task-missing-create")

	waitExecutionRunning(t, harness, created.ExecutionID)
	readTask(t, harness, harness.ProjectToken, created.TaskID)
	askTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, api.TaskAskRequest{
		Question:   "Restore the workspace marker created by the previous attempt.",
		Assignment: "role/reviewer",
	}, "task-missing-ask")
	waiting := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question")
	}

	replied := replyTask(t, harness, harness.ReviewerToken, created.TaskID, &waiting.LifecycleVersion, api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  missingContextAnswer,
	}, "task-missing-reply")
	if replied.ExecutionID == created.ExecutionID {
		t.Fatalf("reply reused the cancelled execution %q", created.ExecutionID)
	}

	// The restoring action selects its answer branch from /kodama/task.json and
	// then requires the previous execution's /workspace marker. A fresh
	// workspace has no such marker, so the action fails.
	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	if fresh.State != "failed" {
		t.Fatalf("fresh execution state = %q; want failed because the inherited marker is absent", fresh.State)
	}
	if fresh.Failure == nil || fresh.Failure.Kind != "runtime" || fresh.Failure.Reason != "workload_exited" {
		t.Fatalf("fresh execution failure = %+v; want runtime/workload_exited", fresh.Failure)
	}
	action := actionByName(t, fresh, "restore")
	if action.State != "failed" {
		t.Fatalf("restoring action state = %q; want failed", action.State)
	}
	logs := executionLogs(t, harness, harness.ProjectToken, fresh.ExecutionID, "restore")
	if !strings.Contains(logs, "missing durable context: /workspace/marker") {
		t.Fatalf("retained logs do not name the missing durable context:\n%s", factory.SanitizeDiagnostic(logs))
	}

	firstWorkspace, err := harness.ExecutionWorkspaceID(ctx, created.ExecutionID)
	if err != nil {
		t.Fatalf("read first execution workspace: %v", err)
	}
	freshWorkspace, err := harness.ExecutionWorkspaceID(ctx, replied.ExecutionID)
	if err != nil {
		t.Fatalf("read fresh execution workspace: %v", err)
	}
	if firstWorkspace == "" || freshWorkspace == "" || firstWorkspace == freshWorkspace {
		t.Fatalf("workspace ids first=%q fresh=%q; want distinct workspaces", firstWorkspace, freshWorkspace)
	}

	// The failure is the restoring action's own missing context, not a task
	// admission failure: the task remains open and its history is preserved.
	after := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if after.State != "open" {
		t.Fatalf("task state after restoring failure = %q; want open", after.State)
	}
	if len(after.ExecutionHistory) != 2 {
		t.Fatalf("task history after restoring failure = %v; want two executions", after.ExecutionHistory)
	}
}

// testTaskCancel proves cancelling a waiting task settles the handoff, and a
// cancel of a running task cancels its nonterminal execution. A stale reply
// after cancellation cannot reopen the task.
func testTaskCancel(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-cancel", holdPipeline(t, harness))

	// Waiting-task cancellation: ask, then cancel the waiting task, then a
	// stale reply is refused.
	waitingTask := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "cancel a waiting task",
		PipelineID: applied.PipelineID,
	}, "task-cancel-waiting-create")
	waitExecutionRunning(t, harness, waitingTask.ExecutionID)
	readTask(t, harness, harness.ProjectToken, waitingTask.TaskID)
	askTaskCurrent(t, harness, harness.ProjectToken, waitingTask.TaskID, api.TaskAskRequest{
		Question:   "Answer before cancellation?",
		Assignment: "role/reviewer",
	}, "task-cancel-waiting-ask")
	waiting := readTask(t, harness, harness.ProjectToken, waitingTask.TaskID)
	if waiting.State != "waiting" || waiting.Question == nil {
		t.Fatalf("task is not waiting before cancel: %+v", waiting)
	}

	// A stale lifecycle version is refused without cancelling the task. This is
	// a direct, non-retrying call: the retrying cancel helper hides a product
	// that ignores the caller's lifecycle version.
	if waiting.LifecycleVersion <= 1 {
		t.Fatalf("waiting task lifecycle version = %d; want > 1", waiting.LifecycleVersion)
	}
	staleLifecycle := waiting.LifecycleVersion - 1
	_, staleCancelError := harness.REST(harness.ProjectToken, "task.cancel", api.TaskCancelRequest{TaskID: waitingTask.TaskID}, &staleLifecycle, "task-cancel-waiting-stale-lifecycle")
	if staleCancelError.Code != api.CodeVersionConflict {
		t.Fatalf("stale lifecycle task.cancel code = %q; want %q", staleCancelError.Code, api.CodeVersionConflict)
	}
	afterStaleCancel := readTask(t, harness, harness.ProjectToken, waitingTask.TaskID)
	if afterStaleCancel.State != "waiting" || afterStaleCancel.Question == nil ||
		afterStaleCancel.Question.QuestionID != waiting.Question.QuestionID ||
		afterStaleCancel.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("stale lifecycle task.cancel changed the waiting task: %+v", afterStaleCancel)
	}
	staleCancelCount, staleCancelCountErr := harness.ExecutionCount(ctx, applied.PipelineID)
	if staleCancelCountErr != nil {
		t.Fatalf("count executions after stale cancel: %v", staleCancelCountErr)
	}
	if staleCancelCount != 1 {
		t.Fatalf("stale lifecycle task.cancel queued executions: count=%d want=1", staleCancelCount)
	}

	cancelTaskCurrent(t, harness, harness.ProjectToken, waitingTask.TaskID, "task-cancel-waiting")
	cancelledTask := readTask(t, harness, harness.ProjectToken, waitingTask.TaskID)
	if cancelledTask.State != "cancelled" {
		t.Fatalf("task state after cancel = %q; want cancelled", cancelledTask.State)
	}
	if cancelledTask.Question != nil {
		t.Fatalf("cancelled task still carries a question: %+v", cancelledTask.Question)
	}
	waitExecution(t, harness, harness.ProjectToken, waitingTask.ExecutionID)
	assertExecutionResourcesReleased(t, harness, waitingTask.ExecutionID)

	_, publicError := harness.REST(harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  waitingTask.TaskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  handoffAnswer,
	}, &cancelledTask.LifecycleVersion, "task-cancel-stale-reply")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale reply after cancel code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	count, err := harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after stale reply: %v", err)
	}
	if count != 1 {
		t.Fatalf("stale reply after cancel queued %d executions; want 1 total", count)
	}

	// Running-execution cancellation: a task cancel cancels its nonterminal
	// execution, clears the execution credential and settles cleanup.
	runningTask := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "cancel a running task",
		PipelineID: applied.PipelineID,
	}, "task-cancel-running-create")
	waitExecutionRunning(t, harness, runningTask.ExecutionID)
	readTask(t, harness, harness.ProjectToken, runningTask.TaskID)
	runningCredential := waitExecutionCredential(t, harness, runningTask.ExecutionID, "hold")
	if _, credentialError := harness.REST(runningCredential, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: runningTask.ExecutionID}, nil, ""); credentialError.Code != "" {
		t.Fatalf("running execution credential was refused before task.cancel: %s (%s)", credentialError.Code, credentialError.Message)
	}
	nonceBefore, err := harness.ExecutionCredentialNoncePresent(ctx, runningTask.ExecutionID)
	if err != nil {
		t.Fatalf("read credential nonce before task.cancel: %v", err)
	}
	if !nonceBefore {
		t.Fatalf("positive control: running execution has no stored credential nonce")
	}
	cancelTaskCurrent(t, harness, harness.ProjectToken, runningTask.TaskID, "task-cancel-running")
	cancelledExecution := waitExecution(t, harness, harness.ProjectToken, runningTask.ExecutionID)
	if cancelledExecution.State != "cancelled" {
		t.Fatalf("cancel of a running task left execution state %q; want cancelled", cancelledExecution.State)
	}
	assertExecutionResourcesReleased(t, harness, runningTask.ExecutionID)

	// The task.cancel path must revoke the credential, not merely leave a
	// terminal execution whose principal the edge disables. The durable nonce
	// probe is the decisive evidence; the edge refusal is the observable effect.
	nonceAfter, err := harness.ExecutionCredentialNoncePresent(ctx, runningTask.ExecutionID)
	if err != nil {
		t.Fatalf("read credential nonce after task.cancel: %v", err)
	}
	if nonceAfter {
		t.Fatalf("task.cancel left the execution credential nonce stored; the terminal state alone also refuses authentication")
	}
	if _, credentialError := harness.REST(runningCredential, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: runningTask.ExecutionID}, nil, ""); credentialError.Code != api.CodeUnauthenticated {
		t.Fatalf("credential after task.cancel code = %q; want %q", credentialError.Code, api.CodeUnauthenticated)
	}
}

// testTaskReceiptParity proves REST and MCP retries of task.create_and_start
// and task.reply with the same idempotency key return the same identities and
// commit exactly one receipt each.
func testTaskReceiptParity(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyPipeline(t, harness, "task-receipts", replyReceiptPipeline(t, harness))

	createRequest := api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "receipt parity",
		PipelineID: applied.PipelineID,
	}
	created := createAndStartTask(t, harness, harness.ProjectToken, createRequest, "task-receipt-create")
	createReplay := harness.MustMCP(harness.ProjectToken, "task.create_and_start", createRequest, nil, "task-receipt-create")
	var replayedCreate api.TaskCreateAndStartResult
	harness.Decode(createReplay, &replayedCreate)
	if replayedCreate.TaskID != created.TaskID || replayedCreate.ExecutionID != created.ExecutionID {
		t.Fatalf("create_and_start MCP replay = %+v; want %+v", replayedCreate, created)
	}
	receipts, err := harness.ReceiptCount(ctx, "task-receipt-create")
	if err != nil {
		t.Fatalf("count create_and_start receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("create_and_start receipt count = %d; want exactly 1", receipts)
	}

	// A retry with the same idempotency key but different content must be
	// refused by the content fingerprint over both edges: a constant fingerprint
	// would silently replay the first task for a different request.
	tasksBefore, tasksBeforeErr := harness.TaskCount(ctx)
	if tasksBeforeErr != nil {
		t.Fatalf("count tasks before changed-content create_and_start: %v", tasksBeforeErr)
	}
	changedCreate := createRequest
	changedCreate.Title = "receipt parity changed title"
	_, createContentError := harness.REST(harness.ProjectToken, "task.create_and_start", changedCreate, nil, "task-receipt-create")
	if createContentError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed-content create_and_start REST code = %q; want %q", createContentError.Code, api.CodeChangedIdempotencyContent)
	}
	_, createMCPError, createMCPErr := harness.MCP(harness.ProjectToken, "task.create_and_start", changedCreate, nil, "task-receipt-create")
	if createMCPErr != nil {
		t.Fatalf("changed-content create_and_start over MCP: %v", createMCPErr)
	}
	if createMCPError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed-content create_and_start MCP code = %q; want %q", createMCPError.Code, api.CodeChangedIdempotencyContent)
	}
	createExecutions, createCountErr := harness.ExecutionCount(ctx, applied.PipelineID)
	if createCountErr != nil {
		t.Fatalf("count executions after changed-content create_and_start: %v", createCountErr)
	}
	if createExecutions != 1 {
		t.Fatalf("changed-content create_and_start queued executions: count=%d want=1", createExecutions)
	}
	taskCount, taskCountErr := harness.TaskCount(ctx)
	if taskCountErr != nil {
		t.Fatalf("count tasks after changed-content create_and_start: %v", taskCountErr)
	}
	if taskCount != tasksBefore {
		t.Fatalf("changed-content create_and_start created a task: before=%d after=%d", tasksBefore, taskCount)
	}
	receipts, err = harness.ReceiptCount(ctx, "task-receipt-create")
	if err != nil {
		t.Fatalf("count create_and_start receipts after changed-content retry: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("changed-content create_and_start receipt count = %d; want exactly 1", receipts)
	}

	waitExecutionRunning(t, harness, created.ExecutionID)
	readTask(t, harness, harness.ProjectToken, created.TaskID)
	askTaskCurrent(t, harness, harness.ProjectToken, created.TaskID, api.TaskAskRequest{
		Question:   "Acknowledge receipt parity?",
		Assignment: "role/reviewer",
	}, "task-receipt-ask")
	waiting := readTask(t, harness, harness.ProjectToken, created.TaskID)
	if waiting.Question == nil {
		t.Fatalf("waiting task has no outstanding question")
	}

	replyRequest := api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  receiptAnswer,
	}
	replied := replyTask(t, harness, harness.ReviewerToken, created.TaskID, &waiting.LifecycleVersion, replyRequest, "task-receipt-reply")
	// The reply pipeline keeps the fresh execution running after it reads the
	// answer, so the replay still observes an open task.
	replyReplay := harness.MustMCP(harness.ReviewerToken, "task.reply", replyRequest, &waiting.LifecycleVersion, "task-receipt-reply")
	var replayedReply api.TaskReplyResult
	harness.Decode(replyReplay, &replayedReply)
	if replayedReply.ExecutionID != replied.ExecutionID || replayedReply.LifecycleVersion != replied.LifecycleVersion {
		t.Fatalf("task.reply MCP replay = %+v; want %+v", replayedReply, replied)
	}
	receipts, err = harness.ReceiptCount(ctx, "task-receipt-reply")
	if err != nil {
		t.Fatalf("count task.reply receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("task.reply receipt count = %d; want exactly 1", receipts)
	}
	count, err := harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after reply replay: %v", err)
	}
	if count != 2 {
		t.Fatalf("reply replay queued %d executions; want 2 total (initial and fresh)", count)
	}

	// A reply retry with the same key but a different answer must be refused by
	// the content fingerprint over both edges. The expected lifecycle version is
	// unchanged, so the answer is the only differing input.
	tasksBefore, err = harness.TaskCount(ctx)
	if err != nil {
		t.Fatalf("count tasks before changed-answer reply: %v", err)
	}
	changedReply := replyRequest
	changedReply.Answer = receiptAnswer + "-changed"
	_, replyContentError := harness.REST(harness.ReviewerToken, "task.reply", changedReply, &waiting.LifecycleVersion, "task-receipt-reply")
	if replyContentError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed-answer task.reply REST code = %q; want %q", replyContentError.Code, api.CodeChangedIdempotencyContent)
	}
	_, replyMCPError, replyMCPErr := harness.MCP(harness.ReviewerToken, "task.reply", changedReply, &waiting.LifecycleVersion, "task-receipt-reply")
	if replyMCPErr != nil {
		t.Fatalf("changed-answer task.reply over MCP: %v", replyMCPErr)
	}
	if replyMCPError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed-answer task.reply MCP code = %q; want %q", replyMCPError.Code, api.CodeChangedIdempotencyContent)
	}
	count, err = harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after changed-answer reply: %v", err)
	}
	if count != 2 {
		t.Fatalf("changed-answer reply queued executions: count=%d want=2", count)
	}
	receipts, err = harness.ReceiptCount(ctx, "task-receipt-reply")
	if err != nil {
		t.Fatalf("count task.reply receipts after changed-answer retry: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("changed-answer task.reply receipt count = %d; want exactly 1", receipts)
	}
	taskCount, err = harness.TaskCount(ctx)
	if err != nil {
		t.Fatalf("count tasks after changed-answer reply: %v", err)
	}
	if taskCount != tasksBefore {
		t.Fatalf("changed-answer reply created a task: before=%d after=%d", tasksBefore, taskCount)
	}

	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	requireSucceeded(t, fresh)
}

// --- task operation helpers -------------------------------------------------

func createTask(t *testing.T, harness *factory.Harness, token string, request api.TaskCreateRequest, key string) api.TaskCreateResult {
	t.Helper()
	result := harness.MustREST(token, "task.create", request, nil, key)
	var created api.TaskCreateResult
	harness.Decode(result, &created)
	return created
}

func createAndStartTask(t *testing.T, harness *factory.Harness, token string, request api.TaskCreateAndStartRequest, key string) api.TaskCreateAndStartResult {
	t.Helper()
	result := harness.MustREST(token, "task.create_and_start", request, nil, key)
	var created api.TaskCreateAndStartResult
	harness.Decode(result, &created)
	return created
}

func readTask(t *testing.T, harness *factory.Harness, token, taskID string) api.TaskReadResult {
	t.Helper()
	result := harness.MustREST(token, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, "")
	var task api.TaskReadResult
	harness.Decode(result, &task)
	return task
}

func startTask(t *testing.T, harness *factory.Harness, token, taskID string, expected *uint64, key string) api.TaskStartResult {
	t.Helper()
	result := harness.MustREST(token, "task.start", api.TaskStartRequest{TaskID: taskID}, expected, key)
	var started api.TaskStartResult
	harness.Decode(result, &started)
	return started
}

// askTaskCurrent asks from the latest task lifecycle version. The caller's
// version guards only the task row; the execution version is an internal
// authority re-read, so a concurrent activation/start commit can refuse a
// legitimate ask with version_conflict. Nothing is committed by the refusal,
// so re-reading the task and retrying with the same idempotency key is safe.
func askTaskCurrent(t *testing.T, harness *factory.Harness, token, taskID string, request api.TaskAskRequest, key string) api.TaskAskResult {
	t.Helper()
	for range 20 {
		current := readTask(t, harness, token, taskID)
		request.TaskID = taskID
		result, publicError := harness.REST(token, "task.ask", request, &current.LifecycleVersion, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.ask refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var asked api.TaskAskResult
		harness.Decode(result, &asked)
		return asked
	}
	t.Fatalf("task.ask did not converge on a current execution version")
	return api.TaskAskResult{}
}

func replyTask(t *testing.T, harness *factory.Harness, token, taskID string, expected *uint64, request api.TaskReplyRequest, key string) api.TaskReplyResult {
	t.Helper()
	request.TaskID = taskID
	result := harness.MustREST(token, "task.reply", request, expected, key)
	var replied api.TaskReplyResult
	harness.Decode(result, &replied)
	return replied
}

// cancelTaskCurrent cancels from the latest task lifecycle version. The same
// internal execution-version race as task.ask is retried from a fresh read;
// the refusal commits nothing.
func cancelTaskCurrent(t *testing.T, harness *factory.Harness, token, taskID, key string) api.TaskCancelResult {
	t.Helper()
	for range 20 {
		current := readTask(t, harness, token, taskID)
		result, publicError := harness.REST(token, "task.cancel", api.TaskCancelRequest{TaskID: taskID}, &current.LifecycleVersion, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var cancelled api.TaskCancelResult
		harness.Decode(result, &cancelled)
		return cancelled
	}
	t.Fatalf("task.cancel did not converge on a current execution version")
	return api.TaskCancelResult{}
}

// inboxWaitingTasks reads only the waiting_task inbox kind for one principal.
func inboxWaitingTasks(t *testing.T, harness *factory.Harness, token string) api.TaskInboxResult {
	t.Helper()
	result := harness.MustREST(token, "task.inbox", api.TaskInboxRequest{
		ProjectID: factory.ProjectID,
		Kinds:     []string{"waiting_task"},
	}, nil, "")
	var inbox api.TaskInboxResult
	harness.Decode(result, &inbox)
	return inbox
}

// findWaitingTask returns the waiting_task item for one exact task.
func findWaitingTask(inbox api.TaskInboxResult, taskID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "waiting_task" && item.TaskID == taskID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// handoffInboxResult reads one inbox page over REST or MCP. A nil kinds list
// omits the member, which asks for every kind.
func handoffInboxResult(t *testing.T, harness *factory.Harness, token string, mcp bool, kinds []string) api.TaskInboxResult {
	t.Helper()
	request := api.TaskInboxRequest{ProjectID: factory.ProjectID, Kinds: kinds}
	var result api.Result
	if mcp {
		result = harness.MustMCP(token, "task.inbox", request, nil, "")
	} else {
		result = harness.MustREST(token, "task.inbox", request, nil, "")
	}
	var inbox api.TaskInboxResult
	harness.Decode(result, &inbox)
	return inbox
}

// handoffFindFailedTask returns the failed_task item for one exact task.
func handoffFindFailedTask(inbox api.TaskInboxResult, taskID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "failed_task" && item.TaskID == taskID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// handoffAssertFailedTaskItem proves the failed_task shape: only the task
// identity, the task lifecycle version and a nonzero execution timestamp; no
// execution/action identity and no question projection.
func handoffAssertFailedTaskItem(t *testing.T, item api.InboxItem, lifecycleVersion uint64) {
	t.Helper()
	if item.Kind != "failed_task" || item.TaskID == "" {
		t.Fatalf("failed_task identity = %+v", item)
	}
	if item.ExecutionID != "" || item.ActionID != "" {
		t.Fatalf("failed_task carries an execution/action identity: %+v", item)
	}
	if item.Assignment != "" {
		t.Fatalf("failed_task carries an assignment %q; want none", item.Assignment)
	}
	if item.Version != lifecycleVersion {
		t.Fatalf("failed_task Version = %d; want the task lifecycle version %d", item.Version, lifecycleVersion)
	}
	if item.At.IsZero() {
		t.Fatalf("failed_task At is zero; want the failed execution's timestamp")
	}
	if item.QuestionID != "" || item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 {
		t.Fatalf("failed_task carries a question projection: %+v", item)
	}
}

// assertWaitingUnchanged proves a refused reply committed nothing: the task is
// still waiting on the same question at the same lifecycle version and no
// execution was queued.
func assertWaitingUnchanged(t *testing.T, harness *factory.Harness, taskID string, before api.TaskReadResult, pipelineID string, wantExecutions int64) {
	t.Helper()
	after := readTask(t, harness, harness.ProjectToken, taskID)
	if after.State != "waiting" || after.Question == nil ||
		after.Question.QuestionID != before.Question.QuestionID ||
		after.LifecycleVersion != before.LifecycleVersion {
		t.Fatalf("refused reply changed the waiting task: before=%+v after=%+v", before, after)
	}
	count, err := harness.ExecutionCount(t.Context(), pipelineID)
	if err != nil {
		t.Fatalf("count executions after refused reply: %v", err)
	}
	if count != wantExecutions {
		t.Fatalf("refused reply queued executions: count=%d want=%d", count, wantExecutions)
	}
}

// waitTaskState waits until one task reaches the given durable state and
// returns that observation. Settlement is a controller scan separate from the
// execution's terminal commit, so this is a barrier, never a fixed delay.
func waitTaskState(t *testing.T, harness *factory.Harness, token, taskID, state string) api.TaskReadResult {
	t.Helper()
	var last api.TaskReadResult
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(token, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		return last.State == state
	})
	return last
}

// waitExecutionRunning waits until one execution is running with an active
// action and returns that observation.
func waitExecutionRunning(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		return last.State == "running" && last.CurrentAction >= 0
	})
	return last
}

// waitExecutionCredential waits until the composition has materialized the
// execution credential for the running workload. Activation commits the
// running action before the workload is prepared, so the file is a separate
// effect; this barrier never assumes a fixed delay.
func waitExecutionCredential(t *testing.T, harness *factory.Harness, executionID, actionName string) string {
	t.Helper()
	var credential string
	harness.Eventually(t, 2*time.Minute, func() bool {
		value, err := harness.ExecutionCredential(t.Context(), executionID, actionName)
		if err != nil {
			return false
		}
		credential = value
		return value != ""
	})
	return credential
}

// executionLogs concatenates the retained stdout and stderr of one action.
func executionLogs(t *testing.T, harness *factory.Harness, token, executionID, actionName string) string {
	t.Helper()
	execution := inspectExecution(t, harness, token, executionID)
	action := actionByName(t, execution, actionName)
	var builder strings.Builder
	for _, stream := range []string{"stdout", "stderr"} {
		result := harness.MustREST(token, "execution.logs.read", api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    action.ActionID,
			Stream:      stream,
		}, nil, "")
		var read api.ExecutionLogsReadResult
		harness.Decode(result, &read)
		for _, chunk := range read.Chunks {
			_, _ = builder.Write(chunk.Data)
		}
	}
	return builder.String()
}

// --- pipeline builders ------------------------------------------------------

// holdPipeline is one container action that records a workspace marker and
// then stays running until the handoff cancels it.
func holdPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "hold",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{`printf 'workspace\n' > /workspace/marker
sleep 300`},
		}},
	})
}

// handoffPipeline is one restoring action that succeeds only when
// /kodama/task.json carries the handoff answer. The first execution has no
// answer and stays running until the ask cancels it.
func handoffPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "restore",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{fmt.Sprintf(`if grep -q %q /kodama/task.json 2>/dev/null; then
  printf 'restored\n' > /workspace/restored
  exit 0
fi
printf 'workspace\n' > /workspace/marker
sleep 300`, handoffAnswer)},
		}},
	})
}

// missingContextPipeline selects the restoring branch from the answer in
// /kodama/task.json and then requires the previous execution's workspace
// marker, which a fresh workspace cannot contain.
func missingContextPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "restore",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{fmt.Sprintf(`if grep -q %q /kodama/task.json 2>/dev/null; then
  test -f /workspace/marker || { echo "missing durable context: /workspace/marker" >&2; exit 1; }
  exit 0
fi
printf 'workspace\n' > /workspace/marker
sleep 300`, missingContextAnswer)},
		}},
	})
}

// replyReceiptPipeline keeps the post-reply execution running long enough for
// the MCP replay to observe the same open task.
func replyReceiptPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "restore",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{fmt.Sprintf(`if grep -q %q /kodama/task.json 2>/dev/null; then
  sleep 8
  exit 0
fi
sleep 300`, receiptAnswer)},
		}},
	})
}

// retryPipeline always fails so a retry can be proven to start a fresh
// execution without replaying the failed action.
func retryPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "work",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{`echo "attempt failed" >&2
exit 1`},
		}},
	})
}
