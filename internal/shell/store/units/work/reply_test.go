package work

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestTaskReplyPersistsAnswerInConversation proves a reply persists its answer
// as a task_messages row in the same unit, so the reader and history expose it.
func TestTaskReplyPersistsAnswerInConversation(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "answer-setup", contentHashA))); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	unit := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "answer", contentHashA))
	if _, err := fixture.task.Reply(context.Background(), unit); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("task messages = %+v; want the reply answer", got.Messages)
	}
	message := got.Messages[0]
	if message.Body != unit.Answer || message.Author != testPrincipal || !message.At.Equal(unit.At) {
		t.Fatalf("persisted answer = %+v", message)
	}
	page, err := fixture.task.History(context.Background(), scope, testTask, task.HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	found := false
	for _, entry := range page.Entries {
		if entry.Kind == task.HistoryMessage && entry.Message != nil && entry.Message.Body == unit.Answer {
			found = true
		}
	}
	if !found {
		t.Fatalf("history omits the reply answer: %+v", page.Entries)
	}
	conversation, err := json.Marshal(got.Messages)
	if err != nil {
		t.Fatalf("marshal conversation: %v", err)
	}
	if !bytes.Contains(conversation, []byte(unit.Answer)) {
		t.Fatalf("conversation bytes omit the answer: %s", conversation)
	}
}

// TestTaskSecondReplySnapshotIncludesEarlierAnswer proves the reader exposes the
// earlier answer, so the conversation bytes a later reply admits contain it.
func TestTaskSecondReplySnapshotIncludesEarlierAnswer(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "second-answer-setup", contentHashA))); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	first := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "first-answer", contentHashA))
	if _, err := fixture.task.Reply(context.Background(), first); err != nil {
		t.Fatalf("first Reply: %v", err)
	}
	current, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after first reply: %v", err)
	}
	conversation, err := json.Marshal(current.Messages)
	if err != nil {
		t.Fatalf("marshal conversation: %v", err)
	}
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 4, fixture.receipt("task.ask", "second-answer-ask", contentHashA))); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	second := fixture.replyUnit(scope, testTask, "execution-3", 5, 1, 1, fixture.receipt("task.reply", "second-answer", contentHashA))
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte("kind: Pipeline\nversion: 2\n"),
		Inputs:       map[id.InputKey]string{"ref": "main"},
		TaskID:       testTask,
		Conversation: conversation,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	second.Snapshot = snapshot
	if _, err := fixture.task.Reply(context.Background(), second); err != nil {
		t.Fatalf("second Reply: %v", err)
	}
	execution, ok := fixture.storedRow(t, scope, "executions", "execution-3")
	if !ok {
		t.Fatal("the second reply did not commit its execution")
	}
	admitted, err := snapshotdoc.Decode(bytesValue(execution, "snapshot"))
	if err != nil {
		t.Fatalf("decode second snapshot: %v", err)
	}
	if !bytes.Contains(admitted.Snapshot.Conversation, []byte(first.Answer)) {
		t.Fatalf("second reply snapshot omits the first answer: %s", admitted.Snapshot.Conversation)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after second reply: %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Body != first.Answer || got.Messages[1].Body != second.Answer {
		t.Fatalf("conversation after two replies = %+v", got.Messages)
	}
}

// TestTaskSelectPipelineRecordsStartAuthority proves a select_pipeline edit
// persists the selecting principal as the start authority the next start reads.
func TestTaskSelectPipelineRecordsStartAuthority(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.createTask(t, scope, testTask)
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	const selector = id.PrincipalID("principal-2")
	if _, err := fixture.task.Edit(context.Background(), task.EditUnit{
		Owner:                    scope,
		TaskID:                   testTask,
		Kind:                     task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1,
		ExpectedPipelineVersion:  1,
		PipelineID:               testPipeline,
		Inputs:                   map[id.InputKey]string{"ref": "release"},
		StartAuthority:           selector,
		Receipt:                  fixture.receipt("task.select_pipeline", "select-authority", contentHashA),
		Audit:                    []event.Audit{fixture.audit(scope, "task.select_pipeline")},
	}); err != nil {
		t.Fatalf("select pipeline: %v", err)
	}
	row := fixture.storedTask(t, scope, testTask)
	if textValue(row, "initiator_id") != string(selector) {
		t.Fatalf("stored start authority = %q; want %q", textValue(row, "initiator_id"), selector)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Initiator != selector {
		t.Fatalf("read start authority = %q; want %q", got.Initiator, selector)
	}
	if got.SelectedPipeline != testPipeline || got.LifecycleVersion != 2 || got.Inputs["ref"] != "release" {
		t.Fatalf("selection did not round-trip: %+v", got)
	}
}

// TestReplyRefusesOperatorOnlyInitiatorWithFullStartRead proves the reply path
// keeps its own fail-closed guard: even a reply unit whose initiator start read
// still names the full task.start grant set cannot create a fresh execution for
// an operator-only initiator. The reply is a fresh start, so the conditional
// failed-task retry grant can never apply to it.
func TestReplyRefusesOperatorOnlyInitiatorWithFullStartRead(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "operator-initiator-setup", contentHashA))); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)

	unit := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "operator-initiator", contentHashA))
	unit.Initiator = testPrincipal
	// Deliberately carry the full registry grant set for the start read, and a
	// later task.reply read after it: the guard must find the task.start read by
	// operation, not assume it is the last read in the unit.
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "task.start", testPrincipal, operatorRequires()),
		{Operation: "task.reply", Check: authorize.CheckTaskVersion, Scope: scope, PrincipalID: testPrincipal},
	}
	before := fixture.commitCounts(t, scope)
	_, err := fixture.task.Reply(context.Background(), unit)
	mustClass(t, err, fault.Forbidden)
	fixture.requireUnchanged(t, scope, before)
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-2"); ok {
		t.Fatal("a refused reply committed a fresh execution for an operator-only initiator")
	}
}
