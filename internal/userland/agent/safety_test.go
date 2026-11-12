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

func TestToolAllowlistRefusesUnknownTool(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{toolCall("repo.write", `{"path":"a"}`)},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
	})
	if err == nil {
		t.Fatalf("unallowlisted tool was invoked: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("class = %v/%v; want forbidden", class, ok)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tool calls = %d; want 0", tool.callCount())
	}
	if outcome.Status != agent.OutcomeRefused || outcome.Class != fault.Forbidden {
		t.Fatalf("outcome = %+v; want refused/forbidden", outcome)
	}
}

func TestReservedToolNamesRefused(t *testing.T) {
	for _, name := range []string{"task.reply", "request.resolve", "role.escalate"} {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{{
				ToolCalls:    []agent.ToolRequest{toolCall(name, `{"decision":"pass"}`)},
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "tool_calls",
			}}}
			tool := &fakeTool{}
			public := &fakePublic{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
				Public: public, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
				Tools:    []string{name},
			})
			if err == nil {
				t.Fatalf("reserved tool %q was accepted: %+v", name, outcome)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
				t.Fatalf("class = %v/%v; want forbidden", class, ok)
			}
			if tool.callCount() != 0 || public.callCount() != 0 {
				t.Fatalf("tool/public calls = %d/%d; want 0/0", tool.callCount(), public.callCount())
			}
		})
	}
}

func TestReservedAuthorityOperationsRefusedAtConfiguration(t *testing.T) {
	names := []string{
		"task.reply",
		"execution.request.resolve",
		"request.resolve",
		"role.escalate",
		"Execution.Request.Resolve",
		"execution_request_resolve",
		"task-reply",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{{
				ToolCalls:    []agent.ToolRequest{toolCall(name, `{"decision":"approved"}`)},
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "tool_calls",
			}}}
			tool := &fakeTool{}
			public := &fakePublic{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
				Public: public, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
				Tools:    []string{name},
			})
			if err == nil {
				t.Fatalf("allowlisted reserved tool %q was accepted: %+v", name, outcome)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
				t.Fatalf("class = %v/%v; want forbidden", class, ok)
			}
			if model.callCount() != 0 || tool.callCount() != 0 || public.callCount() != 0 {
				t.Fatalf("model/tool/public calls = %d/%d/%d; want 0/0/0",
					model.callCount(), tool.callCount(), public.callCount())
			}
		})
	}
}

func TestReservedToolCallRefusedAtCallTime(t *testing.T) {
	const name = "execution.request.resolve"
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{toolCall(name, `{"decision":"approved"}`)},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{}
	public := &fakePublic{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
	})
	if err == nil {
		t.Fatalf("model-requested reserved tool %q was forwarded: %+v", name, outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("class = %v/%v; want forbidden", class, ok)
	}
	if outcome.Status != agent.OutcomeRefused || outcome.Class != fault.Forbidden {
		t.Fatalf("outcome = %+v; want refused/forbidden", outcome)
	}
	if !strings.Contains(outcome.Summary, "reserved") {
		t.Fatalf("summary = %q; want the reserved-tool refusal", outcome.Summary)
	}
	if model.callCount() != 1 || tool.callCount() != 0 || public.callCount() != 0 {
		t.Fatalf("model/tool/public calls = %d/%d/%d; want 1/0/0",
			model.callCount(), tool.callCount(), public.callCount())
	}
}

func TestToolArgumentsAreBounded(t *testing.T) {
	arguments, err := json.Marshal(map[string]string{"data": strings.Repeat("a", agent.MaxToolArgumentBytes)})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{{Name: "repo.write", Arguments: arguments}},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	_, runErr := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.write"},
	})
	if runErr == nil {
		t.Fatal("oversized tool arguments were accepted")
	}
	if class, ok := fault.ClassOf(runErr); !ok || class != fault.InvalidInput {
		t.Fatalf("class = %v/%v; want invalid_input", class, ok)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tool calls = %d; want 0", tool.callCount())
	}
}

func TestToolResultsAreBounded(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.read", `{}`)}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: strings.Repeat("x", agent.MaxToolResultBytes+64)},
	}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	second := model.requestAt(1)
	seenToolResult := false
	for _, message := range second.Messages {
		if message.Role != "tool" {
			continue
		}
		seenToolResult = true
		if len(message.Content) > agent.MaxToolResultBytes {
			t.Fatalf("tool result in conversation is %d bytes; bound is %d", len(message.Content), agent.MaxToolResultBytes)
		}
	}
	if !seenToolResult {
		t.Fatalf("second request has no tool result: %+v", second.Messages)
	}
}

func TestToolForbiddenAndUnauthenticatedAreSanitized(t *testing.T) {
	for _, class := range []fault.Class{fault.Forbidden, fault.Unauthenticated} {
		t.Run(string(class), func(t *testing.T) {
			const canary = "sk-live-tool-canary"
			model := &fakeModel{responses: []agent.ModelResponse{{
				ToolCalls:    []agent.ToolRequest{toolCall("repo.read", `{}`)},
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "tool_calls",
			}}}
			tool := &fakeTool{errAt: map[int]error{
				0: fault.Wrap(class, "tool refused", errors.New("credential "+canary)),
			}}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
				Tools:    []string{"repo.read"},
			})
			if err == nil {
				t.Fatalf("tool %s failure was not reported: %+v", class, outcome)
			}
			if got, ok := fault.ClassOf(err); !ok || got != class {
				t.Fatalf("class = %v/%v; want %s", got, ok, class)
			}
			requireNoSecret(t, "error", err.Error(), canary)
			if outcome.Class != class {
				t.Fatalf("outcome class = %q; want %q", outcome.Class, class)
			}
			if model.callCount() != 1 {
				t.Fatalf("model calls = %d; want 1", model.callCount())
			}
		})
	}
}

func TestToolDependencyErrorIsUncertainAndSanitized(t *testing.T) {
	const canary = "sk-live-tool-dependency"
	model := &fakeModel{responses: []agent.ModelResponse{{
		ToolCalls:    []agent.ToolRequest{toolCall("repo.write", `{"path":"a"}`)},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{errAt: map[int]error{
		0: fault.Wrap(fault.DependencyUnavailable, "tool connection failed", errors.New("Bearer "+canary)),
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.write"},
	})
	if err != nil {
		t.Fatalf("ambiguous tool failure escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeUncertain || len(outcome.Uncertain) != 1 {
		t.Fatalf("outcome = %+v; want uncertain with one effect", outcome)
	}
	for index := 0; index < checkpoints.saveCount(); index++ {
		requireNoSecret(t, "checkpoint", checkpointJSON(t, checkpoints.checkpointAt(index)), canary)
	}
	payload, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	requireNoSecret(t, "outcome", string(payload), canary)
}

func TestKnowledgeFailuresAreBounded(t *testing.T) {
	path := "context/skills/review.md"
	cases := map[string]*fakeKnowledge{
		"dependency": {errs: map[string]error{path: fault.New(fault.DependencyUnavailable, "knowledge unavailable")}},
		"oversized":  {bodies: map[string][]byte{path: make([]byte, agent.MaxKnowledgeBytes+1)}},
	}
	for name, knowledge := range cases {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
				Knowledge: knowledge, Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages:  []agent.Message{{Role: "user", Content: "go"}},
				Knowledge: []string{path},
			})
			if err != nil {
				t.Fatalf("knowledge failure escaped as an error: %v", err)
			}
			if outcome.Status != agent.OutcomeFailed {
				t.Fatalf("status = %q; want failed", outcome.Status)
			}
			if model.callCount() != 0 {
				t.Fatalf("model calls = %d; want 0", model.callCount())
			}
		})
	}

	t.Run("port missing", func(t *testing.T) {
		model := &fakeModel{}
		runtime := newTestRuntime(t, agent.RuntimeConfig{
			Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
		})
		_, err := runtime.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
			Messages:  []agent.Message{{Role: "user", Content: "go"}},
			Knowledge: []string{path},
		})
		if err == nil {
			t.Fatal("knowledge lookup without a KnowledgePort was accepted")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("class = %v/%v; want invalid_input", class, ok)
		}
		if model.callCount() != 0 {
			t.Fatalf("model calls = %d; want 0", model.callCount())
		}
	})
}

// TestRawToolContentNotRetainedByDefault proves raw tool output is not written
// into the durable checkpoint unless retention is explicitly enabled.
func TestRawToolContentNotRetainedByDefault(t *testing.T) {
	const canary = "RAW-TOOL-OUTPUT-CANARY"
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.read", `{}`)}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: canary + strings.Repeat("x", 128)},
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	if checkpoints.saveCount() == 0 {
		t.Fatal("no checkpoint was saved")
	}
	for index := 0; index < checkpoints.saveCount(); index++ {
		requireNoSecret(t, "checkpoint", checkpointJSON(t, checkpoints.checkpointAt(index)), canary)
	}
}
