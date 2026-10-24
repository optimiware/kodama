package apispec

// Client compatibility of the aggregate (AC-10, T-039).
//
// A Summary is the part of the public document a supported client depends on:
// each operation's identifier, parameters, request requirement and response
// statuses, and each schema's type, required members, member shapes and enums.
// A baseline Summary is committed with the tests (internal/app/testdata); the
// current document is compatible with it when everything the baseline declares
// is still there and no client that follows the baseline is asked for more:
//
//   - operations, parameters, response statuses, schemas and members are never
//     removed or retyped, and enums never lose a value;
//   - nothing becomes required of a caller: no new required parameter, a request
//     body that was optional stays optional, and no schema that can be sent in a
//     request body gains a required member;
//   - additions (operations, optional parameters, response members, schemas,
//     enum values, response statuses) are compatible.
//
// The check is about shape names and requirements, not full structural
// subsumption: it is the cheap, deterministic guard against the changes that
// break generated clients, kept next to byte-exact generation drift.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Summary is the compatibility-relevant shape of a public document.
type Summary struct {
	Operations map[string]OperationShape `json:"operations"`
	Schemas    map[string]SchemaShape    `json:"schemas"`
}

// OperationShape is one operation: key "METHOD /path".
type OperationShape struct {
	ID string `json:"id"`
	// Params is keyed "in:name".
	Params map[string]ParamShape `json:"params,omitempty"`
	// Body is "" (no body), "optional" or "required"; BodySchema names the schema.
	Body       string `json:"body,omitempty"`
	BodySchema string `json:"bodySchema,omitempty"`
	// Responses maps a status to the schema name of its JSON body ("" for none).
	Responses map[string]string `json:"responses,omitempty"`
}

// ParamShape is one parameter.
type ParamShape struct {
	Required bool   `json:"required,omitempty"`
	Shape    string `json:"shape"`
}

// SchemaShape is one component schema.
type SchemaShape struct {
	Type       string            `json:"type,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	Enum       []string          `json:"enum,omitempty"`
}

// Summarize reads an OpenAPI 3 document.
func Summarize(data []byte) (Summary, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Summary{}, err
	}
	s := Summary{Operations: map[string]OperationShape{}, Schemas: map[string]SchemaShape{}}
	comps, _ := doc["components"].(map[string]any)
	params, _ := comps["parameters"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	responses, _ := comps["responses"].(map[string]any)
	for name, def := range schemas {
		m, _ := def.(map[string]any)
		s.Schemas[name] = schemaShape(m)
	}
	resolveParam := func(v any) (string, ParamShape, bool) {
		m, _ := v.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			target, _ := params[refName(ref, "#/components/parameters/")].(map[string]any)
			m = target
		}
		if m == nil {
			return "", ParamShape{}, false
		}
		in, _ := m["in"].(string)
		name, _ := m["name"].(string)
		req, _ := m["required"].(bool)
		sch, _ := m["schema"].(map[string]any)
		return in + ":" + name, ParamShape{Required: req || in == "path", Shape: propShape(sch)}, true
	}
	paths, _ := doc["paths"].(map[string]any)
	for path, raw := range paths {
		item, _ := raw.(map[string]any)
		shared := map[string]ParamShape{}
		if list, ok := item["parameters"].([]any); ok {
			for _, p := range list {
				if k, shape, ok := resolveParam(p); ok {
					shared[k] = shape
				}
			}
		}
		for method, rawOp := range item {
			if !methods[method] {
				continue
			}
			op, _ := rawOp.(map[string]any)
			shape := OperationShape{Params: map[string]ParamShape{}, Responses: map[string]string{}}
			shape.ID, _ = op["operationId"].(string)
			for k, p := range shared {
				shape.Params[k] = p
			}
			if list, ok := op["parameters"].([]any); ok {
				for _, p := range list {
					if k, p, ok := resolveParam(p); ok {
						shape.Params[k] = p
					}
				}
			}
			if body, ok := op["requestBody"].(map[string]any); ok {
				shape.Body = "optional"
				if req, _ := body["required"].(bool); req {
					shape.Body = "required"
				}
				shape.BodySchema = jsonSchemaName(body)
			}
			rs, _ := op["responses"].(map[string]any)
			for status, r := range rs {
				rm, _ := r.(map[string]any)
				if ref, ok := rm["$ref"].(string); ok {
					rm, _ = responses[refName(ref, "#/components/responses/")].(map[string]any)
				}
				shape.Responses[status] = jsonSchemaName(rm)
			}
			s.Operations[strings.ToUpper(method)+" "+path] = shape
		}
	}
	return s, nil
}

func refName(ref, prefix string) string { return strings.TrimPrefix(ref, prefix) }

// jsonSchemaName is the component name (or inline type) of the JSON body of a
// request body or response object.
func jsonSchemaName(holder map[string]any) string {
	content, _ := holder["content"].(map[string]any)
	for ct, v := range content {
		if !strings.Contains(ct, "json") {
			continue
		}
		m, _ := v.(map[string]any)
		sch, _ := m["schema"].(map[string]any)
		return propShape(sch)
	}
	return ""
}

func propShape(s map[string]any) string {
	if s == nil {
		return ""
	}
	if ref, ok := s["$ref"].(string); ok {
		return "ref:" + refName(ref, "#/components/schemas/")
	}
	var b strings.Builder
	t, _ := s["type"].(string)
	b.WriteString(t)
	if f, _ := s["format"].(string); f != "" {
		b.WriteString("/" + f)
	}
	if n, _ := s["nullable"].(bool); n {
		b.WriteString("?")
	}
	if items, ok := s["items"].(map[string]any); ok {
		b.WriteString("[" + propShape(items) + "]")
	}
	if len(s) > 0 && t == "" {
		// allOf/oneOf/anyOf composition: keep the member names, not their bodies.
		var keys []string
		for _, k := range []string{"allOf", "oneOf", "anyOf"} {
			if list, ok := s[k].([]any); ok {
				var parts []string
				for _, e := range list {
					em, _ := e.(map[string]any)
					parts = append(parts, propShape(em))
				}
				sort.Strings(parts)
				keys = append(keys, k+"("+strings.Join(parts, ",")+")")
			}
		}
		b.WriteString(strings.Join(keys, ""))
	}
	return b.String()
}

func schemaShape(m map[string]any) SchemaShape {
	out := SchemaShape{Properties: map[string]string{}}
	out.Type, _ = m["type"].(string)
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
		sort.Strings(out.Required)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, p := range props {
			pm, _ := p.(map[string]any)
			out.Properties[name] = propShape(pm)
		}
	}
	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			out.Enum = append(out.Enum, fmt.Sprint(e))
		}
		sort.Strings(out.Enum)
	}
	if len(out.Properties) == 0 {
		out.Properties = nil
	}
	return out
}

// MarshalJSON-stable encoding for the committed baseline.
func (s Summary) JSON() ([]byte, error) { return json.MarshalIndent(s, "", "  ") }

// ParseSummary reads a committed baseline.
func ParseSummary(data []byte) (Summary, error) {
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return Summary{}, err
	}
	return s, nil
}

// inputSchemas lists the schemas a caller can send: those reachable from a
// request body of the current document.
func (cur Summary) inputSchemas(data []byte) map[string]bool {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				if ref, ok := c.(string); ok && k == "$ref" && strings.HasPrefix(ref, "#/components/schemas/") {
					name := refName(ref, "#/components/schemas/")
					if !seen[name] {
						seen[name] = true
						walk(schemas[name])
					}
					continue
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	paths, _ := doc["paths"].(map[string]any)
	for _, raw := range paths {
		item, _ := raw.(map[string]any)
		for method, rawOp := range item {
			if !methods[method] {
				continue
			}
			op, _ := rawOp.(map[string]any)
			if body, ok := op["requestBody"]; ok {
				walk(body)
			}
		}
	}
	return seen
}

// CompatibleWith reports every way the current document (data is its source,
// summarized as s) breaks a client written against the baseline. Empty means
// compatible.
func (s Summary) CompatibleWith(baseline Summary, data []byte) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	inputs := s.inputSchemas(data)

	keys := func(m map[string]OperationShape) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, key := range keys(baseline.Operations) {
		want := baseline.Operations[key]
		got, ok := s.Operations[key]
		if !ok {
			add("operation %s was removed", key)
			continue
		}
		if got.ID != want.ID {
			add("operation %s changed its identifier from %q to %q", key, want.ID, got.ID)
		}
		for pk, wp := range want.Params {
			gp, ok := got.Params[pk]
			switch {
			case !ok:
				add("operation %s lost parameter %s", key, pk)
			case gp.Shape != wp.Shape:
				add("operation %s parameter %s changed from %s to %s", key, pk, wp.Shape, gp.Shape)
			case gp.Required && !wp.Required:
				add("operation %s parameter %s became required", key, pk)
			}
		}
		for pk, gp := range got.Params {
			if _, known := want.Params[pk]; !known && gp.Required {
				add("operation %s gained the required parameter %s", key, pk)
			}
		}
		switch {
		case want.Body == "" && got.Body != "":
			if got.Body == "required" {
				add("operation %s now requires a request body", key)
			}
		case want.Body != "" && got.Body == "":
			add("operation %s no longer accepts a request body", key)
		case want.Body == "optional" && got.Body == "required":
			add("operation %s now requires its request body", key)
		case want.BodySchema != got.BodySchema:
			add("operation %s request body changed from %s to %s", key, want.BodySchema, got.BodySchema)
		}
		for status, ws := range want.Responses {
			gs, ok := got.Responses[status]
			switch {
			case !ok:
				add("operation %s no longer answers %s", key, status)
			case gs != ws:
				add("operation %s response %s changed from %q to %q", key, status, ws, gs)
			}
		}
	}

	names := make([]string, 0, len(baseline.Schemas))
	for n := range baseline.Schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		want := baseline.Schemas[name]
		got, ok := s.Schemas[name]
		if !ok {
			add("schema %s was removed", name)
			continue
		}
		if got.Type != want.Type {
			add("schema %s changed type from %q to %q", name, want.Type, got.Type)
		}
		for p, shape := range want.Properties {
			gs, ok := got.Properties[p]
			switch {
			case !ok:
				add("schema %s lost member %s", name, p)
			case gs != shape:
				add("schema %s member %s changed from %s to %s", name, p, shape, gs)
			}
		}
		for _, v := range want.Enum {
			if !contains(got.Enum, v) {
				add("schema %s lost the enum value %q", name, v)
			}
		}
		if inputs[name] {
			for _, r := range got.Required {
				if !contains(want.Required, r) {
					add("schema %s (sent by callers) gained the required member %s", name, r)
				}
			}
		}
		for _, r := range want.Required {
			if !contains(got.Required, r) && want.Type == "object" {
				// A relaxed requirement never breaks a sender; a reader that relied on
				// the member being present is broken.
				if !inputs[name] {
					add("schema %s no longer guarantees the member %s", name, r)
				}
			}
		}
	}
	return problems
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
