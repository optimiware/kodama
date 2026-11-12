package opsapp_test

import (
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestTaskFamily drives the real task service through its full conversation
// lifecycle and reads the committed task, history and inbox back.
func TestTaskFamily(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipeline(t, h)

	created, err := h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":       "First task",
		"description": "do the thing",
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create: %v", err)
	}
	var createdTask api.TaskCreateResult
	decodeResult(t, created, &createdTask)
	if createdTask.TaskID == "" || createdTask.ContentVersion != 1 || createdTask.LifecycleVersion != 1 {
		t.Fatalf("task.create result = %+v", createdTask)
	}
	taskID := createdTask.TaskID

	read, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.read: %v", err)
	}
	var readTask api.TaskReadResult
	decodeResult(t, read, &readTask)
	if readTask.Title != "First task" || readTask.State != "open" {
		t.Fatalf("task.read result = %+v", readTask)
	}

	edited, err := h.invokeVersion(t, testToken, "task.edit", jsonBody(t, map[string]any{
		"title":       "First task renamed",
		"description": "do the thing",
		"metadata":    map[string]string{"owner": "team"},
	}), 1, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.edit: %v", err)
	}
	var edit api.TaskEditResult
	decodeResult(t, edited, &edit)
	if edit.ContentVersion != 2 || edit.LifecycleVersion != 1 {
		t.Fatalf("task.edit result = %+v", edit)
	}

	commented, err := h.invokeVersion(t, testToken, "task.comment", jsonBody(t, map[string]any{
		"body": "a note",
	}), 2, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.comment: %v", err)
	}
	var comment api.TaskCommentResult
	decodeResult(t, commented, &comment)
	if comment.ContentVersion != 3 || comment.LifecycleVersion != 1 {
		t.Fatalf("task.comment result = %+v", comment)
	}

	selected, err := h.invokeVersion(t, testToken, "task.select_pipeline", jsonBody(t, map[string]any{
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), 1, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.select_pipeline: %v", err)
	}
	var selection api.TaskSelectPipelineResult
	decodeResult(t, selected, &selection)
	if selection.LifecycleVersion != 2 {
		t.Fatalf("task.select_pipeline result = %+v", selection)
	}

	started, err := h.invokeVersion(t, testToken, "task.start", `{}`, 2, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.start: %v", err)
	}
	var start api.TaskStartResult
	decodeResult(t, started, &start)
	if start.ExecutionID == "" || start.LifecycleVersion != 3 {
		t.Fatalf("task.start result = %+v", start)
	}

	asked, err := h.invokeVersion(t, testToken, "task.ask", jsonBody(t, map[string]any{
		"question":   "which branch?",
		"assignment": "role/engineer",
	}), 3, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.ask: %v", err)
	}
	var ask api.TaskAskResult
	decodeResult(t, asked, &ask)
	if ask.QuestionID == "" || ask.LifecycleVersion != 4 || ask.QuestionVersion != 1 {
		t.Fatalf("task.ask result = %+v", ask)
	}

	replied, err := h.invokeVersion(t, testToken, "task.reply", jsonBody(t, map[string]any{
		"question_id":               ask.QuestionID,
		"expected_question_version": 1,
		"answer":                    "main",
	}), 4, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.reply: %v", err)
	}
	var reply api.TaskReplyResult
	decodeResult(t, replied, &reply)
	if reply.ExecutionID == "" || reply.LifecycleVersion != 5 || reply.QuestionVersion != 2 {
		t.Fatalf("task.reply result = %+v", reply)
	}

	cancelled, err := h.invokeVersion(t, testToken, "task.cancel", `{}`, 5, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.cancel: %v", err)
	}
	var cancel api.TaskCancelResult
	decodeResult(t, cancelled, &cancel)
	if cancel.LifecycleVersion != 6 {
		t.Fatalf("task.cancel result = %+v", cancel)
	}

	history, err := h.invoke(t, testToken, "task.history", `{}`, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.history: %v", err)
	}
	var page api.TaskHistoryResult
	decodeResult(t, history, &page)
	if len(page.Entries) == 0 {
		t.Fatal("task.history returned no entries")
	}

	inboxPage, err := h.invoke(t, testToken, "task.inbox", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox: %v", err)
	}
	var inbox api.TaskInboxResult
	decodeResult(t, inboxPage, &inbox)
	if inboxPage.Operation != "task.inbox" {
		t.Fatalf("task.inbox result operation = %q", inboxPage.Operation)
	}
}

// TestTaskCreateAndStart drives the combined creation and admission.
func TestTaskCreateAndStart(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipeline(t, h)
	created, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Combined",
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create_and_start: %v", err)
	}
	var result api.TaskCreateAndStartResult
	decodeResult(t, created, &result)
	if result.TaskID == "" || result.ExecutionID == "" || result.LifecycleVersion < 1 {
		t.Fatalf("task.create_and_start result = %+v", result)
	}
}

// applyPipeline creates the pinned build.v2 pipeline the task tests select.
func applyPipeline(t *testing.T, h *harness) string {
	t.Helper()
	return applyPipelineWithKey(t, h, "build.v2", "key-pipeline.apply")
}

// TestTaskAskReplayAfterReplyOmitsQuestionID pins the optional question id:
// the capability AskResult carries no question id, so a replay after the
// question was answered returns the original versions with no question_id
// instead of failing.
func TestTaskAskReplayAfterReplyOmitsQuestionID(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipeline(t, h)
	created, err := h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":       "Ask replay",
		"pipeline_id": pipelineID,
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create: %v", err)
	}
	var createdTask api.TaskCreateResult
	decodeResult(t, created, &createdTask)
	taskID := createdTask.TaskID

	if _, err := h.invokeVersion(t, testToken, "task.select_pipeline", jsonBody(t, map[string]any{
		"pipeline_id": pipelineID,
	}), 1, map[string]string{"task": taskID}); err != nil {
		t.Fatalf("task.select_pipeline: %v", err)
	}
	if _, err := h.invokeVersion(t, testToken, "task.start", `{}`, 2, map[string]string{"task": taskID}); err != nil {
		t.Fatalf("task.start: %v", err)
	}
	askInput := jsonBody(t, map[string]any{"question": "branch?", "assignment": "role/engineer"})
	asked, err := h.invokeVersion(t, testToken, "task.ask", askInput, 3, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.ask: %v", err)
	}
	var ask api.TaskAskResult
	decodeResult(t, asked, &ask)
	if ask.QuestionID == "" {
		t.Fatal("first task.ask omitted the outstanding question id")
	}
	if _, err := h.invokeVersion(t, testToken, "task.reply", jsonBody(t, map[string]any{
		"question_id":               ask.QuestionID,
		"expected_question_version": 1,
		"answer":                    "main",
	}), 4, map[string]string{"task": taskID}); err != nil {
		t.Fatalf("task.reply: %v", err)
	}
	replayed, err := h.invokeVersion(t, testToken, "task.ask", askInput, 3, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("replayed task.ask: %v", err)
	}
	var replay api.TaskAskResult
	decodeResult(t, replayed, &replay)
	if replay.QuestionID != "" {
		t.Fatalf("replay question_id = %q; want omitted", replay.QuestionID)
	}
	if replay.LifecycleVersion != ask.LifecycleVersion || replay.QuestionVersion != ask.QuestionVersion {
		t.Fatalf("replay versions = %+v; want %+v", replay, ask)
	}
}

// TestTaskInboxWaitingItemCarriesQuestionProjection proves the public
// waiting-task projection end to end: the exact assignee reads the question
// identity, body, references and lifecycle version from task.inbox without a
// second read, Version stays the question version, and the encoded task.inbox
// result passes the public result validation.
func TestTaskInboxWaitingItemCarriesQuestionProjection(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipeline(t, h)

	created, err := h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":       "Inbox projection",
		"pipeline_id": pipelineID,
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create: %v", err)
	}
	var createdTask api.TaskCreateResult
	decodeResult(t, created, &createdTask)
	taskID := createdTask.TaskID

	const reference = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	asked, err := h.invokeVersion(t, testToken, "task.ask", jsonBody(t, map[string]any{
		"question":   "Which base should the merge use?",
		"assignment": "role/engineer",
		"references": []string{reference},
	}), 1, map[string]string{"task": taskID})
	if err != nil {
		t.Fatalf("task.ask: %v", err)
	}
	var ask api.TaskAskResult
	decodeResult(t, asked, &ask)
	if ask.QuestionID == "" || ask.QuestionVersion == 0 || ask.LifecycleVersion == 0 {
		t.Fatalf("task.ask result = %+v", ask)
	}

	page, err := h.invoke(t, testToken, "task.inbox", jsonBody(t, map[string]any{
		"kinds": []string{"waiting_task"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox: %v", err)
	}
	var inbox api.TaskInboxResult
	decodeResult(t, page, &inbox)
	if len(inbox.Items) != 1 {
		t.Fatalf("task.inbox returned %d waiting items; want 1: %+v", len(inbox.Items), inbox.Items)
	}
	item := inbox.Items[0]
	if item.Kind != "waiting_task" || item.TaskID != taskID {
		t.Fatalf("task.inbox item = %+v; want the waiting task %q", item, taskID)
	}
	if item.QuestionID != ask.QuestionID {
		t.Errorf("item question_id = %q; want %q", item.QuestionID, ask.QuestionID)
	}
	if item.Question != "Which base should the merge use?" {
		t.Errorf("item question = %q; want the outstanding body", item.Question)
	}
	if len(item.References) != 1 || item.References[0] != reference {
		t.Errorf("item references = %v; want [%s]", item.References, reference)
	}
	if item.Version != ask.QuestionVersion {
		t.Errorf("item version = %d; want the question version %d", item.Version, ask.QuestionVersion)
	}
	if item.LifecycleVersion != ask.LifecycleVersion {
		t.Errorf("item lifecycle_version = %d; want the task lifecycle version %d", item.LifecycleVersion, ask.LifecycleVersion)
	}
	if item.Version == item.LifecycleVersion {
		t.Fatalf("item question and lifecycle versions collapsed to %d", item.Version)
	}

	codec, ok := codecForOperation("task.inbox")
	if !ok {
		t.Fatal("task.inbox has no public codec")
	}
	if _, err := codec.EncodeResult(inbox); err != nil {
		t.Fatalf("encoded task.inbox result refused: %v", err)
	}
}
