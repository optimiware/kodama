package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// yamlOperationCases lists the three operations that carry inline YAML and one
// non-YAML pipeline operation. The HTTP edge must apply api.MaxEnvelopeBytes
// per matched operation rather than one global bound.
var yamlOperationCases = []struct {
	name      string
	method    string
	path      string
	operation string
}{
	{"pipeline.apply", "PUT", "/api/v1/projects/project-a/pipelines/build.v2", "pipeline.apply"},
	{"pipeline.update", "PATCH", "/api/v1/projects/project-a/pipelines/pipe-1", "pipeline.update"},
	{"pipeline.start canonical", "POST", "/api/v1/projects/project-a/pipelines/pipe-1/start", "pipeline.start"},
	{"pipeline.start alternate", "POST", "/api/v1/projects/project-a/executions", "pipeline.start"},
	{"pipeline.read", "GET", "/api/v1/projects/project-a/pipelines/pipe-1", "pipeline.read"},
}

// paddedBody pads one valid request body with insignificant JSON whitespace to
// an exact byte size.
func paddedBody(t *testing.T, base []byte, size int) []byte {
	t.Helper()
	if size < len(base) {
		t.Fatalf("request size %d is smaller than the minimal body %d", size, len(base))
	}
	body := make([]byte, size)
	copy(body, base)
	for index := len(base); index < size; index++ {
		body[index] = ' '
	}
	return body
}

// wrappedBase64 inserts CRLF line breaks every width bytes, which the standard
// padded base64 decoder ignores.
func wrappedBase64(encoded string, width int) string {
	var builder strings.Builder
	for len(encoded) > width {
		builder.WriteString(encoded[:width])
		builder.WriteString("\r\n")
		encoded = encoded[width:]
	}
	builder.WriteString(encoded)
	return builder.String()
}

// inlineStartBody builds one api.Request envelope whose input selects inline
// YAML on the task-free alternate route. The project is intentionally omitted:
// the REST route supplies it and the edge must hand the route params and raw
// body through unchanged.
func inlineStartBody(t *testing.T, document []byte, wrap bool) []byte {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(document)
	if wrap {
		encoded = wrappedBase64(encoded, 76)
	}
	input, err := json.Marshal(map[string]any{
		"yaml":   encoded,
		"inputs": map[string]string{},
	})
	if err != nil {
		t.Fatalf("marshal inline start input: %v", err)
	}
	body, err := json.Marshal(api.Request{Input: input, IdempotencyKey: "start-inline-1"})
	if err != nil {
		t.Fatalf("marshal inline start envelope: %v", err)
	}
	return body
}

// TestEdgeRoutesAlternateInlineStart pins the alternate binding: POST
// /api/v1/projects/{project}/executions reaches pipeline.start with only the
// project route parameter, while GET on the same path stays execution.list.
// The body selects inline YAML and the edge preserves it exactly for the
// route-aware decoder.
func TestEdgeRoutesAlternateInlineStart(t *testing.T) {
	document := []byte("pipeline: build\nsteps:\n  - run: make\n")
	body := inlineStartBody(t, document, false)

	router := &fakeRouter{}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "POST", "/api/v1/projects/project-a/executions", body, "Bearer token-1")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", response.status, response.body.String())
	}
	if got := router.callCount(); got != 1 {
		t.Fatalf("router calls = %d; want 1", got)
	}
	invocation := router.lastInvocation()
	if string(invocation.Operation) != "pipeline.start" {
		t.Fatalf("operation = %q; want pipeline.start", invocation.Operation)
	}
	if !reflect.DeepEqual(invocation.Params, map[string]string{"project": "project-a"}) {
		t.Fatalf("params = %#v; want only the project", invocation.Params)
	}
	if invocation.Credential != "token-1" {
		t.Fatalf("credential = %q; want token-1", invocation.Credential)
	}
	if invocation.Request.ExpectedVersion != nil {
		t.Fatalf("inline start carried expected_version %d; want none", *invocation.Request.ExpectedVersion)
	}

	route, params, ok := api.Match(api.MethodPost, "/api/v1/projects/project-a/executions")
	if !ok {
		t.Fatal("api.Match did not resolve the alternate route")
	}
	value, err := api.DecodeRouteRequest(route, invocation.Request.Input, params)
	if err != nil {
		t.Fatalf("route-aware decode of the forwarded inline start: %v", err)
	}
	decoded := value.(api.PipelineStartRequest)
	if decoded.ProjectID != "project-a" || decoded.PipelineID != "" {
		t.Fatalf("decoded selection = project %q pipeline %q; want project only", decoded.ProjectID, decoded.PipelineID)
	}
	if !bytes.Equal(decoded.YAML, document) {
		t.Fatalf("inline YAML = %q; want the exact bytes %q", decoded.YAML, document)
	}

	// The same path is still the execution list for GET.
	response = perform(t, edge, "GET", "/api/v1/projects/project-a/executions",
		[]byte(`{"input":{}}`), "Bearer token-1")
	if response.status != http.StatusOK {
		t.Fatalf("GET status = %d; want 200 (body %s)", response.status, response.body.String())
	}
	invocation = router.lastInvocation()
	if string(invocation.Operation) != "execution.list" {
		t.Fatalf("GET operation = %q; want execution.list", invocation.Operation)
	}
	if !reflect.DeepEqual(invocation.Params, map[string]string{"project": "project-a"}) {
		t.Fatalf("GET params = %#v; want only the project", invocation.Params)
	}
}

// TestEdgeAlternateStartStoredSelection pins that the alternate route also
// carries the stored-pipeline selection unchanged, including the shared
// envelope expected_version.
func TestEdgeAlternateStartStoredSelection(t *testing.T) {
	version := uint64(3)
	body, err := json.Marshal(api.Request{
		Input:           json.RawMessage(`{"pipeline_id":"pipe-1","inputs":{}}`),
		ExpectedVersion: &version,
		IdempotencyKey:  "start-stored-1",
	})
	if err != nil {
		t.Fatalf("marshal stored start envelope: %v", err)
	}

	router := &fakeRouter{}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "POST", "/api/v1/projects/project-a/executions", body, "Bearer token-1")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", response.status, response.body.String())
	}
	invocation := router.lastInvocation()
	if string(invocation.Operation) != "pipeline.start" {
		t.Fatalf("operation = %q; want pipeline.start", invocation.Operation)
	}
	if invocation.Request.ExpectedVersion == nil || *invocation.Request.ExpectedVersion != version {
		t.Fatalf("expected_version = %v; want %d", invocation.Request.ExpectedVersion, version)
	}
}

// TestEdgeYAMLEnvelopeBound pins the per-operation request envelope: the three
// YAML-carrying operations accept exactly api.MaxYAMLRequestBytes and refuse one
// byte more, while a non-YAML pipeline operation keeps api.MaxRequestBytes.
func TestEdgeYAMLEnvelopeBound(t *testing.T) {
	for _, testCase := range yamlOperationCases {
		bound := api.MaxEnvelopeBytes(testCase.operation)
		if testCase.operation == "pipeline.read" {
			if bound != api.MaxRequestBytes {
				t.Fatalf("MaxEnvelopeBytes(%q) = %d; want %d", testCase.operation, bound, api.MaxRequestBytes)
			}
		} else if bound != api.MaxYAMLRequestBytes {
			t.Fatalf("MaxEnvelopeBytes(%q) = %d; want %d", testCase.operation, bound, api.MaxYAMLRequestBytes)
		}

		base := []byte(`{"input":{},"idempotency_key":"key-1"}`)

		t.Run(testCase.name+" exactly at the bound", func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path,
				paddedBody(t, base, bound), "Bearer token-1")
			if response.status != http.StatusOK {
				t.Fatalf("status = %d; want 200 at exactly %d bytes (body %s)",
					response.status, bound, response.body.String())
			}
			if got := router.callCount(); got != 1 {
				t.Fatalf("router calls = %d; want 1", got)
			}
			if got := string(router.lastInvocation().Operation); got != testCase.operation {
				t.Fatalf("operation = %q; want %q", got, testCase.operation)
			}
		})

		t.Run(testCase.name+" one byte over the bound", func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path,
				paddedBody(t, base, bound+1), "Bearer token-1")
			if response.status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 one byte over %d", response.status, bound)
			}
			if got := decodePublicError(t, response); got.Code != api.CodeInvalidInput {
				t.Fatalf("code = %q; want %q", got.Code, api.CodeInvalidInput)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestEdgeInlineStartDecodedBoundWithCRLFBase64 pins that a 1 MiB inline
// document (about 1.4 MiB of base64) fits the 2 MiB YAML envelope, that CR/LF
// line wrapping is accepted by the standard padded decoder, and that the exact
// document bytes survive the edge unchanged.
func TestEdgeInlineStartDecodedBoundWithCRLFBase64(t *testing.T) {
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	body := inlineStartBody(t, document, true)
	if len(body) <= api.MaxRequestBytes {
		t.Fatalf("test envelope is only %d bytes; it must exceed the default 1 MiB bound", len(body))
	}
	if len(body) > api.MaxYAMLRequestBytes {
		t.Fatalf("test envelope is %d bytes; it must fit the 2 MiB YAML bound", len(body))
	}

	router := &fakeRouter{}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "POST", "/api/v1/projects/project-a/executions", body, "Bearer token-1")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", response.status, response.body.String())
	}
	invocation := router.lastInvocation()
	route, params, ok := api.Match(api.MethodPost, "/api/v1/projects/project-a/executions")
	if !ok {
		t.Fatal("api.Match did not resolve the alternate route")
	}
	value, err := api.DecodeRouteRequest(route, invocation.Request.Input, params)
	if err != nil {
		t.Fatalf("route-aware decode of the CRLF-padded inline start: %v", err)
	}
	decoded := value.(api.PipelineStartRequest)
	if !bytes.Equal(decoded.YAML, document) {
		t.Fatalf("decoded inline YAML differs from the exact %d-byte document (got %d bytes)", len(document), len(decoded.YAML))
	}
}
