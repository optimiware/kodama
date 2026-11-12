// The repository-level resource operation `git.read` and standard
// non-streaming OpenAI tool calling in the model subset: `tools` on the
// request, nullable assistant `content`, `tool_calls` on assistant messages
// and `tool_call_id` on tool messages, with bounded tool declarations,
// per-schema and aggregate schema bytes and argument bytes.

package publiccontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// paddedObject returns a valid JSON object of exactly size bytes.
func paddedObject(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const overhead = len(`{"p":""}`)
	if size < overhead {
		t.Fatalf("paddedObject(%d): size is below the minimal object", size)
	}
	object := `{"p":"` + strings.Repeat("a", size-overhead) + `"}`
	if len(object) != size {
		t.Fatalf("paddedObject(%d) built %d bytes", size, len(object))
	}
	return json.RawMessage(object)
}

// paddedArguments returns a JSON object encoded as a string of exactly size
// bytes.
func paddedArguments(t *testing.T, size int) string {
	t.Helper()
	argument := string(paddedObject(t, size))
	if len(argument) != size {
		t.Fatalf("paddedArguments(%d) built %d bytes", size, len(argument))
	}
	return argument
}

// tinyTool is one minimal valid tool declaration.
func tinyTool(name string) api.ChatTool {
	return api.ChatTool{
		Type: "function",
		Function: api.ChatToolFunction{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object"}`),
		},
	}
}

// toolMessage fixtures.
func toolCall(id, name, arguments string) api.ChatToolCall {
	return api.ChatToolCall{
		ID:   id,
		Type: "function",
		Function: api.ChatToolCallFunction{
			Name:      name,
			Arguments: arguments,
		},
	}
}

// Tool calling vectors. The request advertises one tool and a second turn in
// which the assistant message carries only tool calls (null content) and one
// correlated tool message returns the result.
const toolRequestJSON = `{
  "model": "fixture-model",
  "messages": [
    {"role": "system", "content": "plan the work"},
    {"role": "user", "content": "read the readme"},
    {
      "role": "assistant",
      "content": null,
      "tool_calls": [
        {"id": "call-1", "type": "function", "function": {"name": "repo_read", "arguments": "{\"path\":\"README.md\"}"}}
      ]
    },
    {"role": "tool", "tool_call_id": "call-1", "content": "README contents"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "repo_read",
        "description": "read one repository file",
        "parameters": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
      }
    }
  ],
  "max_tokens": 64
}`

// TestOpenAIToolCallingRoundTrip pins standard tool calling: tools are
// declared on the request, an assistant message may carry a null content and
// tool calls (arguments are a JSON STRING, not an object), and a correlated
// tool message names the call. Dropping any of tools/calls/IDs, collapsing a
// null content to an empty string, or accepting object-valued arguments must
// fail this test.
func TestOpenAIToolCallingRoundTrip(t *testing.T) {
	request, err := api.DecodeChatCompletionRequest([]byte(toolRequestJSON))
	if err != nil {
		t.Fatalf("DecodeChatCompletionRequest(tool calling): %v", err)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("decoded tools = %d; want 1", len(request.Tools))
	}
	if request.Tools[0].Function.Name != "repo_read" {
		t.Fatalf("decoded tool name = %q; want repo_read", request.Tools[0].Function.Name)
	}
	if len(request.Messages) != 4 {
		t.Fatalf("decoded messages = %d; want 4", len(request.Messages))
	}
	assistant := request.Messages[2]
	if assistant.Role != "assistant" {
		t.Fatalf("message 2 role = %q; want assistant", assistant.Role)
	}
	if !assistant.ContentNull {
		t.Fatal("assistant null content was collapsed to a non-null marker")
	}
	if assistant.Content != "" {
		t.Fatalf("assistant null content = %q; want the empty marker payload", assistant.Content)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d; want 1", len(assistant.ToolCalls))
	}
	if got := assistant.ToolCalls[0].ID; got != "call-1" {
		t.Fatalf("assistant call id = %q; want call-1", got)
	}
	if got := assistant.ToolCalls[0].Function.Arguments; got != `{"path":"README.md"}` {
		t.Fatalf("assistant call arguments = %q; want the JSON string object", got)
	}
	if request.Messages[3].ToolCallID != "call-1" {
		t.Fatalf("tool message tool_call_id = %q; want call-1", request.Messages[3].ToolCallID)
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal tool calling request: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}
	if _, ok := wire["tools"]; !ok {
		t.Fatalf("encoded request dropped tools: %s", encoded)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(wire["messages"], &messages); err != nil {
		t.Fatalf("unmarshal encoded messages: %v", err)
	}
	if got := strings.TrimSpace(string(messages[2]["content"])); got != "null" {
		t.Fatalf("encoded assistant content = %s; want JSON null", got)
	}
	if _, ok := messages[2]["tool_calls"]; !ok {
		t.Fatalf("encoded assistant dropped tool_calls: %s", encoded)
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(messages[2]["tool_calls"], &calls); err != nil {
		t.Fatalf("unmarshal encoded tool_calls: %v", err)
	}
	var function map[string]json.RawMessage
	if err := json.Unmarshal(calls[0]["function"], &function); err != nil {
		t.Fatalf("unmarshal encoded call function: %v", err)
	}
	if got := strings.TrimSpace(string(function["arguments"])); !strings.HasPrefix(got, `"`) {
		t.Fatalf("encoded arguments = %s; want a JSON string", got)
	}
	if got := strings.TrimSpace(string(messages[3]["tool_call_id"])); got != `"call-1"` {
		t.Fatalf("encoded tool_call_id = %s; want \"call-1\"", got)
	}

	var roundTripped api.ChatCompletionRequest
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}
	if err := roundTripped.Validate(); err != nil {
		t.Fatalf("round-tripped request refused: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, request) {
		t.Fatalf("tool calling round trip = %+v; want %+v", roundTripped, request)
	}

	// An empty (non-null) content stays an empty string and is not converted
	// to the null marker.
	emptyMessage, err := json.Marshal(api.ChatMessage{Role: "user", Content: ""})
	if err != nil {
		t.Fatalf("marshal empty content message: %v", err)
	}
	if got := string(emptyMessage); !strings.Contains(got, `"content":""`) {
		t.Fatalf("empty content message = %s; want an empty JSON string", got)
	}
	var decodedEmpty api.ChatMessage
	if err := json.Unmarshal(emptyMessage, &decodedEmpty); err != nil {
		t.Fatalf("unmarshal empty content message: %v", err)
	}
	if decodedEmpty.ContentNull {
		t.Fatal("an empty string content was inferred as null")
	}

	// The response carries the same assistant shape. A standard provider may
	// also include message members this subset does not model; a response is
	// lenient about them while a request is not.
	responseJSON := `{
      "id": "chat-1",
      "model": "fixture-model",
      "choices": [{
        "index": 0,
        "message": {
          "role": "assistant",
          "content": null,
          "refusal": null,
          "annotations": [],
          "tool_calls": [{"id": "call-2", "type": "function", "function": {"name": "repo_read", "arguments": "{}"}}]
        },
        "finish_reason": "tool_calls"
      }],
      "usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
    }`
	var response api.ChatCompletionResponse
	if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
		t.Fatalf("unmarshal tool calling response: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("tool calling response refused: %v", err)
	}
	if !response.Choices[0].Message.ContentNull {
		t.Fatal("response assistant content null was not preserved")
	}
	if len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatal("response assistant tool calls were dropped")
	}

	// Object-valued arguments are refused: arguments is a JSON string.
	objectArguments := strings.Replace(toolRequestJSON,
		`"arguments": "{\"path\":\"README.md\"}"`,
		`"arguments": {"path":"README.md"}`, 1)
	if _, err := api.DecodeChatCompletionRequest([]byte(objectArguments)); err == nil {
		t.Fatal("object-valued tool call arguments were accepted")
	}
}

// TestOpenAIToolCallingBounds pins every tool-calling bound: the exact
// bound passes, one byte or one item more refuses, schema bytes are counted
// over both one schema and the aggregate, and all bounds are byte counts (not
// rune counts).
func TestOpenAIToolCallingBounds(t *testing.T) {
	if api.MaxToolDeclarations != 32 ||
		api.MaxToolCallsPerMessage != 32 ||
		api.MaxToolParametersBytes != 64<<10 ||
		api.MaxToolSchemasBytes != 256<<10 ||
		api.MaxToolArgumentsBytes != 64<<10 ||
		api.MaxToolNameBytes != 64 {
		t.Fatalf("tool-calling bounds = %d/%d/%d/%d/%d/%d; want 32/32/65536/262144/65536/64",
			api.MaxToolDeclarations, api.MaxToolCallsPerMessage, api.MaxToolParametersBytes,
			api.MaxToolSchemasBytes, api.MaxToolArgumentsBytes, api.MaxToolNameBytes)
	}

	base := api.ChatCompletionRequest{
		Model:    "fixture-model",
		Messages: []api.ChatMessage{{Role: "user", Content: "x"}},
	}

	// Declared tools: exactly 32 pass, 33 refuse.
	tools := make([]api.ChatTool, 0, api.MaxToolDeclarations+1)
	for index := 0; index < api.MaxToolDeclarations; index++ {
		tools = append(tools, tinyTool("tool_"+strings.Repeat("a", 1)+string(rune('a'+index%26))+strings.Repeat("b", index/26)))
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    tools,
	}).Validate(); err != nil {
		t.Fatalf("exactly %d tools refused: %v", api.MaxToolDeclarations, err)
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    append(append([]api.ChatTool(nil), tools...), tinyTool("extra")),
	}).Validate(); err == nil {
		t.Fatalf("%d tools were accepted; want refusal", api.MaxToolDeclarations+1)
	}

	// One parameters schema: exactly 64 KiB passes, one byte more refuses.
	atSchemaBound := tinyTool("repo_read")
	atSchemaBound.Function.Parameters = paddedObject(t, api.MaxToolParametersBytes)
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{atSchemaBound},
	}).Validate(); err != nil {
		t.Fatalf("parameters schema at the %d-byte bound refused: %v", api.MaxToolParametersBytes, err)
	}
	overSchemaBound := tinyTool("repo_read")
	overSchemaBound.Function.Parameters = paddedObject(t, api.MaxToolParametersBytes+1)
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{overSchemaBound},
	}).Validate(); err == nil {
		t.Fatalf("parameters schema of %d bytes accepted; want refusal", api.MaxToolParametersBytes+1)
	}

	// Aggregate schemas: four 64 KiB schemas reach exactly 256 KiB; one more
	// byte refuses even though every schema is itself in bounds.
	aggregate := make([]api.ChatTool, 4)
	for index := range aggregate {
		aggregate[index] = tinyTool("tool_" + string(rune('a'+index)))
		aggregate[index].Function.Parameters = paddedObject(t, api.MaxToolParametersBytes)
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    aggregate,
	}).Validate(); err != nil {
		t.Fatalf("aggregate schema bytes at the %d-byte bound refused: %v", api.MaxToolSchemasBytes, err)
	}
	overAggregate := append(append([]api.ChatTool(nil), aggregate...), tinyTool("tool_overflow"))
	overAggregate[len(overAggregate)-1].Function.Parameters = json.RawMessage(`{"p":""}`)
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    overAggregate,
	}).Validate(); err == nil {
		t.Fatalf("aggregate schema bytes above %d accepted; want refusal", api.MaxToolSchemasBytes)
	}

	// Exactly one byte over the aggregate bound refuses. The schemas are
	// individually in bounds and their sizes sum to exactly MaxToolSchemasBytes
	// plus one, so a bound widened by even one byte is caught.
	oneOverAggregate := []api.ChatTool{tinyTool("tool_a"), tinyTool("tool_b"), tinyTool("tool_c"), tinyTool("tool_d"), tinyTool("tool_e")}
	for index, size := range []int{
		api.MaxToolParametersBytes,
		api.MaxToolParametersBytes,
		api.MaxToolParametersBytes,
		api.MaxToolParametersBytes - 7,
		8,
	} {
		oneOverAggregate[index].Function.Parameters = paddedObject(t, size)
	}
	total := 0
	for _, tool := range oneOverAggregate {
		total += len(tool.Function.Parameters)
	}
	if total != api.MaxToolSchemasBytes+1 {
		t.Fatalf("one-over aggregate fixture totals %d bytes; want %d", total, api.MaxToolSchemasBytes+1)
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    oneOverAggregate,
	}).Validate(); err == nil {
		t.Fatalf("aggregate schema bytes of exactly %d accepted; want refusal", api.MaxToolSchemasBytes+1)
	}

	// A tool declaration may only use the function type.
	nonFunction := tinyTool("repo_read")
	nonFunction.Type = "text"
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{nonFunction},
	}).Validate(); err == nil {
		t.Fatal("a non-function tool declaration was accepted")
	}

	// A parameters schema must be valid JSON, not merely a bracketed string.
	malformedSchema := tinyTool("repo_read")
	malformedSchema.Function.Parameters = json.RawMessage(`{not json`)
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{malformedSchema},
	}).Validate(); err == nil {
		t.Fatal("a malformed parameters schema was accepted")
	}

	// Tool calls per assistant message: exactly 32 pass, 33 refuse.
	atCallBound := make([]api.ChatToolCall, api.MaxToolCallsPerMessage)
	for index := range atCallBound {
		atCallBound[index] = toolCall("call-"+strings.Repeat("a", index+1), "repo_read", "{}")
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: []api.ChatMessage{{Role: "assistant", ContentNull: true, ToolCalls: atCallBound}},
	}).Validate(); err != nil {
		t.Fatalf("%d tool calls refused: %v", api.MaxToolCallsPerMessage, err)
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: []api.ChatMessage{{Role: "assistant", ContentNull: true, ToolCalls: append(append([]api.ChatToolCall(nil), atCallBound...), toolCall("call-extra", "repo_read", "{}"))}},
	}).Validate(); err == nil {
		t.Fatalf("%d tool calls were accepted; want refusal", api.MaxToolCallsPerMessage+1)
	}

	// Arguments string: exactly 64 KiB passes, one more refuses.
	atArgumentsBound := api.ChatCompletionRequest{
		Model: base.Model,
		Messages: []api.ChatMessage{{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{
			toolCall("call-1", "repo_read", paddedArguments(t, api.MaxToolArgumentsBytes)),
		}}},
	}
	if err := atArgumentsBound.Validate(); err != nil {
		t.Fatalf("arguments at the %d-byte bound refused: %v", api.MaxToolArgumentsBytes, err)
	}
	overArgumentsBound := api.ChatCompletionRequest{
		Model: base.Model,
		Messages: []api.ChatMessage{{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{
			toolCall("call-1", "repo_read", paddedArguments(t, api.MaxToolArgumentsBytes+1)),
		}}},
	}
	if err := overArgumentsBound.Validate(); err == nil {
		t.Fatalf("arguments of %d bytes accepted; want refusal", api.MaxToolArgumentsBytes+1)
	}

	// Bounds count bytes, not runes: 22000 three-byte runes are fewer than
	// 65536 runes but 66000 bytes, so the string must refuse.
	runeArguments := `{"p":"` + strings.Repeat("€", 22000) + `"}`
	if len(runeArguments) <= api.MaxToolArgumentsBytes {
		t.Fatalf("rune fixture is only %d bytes", len(runeArguments))
	}
	if err := (api.ChatCompletionRequest{
		Model: base.Model,
		Messages: []api.ChatMessage{{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{
			toolCall("call-1", "repo_read", runeArguments),
		}}},
	}).Validate(); err == nil {
		t.Fatal("rune-counted arguments were accepted; the bound is a byte count")
	}
	// The same byte-count rule applies to a parameters schema.
	runeSchema := tinyTool("repo_read")
	runeSchema.Function.Parameters = json.RawMessage(`{"p":"` + strings.Repeat("€", 22000) + `"}`)
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{runeSchema},
	}).Validate(); err == nil {
		t.Fatal("rune-counted parameters schema was accepted; the bound is a byte count")
	}

	// The message and body bounds apply as well.
	messages := make([]api.ChatMessage, api.MaxModelMessages)
	for index := range messages {
		messages[index] = api.ChatMessage{Role: "user", Content: "x"}
	}
	if err := (api.ChatCompletionRequest{Model: base.Model, Messages: messages}).Validate(); err != nil {
		t.Fatalf("%d messages refused: %v", api.MaxModelMessages, err)
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: append(append([]api.ChatMessage(nil), messages...), api.ChatMessage{Role: "user", Content: "x"}),
	}).Validate(); err == nil {
		t.Fatalf("%d messages were accepted; want refusal", api.MaxModelMessages+1)
	}
	oversizedBody := make([]byte, api.MaxModelBodyBytes+1)
	for index := range oversizedBody {
		oversizedBody[index] = ' '
	}
	if _, err := api.DecodeChatCompletionRequest(oversizedBody); err == nil {
		t.Fatalf("a model body above %d bytes was accepted", api.MaxModelBodyBytes)
	}
	bodyPrefix := `{"model":"fixture-model","messages":[{"role":"user","content":"`
	bodySuffix := `"}]}`
	atBodyBound := bodyPrefix + strings.Repeat("a", api.MaxModelBodyBytes-len(bodyPrefix)-len(bodySuffix)) + bodySuffix
	if len(atBodyBound) != api.MaxModelBodyBytes {
		t.Fatalf("body fixture is %d bytes; want %d", len(atBodyBound), api.MaxModelBodyBytes)
	}
	if _, err := api.DecodeChatCompletionRequest([]byte(atBodyBound)); err != nil {
		t.Fatalf("a model body at the %d-byte bound refused: %v", api.MaxModelBodyBytes, err)
	}
	overBodyBound := bodyPrefix + strings.Repeat("a", api.MaxModelBodyBytes-len(bodyPrefix)-len(bodySuffix)+1) + bodySuffix
	if _, err := api.DecodeChatCompletionRequest([]byte(overBodyBound)); err == nil {
		t.Fatalf("a model body of %d bytes accepted", api.MaxModelBodyBytes+1)
	}

	// Function names use the OpenAI-compatible 1-64 character grammar.
	for _, name := range []string{"", "repo.read", "repo/read", strings.Repeat("a", api.MaxToolNameBytes+1)} {
		tool := tinyTool(name)
		if err := (api.ChatCompletionRequest{Model: base.Model, Messages: base.Messages, Tools: []api.ChatTool{tool}}).Validate(); err == nil {
			t.Errorf("tool name %q was accepted; want refusal", name)
		}
	}
	if err := (api.ChatCompletionRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools:    []api.ChatTool{tinyTool(strings.Repeat("a", api.MaxToolNameBytes))},
	}).Validate(); err != nil {
		t.Fatalf("a %d-character tool name refused: %v", api.MaxToolNameBytes, err)
	}
}

// TestToolFieldsAreRoleSpecific pins the role-specific tool fields: tool calls
// only on assistant messages, tool_call_id only on tool messages, null content
// only on assistant messages, and a tool message requires its call identity.
func TestToolFieldsAreRoleSpecific(t *testing.T) {
	call := toolCall("call-1", "repo_read", "{}")

	cases := []struct {
		name    string
		message api.ChatMessage
		want    bool
	}{
		{"assistant null content with calls", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{call}}, true},
		{"assistant empty content", api.ChatMessage{Role: "assistant", Content: ""}, true},
		{"assistant tool_call_id", api.ChatMessage{Role: "assistant", Content: "x", ToolCallID: "call-1"}, false},
		{"user tool_calls", api.ChatMessage{Role: "user", Content: "x", ToolCalls: []api.ChatToolCall{call}}, false},
		{"system tool_calls", api.ChatMessage{Role: "system", Content: "x", ToolCalls: []api.ChatToolCall{call}}, false},
		{"user tool_call_id", api.ChatMessage{Role: "user", Content: "x", ToolCallID: "call-1"}, false},
		{"tool tool_calls", api.ChatMessage{Role: "tool", Content: "x", ToolCallID: "call-1", ToolCalls: []api.ChatToolCall{call}}, false},
		{"tool without identity", api.ChatMessage{Role: "tool", Content: "x"}, false},
		{"tool with identity", api.ChatMessage{Role: "tool", Content: "x", ToolCallID: "call-1"}, true},
		{"dangling tool_call_id", api.ChatMessage{Role: "tool", Content: "x", ToolCallID: "call-unmatched"}, true},
		{"user null content", api.ChatMessage{Role: "user", ContentNull: true}, false},
		{"system null content", api.ChatMessage{Role: "system", ContentNull: true}, false},
		{"tool null content", api.ChatMessage{Role: "tool", ContentNull: true, ToolCallID: "call-1"}, false},
		{"malformed call id", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call/1", "repo_read", "{}")}}, false},
		{"malformed call name", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call-1", "repo.read", "{}")}}, false},
		{"invalid arguments JSON", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call-1", "repo_read", `{not json`)}}, false},
		{"duplicate call ids", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call-1", "repo_read", "{}"), toolCall("call-1", "repo_write", "{}")}}, false},
		{"non-function call type", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{{ID: "call-1", Type: "text", Function: call.Function}}}, false},
		{"object arguments", api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call-1", "repo_read", `[]`)}}, false},
	}
	for _, testCase := range cases {
		err := (api.ChatCompletionRequest{Model: "fixture-model", Messages: []api.ChatMessage{testCase.message}}).Validate()
		if testCase.want && err != nil {
			t.Errorf("%s refused: %v", testCase.name, err)
		}
		if !testCase.want && err == nil {
			t.Errorf("%s accepted; want refusal", testCase.name)
		}
	}

	// The wire decoder refuses the same role violations, so a malformed
	// message cannot bypass validation through a custom decoder.
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"x","tool_calls":[]}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":"x","tool_call_id":"call-1"}]}`,
		`{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":null}]}`,
		// Request messages stay strict even though provider response messages
		// are lenient: an unknown member on the message or inside a call
		// refuses.
		`{"model":"m","messages":[{"role":"user","content":"x","refusal":null}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"repo_read","arguments":"{}"},"extra":1}]}]}`,
		// Duplicate call ids in one assistant message and duplicate tool names
		// in one request are ambiguous and refuse.
		`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"repo_read","arguments":"{}"}},{"id":"call-1","type":"function","function":{"name":"repo_write","arguments":"{}"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"repo_read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"repo_read","parameters":{"type":"object"}}}]}`,
	} {
		if _, err := api.DecodeChatCompletionRequest([]byte(body)); err == nil {
			t.Errorf("DecodeChatCompletionRequest(%s) accepted a role-invalid tool field", body)
		}
	}
}

// TestModelInvokeToolValidation pins that the model.invoke DTO and both decoded
// public inputs enforce the same tool rules: an invalid or oversized
// declaration refuses, duplicate tool names refuse, and a dangling
// tool_call_id stays accepted because correlation is the runtime's job.
func TestModelInvokeToolValidation(t *testing.T) {
	base := api.ModelInvokeRequest{
		ProjectID: "project-1",
		Model:     "fixture-model",
		Messages:  []api.ChatMessage{{Role: "user", Content: "x"}},
	}
	invalid := base
	invalid.Tools = []api.ChatTool{{
		Type:     "text",
		Function: api.ChatToolFunction{Name: "repo_read", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("ModelInvokeRequest accepted a non-function tool declaration")
	}

	oversized := base
	oversized.Tools = make([]api.ChatTool, api.MaxToolDeclarations+1)
	for index := range oversized.Tools {
		oversized.Tools[index] = tinyTool("tool_" + strings.Repeat("a", index+1))
	}
	if err := oversized.Validate(); err == nil {
		t.Fatalf("ModelInvokeRequest accepted %d tool declarations", api.MaxToolDeclarations+1)
	}

	duplicated := base
	duplicated.Tools = []api.ChatTool{tinyTool("repo_read"), tinyTool("repo_read")}
	if err := duplicated.Validate(); err == nil {
		t.Fatal("ModelInvokeRequest accepted duplicate tool names")
	}

	route, ok := api.LookupRoute("model.invoke")
	if !ok {
		t.Fatal("model.invoke is not a registered public route")
	}
	routeParams := map[string]string{"project": "project-1", "model": "fixture-model"}
	for name, request := range map[string]api.ModelInvokeRequest{
		"invalid":    invalid,
		"oversized":  oversized,
		"duplicated": duplicated,
	} {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal %s model request: %v", name, err)
		}
		if _, err := api.DecodeMCPOperationRequest("model.invoke", raw); err == nil {
			t.Errorf("DecodeMCPOperationRequest accepted the %s tool set", name)
		}
		if _, err := api.DecodeRouteRequest(route, raw, routeParams); err == nil {
			t.Errorf("DecodeRouteRequest accepted the %s tool set", name)
		}
	}

	// A tool_call_id that matches no earlier assistant call is accepted: the
	// subset refuses duplicate ids but leaves correlation to the runtime.
	dangling := api.ModelInvokeRequest{
		ProjectID: "project-1",
		Model:     "fixture-model",
		Messages: []api.ChatMessage{
			{Role: "assistant", ContentNull: true, ToolCalls: []api.ChatToolCall{toolCall("call-1", "repo_read", "{}")}},
			{Role: "tool", Content: "ok", ToolCallID: "call-unmatched"},
		},
	}
	if err := dangling.Validate(); err != nil {
		t.Fatalf("a dangling tool_call_id was refused: %v", err)
	}
}

// TestToolCallingWireVectorRoundTrip pins the golden wire vector for tool
// calling and the deterministic provider fixture shape.
func TestToolCallingWireVectorRoundTrip(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "wire", "model-tools.json"))
	if err != nil {
		t.Fatalf("read model tool vector: %v", err)
	}
	var vector struct {
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatalf("unmarshal model tool vector: %v", err)
	}
	request, err := api.DecodeChatCompletionRequest(vector.Request)
	if err != nil {
		t.Fatalf("decode model tool request vector: %v", err)
	}
	if len(request.Tools) == 0 || len(request.Messages) < 3 {
		t.Fatalf("model tool request vector = %+v", request)
	}
	nullable := false
	calls := false
	for _, message := range request.Messages {
		nullable = nullable || message.ContentNull
		calls = calls || len(message.ToolCalls) > 0 || message.ToolCallID != ""
	}
	if !nullable || !calls {
		t.Fatalf("model tool request vector lacks nullable content or call correlation: %+v", request)
	}
	var response api.ChatCompletionResponse
	if err := json.Unmarshal(vector.Response, &response); err != nil {
		t.Fatalf("decode model tool response vector: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("model tool response vector refused: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("re-marshal model tool request vector: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal re-marshaled model tool request: %v", err)
	}
	if _, ok := decoded["tools"]; !ok {
		t.Fatal("re-marshaled model tool request dropped tools")
	}
}
