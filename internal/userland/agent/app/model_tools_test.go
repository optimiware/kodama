package app

// Public model bridge vectors: advertised declarations, assistant call
// correlation and tool-result identities cross the public `model.invoke`
// boundary unchanged, provider calls come back as native tool requests, and a
// malformed exchange is refused instead of reaching a transport or a caller.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// The two declarations and calls below are DISTINCT provider names: the
// translation maps native `task.ask` to `kd_task_0ask` and `task_ask` to
// `kd_task_1ask`, so a naive dot-to-underscore alias would collapse them.
const (
	bridgeParametersAsk = `{
  "type": "object",
  "properties": {
    "question": {"type": "string"},
    "assignment": {"type": "string"}
  },
  "required": ["question", "assignment"]
}`
	bridgeParametersAskUnderscore = `{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`
	bridgeArgumentsAsk            = `{ "question" : "Choose a branch", "assignment":"role/reviewer" }`
	bridgeArgumentsAskUnderscore  = `{"question":"second"}`
)

// bridgeChoice builds one assistant choice with the given message and finish
// reason.
func bridgeChoice(message api.ChatMessage, finishReason string) api.ChatChoice {
	return api.ChatChoice{Index: 0, Message: message, FinishReason: finishReason}
}

// bridgeResult encodes one provider completion as a public result body.
func bridgeResult(t *testing.T, response api.ChatCompletionResponse) api.Result {
	t.Helper()
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal completion: %v", err)
	}
	return api.Result{Operation: "model.invoke", Body: body}
}

// decodeModelInvoke decodes the single captured public model input.
func decodeModelInvoke(t *testing.T, transport *fakeTransport) api.ModelInvokeRequest {
	t.Helper()
	if len(transport.invocations) != 1 {
		t.Fatalf("transport invocations = %d; want 1", len(transport.invocations))
	}
	invocation := transport.invocations[0]
	if invocation.Operation != "model.invoke" {
		t.Fatalf("operation = %q; want model.invoke", invocation.Operation)
	}
	var input api.ModelInvokeRequest
	if err := json.Unmarshal(invocation.Request.Input, &input); err != nil {
		t.Fatalf("decode model input: %v; body=%s", err, string(invocation.Request.Input))
	}
	return input
}

// TestPublicModelPreservesToolDeclarationsAndHistory pins that the public
// model bridge copies the advertised declarations, the assistant call list and
// the tool-result identities into the public request without double-encoding
// the provider names or re-marshalling the string-valued arguments.
func TestPublicModelPreservesToolDeclarationsAndHistory(t *testing.T) {
	transport := &fakeTransport{result: modelResult(t)}
	model, err := NewPublicModel(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicModel: %v", err)
	}
	request := agent.ModelRequest{
		Model:     "model-1",
		MaxTokens: 64,
		Messages: []agent.Message{
			{Role: "system", Content: "plan the work"},
			{Role: "user", Content: "choose a branch"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []agent.ToolCall{
					{ID: "call-ask-1", Name: "kd_task_0ask", Arguments: bridgeArgumentsAsk},
					{ID: "call-ask-2", Name: "kd_task_1ask", Arguments: bridgeArgumentsAskUnderscore},
				},
			},
			{Role: "tool", ToolCallID: "call-ask-2", Content: "second answer"},
			{Role: "tool", ToolCallID: "call-ask-1", Content: "first answer"},
		},
		Tools: []agent.ToolDeclaration{
			{Name: "kd_task_0ask", Description: "ask the served task question", Parameters: json.RawMessage(bridgeParametersAsk)},
			{Name: "kd_task_1ask", Description: "underscore collision control", Parameters: json.RawMessage(bridgeParametersAskUnderscore)},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("fixture request is invalid: %v", err)
	}
	if _, err := model.Invoke(context.Background(), request); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	input := decodeModelInvoke(t, transport)
	if input.ProjectID != "project-1" || input.Model != "model-1" {
		t.Fatalf("model input = %+v", input)
	}

	// Declarations keep their exact provider names, type and schema.
	if len(input.Tools) != 2 {
		t.Fatalf("tools = %d; want 2", len(input.Tools))
	}
	for index, want := range []struct {
		name       string
		parameters string
	}{
		{"kd_task_0ask", bridgeParametersAsk},
		{"kd_task_1ask", bridgeParametersAskUnderscore},
	} {
		tool := input.Tools[index]
		if tool.Type != "function" {
			t.Errorf("tool %d type = %q; want function", index, tool.Type)
		}
		if tool.Function.Name != want.name {
			t.Errorf("tool %d name = %q; want %q (no double encoding)", index, tool.Function.Name, want.name)
		}
		var got, expected any
		if err := json.Unmarshal(tool.Function.Parameters, &got); err != nil {
			t.Fatalf("decode tool %d parameters: %v", index, err)
		}
		if err := json.Unmarshal([]byte(want.parameters), &expected); err != nil {
			t.Fatalf("decode expected tool %d parameters: %v", index, err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("tool %d parameters = %s; want %s", index, tool.Function.Parameters, want.parameters)
		}
	}

	// The assistant call list and tool-result correlation survive the bridge.
	if len(input.Messages) != 5 {
		t.Fatalf("messages = %d; want 5", len(input.Messages))
	}
	assistant := input.Messages[2]
	if assistant.Role != "assistant" {
		t.Fatalf("assistant role = %q", assistant.Role)
	}
	if assistant.Content != "" || assistant.ContentNull {
		t.Errorf("assistant content = %q null=%v; want the existing empty-string representation", assistant.Content, assistant.ContentNull)
	}
	if assistant.ToolCallID != "" {
		t.Errorf("assistant tool_call_id = %q; want empty", assistant.ToolCallID)
	}
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant tool_calls = %d; want 2", len(assistant.ToolCalls))
	}
	for index, want := range []struct {
		id        string
		provider  string
		native    string
		arguments string
	}{
		{"call-ask-1", "kd_task_0ask", "task.ask", bridgeArgumentsAsk},
		{"call-ask-2", "kd_task_1ask", "task_ask", bridgeArgumentsAskUnderscore},
	} {
		call := assistant.ToolCalls[index]
		if call.ID != want.id {
			t.Errorf("call %d id = %q; want %q", index, call.ID, want.id)
		}
		if call.Type != "function" {
			t.Errorf("call %d type = %q; want function", index, call.Type)
		}
		if call.Function.Name != want.provider {
			t.Errorf("call %d name = %q; want the encoded provider name %q", index, call.Function.Name, want.provider)
		}
		if call.Function.Arguments != want.arguments {
			t.Errorf("call %d arguments = %q; want the exact string %q", index, call.Function.Arguments, want.arguments)
		}
		native, err := agent.NativeToolName(call.Function.Name)
		if err != nil {
			t.Errorf("call %d name %q does not decode: %v", index, call.Function.Name, err)
		} else if native != want.native {
			t.Errorf("call %d decodes to %q; want %q (collision-free)", index, native, want.native)
		}
	}
	if input.Messages[3].Role != "tool" || input.Messages[3].ToolCallID != "call-ask-2" {
		t.Errorf("tool message 3 = %+v; want call-ask-2", input.Messages[3])
	}
	if input.Messages[4].Role != "tool" || input.Messages[4].ToolCallID != "call-ask-1" {
		t.Errorf("tool message 4 = %+v; want call-ask-1", input.Messages[4])
	}
}

// TestPublicModelMapsProviderCallsToNativeTools pins that a provider completion
// with nullable assistant content returns every provider call as a native tool
// request, preserving call ids, usage, finish reason and exact argument bytes.
func TestPublicModelMapsProviderCallsToNativeTools(t *testing.T) {
	completion := api.ChatCompletionResponse{
		ID:    "cmpl-bridge-1",
		Model: "model-1",
		Choices: []api.ChatChoice{bridgeChoice(api.ChatMessage{
			Role:        "assistant",
			ContentNull: true,
			ToolCalls: []api.ChatToolCall{
				{
					ID:   "call-ask-1",
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      "kd_task_0ask",
						Arguments: bridgeArgumentsAsk,
					},
				},
				{
					ID:   "call-ask-2",
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      "kd_task_1ask",
						Arguments: bridgeArgumentsAskUnderscore,
					},
				},
			},
		}, "tool_calls")},
		Usage: api.ChatUsage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12},
	}
	transport := &fakeTransport{result: bridgeResult(t, completion)}
	model, err := NewPublicModel(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicModel: %v", err)
	}
	response, err := model.Invoke(context.Background(), agent.ModelRequest{
		Model:    "model-1",
		Messages: []agent.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(transport.invocations) != 1 {
		t.Fatalf("transport invocations = %d; want 1", len(transport.invocations))
	}
	if response.Content != "" {
		t.Errorf("content = %q; want the empty string for nullable provider content", response.Content)
	}
	if response.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q; want tool_calls", response.FinishReason)
	}
	if response.Usage != (agent.Usage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12}) {
		t.Errorf("usage = %+v", response.Usage)
	}
	if len(response.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d; want 2", len(response.ToolCalls))
	}
	for index, want := range []struct {
		id        string
		native    string
		arguments string
	}{
		{"call-ask-1", "task.ask", bridgeArgumentsAsk},
		{"call-ask-2", "task_ask", bridgeArgumentsAskUnderscore},
	} {
		call := response.ToolCalls[index]
		if call.ID != want.id {
			t.Errorf("call %d id = %q; want %q", index, call.ID, want.id)
		}
		if call.Name != want.native {
			t.Errorf("call %d name = %q; want the native name %q", index, call.Name, want.native)
		}
		if string(call.Arguments) != want.arguments {
			t.Errorf("call %d arguments = %s; want the exact bytes %q", index, call.Arguments, want.arguments)
		}
	}
	if err := response.Validate(); err != nil {
		t.Errorf("mapped response is invalid: %v", err)
	}
}

// TestPublicModelRefusesMalformedToolExchange pins that an invalid mapped
// public request never reaches the transport and a malformed provider exchange
// is refused with a classified fault instead of being returned.
func TestPublicModelRefusesMalformedToolExchange(t *testing.T) {
	validMessages := []agent.Message{{Role: "user", Content: "go"}}
	validTool := agent.ToolDeclaration{
		Name:       "kd_task_0ask",
		Parameters: json.RawMessage(bridgeParametersAskUnderscore),
	}

	requestCases := []struct {
		name    string
		request agent.ModelRequest
	}{
		{
			name: "duplicate declaration names",
			request: agent.ModelRequest{
				Model:    "model-1",
				Messages: validMessages,
				Tools:    []agent.ToolDeclaration{validTool, validTool},
			},
		},
		{
			name: "dotted declaration name",
			request: agent.ModelRequest{
				Model:    "model-1",
				Messages: validMessages,
				Tools: []agent.ToolDeclaration{{
					Name:       "task.ask",
					Parameters: json.RawMessage(bridgeParametersAskUnderscore),
				}},
			},
		},
		{
			name: "dotted history call name",
			request: agent.ModelRequest{
				Model: "model-1",
				Messages: []agent.Message{
					{Role: "user", Content: "go"},
					{
						Role: "assistant",
						ToolCalls: []agent.ToolCall{{
							ID:        "call-1",
							Name:      "task.ask",
							Arguments: `{}`,
						}},
					},
				},
			},
		},
	}
	for _, testCase := range requestCases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{result: modelResult(t)}
			model, err := NewPublicModel(transport, "project-1")
			if err != nil {
				t.Fatalf("NewPublicModel: %v", err)
			}
			if _, err := model.Invoke(context.Background(), testCase.request); err == nil {
				t.Fatal("Invoke accepted an invalid tool exchange")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Errorf("class = %v ok=%v; want invalid_input", class, ok)
			}
			if len(transport.invocations) != 0 {
				t.Fatalf("invalid input reached the transport: %d invocation(s)", len(transport.invocations))
			}
		})
	}

	callResponse := func(call api.ChatToolCall, finishReason string) api.ChatCompletionResponse {
		return api.ChatCompletionResponse{
			ID:    "cmpl-bridge-2",
			Model: "model-1",
			Choices: []api.ChatChoice{bridgeChoice(api.ChatMessage{
				Role:        "assistant",
				ContentNull: true,
				ToolCalls:   []api.ChatToolCall{call},
			}, finishReason)},
			Usage: api.ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}
	}
	responseCases := []struct {
		name     string
		response api.ChatCompletionResponse
	}{
		{
			name: "malformed escape",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "kd_task_2ask", Arguments: `{}`},
			}, "tool_calls"),
		},
		{
			name: "incomplete escape",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "kd_task_0", Arguments: `{}`},
			}, "tool_calls"),
		},
		{
			name: "unprefixed provider name",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "task_ask", Arguments: `{}`},
			}, "tool_calls"),
		},
		{
			name: "oversized encoded name",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "kd_" + strings.Repeat("a", 62), Arguments: `{}`},
			}, "tool_calls"),
		},
		{
			name: "arguments are not a JSON object",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "kd_task_0ask", Arguments: `["not", "an", "object"]`},
			}, "tool_calls"),
		},
		{
			name: "malformed arguments",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "function",
				Function: api.ChatToolCallFunction{Name: "kd_task_0ask", Arguments: `{"question":`},
			}, "tool_calls"),
		},
		{
			name: "unsupported call type",
			response: callResponse(api.ChatToolCall{
				ID: "call-1", Type: "text",
				Function: api.ChatToolCallFunction{Name: "kd_task_0ask", Arguments: `{}`},
			}, "tool_calls"),
		},
	}
	for _, testCase := range responseCases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{result: bridgeResult(t, testCase.response)}
			model, err := NewPublicModel(transport, "project-1")
			if err != nil {
				t.Fatalf("NewPublicModel: %v", err)
			}
			if _, err := model.Invoke(context.Background(), agent.ModelRequest{
				Model:    "model-1",
				Messages: validMessages,
			}); err == nil {
				t.Fatal("Invoke accepted a malformed provider exchange")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Errorf("class = %v ok=%v; want internal_failure", class, ok)
			}
			if len(transport.invocations) != 1 {
				t.Fatalf("transport invocations = %d; want the one provider call", len(transport.invocations))
			}
		})
	}
}
