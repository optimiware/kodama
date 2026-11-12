//go:build integration

package services

// Real-boundary evidence for the deterministic model provider fixture.
//
// Each test drives the real served composition: the real router/authorizer, the
// real configured model gateway, the real project-scoped secret store and the
// real usage rows. Only the upstream model is the scripted loopback TLS fixture;
// nothing here fakes the gateway, the credential custody or the persistence it
// asserts.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	modelFxCallID      = "call-ask-1"
	modelFxArguments   = `{"question":"Choose a branch","assignment":"role/reviewer"}`
	modelFxNativeTool  = "task.ask"
	modelFxFinalAnswer = "role/reviewer selected"
)

// modelFxProviderToolName returns the declared provider name and refuses a
// drift from the documented kd_task_0ask encoding.
func modelFxProviderToolName(t *testing.T) string {
	t.Helper()
	name, err := agent.ProviderToolName(modelFxNativeTool)
	if err != nil {
		t.Fatalf("agent.ProviderToolName(%q): %v", modelFxNativeTool, err)
	}
	if name != "kd_task_0ask" {
		t.Fatalf("provider tool name = %q; want kd_task_0ask", name)
	}
	return name
}

// modelFxTool declares the native task.ask operation in provider grammar.
func modelFxTool(t *testing.T) api.ChatTool {
	t.Helper()
	return api.ChatTool{
		Type: "function",
		Function: api.ChatToolFunction{
			Name:        modelFxProviderToolName(t),
			Description: "ask one question of a named assignment",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"question":{"type":"string"},` +
				`"assignment":{"type":"string"}},` +
				`"required":["question","assignment"]}`),
		},
	}
}

// modelFxToolExchange scripts the first provider exchange: the declarations and
// a nullable-content tool call with string-valued JSON-object arguments.
func modelFxToolExchange(t *testing.T) factory.ModelExchange {
	t.Helper()
	name := modelFxProviderToolName(t)
	return factory.ModelExchange{
		Name:   "tool-call",
		Expect: factory.ModelExpectation{Tools: []string{name}},
		Reply: factory.ModelReply{
			ContentNull: true,
			ToolCalls: []api.ChatToolCall{{
				ID:   modelFxCallID,
				Type: "function",
				Function: api.ChatToolCallFunction{
					Name:      name,
					Arguments: modelFxArguments,
				},
			}},
			FinishReason: "tool_calls",
			Usage:        api.ChatUsage{PromptTokens: 17, CompletionTokens: 9, TotalTokens: 26},
		},
	}
}

// modelFxFinalExchange scripts the correlated continuation and the final
// content completion.
func modelFxFinalExchange(t *testing.T) factory.ModelExchange {
	t.Helper()
	name := modelFxProviderToolName(t)
	return factory.ModelExchange{
		Name:   "final-content",
		Expect: factory.ModelExpectation{Tools: []string{name}, ToolCallID: modelFxCallID},
		Reply: factory.ModelReply{
			Content:      modelFxFinalAnswer,
			FinishReason: "stop",
			Usage:        api.ChatUsage{PromptTokens: 31, CompletionTokens: 4, TotalTokens: 35},
		},
	}
}

// modelFxPlainExchange is one final-content exchange for tests that do not
// exercise tool calling.
func modelFxPlainExchange(t *testing.T, answer string) factory.ModelExchange {
	t.Helper()
	return factory.ModelExchange{
		Name:   "final-content",
		Expect: factory.ModelExpectation{},
		Reply: factory.ModelReply{
			Content:      answer,
			FinishReason: "stop",
			Usage:        api.ChatUsage{PromptTokens: 11, CompletionTokens: 5, TotalTokens: 16},
		},
	}
}

// modelFxRequest builds one configured-alias completion request.
func modelFxRequest(messages api.Messages, tools []api.ChatTool) api.ModelInvokeRequest {
	return api.ModelInvokeRequest{
		ProjectID: factory.ProjectID,
		Model:     factory.ModelAlias,
		Messages:  messages,
		MaxTokens: 64,
		Tools:     tools,
	}
}

// modelFxHistory appends the assistant tool call and its matching tool result
// to the conversation.
func modelFxHistory(messages api.Messages, calls []api.ChatToolCall) api.Messages {
	history := append(api.Messages(nil), messages...)
	history = append(history,
		api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: calls},
		api.ChatMessage{Role: "tool", ToolCallID: modelFxCallID, Content: "role/reviewer"},
	)
	return history
}

// modelFxRowInt reads one integer usage value.
func modelFxRowInt(t *testing.T, values map[string]any, key string) int64 {
	t.Helper()
	value, ok := values[key].(int64)
	if !ok {
		t.Fatalf("usage value %q = %v (%T); want int64", key, values[key], values[key])
	}
	return value
}

// TestFactoryModelToolCallingThroughRESTAndMCP proves both public edges carry
// the declared tool-calling subset to the configured upstream and back, and
// that the second call carries correlated history. The fixture advances only on
// the exact declarations and correlated tool identity.
//
// Mutation reasoning: forwarding the caller alias instead of the upstream model
// leaves the fixture refusing every call; dropping tools, assistant tool_calls,
// the null content marker or the string-valued arguments changes what the
// fixture requires before it advances; dropping usage settlement loses the two
// real rows.
func TestFactoryModelToolCallingThroughRESTAndMCP(t *testing.T) {
	fixture := factory.NewModelFixture(t, modelFxToolExchange(t), modelFxFinalExchange(t))
	harness := factory.Start(t, factory.WithModelFixture(fixture))
	tool := modelFxTool(t)
	first := modelFxRequest(api.Messages{
		{Role: "system", Content: "plan the work"},
		{Role: "user", Content: "choose a branch"},
	}, []api.ChatTool{tool})

	result := harness.MustREST(harness.ProjectToken, "model.invoke", first, nil, "model-tool-rest")
	var completion api.ChatCompletionResponse
	harness.Decode(result, &completion)
	if len(completion.Choices) != 1 {
		t.Fatalf("completion choices = %+v; want one", completion.Choices)
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q; want tool_calls", choice.FinishReason)
	}
	if !choice.Message.ContentNull {
		t.Fatal("assistant content is not the explicit JSON null")
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v; want one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != modelFxCallID {
		t.Fatalf("tool call id = %q; want %q", call.ID, modelFxCallID)
	}
	if call.Function.Name != modelFxProviderToolName(t) {
		t.Fatalf("tool name = %q; want the translated provider name", call.Function.Name)
	}
	if call.Function.Arguments != modelFxArguments {
		t.Fatalf("tool arguments = %q; want the string-valued JSON object %q", call.Function.Arguments, modelFxArguments)
	}

	second := modelFxRequest(modelFxHistory(first.Messages, choice.Message.ToolCalls), []api.ChatTool{tool})
	mcpResult := harness.MustMCP(harness.ProjectToken, "model.invoke", second, nil, "model-tool-mcp")
	var final api.ChatCompletionResponse
	harness.Decode(mcpResult, &final)
	if len(final.Choices) != 1 || final.Choices[0].FinishReason != "stop" {
		t.Fatalf("final completion = %+v; want one stop choice", final)
	}
	if final.Choices[0].Message.Content != modelFxFinalAnswer || final.Choices[0].Message.ContentNull {
		t.Fatalf("final content = %+v; want %q", final.Choices[0].Message, modelFxFinalAnswer)
	}
	if final.Usage.TotalTokens != 35 {
		t.Fatalf("final usage = %+v; want the scripted deterministic counts", final.Usage)
	}

	fixture.AssertSatisfied(t)
	requests := fixture.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d; want one per edge", len(requests))
	}
	if got := requests[0].ToolCallIDs; len(got) != 0 {
		t.Fatalf("first provider request already carried tool calls %v", got)
	}
	if got := requests[0].Tools; len(got) != 1 || got[0] != modelFxProviderToolName(t) {
		t.Fatalf("first provider declarations = %v; want the translated tool", got)
	}
	if got := requests[1].ToolCallIDs; len(got) != 1 || got[0] != modelFxCallID {
		t.Fatalf("second provider tool calls = %v; want the correlated %q", got, modelFxCallID)
	}
	if got := requests[1].ToolResultIDs; len(got) != 1 || got[0] != modelFxCallID {
		t.Fatalf("second provider tool results = %v; want the correlated %q", got, modelFxCallID)
	}
	for index, request := range requests {
		if request.Model != factory.ModelUpstreamModel {
			t.Fatalf("provider request %d model = %q; want the configured upstream %q", index, request.Model, factory.ModelUpstreamModel)
		}
		if !request.ProviderCredential {
			t.Fatalf("provider request %d did not carry the resolved provider credential", index)
		}
	}

	usageRows := modelFxUsageRows(t, harness)
	if len(usageRows) != 2 {
		t.Fatalf("model usage rows = %d; want one per edge", len(usageRows))
	}
	for _, row := range usageRows {
		if row.Values["principal_id"] != factory.AdminPrincipal {
			t.Fatalf("usage principal = %v; want the authenticated administrator", row.Values["principal_id"])
		}
		if row.Values["execution_id"] != nil {
			t.Fatalf("human usage execution_id = %v; want nil", row.Values["execution_id"])
		}
		if row.Values["model"] != factory.ModelAlias || row.Values["upstream_model"] != factory.ModelUpstreamModel {
			t.Fatalf("usage attribution = %v/%v; want %q/%q",
				row.Values["model"], row.Values["upstream_model"], factory.ModelAlias, factory.ModelUpstreamModel)
		}
		if row.Values["outcome"] != string(gateway.UsageSucceeded) {
			t.Fatalf("usage outcome = %v; want succeeded", row.Values["outcome"])
		}
	}
}

// TestFactoryModelConfigurationSurvivesControllerRestart proves the scripted
// provider state survives a controller replacement while every controller owns
// a freshly built CA-verifying transport.
func TestFactoryModelConfigurationSurvivesControllerRestart(t *testing.T) {
	fixture := factory.NewModelFixture(t, modelFxToolExchange(t), modelFxFinalExchange(t))
	harness := factory.Start(t, factory.WithModelFixture(fixture))
	if built := fixture.TransportsBuilt(); built != 1 {
		t.Fatalf("transports built before restart = %d; want one per controller", built)
	}

	tool := modelFxTool(t)
	first := modelFxRequest(api.Messages{{Role: "user", Content: "choose a branch"}}, []api.ChatTool{tool})
	result := harness.MustREST(harness.ProjectToken, "model.invoke", first, nil, "model-restart-first")
	var completion api.ChatCompletionResponse
	harness.Decode(result, &completion)
	if len(completion.Choices) != 1 || len(completion.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("first completion = %+v; want one tool call", completion)
	}

	harness.RestartController()
	if built := fixture.TransportsBuilt(); built != 2 {
		t.Fatalf("transports built after restart = %d; want a fresh one for the new controller", built)
	}

	second := modelFxRequest(modelFxHistory(first.Messages, completion.Choices[0].Message.ToolCalls), []api.ChatTool{tool})
	mcpResult := harness.MustMCP(harness.ProjectToken, "model.invoke", second, nil, "model-restart-second")
	var final api.ChatCompletionResponse
	harness.Decode(mcpResult, &final)
	if len(final.Choices) != 1 || final.Choices[0].Message.Content != modelFxFinalAnswer {
		t.Fatalf("post-restart completion = %+v; want the scripted final content", final)
	}
	fixture.AssertSatisfied(t)

	usageRows := modelFxUsageRows(t, harness)
	if len(usageRows) != 2 {
		t.Fatalf("model usage rows after restart = %d; want two persistent rows", len(usageRows))
	}
}

// TestFactoryModelUsageAndCredentialIsolation proves usage attribution, caller
// versus provider credential separation, the absence of raw capture and the
// model-disabled default.
//
// Mutation reasoning: dropping the usage commit loses the row; forwarding the
// caller session token makes the fixture refuse the call; recording the
// Authorization header or provider credential in capture, logs or evidence makes
// AssertCredentialAbsent fail; enabling content capture creates a content row;
// enabling models by default makes the plain harness answer instead of refusing.
func TestFactoryModelUsageAndCredentialIsolation(t *testing.T) {
	fixture := factory.NewModelFixture(t, modelFxPlainExchange(t, "isolated answer"))
	harness := factory.Start(t, factory.WithModelFixture(fixture))

	request := modelFxRequest(api.Messages{{Role: "user", Content: "answer"}}, nil)
	result := harness.MustREST(harness.ProjectToken, "model.invoke", request, nil, "model-usage")
	var completion api.ChatCompletionResponse
	harness.Decode(result, &completion)
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "isolated answer" {
		t.Fatalf("completion = %+v; want the scripted answer", completion)
	}
	if completion.Usage != (api.ChatUsage{PromptTokens: 11, CompletionTokens: 5, TotalTokens: 16}) {
		t.Fatalf("completion usage = %+v; want the deterministic provider counts", completion.Usage)
	}

	usageRows := modelFxUsageRows(t, harness)
	if len(usageRows) != 1 {
		t.Fatalf("model usage rows = %d; want exactly one", len(usageRows))
	}
	row := usageRows[0]
	for key, want := range map[string]int64{"prompt_tokens": 11, "completion_tokens": 5, "total_tokens": 16} {
		if got := modelFxRowInt(t, row.Values, key); got != want {
			t.Fatalf("usage %s = %d; want %d", key, got, want)
		}
	}
	if row.Values["usage_known"] != true {
		t.Fatalf("usage_known = %v; want true for a validated completion", row.Values["usage_known"])
	}
	if row.Values["principal_id"] != factory.AdminPrincipal || row.Values["execution_id"] != nil {
		t.Fatalf("usage attribution = principal %v execution %v; want the human administrator with no execution",
			row.Values["principal_id"], row.Values["execution_id"])
	}
	if row.Values["model"] != factory.ModelAlias || row.Values["upstream_model"] != factory.ModelUpstreamModel {
		t.Fatalf("usage model attribution = %v/%v; want the configured alias/upstream pair",
			row.Values["model"], row.Values["upstream_model"])
	}
	if rows := modelFxContentRows(t, harness); len(rows) != 0 {
		t.Fatalf("content rows = %d; want none without explicit retention", len(rows))
	}

	requests := fixture.Requests()
	if len(requests) != 1 || !requests[0].ProviderCredential {
		t.Fatalf("provider capture = %+v; want exactly one provider-credential-authenticated call", requests)
	}
	evidence := []string{
		string(result.Body),
		harness.Logs(),
		fmt.Sprintf("%+v", requests),
		fmt.Sprintf("%v", row.Values),
	}
	fixture.AssertCredentialAbsent(t, evidence...)
	for index, value := range evidence {
		if strings.Contains(value, harness.ProjectToken) {
			t.Fatalf("evidence value %d carries the caller session credential", index)
		}
	}

	// Refusal controls: a streaming request is outside the declared subset and
	// an unknown alias is not configured. Neither reaches the provider.
	streaming := request
	streaming.Stream = true
	if _, publicError := harness.REST(harness.ProjectToken, "model.invoke", streaming, nil, "model-stream-refused"); publicError.Code != api.CodeInvalidInput {
		t.Fatalf("streaming refusal code = %q; want %q", publicError.Code, api.CodeInvalidInput)
	}
	unknown := request
	unknown.Model = "not-configured"
	if _, publicError := harness.REST(harness.ProjectToken, "model.invoke", unknown, nil, "model-unknown-refused"); publicError.Code != api.CodeNotFound {
		t.Fatalf("unknown model refusal code = %q; want %q", publicError.Code, api.CodeNotFound)
	}
	if got := len(fixture.Requests()); got != 1 {
		t.Fatalf("provider requests after refusals = %d; want only the admitted call", got)
	}
	if got := len(modelFxUsageRows(t, harness)); got != 1 {
		t.Fatalf("model usage rows after refusals = %d; want only the admitted call", got)
	}
	fixture.AssertSatisfied(t)

	// The default factory constructs no model gateway at all.
	plain := factory.Start(t)
	if _, publicError := plain.REST(plain.ProjectToken, "model.invoke", request, nil, "model-disabled"); publicError.Code != api.CodeNotFound {
		t.Fatalf("model-disabled refusal code = %q; want %q", publicError.Code, api.CodeNotFound)
	}
	if plain.Model != nil {
		t.Fatal("the default factory exposed a model fixture")
	}
}

// TestFactoryModelServiceAuthority proves a project service account with the
// engineer role can invoke the configured model and is attributed as itself,
// while a service without that role is refused before any provider call.
func TestFactoryModelServiceAuthority(t *testing.T) {
	fixture := factory.NewModelFixture(t, modelFxPlainExchange(t, "service answer"))
	harness := factory.Start(t, factory.WithModelFixture(fixture))

	engineerCredential := serviceCredentialWithRoles(t, harness, "model-engineer", []string{"engineer"})
	request := modelFxRequest(api.Messages{{Role: "user", Content: "answer as a service"}}, nil)
	result := harness.MustREST(engineerCredential, "model.invoke", request, nil, "model-service-engineer")
	var completion api.ChatCompletionResponse
	harness.Decode(result, &completion)
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "service answer" {
		t.Fatalf("service completion = %+v; want the scripted answer", completion)
	}

	usageRows := modelFxUsageRows(t, harness)
	if len(usageRows) != 1 {
		t.Fatalf("service usage rows = %d; want exactly one", len(usageRows))
	}
	row := usageRows[0]
	if row.Values["principal_id"] != "model-engineer" {
		t.Fatalf("service usage principal = %v; want the service identity", row.Values["principal_id"])
	}
	if row.Values["execution_id"] != nil {
		t.Fatalf("service usage execution_id = %v; want nil for a direct service call", row.Values["execution_id"])
	}

	operatorCredential := serviceCredentialWithRoles(t, harness, "model-operator", []string{"operator"})
	if _, publicError := harness.REST(operatorCredential, "model.invoke", request, nil, "model-service-operator"); publicError.Code != api.CodeForbidden {
		t.Fatalf("operator service refusal code = %q; want %q", publicError.Code, api.CodeForbidden)
	}
	if got := len(fixture.Requests()); got != 1 {
		t.Fatalf("provider requests after refusal = %d; want only the engineer call", got)
	}
	if got := len(modelFxUsageRows(t, harness)); got != 1 {
		t.Fatalf("usage rows after refusal = %d; want only the engineer call", got)
	}
	fixture.AssertSatisfied(t)
}

// modelFxUsageRows reads the committed model_usage rows.
func modelFxUsageRows(t *testing.T, harness *factory.Harness) []store.Row {
	t.Helper()
	rows, err := harness.ModelUsageRows(context.Background())
	if err != nil {
		t.Fatalf("read model usage rows: %v", err)
	}
	return rows
}

// modelFxContentRows reads the committed content_records rows.
func modelFxContentRows(t *testing.T, harness *factory.Harness) []store.Row {
	t.Helper()
	rows, err := harness.ModelContentRows(context.Background())
	if err != nil {
		t.Fatalf("read model content rows: %v", err)
	}
	return rows
}
