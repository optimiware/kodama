package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// Regression: the CLI/app composition must obtain the task's current
// lifecycle version before a native handoff. The caller's RunInput.TaskVersion
// stays zero; the app reads the served task and uses its lifecycle version as
// the task.ask expected_version.

// handoffModel is a deterministic ModelPort fixture. The default public model
// path cannot generate tool calls, so the regression test injects this model
// instead of claiming the public path.
type handoffModel struct {
	mu         sync.Mutex
	calls      int
	question   string
	assignment string
}

func (model *handoffModel) Invoke(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	model.mu.Lock()
	model.calls++
	question := model.question
	assignment := model.assignment
	model.mu.Unlock()
	arguments, err := json.Marshal(map[string]string{"question": question, "assignment": assignment})
	if err != nil {
		return agent.ModelResponse{}, err
	}
	return agent.ModelResponse{
		ToolCalls:    []agent.ToolRequest{{Name: "task.ask", Arguments: arguments}},
		Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		FinishReason: "tool_calls",
	}, nil
}

func (model *handoffModel) callCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

// handoffTools fails the test if the runtime forwards the handoff to the tool
// port instead of the public task.ask operation.
type handoffTools struct{}

func (handoffTools) Invoke(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
	return agent.ToolResult{}, errors.New("task.ask must not reach the tool port")
}

// taskWireTransport serves the two public operations the composition needs.
// readTaskID overrides the served task.read identity so a caller can simulate a
// transport that answers with another task's record.
type taskWireTransport struct {
	mu          sync.Mutex
	invocations []api.Invocation
	lifecycle   uint64
	readTaskID  string
}

func (transport *taskWireTransport) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	transport.mu.Lock()
	transport.invocations = append(transport.invocations, invocation)
	lifecycle := transport.lifecycle
	transport.mu.Unlock()
	switch invocation.Operation {
	case "task.read":
		transport.mu.Lock()
		taskID := transport.readTaskID
		transport.mu.Unlock()
		if taskID == "" {
			taskID = "task-1"
		}
		body, err := json.Marshal(api.TaskReadResult{
			TaskID:           taskID,
			Title:            "native handoff",
			State:            "open",
			ContentVersion:   2,
			LifecycleVersion: lifecycle,
			Initiator:        "principal-1",
			CurrentExecution: "execution-1",
		})
		if err != nil {
			return api.Result{}, err
		}
		return api.Result{Operation: "task.read", Body: body}, nil
	case "task.ask":
		body, err := json.Marshal(api.TaskAskResult{
			QuestionID:       "question-1",
			LifecycleVersion: lifecycle + 1,
			QuestionVersion:  1,
		})
		if err != nil {
			return api.Result{}, err
		}
		return api.Result{Operation: "task.ask", Body: body}, nil
	default:
		return api.Result{}, fmt.Errorf("unexpected public operation %q", invocation.Operation)
	}
}

func (transport *taskWireTransport) invocationAt(index int) api.Invocation {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if index < 0 || index >= len(transport.invocations) {
		return api.Invocation{}
	}
	return transport.invocations[index]
}

func (transport *taskWireTransport) callCount() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return len(transport.invocations)
}

func decodeAppAsk(t *testing.T, raw json.RawMessage, taskID string) api.TaskAskRequest {
	t.Helper()
	value, err := api.DecodeMCPOperationRequest("task.ask", raw)
	if err != nil {
		t.Fatalf("task.ask payload fails the strict public codec: %v; payload=%s", err, raw)
	}
	ask, ok := value.(api.TaskAskRequest)
	if !ok {
		t.Fatalf("task.ask codec returned %T", value)
	}
	if _, err := api.DecodeRESTRequest("task.ask", raw, map[string]string{"task": taskID}); err != nil {
		t.Fatalf("task.ask payload is not a valid REST body: %v", err)
	}
	return ask
}

// TestNativeAppObtainsCurrentTaskLifecycleVersion proves the app reads the
// current task lifecycle version and uses it as the handoff expected_version
// when the caller supplied no version.
func TestNativeAppObtainsCurrentTaskLifecycleVersion(t *testing.T) {
	const lifecycle uint64 = 11
	transport := &taskWireTransport{lifecycle: lifecycle}
	model := &handoffModel{question: "Which branch?", assignment: "role/reviewer"}
	checkpoints := &fakeCheckpoints{}
	var out bytes.Buffer
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			Model:       "fixture-model",
			Messages:    []agent.Message{{Role: "user", Content: "continue the task"}},
			Tools:       []string{"task.ask"},
		},
		ProjectID:   "project-1",
		Transport:   transport,
		Model:       model,
		Tools:       handoffTools{},
		Checkpoints: checkpoints,
		Clock:       fakeClock{now: testNow},
		Out:         &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if transport.callCount() != 2 {
		t.Fatalf("public calls = %d; want task.read then task.ask", transport.callCount())
	}
	readInvocation := transport.invocationAt(0)
	if readInvocation.Operation != "task.read" {
		t.Fatalf("first public operation = %q; want task.read", readInvocation.Operation)
	}
	readValue, err := api.DecodeRESTRequest("task.read", readInvocation.Request.Input, map[string]string{"task": "task-1"})
	if err != nil {
		t.Fatalf("task.read input does not match the public codec: %v", err)
	}
	if read := readValue.(api.TaskReadRequest); read.TaskID != "task-1" {
		t.Fatalf("task.read task_id = %q; want task-1", read.TaskID)
	}

	askInvocation := transport.invocationAt(1)
	if askInvocation.Operation != "task.ask" {
		t.Fatalf("second public operation = %q; want task.ask", askInvocation.Operation)
	}
	if askInvocation.Request.ExpectedVersion == nil || *askInvocation.Request.ExpectedVersion != lifecycle {
		t.Fatalf("task.ask expected version = %v; want the read lifecycle %d",
			askInvocation.Request.ExpectedVersion, lifecycle)
	}
	ask := decodeAppAsk(t, askInvocation.Request.Input, "task-1")
	if ask.TaskID != "task-1" || ask.Question != "Which branch?" || ask.Assignment != "role/reviewer" {
		t.Fatalf("task.ask payload = %+v", ask)
	}
	if len(ask.References) != 1 || !agent.ValidCheckpointRef(ask.References[0]) {
		t.Fatalf("task.ask references = %v; want one durable checkpoint reference", ask.References)
	}

	var outcome agent.Outcome
	if err := json.Unmarshal(out.Bytes(), &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome.Status != agent.OutcomeWaiting {
		t.Fatalf("outcome status = %q; want waiting", outcome.Status)
	}
	if ask.References[0] != outcome.CheckpointRef {
		t.Fatalf("ask reference = %q; outcome checkpoint = %q", ask.References[0], outcome.CheckpointRef)
	}
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want one handoff decision", model.callCount())
	}
	if !checkpoints.saved {
		t.Fatal("the app persisted no checkpoint before the handoff")
	}
}

// TestNativeAppKeepsCallerSuppliedTaskVersion proves the app reads the task
// only when the caller supplied no version: a caller-provided TaskVersion is
// authoritative, it is never overwritten by the served lifecycle version, and
// the run publishes exactly one ask without a task.read.
func TestNativeAppKeepsCallerSuppliedTaskVersion(t *testing.T) {
	const supplied uint64 = 5
	// A different served lifecycle version makes an accidental read or
	// overwrite observable.
	transport := &taskWireTransport{lifecycle: 11}
	model := &handoffModel{question: "Which branch?", assignment: "role/reviewer"}
	checkpoints := &fakeCheckpoints{}
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			TaskVersion: supplied,
			Model:       "fixture-model",
			Messages:    []agent.Message{{Role: "user", Content: "continue the task"}},
			Tools:       []string{"task.ask"},
		},
		ProjectID:   "project-1",
		Transport:   transport,
		Model:       model,
		Tools:       handoffTools{},
		Checkpoints: checkpoints,
		Clock:       fakeClock{now: testNow},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	count := transport.callCount()
	if count != 1 {
		t.Errorf("public calls = %d; want exactly one task.ask and no task.read", count)
	}
	if count > 0 {
		// The published ask is the last call even when a mutant inserted a
		// task.read first, so the version assertion isolates the overwrite.
		invocation := transport.invocationAt(count - 1)
		if invocation.Operation != "task.ask" {
			t.Errorf("public operation = %q; want task.ask", invocation.Operation)
		}
		version := invocation.Request.ExpectedVersion
		if version == nil {
			t.Errorf("task.ask expected version = <nil>; want the caller-supplied %d", supplied)
		} else if *version != supplied {
			t.Errorf("task.ask expected version = %d; want the caller-supplied %d", *version, supplied)
		}
	}
}

// TestNativeAppRejectsMismatchedTaskRead proves the bound task.read result is
// checked: a transport that answers with another task's record fails the run
// instead of publishing an ask against the wrong task.
func TestNativeAppRejectsMismatchedTaskRead(t *testing.T) {
	transport := &taskWireTransport{lifecycle: 11, readTaskID: "task-other"}
	model := &handoffModel{question: "Which branch?", assignment: "role/reviewer"}
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			Model:       "fixture-model",
			Messages:    []agent.Message{{Role: "user", Content: "continue the task"}},
			Tools:       []string{"task.ask"},
		},
		ProjectID:   "project-1",
		Transport:   transport,
		Model:       model,
		Tools:       handoffTools{},
		Checkpoints: &fakeCheckpoints{},
		Clock:       fakeClock{now: testNow},
	})
	if err == nil {
		t.Fatal("task.read for another task was accepted")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
		t.Fatalf("class = %v/%v; want internal_failure", class, ok)
	}
	if transport.callCount() != 1 {
		t.Fatalf("public calls = %d; want only the mismatched task.read", transport.callCount())
	}
	if operation := transport.invocationAt(0).Operation; operation != "task.read" {
		t.Fatalf("public operation = %q; want task.read", operation)
	}
	if model.callCount() != 0 {
		t.Fatalf("model calls = %d; want 0", model.callCount())
	}
}
