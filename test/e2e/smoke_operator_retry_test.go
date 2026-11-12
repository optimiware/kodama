//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// testReleaseSmokeOperatorRetry is S4. A task-free pipeline fails its first
// execution with a deliberate non-zero exit; the engineer appends an explicit
// durable repair comment; the operator-only persona discovers the failed task
// through its inbox and starts a fresh attempt with task.start. The retry runs
// from action zero in a fresh workspace, preserves the failed history, and an
// identical start replay converges on the same second execution.
func testReleaseSmokeOperatorRetry(t *testing.T, instance *live, names smoke.Names, probe string, recordScenario bool) api.ExecutionInspectResult {
	t.Helper()
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	operator := instance.smokeToken(t, harness.PersonaOperator)
	ca := instance.materializeCA(t)

	repairMarker := names.Marker("repair")
	successMarker := names.Marker("retry-succeeded")
	failureMarker := names.Marker("retry-failed")
	pipelineName := "w49-" + names.Run + "-operator-retry"
	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "work",
			Image:   instance.image,
			Timeout: "5m",
			Env: smokeMergeEnv(ca.environment(), map[string]string{
				"SMOKE_REPAIR_MARKER":  repairMarker,
				"SMOKE_SUCCESS_MARKER": successMarker,
				"SMOKE_FAILURE_MARKER": failureMarker,
			}),
			Commands: ca.commands(operatorRetryWorkScript),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the S4 operator-retry pipeline: %v", err)
	}
	applied := instance.smokeApplyPipeline(ctx, t, engineer, pipelineName, document, names.Key("apply"))
	created := instance.smokeTaskCreateAndStart(ctx, t, engineer, api.TaskCreateAndStartRequest{
		ProjectID:  instance.projectID,
		Title:      "W49 operator retry " + names.Run,
		PipelineID: applied.PipelineID,
	}, names.Key("create"))
	t.Cleanup(func() {
		instance.smokeCleanupTask(t, created.TaskID, created.ExecutionID)
	})

	instance.smokeWaitComplete(ctx, t, engineer, created.ExecutionID, 6*time.Minute)
	first := instance.smokeStableInspect(ctx, t, engineer, created.ExecutionID)
	if first.State != "failed" {
		t.Fatalf("first attempt state = %q; want failed", first.State)
	}
	if first.Failure == nil || first.Failure.Kind != "runtime" || first.Failure.Reason != "workload_exited" {
		t.Fatalf("first attempt failure = %+v; want runtime/workload_exited, not a preparation or authentication failure", first.Failure)
	}
	firstAction := actionByName(t, first, "work")
	if firstAction.State != "failed" {
		t.Fatalf("first work action state = %q; want failed", firstAction.State)
	}
	firstLog := instance.smokeReadLogs(ctx, t, engineer, created.ExecutionID, firstAction.ActionID, "stdout")
	if err := smoke.CheckMarker(firstLog, failureMarker, 1); err != nil {
		t.Fatalf("deliberate failure marker: %v", err)
	}

	task := instance.smokeTaskRead(ctx, t, engineer, created.TaskID)
	if task.State != "open" {
		t.Fatalf("failed task state = %q; want open", task.State)
	}
	if task.CurrentExecution != created.ExecutionID {
		t.Fatalf("failed task current execution = %q; want %q", task.CurrentExecution, created.ExecutionID)
	}
	if err := smoke.CheckExactHistory(task.ExecutionHistory, created.ExecutionID); err != nil {
		t.Fatalf("failed task history: %v", err)
	}
	if task.Question != nil {
		t.Fatalf("a failed task carries an automatic question: %+v", task.Question)
	}

	operatorInbox := instance.smokeTaskInbox(ctx, t, operator, "failed_task")
	discovered, ok := smokeFindFailedTask(operatorInbox, created.TaskID)
	if !ok {
		t.Fatalf("operator inbox has no failed_task for %q: %+v", created.TaskID, operatorInbox.Items)
	}
	if discovered.Version != task.LifecycleVersion {
		t.Fatalf("failed_task version = %d; want the task lifecycle version %d", discovered.Version, task.LifecycleVersion)
	}
	// The operator acts only through the inbox and the conditional start: it
	// cannot read the task directly.
	instance.smokeRequireForbidden(ctx, t, operator, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: created.TaskID})

	commented := instance.smokeTaskComment(ctx, t, engineer, api.TaskCommentRequest{
		TaskID: created.TaskID,
		Body:   repairMarker,
	}, &task.ContentVersion, names.Key("comment"))
	if commented.ContentVersion <= task.ContentVersion {
		t.Fatalf("comment content version = %d; want > %d", commented.ContentVersion, task.ContentVersion)
	}

	refreshedInbox := instance.smokeTaskInbox(ctx, t, operator, "failed_task")
	retryItem, ok := smokeFindFailedTask(refreshedInbox, created.TaskID)
	if !ok {
		t.Fatalf("operator inbox has no failed_task after the repair comment: %+v", refreshedInbox.Items)
	}

	startRequest := api.TaskStartRequest{TaskID: created.TaskID}
	retried := instance.smokeTaskStart(ctx, t, operator, startRequest, &retryItem.Version, names.Key("start"))
	if retried.ExecutionID == created.ExecutionID {
		t.Fatalf("operator retry reused the failed execution %q", created.ExecutionID)
	}
	replayed := instance.smokeTaskStart(ctx, t, operator, startRequest, &retryItem.Version, names.Key("start"))
	if err := smoke.CheckReplayConverged(retried, replayed); err != nil {
		t.Fatalf("identical task.start replay: %v", err)
	}

	second := instance.smokeWaitComplete(ctx, t, engineer, retried.ExecutionID, 6*time.Minute)
	requireSucceeded(t, second)
	secondAction := actionByName(t, second, "work")
	secondLog := instance.smokeReadLogs(ctx, t, engineer, retried.ExecutionID, secondAction.ActionID, "stdout")
	if err := smoke.CheckMarker(secondLog, successMarker, 1); err != nil {
		t.Fatalf("retry success marker: %v", err)
	}

	if err := smoke.CheckDistinctWorkspaces(first.Cleanup.WorkspaceID, second.Cleanup.WorkspaceID); err != nil {
		t.Fatalf("retry workspace: %v", err)
	}
	if err := smoke.CheckFreshActionIDs(first, second); err != nil {
		t.Fatalf("retry actions: %v", err)
	}

	unchanged := instance.smokeStableInspect(ctx, t, engineer, created.ExecutionID)
	if unchanged.State != first.State || unchanged.Version != first.Version {
		t.Fatalf("retry changed the failed execution: before state=%q version=%d after state=%q version=%d",
			first.State, first.Version, unchanged.State, unchanged.Version)
	}
	if unchanged.Failure == nil || first.Failure == nil || *unchanged.Failure != *first.Failure {
		t.Fatalf("retry changed the failed execution failure: before=%+v after=%+v", first.Failure, unchanged.Failure)
	}
	unchangedAction := actionByName(t, unchanged, "work")
	if unchangedAction.ActionID != firstAction.ActionID || unchangedAction.State != "failed" || unchangedAction.Version != firstAction.Version {
		t.Fatalf("retry changed the failed action: before=%+v after=%+v", firstAction, unchangedAction)
	}

	finalTask := instance.smokeWaitTaskState(ctx, t, engineer, created.TaskID, "done", 30*time.Second)
	if finalTask.CurrentExecution != retried.ExecutionID {
		t.Fatalf("task current execution = %q; want %q", finalTask.CurrentExecution, retried.ExecutionID)
	}
	if err := smoke.CheckExactHistory(finalTask.ExecutionHistory, created.ExecutionID, retried.ExecutionID); err != nil {
		t.Fatalf("task history after the retry: %v", err)
	}

	finished := time.Now().UTC()
	if recordScenario {
		instance.recordScenario(t, "OperatorRetry", probe, &second, started, finished, map[string]any{
			"task_id":      created.TaskID,
			"action_id":    secondAction.ActionID,
			"workspace_id": second.Cleanup.WorkspaceID,
		})
	}
	return second
}
