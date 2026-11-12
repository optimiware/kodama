// Envelope size bounds, typed validation and registered-binding checks of the
// operation and MCP decoders. The table tests sit exactly on each accept/refuse
// boundary and never start a process or perform an effect.

package publiccontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// padJSON pads one JSON document with insignificant trailing whitespace to
// exactly size bytes so a test can sit exactly on a size bound.
func padJSON(t *testing.T, document []byte, size int) []byte {
	t.Helper()
	if len(document) > size {
		t.Fatalf("test document is %d bytes; cannot pad it down to %d", len(document), size)
	}
	padded := make([]byte, size)
	copy(padded, document)
	for index := len(document); index < size; index++ {
		padded[index] = ' '
	}
	return padded
}

// TestDecodeOperationRequestEnvelopeBoundaries pins the per-operation envelope
// arithmetic: each bound is accepted AT the exact size and refused one byte
// above it, and an unregistered alias is refused before any size decision.
func TestDecodeOperationRequestEnvelopeBoundaries(t *testing.T) {
	envelope := []byte(`{"input":{}}`)
	cases := []struct {
		operation string
		bound     int
	}{
		{"pipeline.apply", api.MaxYAMLRequestBytes},
		{"pipeline.update", api.MaxYAMLRequestBytes},
		{"pipeline.start", api.MaxYAMLRequestBytes},
		{"task.create", api.MaxRequestBytes},
		{"pipeline.read", api.MaxRequestBytes},
	}
	for _, testCase := range cases {
		t.Run(testCase.operation+"/at-bound", func(t *testing.T) {
			body := padJSON(t, envelope, testCase.bound)
			if _, err := api.DecodeOperationRequest(testCase.operation, body); err != nil {
				t.Fatalf("%s envelope of exactly %d bytes refused: %v", testCase.operation, testCase.bound, err)
			}
		})
		t.Run(testCase.operation+"/above-bound", func(t *testing.T) {
			body := padJSON(t, envelope, testCase.bound+1)
			_, err := api.DecodeOperationRequest(testCase.operation, body)
			requireInvalidInput(t, err, testCase.operation+" envelope one byte above its bound")
			if !strings.Contains(err.Error(), "at most") {
				t.Errorf("%s above-bound refusal %q does not name the size bound", testCase.operation, err)
			}
		})
	}

	// An alias is not a registered operation: it is refused as unknown at the
	// 1 MiB size and above it, so a bound mutation cannot make it decodable.
	for _, size := range []int{api.MaxRequestBytes, api.MaxRequestBytes + 1} {
		_, err := api.DecodeOperationRequest("PIPELINE.START", padJSON(t, envelope, size))
		requireInvalidInput(t, err, "an unregistered operation alias")
		if !strings.Contains(err.Error(), "unknown operation") {
			t.Errorf("alias refusal %q does not report an unknown operation", err)
		}
	}

	// The 2 MiB cap runs before JSON parsing: garbage one byte above the cap is
	// refused for its size, never as invalid JSON.
	garbage := bytes.Repeat([]byte("x"), api.MaxYAMLRequestBytes+1)
	_, err := api.DecodeOperationRequest("pipeline.start", garbage)
	requireInvalidInput(t, err, "invalid JSON above the 2 MiB cap")
	if !strings.Contains(err.Error(), "at most") || strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("above-cap garbage refusal = %q; want the size refusal before parsing", err)
	}
}

// mcpToolsCallBody builds one MCP tools/call JSON-RPC body around a raw
// operation request envelope.
func mcpToolsCallBody(t *testing.T, name string, arguments json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  api.MCPMethodToolsCall,
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		t.Fatalf("marshal MCP tools/call body: %v", err)
	}
	return body
}

// TestDecodeMCPToolsCallEnvelopeBoundaries pins the MCP tools/call envelope
// rule: the hard 2 MiB cap runs first, then the named operation's bound, with
// the exact accept/refuse size for a YAML operation and a default operation.
func TestDecodeMCPToolsCallEnvelopeBoundaries(t *testing.T) {
	arguments := json.RawMessage(`{"input":{}}`)
	cases := []struct {
		name  string
		bound int
	}{
		{"pipeline.start", api.MaxYAMLRequestBytes},
		{"task.create", api.MaxRequestBytes},
	}
	for _, testCase := range cases {
		body := mcpToolsCallBody(t, testCase.name, arguments)
		t.Run(testCase.name+"/at-bound", func(t *testing.T) {
			padded := padJSON(t, body, testCase.bound)
			params, err := api.DecodeMCPToolsCall(padded)
			if err != nil {
				t.Fatalf("%s MCP body of exactly %d bytes refused: %v", testCase.name, testCase.bound, err)
			}
			if params.Name != testCase.name {
				t.Errorf("decoded MCP name = %q; want %q", params.Name, testCase.name)
			}
		})
		t.Run(testCase.name+"/above-bound", func(t *testing.T) {
			padded := padJSON(t, body, testCase.bound+1)
			_, err := api.DecodeMCPToolsCall(padded)
			requireInvalidInput(t, err, testCase.name+" MCP body one byte above its bound")
			if !strings.Contains(err.Error(), "at most") {
				t.Errorf("%s above-bound refusal %q does not name the size bound", testCase.name, err)
			}
		})
	}

	// The hard 2 MiB cap refuses garbage by size before the JSON parse.
	garbage := bytes.Repeat([]byte("x"), api.MaxYAMLRequestBytes+1)
	_, err := api.DecodeMCPToolsCall(garbage)
	requireInvalidInput(t, err, "invalid JSON above the hard 2 MiB cap")
	if !strings.Contains(err.Error(), "at most") || strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("above-cap garbage refusal = %q; want the size refusal before parsing", err)
	}

	// A truncated body that decodes a valid prefix is still refused: the parse
	// error must not be ignored just because the populated fields validate.
	body := mcpToolsCallBody(t, "pipeline.start", json.RawMessage(`{"input":{}}`))
	_, err = api.DecodeMCPToolsCall(body[:len(body)-1])
	requireInvalidInput(t, err, "a truncated MCP tools/call body")

	// A fully decoded body followed by a second JSON value is refused; the
	// populated request must not be accepted after the trailing-data error.
	trailing := append(append([]byte(nil), body...), []byte(" {}")...)
	_, err = api.DecodeMCPToolsCall(trailing)
	requireInvalidInput(t, err, "an MCP tools/call body with trailing data")
}

// TestDecodeOperationRequestValidatesEnvelope pins that the envelope decoder
// refuses an unknown operation and the shared envelope validation failures.
func TestDecodeOperationRequestValidatesEnvelope(t *testing.T) {
	if _, err := api.DecodeOperationRequest("task.create", []byte(`{"input":{}}`)); err != nil {
		t.Fatalf("valid minimal task.create envelope refused: %v", err)
	}
	_, err := api.DecodeOperationRequest("task.unknown", []byte(`{"input":{}}`))
	requireInvalidInput(t, err, "an unknown operation")
	if !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("unknown operation refusal = %q; want an unknown operation reason", err)
	}
	_, err = api.DecodeOperationRequest("task.create", []byte("not json"))
	requireInvalidInput(t, err, "a non-JSON body")
	for name, body := range map[string]string{
		"zero expected version":    `{"input":{},"expected_version":0}`,
		"space in idempotency key": `{"input":{},"idempotency_key":"bad key"}`,
		"non-object input":         `{"input":[]}`,
		"unknown envelope member":  `{"input":{},"unexpected":true}`,
	} {
		_, err := api.DecodeOperationRequest("task.create", []byte(body))
		requireInvalidInput(t, err, "task.create with "+name)
	}
}

// TestDecodeMCPToolsCallValidatesRequest pins the MCP request validation that
// the size bound alone cannot cover: the tools/call method, the known tool
// name and the arguments envelope.
func TestDecodeMCPToolsCallValidatesRequest(t *testing.T) {
	arguments := json.RawMessage(`{"input":{"project_id":"prj-1","pipeline_id":"pipe-1"}}`)
	params, err := api.DecodeMCPToolsCall(mcpToolsCallBody(t, "pipeline.start", arguments))
	if err != nil {
		t.Fatalf("valid pipeline.start MCP call refused: %v", err)
	}
	if params.Name != "pipeline.start" {
		t.Errorf("decoded MCP name = %q; want pipeline.start", params.Name)
	}
	if !bytes.Equal(params.Arguments, arguments) {
		t.Errorf("decoded MCP arguments = %s; want %s", params.Arguments, arguments)
	}

	_, err = api.DecodeMCPToolsCall(mcpToolsCallBody(t, "task.unknown", json.RawMessage(`{"input":{}}`)))
	requireInvalidInput(t, err, "an unknown MCP tool name")

	listBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  api.MCPMethodToolsList,
		"params":  map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal tools/list body: %v", err)
	}
	_, err = api.DecodeMCPToolsCall(listBody)
	requireInvalidInput(t, err, "an MCP tools/list body")

	for name, badArguments := range map[string]json.RawMessage{
		"zero expected version":    json.RawMessage(`{"input":{},"expected_version":0}`),
		"space in idempotency key": json.RawMessage(`{"input":{},"idempotency_key":"bad key"}`),
		"non-object input":         json.RawMessage(`{"input":[]}`),
	} {
		_, err := api.DecodeMCPToolsCall(mcpToolsCallBody(t, "pipeline.start", badArguments))
		requireInvalidInput(t, err, "MCP pipeline.start with "+name)
	}
}

// TestDecodeMCPOperationRequestRefusesMalformedJSON pins that the codec's MCP
// decoder refuses a truncated body even when the populated prefix would
// otherwise validate.
func TestDecodeMCPOperationRequestRefusesMalformedJSON(t *testing.T) {
	_, err := api.DecodeMCPOperationRequest("tenant.create", json.RawMessage(`{"tenant_id":"t-1","name":"n"`))
	requireInvalidInput(t, err, "a truncated MCP operation input")
	_, err = api.DecodeMCPOperationRequest("tenant.create", json.RawMessage(`{"tenant_id":"t-1","name":"n"} {}`))
	requireInvalidInput(t, err, "an MCP operation input with trailing data")
}

// TestDecodeRouteRequestRequiresRegisteredBinding pins that DecodeRouteRequest
// only accepts one of the operation's frozen bindings: the operation, method
// and path must all match a registered row.
func TestDecodeRouteRequestRequiresRegisteredBinding(t *testing.T) {
	canonical, ok := api.LookupRoute("pipeline.start")
	if !ok {
		t.Fatal("the registry has no pipeline.start route")
	}
	alternates := api.AlternateRoutes()
	if len(alternates) != 1 {
		t.Fatalf("AlternateRoutes() has %d rows; want the one pipeline.start alternate", len(alternates))
	}
	alternate := alternates[0]

	if _, err := api.DecodeRouteRequest(canonical, json.RawMessage(`{}`),
		map[string]string{"project": "prj-1", "pipeline": "pipe-1"}); err != nil {
		t.Fatalf("registered canonical binding refused: %v", err)
	}
	if _, err := api.DecodeRouteRequest(alternate, json.RawMessage(`{"pipeline_id":"pipe-1"}`),
		map[string]string{"project": "prj-1"}); err != nil {
		t.Fatalf("registered alternate binding refused: %v", err)
	}
	_, err := api.DecodeRouteRequest(canonical, json.RawMessage(`not json`),
		map[string]string{"project": "prj-1", "pipeline": "pipe-1"})
	requireInvalidInput(t, err, "a non-JSON body on a registered binding")

	for name, route := range map[string]api.Route{
		"alternate path with GET": {
			Operation: "pipeline.start", Method: api.MethodGet, Path: alternate.Path,
		},
		"invented path": {
			Operation: "pipeline.start", Method: api.MethodPost, Path: "/api/v1/projects/{project}/invented",
		},
		"other operation on the alternate path": {
			Operation: "pipeline.apply", Method: api.MethodPost, Path: alternate.Path,
		},
		"unknown operation": {
			Operation: "task.unknown", Method: api.MethodPost, Path: "/api/v1/tasks",
		},
	} {
		_, err := api.DecodeRouteRequest(route, json.RawMessage(`{"pipeline_id":"pipe-1"}`),
			map[string]string{"project": "prj-1"})
		requireInvalidInput(t, err, name)
		if name == "unknown operation" {
			if !strings.Contains(err.Error(), "unknown operation") {
				t.Errorf("%s refusal = %q; want an unknown operation reason", name, err)
			}
			continue
		}
		if !strings.Contains(err.Error(), "not a registered binding") {
			t.Errorf("%s refusal = %q; want a not-a-registered-binding reason", name, err)
		}
	}
}

// TestPipelineStartValidateBounds pins the pipeline.start DTO validation:
// the project grammar, the input key/value bounds and the exact boundaries.
func TestPipelineStartValidateBounds(t *testing.T) {
	valid := func() api.PipelineStartRequest {
		return api.PipelineStartRequest{ProjectID: "prj-1", PipelineID: "pipe-1"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid stored start refused: %v", err)
	}
	atKeyBound := valid()
	atKeyBound.Inputs = map[string]string{strings.Repeat("k", 128): strings.Repeat("v", api.MaxRequestBytes)}
	if err := atKeyBound.Validate(); err != nil {
		t.Fatalf("start at the input key/value bounds refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*api.PipelineStartRequest)
	}{
		{"malformed project id", func(request *api.PipelineStartRequest) { request.ProjectID = "bad/id" }},
		{"empty input name", func(request *api.PipelineStartRequest) {
			request.Inputs = map[string]string{"": "v"}
		}},
		{"oversized input name", func(request *api.PipelineStartRequest) {
			request.Inputs = map[string]string{strings.Repeat("k", 129): "v"}
		}},
		{"oversized input value", func(request *api.PipelineStartRequest) {
			request.Inputs = map[string]string{"k": strings.Repeat("v", api.MaxRequestBytes+1)}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := valid()
			testCase.mutate(&request)
			requireInvalidInput(t, request.Validate(), "pipeline.start with "+testCase.name)
		})
	}
}

// TestPipelineStartIdempotencyContentRefusals pins the content-level refusals
// and the always-present empty inputs object.
func TestPipelineStartIdempotencyContentRefusals(t *testing.T) {
	version := uint64(3)
	zero := uint64(0)
	stored := api.PipelineStartRequest{PipelineID: "pipe-1"}

	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(stored, &zero)),
		"stored content with a zero expected version")
	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(
		api.PipelineStartRequest{PipelineID: "bad/id"}, &version)),
		"stored content with a malformed pipeline id")
	requireInvalidInput(t, contentError(api.PipelineStartIdempotencyContent(
		api.PipelineStartRequest{PipelineID: "pipe-1", YAML: inlineYAML}, nil)),
		"content for both selections without a version")

	storedContent, err := api.PipelineStartIdempotencyContent(stored, &version)
	if err != nil {
		t.Fatalf("stored content: %v", err)
	}
	if !bytes.Contains(storedContent, []byte(`"inputs":{}`)) {
		t.Errorf("stored content with absent inputs = %s; want the literal \"inputs\":{}", storedContent)
	}
	inlineContent, err := api.PipelineStartIdempotencyContent(api.PipelineStartRequest{YAML: inlineYAML}, nil)
	if err != nil {
		t.Fatalf("inline content: %v", err)
	}
	if !bytes.Contains(inlineContent, []byte(`"inputs":{}`)) {
		t.Errorf("inline content with absent inputs = %s; want the literal \"inputs\":{}", inlineContent)
	}
}

// TestYAMLOperationsAndRoutesReturnCopies pins that the exported tables are
// defensive copies: mutating a returned slice cannot change later reads or the
// envelope decision.
func TestYAMLOperationsAndRoutesReturnCopies(t *testing.T) {
	want := []string{"pipeline.apply", "pipeline.update", "pipeline.start"}
	if got := api.YAMLOperations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("YAMLOperations() = %v; want %v", got, want)
	}
	mutated := api.YAMLOperations()
	mutated[0] = "task.create"
	// The appended slice is discarded here and below: only its write into any
	// spare capacity of the returned backing array could reach a later read.
	_ = append(mutated, "pipeline.delete")
	if got := api.YAMLOperations(); !reflect.DeepEqual(got, want) {
		t.Errorf("YAMLOperations() after caller mutation = %v; want the unchanged %v", got, want)
	}
	if got := api.MaxEnvelopeBytes("task.create"); got != api.MaxRequestBytes {
		t.Errorf("MaxEnvelopeBytes(task.create) after caller mutation = %d; want %d", got, api.MaxRequestBytes)
	}
	if got := api.MaxEnvelopeBytes("pipeline.apply"); got != api.MaxYAMLRequestBytes {
		t.Errorf("MaxEnvelopeBytes(pipeline.apply) after caller mutation = %d; want %d", got, api.MaxYAMLRequestBytes)
	}

	canonical := api.Routes()
	if len(canonical) != 51 {
		t.Fatalf("Routes() has %d rows; want 51", len(canonical))
	}
	canonical[0].Operation = "mutated"
	_ = append(canonical, api.Route{Operation: "mutated"})
	again := api.Routes()
	if len(again) != 51 {
		t.Errorf("Routes() after caller append has %d rows; want the unchanged 51", len(again))
	}
	if again[0].Operation != "tenant.create" {
		t.Errorf("Routes()[0] after caller mutation = %q; want tenant.create", again[0].Operation)
	}
	if _, ok := api.LookupRoute("tenant.create"); !ok {
		t.Error("LookupRoute(tenant.create) stopped resolving after caller mutation")
	}

	alternates := api.AlternateRoutes()
	if len(alternates) != 1 {
		t.Fatalf("AlternateRoutes() has %d rows; want the one alternate", len(alternates))
	}
	alternates[0].Operation = "mutated"
	_ = append(alternates, api.Route{Operation: "mutated"})
	againAlternates := api.AlternateRoutes()
	if len(againAlternates) != 1 || againAlternates[0].Operation != "pipeline.start" {
		t.Errorf("AlternateRoutes() after caller mutation = %+v; want the unchanged pipeline.start row", againAlternates)
	}
}

// TestWaitingInboxItemVersionOne pins that a waiting item whose question
// version and lifecycle version are both exactly 1 validates; the version
// checks are zero refusals, not <= 1 refusals.
func TestWaitingInboxItemVersionOne(t *testing.T) {
	item := api.InboxItem{
		Kind:             "waiting_task",
		TaskID:           "tsk-1",
		Assignment:       "role/reviewer",
		QuestionID:       "qst-1",
		Question:         "Why?",
		Version:          1,
		LifecycleVersion: 1,
		At:               inboxNow,
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("waiting item with version 1 and lifecycle version 1 refused: %v", err)
	}
	if err := (api.TaskInboxResult{Items: []api.InboxItem{item}}).Validate(); err != nil {
		t.Fatalf("task.inbox result with a version-1 waiting item refused: %v", err)
	}
}

// wrapBase64 inserts CRLF line breaks every width characters.
func wrapBase64(encoded string, width int) string {
	var builder strings.Builder
	for start := 0; start < len(encoded); start += width {
		end := start + width
		if end > len(encoded) {
			end = len(encoded)
		}
		builder.WriteString(encoded[start:end])
		if end < len(encoded) {
			builder.WriteString("\r\n")
		}
	}
	return builder.String()
}

// decodeInlineYAML decodes one pipeline.start input carrying a raw base64 text
// member through the frozen codec.
func decodeInlineYAML(t *testing.T, encoded string) api.PipelineStartRequest {
	t.Helper()
	input, err := json.Marshal(map[string]any{"yaml": encoded})
	if err != nil {
		t.Fatalf("marshal inline YAML input: %v", err)
	}
	value, err := codecFor(t, "pipeline.start").DecodeRequest(input)
	if err != nil {
		t.Fatalf("decode inline YAML input of %d characters: %v", len(encoded), err)
	}
	return value.(api.PipelineStartRequest)
}

// TestInlineYAMLBase64CRLFIstIgnored pins the encoded-length rule: the
// standard padded base64 decoder ignores CR and LF, so MaxInlineYAMLEncodedBytes
// is the advertised schema bound rather than an enforced wire length. The
// enforced limits are the 1 MiB decoded document and the 2 MiB envelope.
func TestInlineYAMLBase64CRLFIstIgnored(t *testing.T) {
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	encoded := base64.StdEncoding.EncodeToString(document)
	if len(encoded) != api.MaxInlineYAMLEncodedBytes {
		t.Fatalf("base64 length of 1 MiB = %d; want the advertised %d", len(encoded), api.MaxInlineYAMLEncodedBytes)
	}
	wrapped64 := wrapBase64(encoded, 64)
	wrapped76 := wrapBase64(encoded, 76)
	if len(wrapped64) <= api.MaxInlineYAMLEncodedBytes {
		t.Fatalf("CRLF-wrapped base64 is %d characters; it must exceed the advertised bound %d",
			len(wrapped64), api.MaxInlineYAMLEncodedBytes)
	}

	unwrapped := decodeInlineYAML(t, encoded)
	fromWrapped := decodeInlineYAML(t, wrapped64)
	if !bytes.Equal(unwrapped.YAML, document) || !bytes.Equal(fromWrapped.YAML, document) {
		t.Fatalf("decoded inline YAML does not equal the exact 1 MiB document")
	}
	if !bytes.Equal(fromWrapped.YAML, unwrapped.YAML) {
		t.Error("CRLF-wrapped and unwrapped base64 decoded different bytes")
	}

	// The full envelope path accepts the wrapped form because the decoded
	// document is at the 1 MiB bound and the envelope stays under 2 MiB.
	input, err := json.Marshal(map[string]any{"yaml": wrapped64})
	if err != nil {
		t.Fatalf("marshal wrapped inline YAML input: %v", err)
	}
	envelope, err := json.Marshal(api.Request{Input: input})
	if err != nil {
		t.Fatalf("marshal wrapped envelope: %v", err)
	}
	if len(envelope) > api.MaxYAMLRequestBytes {
		t.Fatalf("wrapped envelope is %d bytes; it must stay under the 2 MiB bound", len(envelope))
	}
	if _, err := api.DecodeOperationRequest("pipeline.start", envelope); err != nil {
		t.Fatalf("wrapped inline YAML envelope refused: %v", err)
	}

	// Two differently wrapped encodings of the same bytes are the same content.
	fromWrapped76 := decodeInlineYAML(t, wrapped76)
	content64, err := api.PipelineStartIdempotencyContent(fromWrapped, nil)
	if err != nil {
		t.Fatalf("content for the 64-column encoding: %v", err)
	}
	content76, err := api.PipelineStartIdempotencyContent(fromWrapped76, nil)
	if err != nil {
		t.Fatalf("content for the 76-column encoding: %v", err)
	}
	if !bytes.Equal(content64, content76) {
		t.Error("differently wrapped encodings of the same bytes produced different content")
	}

	// The decoder is standard padded base64: URL-safe characters and a space
	// inside the text are refused.
	small := base64.StdEncoding.EncodeToString(inlineYAML)
	for name, replacement := range map[string]string{
		"url-safe dash":       "-",
		"url-safe underscore": "_",
		"space":               " ",
	} {
		corrupted := replacement + small[1:]
		input, err := json.Marshal(map[string]any{"yaml": corrupted})
		if err != nil {
			t.Fatalf("marshal %s input: %v", name, err)
		}
		_, err = codecFor(t, "pipeline.start").DecodeRequest(input)
		requireInvalidInput(t, err, "inline YAML base64 with a "+name)
	}
}
