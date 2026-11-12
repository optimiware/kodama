package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

func askCall(question, assignment string) agent.ToolRequest {
	arguments, _ := json.Marshal(map[string]string{"question": question, "assignment": assignment})
	return agent.ToolRequest{Name: "task.ask", Arguments: arguments}
}

// TestAskSavesCheckpointBeforePublishing proves the handoff order: durable
// context first, then one versioned/keyed task.ask, then exit without a model
// call and without a tool effect.
func TestAskSavesCheckpointBeforePublishing(t *testing.T) {
	log := &eventLog{}
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{askCall("Which branch?", "role/engineer")},
		Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{log: log}
	checkpoints := newFakeCheckpoints()
	checkpoints.log = log
	public := &fakePublic{log: log}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
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
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want 1 (the run exits after asking)", model.callCount())
	}
	if tool.callCount() != 0 {
		t.Fatalf("task.ask was forwarded to ToolPort %d times", tool.callCount())
	}
	if public.callCount() != 1 {
		t.Fatalf("public calls = %d; want 1", public.callCount())
	}
	if checkpoints.saveCount() != 1 {
		t.Fatalf("checkpoint saves = %d; want the handoff context", checkpoints.saveCount())
	}
	invocation := public.invocationAt(0)
	if invocation.Operation != "task.ask" {
		t.Fatalf("operation = %q; want task.ask", invocation.Operation)
	}
	if err := invocation.Validate(); err != nil {
		t.Fatalf("task.ask invocation is not a valid public envelope: %v", err)
	}
	if invocation.Request.ExpectedVersion == nil || *invocation.Request.ExpectedVersion != 7 {
		t.Fatalf("expected version = %v; want 7", invocation.Request.ExpectedVersion)
	}
	if invocation.Request.IdempotencyKey == "" {
		t.Fatal("task.ask has no idempotency key")
	}
	input, err := decodeAskInput(t, invocation.Request.Input, "tsk-1")
	if err != nil {
		t.Fatalf("task.ask input does not match the public codec: %v", err)
	}
	if len(input.References) != 1 || input.References[0] != checkpoints.refAt(0) {
		t.Fatalf("task.ask references = %v; want the durable checkpoint %q", input.References, checkpoints.refAt(0))
	}
	if input.TaskID != "tsk-1" || input.Question != "Which branch?" || input.Assignment != "role/engineer" {
		t.Fatalf("task.ask input = %+v", input)
	}
	if log.index("checkpoint.save") < 0 || log.index("public:task.ask") < 0 {
		t.Fatalf("missing handoff events: %v", log.snapshot())
	}
	if log.index("checkpoint.save") > log.index("public:task.ask") {
		t.Fatalf("ask was published before the checkpoint: %v", log.snapshot())
	}
}

func TestAskRequiresTaskVersion(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{askCall("Which branch?", "role/engineer")},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	_, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"task.ask"},
	})
	if err == nil {
		t.Fatal("unversioned ask was accepted")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("class = %v/%v; want invalid_input", class, ok)
	}
	if checkpoints.saveCount() != 0 || public.callCount() != 0 {
		t.Fatalf("save/public calls = %d/%d; want 0/0", checkpoints.saveCount(), public.callCount())
	}
}

func TestAskSaveFailureDoesNotPublish(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{askCall("Which branch?", "role/engineer")},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	checkpoints := newFakeCheckpoints()
	checkpoints.failAt = map[int]error{0: fault.New(fault.DependencyUnavailable, "checkpoint store unavailable")}
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	_, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"task.ask"},
	})
	if err == nil {
		t.Fatal("ask with an unsaved checkpoint was published")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v/%v; want dependency_unavailable", class, ok)
	}
	if public.callCount() != 0 {
		t.Fatalf("public calls = %d; want 0", public.callCount())
	}
}

func TestAskPublishFailureDoesNotRetry(t *testing.T) {
	const canary = "sk-live-ask-canary"
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{askCall("Which branch?", "role/engineer")},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{err: fault.Wrap(fault.DependencyUnavailable, "ask failed", errors.New("Bearer "+canary))}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	_, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"task.ask"},
	})
	if err == nil {
		t.Fatal("ask publish failure was not reported")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v/%v; want dependency_unavailable", class, ok)
	}
	requireNoSecret(t, "error", err.Error(), canary)
	if public.callCount() != 1 {
		t.Fatalf("public calls = %d; want 1 (no blind retry)", public.callCount())
	}
	if checkpoints.saveCount() != 1 {
		t.Fatalf("checkpoint saves = %d; want 1", checkpoints.saveCount())
	}
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want 1", model.callCount())
	}
}

func TestAskArgumentsAreBounded(t *testing.T) {
	cases := map[string]agent.ToolRequest{
		"missing question": {Name: "task.ask", Arguments: json.RawMessage(`{"assignment":"role/engineer"}`)},
		"long question":    askCall(strings.Repeat("q", agent.MaxQuestionBytes+1), "role/engineer"),
		"missing assignment": {
			Name:      "task.ask",
			Arguments: json.RawMessage(`{"question":"why?"}`),
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{{
				ToolCalls:    []agent.ToolRequest{call},
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "tool_calls",
			}}}
			checkpoints := newFakeCheckpoints()
			public := &fakePublic{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: public, Clock: fakeClock{at: time.Now()},
			})
			_, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
				Tools:    []string{"task.ask"},
			})
			if err == nil {
				t.Fatalf("invalid ask arguments were accepted")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v/%v; want invalid_input", class, ok)
			}
			if checkpoints.saveCount() != 0 || public.callCount() != 0 {
				t.Fatalf("save/public calls = %d/%d; want 0/0", checkpoints.saveCount(), public.callCount())
			}
		})
	}
}

// TestAskRejectsMalformedAssignments proves the handoff validates the declared
// `role/<role>` or `service/<id>` assignment grammar before any effect: a
// malformed assignment fails invalid_input with no checkpoint save and no
// public ask.
func TestAskRejectsMalformedAssignments(t *testing.T) {
	cases := []struct{ name, assignment string }{
		{"bare role", "alice"},
		{"empty role", "role/"},
		{"unknown role", "role/nobody"},
		{"extra segment", "role/engineer/x"},
		{"empty service", "service/"},
		{"trailing space", "role/engineer "},
		{"wrong case kind", "ROLE/engineer"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{
				askModelResponse(t, "Which branch?", testCase.assignment),
			}}
			checkpoints := newFakeCheckpoints()
			public := &fakePublic{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: public, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
				Tools:    []string{"task.ask"},
			})
			if err == nil {
				t.Fatalf("assignment %q was accepted: %+v", testCase.assignment, outcome)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v/%v; want invalid_input", class, ok)
			}
			if checkpoints.saveCount() != 0 {
				t.Fatalf("checkpoint saves = %d; want 0 before the assignment is validated", checkpoints.saveCount())
			}
			if public.callCount() != 0 {
				t.Fatalf("public calls = %d; want 0 for a malformed assignment", public.callCount())
			}
		})
	}
}

// TestFreshExecutionRestoresContext proves an assigned reply's fresh execution
// restores the durable conversation under its own execution identity.
func TestFreshExecutionRestoresContext(t *testing.T) {
	for _, retainRaw := range []bool{false, true} {
		name := "default raw off"
		if retainRaw {
			name = "retain raw opt in"
		}
		t.Run(name, func(t *testing.T) {
			firstModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("first answer", 1, 1)}}
			checkpoints := newFakeCheckpoints()
			first := newTestRuntime(t, agent.RuntimeConfig{
				Model: firstModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: retainRaw,
			})
			firstOutcome, err := first.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "start the task"}},
			})
			if err != nil || firstOutcome.Status != agent.OutcomeSucceeded {
				t.Fatalf("first run = %+v, %v", firstOutcome, err)
			}
			ref := firstOutcome.CheckpointRef

			secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("second answer", 1, 1)}}
			second := newTestRuntime(t, agent.RuntimeConfig{
				Model: secondModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: retainRaw,
			})
			secondOutcome, err := second.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-2", TaskVersion: 8, Model: "fixture-model",
				Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
				RestoreRef: ref,
			})
			if err != nil {
				t.Fatalf("fresh execution: %v", err)
			}
			if secondOutcome.Status != agent.OutcomeSucceeded || secondOutcome.Summary != "second answer" {
				t.Fatalf("fresh outcome = %+v", secondOutcome)
			}
			if checkpoints.restoreCount() != 1 {
				t.Fatalf("restore calls = %d; want 1", checkpoints.restoreCount())
			}
			request := secondModel.requestAt(0)
			if !containsMessage(request.Messages, "start the task") {
				t.Fatalf("restored user prompt missing from the fresh request: %+v", request.Messages)
			}
			if retainRaw {
				if !containsMessage(request.Messages, "first answer") {
					t.Fatalf("retained assistant content missing from the fresh request: %+v", request.Messages)
				}
			} else {
				if containsMessage(request.Messages, "first answer") {
					t.Fatalf("default raw-off retained raw assistant content: %+v", request.Messages)
				}
				if !containsMessage(request.Messages, "[assistant output not retained]") {
					t.Fatalf("default raw-off missing assistant placeholder: %+v", request.Messages)
				}
			}
			if !containsMessage(request.Messages, "the reply") {
				t.Fatalf("fresh reply missing from the request: %+v", request.Messages)
			}
			final := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
			if final.ExecutionID != "exec-2" || final.TaskID != "tsk-1" {
				t.Fatalf("fresh checkpoint identity = %q/%q", final.TaskID, final.ExecutionID)
			}
		})
	}
}

// TestFreshExecutionDoesNotInheritAuthority proves restoration grants no reply
// or escalation authority even though the prior execution ended cleanly.
func TestFreshExecutionDoesNotInheritAuthority(t *testing.T) {
	firstModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("first answer", 1, 1)}}
	checkpoints := newFakeCheckpoints()
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: firstModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	firstOutcome, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "start"}},
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	public := &fakePublic{}
	tool := &fakeTool{}
	secondModel := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{{Name: "task.reply", Arguments: json.RawMessage(`{"decision":"pass"}`)}},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
		Tools:      []string{"task.reply"},
		RestoreRef: firstOutcome.CheckpointRef,
	})
	if err == nil {
		t.Fatalf("restored execution autonomously replied: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("class = %v/%v; want forbidden", class, ok)
	}
	if public.callCount() != 0 || tool.callCount() != 0 {
		t.Fatalf("public/tool calls = %d/%d; want 0/0", public.callCount(), tool.callCount())
	}
}

func TestInvalidDurableReferencesAreRefused(t *testing.T) {
	malformed := []string{
		"ctx-1",
		"/tmp/checkpoint",
		"./checkpoint",
		"file:///tmp/checkpoint",
		"kodama-checkpoint/v2/ctx-1",
		"kodama-checkpoint/v1/../ctx",
		"kodama-checkpoint/v1/",
		strings.Repeat("a", agent.MaxCheckpointRefBytes+1),
	}
	for _, ref := range malformed {
		t.Run(ref, func(t *testing.T) {
			model := &fakeModel{}
			checkpoints := newFakeCheckpoints()
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
			})
			_, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
				Messages:   []agent.Message{{Role: "user", Content: "continue"}},
				RestoreRef: ref,
			})
			if err == nil {
				t.Fatalf("reference %q was accepted", ref)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v/%v; want invalid_input", class, ok)
			}
			if model.callCount() != 0 || checkpoints.restoreCount() != 0 {
				t.Fatalf("model/restore calls = %d/%d; want 0/0", model.callCount(), checkpoints.restoreCount())
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		model := &fakeModel{}
		checkpoints := newFakeCheckpoints()
		checkpoints.restoreErr = fault.New(fault.NotFound, "checkpoint is missing")
		runtime := newTestRuntime(t, agent.RuntimeConfig{
			Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
		})
		_, err := runtime.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
			Messages:   []agent.Message{{Role: "user", Content: "continue"}},
			RestoreRef: "kodama-checkpoint/v1/ctx-missing",
		})
		if err == nil {
			t.Fatal("missing checkpoint was treated as an empty success")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("class = %v/%v; want not_found", class, ok)
		}
		if model.callCount() != 0 {
			t.Fatalf("model calls = %d; want 0", model.callCount())
		}
	})

	t.Run("task mismatch", func(t *testing.T) {
		firstModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("answer", 1, 1)}}
		checkpoints := newFakeCheckpoints()
		first := newTestRuntime(t, agent.RuntimeConfig{
			Model: firstModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
		})
		firstOutcome, err := first.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
			Messages: []agent.Message{{Role: "user", Content: "start"}},
		})
		if err != nil {
			t.Fatalf("first run: %v", err)
		}
		model := &fakeModel{}
		second := newTestRuntime(t, agent.RuntimeConfig{
			Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
		})
		_, err = second.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-2", ExecutionID: "exec-2", Model: "fixture-model",
			Messages:   []agent.Message{{Role: "user", Content: "continue"}},
			RestoreRef: firstOutcome.CheckpointRef,
		})
		if err == nil {
			t.Fatal("checkpoint for another task was restored")
		}
		if class, ok := fault.ClassOf(err); !ok || (class != fault.InvalidInput && class != fault.Forbidden) {
			t.Fatalf("class = %v/%v; want invalid_input or forbidden", class, ok)
		}
		if model.callCount() != 0 {
			t.Fatalf("model calls = %d; want 0", model.callCount())
		}
	})
}
