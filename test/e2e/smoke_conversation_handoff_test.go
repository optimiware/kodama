//go:build e2e

package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// testReleaseSmokeConversationHandoff is S3. A live task action waits for an
// unplanned decision; the engineer asks the assigned reviewer with an ordinary
// durable reference the product accepts without Git; the reviewer answers from
// its own inbox; the cancelled execution is replaced by a distinct fresh
// execution whose /kodama/task.json carries the answer and whose workspace does
// not inherit the cancelled attempt's scratch.
func testReleaseSmokeConversationHandoff(t *testing.T, instance *live, names smoke.Names, probe string, recordScenario bool) api.ExecutionInspectResult {
	t.Helper()
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	reviewer := instance.smokeToken(t, harness.PersonaReviewer)
	operator := instance.smokeToken(t, harness.PersonaOperator)
	reviewerPrincipal := smokePersonaPrincipal(t, instance, harness.PersonaReviewer)
	ca := instance.materializeCA(t)

	answer := names.Answer()
	reference := names.Reference()
	restorationMarker := names.Marker("restored")
	readyMarker := names.Marker("ready")
	pipelineName := "w49-" + names.Run + "-conversation-handoff"
	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "live",
			Image:   instance.image,
			Timeout: "10m",
			Env: smokeMergeEnv(ca.environment(), map[string]string{
				"SMOKE_ANSWER":             answer,
				"SMOKE_RESTORATION_MARKER": restorationMarker,
				"SMOKE_READY_MARKER":       readyMarker,
				"SMOKE_READY_KEY":          names.Key("ready"),
				"SMOKE_PROJECT":            instance.projectID,
			}),
			Commands: ca.commands(conversationHandoffLiveScript),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the S3 conversation-handoff pipeline: %v", err)
	}
	applied := instance.smokeApplyPipeline(ctx, t, engineer, pipelineName, document, names.Key("apply"))
	created := instance.smokeTaskCreateAndStart(ctx, t, engineer, api.TaskCreateAndStartRequest{
		ProjectID:  instance.projectID,
		Title:      "W49 conversation handoff " + names.Run,
		PipelineID: applied.PipelineID,
	}, names.Key("create"))
	t.Cleanup(func() {
		instance.smokeCleanupTask(t, created.TaskID, created.ExecutionID)
	})

	instance.smokeWaitActionRunning(ctx, t, engineer, created.ExecutionID, "live", 6*time.Minute)
	// Retained logs are collected when a workload is removed, so a running
	// action has no execution.logs.read output. The action publishes a custom
	// observation after it writes the scratch; that committed event is the
	// public readiness proof. Its retained log is asserted after cancellation.
	ready := instance.smokeWaitEvent(ctx, t, engineer, readyMarker, created.ExecutionID, 3*time.Minute)
	if ready.Source.Kind != "execution" || ready.Source.PrincipalID == "" {
		t.Fatalf("readiness event source = %+v; want a nonempty execution principal", ready.Source)
	}

	task := instance.smokeTaskRead(ctx, t, engineer, created.TaskID)
	if task.State != "open" || task.CurrentExecution != created.ExecutionID {
		t.Fatalf("running task = state %q current %q; want open/%q", task.State, task.CurrentExecution, created.ExecutionID)
	}

	asked := instance.smokeTaskAsk(ctx, t, engineer, api.TaskAskRequest{
		TaskID:     created.TaskID,
		Question:   "Continue with this committed context?",
		Assignment: "role/reviewer",
		References: []string{reference},
	}, &task.LifecycleVersion, names.Key("ask"))
	waiting := instance.smokeTaskRead(ctx, t, engineer, created.TaskID)
	if waiting.State != "waiting" {
		t.Fatalf("task state after ask = %q; want waiting", waiting.State)
	}
	if waiting.Question == nil {
		t.Fatal("waiting task carries no outstanding question")
	}
	if asked.QuestionID == "" || waiting.Question.QuestionID != asked.QuestionID {
		t.Fatalf("waiting question = %q; want the asked %q", waiting.Question.QuestionID, asked.QuestionID)
	}
	if waiting.Question.Version != asked.QuestionVersion {
		t.Fatalf("waiting question version = %d; want %d", waiting.Question.Version, asked.QuestionVersion)
	}
	if !slices.Equal(waiting.Question.References, []string{reference}) {
		t.Fatalf("waiting question references = %v; want [%s]", waiting.Question.References, reference)
	}

	instance.smokeWaitComplete(ctx, t, engineer, created.ExecutionID, 6*time.Minute)
	cancelled := instance.smokeStableInspect(ctx, t, engineer, created.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("handoff cancelled execution state = %q; want cancelled", cancelled.State)
	}
	cancelledAction := actionByName(t, cancelled, "live")
	if cancelledAction.Stop == nil || cancelledAction.Stop.State != "confirmed" {
		t.Fatalf("cancelled action stop evidence = %+v; want confirmed termination", cancelledAction.Stop)
	}
	requireOpaqueIdentity(t, "cancelled cleanup workspace", cancelled.Cleanup.WorkspaceID)
	cancelledLog := instance.smokeReadLogs(ctx, t, engineer, created.ExecutionID, cancelledAction.ActionID, "stdout")
	if err := smoke.CheckMarker(cancelledLog, readyMarker, 1); err != nil {
		t.Fatalf("handoff readiness marker: %v", err)
	}

	reviewerInbox := instance.smokeTaskInbox(ctx, t, reviewer, "waiting_task")
	item, ok := smokeFindWaitingTask(reviewerInbox, created.TaskID)
	if !ok {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", created.TaskID, reviewerInbox.Items)
	}
	if item.QuestionID != waiting.Question.QuestionID {
		t.Fatalf("waiting_task question_id = %q; want %q", item.QuestionID, waiting.Question.QuestionID)
	}
	if item.Version != waiting.Question.Version {
		t.Fatalf("waiting_task version = %d; want the question version %d", item.Version, waiting.Question.Version)
	}
	if item.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("waiting_task lifecycle version = %d; want %d", item.LifecycleVersion, waiting.LifecycleVersion)
	}
	if item.Question != waiting.Question.Body {
		t.Fatalf("waiting_task question = %q; want %q", item.Question, waiting.Question.Body)
	}
	if !slices.Equal(item.References, []string{reference}) {
		t.Fatalf("waiting_task references = %v; want [%s]", item.References, reference)
	}
	operatorInbox := instance.smokeTaskInbox(ctx, t, operator, "waiting_task")
	if _, found := smokeFindWaitingTask(operatorInbox, created.TaskID); found {
		t.Fatalf("operator-only inbox exposed the reviewer-assigned waiting task: %+v", operatorInbox.Items)
	}
	instance.smokeRequireForbidden(ctx, t, reviewer, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: created.TaskID})

	replyRequest := api.TaskReplyRequest{
		TaskID:                  created.TaskID,
		QuestionID:              item.QuestionID,
		ExpectedQuestionVersion: item.Version,
		Answer:                  answer,
	}
	replied := instance.smokeTaskReply(ctx, t, reviewer, replyRequest, &item.LifecycleVersion, names.Key("reply"))
	replayed := instance.smokeTaskReply(ctx, t, reviewer, replyRequest, &item.LifecycleVersion, names.Key("reply"))
	if err := smoke.CheckReplayConverged(replied, replayed); err != nil {
		t.Fatalf("identical task.reply replay: %v", err)
	}
	if replied.ExecutionID == created.ExecutionID {
		t.Fatalf("reply reused the cancelled execution %q", created.ExecutionID)
	}

	second := instance.smokeWaitComplete(ctx, t, engineer, replied.ExecutionID, 6*time.Minute)
	requireSucceeded(t, second)
	secondAction := actionByName(t, second, "live")
	secondLog := instance.smokeReadLogs(ctx, t, engineer, replied.ExecutionID, secondAction.ActionID, "stdout")
	if err := smoke.CheckMarker(secondLog, restorationMarker, 1); err != nil {
		t.Fatalf("restoration marker: %v", err)
	}
	if err := smoke.CheckDistinctWorkspaces(cancelled.Cleanup.WorkspaceID, second.Cleanup.WorkspaceID); err != nil {
		t.Fatalf("handoff workspace: %v", err)
	}

	reinspected := instance.smokeInspect(ctx, t, engineer, created.ExecutionID)
	if reinspected.State != "cancelled" {
		t.Fatalf("cancelled execution state after the reply = %q; want cancelled", reinspected.State)
	}

	finalTask := instance.smokeWaitTaskState(ctx, t, engineer, created.TaskID, "done", 30*time.Second)
	if err := smoke.CheckExactHistory(finalTask.ExecutionHistory, created.ExecutionID, replied.ExecutionID); err != nil {
		t.Fatalf("task history after the handoff: %v", err)
	}
	if err := smoke.CheckBodyMessage(finalTask.Messages, answer, reviewerPrincipal); err != nil {
		t.Fatalf("task conversation: %v", err)
	}

	finished := time.Now().UTC()
	if recordScenario {
		instance.recordScenario(t, "ConversationHandoff", probe, &second, started, finished, map[string]any{
			"task_id":      created.TaskID,
			"action_id":    secondAction.ActionID,
			"workspace_id": second.Cleanup.WorkspaceID,
			"actor":        reviewerPrincipal,
		})
	}
	return second
}
