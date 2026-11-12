//go:build integration

package composition

// Composition evidence: the configured model gateway receives the complete
// declared tool-calling request from both public edges. The provider
// observes the tools, the assistant tool_calls, the tool message identity, the
// JSON-null assistant content and the untouched string-valued arguments; the
// completion with tool_calls round-trips to the caller; usage attribution is
// unchanged.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const modelToolArguments = `{"path":"README.md","note":"héllo ✓"}`

// toolBodyUpstream is a deterministic local TLS provider that records the exact
// request body it observed and answers one tool-calling completion.
type toolBodyUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []json.RawMessage
	models []string
}

func newToolBodyUpstream(t *testing.T) *toolBodyUpstream {
	t.Helper()
	upstream := &toolBodyUpstream{}
	upstream.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &decoded)
		upstream.mu.Lock()
		upstream.bodies = append(upstream.bodies, append(json.RawMessage(nil), body...))
		upstream.models = append(upstream.models, decoded.Model)
		upstream.mu.Unlock()
		response.Header().Set("Content-Type", api.MediaType)
		_, _ = fmt.Fprintf(response,
			`{"id":"cmpl-tool-edge-1","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-2","type":"function","function":{"name":"repo_read","arguments":"{\"path\":\"README.md\",\"note\":\"héllo ✓\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`,
			decoded.Model)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (upstream *toolBodyUpstream) transport() http.RoundTripper {
	return upstream.server.Client().Transport
}

func (upstream *toolBodyUpstream) received() ([]json.RawMessage, []string) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	bodies := make([]json.RawMessage, len(upstream.bodies))
	copy(bodies, upstream.bodies)
	models := append([]string(nil), upstream.models...)
	return bodies, models
}

// modelToolRequest is one valid tool-calling completion input for the
// configured model alias.
func modelToolRequest(model string) api.ModelInvokeRequest {
	return api.ModelInvokeRequest{
		ProjectID: projectID,
		Model:     model,
		MaxTokens: 64,
		Messages: []api.ChatMessage{
			{Role: "system", Content: "plan the work"},
			{Role: "user", Content: "read the readme"},
			{
				Role:        "assistant",
				ContentNull: true,
				ToolCalls: []api.ChatToolCall{{
					ID:   "call-1",
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      "repo_read",
						Arguments: `{"path":"README.md"}`,
					},
				}},
			},
			{Role: "tool", ToolCallID: "call-1", Content: "README contents"},
		},
		Tools: []api.ChatTool{{
			Type: "function",
			Function: api.ChatToolFunction{
				Name:        "repo_read",
				Description: "read one repository file",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		}},
	}
}

// providerRequest decodes one observed provider body into the completion the
// gateway should have forwarded.
func providerRequest(t *testing.T, body json.RawMessage, wantModel string) api.ChatCompletionRequest {
	t.Helper()
	request, err := api.DecodeChatCompletionRequest(body)
	if err != nil {
		t.Fatalf("decode provider body: %v; body=%s", err, string(body))
	}
	if request.Model != wantModel {
		t.Fatalf("provider model = %q; want the configured upstream %q", request.Model, wantModel)
	}
	return request
}

// TestConfiguredModelToolCallingThroughRESTAndMCP proves both public edges
// forward the complete tool-calling request to the configured provider and
// return the provider's tool-calling completion, with usage attribution
// unchanged.
//
// Mutation reasoning: dropping tools, assistant tool_calls, tool_call_id or
// the null-content marker in the ops binding changes the provider body decoded
// here; re-encoding the string-valued arguments changes their exact value;
// dropping the provider tool_calls or finish reason changes the caller
// completion; skipping usage settlement loses the two usage rows.
func TestConfiguredModelToolCallingThroughRESTAndMCP(t *testing.T) {
	upstream := newToolBodyUpstream(t)
	harness := newHarness(t)
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	harness.config.Models = &app.ModelsConfig{
		Models: []gateway.ModelConfig{
			modelEdgeConfig(scope, modelAlias, modelUpstream, upstream.server.URL+"/v1/chat/completions", false),
		},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelSecretName},
		}},
		Transport: upstream.transport(),
	}
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)
	environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  string(modelSecretName),
		Value:     modelSecretValue,
	}, nil, "model-tool-secret-write")

	request := modelToolRequest(modelAlias)
	restResult, restError := environment.call(projectToken, "model.invoke", request, nil, "")
	if restError.Code != "" {
		t.Fatalf("REST model.invoke code = %q message = %q", restError.Code, restError.Message)
	}
	mcpResult, mcpError, err := environment.mcpCall(projectToken, "model.invoke", request, nil, "")
	if err != nil {
		t.Fatalf("MCP model.invoke: %v", err)
	}
	if mcpError.Code != "" {
		t.Fatalf("MCP model.invoke code = %q message = %q", mcpError.Code, mcpError.Message)
	}

	var restCompletion, mcpCompletion api.ChatCompletionResponse
	environment.decode(restResult, &restCompletion)
	if err := json.Unmarshal(mcpResult.Body, &mcpCompletion); err != nil {
		t.Fatalf("decode MCP completion: %v; body=%s", err, string(mcpResult.Body))
	}
	if !reflect.DeepEqual(restCompletion, mcpCompletion) {
		t.Fatalf("REST and MCP completions differ:\nrest=%+v\nmcp=%+v", restCompletion, mcpCompletion)
	}
	if len(restCompletion.Choices) != 1 {
		t.Fatalf("completion choices = %+v; want one", restCompletion.Choices)
	}
	choice := restCompletion.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("caller finish reason = %q; want tool_calls", choice.FinishReason)
	}
	if !choice.Message.ContentNull {
		t.Fatal("caller lost the null assistant content")
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("caller tool calls = %+v; want one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call-2" || call.Function.Name != "repo_read" || call.Function.Arguments != modelToolArguments {
		t.Fatalf("caller tool call = %+v; want the provider's exact identity and arguments", call)
	}

	bodies, models := upstream.received()
	if len(bodies) != 2 {
		t.Fatalf("provider calls = %d; want one per public edge", len(bodies))
	}
	want := api.ChatCompletionRequest{
		Model:     modelUpstream,
		Messages:  request.Messages,
		MaxTokens: request.MaxTokens,
		Tools:     request.Tools,
	}
	for index, body := range bodies {
		if models[index] != modelUpstream {
			t.Fatalf("call %d upstream model = %q; want %q", index, models[index], modelUpstream)
		}
		observed := providerRequest(t, body, modelUpstream)
		if !reflect.DeepEqual(observed, want) {
			t.Fatalf("call %d provider-observed request = %+v\nwant %+v", index, observed, want)
		}
		if !strings.Contains(string(body), `"content":null`) {
			t.Fatalf("call %d provider body does not carry explicit null assistant content: %s", index, string(body))
		}
		for messageIndex, message := range observed.Messages {
			if message.ContentNull != request.Messages[messageIndex].ContentNull {
				t.Fatalf("call %d message[%d] null content = %v; want %v", index, messageIndex, message.ContentNull, request.Messages[messageIndex].ContentNull)
			}
			if !reflect.DeepEqual(message.ToolCalls, request.Messages[messageIndex].ToolCalls) {
				t.Fatalf("call %d message[%d] tool calls = %+v; want %+v", index, messageIndex, message.ToolCalls, request.Messages[messageIndex].ToolCalls)
			}
			if message.ToolCallID != request.Messages[messageIndex].ToolCallID {
				t.Fatalf("call %d message[%d] tool_call_id = %q; want %q", index, messageIndex, message.ToolCallID, request.Messages[messageIndex].ToolCallID)
			}
		}
	}

	usageRows := modelEdgeUsageRows(t, harness.store, scope)
	if len(usageRows) != 2 {
		t.Fatalf("model usage rows = %d; want one per public edge", len(usageRows))
	}
	for _, row := range usageRows {
		if row.Values["principal_id"] != "admin-1" {
			t.Fatalf("usage principal = %v; want the authenticated caller admin-1", row.Values["principal_id"])
		}
		if row.Values["model"] != modelAlias || row.Values["upstream_model"] != modelUpstream {
			t.Fatalf("usage attribution = %v/%v; want %q/%q",
				row.Values["model"], row.Values["upstream_model"], modelAlias, modelUpstream)
		}
		if row.Values["outcome"] != string(gateway.UsageSucceeded) {
			t.Fatalf("usage outcome = %v; want succeeded", row.Values["outcome"])
		}
		if prompt, _ := row.Values["prompt_tokens"].(int64); prompt != 12 {
			t.Fatalf("usage prompt tokens = %v; want the provider count 12", row.Values["prompt_tokens"])
		}
	}
}
