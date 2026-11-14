package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
)

// OperationInputSchema returns the field-level MCP tools/list inputSchema of one
// registered operation. The schema is the shared request envelope
// (input/expected_version/idempotency_key) whose input member is projected from
// the same table entry the REST edge decodes with. Unknown operations, empty
// names and resource operations are refused with invalid_input.
func OperationInputSchema(operation string) (json.RawMessage, error) {
	if resolved, ok := MCPToolOperationName(operation); ok {
		operation = resolved
	}
	spec, ok := operationCodecSpecs[operation]
	if !ok {
		return nil, invalid("unknown operation %q", operation)
	}
	route, ok := LookupRoute(operation)
	if !ok {
		return nil, invalid("unknown operation %q", operation)
	}
	input := schemaForType(spec.inputType, make(map[reflect.Type]bool))
	if branches, ok := inputSelectionBranches[operation]; ok {
		oneOf := make([]any, 0, len(branches))
		for _, required := range branches {
			oneOf = append(oneOf, map[string]any{"required": []string{required}})
		}
		input["oneOf"] = oneOf
	}
	envelope := map[string]any{
		"type":                 "object",
		"title":                route.InputSchema,
		"additionalProperties": false,
		"properties": map[string]any{
			"input":            input,
			"expected_version": map[string]any{"type": "integer", "minimum": 1},
			"idempotency_key":  map[string]any{"type": "string", "maxLength": MaxIdempotencyKeyBytes},
		},
		"required": []string{"input"},
	}
	return json.Marshal(envelope)
}

// inputSelectionBranches advertises the exclusive input selections of an
// operation whose request requires exactly one of two members. Only
// pipeline.start selects a stored pipeline or inline YAML.
var inputSelectionBranches = map[string][]string{
	"pipeline.start": {"pipeline_id", "yaml"},
}

// schemaForType projects one Go type into a JSON Schema object. Nested declared
// structs become strict objects; opaque members (maps and raw JSON) are
// objects without further field inspection.
func schemaForType(typ reflect.Type, seen map[reflect.Type]bool) map[string]any {
	if typ == nil {
		return map[string]any{"type": "object"}
	}
	typ = indirectSchemaType(typ)
	if typ == nil {
		return map[string]any{"type": "object"}
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if typ == reflect.TypeOf(json.RawMessage(nil)) {
		return map[string]any{"type": "object"}
	}
	if typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.Uint8 {
		return map[string]any{"type": "string", "format": "byte"}
	}
	if seen[typ] {
		return map[string]any{"type": "object"}
	}
	switch typ.Kind() {
	case reflect.Struct:
		seen[typ] = true
		properties := make(map[string]any)
		required := make([]string, 0, typ.NumField())
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			if !field.IsExported() {
				continue
			}
			name, omitempty, ok := schemaJSONName(field)
			if !ok {
				continue
			}
			properties[name] = schemaForField(field, seen)
			if !omitempty {
				required = append(required, name)
			}
		}
		schema := map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           properties,
		}
		if len(required) > 0 {
			sort.Strings(required)
			schema["required"] = required
		}
		return schema
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaForType(typ.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	default:
		return map[string]any{"type": "object"}
	}
}

// schemaForField projects one struct field and applies its bound tag.
func schemaForField(field reflect.StructField, seen map[reflect.Type]bool) map[string]any {
	schema := schemaForType(field.Type, seen)
	switch field.Tag.Get("schema") {
	case "id":
		schema["minLength"] = 1
		schema["maxLength"] = maxIDLength
	case "yaml":
		schema["minLength"] = 1
		schema["maxLength"] = MaxInlineYAMLEncodedBytes
	case "secret":
		schema["minLength"] = 1
		schema["maxLength"] = MaxEventDataBytes
	case "token":
		schema["minLength"] = 1
		schema["maxLength"] = MaxCredentialBytes
	case "ttl", "version":
		schema["minimum"] = 1
	case "nullable":
		// The model subset's assistant content is the only nullable string.
		schema["type"] = []string{"string", "null"}
	case "messages":
		schema["maxItems"] = MaxModelMessages
	case "tools":
		schema["maxItems"] = MaxToolDeclarations
		schema["x-maxBytes"] = MaxToolSchemasBytes
	case "tool_calls":
		schema["maxItems"] = MaxToolCallsPerMessage
	case "tool_parameters":
		schema["x-maxBytes"] = MaxToolParametersBytes
	case "tool_arguments":
		schema["maxLength"] = MaxToolArgumentsBytes
	case "tool_name":
		schema["minLength"] = 1
		schema["maxLength"] = MaxToolNameBytes
		schema["pattern"] = "^[A-Za-z0-9_-]+$"
	}
	return schema
}

// indirectSchemaType strips pointer indirection.
func indirectSchemaType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

// schemaJSONName returns the wire name of a struct field and whether it is
// optional (omitempty) or excluded ("-").
func schemaJSONName(field reflect.StructField) (name string, omitempty bool, ok bool) {
	tag, present := field.Tag.Lookup("json")
	if !present {
		return field.Name, false, true
	}
	parts := strings.Split(tag, ",")
	if parts[0] == "-" {
		return "", false, false
	}
	name = parts[0]
	if name == "" {
		name = field.Name
	}
	for _, option := range parts[1:] {
		if option == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, true
}
