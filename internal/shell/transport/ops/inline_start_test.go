package ops_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// publicPipelineStartCodec validates the pipeline.start input with the declared
// public MCP codec. It models a consumer that has the public DTO and keeps the
// transport tests on the real exclusive-selection and base64 rules without
// implementing the capability handler.
type publicPipelineStartCodec struct{}

func (publicPipelineStartCodec) Validate(raw json.RawMessage) error {
	_, err := api.DecodeMCPOperationRequest("pipeline.start", raw)
	return err
}

// newInlineStartHarness builds the 58-operation harness with pipeline.start's
// input codec replaced by the declared public codec. All other operations keep
// the recording fakes so the router's generic boundary is unchanged.
func newInlineStartHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	bindings := append([]ops.Binding(nil), h.bindings...)
	for index := range bindings {
		if bindings[index].Operation == authorize.OperationName("pipeline.start") {
			bindings[index].Input = publicPipelineStartCodec{}
		}
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.scopes,
		Executions: h.executions,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("NewRouter with the declared pipeline.start codec: %v", err)
	}
	h.router = router
	return h
}

// crlfWrappedBase64 inserts CRLF line breaks every width bytes.
func crlfWrappedBase64(encoded string, width int) string {
	var builder strings.Builder
	for len(encoded) > width {
		builder.WriteString(encoded[:width])
		builder.WriteString("\r\n")
		encoded = encoded[width:]
	}
	builder.WriteString(encoded)
	return builder.String()
}

// inlineStartInput builds the pipeline.start input selecting inline YAML over
// MCP (which must carry the project in the body).
func inlineStartInput(t *testing.T, document []byte, wrap bool) json.RawMessage {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(document)
	if wrap {
		encoded = crlfWrappedBase64(encoded, 76)
	}
	input, err := json.Marshal(map[string]any{
		"project_id": "project-a",
		"yaml":       encoded,
		"inputs":     map[string]string{"branch": "main"},
	})
	if err != nil {
		t.Fatalf("marshal inline start input: %v", err)
	}
	return input
}

// TestRouterInlineStartSelection pins the router's transport boundary for the
// exclusive pipeline.start selection: an inline document passes with no
// pipeline id and no expected pipeline version, while both/neither selections
// are refused with invalid_input before the handler.
func TestRouterInlineStartSelection(t *testing.T) {
	h := newInlineStartHarness(t)
	operation, _ := authorize.LookupOperation("pipeline.start")
	h.prepare(operation)
	probe := h.probeFor(operation.Name)

	document := []byte("pipeline: build\r\nsteps:\r\n  - run: make\r\n")
	inline := inlineStartInput(t, document, false)
	if _, err := h.invoke(operation.Name, api.Request{
		Input:          inline,
		IdempotencyKey: "inline-start-1",
	}, nil); err != nil {
		t.Fatalf("inline pipeline.start: %v", err)
	}
	if got := probe.handler.callCount(); got != 1 {
		t.Fatalf("handler calls = %d; want 1 for the inline selection", got)
	}
	call := probe.handler.lastCall()
	if call.Request.ExpectedVersion != nil {
		t.Fatalf("inline selection carried expected_version %d; want none", *call.Request.ExpectedVersion)
	}
	if call := h.scopes.lastCall(); call.Params != nil {
		t.Fatalf("scope resolver params = %v; want none for an MCP call", call.Params)
	}
	value, err := api.DecodeMCPOperationRequest("pipeline.start", call.Request.Input)
	if err != nil {
		t.Fatalf("decode the forwarded inline input: %v", err)
	}
	typed := value.(api.PipelineStartRequest)
	if typed.PipelineID != "" || !bytes.Equal(typed.YAML, document) {
		t.Fatalf("inline selection = pipeline %q yaml %q; want no pipeline id and the exact bytes", typed.PipelineID, typed.YAML)
	}

	cases := []struct {
		name  string
		input json.RawMessage
	}{
		{"both selections", json.RawMessage(`{"project_id":"project-a","pipeline_id":"pipe-1","yaml":"` + base64.StdEncoding.EncodeToString(document) + `"}`)},
		{"neither selection", json.RawMessage(`{"project_id":"project-a","inputs":{}}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := h.invoke(operation.Name, api.Request{
				Input:          testCase.input,
				IdempotencyKey: "inline-start-refused",
			}, nil)
			mustCode(t, err, fault.InvalidInput)
			if got := probe.handler.callCount(); got != 1 {
				t.Fatalf("handler calls = %d; want the single successful inline call", got)
			}
		})
	}

	// A stored selection still routes and carries the caller's expected version.
	version := uint64(4)
	if _, err := h.invoke(operation.Name, api.Request{
		Input:           json.RawMessage(`{"project_id":"project-a","pipeline_id":"pipe-1","inputs":{}}`),
		ExpectedVersion: &version,
		IdempotencyKey:  "stored-start-1",
	}, nil); err != nil {
		t.Fatalf("stored pipeline.start: %v", err)
	}
	if got := probe.handler.callCount(); got != 2 {
		t.Fatalf("handler calls = %d; want 2 after the stored selection", got)
	}
	if last := probe.handler.lastCall(); last.Request.ExpectedVersion == nil || *last.Request.ExpectedVersion != version {
		t.Fatalf("stored selection expected_version = %v; want %d", last.Request.ExpectedVersion, version)
	}
}

// TestInlineStartExclusiveSelectionRefusedAtBothEdges pins the exclusive
// pipeline.start selection through both public edges: the declared consumer
// codec refuses both/neither selection with invalid_input before any handler
// effect.
func TestInlineStartExclusiveSelectionRefusedAtBothEdges(t *testing.T) {
	h := newInlineStartHarness(t)
	operation, _ := authorize.LookupOperation("pipeline.start")
	h.prepare(operation)
	probe := h.probeFor(operation.Name)

	document := []byte("pipeline: build\n")
	encoded := base64.StdEncoding.EncodeToString(document)
	cases := []struct {
		name  string
		input json.RawMessage
	}{
		{"both selections", json.RawMessage(`{"project_id":"project-a","pipeline_id":"pipe-1","yaml":"` + encoded + `"}`)},
		{"neither selection", json.RawMessage(`{"project_id":"project-a","inputs":{}}`)},
	}

	restEdge, err := httpapi.NewEdge(h.router)
	if err != nil {
		t.Fatalf("httpapi.NewEdge: %v", err)
	}
	mcpEdge, err := mcp.NewEdge(mcp.Config{
		Router:     h.router,
		ServerInfo: api.MCPImplementation{Name: "kodama", Version: "test"},
	})
	if err != nil {
		t.Fatalf("mcp.NewEdge: %v", err)
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := api.Request{Input: testCase.input, IdempotencyKey: "inline-refused-1"}
			envelope, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshal request envelope: %v", err)
			}

			httpRequest, err := http.NewRequest(http.MethodPost,
				"/api/v1/projects/project-a/executions", bytes.NewReader(envelope))
			if err != nil {
				t.Fatalf("http.NewRequest: %v", err)
			}
			httpRequest.Header.Set("Authorization", "Bearer "+testToken)
			httpResponse := newRecorder()
			restEdge.ServeHTTP(httpResponse, httpRequest)
			if httpResponse.status != http.StatusBadRequest {
				t.Fatalf("REST status = %d; want 400 (body %s)", httpResponse.status, httpResponse.body.String())
			}
			var publicError api.Error
			if err := json.Unmarshal(httpResponse.body.Bytes(), &publicError); err != nil {
				t.Fatalf("decode REST error: %v", err)
			}
			if publicError.Code != api.CodeInvalidInput {
				t.Fatalf("REST code = %q; want %q", publicError.Code, api.CodeInvalidInput)
			}

			mcpMessage, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params": map[string]any{
					"name":      "pipeline.start",
					"arguments": request,
				},
			})
			if err != nil {
				t.Fatalf("marshal MCP tools/call: %v", err)
			}
			mcpResponse := decodeMCPResponse(t, mcpEdge.Handle(context.Background(), testToken, mcpMessage))
			if mcpResponse.Error == nil || mcpResponse.Error.Code != -32602 {
				t.Fatalf("MCP error = %+v; want invalid_input -32602", mcpResponse.Error)
			}

			if got := probe.handler.callCount(); got != 0 {
				t.Fatalf("handler calls = %d; want 0 for a refused selection", got)
			}
		})
	}
}

// decodeMCPResponse decodes one JSON-RPC response for the edge refusal test.
func decodeMCPResponse(t *testing.T, body []byte) api.MCPResponse {
	t.Helper()
	var response api.MCPResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("MCP response is invalid: %v", err)
	}
	return response
}

// TestInlineStartRESTAndMCPParity pins that both edges carry the same inline
// start operation, credential and exact request bytes through the shared
// router under the 2 MiB YAML envelope. The REST alternate route supplies the
// project as a path parameter and therefore resolves no pipeline target.
func TestInlineStartRESTAndMCPParity(t *testing.T) {
	h := newInlineStartHarness(t)
	operation, _ := authorize.LookupOperation("pipeline.start")
	h.prepare(operation)
	probe := h.probeFor(operation.Name)

	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	input := inlineStartInput(t, document, true)
	request := api.Request{Input: input, IdempotencyKey: "inline-parity-1"}
	envelope, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request envelope: %v", err)
	}

	restEdge, err := httpapi.NewEdge(h.router)
	if err != nil {
		t.Fatalf("httpapi.NewEdge: %v", err)
	}
	restRequest, err := http.NewRequest(http.MethodPost,
		"/api/v1/projects/project-a/executions", bytes.NewReader(envelope))
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	restRequest.Header.Set("Authorization", "Bearer "+testToken)
	restResponse := newRecorder()
	restEdge.ServeHTTP(restResponse, restRequest)
	if restResponse.status != http.StatusOK {
		t.Fatalf("REST status = %d; want 200 (body %s)", restResponse.status, restResponse.body.String())
	}
	if got := probe.handler.callCount(); got != 1 {
		t.Fatalf("handler calls after REST = %d; want 1", got)
	}
	restCall := probe.handler.lastCall()
	if params := h.scopes.lastCall().Params; !reflect.DeepEqual(params, map[string]string{"project": "project-a"}) {
		t.Fatalf("REST resolver params = %v; want only the project (no pipeline version read)", params)
	}

	mcpEdge, err := mcp.NewEdge(mcp.Config{
		Router:     h.router,
		ServerInfo: api.MCPImplementation{Name: "kodama", Version: "test"},
	})
	if err != nil {
		t.Fatalf("mcp.NewEdge: %v", err)
	}
	mcpMessage, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "pipeline.start",
			"arguments": request,
		},
	})
	if err != nil {
		t.Fatalf("marshal MCP tools/call: %v", err)
	}
	mcpResponse := mcpEdge.Handle(context.Background(), testToken, mcpMessage)
	var response api.MCPResponse
	if err := json.Unmarshal(mcpResponse, &response); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("MCP response is invalid: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("MCP inline start refused: %+v", response.Error)
	}
	if got := probe.handler.callCount(); got != 2 {
		t.Fatalf("handler calls after MCP = %d; want 2", got)
	}
	mcpCall := probe.handler.lastCall()

	if restCall.Operation.Name != mcpCall.Operation.Name {
		t.Fatalf("edge operations differ: REST %q MCP %q", restCall.Operation.Name, mcpCall.Operation.Name)
	}
	if !bytes.Equal(restCall.Request.Input, mcpCall.Request.Input) {
		t.Fatal("the edges forwarded different request input bytes")
	}
	if restCall.Request.ExpectedVersion != nil || mcpCall.Request.ExpectedVersion != nil {
		t.Fatal("an inline selection carried an expected pipeline version")
	}
	if restCall.Principal.ID != mcpCall.Principal.ID {
		t.Fatalf("edge principals differ: REST %q MCP %q", restCall.Principal.ID, mcpCall.Principal.ID)
	}

	value, err := api.DecodeMCPOperationRequest("pipeline.start", mcpCall.Request.Input)
	if err != nil {
		t.Fatalf("decode the MCP-forwarded inline input: %v", err)
	}
	if !bytes.Equal(value.(api.PipelineStartRequest).YAML, document) {
		t.Fatal("the inline document bytes did not survive both edges")
	}
}
