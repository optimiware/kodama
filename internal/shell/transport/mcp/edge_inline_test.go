package mcp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

// paddedMCPMessage pads one valid JSON-RPC message with insignificant JSON
// whitespace to an exact byte size.
func paddedMCPMessage(t *testing.T, base []byte, size int) []byte {
	t.Helper()
	if size < len(base) {
		t.Fatalf("message size %d is smaller than the minimal body %d", size, len(base))
	}
	body := make([]byte, size)
	copy(body, base)
	for index := len(base); index < size; index++ {
		body[index] = ' '
	}
	return body
}

// crlfWrappedBase64 inserts CRLF line breaks every width bytes. The standard
// padded base64 decoder ignores CR and LF.
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

// inlineStartArguments builds one tools/call arguments envelope for the
// pipeline.start MCP tool selecting inline YAML. MCP has no route, so the
// project must be carried in the input.
func inlineStartArguments(t *testing.T, document []byte, wrap bool) json.RawMessage {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(document)
	if wrap {
		encoded = crlfWrappedBase64(encoded, 76)
	}
	input, err := json.Marshal(map[string]any{
		"project_id": "project-a",
		"yaml":       encoded,
		"inputs":     map[string]string{},
	})
	if err != nil {
		t.Fatalf("marshal inline start input: %v", err)
	}
	arguments, err := json.Marshal(api.Request{
		Input:          input,
		IdempotencyKey: "start-inline-1",
	})
	if err != nil {
		t.Fatalf("marshal inline start arguments: %v", err)
	}
	return arguments
}

// TestMCPInlineStartTwoMiBEnvelope pins that a tools/call carrying a 1 MiB
// inline document (about 1.4 MiB of base64) is accepted under the YAML
// operation envelope, routed to pipeline.start, and forwarded byte for byte.
func TestMCPInlineStartTwoMiBEnvelope(t *testing.T) {
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	arguments := inlineStartArguments(t, document, false)
	body := toolsCallRequest(t, 1, "pipeline.start", arguments)
	if len(body) <= api.MaxRequestBytes {
		t.Fatalf("tools/call body is only %d bytes; it must exceed the default 1 MiB bound", len(body))
	}
	if len(body) > api.MaxYAMLRequestBytes {
		t.Fatalf("tools/call body is %d bytes; it must fit the 2 MiB YAML bound", len(body))
	}

	router := &fakeRouter{}
	edge := newEdge(t, router)
	response := decodeResponse(t, edge.Handle(context.Background(), "token-1", body))
	if response.Error != nil {
		t.Fatalf("inline start refused: %+v", response.Error)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response is invalid: %v", err)
	}
	invocation := router.lastInvocation()
	if string(invocation.Operation) != "pipeline.start" {
		t.Fatalf("operation = %q; want pipeline.start", invocation.Operation)
	}
	if invocation.Credential != "token-1" {
		t.Fatalf("credential = %q; want token-1", invocation.Credential)
	}
	if invocation.Request.ExpectedVersion != nil {
		t.Fatalf("inline start carried expected_version %d; want none", *invocation.Request.ExpectedVersion)
	}

	value, err := api.DecodeMCPOperationRequest("pipeline.start", invocation.Request.Input)
	if err != nil {
		t.Fatalf("decode forwarded inline start input: %v", err)
	}
	decoded := value.(api.PipelineStartRequest)
	if decoded.ProjectID != "project-a" || decoded.PipelineID != "" {
		t.Fatalf("decoded selection = project %q pipeline %q; want project only", decoded.ProjectID, decoded.PipelineID)
	}
	if !bytes.Equal(decoded.YAML, document) {
		t.Fatalf("decoded inline YAML differs from the exact %d-byte document (got %d bytes)", len(document), len(decoded.YAML))
	}
}

// TestMCPInlineStartCRLFBase64 pins that CR/LF line wrapping of the base64
// document is accepted and the exact decoded bytes reach the caller unchanged.
func TestMCPInlineStartCRLFBase64(t *testing.T) {
	document := bytes.Repeat([]byte("p"), api.MaxInlineYAMLBytes)
	body := toolsCallRequest(t, 1, "pipeline.start", inlineStartArguments(t, document, true))
	if len(body) <= api.MaxRequestBytes {
		t.Fatalf("tools/call body is only %d bytes; it must exceed the default 1 MiB bound", len(body))
	}

	router := &fakeRouter{}
	edge := newEdge(t, router)
	response := decodeResponse(t, edge.Handle(context.Background(), "token-1", body))
	if response.Error != nil {
		t.Fatalf("CRLF-padded inline start refused: %+v", response.Error)
	}
	invocation := router.lastInvocation()
	value, err := api.DecodeMCPOperationRequest("pipeline.start", invocation.Request.Input)
	if err != nil {
		t.Fatalf("decode forwarded CRLF-padded inline start: %v", err)
	}
	if !bytes.Equal(value.(api.PipelineStartRequest).YAML, document) {
		t.Fatal("CRLF-padded base64 did not decode to the exact document bytes")
	}
}

// TestMCPToolsCallYAMLEnvelopeBound pins the MCP envelope rule: a hard 2 MiB
// cap plus the named operation's api.MaxEnvelopeBytes bound, accepting exactly
// the bound and refusing one byte more.
func TestMCPToolsCallYAMLEnvelopeBound(t *testing.T) {
	cases := []struct {
		operation string
		bound     int
	}{
		{"pipeline.apply", api.MaxYAMLRequestBytes},
		{"pipeline.update", api.MaxYAMLRequestBytes},
		{"pipeline.start", api.MaxYAMLRequestBytes},
		{"task.read", api.MaxRequestBytes},
	}

	for _, testCase := range cases {
		if got := api.MaxEnvelopeBytes(testCase.operation); got != testCase.bound {
			t.Fatalf("MaxEnvelopeBytes(%q) = %d; want %d", testCase.operation, got, testCase.bound)
		}
		base := toolsCallRequest(t, 1, testCase.operation, json.RawMessage(`{"input":{}}`))

		t.Run(testCase.operation+" exactly at the bound", func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
				paddedMCPMessage(t, base, testCase.bound)))
			if response.Error != nil {
				t.Fatalf("exactly-bounded request refused: %+v", response.Error)
			}
			if err := response.Validate(); err != nil {
				t.Fatalf("response is invalid: %v", err)
			}
			if got := router.callCount(); got != 1 {
				t.Fatalf("router calls = %d; want 1", got)
			}
			if got := string(router.lastInvocation().Operation); got != testCase.operation {
				t.Fatalf("operation = %q; want %q", got, testCase.operation)
			}
		})

		t.Run(testCase.operation+" one byte over the bound", func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
				paddedMCPMessage(t, base, testCase.bound+1)))
			if response.Error == nil || response.Error.Code != -32602 {
				t.Fatalf("error = %+v; want MCP invalid_input -32602 one byte over %d", response.Error, testCase.bound)
			}
			if got := decodeErrorData(t, response); got.Code != api.CodeInvalidInput {
				t.Fatalf("public code = %q; want %q", got.Code, api.CodeInvalidInput)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestMCPPipelineStartOneOfParity pins that the MCP catalog advertises the
// declared pipeline.start oneOf (pipeline_id or yaml) and that no other tool
// advertises a oneOf, matching api.OperationInputSchema.
func TestMCPPipelineStartOneOfParity(t *testing.T) {
	catalog := mcp.ToolCatalog()
	if len(catalog) != 58 {
		t.Fatalf("catalog tools = %d; want 58", len(catalog))
	}

	found := 0
	for _, tool := range catalog {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s input schema: %v", tool.Name, err)
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(schema.Properties["input"], &input); err != nil {
			t.Fatalf("%s input member: %v", tool.Name, err)
		}
		rawOneOf, hasOneOf := input["oneOf"]
		if tool.Name != "pipeline_start" {
			if hasOneOf {
				t.Fatalf("%s advertises an unexpected oneOf: %s", tool.Name, rawOneOf)
			}
			continue
		}

		found++
		var branches []struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(rawOneOf, &branches); err != nil {
			t.Fatalf("pipeline.start oneOf: %v", err)
		}
		if len(branches) != 2 {
			t.Fatalf("pipeline.start oneOf has %d branches; want 2", len(branches))
		}
		names := make([]string, 0, 2)
		for _, branch := range branches {
			if len(branch.Required) != 1 {
				t.Fatalf("pipeline.start oneOf branch requires %v; want exactly one member", branch.Required)
			}
			names = append(names, branch.Required[0])
		}
		sort.Strings(names)
		if !reflect.DeepEqual(names, []string{"pipeline_id", "yaml"}) {
			t.Fatalf("pipeline.start oneOf selects %v; want pipeline_id or yaml", names)
		}

		var properties struct {
			YAML map[string]any `json:"yaml"`
		}
		if err := json.Unmarshal(input["properties"], &properties); err != nil {
			t.Fatalf("pipeline.start input properties: %v", err)
		}
		if got, ok := properties.YAML["maxLength"].(float64); !ok || int(got) != api.MaxInlineYAMLEncodedBytes {
			t.Fatalf("pipeline.start yaml maxLength = %v; want %d", properties.YAML["maxLength"], api.MaxInlineYAMLEncodedBytes)
		}
		if got := properties.YAML["format"]; got != "byte" {
			t.Fatalf("pipeline.start yaml format = %v; want byte", got)
		}
	}
	if found != 1 {
		t.Fatalf("pipeline.start tools = %d; want exactly 1", found)
	}
}
