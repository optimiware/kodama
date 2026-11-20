package work

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// TestTaskReplyPersistsAnswerInConversation proves ask and reply both persist
// durable task_messages rows (preserving question references), so the reader
// and history retain both the asked question and the human reply in order.
func TestTaskReplyPersistsAnswerInConversation(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	ask := fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "answer-setup", contentHashA))
	ask.References = []string{"commit=abcdef123456"}
	if _, err := fixture.task.Ask(context.Background(), ask); err != nil {
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
	if len(got.Messages) != 2 {
		t.Fatalf("task messages = %+v; want the asked question and the reply answer", got.Messages)
	}
	questionMessage := got.Messages[0]
	if questionMessage.Body != ask.Question || questionMessage.Author != testPrincipal || !questionMessage.At.Equal(ask.Receipt.At) || !slices.Equal(questionMessage.References, ask.References) {
		t.Fatalf("persisted question = %+v; want body=%q refs=%v", questionMessage, ask.Question, ask.References)
	}
	answerMessage := got.Messages[1]
	if answerMessage.Body != unit.Answer || answerMessage.Author != testPrincipal || !answerMessage.At.Equal(unit.At) || len(answerMessage.References) != 0 {
		t.Fatalf("persisted answer = %+v", answerMessage)
	}
	page, err := fixture.task.History(context.Background(), scope, testTask, task.HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var historyMessages []task.Message
	for _, entry := range page.Entries {
		if entry.Kind == task.HistoryMessage && entry.Message != nil {
			historyMessages = append(historyMessages, *entry.Message)
		}
	}
	if len(historyMessages) != 2 ||
		historyMessages[0].Body != ask.Question || !slices.Equal(historyMessages[0].References, ask.References) ||
		historyMessages[1].Body != unit.Answer {
		t.Fatalf("history messages = %+v; want question (with refs) followed by answer", historyMessages)
	}
	conversation, err := json.Marshal(got.Messages)
	if err != nil {
		t.Fatalf("marshal conversation: %v", err)
	}
	if !bytes.Contains(conversation, []byte(ask.Question)) || !bytes.Contains(conversation, []byte("commit=abcdef123456")) || !bytes.Contains(conversation, []byte(unit.Answer)) {
		t.Fatalf("conversation bytes omit the question, reference or answer: %s", conversation)
	}
}

// TestTaskSecondReplySnapshotIncludesEarlierAnswer proves the reader exposes
// every earlier question (with references) and answer in chronological order,
// so the conversation bytes a later reply admits contain them all.
func TestTaskSecondReplySnapshotIncludesEarlierAnswer(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)

	firstAskReceipt := fixture.receipt("task.ask", "second-answer-setup", contentHashA)
	firstAskReceipt.At = testNow
	firstAsk := fixture.askUnit(t, scope, testTask, 2, firstAskReceipt)
	firstAsk.Question = "First question?"
	firstAsk.References = []string{"commit=1111111"}
	if _, err := fixture.task.Ask(context.Background(), firstAsk); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	first := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "first-answer", contentHashA))
	first.Answer = "First answer."
	first.At = testNow.Add(time.Minute)
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
	secondAskReceipt := fixture.receipt("task.ask", "second-answer-ask", contentHashA)
	secondAskReceipt.At = testNow.Add(2 * time.Minute)
	secondAsk := fixture.askUnit(t, scope, testTask, 4, secondAskReceipt)
	secondAsk.Question = "Second question?"
	secondAsk.References = []string{"commit=2222222"}
	if _, err := fixture.task.Ask(context.Background(), secondAsk); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	second := fixture.replyUnit(scope, testTask, "execution-3", 5, 1, 1, fixture.receipt("task.reply", "second-answer", contentHashA))
	second.Answer = "Second answer."
	second.At = testNow.Add(3 * time.Minute)
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
	if !bytes.Contains(admitted.Snapshot.Conversation, []byte(firstAsk.Question)) ||
		!bytes.Contains(admitted.Snapshot.Conversation, []byte("commit=1111111")) ||
		!bytes.Contains(admitted.Snapshot.Conversation, []byte(first.Answer)) {
		t.Fatalf("second reply snapshot omits the first question/reference/answer: %s", admitted.Snapshot.Conversation)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get after second reply: %v", err)
	}
	if len(got.Messages) != 4 ||
		got.Messages[0].Body != firstAsk.Question || !slices.Equal(got.Messages[0].References, firstAsk.References) ||
		got.Messages[1].Body != first.Answer ||
		got.Messages[2].Body != secondAsk.Question || !slices.Equal(got.Messages[2].References, secondAsk.References) ||
		got.Messages[3].Body != second.Answer {
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

// TestTaskAskReplyRoundTripThroughServicePreservesQuestionAndReferences proves
// the real task service + work adapter round-trip: ask with durable references,
// waiting state, reply, and fresh execution snapshot all preserve both the
// asked question (with references) and the reply in chronological order across
// /kodema/task.json, task.read and task.history.
func TestTaskAskReplyRoundTripThroughServicePreservesQuestionAndReferences(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	actor := env.actor()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Configure target environment", PipelineID: testPipeline, Start: true,
		CallerKey: "roundtrip-create", Content: integrationContent("roundtrip-create"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const (
		questionText = "Which target environment should I configure (staging or prod)?"
		commitRef    = "commit=deadbeef0123456789abcdef0123456789abcdef"
		answerText   = "Use TARGET_ENV=staging for this deployment."
	)
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor:                    actor,
		Scope:                    env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: created.LifecycleVersion,
		Question:                 questionText,
		Assignment:               request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		References:               []string{commitRef},
		CallerKey:                "roundtrip-ask",
		Content:                  integrationContent("roundtrip-ask"),
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	waiting, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("Get while waiting: %v", err)
	}
	if waiting.State != task.StateWaiting || waiting.Question == nil {
		t.Fatalf("waiting task = %+v; want waiting with question", waiting)
	}
	if !slices.Equal(waiting.Question.References, []string{commitRef}) {
		t.Fatalf("waiting question references = %v; want [%s]", waiting.Question.References, commitRef)
	}

	replied, err := env.service.Reply(ctx, task.ReplyCommand{
		Actor:                    actor,
		Scope:                    env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: waiting.LifecycleVersion,
		QuestionID:               waiting.Question.ID,
		ExpectedQuestionVersion:  waiting.Question.Version,
		Answer:                   answerText,
		CallerKey:                "roundtrip-reply",
		Content:                  integrationContent("roundtrip-reply"),
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// Verify /kodema/task.json snapshot admitted for the fresh execution.
	execRow, ok := env.fixture.storedRow(t, env.scope, "executions", string(replied.ExecutionID))
	if !ok {
		t.Fatalf("reply execution %q not found", replied.ExecutionID)
	}
	admitted, err := snapshotdoc.Decode(bytesValue(execRow, "snapshot"))
	if err != nil {
		t.Fatalf("decode reply snapshot: %v", err)
	}
	var snapshotMessages []task.Message
	if err := json.Unmarshal(admitted.Snapshot.ConversationBytes(), &snapshotMessages); err != nil {
		t.Fatalf("decode admitted conversation %q: %v", admitted.Snapshot.ConversationBytes(), err)
	}
	if len(snapshotMessages) != 2 {
		t.Fatalf("admitted conversation = %+v; want 2 messages (question + reply)", snapshotMessages)
	}
	if snapshotMessages[0].Author != actor.ID || snapshotMessages[0].Body != questionText || !slices.Equal(snapshotMessages[0].References, []string{commitRef}) {
		t.Fatalf("admitted conversation[0] = %+v; want asked question with ref %q", snapshotMessages[0], commitRef)
	}
	if snapshotMessages[1].Author != actor.ID || snapshotMessages[1].Body != answerText || len(snapshotMessages[1].References) != 0 {
		t.Fatalf("admitted conversation[1] = %+v; want reply answer without refs", snapshotMessages[1])
	}

	// Verify task.read preserves both messages in chronological order.
	afterRead, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("Get after reply: %v", err)
	}
	if afterRead.Question != nil {
		t.Fatalf("question was not cleared after reply: %+v", afterRead.Question)
	}
	if !reflect.DeepEqual(afterRead.Messages, snapshotMessages) {
		t.Fatalf("task.read messages = %+v; want %+v", afterRead.Messages, snapshotMessages)
	}

	// Verify task.history retains both the answered question (with references)
	// and the reply.
	historyPage, err := env.fixture.task.History(ctx, env.scope, created.TaskID, task.HistoryQuery{Limit: 20})
	if err != nil {
		t.Fatalf("History after reply: %v", err)
	}
	var historyMessages []task.Message
	for _, entry := range historyPage.Entries {
		if entry.Kind == task.HistoryMessage && entry.Message != nil {
			historyMessages = append(historyMessages, *entry.Message)
		}
	}
	if !reflect.DeepEqual(historyMessages, snapshotMessages) {
		t.Fatalf("task.history messages = %+v; want %+v", historyMessages, snapshotMessages)
	}
}

// TestTaskAskReplayAndRollbackKeepMessagesConsistent proves ask replay does not
// insert a duplicate task_messages row and a rolled-back ask leaves none.
func TestTaskAskReplayAndRollbackKeepMessagesConsistent(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)

	ask := fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "ask-replay-key", contentHashA))
	ask.References = []string{"commit=1234567890"}

	// Rollback first: must leave zero messages.
	fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "transient failure"))
	if _, err := fixture.task.Ask(context.Background(), ask); err == nil {
		t.Fatal("failed ask succeeded")
	}
	if count := fixture.countRows(t, scope, "task_messages", map[string]any{"task_id": string(testTask)}); count != 0 {
		t.Fatalf("rolled-back ask left %d task_messages rows; want 0", count)
	}

	// Commit, then replay with the same receipt: must leave exactly one message.
	if _, err := fixture.task.Ask(context.Background(), ask); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	replayed, err := fixture.task.Ask(context.Background(), ask)
	if err != nil {
		t.Fatalf("replayed Ask: %v", err)
	}
	if !replayed.Replayed {
		t.Fatalf("expected replayed ask result: %+v", replayed)
	}
	if count := fixture.countRows(t, scope, "task_messages", map[string]any{"task_id": string(testTask)}); count != 1 {
		t.Fatalf("replayed ask left %d task_messages rows; want 1", count)
	}
}

// TestTaskReplyPreservesLegacyWaitingQuestionWithoutPriorMessage proves a reply
// against a pre-existing waiting task whose question was never written to
// task_messages still persists both the question (with references) and answer.
func TestTaskReplyPreservesLegacyWaitingQuestionWithoutPriorMessage(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)

	assignment, err := jsonColumn(request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator})
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	refs, err := jsonColumn([]string{"commit=legacy999"})
	if err != nil {
		t.Fatalf("encode references: %v", err)
	}
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{taskWrite(testTask, map[string]any{"lifecycle_version": int64(2)}, map[string]any{
			"state":               string(task.StateWaiting),
			"lifecycle_version":   int64(3),
			"question_id":         "question-1",
			"question_body":       "Legacy waiting question?",
			"question_assignment": assignment,
			"question_references": refs,
			"question_version":    int64(1),
			"question_asked_by":   string(testPrincipal),
			"question_at":         testNow,
		})},
	}); err != nil {
		t.Fatalf("seed legacy waiting task: %v", err)
	}

	unit := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "legacy-reply", contentHashA))
	if _, err := fixture.task.Reply(context.Background(), unit); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages after legacy reply = %+v; want 2 (question + answer)", got.Messages)
	}
	if got.Messages[0].Body != "Legacy waiting question?" || !slices.Equal(got.Messages[0].References, []string{"commit=legacy999"}) {
		t.Fatalf("legacy question message = %+v", got.Messages[0])
	}
	if got.Messages[1].Body != unit.Answer {
		t.Fatalf("legacy answer message = %+v", got.Messages[1])
	}
}
