package app

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
)

// TestTaskRestartActivatesReplacementAfterPredecessorStopSettles pins that
// task.restart cancels an actively running predecessor execution with
// CancelSuperseded, holds the queued replacement until the predecessor stop
// settles, and then lets the controller's Scan/Reconcile loop automatically
// activate and complete the replacement execution.
func TestTaskRestartActivatesReplacementAfterPredecessorStopSettles(t *testing.T) {
	fixture := newActivationFixture(t, nil, "work", "retry-work")
	fixture.runtime.pauseExits()
	application := fixture.environment.startApp(t)
	handler := application.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")

	stageOne := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
		activationAction{name: "work"},
	), "restart-stage-one")
	stageTwo := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
		activationAction{name: "retry-work"},
	), "restart-stage-two")

	// 1. Start initial execution and wait until its workload is prepared and running.
	createRec, createRes, createErr := restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", "dev-project-token",
		api.TaskCreateAndStartRequest{
			Title:      "Restart reconciliation test",
			PipelineID: stageOne.PipelineID,
		}, nil, "restart-reconcile-create")
	if createRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", createRec.Code, createErr)
	}
	created := decodeBody[api.TaskCreateAndStartResult](t, createRes)

	waitForActivationPrepares(t, fixture.runtime, 1)

	// 2. Restart the task atomically onto stageTwo while stageOne is still running.
	expectedLifecycle := created.LifecycleVersion
	restartRec, restartRes, restartErr := restCall(t, handler, http.MethodPost,
		"/api/v1/tasks/"+created.TaskID+"/restart", "dev-project-token",
		api.TaskRestartRequest{
			Reason:     "superseded by fresh attempt",
			PipelineID: stageTwo.PipelineID,
		}, &expectedLifecycle, "restart-reconcile-restart")
	if restartRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.restart status=%d error=%+v", restartRec.Code, restartErr)
	}
	restarted := decodeBody[api.TaskRestartResult](t, restartRes)
	if restarted.SupersededExecution != created.ExecutionID {
		t.Fatalf("superseded_execution = %q; want %q", restarted.SupersededExecution, created.ExecutionID)
	}
	if restarted.ExecutionID == "" || restarted.ExecutionID == created.ExecutionID {
		t.Fatalf("replacement execution_id = %q; want distinct execution", restarted.ExecutionID)
	}

	// 3. Release paused runtime exits so predecessor stop + cleanup and replacement
	//    execution can run to completion via the real Scan/Reconcile loop.
	fixture.runtime.releaseExits()

	deadline := time.Now().Add(10 * time.Second)
	var oldView, newView api.ExecutionInspectResult
	for time.Now().Before(deadline) {
		oldRec, oldRes, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+created.ExecutionID, "dev-project-token",
			api.ExecutionInspectRequest{}, nil, "")
		newRec, newRes, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+restarted.ExecutionID, "dev-project-token",
			api.ExecutionInspectRequest{}, nil, "")
		if oldRec.Code == api.HTTPStatusSuccess && newRec.Code == api.HTTPStatusSuccess {
			oldView = decodeBody[api.ExecutionInspectResult](t, oldRes)
			newView = decodeBody[api.ExecutionInspectResult](t, newRes)
			if oldView.State == "cancelled" && oldView.Cleanup.State == "complete" &&
				newView.State == "succeeded" && newView.Cleanup.State == "complete" {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	if oldView.State != "cancelled" || oldView.Cleanup.State != "complete" {
		t.Fatalf("predecessor execution = state:%q cleanup:%q; want cancelled/complete", oldView.State, oldView.Cleanup.State)
	}
	if oldView.Failure == nil || oldView.Failure.Reason != "superseded" {
		t.Fatalf("predecessor failure = %+v; want reason=superseded", oldView.Failure)
	}
	if newView.State != "succeeded" || newView.Cleanup.State != "complete" {
		t.Fatalf("replacement execution = state:%q cleanup:%q; want succeeded/complete", newView.State, newView.Cleanup.State)
	}

	// Confirm the task settled to done with current_execution = replacement and both executions in history.
	readRec, readRes, readErr := restCall(t, handler, http.MethodGet,
		"/api/v1/tasks/"+created.TaskID, "dev-project-token",
		api.TaskReadRequest{}, nil, "")
	if readRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.read status=%d error=%+v", readRec.Code, readErr)
	}
	taskView := decodeBody[api.TaskReadResult](t, readRes)
	if taskView.State != "done" || taskView.CurrentExecution != restarted.ExecutionID {
		t.Fatalf("task view = state:%q current:%q; want done/%q", taskView.State, taskView.CurrentExecution, restarted.ExecutionID)
	}
	if len(taskView.ExecutionHistory) != 2 || taskView.ExecutionHistory[0] != created.ExecutionID || taskView.ExecutionHistory[1] != restarted.ExecutionID {
		t.Fatalf("task execution history = %v; want [%s %s]", taskView.ExecutionHistory, created.ExecutionID, restarted.ExecutionID)
	}
}
