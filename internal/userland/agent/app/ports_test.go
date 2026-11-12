package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/session"
)

type fakeTransport struct {
	invocations []api.Invocation
	result      api.Result
	err         error
}

func (transport *fakeTransport) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	transport.invocations = append(transport.invocations, invocation)
	return transport.result, transport.err
}

func modelResult(t *testing.T) api.Result {
	t.Helper()
	body, err := json.Marshal(api.ChatCompletionResponse{
		ID:    "completion-1",
		Model: "model-1",
		Choices: []api.ChatChoice{{
			Index:        0,
			Message:      api.ChatMessage{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}},
		Usage: api.ChatUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return api.Result{Operation: "model.invoke", Body: body}
}

func TestPublicModelUsesPublicOperation(t *testing.T) {
	transport := &fakeTransport{result: modelResult(t)}
	model, err := NewPublicModel(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicModel: %v", err)
	}
	response, err := model.Invoke(context.Background(), agent.ModelRequest{
		Model:    "model-1",
		Messages: []agent.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if response.Content != "done" || response.FinishReason != "stop" {
		t.Fatalf("response = %+v", response)
	}
	if response.Usage != (agent.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}) {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if len(transport.invocations) != 1 || transport.invocations[0].Operation != "model.invoke" {
		t.Fatalf("invocations = %+v", transport.invocations)
	}
	var input api.ModelInvokeRequest
	if err := json.Unmarshal(transport.invocations[0].Request.Input, &input); err != nil {
		t.Fatalf("decode model input: %v", err)
	}
	if input.ProjectID != "project-1" || input.Model != "model-1" {
		t.Fatalf("model input = %+v", input)
	}
}

func TestPublicModelRefusesUnknownFinishReason(t *testing.T) {
	body, err := json.Marshal(api.ChatCompletionResponse{
		ID:    "completion-1",
		Model: "model-1",
		Choices: []api.ChatChoice{{
			Message:      api.ChatMessage{Role: "assistant", Content: "done"},
			FinishReason: "surprise",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	transport := &fakeTransport{result: api.Result{Operation: "model.invoke", Body: body}}
	model, err := NewPublicModel(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicModel: %v", err)
	}
	if _, err := model.Invoke(context.Background(), agent.ModelRequest{
		Model:    "model-1",
		Messages: []agent.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Invoke accepted an unknown finish reason")
	}
}

func TestPublicToolsUsesPublicOperation(t *testing.T) {
	body, err := json.Marshal(api.ToolInvokeResult{Content: "ok", IsError: false})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	transport := &fakeTransport{result: api.Result{Operation: "tool.invoke", Body: body}}
	tools, err := NewPublicTools(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicTools: %v", err)
	}
	result, err := tools.Invoke(context.Background(), agent.ToolRequest{Name: "search.docs", Arguments: json.RawMessage("null")})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Content != "ok" || result.Name != "search.docs" {
		t.Fatalf("result = %+v", result)
	}
	var input api.ToolInvokeRequest
	if err := json.Unmarshal(transport.invocations[0].Request.Input, &input); err != nil {
		t.Fatalf("decode tool input: %v", err)
	}
	if input.ProjectID != "project-1" || input.Tool != "search.docs" || string(input.Arguments) != "{}" {
		t.Fatalf("tool input = %+v", input)
	}
}

func TestFileKnowledgeReadsRootedBody(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("# knowledge"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	knowledge, err := NewFileKnowledge(root)
	if err != nil {
		t.Fatalf("NewFileKnowledge: %v", err)
	}
	body, err := knowledge.Load(context.Background(), "docs/a.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(body) != "# knowledge" {
		t.Fatalf("body = %q", body)
	}
}

func TestFileKnowledgeRefusesTraversalAndAbsolutePaths(t *testing.T) {
	knowledge, err := NewFileKnowledge(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileKnowledge: %v", err)
	}
	for _, path := range []string{"../secret", "/etc/passwd", "docs/../../secret", "."} {
		if _, err := knowledge.Load(context.Background(), path); err == nil {
			t.Fatalf("Load accepted %q", path)
		}
	}
}

func TestFileKnowledgeBoundsBody(t *testing.T) {
	root := t.TempDir()
	body := bytes.Repeat([]byte("x"), agent.MaxKnowledgeBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.md"), body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	knowledge, err := NewFileKnowledge(root)
	if err != nil {
		t.Fatalf("NewFileKnowledge: %v", err)
	}
	if _, err := knowledge.Load(context.Background(), "big.md"); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("Load error = %v; want a size-bound refusal", err)
	}
}

func TestRunProducesOutcomeWithInjectedPorts(t *testing.T) {
	var out bytes.Buffer
	checkpoints := &fakeCheckpoints{}
	transport := &fakeTransport{result: modelResult(t)}
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			Model:       "model-1",
			Messages:    []agent.Message{{Role: "user", Content: "hi"}},
		},
		ProjectID:   "project-1",
		Transport:   transport,
		Checkpoints: checkpoints,
		Out:         &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var outcome agent.Outcome
	if err := json.Unmarshal(out.Bytes(), &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome.Status != agent.OutcomeSucceeded {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !checkpoints.saved {
		t.Fatal("the run persisted no checkpoint")
	}
}

func TestRunRefusesMissingCheckpoint(t *testing.T) {
	var out bytes.Buffer
	root := t.TempDir()
	model := &countingModel{}
	tools := &countingTools{}
	transport := &fakeTransport{result: modelResult(t)}
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			Model:       "model-1",
			Messages:    []agent.Message{{Role: "user", Content: "hi"}},
			RestoreRef:  "kodama-checkpoint/v1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1",
		},
		ProjectID:      "project-1",
		Transport:      transport,
		Model:          model,
		Tools:          tools,
		CheckpointRoot: root,
		Out:            &out,
	})
	if err == nil {
		t.Fatal("Run fabricated a successful restore of a missing checkpoint")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Run error = %v; want NotFound", err)
	}
	if model.calls != 0 || tools.calls != 0 || len(transport.invocations) != 0 {
		t.Fatalf("a missing checkpoint caused dependency calls: model=%d tools=%d transport=%d",
			model.calls, tools.calls, len(transport.invocations))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a missing checkpoint still wrote %d checkpoint files", len(entries))
	}
	if strings.Contains(out.String(), string(agent.OutcomeSucceeded)) {
		t.Fatalf("outcome claimed success: %s", out.String())
	}
}

func TestRunRetentionSelectionReachesCheckpointStorage(t *testing.T) {
	for _, test := range []struct {
		name        string
		retainRaw   bool
		wantContent string
	}{
		{"raw content is off by default", false, "[assistant output not retained]"},
		{"explicit opt in retains the challenge response", true, "done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			var out bytes.Buffer
			err := Run(context.Background(), Config{
				Run: agent.RunInput{
					TaskID:      "task-1",
					ExecutionID: "execution-1",
					Model:       "model-1",
					Messages:    []agent.Message{{Role: "user", Content: "hi"}},
				},
				ProjectID:      "project-1",
				Transport:      &fakeTransport{result: modelResult(t)},
				CheckpointRoot: root,
				RetainRaw:      test.retainRaw,
				Out:            &out,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var outcome agent.Outcome
			if err := json.Unmarshal(out.Bytes(), &outcome); err != nil {
				t.Fatalf("decode outcome: %v", err)
			}
			if outcome.Status != agent.OutcomeSucceeded || outcome.CheckpointRef == "" {
				t.Fatalf("outcome = %+v", outcome)
			}
			storage := newStorageAt(t, root)
			record, err := storage.Read(context.Background(), session.Ref(outcome.CheckpointRef))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			messages := record.Messages
			if len(messages) == 0 {
				t.Fatal("the run persisted no conversation")
			}
			if got := messages[len(messages)-1].Content; got != test.wantContent {
				t.Fatalf("persisted final content = %q; want %q", got, test.wantContent)
			}
		})
	}
}

// scriptedAppModel returns scripted responses and records incoming requests.
type scriptedAppModel struct {
	responses []agent.ModelResponse
	requests  []agent.ModelRequest
}

func (model *scriptedAppModel) Invoke(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	model.requests = append(model.requests, request)
	index := len(model.requests) - 1
	if index >= len(model.responses) {
		return agent.ModelResponse{}, errors.New("scripted model exhausted")
	}
	return model.responses[index], nil
}

// scriptedAppTools returns one scripted tool output and counts calls.
type scriptedAppTools struct {
	content string
	calls   int
}

func (tools *scriptedAppTools) Invoke(_ context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	tools.calls++
	return agent.ToolResult{Name: request.Name, Content: tools.content}, nil
}

// TestRunDefaultRawOffSaveAndRestoreRoundTrip proves that default RetainRaw: false
// persists redacted prompts + closed synthetic placeholders (without raw tool or
// assistant content) and cleanly restores in a follow-up execution without
// re-running completed effects.
func TestRunDefaultRawOffSaveAndRestoreRoundTrip(t *testing.T) {
	const (
		rawArgCanary       = "RAW-ARG-SECRET-CANARY"
		rawToolCanary      = "RAW-TOOL-OUTPUT-CANARY"
		rawAssistantCanary = "RAW-ASSISTANT-CANARY"
		userSecretCanary   = "sk-live-USER-PROMPT-SECRET"
	)
	root := t.TempDir()
	tools := &scriptedAppTools{content: rawToolCanary}
	firstModel := &scriptedAppModel{
		responses: []agent.ModelResponse{
			{
				Content: rawAssistantCanary + "-thinking",
				ToolCalls: []agent.ToolRequest{{
					ID:        "provider-call-99",
					Name:      "search.docs",
					Arguments: json.RawMessage(`{"query":"` + rawArgCanary + `"}`),
				}},
				Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
				FinishReason: "tool_calls",
			},
			{
				Content:      rawAssistantCanary + "-final",
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "stop",
			},
		},
	}

	var firstOut bytes.Buffer
	err := Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-1",
			Model:       "model-1",
			Messages:    []agent.Message{{Role: "user", Content: "check docs API_KEY=" + userSecretCanary}},
			Tools:       []string{"search.docs"},
		},
		ProjectID:      "project-1",
		Transport:      &fakeTransport{},
		Model:          firstModel,
		Tools:          tools,
		CheckpointRoot: root,
		RetainRaw:      false,
		Out:            &firstOut,
	})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	var firstOutcome agent.Outcome
	if err := json.Unmarshal(firstOut.Bytes(), &firstOutcome); err != nil {
		t.Fatalf("decode first outcome: %v", err)
	}
	if firstOutcome.Status != agent.OutcomeSucceeded || firstOutcome.CheckpointRef == "" {
		t.Fatalf("first outcome = %+v", firstOutcome)
	}
	if tools.calls != 1 {
		t.Fatalf("first tool calls = %d; want 1", tools.calls)
	}

	// Inspect every file on disk under CheckpointRoot to prove no raw canary leaked.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		rawBytes, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", entry.Name(), readErr)
		}
		text := string(rawBytes)
		for _, canary := range []string{rawArgCanary, rawToolCanary, rawAssistantCanary, userSecretCanary, "provider-call-99"} {
			if strings.Contains(text, canary) {
				t.Fatalf("checkpoint file %s leaked %q: %s", entry.Name(), canary, text)
			}
		}
	}

	// Restore in a second execution with RetainRaw: false and verify the same
	// tool call is skipped via durable effect bookkeeping while the conversation
	// passes ValidateConversation and ModelRequest.Validate.
	secondModel := &scriptedAppModel{
		responses: []agent.ModelResponse{
			{
				ToolCalls: []agent.ToolRequest{{
					ID:        "provider-call-100",
					Name:      "search.docs",
					Arguments: json.RawMessage(`{"query":"` + rawArgCanary + `"}`),
				}},
				Usage:        agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
				FinishReason: "tool_calls",
			},
			{
				Content:      "completed cleanly",
				Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
				FinishReason: "stop",
			},
		},
	}
	var secondOut bytes.Buffer
	err = Run(context.Background(), Config{
		Run: agent.RunInput{
			TaskID:      "task-1",
			ExecutionID: "execution-2",
			Model:       "model-1",
			Messages:    []agent.Message{{Role: "user", Content: "continue work"}},
			Tools:       []string{"search.docs"},
			RestoreRef:  firstOutcome.CheckpointRef,
		},
		ProjectID:      "project-1",
		Transport:      &fakeTransport{},
		Model:          secondModel,
		Tools:          tools,
		CheckpointRoot: root,
		RetainRaw:      false,
		Out:            &secondOut,
	})
	if err != nil {
		t.Fatalf("restored Run with RetainRaw=false: %v", err)
	}
	var secondOutcome agent.Outcome
	if err := json.Unmarshal(secondOut.Bytes(), &secondOutcome); err != nil {
		t.Fatalf("decode second outcome: %v", err)
	}
	if secondOutcome.Status != agent.OutcomeSucceeded || secondOutcome.Summary != "completed cleanly" {
		t.Fatalf("second outcome = %+v", secondOutcome)
	}
	if tools.calls != 1 {
		t.Fatalf("tool calls after restore = %d; want 1 (completed effect must not repeat)", tools.calls)
	}
	if len(secondModel.requests) != 2 {
		t.Fatalf("second model requests = %d; want 2", len(secondModel.requests))
	}
	restoredMsgs := secondModel.requests[0].Messages
	if len(restoredMsgs) < 4 || restoredMsgs[0].Content != "check docs API_KEY=[redacted]" {
		t.Fatalf("restored first prompt = %+v; want redacted user prompt", restoredMsgs)
	}
}

// countingModel records model calls so a refusal can assert zero effects.
type countingModel struct{ calls int }

func (model *countingModel) Invoke(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	model.calls++
	return agent.ModelResponse{Content: "done", FinishReason: "stop"}, nil
}

// countingTools records tool calls so a refusal can assert zero effects.
type countingTools struct{ calls int }

func (tools *countingTools) Invoke(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
	tools.calls++
	return agent.ToolResult{}, nil
}

type fakeCheckpoints struct {
	saved bool
}

func (checkpoints *fakeCheckpoints) Save(_ context.Context, checkpoint agent.Checkpoint) (string, error) {
	checkpoints.saved = true
	return "kodama-checkpoint/v1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1", nil
}

func (checkpoints *fakeCheckpoints) Restore(context.Context, string) (agent.Checkpoint, error) {
	return agent.Checkpoint{}, errors.New("not used")
}
