package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// Regression: native checkpoint-before-ask must reach the served task
// API. The public codec is the authority for the handoff wire shape, so every
// test below decodes the captured task.ask input with it instead of a local
// struct. A deterministic model fixture is used because the default public
// model path cannot generate tool calls.

// decodeAskInput strictly decodes one task.ask payload as both an MCP caller
// (task_id required) and a REST caller (route task must agree).
func decodeAskInput(t *testing.T, raw json.RawMessage, taskID string) (api.TaskAskRequest, error) {
	t.Helper()
	value, err := api.DecodeMCPOperationRequest("task.ask", raw)
	if err != nil {
		return api.TaskAskRequest{}, err
	}
	ask, ok := value.(api.TaskAskRequest)
	if !ok {
		return api.TaskAskRequest{}, fmt.Errorf("task.ask codec returned %T", value)
	}
	if _, err := api.DecodeRESTRequest("task.ask", raw, map[string]string{"task": taskID}); err != nil {
		return api.TaskAskRequest{}, err
	}
	return ask, nil
}

func askToolCall(t *testing.T, question, assignment string) agent.ToolRequest {
	t.Helper()
	arguments, err := json.Marshal(map[string]string{"question": question, "assignment": assignment})
	if err != nil {
		t.Fatalf("marshal task.ask arguments: %v", err)
	}
	return agent.ToolRequest{Name: "task.ask", Arguments: arguments}
}

func askModelResponse(t *testing.T, question, assignment string) agent.ModelResponse {
	t.Helper()
	return agent.ModelResponse{
		ToolCalls:    []agent.ToolRequest{askToolCall(t, question, assignment)},
		Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		FinishReason: "tool_calls",
	}
}

// TestNativeHandoffMatchesTaskAskCodec proves the handoff payload is the declared
// api.TaskAskRequest: task_id, question, assignment and durable references. It
// must pass the strict codec (unknown legacy fields such as checkpoint_ref or
// assignee are refused), and the reference must be the saved checkpoint.
func TestNativeHandoffMatchesTaskAskCodec(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{askModelResponse(t, "Which branch?", "role/engineer")}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "continue the task"}},
		Tools:    []string{"task.ask"},
	})
	if err != nil {
		t.Fatalf("ask handoff: %v", err)
	}
	if outcome.Status != agent.OutcomeWaiting {
		t.Fatalf("status = %q; want waiting", outcome.Status)
	}
	if public.callCount() != 1 {
		t.Fatalf("public calls = %d; want exactly one task.ask", public.callCount())
	}
	invocation := public.invocationAt(0)
	if invocation.Operation != "task.ask" {
		t.Fatalf("operation = %q; want task.ask", invocation.Operation)
	}
	if invocation.Request.ExpectedVersion == nil || *invocation.Request.ExpectedVersion != 7 {
		t.Fatalf("expected version = %v; want 7", invocation.Request.ExpectedVersion)
	}
	ask, err := decodeAskInput(t, invocation.Request.Input, "tsk-1")
	if err != nil {
		t.Fatalf("task.ask payload fails the strict public codec: %v; payload=%s", err, invocation.Request.Input)
	}
	if ask.TaskID != "tsk-1" {
		t.Fatalf("task_id = %q; want tsk-1", ask.TaskID)
	}
	if ask.Question != "Which branch?" || ask.Assignment != "role/engineer" {
		t.Fatalf("task.ask payload = %+v", ask)
	}
	if len(ask.References) != 1 || ask.References[0] != checkpoints.refAt(0) {
		t.Fatalf("references = %v; want the saved checkpoint %q", ask.References, checkpoints.refAt(0))
	}
	if !agent.ValidCheckpointRef(ask.References[0]) {
		t.Fatalf("reference %q is not a durable checkpoint reference", ask.References[0])
	}
}

// failingCheckpointPort records each save attempt and always fails it. It makes
// the ordering assertion explicit: a failed checkpoint save must be attempted
// before any ask, and must leave zero asks.
type failingCheckpointPort struct {
	mu       sync.Mutex
	attempts int
	err      error
}

func (port *failingCheckpointPort) Save(context.Context, agent.Checkpoint) (string, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.attempts++
	return "", port.err
}

func (port *failingCheckpointPort) Restore(context.Context, string) (agent.Checkpoint, error) {
	return agent.Checkpoint{}, errors.New("restore is not used")
}

func (port *failingCheckpointPort) saveAttempts() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.attempts
}

// TestNativeHandoffCheckpointCommittedBeforeAsk proves a failed checkpoint save
// stops the handoff: the save is attempted, no task.ask reaches the public
// boundary, and the run reports the dependency failure.
func TestNativeHandoffCheckpointCommittedBeforeAsk(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{askModelResponse(t, "Which branch?", "role/engineer")}}
	checkpoints := &failingCheckpointPort{
		err: fault.New(fault.DependencyUnavailable, "checkpoint store unavailable"),
	}
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "continue"}},
		Tools:    []string{"task.ask"},
	})
	if err == nil {
		t.Fatalf("handoff with an unsaved checkpoint succeeded: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v/%v; want dependency_unavailable", class, ok)
	}
	if checkpoints.saveAttempts() != 1 {
		t.Fatalf("checkpoint save attempts = %d; want exactly one before the ask", checkpoints.saveAttempts())
	}
	if public.callCount() != 0 {
		t.Fatalf("public calls = %d; want zero asks after a failed checkpoint save", public.callCount())
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("outcome status = %q; want failed", outcome.Status)
	}
}

// TestNativeHandoffReferenceSurvivesFreshExecution proves the ask references a
// durable checkpoint, not the previous workspace or a process-local id: a fresh
// execution restores the prior conversation using only the reference from the
// served ask payload.
func TestNativeHandoffReferenceSurvivesFreshExecution(t *testing.T) {
	firstModel := &fakeModel{responses: []agent.ModelResponse{askModelResponse(t, "Which branch?", "role/engineer")}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{}
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: firstModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	firstOutcome, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "continue the task"}},
		Tools:    []string{"task.ask"},
	})
	if err != nil || firstOutcome.Status != agent.OutcomeWaiting {
		t.Fatalf("first run = %+v, %v", firstOutcome, err)
	}
	ask, err := decodeAskInput(t, public.invocationAt(0).Request.Input, "tsk-1")
	if err != nil {
		t.Fatalf("task.ask payload fails the strict public codec: %v", err)
	}
	if len(ask.References) != 1 {
		t.Fatalf("references = %v; want one durable reference", ask.References)
	}
	reference := ask.References[0]
	if !agent.ValidCheckpointRef(reference) {
		t.Fatalf("reference %q is not a durable checkpoint reference", reference)
	}
	if strings.HasPrefix(reference, "/") || strings.Contains(reference, "workspace") || strings.Contains(reference, "..") {
		t.Fatalf("reference %q looks like a workspace path, not a durable checkpoint", reference)
	}

	secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("restored", 1, 1)}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	secondOutcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", TaskVersion: 8, Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
		RestoreRef: reference,
	})
	if err != nil {
		t.Fatalf("fresh execution restore: %v", err)
	}
	if secondOutcome.Status != agent.OutcomeSucceeded || secondOutcome.Summary != "restored" {
		t.Fatalf("fresh outcome = %+v", secondOutcome)
	}
	if checkpoints.restoreCount() != 1 {
		t.Fatalf("restore calls = %d; want 1", checkpoints.restoreCount())
	}
	request := secondModel.requestAt(0)
	if !containsMessage(request.Messages, "continue the task") {
		t.Fatalf("restored conversation missing from the fresh request: %+v", request.Messages)
	}
	if !containsMessage(request.Messages, "the reply") {
		t.Fatalf("fresh reply missing from the request: %+v", request.Messages)
	}
}

// invalidRefCheckpointPort records each save attempt and answers it with a
// caller-supplied reference, valid or not.
type invalidRefCheckpointPort struct {
	mu       sync.Mutex
	ref      string
	attempts int
}

func (port *invalidRefCheckpointPort) Save(context.Context, agent.Checkpoint) (string, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.attempts++
	return port.ref, nil
}

func (port *invalidRefCheckpointPort) Restore(context.Context, string) (agent.Checkpoint, error) {
	return agent.Checkpoint{}, errors.New("restore is not used")
}

func (port *invalidRefCheckpointPort) saveAttempts() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.attempts
}

// TestNativeHandoffRejectsInvalidCheckpointReference proves the handoff checks
// the checkpoint port's reference: a workspace path or an empty reference is an
// internal failure that leaves a failed outcome and publishes no ask.
func TestNativeHandoffRejectsInvalidCheckpointReference(t *testing.T) {
	cases := map[string]string{
		"workspace path": "/workspace/x",
		"empty":          "",
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{askModelResponse(t, "Which branch?", "role/engineer")}}
			checkpoints := &invalidRefCheckpointPort{ref: ref}
			public := &fakePublic{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: public, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "continue"}},
				Tools:    []string{"task.ask"},
			})
			if err == nil {
				t.Fatalf("checkpoint reference %q was accepted: %+v", ref, outcome)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Fatalf("class = %v/%v; want internal_failure", class, ok)
			}
			if outcome.Status != agent.OutcomeFailed {
				t.Fatalf("outcome status = %q; want failed", outcome.Status)
			}
			if checkpoints.saveAttempts() != 1 {
				t.Fatalf("checkpoint save attempts = %d; want 1", checkpoints.saveAttempts())
			}
			if public.callCount() != 0 {
				t.Fatalf("public calls = %d; want zero asks after an invalid reference", public.callCount())
			}
		})
	}
}

// TestNativeRestoreMissingCheckpointFailsAction proves a missing durable
// reference fails the restoring action instead of starting from incomplete
// context. This is green regression evidence for the existing restore guard.
func TestNativeRestoreMissingCheckpointFailsAction(t *testing.T) {
	model := &fakeModel{}
	checkpoints := newFakeCheckpoints()
	checkpoints.restoreErr = fault.New(fault.NotFound, "checkpoint is missing")
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "continue"}},
		RestoreRef: "kodama-checkpoint/v1/ctx-missing",
	})
	if err == nil {
		t.Fatalf("missing checkpoint was treated as an empty success: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("class = %v/%v; want not_found", class, ok)
	}
	if model.callCount() != 0 || checkpoints.saveCount() != 0 || public.callCount() != 0 {
		t.Fatalf("model/save/public calls = %d/%d/%d; want 0/0/0",
			model.callCount(), checkpoints.saveCount(), public.callCount())
	}
	if outcome.Status == agent.OutcomeSucceeded {
		t.Fatalf("missing checkpoint produced a successful outcome: %+v", outcome)
	}
}
