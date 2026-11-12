package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

func stopResponse(content string, prompt, completion int) agent.ModelResponse {
	return agent.ModelResponse{
		Content:      content,
		Usage:        agent.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
		FinishReason: "stop",
	}
}

func toolCall(name, arguments string) agent.ToolRequest {
	return agent.ToolRequest{Name: name, Arguments: json.RawMessage(arguments)}
}

func TestRuntimeSuccessLoop(t *testing.T) {
	log := &eventLog{}
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			Content:      "reading the repository",
			ToolCalls:    []agent.ToolRequest{toolCall("repo.read", `{"path":"README.md"}`)},
			Usage:        agent.Usage{PromptTokens: 12, CompletionTokens: 4, TotalTokens: 16},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 3, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{"repo.read": {Name: "repo.read", Content: "file body"}}, log: log}
	checkpoints := newFakeCheckpoints()
	checkpoints.log = log
	knowledge := &fakeKnowledge{}
	clock := fakeClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints, Knowledge: knowledge,
		Public: &fakePublic{}, Clock: clock,
	})

	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Model:       "fixture-model",
		Messages:    []agent.Message{{Role: "user", Content: "do the work"}},
		Tools:       []string{"repo.read"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("status = %q; want succeeded (%+v)", outcome.Status, outcome)
	}
	if outcome.Summary != "done" {
		t.Fatalf("summary = %q; want done", outcome.Summary)
	}
	if outcome.Usage != (agent.Usage{PromptTokens: 15, CompletionTokens: 5, TotalTokens: 20}) {
		t.Fatalf("usage = %+v; want 15/5/20", outcome.Usage)
	}
	if model.callCount() != 2 || tool.callCount() != 1 {
		t.Fatalf("model/tool calls = %d/%d; want 2/1", model.callCount(), tool.callCount())
	}
	if outcome.CheckpointRef == "" || !agent.ValidCheckpointRef(outcome.CheckpointRef) {
		t.Fatalf("outcome checkpoint ref = %q", outcome.CheckpointRef)
	}
	if len(outcome.Completed) != 1 {
		t.Fatalf("outcome completed = %v; want one effect", outcome.Completed)
	}
	if checkpoints.saveCount() < 2 {
		t.Fatalf("checkpoint saves = %d; want intent and completion", checkpoints.saveCount())
	}
	last := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	if last.TaskID != "tsk-1" || last.ExecutionID != "exec-1" {
		t.Fatalf("checkpoint identity = %q/%q", last.TaskID, last.ExecutionID)
	}
	if !last.CreatedAt.Equal(clock.at) {
		t.Fatalf("checkpoint CreatedAt = %v; want %v", last.CreatedAt, clock.at)
	}
	if len(last.Completed) != 1 || len(last.InFlight) != 0 || len(last.Uncertain) != 0 {
		t.Fatalf("final checkpoint effects = completed %v in-flight %v uncertain %v", last.Completed, last.InFlight, last.Uncertain)
	}
	first := model.requestAt(0)
	if first.Model != "fixture-model" || len(first.Messages) == 0 {
		t.Fatalf("first model request = %+v", first)
	}
	if !containsMessage(first.Messages, "do the work") {
		t.Fatalf("first model request lost the user task: %+v", first.Messages)
	}
	if requests := tool.requests(); requests[0].Name != "repo.read" || string(requests[0].Arguments) != `{"path":"README.md"}` {
		t.Fatalf("tool request = %+v", requests[0])
	}
	// Intent checkpoint precedes the tool effect and completion follows it.
	if log.index("checkpoint.save") > log.index("tool:repo.read") {
		t.Fatalf("tool ran before the intent checkpoint: %v", log.snapshot())
	}
}

func TestRuntimeLoadsKnowledgeBeforePlanning(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{stopResponse("ok", 1, 1)}}
	knowledge := &fakeKnowledge{bodies: map[string][]byte{"context/skills/review.md": []byte("# Review\ncheck the diff")}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(), Knowledge: knowledge,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages:  []agent.Message{{Role: "user", Content: "review"}},
		Knowledge: []string{"context/skills/review.md"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	request := model.requestAt(0)
	if !containsMessage(request.Messages, "check the diff") {
		t.Fatalf("knowledge body missing from the first request: %+v", request.Messages)
	}
}

func TestRuntimeMalformedModelResponseIsBounded(t *testing.T) {
	cases := map[string]agent.ModelResponse{
		"unknown finish reason": {Content: "x", Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "wizard"},
		"usage mismatch":        {Content: "x", Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 3}, FinishReason: "stop"},
		"invalid tool name": {
			ToolCalls:    []agent.ToolRequest{{Name: "Repo.Read"}},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{response}}
			tool := &fakeTool{}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
			})
			if err != nil {
				t.Fatalf("malformed response escaped as an error: %v", err)
			}
			if outcome.Status != agent.OutcomeFailed {
				t.Fatalf("status = %q; want failed", outcome.Status)
			}
			if model.callCount() != 1 || tool.callCount() != 0 {
				t.Fatalf("model/tool calls = %d/%d; want 1/0", model.callCount(), tool.callCount())
			}
		})
	}
}

func TestRuntimeFinishReasonsAreBounded(t *testing.T) {
	for _, reason := range []string{"length", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			model := &fakeModel{responses: []agent.ModelResponse{{
				Content:      "partial",
				Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
				FinishReason: reason,
			}}}
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
				Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
				Messages: []agent.Message{{Role: "user", Content: "go"}},
			})
			if err != nil {
				t.Fatalf("bounded finish reason escaped as an error: %v", err)
			}
			if outcome.Status != agent.OutcomeFailed || outcome.FinishReason != reason {
				t.Fatalf("outcome = %+v; want failed with finish reason %q", outcome, reason)
			}
			if model.callCount() != 1 {
				t.Fatalf("model calls = %d; want 1", model.callCount())
			}
		})
	}
}

func TestRuntimeEmptyStopIsNotSuccess(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{stopResponse("   ", 1, 1)}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("status = %q; want failed", outcome.Status)
	}
}

func TestRuntimeModelCallLimit(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.read", `{}`)}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
		Limits:   agent.Limits{ModelCalls: 1, Steps: 10, Messages: 10, Tokens: 1000},
	})
	if err != nil {
		t.Fatalf("limit outcome escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("status = %q; want failed", outcome.Status)
	}
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want 1", model.callCount())
	}
}

func TestRuntimeStepLimit(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls: []agent.ToolRequest{
				toolCall("repo.read", `{"path":"a"}`),
				toolCall("repo.read", `{"path":"b"}`),
			},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
		Limits:   agent.Limits{ModelCalls: 10, Steps: 1, Messages: 10, Tokens: 1000},
	})
	if err != nil {
		t.Fatalf("limit outcome escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("status = %q; want failed", outcome.Status)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls = %d; want 1", tool.callCount())
	}
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want 1", model.callCount())
	}
}

func TestRuntimeTokenLimit(t *testing.T) {
	usage := agent.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.read", `{}`)}, Usage: usage, FinishReason: "tool_calls"},
		{Content: "done", Usage: usage, FinishReason: "stop"},
	}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
		Limits:   agent.Limits{ModelCalls: 10, Steps: 10, Messages: 10, Tokens: 10},
	})
	if err != nil {
		t.Fatalf("limit outcome escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeFailed {
		t.Fatalf("status = %q; want failed", outcome.Status)
	}
	if model.callCount() != 2 {
		t.Fatalf("model calls = %d; want 2", model.callCount())
	}
}

func TestRuntimeMessageLimitRefusesBeforeCallingModel(t *testing.T) {
	model := &fakeModel{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "one"}, {Role: "user", Content: "two"}},
		Limits:   agent.Limits{ModelCalls: 10, Steps: 10, Messages: 1, Tokens: 1000},
	})
	if err == nil {
		t.Fatalf("message limit accepted: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("class = %v/%v; want invalid_input", class, ok)
	}
	if model.callCount() != 0 {
		t.Fatalf("model calls = %d; want 0", model.callCount())
	}
}

func TestRuntimeModelDependencyErrorIsSanitized(t *testing.T) {
	const canary = "sk-live-provider-canary"
	model := &fakeModel{errAt: map[int]error{
		0: fault.Wrap(fault.DependencyUnavailable, "provider unavailable", errors.New("authorization: Bearer "+canary)),
	}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
	})
	if err == nil {
		t.Fatal("model dependency failure was not reported")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v/%v; want dependency_unavailable", class, ok)
	}
	if outcome.Class != fault.DependencyUnavailable {
		t.Fatalf("outcome class = %q", outcome.Class)
	}
	requireNoSecret(t, "error", err.Error(), canary)
	if model.callCount() != 1 {
		t.Fatalf("model calls = %d; want 1", model.callCount())
	}
}

func TestRuntimeCancellationStopsFurtherCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &fakeModel{responses: []agent.ModelResponse{
		{ToolCalls: []agent.ToolRequest{toolCall("repo.read", `{}`)}, Usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, FinishReason: "tool_calls"},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{after: func(_ int, _ agent.ToolRequest) { cancel() }}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(ctx, agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read"},
	})
	if err != nil {
		t.Fatalf("cancellation escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeCancelled {
		t.Fatalf("status = %q; want cancelled", outcome.Status)
	}
	if tool.callCount() != 1 || model.callCount() != 1 {
		t.Fatalf("model/tool calls = %d/%d; want 1/1", model.callCount(), tool.callCount())
	}
	last := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	if len(last.Completed) != 1 {
		t.Fatalf("completed effects = %v; want the finished tool effect", last.Completed)
	}
}

func TestRuntimeExpiredDeadlineMakesNoCalls(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	model := &fakeModel{}
	tool := &fakeTool{}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(ctx, agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("expired deadline escaped as an error: %v", err)
	}
	if outcome.Status != agent.OutcomeCancelled {
		t.Fatalf("status = %q; want cancelled", outcome.Status)
	}
	if model.callCount() != 0 || tool.callCount() != 0 {
		t.Fatalf("model/tool calls = %d/%d; want 0/0", model.callCount(), tool.callCount())
	}
}

func TestRuntimeConcurrentRunsAreIsolated(t *testing.T) {
	model := &fakeModel{respond: func(request agent.ModelRequest) (agent.ModelResponse, error) {
		content := ""
		for _, v := range slices.Backward(request.Messages) {
			if v.Role == "user" {
				content = v.Content
				break
			}
		}
		return stopResponse(content, 1, 1), nil
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})

	const runs = 8
	var wait sync.WaitGroup
	outcomes := make([]agent.Outcome, runs)
	errs := make([]error, runs)
	for index := range runs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			outcomes[index], errs[index] = runtime.Run(context.Background(), agent.RunInput{
				TaskID:      fmt.Sprintf("tsk-%d", index),
				ExecutionID: fmt.Sprintf("exec-%d", index),
				Model:       "fixture-model",
				Messages:    []agent.Message{{Role: "user", Content: fmt.Sprintf("task-%d", index)}},
			})
		}(index)
	}
	wait.Wait()
	for index := range runs {
		if errs[index] != nil {
			t.Errorf("run %d: %v", index, errs[index])
			continue
		}
		if outcomes[index].Status != agent.OutcomeSucceeded {
			t.Errorf("run %d status = %q", index, outcomes[index].Status)
		}
		if outcomes[index].Summary != fmt.Sprintf("task-%d", index) {
			t.Errorf("run %d summary = %q; runs shared state", index, outcomes[index].Summary)
		}
		if outcomes[index].Usage != (agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}) {
			t.Errorf("run %d usage = %+v", index, outcomes[index].Usage)
		}
	}
}

func containsMessage(messages []agent.Message, fragment string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, fragment) {
			return true
		}
	}
	return false
}
