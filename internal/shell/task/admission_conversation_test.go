package task_test

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// requirePresentEmptyConversation asserts the admission input the task service
// hands to pipeline.Admit for a task-bound execution with zero messages. The
// value is the present JSON empty array: non-nil so the pipeline snapshot
// distinguishes it from an absent (nil, task-free) conversation and workspace
// materialization writes /kodama/task.json, and valid JSON so a workload can
// parse that file. Every non-empty conversation is a JSON array of messages, so
// the empty array is the consistent encoding rather than zero bytes.
func requirePresentEmptyConversation(t *testing.T, label string, conversation []byte) {
	t.Helper()
	if conversation == nil {
		t.Fatalf("%s admitted a nil task conversation for a task-bound execution with zero messages; want the present JSON empty array so /kodama/task.json exists", label)
	}
	if string(conversation) != "[]" {
		t.Fatalf("%s admitted conversation = %q; want the present JSON empty array %q of a zero-message task", label, conversation, "[]")
	}
	if !json.Valid(conversation) {
		t.Fatalf("%s admitted conversation %q is not valid JSON", label, conversation)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(conversation, &messages); err != nil {
		t.Fatalf("%s admitted conversation %q is not a JSON array: %v", label, conversation, err)
	}
	if len(messages) != 0 {
		t.Fatalf("%s admitted conversation %q decodes to %d messages; want a JSON array with zero elements", label, conversation, len(messages))
	}
}

// requireAdmittedMetadata asserts the opaque task metadata travels with the
// admission alongside the conversation.
func requireAdmittedMetadata(t *testing.T, label string, raw []byte, want map[string]string) {
	t.Helper()
	if raw == nil {
		t.Fatalf("%s admitted no task metadata; want %+v", label, want)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s admitted metadata %q is not a JSON string object: %v", label, raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s admitted metadata = %+v; want %+v", label, got, want)
	}
}

// requireAdmittedSnapshotConversation asserts the admitted snapshot downstream
// of the fake admission keeps the conversation present as the JSON empty
// array. This is the value the execution store and the activation
// materialization consume, so a nil here is the reported missing
// /kodama/task.json and zero bytes would be an unparseable task file.
func requireAdmittedSnapshotConversation(t *testing.T, label string, snapshot interface {
	HasConversation() bool
	ConversationBytes() []byte
}) {
	t.Helper()
	if !snapshot.HasConversation() {
		t.Fatalf("%s admitted snapshot reports no conversation for a task-bound execution; want an admitted JSON empty-array conversation", label)
	}
	conversation := snapshot.ConversationBytes()
	if conversation == nil {
		t.Fatalf("%s admitted snapshot conversation is nil; want the present JSON empty array", label)
	}
	requirePresentEmptyConversation(t, label+" snapshot", conversation)
}

// TestStartAdmitsPresentEmptyConversation pins that an ordinary task.start of a
// task whose conversation has zero messages must hand admission the present
// JSON empty array ([]), not nil and not zero bytes. nil is reserved for a
// task-free execution, and a task-bound execution with nil bytes materializes
// no /kodama/task.json while KODAMA_TASK is still set; zero bytes would
// materialize an empty, unparseable task file.
func TestStartAdmitsPresentEmptyConversation(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := openTaskWithoutExecution()
	if len(value.Messages) != 0 {
		t.Fatalf("fixture has %d messages; this case must exercise a zero-message task", len(value.Messages))
	}
	env.reader.put(value)

	result, err := env.service.Start(t.Context(), startCommand(actor, value.LifecycleVersion))
	requireNoError(t, err, "Start")

	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	admitted := env.admission.requestsSnapshot()[0]
	if admitted.Task.TaskID != taskID {
		t.Fatalf("admitted task id = %q; want %q", admitted.Task.TaskID, taskID)
	}
	requirePresentEmptyConversation(t, "task.start", admitted.Task.Conversation)
	requireAdmittedMetadata(t, "task.start", admitted.Task.Metadata, value.Metadata)

	unit := env.ports.lastStart(t)
	if unit.Snapshot.TaskID != taskID {
		t.Fatalf("admitted snapshot task id = %q; want %q", unit.Snapshot.TaskID, taskID)
	}
	requireAdmittedSnapshotConversation(t, "task.start", unit.Snapshot)
	if unit.NewExecutionID != result.ExecutionID {
		t.Fatalf("queued execution = %q; want result %q", unit.NewExecutionID, result.ExecutionID)
	}
}

// TestCreateAndStartAdmitsPresentEmptyConversation pins the same rule on the
// create_and_start path, which admits a brand-new zero-message task. The task
// context id, the present JSON empty-array conversation and the caller's opaque
// metadata must reach admission together.
func TestCreateAndStartAdmitsPresentEmptyConversation(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Run release")
	command.Start = true

	result, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "CreateAndStart")

	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	admitted := env.admission.requestsSnapshot()[0]
	if admitted.Task.TaskID == "" || admitted.Task.TaskID != result.TaskID {
		t.Fatalf("admitted task id = %q; want the created task %q", admitted.Task.TaskID, result.TaskID)
	}
	requirePresentEmptyConversation(t, "task.create_and_start", admitted.Task.Conversation)
	requireAdmittedMetadata(t, "task.create_and_start", admitted.Task.Metadata, map[string]string{
		"stage":    "review",
		"priority": "high",
	})

	unit := env.ports.lastCreate(t)
	if unit.Start == nil {
		t.Fatal("create_and_start committed no start unit")
	}
	requireAdmittedSnapshotConversation(t, "task.create_and_start", unit.Start.Snapshot)
}

// TestReplyAdmitsConversationWithAnswer is the regression pin for the path that
// already produced a conversation: a reply hands admission the prior messages
// plus the new answer, in order, and the answer is never lost to a
// presence/emptiness collapse.
func TestReplyAdmitsConversationWithAnswer(t *testing.T) {
	prior := task.Message{
		Author: engineerPrincipal,
		At:     testNow.Add(-time.Hour),
		Body:   "Initial investigation",
	}
	cases := map[string][]task.Message{
		"answer only":   nil,
		"prior message": {prior},
	}
	for name, messages := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
			value := waitingTask(t)
			value.Messages = messages
			env.reader.put(value)
			env.executions.put(task.ExecutionView{
				ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop(),
			})

			_, err := env.service.Reply(t.Context(), replyCommand(actor, value.LifecycleVersion, questionID, 1, "Use candidate B"))
			requireNoError(t, err, "Reply")

			admitted := env.admission.requestsSnapshot()[0]
			if admitted.Task.Conversation == nil {
				t.Fatal("task.reply admitted a nil conversation; want the prior messages plus the answer")
			}
			var decoded []task.Message
			if err := json.Unmarshal(admitted.Task.Conversation, &decoded); err != nil {
				t.Fatalf("admitted conversation %q is not a message array: %v", admitted.Task.Conversation, err)
			}
			want := append(append([]task.Message(nil), messages...), task.Message{
				Author: reviewerPrincipal,
				At:     testNow,
				Body:   "Use candidate B",
			})
			if !reflect.DeepEqual(decoded, want) {
				t.Fatalf("admitted conversation = %+v; want prior messages plus the answer %+v", decoded, want)
			}

			unit := env.ports.lastReply(t)
			if unit.Snapshot.TaskID != taskID || unit.Snapshot.ConversationBytes() == nil {
				t.Fatalf("reply snapshot task = %q conversation = %q; want task %q with a present conversation", unit.Snapshot.TaskID, unit.Snapshot.ConversationBytes(), taskID)
			}
			if !json.Valid(unit.Snapshot.ConversationBytes()) {
				t.Fatalf("reply snapshot conversation %q is not valid JSON", unit.Snapshot.ConversationBytes())
			}
		})
	}
}

// TestStartReplayKeepsPresentEmptyConversationAndOneAppliedAdmission models a
// lost response after the start committed: the identical retry must return the
// original execution with one applied admission, and the re-run admission must
// still carry the present JSON empty-array conversation. Admission is
// re-invoked before the receipt-first port replay; the durable invariant is that
// only one start is applied and no second execution exists.
func TestStartReplayKeepsPresentEmptyConversationAndOneAppliedAdmission(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(openTaskWithoutExecution())

	type committed struct {
		result task.StartResult
		hash   string
	}
	var mu sync.Mutex
	receipts := make(map[string]committed)
	applied := 0
	original := task.StartResult{ExecutionID: "execution-original", LifecycleVersion: 5}
	env.ports.setStart(func(unit task.StartUnit) (task.StartResult, error) {
		mu.Lock()
		defer mu.Unlock()
		key := unit.Receipt.Key.CallerKey
		if previous, ok := receipts[key]; ok {
			if previous.hash != unit.Receipt.ContentHash {
				return task.StartResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
			}
			replayed := previous.result
			replayed.Replayed = true
			return replayed, nil
		}
		receipts[key] = committed{result: original, hash: unit.Receipt.ContentHash}
		applied++
		return task.StartResult{}, fault.New(fault.DependencyUnavailable, "response lost after commit")
	})

	if _, err := env.service.Start(t.Context(), startCommand(actor, 4)); err == nil {
		t.Fatal("first start reported success despite the lost response")
	} else {
		requireFaultClass(t, err, fault.DependencyUnavailable)
	}
	result, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "Start retry")
	if result.ExecutionID != original.ExecutionID || !result.Replayed {
		t.Fatalf("retry result = %+v; want original %q replayed", result, original.ExecutionID)
	}
	if applied != 1 {
		t.Fatalf("applied starts = %d; want exactly one", applied)
	}

	requests := env.admission.requestsSnapshot()
	if len(requests) == 0 {
		t.Fatal("no admission request was recorded")
	}
	for attempt, admitted := range requests {
		if admitted.Task.TaskID != taskID {
			t.Fatalf("attempt %d admitted task id = %q; want %q", attempt, admitted.Task.TaskID, taskID)
		}
		requirePresentEmptyConversation(t, "task.start retry", admitted.Task.Conversation)
	}
}
