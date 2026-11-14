package publiccontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// inlineYAML is one small deterministic pipeline document.
var inlineYAML = []byte("pipeline: build\nsteps:\n  - run: make\n")

// encodeYAML returns the base64 standard encoding the public wire uses for a
// []byte member (Go's encoding/json representation).
func encodeYAML(t *testing.T, document []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(document)
}

// TestPipelineStartRequiresExactlyOneSelection pins the exclusive oneOf: exactly
// one of a stored pipeline id and inline YAML, never both and never neither.
func TestPipelineStartRequiresExactlyOneSelection(t *testing.T) {
	stored := `{"project_id":"prj-1","pipeline_id":"pipe-1","inputs":{"branch":"main"}}`
	inline := `{"project_id":"prj-1","yaml":"` + encodeYAML(t, inlineYAML) + `","inputs":{"branch":"main"}}`
	for name, raw := range map[string]string{"stored selection": stored, "inline yaml": inline} {
		if _, err := codecFor(t, "pipeline.start").DecodeRequest(json.RawMessage(raw)); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}

	both := `{"project_id":"prj-1","pipeline_id":"pipe-1","yaml":"` + encodeYAML(t, inlineYAML) + `"}`
	neither := `{"project_id":"prj-1","inputs":{"branch":"main"}}`
	for name, raw := range map[string]string{"both selections": both, "neither selection": neither} {
		_, err := codecFor(t, "pipeline.start").DecodeRequest(json.RawMessage(raw))
		requireInvalidInput(t, err, "pipeline.start with "+name)
	}

	// A malformed stored identity is refused.
	requireInvalidInput(t, (api.PipelineStartRequest{ProjectID: "prj-1", PipelineID: "bad/id"}).Validate(),
		"pipeline.start with a malformed pipeline id")
}

// TestPipelineStartInlineYAMLBoundAndExactBytes pins the 1 MiB decoded YAML
// bound (exactly at the bound, refused one byte more) and that the bytes are
// carried verbatim with no normalization.
func TestPipelineStartInlineYAMLBoundAndExactBytes(t *testing.T) {
	atBound := make([]byte, api.MaxInlineYAMLBytes)
	for index := range atBound {
		atBound[index] = 'a'
	}
	atBound[0] = 'p'
	if err := (api.PipelineStartRequest{YAML: atBound}).Validate(); err != nil {
		t.Fatalf("inline YAML of exactly %d bytes refused: %v", api.MaxInlineYAMLBytes, err)
	}
	overBound := append(append([]byte(nil), atBound...), 'a')
	requireInvalidInput(t, (api.PipelineStartRequest{YAML: overBound}).Validate(),
		"inline YAML one byte above the decoded bound")
	requireInvalidInput(t, (api.PipelineStartRequest{YAML: []byte{}}).Validate(),
		"pipeline.start with neither selection")

	// Byte-distinct but semantically equivalent documents stay distinct, and
	// the exact bytes survive a wire round trip unchanged.
	documents := [][]byte{
		[]byte("pipeline: build\n"),
		[]byte("pipeline: build"),
		[]byte("pipeline:  build\n"),
		[]byte("pipeline: build\r\n"),
	}
	codec := codecFor(t, "pipeline.start")
	for _, document := range documents {
		input, err := json.Marshal(api.PipelineStartRequest{YAML: document})
		if err != nil {
			t.Fatalf("marshal inline YAML input: %v", err)
		}
		value, err := codec.DecodeRequest(input)
		if err != nil {
			t.Fatalf("decode inline YAML input: %v", err)
		}
		decoded := value.(api.PipelineStartRequest)
		if !bytes.Equal(decoded.YAML, document) {
			t.Errorf("inline YAML round trip = %q; want the exact bytes %q", decoded.YAML, document)
		}
	}
}

// TestPipelineStartOneByteInlineSelection pins that inline presence is decided
// by a non-empty document, not by a size above one byte: a one-byte YAML
// document is a real selection (accepted alone, refused beside a stored
// pipeline id) and its idempotency content hashes exactly those bytes.
func TestPipelineStartOneByteInlineSelection(t *testing.T) {
	one := []byte("x")
	requireInvalidInput(t, (api.PipelineStartRequest{PipelineID: "pipe-1", YAML: one}).Validate(),
		"pipeline.start with a stored pipeline id and a one-byte inline document")
	if err := (api.PipelineStartRequest{YAML: one}).Validate(); err != nil {
		t.Fatalf("one-byte inline document refused: %v", err)
	}

	content, err := api.PipelineStartIdempotencyContent(api.PipelineStartRequest{YAML: one}, nil)
	if err != nil {
		t.Fatalf("one-byte inline idempotency content refused: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("inline idempotency content is not a JSON object: %v (%s)", err, content)
	}
	digest := sha256.Sum256(one)
	want := `"sha256:` + hex.EncodeToString(digest[:]) + `"`
	if got := string(decoded["yaml_sha256"]); got != want {
		t.Errorf("inline idempotency yaml_sha256 = %s; want %s", got, want)
	}
	if got := string(decoded["inputs"]); got != "{}" {
		t.Errorf("inline idempotency inputs = %s; want {}", got)
	}
	if len(decoded) != 2 {
		t.Errorf("inline idempotency content has members %v; want only yaml_sha256 and inputs", decoded)
	}
	for _, member := range []string{"pipeline_id", "pipeline_version"} {
		if _, ok := decoded[member]; ok {
			t.Errorf("inline idempotency content carries the stored-selection member %q: %s", member, content)
		}
	}
}

// TestPipelineStartEmptyYAMLIsAbsent pins the presence rule: a
// zero-length YAML member is not an inline document. A non-nil empty member
// beside a pipeline id is therefore the stored selection, while an empty member
// alone is refused as neither selection.
func TestPipelineStartEmptyYAMLIsAbsent(t *testing.T) {
	if err := (api.PipelineStartRequest{PipelineID: "pipe-1", YAML: []byte{}}).Validate(); err != nil {
		t.Fatalf("stored selection with an empty yaml member refused: %v", err)
	}
	if err := (api.PipelineStartRequest{PipelineID: "pipe-1", YAML: nil}).Validate(); err != nil {
		t.Fatalf("stored selection with an absent yaml member refused: %v", err)
	}
	requireInvalidInput(t, (api.PipelineStartRequest{YAML: []byte{}}).Validate(),
		"pipeline.start with an empty yaml member and no pipeline id")
}

// TestPipelineStartVersionOnlyAppliesToStoredSelection pins that the DTO has no
// pipeline_version member: the expected pipeline version travels only in the
// shared envelope expected_version, and a caller cannot smuggle a version into
// the inline YAML selection.
func TestPipelineStartVersionOnlyAppliesToStoredSelection(t *testing.T) {
	codec := codecFor(t, "pipeline.start")
	for _, member := range []string{
		`"pipeline_version":3`,
		`"yaml_sha256":"sha256:` + strings.Repeat("0", 64) + `"`,
		`"role":"operator"`,
	} {
		raw := `{"pipeline_id":"pipe-1",` + member + `}`
		if _, err := codec.DecodeRequest(json.RawMessage(raw)); err == nil {
			t.Errorf("pipeline.start accepted the undeclared member %s", member)
		}
	}
	// The same input is accepted once the expected version is carried by the
	// envelope, not by the DTO.
	version := uint64(3)
	request := api.Request{
		Input:           json.RawMessage(`{"pipeline_id":"pipe-1"}`),
		ExpectedVersion: &version,
		IdempotencyKey:  "start-1",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("envelope with the pipeline version refused: %v", err)
	}
}

// TestPipelineStartAlternateRouteInventory pins the second REST binding of
// pipeline.start: the canonical route table keeps its 58 rows, and
// exactly one alternate route exists with the same operation, MCP tool and
// schema names.
func TestPipelineStartAlternateRouteInventory(t *testing.T) {
	canonical := api.Routes()
	if len(canonical) != 58 {
		t.Fatalf("api.Routes() has %d rows; want the 58 canonical operations", len(canonical))
	}
	route, ok := api.LookupRoute("pipeline.start")
	if !ok {
		t.Fatal("LookupRoute refused pipeline.start")
	}
	if route.Method != api.MethodPost || route.Path != "/api/v1/projects/{project}/pipelines/{pipeline}/start" {
		t.Fatalf("canonical pipeline.start route = %s %s; want the existing start route", route.Method, route.Path)
	}

	alternates := api.AlternateRoutes()
	if len(alternates) != 1 {
		t.Fatalf("api.AlternateRoutes() has %d rows; want exactly one", len(alternates))
	}
	alternate := alternates[0]
	if alternate.Operation != "pipeline.start" ||
		alternate.Method != api.MethodPost ||
		alternate.Path != "/api/v1/projects/{project}/executions" {
		t.Fatalf("alternate route = %+v; want POST /api/v1/projects/{project}/executions for pipeline.start", alternate)
	}
	if alternate.MCPTool != "pipeline_start" {
		t.Errorf("alternate route MCP tool = %q; want pipeline_start", alternate.MCPTool)
	}
	if alternate.InputSchema != "pipeline.start.input.v1" || alternate.ResultSchema != "pipeline.start.result.v1" {
		t.Errorf("alternate route schemas = %q/%q; want pipeline.start.input.v1/result.v1",
			alternate.InputSchema, alternate.ResultSchema)
	}

	// The alternate is a second binding, not a second operation: one MCP tool
	// and one canonical route per operation remain.
	seen := make(map[string]bool)
	for _, row := range append(append([]api.Route(nil), canonical...), alternates...) {
		key := string(row.Method) + " " + row.Path
		if seen[key] {
			t.Errorf("duplicate method/path binding %s", key)
		}
		seen[key] = true
	}

	// The returned slice is a copy: mutating it cannot rewrite the table.
	alternates[0].Path = "/api/v1/tampered"
	if got := api.AlternateRoutes()[0].Path; got != "/api/v1/projects/{project}/executions" {
		t.Errorf("AlternateRoutes() exposed mutable state: path = %q", got)
	}
}

// TestPipelineStartAlternateRouteMatch pins concrete path matching for both
// bindings and the read route on the same path prefix.
func TestPipelineStartAlternateRouteMatch(t *testing.T) {
	route, params, ok := api.Match(api.MethodPost, "/api/v1/projects/prj-1/executions")
	if !ok || route.Operation != "pipeline.start" {
		t.Fatalf("Match(POST, /api/v1/projects/prj-1/executions) = %+v, %v; want pipeline.start", route, ok)
	}
	if !reflect.DeepEqual(params, map[string]string{"project": "prj-1"}) {
		t.Errorf("alternate route params = %v; want {project: prj-1}", params)
	}
	if route.Path != "/api/v1/projects/{project}/executions" {
		t.Errorf("matched alternate path = %q", route.Path)
	}

	route, params, ok = api.Match(api.MethodPost, "/api/v1/projects/prj-1/pipelines/pipe-1/start")
	if !ok || route.Operation != "pipeline.start" {
		t.Fatalf("Match(POST, canonical start) = %+v, %v; want pipeline.start", route, ok)
	}
	if !reflect.DeepEqual(params, map[string]string{"project": "prj-1", "pipeline": "pipe-1"}) {
		t.Errorf("canonical start params = %v; want project and pipeline", params)
	}

	route, params, ok = api.Match(api.MethodGet, "/api/v1/projects/prj-1/executions")
	if !ok || route.Operation != "execution.list" {
		t.Fatalf("Match(GET, /api/v1/projects/prj-1/executions) = %+v, %v; want execution.list", route, ok)
	}
	if !reflect.DeepEqual(params, map[string]string{"project": "prj-1"}) {
		t.Errorf("execution.list params = %v; want {project: prj-1}", params)
	}

	if _, _, ok := api.Match(api.MethodPost, "/api/v1/projects/prj-1/executions/prj-1"); ok {
		t.Error("Match accepted an invented executions path")
	}
}

// TestPipelineStartRouteAwareRESTDecode pins the route-aware REST decoder: each
// frozen binding fills exactly its own path parameters, a body value that
// conflicts with the path is refused, and the alternate route cannot decode
// an undeclared route.
func TestPipelineStartRouteAwareRESTDecode(t *testing.T) {
	alternate, alternateParams, ok := api.Match(api.MethodPost, "/api/v1/projects/prj-1/executions")
	if !ok {
		t.Fatal("Match did not resolve the alternate route")
	}
	canonical, canonicalParams, ok := api.Match(api.MethodPost, "/api/v1/projects/prj-1/pipelines/pipe-1/start")
	if !ok {
		t.Fatal("Match did not resolve the canonical route")
	}

	// The canonical route still fills the pipeline id from the path.
	value, err := api.DecodeRouteRequest(canonical, json.RawMessage(`{"inputs":{"branch":"main"}}`), canonicalParams)
	if err != nil {
		t.Fatalf("canonical REST decode: %v", err)
	}
	stored := value.(api.PipelineStartRequest)
	if stored.ProjectID != "prj-1" || stored.PipelineID != "pipe-1" {
		t.Errorf("canonical REST decode = %+v; want project/pipeline filled from the route", stored)
	}

	// The alternate route fills only the project and accepts inline YAML.
	inlineBody := json.RawMessage(`{"yaml":"` + encodeYAML(t, inlineYAML) + `","inputs":{"branch":"main"}}`)
	value, err = api.DecodeRouteRequest(alternate, inlineBody, alternateParams)
	if err != nil {
		t.Fatalf("alternate REST decode: %v", err)
	}
	inline := value.(api.PipelineStartRequest)
	if inline.ProjectID != "prj-1" || inline.PipelineID != "" || !bytes.Equal(inline.YAML, inlineYAML) {
		t.Errorf("alternate REST decode = %+v; want project only and the exact YAML", inline)
	}

	// A body project that disagrees with the path is refused on both routes.
	for name, decode := range map[string]struct {
		route  api.Route
		params map[string]string
		body   json.RawMessage
	}{
		"alternate": {alternate, alternateParams, json.RawMessage(`{"project_id":"prj-2","yaml":"` + encodeYAML(t, inlineYAML) + `"}`)},
		"canonical": {canonical, canonicalParams, json.RawMessage(`{"project_id":"prj-2","inputs":{}}`)},
	} {
		_, err := api.DecodeRouteRequest(decode.route, decode.body, decode.params)
		requireInvalidInput(t, err, name+" REST decode with a conflicting project")
	}

	// Both/neither selections are refused identically on the alternate route.
	for name, body := range map[string]json.RawMessage{
		"both":    json.RawMessage(`{"pipeline_id":"pipe-1","yaml":"` + encodeYAML(t, inlineYAML) + `"}`),
		"neither": json.RawMessage(`{"inputs":{}}`),
	} {
		_, err := api.DecodeRouteRequest(alternate, body, alternateParams)
		requireInvalidInput(t, err, "alternate REST decode with "+name)
	}

	// The canonical route supplies a pipeline id from the path, so an inline
	// YAML body is a second selection and is refused.
	_, err = api.DecodeRouteRequest(canonical,
		json.RawMessage(`{"yaml":"`+encodeYAML(t, inlineYAML)+`"}`), canonicalParams)
	requireInvalidInput(t, err, "canonical REST decode with inline YAML")

	// Missing route params and an unregistered operation are refused.
	_, err = api.DecodeRouteRequest(alternate, inlineBody, map[string]string{})
	requireInvalidInput(t, err, "alternate REST decode without the project param")
	_, err = api.DecodeRouteRequest(api.Route{Operation: "pipeline.unknown", Method: api.MethodPost, Path: "/api/v1/projects/{project}/executions"},
		inlineBody, alternateParams)
	requireInvalidInput(t, err, "REST decode of an unregistered operation")
	_, err = api.DecodeRouteRequest(api.Route{Operation: "pipeline.start", Method: api.MethodPost, Path: "/api/v1/projects/{project}/executions/{execution}"},
		inlineBody, map[string]string{"project": "prj-1", "execution": "exe-1"})
	requireInvalidInput(t, err, "REST decode of a route with an undeclared path parameter")
}

// TestPipelineStartMCPSelectionAndRouteParams pins that the MCP tool keeps one
// name and one schema, requires the project in the input, and does not
// require a pipeline id when the caller selects inline YAML.
func TestPipelineStartMCPSelectionAndRouteParams(t *testing.T) {
	codec := codecFor(t, "pipeline.start")
	mcpParams := make(map[string]string, len(codec.MCPRouteParams))
	for _, param := range codec.MCPRouteParams {
		mcpParams[param.Name] = param.Field
	}
	if !reflect.DeepEqual(mcpParams, map[string]string{"project": "project_id"}) {
		t.Fatalf("pipeline.start MCP route params = %v; want only the project", mcpParams)
	}
	restParams := make(map[string]string, len(codec.RouteParams))
	for _, param := range codec.RouteParams {
		restParams[param.Name] = param.Field
	}
	if !reflect.DeepEqual(restParams, map[string]string{"project": "project_id", "pipeline": "pipeline_id"}) {
		t.Fatalf("pipeline.start REST route params = %v; want project and pipeline", restParams)
	}

	// Inline YAML over MCP: project present, no pipeline id.
	inline := `{"project_id":"prj-1","yaml":"` + encodeYAML(t, inlineYAML) + `","inputs":{}}`
	value, err := api.DecodeMCPOperationRequest("pipeline.start", json.RawMessage(inline))
	if err != nil {
		t.Fatalf("MCP inline YAML start refused: %v", err)
	}
	if !bytes.Equal(value.(api.PipelineStartRequest).YAML, inlineYAML) {
		t.Errorf("MCP inline YAML start lost the exact bytes")
	}

	// Stored selection over MCP: project and pipeline id in the input.
	stored := `{"project_id":"prj-1","pipeline_id":"pipe-1","inputs":{}}`
	if _, err := api.DecodeMCPOperationRequest("pipeline.start", json.RawMessage(stored)); err != nil {
		t.Fatalf("MCP stored selection refused: %v", err)
	}

	for name, raw := range map[string]string{
		"without project": `{"yaml":"` + encodeYAML(t, inlineYAML) + `"}`,
		"both selections": `{"project_id":"prj-1","pipeline_id":"pipe-1","yaml":"` + encodeYAML(t, inlineYAML) + `"}`,
		"neither":         `{"project_id":"prj-1","inputs":{}}`,
	} {
		_, err := api.DecodeMCPOperationRequest("pipeline.start", json.RawMessage(raw))
		requireInvalidInput(t, err, "MCP pipeline.start "+name)
	}
}

// TestPipelineStartSchemaOneOf pins the advertised MCP input schema: the input
// object carries exactly two oneOf branches selecting pipeline_id or yaml, and
// the yaml member advertises the base64-encoded 1 MiB bound.
func TestPipelineStartSchemaOneOf(t *testing.T) {
	schema := inputSchemaObject(t, "pipeline.start")
	properties := schemaObject(t, schema["properties"], "pipeline.start schema properties")
	input := schemaObject(t, properties["input"], "pipeline.start input")
	oneOf, ok := input["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("pipeline.start input oneOf = %v; want exactly two branches", input["oneOf"])
	}
	branches := make([]string, 0, len(oneOf))
	for _, raw := range oneOf {
		branch := schemaObject(t, raw, "pipeline.start oneOf branch")
		required, ok := branch["required"].([]any)
		if !ok || len(required) != 1 {
			t.Fatalf("oneOf branch required = %v; want exactly one member", branch["required"])
		}
		name, ok := required[0].(string)
		if !ok {
			t.Fatalf("oneOf branch required member is %T; want a string", required[0])
		}
		branches = append(branches, name)
	}
	sort.Strings(branches)
	if !reflect.DeepEqual(branches, []string{"pipeline_id", "yaml"}) {
		t.Fatalf("pipeline.start oneOf selects %v; want pipeline_id or yaml", branches)
	}

	inputProperties := schemaObject(t, input["properties"], "pipeline.start input properties")
	yamlProperty := schemaObject(t, inputProperties["yaml"], "pipeline.start yaml property")
	if got := yamlProperty["type"]; got != "string" {
		t.Errorf("yaml schema type = %v; want string (base64)", got)
	}
	if got := yamlProperty["format"]; got != "byte" {
		t.Errorf("yaml schema format = %v; want byte", got)
	}
	if got, ok := yamlProperty["maxLength"].(float64); !ok || int(got) != api.MaxInlineYAMLEncodedBytes {
		t.Errorf("yaml schema maxLength = %v; want %d", yamlProperty["maxLength"], api.MaxInlineYAMLEncodedBytes)
	}

	// The oneOf is specific to the YAML-capable start operation.
	for _, operation := range []string{"pipeline.apply", "task.create"} {
		other := inputSchemaObject(t, operation)
		otherProperties := schemaObject(t, other["properties"], operation+" properties")
		otherInput := schemaObject(t, otherProperties["input"], operation+" input")
		if _, ok := otherInput["oneOf"]; ok {
			t.Errorf("%s input schema unexpectedly carries a oneOf", operation)
		}
	}
}

// TestPipelineStartResultCarriesExecutionIdentity pins that the result carries
// the admitted execution id and the EXECUTION version, never a pipeline
// identity or pipeline version.
func TestPipelineStartResultCarriesExecutionIdentity(t *testing.T) {
	codec := codecFor(t, "pipeline.start")
	valid := `{"execution_id":"exe-1","version":2}`
	value, err := codec.DecodeResult(json.RawMessage(valid))
	if err != nil {
		t.Fatalf("valid pipeline.start result refused: %v", err)
	}
	result := value.(api.PipelineStartResult)
	if result.ExecutionID != "exe-1" || result.Version != 2 {
		t.Errorf("pipeline.start result = %+v; want execution exe-1 version 2", result)
	}
	for name, raw := range map[string]string{
		"pipeline_id":       `{"execution_id":"exe-1","version":2,"pipeline_id":"pipe-1"}`,
		"pipeline_version":  `{"execution_id":"exe-1","version":2,"pipeline_version":2}`,
		"missing execution": `{"version":2}`,
		"missing version":   `{"execution_id":"exe-1"}`,
		"zero version":      `{"execution_id":"exe-1","version":0}`,
	} {
		_, err := codec.DecodeResult(json.RawMessage(raw))
		requireInvalidInput(t, err, "pipeline.start result with "+name)
	}
}

// TestPipelineStartIdempotencyContent pins the canonical receipt content: the
// selection (pipeline id plus version, or the exact YAML SHA-256) plus inputs.
// Two byte-distinct YAML documents are different content; the document itself
// is never part of the content.
func TestPipelineStartIdempotencyContent(t *testing.T) {
	version := uint64(3)
	stored := api.PipelineStartRequest{PipelineID: "pipe-1", Inputs: map[string]string{"branch": "main"}}
	content, err := api.PipelineStartIdempotencyContent(stored, &version)
	if err != nil {
		t.Fatalf("stored selection content: %v", err)
	}
	var storedContent map[string]any
	if err := json.Unmarshal(content, &storedContent); err != nil {
		t.Fatalf("stored content is not JSON: %v", err)
	}
	if storedContent["pipeline_id"] != "pipe-1" {
		t.Errorf("stored content pipeline_id = %v; want pipe-1", storedContent["pipeline_id"])
	}
	if storedContent["pipeline_version"] != float64(3) {
		t.Errorf("stored content pipeline_version = %v; want 3", storedContent["pipeline_version"])
	}
	if _, ok := storedContent["yaml_sha256"]; ok {
		t.Errorf("stored content carries yaml_sha256: %s", content)
	}
	inputs, ok := storedContent["inputs"].(map[string]any)
	if !ok || inputs["branch"] != "main" {
		t.Errorf("stored content inputs = %v; want {branch: main}", storedContent["inputs"])
	}

	// The same logical request produces byte-identical content; a different
	// pipeline version or an input change produces different content.
	same, err := api.PipelineStartIdempotencyContent(stored, &version)
	if err != nil {
		t.Fatalf("stored selection content repeat: %v", err)
	}
	if !bytes.Equal(content, same) {
		t.Errorf("pipeline.start content is not deterministic: %s then %s", content, same)
	}
	otherVersion := uint64(4)
	if changed, _ := api.PipelineStartIdempotencyContent(stored, &otherVersion); bytes.Equal(content, changed) {
		t.Error("a different expected pipeline version produced equal idempotency content")
	}
	changedInputs := api.PipelineStartRequest{PipelineID: "pipe-1", Inputs: map[string]string{"branch": "other"}}
	if changed, _ := api.PipelineStartIdempotencyContent(changedInputs, &version); bytes.Equal(content, changed) {
		t.Error("different inputs produced equal idempotency content")
	}

	// Inline YAML contributes only its SHA-256 plus inputs.
	inline := api.PipelineStartRequest{YAML: inlineYAML, Inputs: map[string]string{"branch": "main"}}
	inlineContent, err := api.PipelineStartIdempotencyContent(inline, nil)
	if err != nil {
		t.Fatalf("inline content: %v", err)
	}
	var inlineMap map[string]any
	if err := json.Unmarshal(inlineContent, &inlineMap); err != nil {
		t.Fatalf("inline content is not JSON: %v", err)
	}
	digest := sha256.Sum256(inlineYAML)
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if inlineMap["yaml_sha256"] != wantDigest {
		t.Errorf("inline content yaml_sha256 = %v; want %s", inlineMap["yaml_sha256"], wantDigest)
	}
	for _, forbidden := range []string{"pipeline_id", "pipeline_version"} {
		if _, ok := inlineMap[forbidden]; ok {
			t.Errorf("inline content carries %s: %s", forbidden, inlineContent)
		}
	}
	if bytes.Contains(inlineContent, inlineYAML) || bytes.Contains(inlineContent, []byte(encodeYAML(t, inlineYAML))) {
		t.Errorf("inline content embeds the YAML document or its base64: %s", inlineContent)
	}

	// Byte-distinct documents are different content even when semantically
	// equivalent.
	variant := api.PipelineStartRequest{YAML: append(append([]byte(nil), inlineYAML...), '\n'), Inputs: inline.Inputs}
	if variantContent, _ := api.PipelineStartIdempotencyContent(variant, nil); bytes.Equal(inlineContent, variantContent) {
		t.Error("a byte-distinct YAML document produced equal idempotency content")
	}

	// An absent input map equals an empty one.
	nilInputs := api.PipelineStartRequest{YAML: inlineYAML}
	emptyInputs := api.PipelineStartRequest{YAML: inlineYAML, Inputs: map[string]string{}}
	nilContent, err := api.PipelineStartIdempotencyContent(nilInputs, nil)
	if err != nil {
		t.Fatalf("inline content with nil inputs: %v", err)
	}
	emptyContent, err := api.PipelineStartIdempotencyContent(emptyInputs, nil)
	if err != nil {
		t.Fatalf("inline content with empty inputs: %v", err)
	}
	if !bytes.Equal(nilContent, emptyContent) {
		t.Errorf("nil and empty inputs produced different content: %s vs %s", nilContent, emptyContent)
	}

	// A version for inline YAML or no version for a stored selection is refused.
	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(inline, &version)),
		"inline YAML content with an expected version")
	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(stored, nil)),
		"stored selection content without an expected version")
	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(api.PipelineStartRequest{
		PipelineID: "pipe-1", YAML: inlineYAML,
	}, &version)), "content for both selections")
}

// contentError turns a (content, error) call into its error for assertion.
func contentError(_ []byte, err error) error {
	return err
}

// TestYAMLEnvelopeBounds pins the YAML envelope bound: YAML-carrying operations
// (pipeline.apply, pipeline.update and pipeline.start) accept a 2 MiB request
// envelope while every other operation keeps the default 1 MiB envelope. The
// decoded YAML itself stays bounded to 1 MiB.
func TestYAMLEnvelopeBounds(t *testing.T) {
	if api.MaxInlineYAMLBytes != 1<<20 {
		t.Fatalf("MaxInlineYAMLBytes = %d; want 1 MiB", api.MaxInlineYAMLBytes)
	}
	if api.MaxYAMLRequestBytes != 2<<20 {
		t.Fatalf("MaxYAMLRequestBytes = %d; want 2 MiB", api.MaxYAMLRequestBytes)
	}
	if api.MaxInlineYAMLEncodedBytes != (api.MaxInlineYAMLBytes+2)/3*4 {
		t.Fatalf("MaxInlineYAMLEncodedBytes = %d; want the base64 length of 1 MiB", api.MaxInlineYAMLEncodedBytes)
	}
	wantYAML := []string{"pipeline.apply", "pipeline.update", "pipeline.start"}
	if got := api.YAMLOperations(); !reflect.DeepEqual(got, wantYAML) {
		t.Fatalf("YAMLOperations() = %v; want %v", got, wantYAML)
	}
	for _, operation := range wantYAML {
		if got := api.MaxEnvelopeBytes(operation); got != api.MaxYAMLRequestBytes {
			t.Errorf("MaxEnvelopeBytes(%q) = %d; want %d", operation, got, api.MaxYAMLRequestBytes)
		}
	}
	for _, operation := range []string{"task.create", "execution.inspect", "pipeline.read", "pipeline.delete"} {
		if got := api.MaxEnvelopeBytes(operation); got != api.MaxRequestBytes {
			t.Errorf("MaxEnvelopeBytes(%q) = %d; want the unchanged %d", operation, got, api.MaxRequestBytes)
		}
	}
	if got := api.MaxEnvelopeBytes("task.unknown"); got != api.MaxRequestBytes {
		t.Errorf("MaxEnvelopeBytes(unknown) = %d; want the default %d", got, api.MaxRequestBytes)
	}

	// A 1 MiB document expands to about 1.4 MiB of base64: it does not fit
	// the default 1 MiB envelope but fits the YAML-operation envelope.
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	envelope := pipelineEnvelope(t, document)
	if len(envelope) <= api.MaxRequestBytes {
		t.Fatalf("test envelope is only %d bytes; the bound under test was not reached", len(envelope))
	}
	if len(envelope) > api.MaxYAMLRequestBytes {
		t.Fatalf("test envelope is %d bytes; it must fit the 2 MiB YAML bound", len(envelope))
	}
	if _, err := api.DecodeRequest(envelope); err == nil {
		t.Error("the default envelope decoder accepted a YAML-operation envelope above 1 MiB")
	}
	if _, err := api.DecodeOperationRequest("pipeline.start", envelope); err != nil {
		t.Fatalf("pipeline.start envelope refused: %v", err)
	}
	if _, err := api.DecodeOperationRequest("pipeline.apply", envelope); err != nil {
		t.Fatalf("pipeline.apply envelope refused: %v", err)
	}
	if _, err := api.DecodeOperationRequest("task.create", envelope); err == nil {
		t.Error("a non-YAML operation accepted an envelope above 1 MiB")
	}

	// A document whose base64 pushes the envelope past 2 MiB is refused by the
	// envelope decoder before any DTO validation.
	oversized := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes*3/2)
	oversizedEnvelope := pipelineEnvelope(t, oversized)
	if len(oversizedEnvelope) <= api.MaxYAMLRequestBytes {
		t.Fatalf("test envelope is %d bytes; want above %d", len(oversizedEnvelope), api.MaxYAMLRequestBytes)
	}
	_, err := api.DecodeOperationRequest("pipeline.start", oversizedEnvelope)
	requireInvalidInput(t, err, "pipeline.start envelope above the 2 MiB YAML bound")

	// A 1.2 MiB document fits the envelope but the DTO refuses the decoded
	// document one byte above the 1 MiB YAML bound.
	above := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes+1)
	aboveEnvelope := pipelineEnvelope(t, above)
	if len(aboveEnvelope) > api.MaxYAMLRequestBytes {
		t.Fatalf("1.2 MiB document envelope is %d bytes; it must fit 2 MiB", len(aboveEnvelope))
	}
	if _, err := api.DecodeOperationRequest("pipeline.start", aboveEnvelope); err != nil {
		t.Fatalf("pipeline.start envelope with a 1.2 MiB document refused: %v", err)
	}
	input := aboveEnvelopeInput(t, aboveEnvelope)
	_, err = api.DecodeMCPOperationRequest("pipeline.start", input)
	requireInvalidInput(t, err, "pipeline.start with a document above the decoded YAML bound")

	if _, err := api.DecodeOperationRequest("pipeline.unknown", envelope); err == nil {
		t.Error("DecodeOperationRequest accepted an unregistered operation")
	}
}

// pipelineEnvelope builds one minimal request envelope carrying inline YAML.
func pipelineEnvelope(t *testing.T, document []byte) []byte {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID: "prj-1",
		YAML:      document,
	})
	if err != nil {
		t.Fatalf("marshal inline YAML input: %v", err)
	}
	envelope, err := json.Marshal(api.Request{
		Input:          input,
		IdempotencyKey: "start-inline-1",
	})
	if err != nil {
		t.Fatalf("marshal request envelope: %v", err)
	}
	return envelope
}

// aboveEnvelopeInput extracts the input member of one decoded request envelope.
func aboveEnvelopeInput(t *testing.T, envelope []byte) json.RawMessage {
	t.Helper()
	request, err := api.DecodeOperationRequest("pipeline.start", envelope)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return request.Input
}

// mcpStartCall builds one MCP tools/call JSON-RPC body for pipeline.start.
func mcpStartCall(t *testing.T, document []byte) []byte {
	t.Helper()
	arguments, err := json.Marshal(api.Request{
		Input: mustMarshal(t, api.PipelineStartRequest{ProjectID: "prj-1", YAML: document}),
	})
	if err != nil {
		t.Fatalf("marshal MCP arguments: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  api.MCPMethodToolsCall,
		"params": api.MCPToolsCallParams{
			Name:      "pipeline.start",
			Arguments: arguments,
		},
	})
	if err != nil {
		t.Fatalf("marshal MCP body: %v", err)
	}
	return body
}

// mustMarshal encodes one deterministic JSON value.
func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return encoded
}

// TestYAMLEnvelopeBoundsMCP pins REST/MCP envelope parity: an MCP tools/call
// body for a YAML operation may reach the same 2 MiB envelope bound, a
// non-YAML operation keeps the 1 MiB default, and DecodeMCPRequest itself
// keeps the default bound.
func TestYAMLEnvelopeBoundsMCP(t *testing.T) {
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	body := mcpStartCall(t, document)
	if len(body) <= api.MaxRequestBytes {
		t.Fatalf("MCP body is only %d bytes; the bound under test was not reached", len(body))
	}
	if len(body) > api.MaxYAMLRequestBytes {
		t.Fatalf("MCP body is %d bytes; it must fit the 2 MiB YAML bound", len(body))
	}
	if _, err := api.DecodeMCPRequest(body); err == nil {
		t.Error("the default MCP decoder accepted a YAML-operation body above 1 MiB")
	}
	params, err := api.DecodeMCPToolsCall(body)
	if err != nil {
		t.Fatalf("MCP pipeline.start body refused: %v", err)
	}
	if params.Name != "pipeline.start" {
		t.Errorf("MCP call name = %q; want pipeline.start", params.Name)
	}

	// The same oversized body naming a non-YAML operation keeps the 1 MiB
	// default.
	taskArguments, err := json.Marshal(api.Request{
		Input: mustMarshal(t, api.TaskCreateRequest{Title: strings.Repeat("t", api.MaxInlineYAMLBytes)}),
	})
	if err != nil {
		t.Fatalf("marshal task.create arguments: %v", err)
	}
	taskBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  api.MCPMethodToolsCall,
		"params":  api.MCPToolsCallParams{Name: "task.create", Arguments: taskArguments},
	})
	if err != nil {
		t.Fatalf("marshal task.create MCP body: %v", err)
	}
	requireInvalidInput(t, decodeMCPCallError(api.DecodeMCPToolsCall(taskBody)),
		"MCP task.create body above the default envelope")

	// Above the YAML-operation bound the MCP call is refused as well.
	oversized := mcpStartCall(t, bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes*3/2))
	requireInvalidInput(t, decodeMCPCallError(api.DecodeMCPToolsCall(oversized)),
		"MCP pipeline.start body above the 2 MiB YAML bound")
}

// decodeMCPCallError turns a (params, error) call into its error.
func decodeMCPCallError(_ api.MCPToolsCallParams, err error) error {
	return err
}

// TestPipelineStartRESTAndMCPParity pins that the two edges decode the same
// logical start into the same canonical DTO (and therefore the same receipt
// content) and refuse the same invalid shapes with the same public code.
func TestPipelineStartRESTAndMCPParity(t *testing.T) {
	alternate, params, ok := api.Match(api.MethodPost, "/api/v1/projects/prj-1/executions")
	if !ok {
		t.Fatal("Match did not resolve the alternate route")
	}
	body := json.RawMessage(`{"yaml":"` + encodeYAML(t, inlineYAML) + `","inputs":{"branch":"main"}}`)
	restValue, err := api.DecodeRouteRequest(alternate, body, params)
	if err != nil {
		t.Fatalf("REST decode: %v", err)
	}
	mcpValue, err := api.DecodeMCPOperationRequest("pipeline.start",
		json.RawMessage(`{"project_id":"prj-1","yaml":"`+encodeYAML(t, inlineYAML)+`","inputs":{"branch":"main"}}`))
	if err != nil {
		t.Fatalf("MCP decode: %v", err)
	}
	rest := restValue.(api.PipelineStartRequest)
	mcp := mcpValue.(api.PipelineStartRequest)
	if !reflect.DeepEqual(rest, mcp) {
		t.Fatalf("REST and MCP decoded different starts:\nREST %+v\nMCP  %+v", rest, mcp)
	}
	restContent, err := api.PipelineStartIdempotencyContent(rest, nil)
	if err != nil {
		t.Fatalf("REST content: %v", err)
	}
	mcpContent, err := api.PipelineStartIdempotencyContent(mcp, nil)
	if err != nil {
		t.Fatalf("MCP content: %v", err)
	}
	if !bytes.Equal(restContent, mcpContent) {
		t.Errorf("REST and MCP content differ: %s vs %s", restContent, mcpContent)
	}

	// The same invalid shapes are refused with invalid_input on both edges.
	for name, raw := range map[string]json.RawMessage{
		"both":    json.RawMessage(`{"pipeline_id":"pipe-1","yaml":"` + encodeYAML(t, inlineYAML) + `"}`),
		"neither": json.RawMessage(`{"inputs":{}}`),
	} {
		_, restErr := api.DecodeRouteRequest(alternate, raw, params)
		requireInvalidInput(t, restErr, "REST "+name)
		_, mcpErr := api.DecodeMCPOperationRequest("pipeline.start",
			json.RawMessage(`{"project_id":"prj-1",`+string(raw[1:])))
		requireInvalidInput(t, mcpErr, "MCP "+name)
	}
}

// TestPipelineStartRegistryAuthority pins that operator, reviewer and viewer
// are refused by the operation registry, not by the public DTO: the
// registry row grants pipeline.start only to engineer humans and engineer
// services, and the DTO carries no role member.
func TestPipelineStartRegistryAuthority(t *testing.T) {
	operation, ok := authorize.LookupOperation("pipeline.start")
	if !ok {
		t.Fatal("the registry has no pipeline.start row")
	}
	var human []string
	for _, role := range operation.Requires.HumanRoles {
		human = append(human, string(role))
	}
	var service []string
	for _, role := range operation.Requires.ServiceRoles {
		service = append(service, string(role))
	}
	if !reflect.DeepEqual(human, []string{"engineer"}) {
		t.Errorf("pipeline.start human roles = %v; want [engineer]", human)
	}
	if !reflect.DeepEqual(service, []string{"engineer"}) {
		t.Errorf("pipeline.start service roles = %v; want [engineer]", service)
	}
	for _, denied := range []string{"operator", "reviewer", "viewer", "admin"} {
		for _, granted := range append(append([]string(nil), human...), service...) {
			if granted == denied {
				t.Errorf("pipeline.start grants the %s role; only engineer is granted", denied)
			}
		}
	}

	// The DTO itself never carries a role: the same well-formed request is
	// valid for every authenticated principal and authority is decided by the
	// registry row above.
	if err := (api.PipelineStartRequest{ProjectID: "prj-1", PipelineID: "pipe-1"}).Validate(); err != nil {
		t.Errorf("well-formed stored start refused by the DTO: %v", err)
	}
	if err := (api.PipelineStartRequest{ProjectID: "prj-1", YAML: inlineYAML}).Validate(); err != nil {
		t.Errorf("well-formed inline start refused by the DTO: %v", err)
	}
	if _, err := codecFor(t, "pipeline.start").DecodeRequest(
		json.RawMessage(`{"project_id":"prj-1","pipeline_id":"pipe-1","role":"operator"}`)); err == nil {
		t.Error("pipeline.start accepted a role member in the DTO")
	}
}

// inputSchemaObject decodes one operation's advertised MCP input schema.
func inputSchemaObject(t *testing.T, operation string) map[string]any {
	t.Helper()
	raw, err := api.OperationInputSchema(operation)
	if err != nil {
		t.Fatalf("OperationInputSchema(%q): %v", operation, err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("OperationInputSchema(%q) is not JSON: %v", operation, err)
	}
	return schemaObject(t, decoded, operation+" input schema")
}
