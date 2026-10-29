package mcpserver

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Param is one OpenAPI parameter exposed as a tool argument. API is the wire
// name; Arg is the stable snake_case tool argument.
type Param struct {
	API      string
	Arg      string
	In       string
	Required bool
	Schema   map[string]any
}

// Operation is one public API operation and the MCP tool that exposes it.
type Operation struct {
	ID          string
	Tool        string
	Toolset     string
	Method      string
	Path        string
	Summary     string
	Description string

	PathParams   []Param
	QueryParams  []Param
	HeaderParams []Param
	BodySchema   map[string]any
	BodyRequired bool

	InputSchema map[string]any

	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Sensitive   bool
	Streaming   bool
}

// BuildOperations parses the aggregate OpenAPI document and returns one
// operation per public route. The document is the committed public contract, so
// adding a route without adding its MCP tool fails the coverage test.
func BuildOperations(document []byte) ([]Operation, error) {
	var root map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(document))
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("mcpserver: parse OpenAPI: %w", err)
	}
	paths, _ := root["paths"].(map[string]any)
	if len(paths) == 0 {
		return nil, fmt.Errorf("mcpserver: OpenAPI has no paths")
	}
	pathNames := make([]string, 0, len(paths))
	for p := range paths {
		pathNames = append(pathNames, p)
	}
	sort.Strings(pathNames)

	var out []Operation
	for _, path := range pathNames {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		pathParams := parameters(root, item["parameters"])
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			raw, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := raw["operationId"].(string)
			if id == "" {
				return nil, fmt.Errorf("mcpserver: %s %s has no operationId", strings.ToUpper(method), path)
			}
			op, err := buildOperation(root, path, method, id, raw, pathParams)
			if err != nil {
				return nil, err
			}
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func buildOperation(root map[string]any, path, method, id string, raw map[string]any, pathParams []Param) (Operation, error) {
	op := Operation{
		ID: id, Tool: toolName(id), Toolset: toolset(path, id), Method: strings.ToUpper(method), Path: path,
		Summary: stringValue(raw["summary"]), Description: stringValue(raw["description"]),
		ReadOnly: method == "get", Idempotent: method == "get" || method == "delete" || method == "put",
		Destructive: method == "delete" || containsAny(id, "cancel", "cleanup", "revoke", "delete"),
		Sensitive:   id == "createSecret" || id == "createSecretVersion",
		Streaming:   id == "observeProjectEvents" || id == "observeRunEvents" || id == "getRunLogs",
	}
	if id == "replayChatopsDelivery" {
		op.Idempotent = true
	}

	params := append([]Param(nil), parameters(root, raw["parameters"])...)
	params = append(params, pathParams...)
	seen := map[string]bool{}
	for _, p := range params {
		key := p.In + "\x00" + p.API
		if seen[key] {
			continue
		}
		seen[key] = true
		switch p.In {
		case "path":
			op.PathParams = append(op.PathParams, p)
		case "query":
			op.QueryParams = append(op.QueryParams, p)
		case "header":
			if p.API == "If-Match" || p.API == "Idempotency-Key" || p.API == "Last-Event-ID" {
				op.HeaderParams = append(op.HeaderParams, p)
			}
		}
	}

	body, required, err := requestBody(root, raw["requestBody"])
	if err != nil {
		return Operation{}, fmt.Errorf("mcpserver: %s: %w", id, err)
	}
	op.BodySchema, op.BodyRequired = body, required
	op.InputSchema = inputSchema(op)
	return op, nil
}

func parameters(root map[string]any, raw any) []Param {
	list, _ := raw.([]any)
	out := make([]Param, 0, len(list))
	for _, entry := range list {
		m := asMap(resolve(root, entry))
		if m == nil {
			continue
		}
		in, _ := m["in"].(string)
		name, _ := m["name"].(string)
		if in == "" || name == "" {
			continue
		}
		schema := asMap(resolve(root, m["schema"]))
		if schema == nil {
			schema = map[string]any{"type": "string"}
		}
		required, _ := m["required"].(bool)
		if in == "path" {
			required = true
		}
		out = append(out, Param{
			API: name, Arg: snake(name), In: in, Required: required,
			Schema: cleanSchema(root, schema),
		})
	}
	return out
}

func requestBody(root map[string]any, raw any) (map[string]any, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	m := asMap(resolve(root, raw))
	if m == nil {
		return nil, false, nil
	}
	required, _ := m["required"].(bool)
	content := asMap(resolve(root, m["content"]))
	if content == nil {
		return map[string]any{"type": "object"}, required, nil
	}
	jsonBody := asMap(resolve(root, content["application/json"]))
	if jsonBody == nil {
		// A few routes use another JSON-compatible media type; take the first
		// object-shaped schema rather than inventing one.
		for _, v := range content {
			if candidate := asMap(resolve(root, v)); candidate != nil {
				if schema := asMap(resolve(root, candidate["schema"])); schema != nil {
					return cleanSchema(root, schema), required, nil
				}
			}
		}
		return map[string]any{"type": "object"}, required, nil
	}
	schema := asMap(resolve(root, jsonBody["schema"]))
	if schema == nil {
		return map[string]any{"type": "object"}, required, nil
	}
	return cleanSchema(root, schema), required, nil
}

func inputSchema(op Operation) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0)
	add := func(p Param) {
		schema := cloneMap(p.Schema)
		if p.Required {
			required = append(required, p.Arg)
		}
		properties[p.Arg] = schema
	}
	for _, p := range op.PathParams {
		add(p)
	}
	for _, p := range op.QueryParams {
		add(p)
	}
	for _, p := range op.HeaderParams {
		add(p)
	}
	if op.BodySchema != nil {
		body := cloneMap(op.BodySchema)
		if _, ok := body["type"]; !ok {
			body["type"] = "object"
		}
		properties["body"] = body
		if op.BodyRequired {
			required = append(required, "body")
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func cleanSchema(root map[string]any, schema map[string]any) map[string]any {
	out := cloneMap(schema)
	for _, drop := range []string{"nullable", "example", "examples", "xml", "discriminator", "externalDocs", "deprecated", "readOnly", "writeOnly"} {
		delete(out, drop)
	}
	if nullable, _ := schema["nullable"].(bool); nullable {
		switch t := out["type"].(type) {
		case string:
			out["type"] = []any{t, "null"}
		case []any:
			out["type"] = append(t, "null")
		}
	}
	for _, key := range []string{"items", "additionalProperties", "not"} {
		if child := asMap(out[key]); child != nil {
			out[key] = cleanSchema(root, child)
		}
	}
	for _, key := range []string{"properties"} {
		if children := asMap(out[key]); children != nil {
			next := map[string]any{}
			for name, child := range children {
				if c := asMap(child); c != nil {
					next[name] = cleanSchema(root, c)
				}
			}
			out[key] = next
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if list, ok := out[key].([]any); ok {
			next := make([]any, 0, len(list))
			for _, child := range list {
				if c := asMap(child); c != nil {
					next = append(next, cleanSchema(root, c))
				}
			}
			out[key] = next
		}
	}
	return out
}

func resolve(root map[string]any, value any) any {
	return resolveDepth(root, value, 0)
}

func resolveDepth(root map[string]any, value any, depth int) any {
	if depth > 32 {
		return value
	}
	m, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if ref, _ := m["$ref"].(string); ref != "" {
		target := lookup(root, ref)
		if target == nil {
			return value
		}
		resolved := resolveDepth(root, target, depth+1)
		base, _ := resolved.(map[string]any)
		out := cloneMap(base)
		for k, v := range m {
			if k != "$ref" {
				out[k] = v
			}
		}
		return resolveDepth(root, out, depth+1)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = resolveDepth(root, v, depth+1)
	}
	return out
}

func lookup(root map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var current any = root
	for _, raw := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func toolName(id string) string { return "kodama_" + snake(id) }

func snake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		switch {
		case r == '-' || r == ' ':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
		case r >= 'A' && r <= 'Z':
			prevLower := i > 0 && (runes[i-1] >= 'a' && runes[i-1] <= 'z' || runes[i-1] >= '0' && runes[i-1] <= '9')
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			prevUpper := i > 0 && runes[i-1] >= 'A' && runes[i-1] <= 'Z'
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") && (prevLower || prevUpper && nextLower) {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "_")
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(strings.ToLower(s), p) {
			return true
		}
	}
	return false
}

func toolset(path, id string) string {
	switch {
	case strings.Contains(path, "/work-items"):
		return "work"
	case strings.Contains(path, "/workspaces") || strings.Contains(path, "/workspace"):
		return "workspace"
	case strings.Contains(path, "/pipeline-definitions") || strings.Contains(path, "/agent-profiles"):
		return "pipeline"
	case strings.Contains(path, "/runs"):
		return "runs"
	case strings.Contains(path, "/gates"):
		return "gates"
	case strings.Contains(path, "/environments") || strings.Contains(path, "/deployments"):
		return "delivery"
	case strings.Contains(path, "/variables") || strings.Contains(path, "/secrets"):
		return "configuration"
	case strings.Contains(path, "/artifacts"):
		return "artifacts"
	case strings.Contains(path, "/model-settings") || strings.Contains(path, "/mcp-servers"):
		return "model"
	case strings.Contains(path, "/repositories") || strings.Contains(path, "/git-credentials"):
		return "repo"
	case strings.Contains(path, "/chatops"):
		return "chatops"
	case path == "/me" || strings.Contains(path, "/tenants") || strings.Contains(path, "/projects") || strings.Contains(path, "/memberships"):
		return "identity"
	default:
		return "other"
	}
}
