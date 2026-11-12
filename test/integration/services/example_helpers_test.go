//go:build integration

package services

// Example fixtures: validate every shipped and documented example against the
// schema catalog the served API actually publishes.
//
// The source of truth is the running factory composition's MCP tools/list
// response. internal/shell/transport/mcp builds that catalog from
// api.OperationInputSchema, the same registry-keyed table the REST edge decodes
// with, so a schema mutation in the product is visible here rather than being
// mirrored by a second hand-written expectation. Nothing in this file weakens a
// schema or rewrites an example: a disagreement is a defect in the example or in
// the served schema and the test must fail.
//
// Examples covered:
//   - every examples/pipelines/*.yaml and *.yml recipe (decode, apply, and
//     every `kodactl call <operation> --input=<json>` invocation);
//   - examples/testdata/invalid/*.yaml and *.yml (the served admission must
//     refuse them);
//   - the documented subject example in docs/specs/03-admission.md;
//   - the documented route table in docs/specs/08-operations-events.md;
//   - the shipped public request/response and capability testdata
//     (test/contracts/public/testdata/wire/{operations,model-tools,event}.json).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/test/support/factory"
)

// exampleRepoRoot resolves the repository root from this package directory.
func exampleRepoRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(working, "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %s has no go.mod: %v", root, err)
	}
	return root
}

// exampleRead reads one repository-relative example file.
func exampleRead(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(exampleRepoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read example %s: %v", rel, err)
	}
	return data
}

// exampleFiles returns the sorted file names with any of the given extensions
// in one repository-relative directory. Pipeline examples accept both .yaml and
// .yml exactly like the real `kodactl pipeline apply` inventory.
func exampleFiles(t *testing.T, rel string, extensions ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(exampleRepoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read example directory %s: %v", rel, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		for _, extension := range extensions {
			if strings.HasSuffix(entry.Name(), extension) {
				names = append(names, entry.Name())
				break
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("%s has no %v files", rel, extensions)
	}
	return names
}

// examplePipelineInventory is the exact shipped pipeline set, in both YAML
// spellings. A deletion is a missing entry and fails instead of silently
// shrinking what the served-schema and admission tests exercise.
var examplePipelineInventory = []string{
	"plain.yaml",
	"external-harness.yaml",
	"planned-review.yaml",
	"task-handoff.yaml",
	"service-response.yaml",
	"push-test.yaml",
	"build.yaml",
	"test.yaml",
	"promote.yaml",
	"rollback.yaml",
	"readiness.yaml",
}

// exampleInvalidFixtureInventory is the exact negative-fixture set.
var exampleInvalidFixtureInventory = []string{
	"unknown-field.yaml",
	"forward-reference.yaml",
	"undeclared-input-reference.yaml",
	"both-kinds.yaml",
	"push-glob-ref.yaml",
	"reserved-env.yaml",
	"reserved-secret.yaml",
	"interpolated-subject.yaml",
	"duplicate-action.yaml",
	"no-actions.yaml",
	"yaml-alias.yaml",
}

// exampleStem returns one example file name without its YAML extension.
func exampleStem(name string) string {
	for _, extension := range []string{".yaml", ".yml"} {
		if before, ok := strings.CutSuffix(name, extension); ok {
			return before
		}
	}
	return name
}

// exampleRequireInventory fails with the exact missing and unexpected files so
// a deleted example is visible to the services suite too.
func exampleRequireInventory(t *testing.T, rel string, got, want []string) {
	t.Helper()
	present := make(map[string]struct{}, len(got))
	for _, name := range got {
		present[name] = struct{}{}
	}
	expected := make(map[string]struct{}, len(want))
	for _, name := range want {
		expected[name] = struct{}{}
		if _, ok := present[name]; !ok {
			t.Fatalf("%s is missing the shipped example %s; a deletion is an inventory change", rel, name)
		}
	}
	for _, name := range got {
		if _, ok := expected[name]; !ok {
			t.Fatalf("%s has the unexpected example %s; add it to the inventory or remove the file", rel, name)
		}
	}
}

// examplePipelineFiles returns every shipped pipeline example, in both the
// .yaml and .yml spellings the CLI inventories.
func examplePipelineFiles(t *testing.T) []string {
	t.Helper()
	names := exampleFiles(t, "examples/pipelines", ".yaml", ".yml")
	exampleRequireInventory(t, "examples/pipelines", names, examplePipelineInventory)
	return names
}

// exampleInvalidFixtureFiles returns every shipped invalid-pipeline fixture.
func exampleInvalidFixtureFiles(t *testing.T) []string {
	t.Helper()
	names := exampleFiles(t, "examples/testdata/invalid", ".yaml", ".yml")
	exampleRequireInventory(t, "examples/testdata/invalid", names, exampleInvalidFixtureInventory)
	return names
}

// exampleDecodedPipeline decodes one shipped pipeline through the real
// admission parser. A decode failure is a fixture defect, never an expected red
// state.
func exampleDecodedPipeline(t *testing.T, rel string, data []byte) definition.Pipeline {
	t.Helper()
	decoded, err := pipeline.Decode(data, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("decode example pipeline %s: %v", rel, err)
	}
	return decoded
}

// examplePipelineCommands returns every container command of one decoded
// pipeline in declaration order.
func examplePipelineCommands(decoded definition.Pipeline) []string {
	commands := make([]string, 0)
	for _, action := range decoded.Actions {
		if action.Container == nil {
			continue
		}
		commands = append(commands, action.Container.Commands...)
	}
	return commands
}

// examplePublishedTools fetches the live MCP catalog from the running
// composition and pins it to the api-generated schema table.
func examplePublishedTools(t *testing.T, harness *factory.Harness) map[string]api.MCPTool {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": api.MCPJSONRPC,
		"id":      1,
		"method":  api.MCPMethodToolsList,
	})
	if err != nil {
		t.Fatalf("marshal tools/list request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, harness.BaseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build tools/list request: %v", err)
	}
	request.Header.Set("Content-Type", api.MediaType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("tools/list request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read tools/list response: %v", err)
	}
	var envelope api.MCPResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode tools/list response: %v; body=%s", err, string(data))
	}
	if envelope.Error != nil {
		t.Fatalf("tools/list refused: %d %s", envelope.Error.Code, envelope.Error.Message)
	}
	var listing api.MCPToolsListResult
	if err := json.Unmarshal(envelope.Result, &listing); err != nil {
		t.Fatalf("decode tools/list result: %v; result=%s", err, string(envelope.Result))
	}

	routes := api.Routes()
	if len(listing.Tools) != len(routes) {
		t.Fatalf("served tools/list has %d tools; want %d", len(listing.Tools), len(routes))
	}
	tools := make(map[string]api.MCPTool, len(listing.Tools))
	for index, tool := range listing.Tools {
		if tool.Name != routes[index].Operation {
			t.Fatalf("served tool %d = %q; want %q in the declared route order", index, tool.Name, routes[index].Operation)
		}
		want, err := api.OperationInputSchema(tool.Name)
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", tool.Name, err)
		}
		if !bytes.Equal(tool.InputSchema, want) {
			t.Fatalf("served inputSchema of %q differs from api.OperationInputSchema:\nserved=%s\napi=%s",
				tool.Name, string(tool.InputSchema), string(want))
		}
		tools[tool.Name] = tool
	}
	return tools
}

// exampleInvocation is one `kodactl call` invocation extracted from an example
// command together with the exact JSON input it submits.
type exampleInvocation struct {
	Operation string
	Input     json.RawMessage
}

// exampleCallPattern matches one `kodactl call <operation>` invocation. It is
// the same invocation grammar the examples themselves document.
var exampleCallPattern = regexp.MustCompile(`(?:^|[\s;&|(${])kodactl[ \t]+call[ \t]+([A-Za-z0-9_.-]+)`)

// exampleInvocations extracts every kodactl call and its JSON input from one
// decoded pipeline example. The accepted shell forms are the shipped ones:
// a literal JSON argument and `--input="$(printf '<json format>' ...)"`. Any
// other dynamic form fails instead of being skipped, so a new example cannot
// silently escape validation.
func exampleInvocations(t *testing.T, rel string, commands []string) []exampleInvocation {
	t.Helper()
	invocations := make([]exampleInvocation, 0, len(commands))
	for _, command := range commands {
		matches := exampleCallPattern.FindAllStringSubmatchIndex(command, -1)
		for index, match := range matches {
			operation := command[match[2]:match[3]]
			segmentEnd := len(command)
			if index+1 < len(matches) {
				segmentEnd = matches[index+1][0]
			}
			segment := command[match[3]:segmentEnd]
			if count := strings.Count(segment, "--input="); count > 1 {
				t.Fatalf("%s `kodactl call %s` declares %d --input arguments; want at most one", rel, operation, count)
			}
			var input json.RawMessage
			if strings.Contains(segment, "--input=") {
				input = exampleInputAfter(t, rel, command, match[3])
			} else {
				input = json.RawMessage(`{}`)
			}
			invocations = append(invocations, exampleInvocation{
				Operation: operation,
				Input:     input,
			})
		}
	}
	return invocations
}

// exampleInputAfter extracts the JSON input that follows the first --input= in
// one command segment.
func exampleInputAfter(t *testing.T, rel string, command string, from int) json.RawMessage {
	t.Helper()
	segment := command[from:]
	_, after, ok := strings.Cut(segment, "--input=")
	if !ok {
		t.Fatalf("%s has an unbalanced --input marker", rel)
	}
	value := after
	switch {
	case strings.HasPrefix(value, `"$(printf '`):
		return examplePrintfInput(t, rel, strings.TrimPrefix(value, `"$(printf '`))
	case strings.HasPrefix(value, `$(printf '`):
		return examplePrintfInput(t, rel, strings.TrimPrefix(value, `$(printf '`))
	case strings.HasPrefix(value, `"`):
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			t.Fatalf("%s has an unterminated quoted --input argument", rel)
		}
		return exampleJSONInput(t, rel, value[1:1+end])
	case strings.HasPrefix(value, "{"):
		depth := 0
		for offset, character := range value {
			switch character {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return exampleJSONInput(t, rel, value[:offset+1])
				}
			}
		}
		t.Fatalf("%s has an unterminated JSON --input argument", rel)
	default:
		t.Fatalf("%s uses an unsupported dynamic --input form %q; extend the example validator rather than skipping it", rel, value)
	}
	return nil
}

// examplePrintfInput renders the single-quoted printf format an example uses
// for one input, replacing every %s with a neutral placeholder. The format is
// the documented JSON shape; its shell arguments are runtime values.
func examplePrintfInput(t *testing.T, rel, content string) json.RawMessage {
	t.Helper()
	end := strings.IndexByte(content, '\'')
	if end < 0 {
		t.Fatalf("%s has an unterminated printf format", rel)
	}
	format := content[:end]
	rendered := strings.ReplaceAll(format, "%s", "placeholder")
	return exampleJSONInput(t, rel, rendered)
}

// exampleJSONInput validates one extracted JSON document and returns it.
func exampleJSONInput(t *testing.T, rel, text string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(text)) {
		t.Fatalf("%s carries an invalid JSON input: %q", rel, text)
	}
	return json.RawMessage(text)
}

// exampleInputSchema decodes one published tool schema and returns the inner
// request-input schema.
func exampleInputSchema(t *testing.T, tool api.MCPTool) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("%s inputSchema is not a JSON object: %v", tool.Name, err)
	}
	if title, _ := schema["title"].(string); title != tool.Name+".input.v1" {
		t.Fatalf("%s inputSchema title = %q; want %q", tool.Name, title, tool.Name+".input.v1")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s inputSchema has no properties object", tool.Name)
	}
	input, ok := properties["input"].(map[string]any)
	if !ok {
		t.Fatalf("%s inputSchema has no input object schema", tool.Name)
	}
	return input
}

// exampleValidateInput validates one example input against the served tool's
// published schema and the served MCP decoder. It never repairs the input.
func exampleValidateInput(t *testing.T, tool api.MCPTool, operation string, input json.RawMessage) {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("%s input is not one JSON object: %v; input=%s", operation, err, string(input))
	}
	violations := exampleSchemaViolations(exampleInputSchema(t, tool), value, operation+".input")
	if len(violations) != 0 {
		t.Fatalf("%s input drifts from the served schema:\n  %s\ninput=%s",
			operation, strings.Join(violations, "\n  "), string(input))
	}
	if _, err := api.DecodeMCPOperationRequest(operation, input); err != nil {
		t.Fatalf("%s input is refused by the served MCP decoder: %v; input=%s", operation, err, string(input))
	}
}

// exampleSchemaViolations reports every place a value disagrees with the JSON
// Schema vocabulary api.OperationInputSchema publishes. It is intentionally
// strict about unknown members, required members and declared bounds.
func exampleSchemaViolations(schema map[string]any, value any, path string) []string {
	violations := make([]string, 0)
	exampleCollectSchemaViolations(schema, value, path, &violations)
	return violations
}

// exampleCollectSchemaViolations walks one published schema node.
func exampleCollectSchemaViolations(schema map[string]any, value any, path string, violations *[]string) {
	if len(schema) == 0 {
		return
	}
	// A declared type is checked first; a mismatch makes nested checks
	// meaningless.
	if declared, ok := schema["type"]; ok {
		if !exampleTypeMatches(exampleSchemaTypeNames(declared), value) {
			*violations = append(*violations, fmt.Sprintf("%s = %s; want type %v", path, exampleWireType(value), exampleSchemaTypeNames(declared)))
			return
		}
	}
	// pipeline.start is the only oneOf: exactly one selection branch must hold.
	if branches, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, branch := range branches {
			branchSchema, _ := branch.(map[string]any)
			if exampleRequiredSatisfied(branchSchema, value) {
				matched++
			}
		}
		if matched != 1 {
			*violations = append(*violations, fmt.Sprintf("%s satisfies %d oneOf branches; want exactly one", path, matched))
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				member, ok := name.(string)
				if !ok {
					continue
				}
				if _, present := typed[member]; !present {
					*violations = append(*violations, fmt.Sprintf("%s is missing required member %q", path, member))
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			names := make([]string, 0, len(typed))
			for name := range typed {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, declared := properties[name]; !declared {
					*violations = append(*violations, fmt.Sprintf("%s has unknown member %q", path, name))
				}
			}
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			member, present := typed[name]
			if !present {
				continue
			}
			memberSchema, _ := properties[name].(map[string]any)
			exampleCollectSchemaViolations(memberSchema, member, path+"."+name, violations)
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for index, item := range typed {
				exampleCollectSchemaViolations(items, item, fmt.Sprintf("%s[%d]", path, index), violations)
			}
		}
		if maximum, ok := exampleSchemaNumber(schema["maxItems"]); ok && int64(len(typed)) > maximum {
			*violations = append(*violations, fmt.Sprintf("%s has %d items; at most %d are allowed", path, len(typed), maximum))
		}
	case string:
		if minimum, ok := exampleSchemaNumber(schema["minLength"]); ok && int64(len(typed)) < minimum {
			*violations = append(*violations, fmt.Sprintf("%s is %d bytes; at least %d are required", path, len(typed), minimum))
		}
		if maximum, ok := exampleSchemaNumber(schema["maxLength"]); ok && int64(len(typed)) > maximum {
			*violations = append(*violations, fmt.Sprintf("%s is %d bytes; at most %d are allowed", path, len(typed), maximum))
		}
		if pattern, ok := schema["pattern"].(string); ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				*violations = append(*violations, fmt.Sprintf("%s has an uncompilable pattern %q: %v", path, pattern, err))
			} else if !compiled.MatchString(typed) {
				*violations = append(*violations, fmt.Sprintf("%s = %q does not match %q", path, typed, pattern))
			}
		}
	case json.Number:
		if minimum, ok := exampleSchemaNumber(schema["minimum"]); ok {
			number, err := typed.Int64()
			if err != nil || number < minimum {
				*violations = append(*violations, fmt.Sprintf("%s = %s; minimum is %d", path, typed, minimum))
			}
		}
	}
}

// exampleWireType names one decoded JSON value's type for a failure message.
func exampleWireType(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// exampleSchemaNumber reads one non-negative schema bound.
func exampleSchemaNumber(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 {
		return 0, false
	}
	return int64(number), true
}

// exampleTypeMatches reports whether one decoded JSON value has a declared
// JSON Schema type.
func exampleTypeMatches(allowed []string, value any) bool {
	for _, candidate := range allowed {
		switch candidate {
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "integer":
			if number, ok := value.(json.Number); ok {
				if _, err := number.Int64(); err == nil {
					return true
				}
			}
		case "number":
			if number, ok := value.(json.Number); ok {
				if _, err := number.Float64(); err == nil {
					return true
				}
			}
		case "null":
			if value == nil {
				return true
			}
		}
	}
	return false
}

// exampleRequiredSatisfied reports whether an object carries every member one
// oneOf branch requires.
func exampleRequiredSatisfied(branch map[string]any, value any) bool {
	required, _ := branch["required"].([]any)
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, name := range required {
		text, ok := name.(string)
		if !ok {
			return false
		}
		if _, present := object[text]; !present {
			return false
		}
	}
	return true
}

// exampleSchemaTypeNames converts a decoded `type` member into its alternatives.
func exampleSchemaTypeNames(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		names := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
		return names
	default:
		return nil
	}
}

// exampleFencedBlock returns the body of the first fenced block of one language
// in a Markdown file.
func exampleFencedBlock(t *testing.T, rel, language string) string {
	t.Helper()
	inside := false
	body := make([]string, 0)
	for line := range strings.SplitSeq(string(exampleRead(t, rel)), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "```"); ok {
			if !inside {
				inside = strings.TrimSpace(after) == language
				continue
			}
			break
		}
		if inside {
			body = append(body, line)
		}
	}
	if !inside && len(body) == 0 {
		t.Fatalf("%s has no %s fenced block", rel, language)
	}
	return strings.Join(body, "\n")
}

// exampleRouteRow is one documented method/path example.
type exampleRouteRow struct {
	Method    string
	Path      string
	Operation string
}

// exampleRouteRowPattern matches one `| \`METHOD /path\` | ... |` table row.
var exampleRouteRowPattern = regexp.MustCompile("^\\| `([A-Z]+) ([^`]+)` \\| (.*)\\|$")

// exampleLeadingOperationPattern matches the operation name at the start of a
// route table's second cell.
var exampleLeadingOperationPattern = regexp.MustCompile("^`([a-z][a-z0-9_.]*)`")

// exampleDocumentedRoutes parses every documented method/path example from one
// Markdown file.
func exampleDocumentedRoutes(t *testing.T, rel string) []exampleRouteRow {
	t.Helper()
	rows := make([]exampleRouteRow, 0)
	for line := range strings.SplitSeq(string(exampleRead(t, rel)), "\n") {
		match := exampleRouteRowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		row := exampleRouteRow{
			Method: match[1],
			Path:   match[2],
		}
		if operation := exampleLeadingOperationPattern.FindStringSubmatch(match[3]); operation != nil {
			row.Operation = operation[1]
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		t.Fatalf("%s documents no method/path examples", rel)
	}
	return rows
}

// exampleCodec returns the public codec for one operation.
func exampleCodec(t *testing.T, operation string) api.OperationCodec {
	t.Helper()
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec
		}
	}
	t.Fatalf("api.OperationCodecs() has no codec for %q", operation)
	return api.OperationCodec{}
}

// exampleWireVector is one shipped request/result vector.
type exampleWireVector struct {
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
	Result    json.RawMessage `json:"result"`
}

// exampleLoadWireVectors reads test/contracts/public/testdata/wire/operations.json.
func exampleLoadWireVectors(t *testing.T) []exampleWireVector {
	t.Helper()
	var file struct {
		Operations []exampleWireVector `json:"operations"`
	}
	if err := json.Unmarshal(exampleRead(t, "test/contracts/public/testdata/wire/operations.json"), &file); err != nil {
		t.Fatalf("decode wire operation vectors: %v", err)
	}
	if len(file.Operations) == 0 {
		t.Fatal("wire operation vectors are empty")
	}
	return file.Operations
}
