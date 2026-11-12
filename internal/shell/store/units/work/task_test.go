package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestTaskCreateRoundTrip(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	result := fixture.createTask(t, scope, testTask)
	if result.TaskID != testTask || result.ContentVersion != 1 || result.LifecycleVersion != 1 || result.ExecutionID != "" || result.Replayed {
		t.Fatalf("unexpected create result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Fix the build" || got.Description != "make it green" {
		t.Fatalf("content did not round-trip: %+v", got)
	}
	if got.Metadata["team"] != "core" || got.Inputs["ref"] != "main" {
		t.Fatalf("metadata/inputs did not round-trip: %+v", got)
	}
	if got.State != task.StateOpen || got.LifecycleVersion != 1 || got.ContentVersion != 1 {
		t.Fatalf("versions/state did not round-trip: %+v", got)
	}
	if len(got.Messages) != 0 || got.Question != nil || got.CurrentExecution != "" {
		t.Fatalf("a plain create must not carry conversation or execution state: %+v", got)
	}
}

func TestTaskCreateAndStartCommitTogether(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	start := fixture.startUnit(scope, testTask, testExecution, 0, 1, fixture.receipt("task.create", "create-and-start", contentHashA))
	result, err := fixture.task.Create(context.Background(), task.CreateUnit{
		Owner:            scope,
		TaskID:           testTask,
		Title:            "Fix the build",
		SelectedPipeline: testPipeline,
		Inputs:           map[id.InputKey]string{"ref": "main"},
		Initiator:        testPrincipal,
		At:               testNow,
		Start:            &start,
		Receipt:          fixture.receipt("task.create", "create-and-start", contentHashA),
		Audit:            []event.Audit{fixture.audit(scope, "task.create")},
		Events:           []event.Envelope{fixture.event(scope, string(testExecution), 1)},
	})
	if err != nil {
		t.Fatalf("Create with start: %v", err)
	}
	if result.ExecutionID != testExecution || result.LifecycleVersion != 2 || result.ContentVersion != 1 {
		t.Fatalf("unexpected create-and-start result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LifecycleVersion != 2 || got.CurrentExecution != testExecution || got.State != task.StateOpen {
		t.Fatalf("task did not advance to the started state: %+v", got)
	}
	if len(got.ExecutionHistory) != 1 || got.ExecutionHistory[0] != testExecution {
		t.Fatalf("execution history = %v", got.ExecutionHistory)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", string(testExecution))
	if !ok {
		t.Fatal("the queued execution was not committed with the task")
	}
	if textValue(execution, "state") != string(kernel.ExecutionQueued) || versionValue(execution, "version") != 1 {
		t.Fatalf("execution row = %+v", execution.Values)
	}
	admitted, err := snapshotdoc.Decode(bytesValue(execution, "snapshot"))
	if err != nil {
		t.Fatalf("snapshotdoc.Decode: %v", err)
	}
	snapshot := admitted.Snapshot
	if string(snapshot.YAMLBytes()) != "kind: Pipeline\nversion: 1\n" {
		t.Fatalf("admitted YAML did not round-trip exactly: %q", snapshot.YAMLBytes())
	}
	if snapshot.Inputs["ref"] != "main" || snapshot.Metadata["owner"] != "team" || snapshot.TaskID != testTask {
		t.Fatalf("admitted snapshot did not round-trip: %+v", snapshot)
	}
	if len(admitted.Actions) != 2 || admitted.Actions[0].Container == nil {
		t.Fatalf("admitted actions did not round-trip: %+v", admitted.Actions)
	}
	if count := fixture.countRows(t, scope, "actions", map[string]any{"execution_id": string(testExecution)}); count != 2 {
		t.Fatalf("committed actions = %d; want 2", count)
	}
	if count := fixture.countRows(t, scope, "receipts", nil); count != 1 {
		t.Fatalf("committed receipts = %d; want 1", count)
	}
}

func TestTaskStartReplayAndChangedContent(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	record := fixture.receipt("task.start", "start-1", contentHashA)
	unit := fixture.startUnit(scope, testTask, testExecution, 1, 1, record)
	first, err := fixture.task.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if first.Replayed {
		t.Fatal("the first start must not be a replay")
	}
	replayed, err := fixture.task.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Start: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionID != first.ExecutionID || replayed.LifecycleVersion != first.LifecycleVersion {
		t.Fatalf("replay returned %+v; want the original %+v", replayed, first)
	}
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 1 {
		t.Fatalf("a replayed start committed %d executions; want 1", count)
	}
	changed := unit
	changed.Receipt = fixture.receipt("task.start", "start-1", contentHashB)
	if _, err := fixture.task.Start(context.Background(), changed); err == nil {
		t.Fatal("changed receipt content must be refused")
	} else {
		mustClass(t, err, fault.ChangedIdempotencyContent)
	}
}

func TestTaskStartStaleVersionLeavesNoState(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	record := fixture.receipt("task.start", "start-stale", contentHashA)
	_, err := fixture.task.Start(context.Background(), fixture.startUnit(scope, testTask, testExecution, 9, 1, record))
	mustClass(t, err, fault.VersionConflict)
	if _, ok := fixture.storedRow(t, scope, "executions", string(testExecution)); ok {
		t.Fatal("a refused start left an execution row")
	}
	if _, ok := fixture.store.Receipt(record.Key); ok {
		t.Fatal("a refused start left a receipt")
	}
	row := fixture.storedTask(t, scope, testTask)
	if versionValue(row, "lifecycle_version") != 1 {
		t.Fatal("a refused start advanced the task lifecycle version")
	}
}

func TestTaskAskAtomicHandoff(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	// Stage outputs on the running execution's first action so the handoff must
	// discard them in the same unit.
	actionID := string(snapshotdoc.DeriveActionID(testExecution, 0))
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "actions",
			ID:            actionID,
			ExpectVersion: 1,
			Values: map[string]any{
				"staged_outputs_set": true,
				"staged_outputs":     []byte(`{"artifact":"sha256:abc"}`),
			},
		}},
	}); err != nil {
		t.Fatalf("stage outputs: %v", err)
	}
	unit := fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-1", contentHashA))
	result, err := fixture.task.Ask(context.Background(), unit)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.LifecycleVersion != 3 || result.QuestionVersion != 1 || result.Replayed {
		t.Fatalf("unexpected ask result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateWaiting || got.LifecycleVersion != 3 {
		t.Fatalf("task did not enter waiting: %+v", got)
	}
	// The cancelled execution stays current so a later reply waits for its
	// confirmed stop evidence before dispatching.
	if got.CurrentExecution != testExecution {
		t.Fatalf("ask dropped the current execution: %+v", got)
	}
	if got.Question == nil || got.Question.ID != "question-1" || got.Question.Body != "Which branch should I target?" {
		t.Fatalf("question did not round-trip: %+v", got.Question)
	}
	if got.Question.Version != 1 || got.Question.AskedBy != testPrincipal || !got.Question.At.Equal(testNow) {
		t.Fatalf("question metadata = %+v", got.Question)
	}
	if got.Question.Assignment.Kind != "role" || got.Question.Assignment.Role != tenancy.RoleOperator {
		t.Fatalf("question assignment = %+v", got.Question.Assignment)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", string(testExecution))
	if !ok {
		t.Fatal("the cancelled execution disappeared")
	}
	if textValue(execution, "state") != string(kernel.ExecutionCancelled) || versionValue(execution, "version") != 2 {
		t.Fatalf("execution row = %+v", execution.Values)
	}
	if execution.Values["token_nonce"] != nil || execution.Values["token_expires_at"] != nil {
		t.Fatal("the cancelled execution kept a credential pair")
	}
	action, ok := fixture.storedRow(t, scope, "actions", actionID)
	if !ok {
		t.Fatal("the action row disappeared")
	}
	if boolValue(action, "staged_outputs_set") || len(bytesValue(action, "staged_outputs")) != 0 {
		t.Fatal("the handoff did not discard staged outputs")
	}
	// The handoff is the Kernel cancel reduction, not a direct state write:
	// the pending actions are cancelled and the cleanup row is owed.
	if textValue(action, "state") != string(kernel.ActionCancelled) {
		t.Fatalf("the handoff left the action %q; want cancelled", textValue(action, "state"))
	}
	if cleanup, ok := cleanupRow(t, fixture, scope, testExecution); !ok || textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("the handoff committed no pending cleanup row: %+v", cleanup.Values)
	}
	// A stale retry is refused and changes nothing.
	_, err = fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-stale", contentHashA)))
	mustClass(t, err, fault.VersionConflict)
	after, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after stale ask: %v", err)
	}
	if after.LifecycleVersion != 3 || after.Question == nil || after.Question.ID != "question-1" {
		t.Fatalf("a stale ask changed the task: %+v", after)
	}
}

func TestTaskAskRollbackLeavesNoPartialState(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
	_, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-rollback", contentHashA)))
	mustClass(t, err, fault.DependencyUnavailable)
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateOpen || got.LifecycleVersion != 2 || got.Question != nil {
		t.Fatalf("a failed ask left partial state: %+v", got)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", string(testExecution))
	if !ok {
		t.Fatal("the execution disappeared")
	}
	if textValue(execution, "state") != string(kernel.ExecutionQueued) || versionValue(execution, "version") != 1 {
		t.Fatalf("a failed ask changed the execution: %+v", execution.Values)
	}
}

func TestTaskReplyReopensWithExactlyOneExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-1", contentHashA))); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	unit := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "reply-1", contentHashA))
	result, err := fixture.task.Reply(context.Background(), unit)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if result.ExecutionID != "execution-2" || result.LifecycleVersion != 4 || result.QuestionVersion != 2 || result.Replayed {
		t.Fatalf("unexpected reply result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateOpen || got.LifecycleVersion != 4 || got.CurrentExecution != "execution-2" {
		t.Fatalf("task did not reopen: %+v", got)
	}
	if got.Question != nil {
		t.Fatalf("reply did not clear the question: %+v", got.Question)
	}
	if len(got.ExecutionHistory) != 2 || got.ExecutionHistory[1] != "execution-2" {
		t.Fatalf("execution history = %v", got.ExecutionHistory)
	}
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 2 {
		t.Fatalf("committed executions = %d; want 2", count)
	}
	if count := fixture.countRows(t, scope, "actions", map[string]any{"execution_id": "execution-2"}); count != 1 {
		t.Fatalf("new execution actions = %d; want 1", count)
	}
	// A retry with the answered question is refused and creates no third
	// execution.
	stale := fixture.replyUnit(scope, testTask, "execution-3", 3, 1, 1, fixture.receipt("task.reply", "reply-stale", contentHashA))
	if _, err := fixture.task.Reply(context.Background(), stale); err == nil {
		t.Fatal("a stale reply must be refused")
	} else {
		mustClass(t, err, fault.VersionConflict)
	}
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 2 {
		t.Fatalf("a stale reply committed an execution; count = %d", count)
	}
}

// TestTaskCancelClearsCredentialAndCancelsQueuedExecution proves the execution
// row is cancelled at version 2 through the Kernel reduction, not a direct
// write, so the pending actions are cancelled and the cleanup row is owed. The
// queued predecessor never started, so no stop intent is carried (covered by
// TestTaskCancelQueuedPredecessorHasNoStopIntent).
func TestTaskCancelClearsCredentialAndCancelsQueuedExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	result, err := fixture.task.Cancel(context.Background(), fixture.cancelUnit(t, scope, testTask, 2, fixture.receipt("task.cancel", "cancel-1", contentHashA)))
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if result.LifecycleVersion != 3 || result.Replayed {
		t.Fatalf("unexpected cancel result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateCancelled || got.LifecycleVersion != 3 {
		t.Fatalf("task was not cancelled: %+v", got)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", string(testExecution))
	if !ok {
		t.Fatal("the execution disappeared")
	}
	if textValue(execution, "state") != string(kernel.ExecutionCancelled) || versionValue(execution, "version") != 2 {
		t.Fatalf("execution row = %+v", execution.Values)
	}
	if execution.Values["token_nonce"] != nil || execution.Values["token_expires_at"] != nil {
		t.Fatal("cancel kept the execution credential")
	}
	actions := actionRows(t, fixture, scope, testExecution)
	if len(actions) != 2 {
		t.Fatalf("action rows = %d; want 2", len(actions))
	}
	for _, action := range actions {
		if textValue(action, "state") != string(kernel.ActionCancelled) {
			t.Fatalf("action row = %+v; want cancelled", action.Values)
		}
	}
	cleanup, ok := cleanupRow(t, fixture, scope, testExecution)
	if !ok || textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("cleanup row = %+v; want pending", cleanup.Values)
	}
}

func TestTaskEditContentCommentAndSelection(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	content := task.EditUnit{
		Owner:                  scope,
		TaskID:                 testTask,
		Kind:                   task.EditContent,
		ExpectedContentVersion: 1,
		Title:                  "Retitled",
		Description:            "new body",
		Metadata:               map[string]string{"team": "platform"},
		Receipt:                fixture.receipt("task.edit", "edit-content", contentHashA),
		Audit:                  []event.Audit{fixture.audit(scope, "task.edit")},
	}
	result, err := fixture.task.Edit(context.Background(), content)
	if err != nil {
		t.Fatalf("content edit: %v", err)
	}
	if result.ContentVersion != 2 || result.LifecycleVersion != 0 {
		t.Fatalf("unexpected content result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Retitled" || got.Description != "new body" || got.Metadata["team"] != "platform" {
		t.Fatalf("content edit did not round-trip: %+v", got)
	}
	if got.LifecycleVersion != 1 {
		t.Fatal("a content edit advanced the lifecycle version")
	}
	comment := task.EditUnit{
		Owner:                  scope,
		TaskID:                 testTask,
		Kind:                   task.EditComment,
		ExpectedContentVersion: 2,
		Message:                &task.Message{Author: testPrincipal, At: testNow, Body: "first comment", References: []string{"pipeline:1"}},
		Receipt:                fixture.receipt("task.edit", "edit-comment", contentHashA),
		Audit:                  []event.Audit{fixture.audit(scope, "task.edit")},
	}
	if _, err := fixture.task.Edit(context.Background(), comment); err != nil {
		t.Fatalf("comment edit: %v", err)
	}
	got, err = fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after comment: %v", err)
	}
	if got.ContentVersion != 3 || len(got.Messages) != 1 || got.Messages[0].Body != "first comment" {
		t.Fatalf("comment did not round-trip: %+v", got)
	}
	if got.Messages[0].Author != testPrincipal || len(got.Messages[0].References) != 1 {
		t.Fatalf("comment metadata = %+v", got.Messages[0])
	}
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	selection := task.EditUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		Kind:                     task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1,
		ExpectedPipelineVersion:  1,
		PipelineID:               testPipeline,
		Inputs:                   map[id.InputKey]string{"ref": "release"},
		StartAuthority:           testPrincipal,
		Receipt:                  fixture.receipt("task.edit", "edit-select", contentHashA),
		Audit:                    []event.Audit{fixture.audit(scope, "task.edit")},
	}
	result, err = fixture.task.Edit(context.Background(), selection)
	if err != nil {
		t.Fatalf("selection edit: %v", err)
	}
	if result.LifecycleVersion != 2 {
		t.Fatalf("unexpected selection result: %+v", result)
	}
	got, err = fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after selection: %v", err)
	}
	if got.SelectedPipeline != testPipeline || got.Inputs["ref"] != "release" {
		t.Fatalf("selection did not round-trip: %+v", got)
	}
	// A stale content edit is refused and leaves the row unchanged.
	stale := content
	stale.Receipt = fixture.receipt("task.edit", "edit-stale", contentHashA)
	if _, err := fixture.task.Edit(context.Background(), stale); err == nil {
		t.Fatal("a stale content edit must be refused")
	} else {
		mustClass(t, err, fault.VersionConflict)
	}
	after, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after stale edit: %v", err)
	}
	if after.Title != "Retitled" || after.ContentVersion != 3 {
		t.Fatalf("a stale edit changed the task: %+v", after)
	}
}

func TestTaskEditReplayDoesNotDuplicateComment(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	comment := task.EditUnit{
		Owner:                  scope,
		TaskID:                 testTask,
		Kind:                   task.EditComment,
		ExpectedContentVersion: 1,
		Message:                &task.Message{Author: testPrincipal, At: testNow, Body: "once"},
		Receipt:                fixture.receipt("task.edit", "edit-once", contentHashA),
		Audit:                  []event.Audit{fixture.audit(scope, "task.edit")},
	}
	if _, err := fixture.task.Edit(context.Background(), comment); err != nil {
		t.Fatalf("comment edit: %v", err)
	}
	replayed, err := fixture.task.Edit(context.Background(), comment)
	if err != nil {
		t.Fatalf("replayed comment edit: %v", err)
	}
	if !replayed.Replayed || replayed.ContentVersion != 2 {
		t.Fatalf("unexpected replay result: %+v", replayed)
	}
	if count := fixture.countRows(t, scope, "task_messages", map[string]any{"task_id": string(testTask)}); count != 1 {
		t.Fatalf("a replayed comment committed %d messages; want 1", count)
	}
}

func TestTaskProgressCurrentOnly(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	result, err := fixture.task.Apply(context.Background(), task.ProgressUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		ExecutionID:              testExecution,
		ExpectedLifecycleVersion: 2,
		At:                       testNow,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.LifecycleVersion != 3 || result.Replayed {
		t.Fatalf("unexpected progress result: %+v", result)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateDone || got.LifecycleVersion != 3 {
		t.Fatalf("task was not marked done: %+v", got)
	}
	// The same completion observed again is no longer current.
	_, err = fixture.task.Apply(context.Background(), task.ProgressUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		ExecutionID:              testExecution,
		ExpectedLifecycleVersion: 2,
		At:                       testNow,
	})
	mustClass(t, err, fault.VersionConflict)
}

func TestTaskProgressRefusesNonCurrentExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	_, err := fixture.task.Apply(context.Background(), task.ProgressUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		ExecutionID:              "execution-other",
		ExpectedLifecycleVersion: 2,
		At:                       testNow,
	})
	mustClass(t, err, fault.VersionConflict)
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateOpen || got.LifecycleVersion != 2 {
		t.Fatalf("a non-current completion changed the task: %+v", got)
	}
}

func TestTaskHistoryStablePages(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	for index, body := range []string{"first", "second"} {
		at := testNow.Add(time.Duration(index+1) * time.Minute)
		_, err := fixture.task.Edit(context.Background(), task.EditUnit{
			Owner:                  scope,
			TaskID:                 testTask,
			Kind:                   task.EditComment,
			ExpectedContentVersion: uint64(index + 1),
			Message:                &task.Message{Author: testPrincipal, At: at, Body: body},
			Receipt:                fixture.receipt("task.edit", "history-"+body, contentHashA),
			Audit:                  []event.Audit{fixture.audit(scope, "task.edit")},
		})
		if err != nil {
			t.Fatalf("comment %q: %v", body, err)
		}
	}
	var (
		entries []task.HistoryEntry
		cursor  string
	)
	for {
		page, err := fixture.task.History(context.Background(), scope, testTask, task.HistoryQuery{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		entries = append(entries, page.Entries...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(entries) != 3 {
		t.Fatalf("history entries = %d; want 3", len(entries))
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		identity := string(entry.Kind) + "/" + string(entry.ExecutionID) + "/" + historyIdentity(entry)
		if seen[identity] {
			t.Fatalf("history repeated entry %q", identity)
		}
		seen[identity] = true
	}
	if entries[0].Kind != task.HistoryExecution || entries[1].Kind != task.HistoryMessage || entries[2].Kind != task.HistoryMessage {
		t.Fatalf("unexpected history order: %+v", entries)
	}
	// A cursor from another task is refused.
	page, err := fixture.task.History(context.Background(), scope, testTask, task.HistoryQuery{Limit: 1})
	if err != nil {
		t.Fatalf("History first page: %v", err)
	}
	fixture.createTask(t, otherScope(), "task-2")
	_, err = fixture.task.History(context.Background(), otherScope(), "task-2", task.HistoryQuery{Limit: 1, Cursor: page.NextCursor})
	mustClass(t, err, fault.Forbidden)
}

// historyIdentity names one history entry by its execution, message body or
// question, so a test can detect an entry a paged read returned twice.
func historyIdentity(entry task.HistoryEntry) string {
	switch entry.Kind {
	case task.HistoryExecution:
		return string(entry.ExecutionID)
	case task.HistoryMessage:
		if entry.Message != nil {
			return entry.Message.Body
		}
	case task.HistoryQuestion:
		if entry.Question != nil {
			return string(entry.Question.ID)
		}
	}
	return ""
}

func TestTaskScopeIsolation(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	if _, err := fixture.task.Get(context.Background(), otherScope(), testTask); err == nil {
		t.Fatal("a cross-scope task read must be refused")
	} else {
		mustClass(t, err, fault.NotFound)
	}
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	_, err := fixture.task.Start(context.Background(), fixture.startUnit(otherScope(), testTask, testExecution, 1, 1, fixture.receipt("task.start", "cross-scope", contentHashA)))
	mustClass(t, err, fault.NotFound)
	if _, ok := fixture.storedRow(t, scope, "executions", string(testExecution)); ok {
		t.Fatal("a cross-scope start committed an execution")
	}
}

func TestTaskStartRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	units := []task.StartUnit{
		fixture.startUnit(scope, testTask, "execution-a", 1, 1, fixture.receipt("task.start", "race-a", contentHashA)),
		fixture.startUnit(scope, testTask, "execution-b", 1, 1, fixture.receipt("task.start", "race-b", contentHashA)),
	}
	start := make(chan struct{})
	results := make([]error, len(units))
	var wait sync.WaitGroup
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, results[index] = fixture.task.Start(context.Background(), units[index])
		}(index)
	}
	close(start)
	wait.Wait()
	winners, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		default:
			mustClass(t, err, fault.VersionConflict)
			conflicts++
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("race produced %d winners and %d conflicts; want one of each", winners, conflicts)
	}
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 1 {
		t.Fatalf("race committed %d executions; want 1", count)
	}
}

func TestTaskCancelRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	units := []task.CancelUnit{
		fixture.cancelUnit(t, scope, testTask, 2, fixture.receipt("task.cancel", "race-a", contentHashA)),
		fixture.cancelUnit(t, scope, testTask, 2, fixture.receipt("task.cancel", "race-b", contentHashA)),
	}
	start := make(chan struct{})
	results := make([]error, len(units))
	var wait sync.WaitGroup
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, results[index] = fixture.task.Cancel(context.Background(), units[index])
		}(index)
	}
	close(start)
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		mustClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("cancel race produced %d winners; want 1", winners)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", string(testExecution))
	if !ok {
		t.Fatal("the execution disappeared")
	}
	if versionValue(execution, "version") != 2 {
		t.Fatalf("the execution was cancelled twice: version %d", versionValue(execution, "version"))
	}
}

// ensure the imports used only by helpers stay referenced in this file's build.
var (
	_ = pipeline.PipelineID("")
	_ = receipt.Record{}
)

func TestTaskEditSelectionRefusesStalePipelineVersion(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	_, err := fixture.task.Edit(context.Background(), task.EditUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		Kind:                     task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1,
		ExpectedPipelineVersion:  9,
		PipelineID:               testPipeline,
		Receipt:                  fixture.receipt("task.edit", "select-stale-pipeline", contentHashA),
		Audit:                    []event.Audit{fixture.audit(scope, "task.edit")},
	})
	mustClass(t, err, fault.VersionConflict)
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SelectedPipeline != "" || got.LifecycleVersion != 1 {
		t.Fatalf("a stale pipeline selection changed the task: %+v", got)
	}
}

func TestTaskWritePortsRollBackOnCommitFailure(t *testing.T) {
	type testCase struct {
		name  string
		setup func(*fixture, tenancy.Scope)
		apply func(*fixture, tenancy.Scope) error
		check func(*testing.T, *fixture, tenancy.Scope)
	}
	cases := []testCase{
		{
			name:  "create",
			setup: func(*fixture, tenancy.Scope) {},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Create(context.Background(), task.CreateUnit{
					Owner: scope, TaskID: testTask, Title: "t", Initiator: testPrincipal, At: testNow,
					Receipt: f.receipt("task.create", "rollback-create", contentHashA),
				})
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "tasks", string(testTask)); ok {
					t.Fatal("a failed create committed a task row")
				}
			},
		},
		{
			name: "start",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Start(context.Background(), f.startUnit(scope, testTask, testExecution, 1, 1, f.receipt("task.start", "rollback-start", contentHashA)))
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "executions", string(testExecution)); ok {
					t.Fatal("a failed start committed an execution row")
				}
				if versionValue(f.storedTask(t, scope, testTask), "lifecycle_version") != 1 {
					t.Fatal("a failed start advanced the task")
				}
			},
		},
		{
			name: "ask",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Ask(context.Background(), f.askUnit(t, scope, testTask, 2, f.receipt("task.ask", "rollback-ask", contentHashA)))
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if f.storedTask(t, scope, testTask).Values["question_id"] != nil {
					t.Fatal("a failed ask committed a question")
				}
			},
		},
		{
			name: "reply",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
				if _, err := f.task.Ask(context.Background(), f.askUnit(t, scope, testTask, 2, f.receipt("task.ask", "rollback-ask-setup", contentHashA))); err != nil {
					t.Fatalf("setup ask: %v", err)
				}
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Reply(context.Background(), f.replyUnit(scope, testTask, "execution-2", 3, 1, 1, f.receipt("task.reply", "rollback-reply", contentHashA)))
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "executions", "execution-2"); ok {
					t.Fatal("a failed reply committed an execution")
				}
				if f.storedTask(t, scope, testTask).Values["question_id"] == nil {
					t.Fatal("a failed reply cleared the question")
				}
			},
		},
		{
			name: "cancel",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Cancel(context.Background(), f.cancelUnit(t, scope, testTask, 2, f.receipt("task.cancel", "rollback-cancel", contentHashA)))
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				row := f.storedTask(t, scope, testTask)
				if textValue(row, "state") != string(task.StateOpen) || versionValue(row, "lifecycle_version") != 2 {
					t.Fatalf("a failed cancel changed the task: %+v", row.Values)
				}
			},
		},
		{
			name: "edit",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.createTask(t, scope, testTask)
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Edit(context.Background(), task.EditUnit{
					Owner: scope, TaskID: testTask, Kind: task.EditContent, ExpectedContentVersion: 1,
					Title: "changed", Receipt: f.receipt("task.edit", "rollback-edit", contentHashA),
				})
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if textValue(f.storedTask(t, scope, testTask), "title") != "Fix the build" {
					t.Fatal("a failed edit changed the title")
				}
			},
		},
		{
			name: "progress",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
			},
			apply: func(f *fixture, scope tenancy.Scope) error {
				_, err := f.task.Apply(context.Background(), task.ProgressUnit{
					Owner: scope, TaskID: testTask, ExecutionID: testExecution,
					ExpectedLifecycleVersion: 2, At: testNow,
				})
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if textValue(f.storedTask(t, scope, testTask), "state") != string(task.StateOpen) {
					t.Fatal("a failed progress marked the task done")
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			test.setup(fixture, scope)
			receipts := fixture.countRows(t, scope, "receipts", nil)
			events := fixture.countRows(t, scope, "events", nil)
			fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
			err := test.apply(fixture, scope)
			mustClass(t, err, fault.DependencyUnavailable)
			test.check(t, fixture, scope)
			if count := fixture.countRows(t, scope, "receipts", nil); count != receipts {
				t.Fatalf("a failed %s committed %d receipts; want %d", test.name, count, receipts)
			}
			if count := fixture.countRows(t, scope, "events", nil); count != events {
				t.Fatalf("a failed %s published %d events; want %d", test.name, count, events)
			}
		})
	}
}

// runConcurrently starts both calls on one barrier and returns their errors.
func runConcurrently(first, second func() error) []error {
	start := make(chan struct{})
	results := make([]error, 2)
	var wait sync.WaitGroup
	for index, call := range []func() error{first, second} {
		wait.Add(1)
		go func(index int, call func() error) {
			defer wait.Done()
			<-start
			results[index] = call()
		}(index, call)
	}
	close(start)
	wait.Wait()
	return results
}

// mustOneWinner asserts exactly one call succeeded and the other lost the guard.
func mustOneWinner(t *testing.T, results []error) {
	t.Helper()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		mustClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("race produced %d winners; want 1", winners)
	}
}

func TestTaskAskRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	first := fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-race-a", contentHashA))
	second := fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-race-b", contentHashA))
	second.QuestionID = "question-2"
	results := runConcurrently(
		func() error { _, err := fixture.task.Ask(context.Background(), first); return err },
		func() error { _, err := fixture.task.Ask(context.Background(), second); return err },
	)
	mustOneWinner(t, results)
}

func TestTaskReplyRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "reply-race-setup", contentHashA))); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	first := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "reply-race-a", contentHashA))
	second := fixture.replyUnit(scope, testTask, "execution-3", 3, 1, 1, fixture.receipt("task.reply", "reply-race-b", contentHashA))
	results := runConcurrently(
		func() error { _, err := fixture.task.Reply(context.Background(), first); return err },
		func() error { _, err := fixture.task.Reply(context.Background(), second); return err },
	)
	mustOneWinner(t, results)
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 2 {
		t.Fatalf("reply race committed %d executions; want 2", count)
	}
}

func TestTaskEditRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	first := task.EditUnit{
		Owner: scope, TaskID: testTask, Kind: task.EditContent, ExpectedContentVersion: 1,
		Title: "a", Receipt: fixture.receipt("task.edit", "edit-race-a", contentHashA),
	}
	second := task.EditUnit{
		Owner: scope, TaskID: testTask, Kind: task.EditContent, ExpectedContentVersion: 1,
		Title: "b", Receipt: fixture.receipt("task.edit", "edit-race-b", contentHashA),
	}
	results := runConcurrently(
		func() error { _, err := fixture.task.Edit(context.Background(), first); return err },
		func() error { _, err := fixture.task.Edit(context.Background(), second); return err },
	)
	mustOneWinner(t, results)
	if versionValue(fixture.storedTask(t, scope, testTask), "content_version") != 2 {
		t.Fatal("the edit race advanced the content version more than once")
	}
}

func TestTaskProgressRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	unit := task.ProgressUnit{Owner: scope, TaskID: testTask, ExecutionID: testExecution, ExpectedLifecycleVersion: 2, At: testNow}
	results := runConcurrently(
		func() error { _, err := fixture.task.Apply(context.Background(), unit); return err },
		func() error { _, err := fixture.task.Apply(context.Background(), unit); return err },
	)
	mustOneWinner(t, results)
}

// TestTaskAskRefusesNonOpenStatesAndReplaysReceipt proves Task.Ask pins
// state="open" in both lock and write expectations so waiting, done and
// cancelled tasks fail with fault.VersionConflict even at the current lifecycle
// version, while an equal-key receipt replay succeeds cleanly.
func TestTaskAskRefusesNonOpenStatesAndReplaysReceipt(t *testing.T) {
	t.Run("waiting conflict and equal-key replay", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
		fixture.createTask(t, scope, testTask)
		fixture.startTask(t, scope, testTask, 1, testExecution)

		initialReceipt := fixture.receipt("task.ask", "ask-initial", contentHashA)
		initialUnit := fixture.askUnit(t, scope, testTask, 2, initialReceipt)
		first, err := fixture.task.Ask(context.Background(), initialUnit)
		if err != nil {
			t.Fatalf("initial Ask: %v", err)
		}
		if first.LifecycleVersion != 3 || first.QuestionVersion != 1 || first.Replayed {
			t.Fatalf("initial ask result = %+v; want lifecycle 3 question 1 fresh", first)
		}

		before := fixture.commitCounts(t, scope)

		// A fresh Ask against the waiting task at its current lifecycle version (3)
		// must fail with VersionConflict because state is "waiting", not "open".
		freshWhileWaiting := fixture.askUnit(t, scope, testTask, 3, fixture.receipt("task.ask", "ask-while-waiting", contentHashA))
		freshWhileWaiting.QuestionID = "question-2"
		freshWhileWaiting.Question = "Another question while waiting?"
		_, err = fixture.task.Ask(context.Background(), freshWhileWaiting)
		mustClass(t, err, fault.VersionConflict)
		fixture.requireUnchanged(t, scope, before)

		got, err := fixture.task.Get(context.Background(), scope, testTask)
		if err != nil {
			t.Fatalf("Get after refused waiting ask: %v", err)
		}
		if got.State != task.StateWaiting || got.LifecycleVersion != 3 || got.Question == nil || got.Question.ID != "question-1" {
			t.Fatalf("refused waiting ask mutated task: %+v", got)
		}

		// Equal-key receipt replay with the original unit succeeds.
		replayed, err := fixture.task.Ask(context.Background(), initialUnit)
		if err != nil {
			t.Fatalf("replayed Ask (original unit): %v", err)
		}
		if !replayed.Replayed || replayed.LifecycleVersion != first.LifecycleVersion || replayed.QuestionVersion != first.QuestionVersion {
			t.Fatalf("replayed Ask = %+v; want replay of %+v", replayed, first)
		}
		fixture.requireUnchanged(t, scope, before)

		// Equal-key receipt replay rebuilt against the now-terminal execution
		// (matching Service.Ask retry behavior) also succeeds.
		rebuiltReplay := fixture.askUnit(t, scope, testTask, 2, initialReceipt)
		replayedRebuilt, err := fixture.task.Ask(context.Background(), rebuiltReplay)
		if err != nil {
			t.Fatalf("replayed Ask (rebuilt unit): %v", err)
		}
		if !replayedRebuilt.Replayed || replayedRebuilt.LifecycleVersion != first.LifecycleVersion || replayedRebuilt.QuestionVersion != first.QuestionVersion {
			t.Fatalf("replayed Ask (rebuilt) = %+v; want replay of %+v", replayedRebuilt, first)
		}
		fixture.requireUnchanged(t, scope, before)

		// Same key with changed content hash fails with ChangedIdempotencyContent.
		changed := rebuiltReplay
		changed.Receipt = fixture.receipt("task.ask", "ask-initial", contentHashB)
		_, err = fixture.task.Ask(context.Background(), changed)
		mustClass(t, err, fault.ChangedIdempotencyContent)
		fixture.requireUnchanged(t, scope, before)
	})

	t.Run("done conflict and prior ask replay", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
		fixture.createTask(t, scope, testTask)
		fixture.startTask(t, scope, testTask, 1, testExecution)

		initialAskReceipt := fixture.receipt("task.ask", "ask-before-done", contentHashA)
		initialAskUnit := fixture.askUnit(t, scope, testTask, 2, initialAskReceipt)
		firstAsk, err := fixture.task.Ask(context.Background(), initialAskUnit)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		reply := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "reply-before-done", contentHashA))
		if _, err := fixture.task.Reply(context.Background(), reply); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if _, err := fixture.task.Apply(context.Background(), task.ProgressUnit{
			Owner:                    scope,
			TaskID:                   testTask,
			ExecutionID:              "execution-2",
			ExpectedLifecycleVersion: 4,
			At:                       testNow,
		}); err != nil {
			t.Fatalf("Apply progress: %v", err)
		}

		before := fixture.commitCounts(t, scope)

		// Fresh Ask on done task at current lifecycle version (5) fails with VersionConflict.
		askOnDone := fixture.askUnit(t, scope, testTask, 5, fixture.receipt("task.ask", "ask-while-done", contentHashA))
		_, err = fixture.task.Ask(context.Background(), askOnDone)
		mustClass(t, err, fault.VersionConflict)
		fixture.requireUnchanged(t, scope, before)

		got, err := fixture.task.Get(context.Background(), scope, testTask)
		if err != nil {
			t.Fatalf("Get after refused done ask: %v", err)
		}
		if got.State != task.StateDone || got.LifecycleVersion != 5 || got.Question != nil {
			t.Fatalf("refused done ask mutated task: %+v", got)
		}

		// Equal-key replay of the earlier Ask still succeeds even after the task is done.
		replayed, err := fixture.task.Ask(context.Background(), initialAskUnit)
		if err != nil {
			t.Fatalf("replayed Ask after done: %v", err)
		}
		if !replayed.Replayed || replayed.LifecycleVersion != firstAsk.LifecycleVersion || replayed.QuestionVersion != firstAsk.QuestionVersion {
			t.Fatalf("replayed Ask after done = %+v; want replay of %+v", replayed, firstAsk)
		}
		fixture.requireUnchanged(t, scope, before)
	})

	t.Run("cancelled conflict and prior ask replay", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
		fixture.createTask(t, scope, testTask)
		fixture.startTask(t, scope, testTask, 1, testExecution)

		initialAskReceipt := fixture.receipt("task.ask", "ask-before-cancel", contentHashA)
		initialAskUnit := fixture.askUnit(t, scope, testTask, 2, initialAskReceipt)
		firstAsk, err := fixture.task.Ask(context.Background(), initialAskUnit)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if _, err := fixture.task.Cancel(context.Background(), fixture.cancelUnit(t, scope, testTask, 3, fixture.receipt("task.cancel", "cancel-waiting", contentHashA))); err != nil {
			t.Fatalf("Cancel waiting task: %v", err)
		}

		before := fixture.commitCounts(t, scope)

		// Fresh Ask on cancelled task at current lifecycle version (4) fails with VersionConflict.
		askOnCancelled := fixture.askUnit(t, scope, testTask, 4, fixture.receipt("task.ask", "ask-while-cancelled", contentHashA))
		_, err = fixture.task.Ask(context.Background(), askOnCancelled)
		mustClass(t, err, fault.VersionConflict)
		fixture.requireUnchanged(t, scope, before)

		got, err := fixture.task.Get(context.Background(), scope, testTask)
		if err != nil {
			t.Fatalf("Get after refused cancelled ask: %v", err)
		}
		if got.State != task.StateCancelled || got.LifecycleVersion != 4 || got.Question != nil {
			t.Fatalf("refused cancelled ask mutated task: %+v", got)
		}

		// Equal-key replay of the earlier Ask still succeeds even after the task is cancelled.
		replayed, err := fixture.task.Ask(context.Background(), initialAskUnit)
		if err != nil {
			t.Fatalf("replayed Ask after cancel: %v", err)
		}
		if !replayed.Replayed || replayed.LifecycleVersion != firstAsk.LifecycleVersion || replayed.QuestionVersion != firstAsk.QuestionVersion {
			t.Fatalf("replayed Ask after cancel = %+v; want replay of %+v", replayed, firstAsk)
		}
		fixture.requireUnchanged(t, scope, before)
	})
}

// TestTaskCancelRefusesDoneAndCancelledAndReplaysReceipt proves Task.Cancel
// permits open and waiting tasks, refuses done and cancelled tasks with
// fault.VersionConflict even at the current lifecycle version, and replays
// equal-key receipts cleanly.
func TestTaskCancelRefusesDoneAndCancelledAndReplaysReceipt(t *testing.T) {
	t.Run("done conflict", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
		fixture.createTask(t, scope, testTask)
		fixture.startTask(t, scope, testTask, 1, testExecution)
		if _, err := fixture.task.Apply(context.Background(), task.ProgressUnit{
			Owner:                    scope,
			TaskID:                   testTask,
			ExecutionID:              testExecution,
			ExpectedLifecycleVersion: 2,
			At:                       testNow,
		}); err != nil {
			t.Fatalf("Apply progress: %v", err)
		}

		before := fixture.commitCounts(t, scope)

		// Cancel on done task at current lifecycle version (3) fails with VersionConflict.
		cancelOnDone := fixture.cancelUnit(t, scope, testTask, 3, fixture.receipt("task.cancel", "cancel-while-done", contentHashA))
		_, err := fixture.task.Cancel(context.Background(), cancelOnDone)
		mustClass(t, err, fault.VersionConflict)
		fixture.requireUnchanged(t, scope, before)

		got, err := fixture.task.Get(context.Background(), scope, testTask)
		if err != nil {
			t.Fatalf("Get after refused done cancel: %v", err)
		}
		if got.State != task.StateDone || got.LifecycleVersion != 3 {
			t.Fatalf("refused done cancel mutated task: %+v", got)
		}
	})

	t.Run("cancelled conflict and equal-key replay", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
		fixture.createTask(t, scope, testTask)
		fixture.startTask(t, scope, testTask, 1, testExecution)

		initialReceipt := fixture.receipt("task.cancel", "cancel-initial", contentHashA)
		initialUnit := fixture.cancelUnit(t, scope, testTask, 2, initialReceipt)
		first, err := fixture.task.Cancel(context.Background(), initialUnit)
		if err != nil {
			t.Fatalf("initial Cancel: %v", err)
		}
		if first.LifecycleVersion != 3 || first.Replayed {
			t.Fatalf("initial cancel result = %+v; want lifecycle 3 fresh", first)
		}

		before := fixture.commitCounts(t, scope)

		// Fresh Cancel on already-cancelled task at current lifecycle version (3)
		// fails with VersionConflict.
		secondCancel := fixture.cancelUnit(t, scope, testTask, 3, fixture.receipt("task.cancel", "cancel-again", contentHashA))
		_, err = fixture.task.Cancel(context.Background(), secondCancel)
		mustClass(t, err, fault.VersionConflict)
		fixture.requireUnchanged(t, scope, before)

		got, err := fixture.task.Get(context.Background(), scope, testTask)
		if err != nil {
			t.Fatalf("Get after refused second cancel: %v", err)
		}
		if got.State != task.StateCancelled || got.LifecycleVersion != 3 {
			t.Fatalf("refused second cancel mutated task: %+v", got)
		}

		// Equal-key receipt replay with the original unit succeeds.
		replayed, err := fixture.task.Cancel(context.Background(), initialUnit)
		if err != nil {
			t.Fatalf("replayed Cancel (original unit): %v", err)
		}
		if !replayed.Replayed || replayed.LifecycleVersion != first.LifecycleVersion {
			t.Fatalf("replayed Cancel = %+v; want replay of %+v", replayed, first)
		}
		fixture.requireUnchanged(t, scope, before)

		// Equal-key receipt replay rebuilt against the now-terminal execution
		// (matching Service.Cancel retry behavior) also succeeds.
		rebuiltReplay := fixture.cancelUnit(t, scope, testTask, 2, initialReceipt)
		replayedRebuilt, err := fixture.task.Cancel(context.Background(), rebuiltReplay)
		if err != nil {
			t.Fatalf("replayed Cancel (rebuilt unit): %v", err)
		}
		if !replayedRebuilt.Replayed || replayedRebuilt.LifecycleVersion != first.LifecycleVersion {
			t.Fatalf("replayed Cancel (rebuilt) = %+v; want replay of %+v", replayedRebuilt, first)
		}
		fixture.requireUnchanged(t, scope, before)

		// Same key with changed content hash fails with ChangedIdempotencyContent.
		changed := rebuiltReplay
		changed.Receipt = fixture.receipt("task.cancel", "cancel-initial", contentHashB)
		_, err = fixture.task.Cancel(context.Background(), changed)
		mustClass(t, err, fault.ChangedIdempotencyContent)
		fixture.requireUnchanged(t, scope, before)
	})
}

// TestTaskStartAndProgressPinOpenState proves Task.Start and Task.Apply
// (progress) pin state="open" so non-open tasks at the current lifecycle
// version fail with fault.VersionConflict.
func TestTaskStartAndProgressPinOpenState(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-setup", contentHashA))); err != nil {
		t.Fatalf("setup Ask: %v", err)
	}

	before := fixture.commitCounts(t, scope)

	// Task is now waiting at lifecycle 3 with current_execution_id = testExecution.
	// Both Start and Apply at lifecycle 3 must fail with VersionConflict.
	_, err := fixture.task.Start(context.Background(), fixture.startUnit(scope, testTask, "execution-2", 3, 1, fixture.receipt("task.start", "start-while-waiting", contentHashA)))
	mustClass(t, err, fault.VersionConflict)
	fixture.requireUnchanged(t, scope, before)

	_, err = fixture.task.Apply(context.Background(), task.ProgressUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		ExecutionID:              testExecution,
		ExpectedLifecycleVersion: 3,
		At:                       testNow,
	})
	mustClass(t, err, fault.VersionConflict)
	fixture.requireUnchanged(t, scope, before)
}

// TestRealTaskServiceAskAndCancelStateGuardsThroughAdapters drives the real
// task.Service over the work adapter to verify end-to-end state checks and
// receipt replay on waiting, done and cancelled tasks.
func TestRealTaskServiceAskAndCancelStateGuardsThroughAdapters(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "State guards", PipelineID: testPipeline, Start: true,
		CallerKey: "guard-create", Content: integrationContent("guard-create"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	assignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}
	asked, err := env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "First question?", Assignment: assignment,
		CallerKey: "guard-ask-1", Content: integrationContent("guard-ask-1"),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Equal-key retry on waiting replays; fresh Ask on waiting at lifecycle 3 conflicts.
	replayedAsk, err := env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "First question?", Assignment: assignment,
		CallerKey: "guard-ask-1", Content: integrationContent("guard-ask-1"),
	})
	if err != nil {
		t.Fatalf("Ask replay: %v", err)
	}
	if !replayedAsk.Replayed || replayedAsk.LifecycleVersion != asked.LifecycleVersion {
		t.Fatalf("Ask replay = %+v; want replay of %+v", replayedAsk, asked)
	}

	_, err = env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		Question: "Second question while waiting?", Assignment: assignment,
		CallerKey: "guard-ask-2", Content: integrationContent("guard-ask-2"),
	})
	mustClass(t, err, fault.VersionConflict)

	// Cancel from waiting succeeds; fresh Cancel on cancelled at lifecycle 4 conflicts;
	// equal-key Cancel retry replays.
	cancelled, err := env.service.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		CallerKey: "guard-cancel-1", Content: integrationContent("guard-cancel-1"),
	})
	if err != nil {
		t.Fatalf("Cancel from waiting: %v", err)
	}

	_, err = env.service.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 4,
		CallerKey: "guard-cancel-2", Content: integrationContent("guard-cancel-2"),
	})
	mustClass(t, err, fault.VersionConflict)

	replayedCancel, err := env.service.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		CallerKey: "guard-cancel-1", Content: integrationContent("guard-cancel-1"),
	})
	if err != nil {
		t.Fatalf("Cancel replay: %v", err)
	}
	if !replayedCancel.Replayed || replayedCancel.LifecycleVersion != cancelled.LifecycleVersion {
		t.Fatalf("Cancel replay = %+v; want replay of %+v", replayedCancel, cancelled)
	}

	// Service.Ask on cancelled fails immediately with VersionConflict.
	_, err = env.service.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 4,
		Question: "Ask after cancel?", Assignment: assignment,
		CallerKey: "guard-ask-after-cancel", Content: integrationContent("guard-ask-after-cancel"),
	})
	mustClass(t, err, fault.VersionConflict)
}
