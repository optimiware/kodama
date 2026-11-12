package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// providerNameFor translates one native dotted tool name for an assertion.
func providerNameFor(t *testing.T, native string) string {
	t.Helper()
	encoded, err := agent.ProviderToolName(native)
	if err != nil {
		t.Fatalf("ProviderToolName(%q): %v", native, err)
	}
	return encoded
}

// assistantCallTurns returns every assistant message that carries tool calls.
func assistantCallTurns(messages []agent.Message) []agent.Message {
	var turns []agent.Message
	for _, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			turns = append(turns, message)
		}
	}
	return turns
}

// toolResultMessages returns every tool message of one conversation.
func toolResultMessages(messages []agent.Message) []agent.Message {
	var results []agent.Message
	for _, message := range messages {
		if message.Role == "tool" {
			results = append(results, message)
		}
	}
	return results
}

// TestRuntimeAdvertisesTranslatedAllowlist proves the runtime advertises only
// the run allowlist, in the provider function-name grammar, with an
// object schema and explicit task.ask requirements. The task.ask/task_ask pair
// pins that the translation is collision-free.
func TestRuntimeAdvertisesTranslatedAllowlist(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{stopResponse("done", 1, 1)}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
		Tools:    []string{"repo.read", "task.ask", "task_ask"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	request := model.requestAt(0)
	if err := request.Validate(); err != nil {
		t.Fatalf("advertised request is not a valid model request: %v", err)
	}
	if len(request.Tools) != 3 {
		t.Fatalf("advertised tools = %d; want the three allowlisted tools: %+v", len(request.Tools), request.Tools)
	}

	want := map[string]string{
		"repo.read": "kd_repo_0read",
		"task.ask":  "kd_task_0ask",
		"task_ask":  "kd_task_1ask",
	}
	seen := make(map[string]bool, len(want))
	for _, declaration := range request.Tools {
		if strings.Contains(declaration.Name, ".") {
			t.Fatalf("advertised tool %q is a native dotted name", declaration.Name)
		}
		native, err := agent.NativeToolName(declaration.Name)
		if err != nil {
			t.Fatalf("advertised tool %q does not decode: %v", declaration.Name, err)
		}
		expected, ok := want[native]
		if !ok {
			t.Fatalf("advertised tool %q is not in the run allowlist", declaration.Name)
		}
		if declaration.Name != expected {
			t.Fatalf("advertised tool for %q = %q; want %q", native, declaration.Name, expected)
		}
		if seen[native] {
			t.Fatalf("native tool %q was advertised twice", native)
		}
		seen[native] = true
		if err := declaration.Validate(); err != nil {
			t.Fatalf("advertised tool %q is invalid: %v", declaration.Name, err)
		}

		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(declaration.Parameters, &schema); err != nil {
			t.Fatalf("tool %q parameters are not JSON: %v", declaration.Name, err)
		}
		if schema.Type != "object" {
			t.Fatalf("tool %q schema type = %q; want object", declaration.Name, schema.Type)
		}
		if native != "task.ask" {
			continue
		}
		if len(schema.Properties["question"]) == 0 || len(schema.Properties["assignment"]) == 0 {
			t.Fatalf("task.ask schema lacks question/assignment properties: %s", declaration.Parameters)
		}
		required := make(map[string]bool, len(schema.Required))
		for _, name := range schema.Required {
			required[name] = true
		}
		if !required["question"] || !required["assignment"] {
			t.Fatalf("task.ask required = %v; want question and assignment", schema.Required)
		}
	}
	for native := range want {
		if !seen[native] {
			t.Fatalf("allowlisted tool %q was not advertised", native)
		}
	}
	if seen["repo.write"] {
		t.Fatal("a tool outside the allowlist was advertised")
	}
}

// TestRuntimeCorrelatesMultipleToolResults proves one assistant call turn is
// recorded and every result, from a multi-call response, answers its own call
// identity with the exact provider arguments preserved.
func TestRuntimeCorrelatesMultipleToolResults(t *testing.T) {
	readArguments := `{ "path" : "README.md" }`
	writeArguments := `{"path":"a","content":"body"}`
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls: []agent.ToolRequest{
				{ID: "call-read", Name: "repo.read", Arguments: json.RawMessage(readArguments)},
				{ID: "call-write", Name: "repo.write", Arguments: json.RawMessage(writeArguments)},
			},
			Usage:        agent.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read":  {Name: "repo.read", Content: "read body"},
		"repo.write": {Name: "repo.write", Content: "write body"},
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "do both"}},
		Tools:    []string{"repo.read", "repo.write"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	if tool.callCount() != 2 || len(outcome.Completed) != 2 {
		t.Fatalf("tool calls/completed effects = %d/%d; want 2/2", tool.callCount(), len(outcome.Completed))
	}

	second := model.requestAt(1)
	turns := assistantCallTurns(second.Messages)
	if len(turns) != 1 {
		t.Fatalf("assistant call turns = %d; want exactly one: %+v", len(turns), second.Messages)
	}
	calls := turns[0].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("assistant call turn has %d calls; want 2", len(calls))
	}
	if calls[0].ID != "call-read" || calls[1].ID != "call-write" {
		t.Fatalf("assistant call ids = %q/%q; want call-read/call-write", calls[0].ID, calls[1].ID)
	}
	if calls[0].Name != providerNameFor(t, "repo.read") || calls[1].Name != providerNameFor(t, "repo.write") {
		t.Fatalf("assistant call names = %q/%q; want provider-encoded names", calls[0].Name, calls[1].Name)
	}
	if calls[0].Arguments != readArguments || calls[1].Arguments != writeArguments {
		t.Fatalf("assistant call arguments changed: %q / %q", calls[0].Arguments, calls[1].Arguments)
	}
	results := toolResultMessages(second.Messages)
	if len(results) != 2 {
		t.Fatalf("tool result messages = %d; want 2: %+v", len(results), second.Messages)
	}
	byID := map[agent.ToolCallID]agent.Message{}
	for _, result := range results {
		byID[result.ToolCallID] = result
	}
	if byID["call-read"].Content != "read body" || byID["call-write"].Content != "write body" {
		t.Fatalf("tool results = %+v; want each result correlated to its call", results)
	}
	for _, message := range second.Messages {
		if message.Role != "tool" {
			continue
		}
		if message.ToolCallID == "" {
			t.Fatalf("tool message has no call identity: %+v", message)
		}
	}

	final := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	finalTurns := assistantCallTurns(final.Messages)
	if len(finalTurns) != 1 || len(finalTurns[0].ToolCalls) != 2 {
		t.Fatalf("checkpoint lost the assistant call turn: %+v", finalTurns)
	}
	checkpointResults := toolResultMessages(final.Messages)
	if len(checkpointResults) != 2 {
		t.Fatalf("checkpoint tool results = %d; want 2", len(checkpointResults))
	}
	for _, result := range checkpointResults {
		if result.ToolCallID == "" {
			t.Fatalf("checkpoint tool message lost its correlation: %+v", result)
		}
	}
	if err := agent.ValidateConversation(final.Messages); err != nil {
		t.Fatalf("saved conversation is not a closed correlated transcript: %v", err)
	}
}

// TestRuntimeCompletedEffectUsesNewCallIDWithoutReplay proves provider
// correlation is not effect identity: a fresh execution that repeats the same
// name and exact arguments under a different call id must not repeat the
// completed effect, and must still answer the new call id.
func TestRuntimeCompletedEffectUsesNewCallIDWithoutReplay(t *testing.T) {
	const arguments = `{"path":"a"}`
	firstModel := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls:    []agent.ToolRequest{{ID: "call-first", Name: "repo.write", Arguments: json.RawMessage(arguments)}},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{"repo.write": {Name: "repo.write", Content: "written"}}}
	checkpoints := newFakeCheckpoints()
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: firstModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	firstOutcome, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	})
	if err != nil || firstOutcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("first run = %+v, %v", firstOutcome, err)
	}
	if tool.callCount() != 1 || len(firstOutcome.Completed) != 1 {
		t.Fatalf("first tool calls/completed = %d/%d; want 1/1", tool.callCount(), len(firstOutcome.Completed))
	}
	effect := firstOutcome.Completed[0]

	secondModel := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls:    []agent.ToolRequest{{ID: "call-second", Name: "repo.write", Arguments: json.RawMessage(arguments)}},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
		stopResponse("finished", 1, 1),
	}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	outcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "again"}},
		Tools:      []string{"repo.write"},
		RestoreRef: firstOutcome.CheckpointRef,
	})
	if err != nil {
		t.Fatalf("restored run: %v", err)
	}
	if outcome.Status != agent.OutcomeSucceeded || outcome.Summary != "finished" {
		t.Fatalf("restored outcome = %+v", outcome)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls = %d; want 1 (a new call id must not repeat the completed effect)", tool.callCount())
	}
	if len(outcome.Completed) != 1 || outcome.Completed[0] != effect {
		t.Fatalf("completed identity = %v; want stable %q", outcome.Completed, effect)
	}

	after := secondModel.requestAt(1)
	turns := assistantCallTurns(after.Messages)
	if len(turns) != 2 {
		t.Fatalf("assistant call turns = %d; want the restored turn and the repeated turn", len(turns))
	}
	repeated := turns[len(turns)-1]
	if len(repeated.ToolCalls) != 1 || repeated.ToolCalls[0].ID != "call-second" {
		t.Fatalf("repeated call turn = %+v; want call-second", repeated.ToolCalls)
	}
	results := toolResultMessages(after.Messages)
	if len(results) != 2 {
		t.Fatalf("tool results = %d; want the restored result and the correlated completion answer", len(results))
	}
	last := results[len(results)-1]
	if last.ToolCallID != "call-second" {
		t.Fatalf("completion answer call id = %q; want call-second", last.ToolCallID)
	}
	if !strings.Contains(last.Content, "already completed") {
		t.Fatalf("completion answer = %q; want the already-completed marker", last.Content)
	}
}

// TestRuntimeRestoresClosedCorrelatedHistory proves a retained correlated
// conversation round-trips into a fresh execution, and that a handoff
// checkpoint is a closed prefix: the served task snapshot owns the question, so
// restoration never replays task.ask or forwards an orphan assistant/tool turn.
func TestRuntimeRestoresClosedCorrelatedHistory(t *testing.T) {
	t.Run("completed correlation round trips", func(t *testing.T) {
		model := &fakeModel{responses: []agent.ModelResponse{
			{
				ToolCalls: []agent.ToolRequest{
					{ID: "call-read", Name: "repo.read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
					{ID: "call-stat", Name: "repo.stat", Arguments: json.RawMessage(`{"path":"main.go"}`)},
				},
				Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
				FinishReason: "tool_calls",
			},
			stopResponse("first answer", 1, 1),
		}}
		tool := &fakeTool{results: map[string]agent.ToolResult{
			"repo.read": {Name: "repo.read", Content: "README body"},
			"repo.stat": {Name: "repo.stat", Content: "main body"},
		}}
		checkpoints := newFakeCheckpoints()
		first := newTestRuntime(t, agent.RuntimeConfig{
			Model: model, Tools: tool, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
		})
		firstOutcome, err := first.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
			Messages: []agent.Message{{Role: "user", Content: "read two files"}},
			Tools:    []string{"repo.read", "repo.stat"},
		})
		if err != nil || firstOutcome.Status != agent.OutcomeSucceeded {
			t.Fatalf("first run = %+v, %v", firstOutcome, err)
		}
		saved := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
		if err := agent.ValidateConversation(saved.Messages); err != nil {
			t.Fatalf("saved conversation is not closed and correlated: %v", err)
		}

		secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("second answer", 1, 1)}}
		second := newTestRuntime(t, agent.RuntimeConfig{
			Model: secondModel, Tools: tool, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
		})
		outcome, err := second.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
			Messages:   []agent.Message{{Role: "user", Content: "continue"}},
			Tools:      []string{"repo.read", "repo.stat"},
			RestoreRef: firstOutcome.CheckpointRef,
		})
		if err != nil || outcome.Status != agent.OutcomeSucceeded || outcome.Summary != "second answer" {
			t.Fatalf("restored run = %+v, %v", outcome, err)
		}
		request := secondModel.requestAt(0)
		turns := assistantCallTurns(request.Messages)
		if len(turns) != 1 || len(turns[0].ToolCalls) != 2 {
			t.Fatalf("restored assistant call turn = %+v; want two correlated calls", turns)
		}
		if turns[0].ToolCalls[0].ID != "call-read" || turns[0].ToolCalls[1].ID != "call-stat" {
			t.Fatalf("restored call ids = %+v", turns[0].ToolCalls)
		}
		restoredResults := toolResultMessages(request.Messages)
		if len(restoredResults) != 2 {
			t.Fatalf("restored tool results = %d; want 2", len(restoredResults))
		}
		for index, id := range []agent.ToolCallID{"call-read", "call-stat"} {
			if restoredResults[index].ToolCallID != id {
				t.Fatalf("restored tool result %d call id = %q; want %q", index, restoredResults[index].ToolCallID, id)
			}
		}
		if tool.callCount() != 2 {
			t.Fatalf("tool calls = %d; want the two original effects only", tool.callCount())
		}
		finalCheckpoint := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
		if finalCheckpoint.ExecutionID != "exec-2" {
			t.Fatalf("final checkpoint execution = %q; want exec-2", finalCheckpoint.ExecutionID)
		}
	})

	t.Run("handoff prefix is closed", func(t *testing.T) {
		firstModel := &fakeModel{responses: []agent.ModelResponse{askModelResponse(t, "Which branch?", "role/engineer")}}
		checkpoints := newFakeCheckpoints()
		public := &fakePublic{}
		first := newTestRuntime(t, agent.RuntimeConfig{
			Model: firstModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: public, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
		})
		firstOutcome, err := first.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
			Messages: []agent.Message{{Role: "user", Content: "continue the task"}},
			Tools:    []string{"task.ask"},
		})
		if err != nil || firstOutcome.Status != agent.OutcomeWaiting {
			t.Fatalf("handoff run = %+v, %v", firstOutcome, err)
		}
		if public.callCount() != 1 {
			t.Fatalf("public calls = %d; want one task.ask", public.callCount())
		}
		saved := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
		if len(assistantCallTurns(saved.Messages)) != 0 {
			t.Fatalf("handoff checkpoint persisted an assistant call turn: %+v", saved.Messages)
		}
		if len(toolResultMessages(saved.Messages)) != 0 {
			t.Fatalf("handoff checkpoint persisted a tool turn: %+v", saved.Messages)
		}
		for _, message := range saved.Messages {
			if message.ToolCallID != "" {
				t.Fatalf("handoff checkpoint persisted a tool-call id: %+v", message)
			}
		}
		if err := agent.ValidateConversation(saved.Messages); err != nil {
			t.Fatalf("handoff checkpoint is not a closed prefix: %v", err)
		}
		if last := saved.Messages[len(saved.Messages)-1]; last.Role != "user" || last.Content != "continue the task" {
			t.Fatalf("handoff checkpoint last message = %+v; want the outstanding user context", last)
		}

		secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("answered", 1, 1)}}
		second := newTestRuntime(t, agent.RuntimeConfig{
			Model: secondModel, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: public, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
		})
		outcome, err := second.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-2", TaskVersion: 8, Model: "fixture-model",
			Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
			Tools:      []string{"task.ask"},
			RestoreRef: savedRef(t, firstOutcome),
		})
		if err != nil || outcome.Status != agent.OutcomeSucceeded {
			t.Fatalf("restored handoff run = %+v, %v", outcome, err)
		}
		if public.callCount() != 1 {
			t.Fatalf("public calls = %d; want task.ask never replayed", public.callCount())
		}
		request := secondModel.requestAt(0)
		if len(assistantCallTurns(request.Messages)) != 0 {
			t.Fatalf("restored handoff forwarded an orphan assistant call turn: %+v", request.Messages)
		}
		if len(toolResultMessages(request.Messages)) != 0 {
			t.Fatalf("restored handoff forwarded an orphan tool turn: %+v", request.Messages)
		}
		if !containsMessage(request.Messages, "continue the task") || !containsMessage(request.Messages, "the reply") {
			t.Fatalf("restored handoff lost the served context: %+v", request.Messages)
		}
	})
}

// savedRef returns the durable reference of one settled run.
func savedRef(t *testing.T, outcome agent.Outcome) string {
	t.Helper()
	if !agent.ValidCheckpointRef(outcome.CheckpointRef) {
		t.Fatalf("outcome checkpoint ref = %q", outcome.CheckpointRef)
	}
	return outcome.CheckpointRef
}

// TestRuntimeMissingRequiredContextCallsNoDependencies proves a checkpoint
// whose closed-correlation context is unavailable fails explicitly, before any
// model, tool or public call, rather than manufacturing history.
func TestRuntimeMissingRequiredContextCallsNoDependencies(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := func(messages []agent.Message) agent.Checkpoint {
		return agent.Checkpoint{
			Version:     agent.CheckpointVersion,
			TaskID:      "tsk-1",
			ExecutionID: "exec-1",
			Messages:    messages,
			CreatedAt:   now,
		}
	}
	cases := map[string]agent.Checkpoint{
		"dangling tool message": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "tool", Content: "ok", ToolCallID: "call-1"},
		}),
		"tool message without correlation": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "tool", Content: "[tool output not retained]"},
		}),
		"assistant turn emptied by withholding": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant"},
		}),
		"empty user message": base([]agent.Message{
			{Role: "user", Content: ""},
		}),
		"whitespace user message": base([]agent.Message{
			{Role: "user", Content: "   "},
		}),
		"whitespace system message": base([]agent.Message{
			{Role: "system", Content: "\t\n"},
			{Role: "user", Content: "go"},
		}),
		"open assistant call": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "kd_repo_0read", Arguments: "{}"}}},
		}),
		"replayed task.ask": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "kd_task_0ask", Arguments: `{"question":"q","assignment":"role/engineer"}`}}},
			{Role: "tool", Content: "answer", ToolCallID: "call-1"},
		}),
		"undecodable provider name": base([]agent.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "repo_read", Arguments: "{}"}}},
			{Role: "tool", Content: "ok", ToolCallID: "call-1"},
		}),
	}
	for name, checkpoint := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkpoint.Validate(); err != nil {
				t.Fatalf("test checkpoint fixture is not structurally valid: %v", err)
			}
			model := &fakeModel{}
			tool := &fakeTool{}
			public := &fakePublic{}
			store := newFakeCheckpoints()
			const ref = "kodama-checkpoint/v1/ctx-bad"
			store.store[ref] = checkpoint
			runtime := newTestRuntime(t, agent.RuntimeConfig{
				Model: model, Tools: tool, Checkpoints: store,
				Public: public, Clock: fakeClock{at: now},
			})
			outcome, err := runtime.Run(context.Background(), agent.RunInput{
				TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
				Messages:   []agent.Message{{Role: "user", Content: "continue"}},
				Tools:      []string{"repo.read", "task.ask"},
				RestoreRef: ref,
			})
			if err == nil {
				t.Fatalf("restored an unusable conversation: %+v", outcome)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v/%v; want invalid_input", class, ok)
			}
			if model.callCount() != 0 || tool.callCount() != 0 || public.callCount() != 0 {
				t.Fatalf("model/tool/public calls = %d/%d/%d; want 0/0/0",
					model.callCount(), tool.callCount(), public.callCount())
			}
			if store.saveCount() != 0 {
				t.Fatalf("checkpoint saves = %d; want 0", store.saveCount())
			}
		})
	}
}

// TestRuntimeWithholdsRawCorrelationByDefault proves default retention withholds
// the new raw call arguments and correlation as well as tool output.
func TestRuntimeWithholdsRawCorrelationByDefault(t *testing.T) {
	const (
		argumentCanary  = "RAW-CALL-ARGUMENT-CANARY"
		outputCanary    = "RAW-TOOL-OUTPUT-CANARY"
		assistantCanary = "RAW-ASSISTANT-CONTENT-CANARY"
	)
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			Content: assistantCanary + "-turn",
			ToolCalls: []agent.ToolRequest{{
				ID:        "call-secret-1",
				Name:      "repo.write",
				Arguments: json.RawMessage(fmt.Sprintf(`{"path":"a","content":%q}`, argumentCanary)),
			}},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
		stopResponse(assistantCanary+"-final", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.write": {Name: "repo.write", Content: outputCanary + strings.Repeat("x", 32)},
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "write"}},
		Tools:    []string{"repo.write"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	if checkpoints.saveCount() == 0 {
		t.Fatal("no checkpoint was saved")
	}
	for index := 0; index < checkpoints.saveCount(); index++ {
		checkpoint := checkpoints.checkpointAt(index)
		payload := checkpointJSON(t, checkpoint)
		requireNoSecret(t, "checkpoint arguments", payload, argumentCanary)
		requireNoSecret(t, "checkpoint tool output", payload, outputCanary)
		requireNoSecret(t, "checkpoint assistant output", payload, assistantCanary)
		requireNoSecret(t, "checkpoint correlation", payload, "call-secret-1")
		if err := agent.ValidateConversation(checkpoint.Messages); err != nil {
			t.Fatalf("checkpoint %d is not a closed withheld transcript: %v", index, err)
		}
		open := make(map[agent.ToolCallID]struct{})
		for _, message := range checkpoint.Messages {
			switch message.Role {
			case "assistant":
				for _, call := range message.ToolCalls {
					if !strings.HasPrefix(call.ID, "call-durable-") {
						t.Fatalf("checkpoint %d retained a provider call id: %+v", index, call)
					}
					if call.Arguments != "{}" {
						t.Fatalf("checkpoint %d retained call arguments: %+v", index, call)
					}
					open[agent.ToolCallID(call.ID)] = struct{}{}
				}
			case "tool":
				if _, exists := open[message.ToolCallID]; !exists {
					t.Fatalf("checkpoint %d tool message answers no durable call: %+v", index, message)
				}
				if message.Content != "[tool output not retained]" {
					t.Fatalf("checkpoint %d tool content = %q; want the withholding placeholder",
						index, message.Content)
				}
				delete(open, message.ToolCallID)
			}
		}
	}
}

// mutatingCheckpointStore mutates the checkpoint it receives, as an aliasing
// storage boundary would, so the runtime's defensive copy is observable.
type mutatingCheckpointStore struct {
	*fakeCheckpoints
}

func (store mutatingCheckpointStore) Save(ctx context.Context, checkpoint agent.Checkpoint) (string, error) {
	for index := range checkpoint.Messages {
		if len(checkpoint.Messages[index].ToolCalls) > 0 {
			checkpoint.Messages[index].ToolCalls[0].Arguments = `{"store-mutated":true}`
		}
	}
	return store.fakeCheckpoints.Save(ctx, checkpoint)
}

// TestRuntimeDeepCopiesToolCorrelation proves a model port or checkpoint store
// that mutates the nested call slice cannot rewrite the runtime's conversation
// or the correlation it persists later.
func TestRuntimeDeepCopiesToolCorrelation(t *testing.T) {
	const arguments = `{"path":"README.md"}`
	responses := func() []agent.ModelResponse {
		return []agent.ModelResponse{
			{
				ToolCalls:    []agent.ToolRequest{{ID: "call-1", Name: "repo.read", Arguments: json.RawMessage(arguments)}},
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "tool_calls",
			},
			stopResponse("done", 1, 1),
		}
	}
	run := func(t *testing.T, model agent.ModelPort, checkpoints agent.CheckpointPort) {
		t.Helper()
		runtime := newTestRuntime(t, agent.RuntimeConfig{
			Model: model, Tools: &fakeTool{}, Checkpoints: checkpoints,
			Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
		})
		outcome, err := runtime.Run(context.Background(), agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
			Messages: []agent.Message{{Role: "user", Content: "read"}},
			Tools:    []string{"repo.read"},
		})
		if err != nil || outcome.Status != agent.OutcomeSucceeded {
			t.Fatalf("Run = %+v, %v", outcome, err)
		}
	}

	t.Run("model mutation", func(t *testing.T) {
		model := &mutatingModel{responses: responses()}
		store := newFakeCheckpoints()
		run(t, model, store)
		final := store.checkpointAt(store.saveCount() - 1)
		turns := assistantCallTurns(final.Messages)
		if len(turns) != 1 || len(turns[0].ToolCalls) != 1 {
			t.Fatalf("persisted assistant turn = %+v", turns)
		}
		if turns[0].ToolCalls[0].Arguments != arguments {
			t.Fatalf("model-port mutation rewrote the runtime conversation: %q", turns[0].ToolCalls[0].Arguments)
		}
		results := toolResultMessages(final.Messages)
		if len(results) != 1 || results[0].ToolCallID != "call-1" {
			t.Fatalf("persisted tool correlation = %+v", results)
		}
	})

	t.Run("checkpoint mutation", func(t *testing.T) {
		model := &fakeModel{responses: responses()}
		store := mutatingCheckpointStore{fakeCheckpoints: newFakeCheckpoints()}
		run(t, model, store)
		second := model.requestAt(1)
		turns := assistantCallTurns(second.Messages)
		if len(turns) != 1 || len(turns[0].ToolCalls) != 1 {
			t.Fatalf("second request assistant turn = %+v", turns)
		}
		if turns[0].ToolCalls[0].Arguments != arguments {
			t.Fatalf("checkpoint-store mutation rewrote the runtime conversation: %q", turns[0].ToolCalls[0].Arguments)
		}
		results := toolResultMessages(second.Messages)
		if len(results) != 1 || results[0].ToolCallID != "call-1" {
			t.Fatalf("second request tool correlation = %+v", results)
		}
	})
}

// mutatingModel mutates every request it receives before answering, as a port
// that aliases its input would.
type mutatingModel struct {
	responses []agent.ModelResponse
	calls     int
}

func (model *mutatingModel) Invoke(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	index := model.calls
	model.calls++
	for messageIndex := range request.Messages {
		if len(request.Messages[messageIndex].ToolCalls) > 0 {
			request.Messages[messageIndex].ToolCalls[0].Arguments = `{"model-mutated":true}`
		}
		if request.Messages[messageIndex].ToolCallID != "" {
			request.Messages[messageIndex].ToolCallID = "model-mutated"
		}
	}
	if index >= len(model.responses) {
		return agent.ModelResponse{}, fmt.Errorf("mutating model script exhausted at call %d", index)
	}
	return model.responses[index], nil
}

// mapPublicChatMessages mirrors the production public model bridge's
// agent.Message -> api.ChatMessage mapping. The bridge validates exactly this
// public request before its transport call, so a conversation this
// request refuses is one the real bridge refuses.
func mapPublicChatMessages(messages []agent.Message) []api.ChatMessage {
	mapped := make([]api.ChatMessage, len(messages))
	for index, message := range messages {
		entry := api.ChatMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: string(message.ToolCallID),
		}
		for _, call := range message.ToolCalls {
			entry.ToolCalls = append(entry.ToolCalls, api.ChatToolCall{
				ID:   call.ID,
				Type: "function",
				Function: api.ChatToolCallFunction{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
		mapped[index] = entry
	}
	return mapped
}

// requirePublicBridgeAccepts proves the public model request the
// production bridge builds from the conversation passes its validation.
func requirePublicBridgeAccepts(t *testing.T, messages []agent.Message) {
	t.Helper()
	request := api.ModelInvokeRequest{
		ProjectID: "project-1",
		Model:     "fixture-model",
		Messages:  mapPublicChatMessages(messages),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("public model bridge request rejected the conversation: %v", err)
	}
}

// TestRuntimeRestoresWithheldCorrelationAsClosedPlaceholder proves the
// default-retention durable form is a closed, bridge-valid transcript: the
// provider call id, raw arguments and raw output are withheld and replaced by a
// deterministic runtime-local id and placeholder content, so a restore after a
// tool call is usable instead of an orphan tool message the real model bridge
// refuses.
func TestRuntimeRestoresWithheldCorrelationAsClosedPlaceholder(t *testing.T) {
	const (
		argumentCanary = "RAW-RESTORE-ARGUMENT-CANARY"
		outputCanary   = "RAW-RESTORE-OUTPUT-CANARY"
		providerCallID = "call-provider-canary-7"
	)
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls: []agent.ToolRequest{{
				ID:        providerCallID,
				Name:      "repo.read",
				Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, argumentCanary)),
			}},
			Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
			FinishReason: "tool_calls",
		},
		askModelResponse(t, "Which branch?", "role/engineer"),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: outputCanary},
	}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{}
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	firstOutcome, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "read then ask"}},
		Tools:    []string{"repo.read", "task.ask"},
	})
	if err != nil || firstOutcome.Status != agent.OutcomeWaiting {
		t.Fatalf("first run = %+v, %v; want waiting", firstOutcome, err)
	}
	if tool.callCount() != 1 || public.callCount() != 1 {
		t.Fatalf("tool/public calls = %d/%d; want 1/1", tool.callCount(), public.callCount())
	}

	saved := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	payload := checkpointJSON(t, saved)
	for label, canary := range map[string]string{
		"withheld arguments": argumentCanary,
		"withheld output":    outputCanary,
		"withheld call id":   providerCallID,
	} {
		requireNoSecret(t, label, payload, canary)
	}
	if err := agent.ValidateConversation(saved.Messages); err != nil {
		t.Fatalf("default checkpoint is not a closed correlation: %v", err)
	}
	turns := assistantCallTurns(saved.Messages)
	if len(turns) != 1 || len(turns[0].ToolCalls) != 1 {
		t.Fatalf("default checkpoint assistant turns = %+v; want one closed call", turns)
	}
	call := turns[0].ToolCalls[0]
	if call.ID == "" || call.ID == providerCallID {
		t.Fatalf("durable call id = %q; want a synthetic id", call.ID)
	}
	if !strings.HasPrefix(call.ID, "call-durable-") {
		t.Fatalf("durable call id = %q; want the deterministic durable prefix", call.ID)
	}
	if call.Arguments != "{}" {
		t.Fatalf("durable call arguments = %q; want the empty-object placeholder", call.Arguments)
	}
	results := toolResultMessages(saved.Messages)
	if len(results) != 1 || string(results[0].ToolCallID) != call.ID {
		t.Fatalf("durable tool result = %+v; want the durable call id %q", results, call.ID)
	}
	if results[0].Content != "[tool output not retained]" {
		t.Fatalf("durable tool content = %q; want the withholding placeholder", results[0].Content)
	}
	requirePublicBridgeAccepts(t, saved.Messages)

	secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("done", 1, 1)}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", TaskVersion: 8, Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
		Tools:      []string{"repo.read", "task.ask"},
		RestoreRef: savedRef(t, firstOutcome),
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("restored run = %+v, %v; want a usable restored conversation", outcome, err)
	}
	request := secondModel.requestAt(0)
	if err := request.Validate(); err != nil {
		t.Fatalf("restored model request is invalid: %v", err)
	}
	requirePublicBridgeAccepts(t, request.Messages)
	if !containsMessage(request.Messages, "[tool output not retained]") {
		t.Fatalf("restored request lost the withholding placeholder: %+v", request.Messages)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls = %d; want the original effect only", tool.callCount())
	}
	if public.callCount() != 1 {
		t.Fatalf("public calls = %d; want task.ask never replayed", public.callCount())
	}
}

// TestRuntimeHandoffKeepsAnsweredCallsFromTheAskTurn proves a handoff persists a
// closed prefix that keeps the answered part of the current model turn: when one
// response requests [repo.read, task.ask], the checkpoint contains the assistant
// turn trimmed to the read call and the read result, never the unrun ask call,
// so the restored execution keeps the read output and does not replay either
// the effect or the handoff.
func TestRuntimeHandoffKeepsAnsweredCallsFromTheAskTurn(t *testing.T) {
	const (
		readArguments = `{"path":"README.md"}`
		readOutput    = "README body"
		turnContent   = "I inspected the README; I still need the branch."
	)
	askArguments, err := json.Marshal(map[string]string{
		"question":   "Which branch?",
		"assignment": "role/engineer",
	})
	if err != nil {
		t.Fatalf("marshal task.ask arguments: %v", err)
	}
	model := &fakeModel{responses: []agent.ModelResponse{{
		Content: turnContent,
		ToolCalls: []agent.ToolRequest{
			{ID: "call-read", Name: "repo.read", Arguments: json.RawMessage(readArguments)},
			{ID: "call-ask", Name: "task.ask", Arguments: askArguments},
		},
		Usage:        agent.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
		FinishReason: "tool_calls",
	}}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: readOutput},
	}}
	checkpoints := newFakeCheckpoints()
	public := &fakePublic{}
	first := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	firstOutcome, err := first.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", TaskVersion: 7, Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "read then ask"}},
		Tools:    []string{"repo.read", "task.ask"},
	})
	if err != nil || firstOutcome.Status != agent.OutcomeWaiting {
		t.Fatalf("handoff run = %+v, %v; want waiting", firstOutcome, err)
	}
	if tool.callCount() != 1 || public.callCount() != 1 {
		t.Fatalf("tool/public calls = %d/%d; want 1/1", tool.callCount(), public.callCount())
	}

	saved := checkpoints.checkpointAt(checkpoints.saveCount() - 1)
	turns := assistantCallTurns(saved.Messages)
	if len(turns) != 1 || len(turns[0].ToolCalls) != 1 {
		t.Fatalf("handoff checkpoint assistant turns = %+v; want the answered read call only", turns)
	}
	if turns[0].Content != turnContent {
		t.Fatalf("handoff checkpoint assistant content = %q; want %q", turns[0].Content, turnContent)
	}
	call := turns[0].ToolCalls[0]
	if call.ID != "call-read" || call.Name != providerNameFor(t, "repo.read") || call.Arguments != readArguments {
		t.Fatalf("handoff checkpoint call = %+v; want the answered read call", call)
	}
	for _, message := range saved.Messages {
		for _, candidate := range message.ToolCalls {
			native, err := agent.NativeToolName(candidate.Name)
			if err == nil && native == "task.ask" {
				t.Fatalf("handoff checkpoint persisted the unrun task.ask call: %+v", saved.Messages)
			}
		}
	}
	results := toolResultMessages(saved.Messages)
	if len(results) != 1 || results[0].ToolCallID != "call-read" || results[0].Content != readOutput {
		t.Fatalf("handoff checkpoint tool results = %+v; want the answered read output", results)
	}
	if len(saved.Completed) != 1 {
		t.Fatalf("handoff checkpoint completed effects = %v; want the read effect", saved.Completed)
	}
	if err := agent.ValidateConversation(saved.Messages); err != nil {
		t.Fatalf("handoff checkpoint is not closed: %v", err)
	}

	secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("done", 1, 1)}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: public, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	outcome, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", TaskVersion: 8, Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
		Tools:      []string{"repo.read", "task.ask"},
		RestoreRef: savedRef(t, firstOutcome),
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("restored run = %+v, %v", outcome, err)
	}
	request := secondModel.requestAt(0)
	if err := request.Validate(); err != nil {
		t.Fatalf("restored request is invalid: %v", err)
	}
	if !containsMessage(request.Messages, readOutput) {
		t.Fatalf("restored request lost the read output: %+v", request.Messages)
	}
	if !containsMessage(request.Messages, turnContent) {
		t.Fatalf("restored request lost the assistant turn content: %+v", request.Messages)
	}
	for _, message := range request.Messages {
		for _, candidate := range message.ToolCalls {
			native, err := agent.NativeToolName(candidate.Name)
			if err == nil && native == "task.ask" {
				t.Fatalf("restored request replays task.ask: %+v", request.Messages)
			}
		}
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls = %d; want the read not re-run", tool.callCount())
	}
	if public.callCount() != 1 {
		t.Fatalf("public calls = %d; want task.ask not replayed", public.callCount())
	}
}

// TestRuntimeMidTurnCheckpointsAreClosed proves every mid-turn checkpoint,
// including the completion of one call of a multi-call turn, is a closed prefix
// that a fresh execution can restore instead of an open assistant turn.
func TestRuntimeMidTurnCheckpointsAreClosed(t *testing.T) {
	model := &fakeModel{responses: []agent.ModelResponse{
		{
			ToolCalls: []agent.ToolRequest{
				{ID: "call-read", Name: "repo.read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
				{ID: "call-stat", Name: "repo.stat", Arguments: json.RawMessage(`{"path":"main.go"}`)},
			},
			Usage:        agent.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: "README body"},
		"repo.stat": {Name: "repo.stat", Content: "main body"},
	}}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "read two files"}},
		Tools:    []string{"repo.read", "repo.stat"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	if checkpoints.saveCount() < 3 {
		t.Fatalf("checkpoint saves = %d; want the intent/completion saves", checkpoints.saveCount())
	}
	for index := 0; index < checkpoints.saveCount(); index++ {
		checkpoint := checkpoints.checkpointAt(index)
		if err := agent.ValidateConversation(checkpoint.Messages); err != nil {
			t.Fatalf("checkpoint %d is not a closed prefix: %v (%+v)", index, err, checkpoint.Messages)
		}
	}

	// The completion of the first call must keep only the answered read call.
	mid := checkpoints.checkpointAt(1)
	turns := assistantCallTurns(mid.Messages)
	if len(turns) != 1 || len(turns[0].ToolCalls) != 1 || turns[0].ToolCalls[0].ID != "call-read" {
		t.Fatalf("mid-turn checkpoint assistant turn = %+v; want the answered read call", turns)
	}
	results := toolResultMessages(mid.Messages)
	if len(results) != 1 || results[0].ToolCallID != "call-read" || results[0].Content != "README body" {
		t.Fatalf("mid-turn checkpoint tool results = %+v; want the read output", results)
	}

	secondModel := &fakeModel{responses: []agent.ModelResponse{stopResponse("resumed", 1, 1)}}
	second := newTestRuntime(t, agent.RuntimeConfig{
		Model: secondModel, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	restored, err := second.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "continue"}},
		Tools:      []string{"repo.read", "repo.stat"},
		RestoreRef: checkpoints.refAt(1),
	})
	if err != nil || restored.Status != agent.OutcomeSucceeded {
		t.Fatalf("restored mid-turn run = %+v, %v", restored, err)
	}
	request := secondModel.requestAt(0)
	if !containsMessage(request.Messages, "README body") {
		t.Fatalf("restored mid-turn request lost the answered read output: %+v", request.Messages)
	}
	if tool.callCount() != 2 {
		t.Fatalf("tool calls = %d; want the two original effects only", tool.callCount())
	}
}

// TestRuntimeRejectsCallerInjectedInvalidMessageBeforeModel proves one invalid
// caller-supplied message (a user message carrying a tool_call_id) fails
// invalid_input before any model, tool or checkpoint call.
func TestRuntimeRejectsCallerInjectedInvalidMessageBeforeModel(t *testing.T) {
	model := &fakeModel{}
	tool := &fakeTool{}
	checkpoints := newFakeCheckpoints()
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: checkpoints,
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()},
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{
			{Role: "user", Content: "go"},
			{Role: "user", Content: "injected", ToolCallID: "call-1"},
		},
		Tools: []string{"repo.read"},
	})
	if err == nil {
		t.Fatalf("invalid caller message was accepted: %+v", outcome)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("class = %v/%v; want invalid_input", class, ok)
	}
	if model.callCount() != 0 || tool.callCount() != 0 || checkpoints.saveCount() != 0 {
		t.Fatalf("model/tool/save calls = %d/%d/%d; want 0/0/0",
			model.callCount(), tool.callCount(), checkpoints.saveCount())
	}
}

// TestRunInputRejectsDuplicateAndOversizedAllowlists pins the run-input
// validation that runs before any port is touched.
func TestRunInputRejectsDuplicateAndOversizedAllowlists(t *testing.T) {
	base := func(tools []string) agent.RunInput {
		return agent.RunInput{
			TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
			Messages: []agent.Message{{Role: "user", Content: "go"}},
			Tools:    tools,
		}
	}
	duplicate := base([]string{"repo.read", "repo.read"})
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate allowlist tool was accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("duplicate class = %v/%v; want invalid_input", class, ok)
	}

	oversized := make([]string, agent.MaxToolDeclarations+1)
	for index := range oversized {
		oversized[index] = fmt.Sprintf("tool%d", index)
	}
	tooMany := base(oversized)
	if err := tooMany.Validate(); err == nil {
		t.Fatal("oversized allowlist was accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized class = %v/%v; want invalid_input", class, ok)
	}

	atBound := base(oversized[:agent.MaxToolDeclarations])
	if err := atBound.Validate(); err != nil {
		t.Fatalf("%d allowlisted tools were refused: %v", agent.MaxToolDeclarations, err)
	}
}

// mutatingMessageAndToolsModel rewrites every request it receives, as a port
// that aliases the runtime's memory would.
type mutatingMessageAndToolsModel struct {
	responses []agent.ModelResponse
	calls     int
	requests  []agent.ModelRequest
}

func (model *mutatingMessageAndToolsModel) Invoke(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	index := model.calls
	model.calls++
	tools := make([]agent.ToolDeclaration, len(request.Tools))
	for toolIndex, declaration := range request.Tools {
		tools[toolIndex] = declaration
		tools[toolIndex].Parameters = append(json.RawMessage(nil), declaration.Parameters...)
	}
	model.requests = append(model.requests, agent.ModelRequest{Tools: tools})
	for messageIndex := range request.Messages {
		request.Messages[messageIndex].Content = "model-mutated"
		if len(request.Messages[messageIndex].ToolCalls) > 0 {
			request.Messages[messageIndex].ToolCalls[0].Arguments = `{"model-mutated":true}`
		}
		if request.Messages[messageIndex].ToolCallID != "" {
			request.Messages[messageIndex].ToolCallID = "model-mutated"
		}
	}
	for toolIndex := range request.Tools {
		request.Tools[toolIndex].Name = "kd_model_0mutated"
		if len(request.Tools[toolIndex].Parameters) > 0 {
			request.Tools[toolIndex].Parameters[0] = '['
		}
	}
	if index >= len(model.responses) {
		return agent.ModelResponse{}, fmt.Errorf("mutating model script exhausted at call %d", index)
	}
	return model.responses[index], nil
}

// aliasingRestoreStore returns a checkpoint whose message slice has spare
// capacity and whose nested call slice aliases the store's memory, so a runtime
// that keeps the restored slice or its nested calls is observable.
type aliasingRestoreStore struct {
	checkpoint agent.Checkpoint
	backing    []agent.Message
	saves      int
}

func (store *aliasingRestoreStore) Save(_ context.Context, checkpoint agent.Checkpoint) (string, error) {
	store.saves++
	return "kodama-checkpoint/v1/ctx-alias", nil
}

func (store *aliasingRestoreStore) Restore(_ context.Context, _ string) (agent.Checkpoint, error) {
	checkpoint := store.checkpoint
	checkpoint.Messages = store.backing[:len(store.checkpoint.Messages)]
	return checkpoint, nil
}

// TestRuntimeCopiesRestoredMessagesAndAdvertisedTools proves the restored
// conversation and the advertised tool declarations never alias a checkpoint
// store or a mutating model port.
func TestRuntimeCopiesRestoredMessagesAndAdvertisedTools(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	backing := make([]agent.Message, 0, 8)
	backing = append(backing,
		agent.Message{Role: "user", Content: "restored question"},
		agent.Message{
			Role:      "assistant",
			Content:   "restored call",
			ToolCalls: append([]agent.ToolCall(nil), agent.ToolCall{ID: "call-1", Name: providerNameFor(t, "repo.read"), Arguments: "{}"}),
		},
		agent.Message{Role: "tool", Content: "restored output", ToolCallID: "call-1"},
	)
	checkpoint := agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages:    backing,
		CreatedAt:   now,
	}
	store := &aliasingRestoreStore{checkpoint: checkpoint, backing: backing}
	model := &mutatingMessageAndToolsModel{responses: []agent.ModelResponse{stopResponse("done", 1, 1)}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: &fakeTool{}, Checkpoints: store,
		Public: &fakePublic{}, Clock: fakeClock{at: now}, RetainRaw: true,
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-2", Model: "fixture-model",
		Messages:   []agent.Message{{Role: "user", Content: "the reply"}},
		Tools:      []string{"repo.read"},
		RestoreRef: "kodama-checkpoint/v1/ctx-alias",
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("restored run = %+v, %v", outcome, err)
	}
	if store.checkpoint.Messages[0].Content != "restored question" ||
		store.checkpoint.Messages[1].Content != "restored call" ||
		store.checkpoint.Messages[1].ToolCalls[0].Arguments != "{}" ||
		store.checkpoint.Messages[2].Content != "restored output" ||
		store.checkpoint.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("runtime mutated the restored checkpoint through an alias: %+v", store.checkpoint.Messages)
	}
	spare := store.backing[:cap(store.backing)]
	for index := len(store.checkpoint.Messages); index < len(spare); index++ {
		if spare[index].Role != "" || spare[index].Content != "" {
			t.Fatalf("runtime appended into the restored store's spare capacity at %d: %+v",
				index, spare[index])
		}
	}
}

// TestRuntimeCopiesAdvertisedTools proves a model port that mutates the
// advertised declaration list cannot change the next request.
func TestRuntimeCopiesAdvertisedTools(t *testing.T) {
	model := &mutatingMessageAndToolsModel{responses: []agent.ModelResponse{
		{
			ToolCalls:    []agent.ToolRequest{{ID: "call-read", Name: "repo.read", Arguments: json.RawMessage(`{"path":"README.md"}`)}},
			Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "tool_calls",
		},
		stopResponse("done", 1, 1),
	}}
	tool := &fakeTool{results: map[string]agent.ToolResult{
		"repo.read": {Name: "repo.read", Content: "README body"},
	}}
	runtime := newTestRuntime(t, agent.RuntimeConfig{
		Model: model, Tools: tool, Checkpoints: newFakeCheckpoints(),
		Public: &fakePublic{}, Clock: fakeClock{at: time.Now()}, RetainRaw: true,
	})
	outcome, err := runtime.Run(context.Background(), agent.RunInput{
		TaskID: "tsk-1", ExecutionID: "exec-1", Model: "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "read"}},
		Tools:    []string{"repo.read", "task.ask"},
	})
	if err != nil || outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("Run = %+v, %v", outcome, err)
	}
	first := model.requests[0]
	second := model.requests[1]
	for index := range second.Tools {
		if second.Tools[index].Name != first.Tools[index].Name {
			t.Fatalf("tool %d name changed by the model port: %q vs %q",
				index, second.Tools[index].Name, first.Tools[index].Name)
		}
		if len(second.Tools[index].Parameters) == 0 || second.Tools[index].Parameters[0] != '{' {
			t.Fatalf("tool %d parameters changed by the model port: %s",
				index, second.Tools[index].Parameters)
		}
	}
}
