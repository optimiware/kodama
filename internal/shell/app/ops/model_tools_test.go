package opsapp_test

// Model tool-calling propagation tests. The public model.invoke
// request decodes the complete tool-calling vocabulary; the ops
// binding must hand every field to the gateway model port unchanged, in both
// directions, over both the REST and the MCP entry shapes. The gateway
// marshals the complete request, so the port request is exactly what
// the provider observes.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// providerCapture records the exact port invocation the ops binding made.
type providerCapture struct {
	calls      int
	principals []authorize.Principal
	projects   []id.ProjectID
	requests   []api.ChatCompletionRequest
	response   api.ChatCompletionResponse
	err        error
}

func (capture *providerCapture) Invoke(
	_ context.Context,
	principal authorize.Principal,
	project id.ProjectID,
	request api.ChatCompletionRequest,
) (api.ChatCompletionResponse, error) {
	capture.calls++
	capture.principals = append(capture.principals, principal)
	capture.projects = append(capture.projects, project)
	capture.requests = append(capture.requests, request)
	if capture.err != nil {
		return api.ChatCompletionResponse{}, capture.err
	}
	return capture.response, nil
}

// useModelPort rebuilds the real binding table and router over the captured
// model port. Every other dependency stays the harness's real service.
func useModelPort(t *testing.T, h *harness, port opsapp.ModelInvocation) {
	t.Helper()
	deps := h.deps
	deps.Models = port
	bindings, err := opsapp.Bindings(deps)
	if err != nil {
		t.Fatalf("Bindings with captured model port: %v", err)
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.ownership,
		Executions: h.ownership,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter with captured model port: %v", err)
	}
	h.router = router
}

// toolCompletion is the provider completion the fake port returns: the
// response direction must carry the call identity, the string-valued arguments
// byte for byte, the null content and the finish reason to the caller.
func toolCompletion(model string) api.ChatCompletionResponse {
	return api.ChatCompletionResponse{
		ID:    "chat-tool-1",
		Model: model,
		Choices: []api.ChatChoice{{
			Index: 0,
			Message: api.ChatMessage{
				Role:        "assistant",
				ContentNull: true,
				ToolCalls: []api.ChatToolCall{{
					ID:   "call-2",
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      "repo_read",
						Arguments: `{"path":"README.md","note":"héllo ✓"}`,
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: api.ChatUsage{PromptTokens: 12, CompletionTokens: 4, TotalTokens: 16},
	}
}

// modelToolsVector reads the tool-calling wire vector.
func modelToolsVector(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "contracts", "public", "testdata", "wire", "model-tools.json"))
	if err != nil {
		t.Fatalf("read model tool vector: %v", err)
	}
	var vector struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatalf("decode model tool vector: %v", err)
	}
	return vector.Request
}

// mcpModelBody adds the MCP-only project identity to one request body.
func mcpModelBody(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	fields["project_id"] = testProject
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode MCP request body: %v", err)
	}
	return string(encoded)
}

// modelRequestFromCaller decodes the exact body one edge sends into the
// completion the binding must hand to the model port. It uses the same public
// decoders the router uses, so the expectation is the caller's request after
// the wire decode and before the ops binding.
func modelRequestFromCaller(t *testing.T, body string, mcp bool) api.ChatCompletionRequest {
	t.Helper()
	if !mcp {
		request, err := api.DecodeChatCompletionRequest([]byte(body))
		if err != nil {
			t.Fatalf("decode REST request: %v", err)
		}
		return request
	}
	decoded, err := api.DecodeMCPOperationRequest("model.invoke", json.RawMessage(body))
	if err != nil {
		t.Fatalf("decode MCP request: %v", err)
	}
	request, ok := decoded.(api.ModelInvokeRequest)
	if !ok {
		t.Fatalf("MCP request decoded into %T", decoded)
	}
	return api.ChatCompletionRequest{
		Model:       request.Model,
		Messages:    request.Messages,
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
		Tools:       request.Tools,
	}
}

// TestModelInvokeForwardsCompleteToolRequest pins the request direction: the
// provider port receives exactly the tools, assistant tool_calls, tool message
// identity, null content and untouched string arguments that the caller sent,
// on both public entry shapes.
func TestModelInvokeForwardsCompleteToolRequest(t *testing.T) {
	vector := modelToolsVector(t)
	vectorRequest, err := api.DecodeChatCompletionRequest(vector)
	if err != nil {
		t.Fatalf("decode model tool vector: %v", err)
	}
	if len(vectorRequest.Tools) == 0 {
		t.Fatal("fixture has no tools; the test would be vacuous")
	}

	unicodeArguments := json.RawMessage(`{
		"model": "fixture-model",
		"messages": [
			{"role": "user", "content": "run the tool"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call-u1", "type": "function", "function": {"name": "repo_read", "arguments": "{\"a\":\"1\",\"text\":\"héllo ✓\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call-u1", "content": "tool result"},
			{"role": "assistant", "content": ""}
		],
		"tools": [
			{"type": "function", "function": {"name": "repo_read", "description": "réad one file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}
		],
		"max_tokens": 32
	}`)
	emptyTools := json.RawMessage(`{
		"model": "fixture-model",
		"messages": [{"role": "user", "content": "no tools"}],
		"tools": []
	}`)

	cases := []struct {
		name    string
		body    string
		params  map[string]string
		project id.ProjectID
		mcp     bool
	}{
		{
			name:    "rest tool vector",
			body:    string(vector),
			params:  map[string]string{"project": testProject, "model": vectorRequest.Model},
			project: testProject,
		},
		{
			name:    "mcp tool vector",
			body:    mcpModelBody(t, vector),
			project: testProject,
			mcp:     true,
		},
		{
			name:    "rest unicode arguments",
			body:    string(unicodeArguments),
			params:  map[string]string{"project": testProject, "model": "fixture-model"},
			project: testProject,
		},
		{
			name:    "mcp unicode arguments",
			body:    mcpModelBody(t, unicodeArguments),
			project: testProject,
			mcp:     true,
		},
		{
			name:    "rest explicit empty tools",
			body:    string(emptyTools),
			params:  map[string]string{"project": testProject, "model": "fixture-model"},
			project: testProject,
		},
		{
			name:    "mcp explicit empty tools",
			body:    mcpModelBody(t, emptyTools),
			project: testProject,
			mcp:     true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// The expectation is the request as the caller sent it over this
			// edge; the MCP decoder normalizes JSON whitespace, which is not
			// a value change.
			want := modelRequestFromCaller(t, testCase.body, testCase.mcp)
			h := newHarness(t)
			capture := &providerCapture{response: toolCompletion(want.Model)}
			useModelPort(t, h, capture)

			if _, err := h.invoke(t, testToken, "model.invoke", testCase.body, testCase.params); err != nil {
				t.Fatalf("model.invoke: %v", err)
			}
			if capture.calls != 1 {
				t.Fatalf("provider calls = %d; want exactly 1", capture.calls)
			}
			if capture.projects[0] != testCase.project {
				t.Fatalf("provider project = %q; want %q", capture.projects[0], testCase.project)
			}
			if capture.principals[0].ID != testEngineer {
				t.Fatalf("provider principal = %q; want the authenticated caller", capture.principals[0].ID)
			}
			if got := capture.requests[0]; !reflect.DeepEqual(got, want) {
				t.Fatalf("provider-observed request = %+v\nwant %+v", got, want)
			}
			// Field-level assertions so a future refactor cannot hide a drop
			// behind an accidentally equal aggregate.
			got := capture.requests[0]
			if !reflect.DeepEqual(got.Tools, want.Tools) {
				t.Fatalf("provider tools = %+v; want %+v", got.Tools, want.Tools)
			}
			if len(got.Messages) != len(want.Messages) {
				t.Fatalf("provider messages = %d; want %d", len(got.Messages), len(want.Messages))
			}
			for index, want := range want.Messages {
				if !reflect.DeepEqual(got.Messages[index], want) {
					t.Fatalf("provider message[%d] = %+v; want %+v", index, got.Messages[index], want)
				}
				if got.Messages[index].ContentNull != want.ContentNull {
					t.Fatalf("provider message[%d] null content = %v; want %v", index, got.Messages[index].ContentNull, want.ContentNull)
				}
				for callIndex, wantCall := range want.ToolCalls {
					if got.Messages[index].ToolCalls[callIndex].Function.Arguments != wantCall.Function.Arguments {
						t.Fatalf("provider message[%d] call[%d] arguments = %q; want byte-identical %q",
							index, callIndex, got.Messages[index].ToolCalls[callIndex].Function.Arguments, wantCall.Function.Arguments)
					}
				}
			}
		})
	}
}

// TestModelInvokeReturnsCompleteToolCompletion pins the response direction:
// a provider completion with tool_calls, null content and finish_reason
// tool_calls reaches the caller with the same call identity and exactly the
// same string-valued arguments.
func TestModelInvokeReturnsCompleteToolCompletion(t *testing.T) {
	h := newHarness(t)
	want := toolCompletion("fixture-model")
	capture := &providerCapture{response: want}
	useModelPort(t, h, capture)

	result, err := h.invoke(t, testToken, "model.invoke", `{
		"model": "fixture-model",
		"messages": [
			{"role": "user", "content": "read it"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call-1", "type": "function", "function": {"name": "repo_read", "arguments": "{\"path\":\"README.md\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call-1", "content": "README contents"}
		],
		"tools": [
			{"type": "function", "function": {"name": "repo_read", "parameters": {"type": "object"}}}
		]
	}`, map[string]string{"project": testProject, "model": "fixture-model"})
	if err != nil {
		t.Fatalf("model.invoke: %v", err)
	}
	var got api.ChatCompletionResponse
	decodeResult(t, result, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("caller completion = %+v; want %+v", got, want)
	}
	choice := got.Choices[0]
	if !choice.Message.ContentNull {
		t.Fatal("caller lost the null assistant content")
	}
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("caller finish reason = %q; want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("caller tool calls = %+v; want one", choice.Message.ToolCalls)
	}
	if arguments := choice.Message.ToolCalls[0].Function.Arguments; arguments != want.Choices[0].Message.ToolCalls[0].Function.Arguments {
		t.Fatalf("caller arguments = %q; want byte-identical %q", arguments, want.Choices[0].Message.ToolCalls[0].Function.Arguments)
	}
}

// TestModelInvokeRefusalsStayUnchanged pins the refusal shapes around the
// widened copy: malformed tool vocabulary is invalid_input before the model
// port, an unprivileged caller is forbidden before the port, and a foreign
// project is not_found before the port.
func TestModelInvokeRefusalsStayUnchanged(t *testing.T) {
	body := string(modelToolsVector(t))

	t.Run("invalid tool identity", func(t *testing.T) {
		h := newHarness(t)
		capture := &providerCapture{response: toolCompletion("fixture-model")}
		useModelPort(t, h, capture)
		_, err := h.invoke(t, testToken, "model.invoke", `{
			"model": "fixture-model",
			"messages": [{"role": "tool", "content": "missing the call identity"}]
		}`, map[string]string{"project": testProject, "model": "fixture-model"})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("missing tool_call_id refusal = %v; want invalid_input", err)
		}
		if capture.calls != 0 {
			t.Fatalf("model port calls = %d; want 0 before a refusal", capture.calls)
		}
	})

	t.Run("unauthorized caller", func(t *testing.T) {
		h := newHarness(t)
		capture := &providerCapture{response: toolCompletion("fixture-model")}
		useModelPort(t, h, capture)
		scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
		h.principals.principals["token-viewer"] = identity.Principal{
			ID:      "viewer-1",
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleViewer},
			Enabled: true,
			Version: 1,
		}
		h.seedMembership(t, scope, "membership-viewer", "viewer-1", 1, tenancy.RoleViewer)
		_, err := h.invoke(t, "token-viewer", "model.invoke", body, map[string]string{"project": testProject, "model": "fixture-model"})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("viewer refusal = %v; want forbidden", err)
		}
		if capture.calls != 0 {
			t.Fatalf("model port calls = %d; want 0 before a refusal", capture.calls)
		}
	})

	t.Run("foreign project", func(t *testing.T) {
		h := newHarness(t)
		capture := &providerCapture{response: toolCompletion("fixture-model")}
		useModelPort(t, h, capture)
		_, err := h.invoke(t, testToken, "model.invoke", body, map[string]string{"project": "project-b", "model": "fixture-model"})
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("foreign project refusal = %v; want not_found", err)
		}
		if capture.calls != 0 {
			t.Fatalf("model port calls = %d; want 0 before a refusal", capture.calls)
		}
	})
}
