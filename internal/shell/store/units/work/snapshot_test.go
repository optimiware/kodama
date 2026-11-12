package work

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// loadExecution loads one started execution through the execution adapter's
// Kernel engine, exactly as a restart/recovery pass would.
func (f *fixture) loadExecution(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	aggregate, err := executionstore.NewEngine(f.store.Controller()).Load(context.Background(), executionID)
	if err != nil {
		t.Fatalf("Engine.Load(%s): %v", executionID, err)
	}
	return aggregate
}

// assertAdmittedSnapshot asserts the shared snapshot document round-trips every
// admitted fact through the execution adapter.
func assertAdmittedSnapshot(t *testing.T, aggregate kernel.Execution, want pipeline.Snapshot, wantActions []pipeline.AdmittedAction) {
	t.Helper()
	if aggregate.WorkspaceID != snapshotdoc.DeriveWorkspaceID(aggregate.ID) {
		t.Fatalf("workspace = %q; want %q", aggregate.WorkspaceID, snapshotdoc.DeriveWorkspaceID(aggregate.ID))
	}
	if string(aggregate.Snapshot.YAML()) != string(want.YAMLBytes()) {
		t.Fatalf("admitted YAML = %q; want %q", aggregate.Snapshot.YAML(), want.YAMLBytes())
	}
	if aggregate.Snapshot.YAMLDigest() != want.YAMLSHA256 {
		t.Fatal("admitted YAML digest did not round-trip")
	}
	gotInputs := aggregate.Snapshot.Inputs()
	if len(gotInputs) != len(want.Inputs) {
		t.Fatalf("admitted inputs = %+v; want %+v", gotInputs, want.Inputs)
	}
	for key, value := range want.Inputs {
		if gotInputs[string(key)] != value {
			t.Fatalf("admitted input %q = %q; want %q", key, gotInputs[string(key)], value)
		}
	}
	if aggregate.Snapshot.TaskID() != want.TaskID {
		t.Fatalf("admitted task id = %q; want %q", aggregate.Snapshot.TaskID(), want.TaskID)
	}
	gotTask, gotPresent := aggregate.Snapshot.Task()
	wantTask, wantPresent := want.ConversationBytes(), want.ConversationBytes() != nil
	if gotPresent != wantPresent || string(gotTask) != string(wantTask) {
		t.Fatalf("admitted task bytes = %q present=%v; want %q present=%v", gotTask, gotPresent, wantTask, wantPresent)
	}
	if len(aggregate.Snapshot.Pipeline().Env) != 1 || aggregate.Snapshot.Pipeline().Env[0].Name != "LOG_LEVEL" {
		t.Fatalf("admitted env = %+v", aggregate.Snapshot.Pipeline().Env)
	}
	if len(aggregate.Snapshot.Runtime().Capabilities) != 1 || aggregate.Snapshot.Runtime().Capabilities[0] != "linux/amd64" {
		t.Fatalf("admitted requirements = %+v", aggregate.Snapshot.Runtime().Capabilities)
	}
	if len(aggregate.Actions) != len(wantActions) {
		t.Fatalf("actions = %d; want %d", len(aggregate.Actions), len(wantActions))
	}
	for index, action := range aggregate.Actions {
		want := wantActions[index]
		if action.ID != snapshotdoc.DeriveActionID(aggregate.ID, index) {
			t.Fatalf("action %d id = %q; want %q", index, action.ID, snapshotdoc.DeriveActionID(aggregate.ID, index))
		}
		if action.Name != want.Name || action.Kind != want.Kind || action.Timeout != want.Timeout || action.State != kernel.ActionPending {
			t.Fatalf("action %d = %+v; want %+v", index, action, want)
		}
		switch want.Kind {
		case definition.ActionKindContainer:
			if action.Container == nil || action.Container.Definition.Image != want.Container.Image {
				t.Fatalf("action %d container = %+v", index, action.Container)
			}
		case definition.ActionKindRequest:
			if action.Request == nil || action.Request.Definition.Assignment != want.Request.Assignment {
				t.Fatalf("action %d request = %+v", index, action.Request)
			}
		}
	}
}

// assertStartedRows asserts the execution and task rows agree with the start.
func assertStartedRows(t *testing.T, fixture *fixture, executionID id.ExecutionID, taskID id.TaskID) {
	t.Helper()
	executionRow, ok := fixture.storedRow(t, testScope(), "executions", string(executionID))
	if !ok {
		t.Fatal("the execution row was not committed")
	}
	if textValue(executionRow, "state") != string(kernel.ExecutionQueued) || versionValue(executionRow, "version") != 1 {
		t.Fatalf("execution row = %+v", executionRow.Values)
	}
	if textValue(executionRow, "workspace_id") != string(snapshotdoc.DeriveWorkspaceID(executionID)) {
		t.Fatalf("execution workspace_id = %q", textValue(executionRow, "workspace_id"))
	}
	if taskID != "" {
		if textValue(executionRow, "task_id") != string(taskID) {
			t.Fatalf("execution task_id = %q; want %q", textValue(executionRow, "task_id"), taskID)
		}
		taskRow := fixture.storedTask(t, testScope(), taskID)
		if textValue(taskRow, "current_execution_id") != string(executionID) {
			t.Fatalf("task current execution = %q; want %q", textValue(taskRow, "current_execution_id"), executionID)
		}
	}
}

func TestTaskStartSnapshotRoundTripsThroughExecutionEngine(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\nversion: 1\n"), 1)
	fixture.createTask(t, scope, testTask)
	snapshot := richSnapshot([]byte("kind: Pipeline\nversion: 1\n"), []byte("conversation\x00bytes"))
	actions := []task.AdmittedAction{testAction(0), testRequestAction(1)}
	unit := task.StartUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: 1,
		ExpectedPipelineVersion:  1,
		Snapshot:                 snapshot,
		Actions:                  actions,
		Initiator:                testPrincipal,
		ExecutionPrincipal:       "execution-principal-1",
		NewExecutionID:           testExecution,
		At:                       testNow,
		Receipt:                  fixture.receipt("task.start", "snapshot-start", contentHashA),
		Audit:                    []event.Audit{fixture.audit(scope, "task.start")},
		Events:                   []event.Envelope{fixture.event(scope, string(testExecution), 1)},
	}
	if _, err := fixture.task.Start(context.Background(), unit); err != nil {
		t.Fatalf("Start: %v", err)
	}
	aggregate := fixture.loadExecution(t, testExecution)
	assertAdmittedSnapshot(t, aggregate, snapshot, []pipeline.AdmittedAction{actions[0], actions[1]})
	if aggregate.Snapshot.TaskID() != testTask {
		t.Fatalf("task id = %q", aggregate.Snapshot.TaskID())
	}
	assertStartedRows(t, fixture, testExecution, testTask)
}

func TestTaskCreateAndStartSnapshotRoundTripsThroughExecutionEngine(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\nversion: 1\n"), 1)
	snapshot := richSnapshot([]byte("kind: Pipeline\nversion: 1\n"), []byte("create-conversation"))
	actions := []task.AdmittedAction{testAction(0), testRequestAction(1)}
	start := task.StartUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: 0,
		ExpectedPipelineVersion:  1,
		Snapshot:                 snapshot,
		Actions:                  actions,
		Initiator:                testPrincipal,
		ExecutionPrincipal:       "execution-principal-1",
		NewExecutionID:           testExecution,
		At:                       testNow,
	}
	if _, err := fixture.task.Create(context.Background(), task.CreateUnit{
		Owner:            scope,
		TaskID:           testTask,
		Title:            "Fix the build",
		SelectedPipeline: testPipeline,
		Inputs:           map[id.InputKey]string{"ref": "main"},
		Initiator:        testPrincipal,
		At:               testNow,
		Start:            &start,
		Receipt:          fixture.receipt("task.create", "snapshot-create", contentHashA),
		Audit:            []event.Audit{fixture.audit(scope, "task.create")},
		Events:           []event.Envelope{fixture.event(scope, string(testExecution), 1)},
	}); err != nil {
		t.Fatalf("Create with start: %v", err)
	}
	aggregate := fixture.loadExecution(t, testExecution)
	assertAdmittedSnapshot(t, aggregate, snapshot, []pipeline.AdmittedAction{actions[0], actions[1]})
	assertStartedRows(t, fixture, testExecution, testTask)
}

func TestTaskReplySnapshotRoundTripsThroughExecutionEngine(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "snapshot-ask", contentHashA))); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	snapshot := richSnapshot([]byte("kind: Pipeline\nversion: 2\n"), []byte("reply-conversation"))
	actions := []task.AdmittedAction{testAction(0), testRequestAction(1)}
	unit := task.ReplyUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		PipelineID:               testPipeline,
		ExpectedLifecycleVersion: 3,
		QuestionID:               "question-1",
		ExpectedQuestionVersion:  1,
		Assignment:               fixture.askUnit(t, scope, testTask, 3, fixture.receipt("task.ask", "unused", contentHashA)).Assignment,
		Answer:                   "Target main.",
		Initiator:                testPrincipal,
		ExpectedPipelineVersion:  1,
		Snapshot:                 snapshot,
		Actions:                  actions,
		ExecutionPrincipal:       "execution-principal-2",
		NewExecutionID:           "execution-2",
		At:                       testNow.Add(time.Minute),
		Receipt:                  fixture.receipt("task.reply", "snapshot-reply", contentHashA),
		Audit:                    []event.Audit{fixture.audit(scope, "task.reply")},
		Events:                   []event.Envelope{fixture.event(scope, "execution-2", 1)},
	}
	if _, err := fixture.task.Reply(context.Background(), unit); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	aggregate := fixture.loadExecution(t, "execution-2")
	assertAdmittedSnapshot(t, aggregate, snapshot, []pipeline.AdmittedAction{actions[0], actions[1]})
	assertStartedRows(t, fixture, "execution-2", testTask)
}

func TestTriggerStartSnapshotRoundTripsThroughExecutionEngine(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)); err != nil {
		t.Fatalf("Select: %v", err)
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte("kind: Pipeline\nversion: 1\n"),
		Inputs:       map[id.InputKey]string{"ref": "refs/heads/main"},
		Env:          []definition.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}},
		Requirements: []string{"linux/amd64"},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	actions := []pipeline.AdmittedAction{testAction(0), testRequestAction(1)}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	start.Admission = pipeline.AdmissionResult{PipelineID: testPipeline, PipelineVersion: 1, Snapshot: snapshot, Actions: actions}
	start.Events = []event.Envelope{fixture.event(scope, "execution-1", 1)}
	if _, err := fixture.triggers.Start(context.Background(), start); err != nil {
		t.Fatalf("Start: %v", err)
	}
	aggregate := fixture.loadExecution(t, "execution-1")
	assertAdmittedSnapshot(t, aggregate, snapshot, actions)
	if _, present := aggregate.Snapshot.Task(); present {
		t.Fatal("a task-free trigger execution must not carry task bytes")
	}
	if aggregate.Snapshot.TaskID() != "" {
		t.Fatalf("task-free execution task id = %q", aggregate.Snapshot.TaskID())
	}
	assertStartedRows(t, fixture, "execution-1", "")
}
