package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

// TestMCPToolCatalogEqualsRegistry pins the MCP tool catalog: exactly the 58
// registry operations in registry order, each advertising the sanitized
// underscore tool name, a non-empty description and the field-level
// api.OperationInputSchema of that operation byte for byte.
func TestMCPToolCatalogEqualsRegistry(t *testing.T) {
	catalog := mcp.ToolCatalog()
	operations := authorize.Operations()

	if len(catalog) != len(operations) {
		t.Fatalf("catalog tools = %d; want %d registry operations", len(catalog), len(operations))
	}
	for index, operation := range operations {
		tool := catalog[index]
		route, ok := api.LookupRoute(string(operation.Name))
		if !ok {
			t.Fatalf("operation %q has no declared route", operation.Name)
		}
		if tool.Name != route.MCPTool {
			t.Fatalf("catalog[%d] = %q; want %q", index, tool.Name, route.MCPTool)
		}
		if strings.Contains(tool.Name, ".") || len(tool.Name) == 0 || len(tool.Name) > 64 {
			t.Fatalf("catalog[%d] tool name %q violates ^[A-Za-z0-9_-]{1,64}$", index, tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("catalog[%d] %q has an empty description", index, tool.Name)
		}
		wantSchema, err := api.OperationInputSchema(string(operation.Name))
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", operation.Name, err)
		}
		if !bytes.Equal(tool.InputSchema, wantSchema) {
			t.Fatalf("tool %q input schema = %s; want the field-level %s",
				tool.Name, tool.InputSchema, wantSchema)
		}
		var schema struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %q input schema is not a JSON object: %v", tool.Name, err)
		}
		if schema.Type != "object" {
			t.Fatalf("tool %q input schema type = %q; want object", tool.Name, schema.Type)
		}
		if schema.Title != route.InputSchema {
			t.Fatalf("tool %q input schema title = %q; want %q", tool.Name, schema.Title, route.InputSchema)
		}
	}
}

// TestMCPToolCatalogAdvertisesFieldLevelSchemas pins every advertised schema to
// the declared field-level envelope: the shared envelope members, the retained
// title and the operation's own typed input fields from the registry-keyed
// codec table.
func TestMCPToolCatalogAdvertisesFieldLevelSchemas(t *testing.T) {
	catalog := mcp.ToolCatalog()
	routes := api.Routes()
	if len(catalog) != len(routes) {
		t.Fatalf("catalog tools = %d; want %d routes", len(catalog), len(routes))
	}

	codecs := make(map[string]api.OperationCodec, len(routes))
	for _, codec := range api.OperationCodecs() {
		codecs[codec.Operation] = codec
	}

	for index, route := range routes {
		t.Run(route.Operation, func(t *testing.T) {
			tool := catalog[index]
			if tool.Name != route.MCPTool {
				t.Fatalf("tool %d = %q; want the route's MCP tool %q", index, tool.Name, route.MCPTool)
			}

			schema := decodedObject(t, tool.InputSchema, tool.Name+" inputSchema")
			if got := schemaStringMember(t, schema, "type", tool.Name+" inputSchema"); got != "object" {
				t.Errorf("%s inputSchema type = %q; want object", tool.Name, got)
			}
			if got := schemaStringMember(t, schema, "title", tool.Name+" inputSchema"); got != route.InputSchema {
				t.Errorf("%s inputSchema title = %q; want %q", tool.Name, got, route.InputSchema)
			}
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("%s inputSchema does not refuse unknown envelope members", tool.Name)
			}

			properties := objectMember(t, schema, "properties", tool.Name+" inputSchema")
			for _, name := range []string{"input", "expected_version", "idempotency_key"} {
				if _, ok := properties[name]; !ok {
					t.Errorf("%s inputSchema has no %q envelope member", tool.Name, name)
				}
			}
			required := schemaStringSet(t, schema["required"], tool.Name+" inputSchema required")
			if !reflect.DeepEqual(required, []string{"input"}) {
				t.Errorf("%s inputSchema required = %v; want [input]", tool.Name, required)
			}

			input := objectMember(t, properties, "input", tool.Name+" envelope properties")
			if got := schemaStringMember(t, input, "type", tool.Name+" input"); got != "object" {
				t.Errorf("%s input schema type = %q; want object", tool.Name, got)
			}
			if got, ok := input["additionalProperties"].(bool); !ok || got {
				t.Errorf("%s input schema does not refuse unknown input members", tool.Name)
			}
			inputProperties := objectMember(t, input, "properties", tool.Name+" input")

			codec, ok := codecs[route.Operation]
			if !ok {
				t.Fatalf("%s has no registry codec", route.Operation)
			}
			declared := make([]string, 0, len(inputProperties))
			for name := range inputProperties {
				declared = append(declared, name)
			}
			sort.Strings(declared)
			want := wireFieldNames(codec.RequestType)
			if !reflect.DeepEqual(declared, want) {
				t.Errorf("%s input properties = %v; want the request DTO fields %v", tool.Name, declared, want)
			}
		})
	}

	// A route-parameter operation advertises its canonical request field, so an
	// MCP caller can supply what the REST route would have filled.
	index := routeIndex(t, "membership.list")
	input := objectMember(t,
		objectMember(t,
			objectMember(t, decodedObject(t, catalog[index].InputSchema, "membership.list inputSchema"), "properties", "membership.list inputSchema"),
			"input", "membership.list envelope properties"),
		"properties", "membership.list input")
	if _, ok := input["project_id"]; !ok {
		t.Fatalf("membership.list input schema has no project_id: %v", input)
	}
}

// TestMCPToolCatalogCopiesSchemaBytesPerCall pins that a consumer cannot mutate
// the shared catalog: every call returns independently owned schema bytes.
func TestMCPToolCatalogCopiesSchemaBytesPerCall(t *testing.T) {
	first := mcp.ToolCatalog()
	if len(first) < 2 {
		t.Fatalf("catalog tools = %d; want at least 2", len(first))
	}
	if len(first[0].InputSchema) == 0 || len(first[1].InputSchema) == 0 {
		t.Fatal("catalog advertises an empty input schema")
	}

	// Mutate the returned slice in place and replace another schema entirely.
	first[0].InputSchema[0] = 'x'
	first[1].InputSchema = json.RawMessage(`{"mutated":true}`)

	second := mcp.ToolCatalog()
	for _, index := range []int{0, 1} {
		want, err := api.OperationInputSchema(second[index].Name)
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", second[index].Name, err)
		}
		if !bytes.Equal(second[index].InputSchema, want) {
			t.Fatalf("catalog[%d] schema = %s after a caller mutation; want %s",
				index, second[index].InputSchema, want)
		}
	}
}

// TestMCPToolsListAdvertisesFieldLevelSchemas pins the edge result: tools/list
// carries the field-level api.OperationInputSchema of every route in order and
// never dispatches an operation.
func TestMCPToolsListAdvertisesFieldLevelSchemas(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	response := decodeResponse(t, edge.Handle(context.Background(), "", toolsListRequest()))
	if err := response.Validate(); err != nil {
		t.Fatalf("tools/list response is invalid: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("tools/list refused: %+v", response.Error)
	}
	var result api.MCPToolsListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}

	routes := api.Routes()
	if len(result.Tools) != len(routes) {
		t.Fatalf("tools/list tools = %d; want %d routes", len(result.Tools), len(routes))
	}
	for index, route := range routes {
		if result.Tools[index].Name != route.MCPTool {
			t.Fatalf("tools/list[%d] = %q; want %q", index, result.Tools[index].Name, route.MCPTool)
		}
		if strings.TrimSpace(result.Tools[index].Description) == "" {
			t.Fatalf("tools/list[%d] %q has no description", index, result.Tools[index].Name)
		}
		want, err := api.OperationInputSchema(route.Operation)
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", route.Operation, err)
		}
		if !bytes.Equal(result.Tools[index].InputSchema, want) {
			t.Fatalf("tools/list[%d] %s inputSchema = %s; want the field-level %s",
				index, route.Operation, result.Tools[index].InputSchema, want)
		}
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0 for tools/list", got)
	}
}

// TestMCPToolsListReturnsCatalog pins the tools/list result: exactly the
// catalog, with no router dispatch and no credential requirement.
func TestMCPToolsListReturnsCatalog(t *testing.T) {
	catalog := mcp.ToolCatalog()

	for _, body := range [][]byte{
		toolsListRequest(),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`),
	} {
		router := &fakeRouter{}
		edge := newEdge(t, router)
		response := decodeResponse(t, edge.Handle(context.Background(), "", body))
		if err := response.Validate(); err != nil {
			t.Fatalf("tools/list response is invalid: %v", err)
		}
		if response.Error != nil {
			t.Fatalf("tools/list refused: %+v", response.Error)
		}

		var result struct {
			Tools []mcp.Tool `json:"tools"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			t.Fatalf("decode tools/list result: %v", err)
		}
		if len(result.Tools) != len(catalog) {
			t.Fatalf("tools/list tools = %d; want %d", len(result.Tools), len(catalog))
		}
		for index := range catalog {
			if result.Tools[index].Name != catalog[index].Name {
				t.Fatalf("tools/list[%d] = %q; want %q", index, result.Tools[index].Name, catalog[index].Name)
			}
			if string(result.Tools[index].InputSchema) != string(catalog[index].InputSchema) {
				t.Fatalf("tools/list[%d] input schema = %s; want %s",
					index, result.Tools[index].InputSchema, catalog[index].InputSchema)
			}
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0 for tools/list", got)
		}
	}
}

// TestMCPToolCatalogIsStable pins that the catalog is a deterministic
// declaration: repeated calls return the same tool identities and schemas.
func TestMCPToolCatalogIsStable(t *testing.T) {
	first := mcp.ToolCatalog()
	second := mcp.ToolCatalog()
	if len(first) != len(second) {
		t.Fatalf("catalog lengths differ: %d and %d", len(first), len(second))
	}
	for index := range first {
		if first[index].Name != second[index].Name {
			t.Fatalf("catalog[%d] name = %q then %q", index, first[index].Name, second[index].Name)
		}
		if string(first[index].InputSchema) != string(second[index].InputSchema) {
			t.Fatalf("catalog[%d] schema is not stable", index)
		}
	}
}

// routeIndex returns the declaration index of one declared operation.
func routeIndex(t *testing.T, operation string) int {
	t.Helper()
	for index, route := range api.Routes() {
		if route.Operation == operation {
			return index
		}
	}
	t.Fatalf("operation %q has no declared route", operation)
	return -1
}

// decodedObject decodes raw JSON and requires a JSON object.
func decodedObject(t *testing.T, raw json.RawMessage, what string) map[string]any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T; want a JSON object", what, decoded)
	}
	return object
}

// objectMember returns one required JSON object member.
func objectMember(t *testing.T, object map[string]any, name, what string) map[string]any {
	t.Helper()
	value, ok := object[name]
	if !ok {
		t.Fatalf("%s has no %q member", what, name)
	}
	member, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s %q is %T; want a JSON object", what, name, value)
	}
	return member
}

// schemaStringMember returns one required string member.
func schemaStringMember(t *testing.T, object map[string]any, name, what string) string {
	t.Helper()
	value, ok := object[name]
	if !ok {
		t.Fatalf("%s has no %q member", what, name)
	}
	member, ok := value.(string)
	if !ok {
		t.Fatalf("%s %q is %T; want a string", what, name, value)
	}
	return member
}

// schemaStringSet returns the sorted members of a JSON string array.
func schemaStringSet(t *testing.T, value any, what string) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T; want a JSON array", what, value)
	}
	names := make([]string, len(items))
	for index, item := range items {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("%s[%d] is %T; want a string", what, index, item)
		}
		names[index] = name
	}
	sort.Strings(names)
	return names
}

// wireFieldNames returns the sorted top-level JSON names of a request DTO.
func wireFieldNames(typ reflect.Type) []string {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil
	}
	names := make([]string, 0, typ.NumField())
	for field := range typ.Fields() {
		if !field.IsExported() {
			continue
		}
		tag, present := field.Tag.Lookup("json")
		if !present {
			names = append(names, field.Name)
			continue
		}
		parts := strings.Split(tag, ",")
		if parts[0] == "-" {
			continue
		}
		if parts[0] == "" {
			names = append(names, field.Name)
			continue
		}
		names = append(names, parts[0])
	}
	sort.Strings(names)
	return names
}
