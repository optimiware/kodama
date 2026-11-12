// MCP schema projection: tools/list advertises a field-level JSON
// Schema for the shared request envelope of every exposed operation, derived
// from the same registry-keyed table that the REST edge decodes with.
package publiccontract

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// schemaObject asserts that value is a JSON object and returns it.
func schemaObject(t *testing.T, value any, what string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T; want a JSON object", what, value)
	}
	return object
}

// schemaMember returns one required object member.
func schemaMember(t *testing.T, object map[string]any, name, what string) any {
	t.Helper()
	value, ok := object[name]
	if !ok {
		t.Fatalf("%s has no %q member", what, name)
	}
	return value
}

// schemaString returns one required string member.
func schemaString(t *testing.T, object map[string]any, name, what string) string {
	t.Helper()
	value, ok := schemaMember(t, object, name, what).(string)
	if !ok {
		t.Fatalf("%s %q member is not a string", what, name)
	}
	return value
}

// schemaStringSet returns the sorted members of a required JSON array of
// strings.
func schemaStringSet(t *testing.T, value any, what string) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T; want a JSON array", what, value)
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains a non-string member", what)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// requireSameSet compares two sorted name lists. A missing list and an empty
// list are the same set.
func requireSameSet(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v; want %v", what, got, want)
		return
	}
	for index := range got {
		if got[index] != want[index] {
			t.Errorf("%s = %v; want %v", what, got, want)
			return
		}
	}
}

// optionalSchemaStringSet returns the sorted members of an optional required
// array, treating an absent member as empty.
func optionalSchemaStringSet(t *testing.T, object map[string]any, name, what string) []string {
	t.Helper()
	value, ok := object[name]
	if !ok {
		return nil
	}
	return schemaStringSet(t, value, what)
}

// inputSchemaFor decodes the frozen input schema of one operation and returns
// the schema object plus its typed input properties.
func inputSchemaFor(t *testing.T, operation string) (map[string]any, map[string]any) {
	t.Helper()
	raw, err := api.OperationInputSchema(operation)
	if err != nil {
		t.Fatalf("OperationInputSchema(%q): %v", operation, err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("OperationInputSchema(%q) is not valid JSON: %v", operation, err)
	}
	schema := schemaObject(t, decoded, operation+" input schema")
	properties := schemaObject(t, schemaMember(t, schema, "properties", operation+" input schema"), operation+" properties")
	input := schemaObject(t, schemaMember(t, properties, "input", operation+" properties"), operation+" input property")
	inputProperties := schemaObject(t, schemaMember(t, input, "properties", operation+" input"), operation+" input properties")
	return schema, inputProperties
}

// TestOperationInputSchemaIsFieldLevel pins that every one of the 51 exposed
// operations advertises a strict, field-level envelope schema that keeps the
// frozen title and the typed input member.
func TestOperationInputSchemaIsFieldLevel(t *testing.T) {
	routes := api.Routes()
	if len(routes) != 51 {
		t.Fatalf("api.Routes() has %d routes; want 51", len(routes))
	}
	for _, route := range routes {
		t.Run(route.Operation, func(t *testing.T) {
			schema, inputProperties := inputSchemaFor(t, route.Operation)

			if got := schemaString(t, schema, "type", route.Operation+" schema"); got != "object" {
				t.Errorf("%s schema type = %q; want object", route.Operation, got)
			}
			if got := schemaString(t, schema, "title", route.Operation+" schema"); got != route.InputSchema {
				t.Errorf("%s schema title = %q; want %q", route.Operation, got, route.InputSchema)
			}
			if got, ok := schemaMember(t, schema, "additionalProperties", route.Operation+" schema").(bool); !ok || got {
				t.Errorf("%s schema does not refuse unknown envelope members", route.Operation)
			}

			properties := schemaObject(t, schemaMember(t, schema, "properties", route.Operation+" schema"), route.Operation+" properties")
			for _, name := range []string{"input", "expected_version", "idempotency_key"} {
				if _, ok := properties[name]; !ok {
					t.Errorf("%s schema has no %q envelope member", route.Operation, name)
				}
			}
			idempotencyKey := schemaObject(t, schemaMember(t, properties, "idempotency_key", route.Operation+" properties"), route.Operation+" idempotency_key")
			if got, ok := schemaMember(t, idempotencyKey, "maxLength", route.Operation+" idempotency_key").(float64); !ok || int(got) != api.MaxIdempotencyKeyBytes {
				t.Errorf("%s idempotency_key maxLength = %v; want %d", route.Operation, idempotencyKey["maxLength"], api.MaxIdempotencyKeyBytes)
			}
			required := schemaStringSet(t, schemaMember(t, schema, "required", route.Operation+" schema"), route.Operation+" required")
			requireSameSet(t, required, []string{"input"}, route.Operation+" envelope required")

			input := schemaObject(t, schemaMember(t, properties, "input", route.Operation+" properties"), route.Operation+" input property")
			if got := schemaString(t, input, "type", route.Operation+" input"); got != "object" {
				t.Errorf("%s input schema type = %q; want object", route.Operation, got)
			}
			if got, ok := schemaMember(t, input, "additionalProperties", route.Operation+" input").(bool); !ok || got {
				t.Errorf("%s input schema does not refuse unknown input members", route.Operation)
			}
			if len(inputProperties) == 0 {
				t.Errorf("%s input schema is not field-level: it declares no properties", route.Operation)
			}
		})
	}
}

// TestOperationInputSchemaMatchesFrozenDTOs pins that the advertised input
// properties and required list are derived from the frozen request DTO and that
// a bounded identity is advertised as a 128-byte string.
func TestOperationInputSchemaMatchesFrozenDTOs(t *testing.T) {
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			requestType := reflect.TypeOf(testCase.request)
			_, inputProperties := inputSchemaFor(t, testCase.operation)

			declared := make([]string, 0, len(inputProperties))
			for name := range inputProperties {
				declared = append(declared, name)
			}
			sort.Strings(declared)
			requireSameSet(t, declared, wireFieldNames(requestType), testCase.operation+" input properties")

			raw, err := api.OperationInputSchema(testCase.operation)
			if err != nil {
				t.Fatalf("OperationInputSchema(%q): %v", testCase.operation, err)
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("OperationInputSchema(%q) is not valid JSON: %v", testCase.operation, err)
			}
			schema := schemaObject(t, decoded, testCase.operation+" input schema")
			properties := schemaObject(t, schemaMember(t, schema, "properties", testCase.operation+" schema"), testCase.operation+" properties")
			input := schemaObject(t, schemaMember(t, properties, "input", testCase.operation+" properties"), testCase.operation+" input")
			required := optionalSchemaStringSet(t, input, "required", testCase.operation+" input required")
			requireSameSet(t, required, requiredFieldNames(requestType), testCase.operation+" input required")

			for _, name := range testCase.identifiers {
				property := schemaObject(t, schemaMember(t, inputProperties, name, testCase.operation+" input properties"), testCase.operation+" "+name)
				if got := schemaString(t, property, "type", testCase.operation+" "+name); got != "string" {
					t.Errorf("%s identifier %s type = %q; want string", testCase.operation, name, got)
				}
				if got, ok := schemaMember(t, property, "maxLength", testCase.operation+" "+name).(float64); !ok || got != 128 {
					t.Errorf("%s identifier %s maxLength = %v; want 128", testCase.operation, name, got)
				}
				if got, ok := schemaMember(t, property, "minLength", testCase.operation+" "+name).(float64); !ok || got != 1 {
					t.Errorf("%s identifier %s minLength = %v; want 1", testCase.operation, name, got)
				}
			}
		})
	}
}

// TestOperationInputSchemaBoundsVersionFields pins that a version-tagged input
// field is advertised as a positive integer (minimum 1), matching the DTO's
// zero refusal, and that the action-only resolve does not advertise an
// execution-version body member.
func TestOperationInputSchemaBoundsVersionFields(t *testing.T) {
	_, inputProperties := inputSchemaFor(t, "execution.request.resolve")
	name := "expected_action_version"
	property := schemaObject(t, schemaMember(t, inputProperties, name, "execution.request.resolve "+name), "execution.request.resolve "+name)
	if got, ok := schemaMember(t, property, "minimum", "execution.request.resolve "+name).(float64); !ok || got != 1 {
		t.Errorf("execution.request.resolve %s minimum = %v; want 1", name, property["minimum"])
	}
	if _, present := inputProperties["expected_execution_version"]; present {
		t.Error("execution.request.resolve schema still advertises expected_execution_version")
	}
}

// schemaNested walks a chain of object member names and returns the final
// value. It fails the test when a member is missing.
func schemaNested(t *testing.T, value any, what string, members ...string) map[string]any {
	t.Helper()
	current := value
	for _, member := range members {
		object := schemaObject(t, current, what)
		current = schemaMember(t, object, member, what)
	}
	return schemaObject(t, current, what)
}

// schemaTypeSet returns the advertised type names of one schema.
func schemaTypeSet(t *testing.T, value any, what string) []string {
	t.Helper()
	object := schemaObject(t, value, what)
	switch advertised := object["type"].(type) {
	case string:
		return []string{advertised}
	case []any:
		names := make([]string, 0, len(advertised))
		for _, item := range advertised {
			name, ok := item.(string)
			if !ok {
				t.Fatalf("%s type contains a non-string member", what)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	default:
		t.Fatalf("%s type is %T; want a string or a string array", what, object["type"])
		return nil
	}
}

func schemaNumber(t *testing.T, value any, what string) int {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("%s is %T; want a number", what, value)
	}
	return int(number)
}

// TestModelSchemaAdvertisesToolCalling pins that the model.invoke input schema
// advertises the tool-calling vocabulary from the same constants the
// decoder enforces: nullable assistant content, string-valued call arguments,
// tool_call_id, and the tool/schema/call/message bounds. This kills a decoder
// and schema that drift apart.
func TestModelSchemaAdvertisesToolCalling(t *testing.T) {
	_, inputProperties := inputSchemaFor(t, "model.invoke")

	messages := schemaObject(t, schemaMember(t, inputProperties, "messages", "model.invoke messages"), "model.invoke messages")
	if got := schemaNumber(t, schemaMember(t, messages, "maxItems", "model.invoke messages"), "model.invoke messages maxItems"); got != api.MaxModelMessages {
		t.Errorf("model.invoke messages maxItems = %d; want %d", got, api.MaxModelMessages)
	}
	messageItems := schemaNested(t, messages, "model.invoke messages items", "items")
	messageProperties := schemaNested(t, messageItems, "model.invoke message properties", "properties")
	content := schemaObject(t, schemaMember(t, messageProperties, "content", "model.invoke message properties"), "model.invoke message content")
	types := schemaTypeSet(t, content, "model.invoke message content")
	requireSameSet(t, types, []string{"null", "string"}, "model.invoke message content types")

	toolCalls := schemaObject(t, schemaMember(t, messageProperties, "tool_calls", "model.invoke message properties"), "model.invoke tool_calls")
	if got := schemaNumber(t, schemaMember(t, toolCalls, "maxItems", "model.invoke tool_calls"), "model.invoke tool_calls maxItems"); got != api.MaxToolCallsPerMessage {
		t.Errorf("model.invoke tool_calls maxItems = %d; want %d", got, api.MaxToolCallsPerMessage)
	}
	toolCallFunction := schemaNested(t, toolCalls, "model.invoke tool call function", "items", "properties", "function", "properties")
	arguments := schemaObject(t, schemaMember(t, toolCallFunction, "arguments", "model.invoke tool call function"), "model.invoke tool call arguments")
	if got := schemaString(t, arguments, "type", "model.invoke tool call arguments"); got != "string" {
		t.Errorf("model.invoke call arguments type = %q; want string (arguments is a JSON string, never an object)", got)
	}
	if got := schemaNumber(t, schemaMember(t, arguments, "maxLength", "model.invoke tool call arguments"), "model.invoke tool call arguments maxLength"); got != api.MaxToolArgumentsBytes {
		t.Errorf("model.invoke call arguments maxLength = %d; want %d", got, api.MaxToolArgumentsBytes)
	}

	toolCallID := schemaObject(t, schemaMember(t, messageProperties, "tool_call_id", "model.invoke message properties"), "model.invoke tool_call_id")
	if got := schemaString(t, toolCallID, "type", "model.invoke tool_call_id"); got != "string" {
		t.Errorf("model.invoke tool_call_id type = %q; want string", got)
	}
	if got := schemaNumber(t, schemaMember(t, toolCallID, "maxLength", "model.invoke tool_call_id"), "model.invoke tool_call_id maxLength"); got != 128 {
		t.Errorf("model.invoke tool_call_id maxLength = %d; want 128", got)
	}

	tools := schemaObject(t, schemaMember(t, inputProperties, "tools", "model.invoke tools"), "model.invoke tools")
	if got := schemaNumber(t, schemaMember(t, tools, "maxItems", "model.invoke tools"), "model.invoke tools maxItems"); got != api.MaxToolDeclarations {
		t.Errorf("model.invoke tools maxItems = %d; want %d", got, api.MaxToolDeclarations)
	}
	if got := schemaNumber(t, schemaMember(t, tools, "x-maxBytes", "model.invoke tools"), "model.invoke tools x-maxBytes"); got != api.MaxToolSchemasBytes {
		t.Errorf("model.invoke tools x-maxBytes = %d; want %d", got, api.MaxToolSchemasBytes)
	}
	toolFunction := schemaNested(t, tools, "model.invoke tool function", "items", "properties", "function", "properties")
	name := schemaObject(t, schemaMember(t, toolFunction, "name", "model.invoke tool function"), "model.invoke tool name")
	if got := schemaNumber(t, schemaMember(t, name, "maxLength", "model.invoke tool name"), "model.invoke tool name maxLength"); got != api.MaxToolNameBytes {
		t.Errorf("model.invoke tool name maxLength = %d; want %d", got, api.MaxToolNameBytes)
	}
	parameters := schemaObject(t, schemaMember(t, toolFunction, "parameters", "model.invoke tool function"), "model.invoke tool parameters")
	if got := schemaString(t, parameters, "type", "model.invoke tool parameters"); got != "object" {
		t.Errorf("model.invoke tool parameters type = %q; want object", got)
	}
	if got := schemaNumber(t, schemaMember(t, parameters, "x-maxBytes", "model.invoke tool parameters"), "model.invoke tool parameters x-maxBytes"); got != api.MaxToolParametersBytes {
		t.Errorf("model.invoke tool parameters x-maxBytes = %d; want %d", got, api.MaxToolParametersBytes)
	}
}

// TestOperationInputSchemaRefusesUnknownOperations pins that the schema helper
// refuses an empty, unknown or resource-operation name.
func TestOperationInputSchemaRefusesUnknownOperations(t *testing.T) {
	for _, name := range []string{"", "task.unknown", "git.ref.update", "git.read", "artifact.register"} {
		if _, err := api.OperationInputSchema(name); err == nil {
			t.Errorf("OperationInputSchema(%q) was accepted; want refusal", name)
		} else {
			requirePublicError(t, err, api.CodeInvalidInput)
		}
	}
}

// TestOperationMCPToolsListCarriesFieldLevelSchema pins that the advertised
// tools/list catalog is exactly the 51 registry operations in order, each with
// a field-level input schema whose title is retained.
func TestOperationMCPToolsListCarriesFieldLevelSchema(t *testing.T) {
	var catalog api.MCPToolsListResult
	for _, route := range api.Routes() {
		schema, err := api.OperationInputSchema(route.Operation)
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", route.Operation, err)
		}
		catalog.Tools = append(catalog.Tools, api.MCPTool{Name: route.MCPTool, InputSchema: schema})
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal tools/list result: %v", err)
	}
	var decoded api.MCPToolsListResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal tools/list result: %v", err)
	}
	names := authorize.OperationNames()
	if len(decoded.Tools) != len(names) {
		t.Fatalf("tools/list advertises %d tools; want %d", len(decoded.Tools), len(names))
	}
	for index, tool := range decoded.Tools {
		want := string(names[index])
		if tool.Name != want {
			t.Fatalf("tools/list tool %d = %q; want %q in registry order", index, tool.Name, want)
		}
		var schema any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s inputSchema is not valid JSON: %v", tool.Name, err)
		}
		object := schemaObject(t, schema, tool.Name+" inputSchema")
		if got := schemaString(t, object, "title", tool.Name+" inputSchema"); got != want+".input.v1" {
			t.Errorf("%s inputSchema title = %q; want %q", tool.Name, got, want+".input.v1")
		}
		properties := schemaObject(t, schemaMember(t, object, "properties", tool.Name+" inputSchema"), tool.Name+" properties")
		input := schemaObject(t, schemaMember(t, properties, "input", tool.Name+" properties"), tool.Name+" input")
		if len(schemaObject(t, schemaMember(t, input, "properties", tool.Name+" input"), tool.Name+" input properties")) == 0 {
			t.Errorf("%s inputSchema is not field-level", tool.Name)
		}
	}
}

// TestOperationMCPToolsCallEnvelope pins that a tools/call can carry every
// frozen operation envelope, that the envelope is decoded strictly, and that
// the operation input is still decoded strictly by the registry table.
func TestOperationMCPToolsCallEnvelope(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]

			envelope := map[string]json.RawMessage{"input": vector.Request}
			if codec.RequiresIdempotencyKey {
				envelope["idempotency_key"] = json.RawMessage(`"mcp-key-1"`)
			}
			arguments, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			params, err := json.Marshal(api.MCPToolsCallParams{Name: testCase.operation, Arguments: arguments})
			if err != nil {
				t.Fatalf("marshal tools/call params: %v", err)
			}
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(params) + `}`
			request, err := api.DecodeMCPRequest([]byte(body))
			if err != nil {
				t.Fatalf("DecodeMCPRequest(%s): %v", body, err)
			}
			if err := request.Validate(); err != nil {
				t.Errorf("%s tools/call envelope refused: %v", testCase.operation, err)
			}

			// The arguments member is the shared envelope: an unknown member
			// outside `input` refuses.
			withUnknown := cloneObject(envelope)
			withUnknown["bogus_field"] = json.RawMessage(`true`)
			unknownArguments, err := json.Marshal(withUnknown)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			unknownParams, err := json.Marshal(api.MCPToolsCallParams{Name: testCase.operation, Arguments: unknownArguments})
			if err != nil {
				t.Fatalf("marshal tools/call params: %v", err)
			}
			unknownBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(unknownParams) + `}`
			if _, err := api.DecodeMCPRequest([]byte(unknownBody)); err == nil {
				t.Errorf("%s tools/call accepted an unknown envelope member", testCase.operation)
			}

			// The table remains the strict per-operation decoder used at
			// dispatch: an unknown input member refuses even though the MCP
			// envelope itself only checks that `input` is an object.
			if _, err := decodeRequestPayload(codec, json.RawMessage(`{"bogus_field":true}`)); err == nil {
				t.Errorf("%s table accepted an unknown input member", testCase.operation)
			}
		})
	}

	if !strings.Contains(string(mustSchema(t, "tenant.create")), `"tenant_id"`) {
		t.Error("tenant.create input schema does not expose tenant_id")
	}
}

// mustSchema returns the raw schema of one operation for the containment check.
func mustSchema(t *testing.T, operation string) json.RawMessage {
	t.Helper()
	raw, err := api.OperationInputSchema(operation)
	if err != nil {
		t.Fatalf("OperationInputSchema(%q): %v", operation, err)
	}
	return raw
}
