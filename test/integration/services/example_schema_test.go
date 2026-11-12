//go:build integration

package services

// Every shipped and documented example agrees with the schema catalog the
// served API publishes.
//
// The catalog is fetched from the running factory composition (MCP tools/list),
// not reconstructed from a second expectation. Each request example is then
// checked against that published field-level schema and against the served
// MCP-mode decoder; each response example crosses the served result codec. A
// product schema mutation and an example mutation must both fail here.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestExamplesMatchServedSchema validates every shipped and documented example
// against the live catalog and the served codecs.
func TestExamplesMatchServedSchema(t *testing.T) {
	harness := factory.Start(t)
	tools := examplePublishedTools(t, harness)

	t.Run("pipeline invocations", func(t *testing.T) {
		testExamplePipelineInvocations(t, tools)
	})
	t.Run("documented routes", func(t *testing.T) {
		testDocumentedRoutes(t, tools)
	})
	t.Run("shipped wire examples", func(t *testing.T) {
		testShippedWireExamples(t, tools)
	})
}

// testExamplePipelineInvocations checks every `kodactl call` in every shipped
// pipeline against the served schema of that operation.
func testExamplePipelineInvocations(t *testing.T, tools map[string]api.MCPTool) {
	t.Helper()
	invoked := make(map[string]bool)
	invocations := 0
	for _, name := range examplePipelineFiles(t) {
		rel := "examples/pipelines/" + name
		t.Run(name, func(t *testing.T) {
			decoded := exampleDecodedPipeline(t, rel, exampleRead(t, rel))
			for _, invocation := range exampleInvocations(t, rel, examplePipelineCommands(decoded)) {
				tool, published := tools[invocation.Operation]
				if !published {
					t.Fatalf("%s invokes `%s`, which the served API does not publish", rel, invocation.Operation)
				}
				exampleValidateInput(t, tool, invocation.Operation, invocation.Input)
				invoked[invocation.Operation] = true
				invocations++
			}
		})
	}
	if invocations == 0 {
		t.Fatal("no example invokes a served operation; the pipeline example set is empty or the extractor missed every call")
	}
	for _, required := range []string{"task.read", "task.ask"} {
		if !invoked[required] {
			t.Fatalf("the shipped examples no longer exercise %q; the documented request/response examples regressed", required)
		}
	}
}

// testDocumentedRoutes checks every documented method/path example against the
// served route table.
func testDocumentedRoutes(t *testing.T, tools map[string]api.MCPTool) {
	t.Helper()
	cases := []struct {
		path     string
		required []string
	}{
		{
			path: "docs/specs/08-operations-events.md",
			required: []string{
				"execution.action.outputs",
				"execution.request.resolve",
				"task.ask",
				"task.reply",
				"actor.verify",
				"execution.inspect",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.path, func(t *testing.T) {
			seen := make(map[string]bool)
			for _, row := range exampleDocumentedRoutes(t, testCase.path) {
				route, _, ok := api.Match(api.Method(row.Method), row.Path)
				if !ok {
					t.Fatalf("%s documents %s %s, which the served route table does not match",
						testCase.path, row.Method, row.Path)
				}
				if row.Operation != "" && route.Operation != row.Operation {
					t.Fatalf("%s documents %s %s as %q; the served route is %q",
						testCase.path, row.Method, row.Path, row.Operation, route.Operation)
				}
				if _, published := tools[route.Operation]; !published {
					t.Fatalf("%s documents %s, which the served API does not publish",
						testCase.path, route.Operation)
				}
				seen[route.Operation] = true
			}
			for _, operation := range testCase.required {
				if !seen[operation] {
					t.Fatalf("%s no longer documents the %q route", testCase.path, operation)
				}
			}
		})
	}
}

// testShippedWireExamples checks the shipped public request/response and
// capability testdata against the served schema catalog and codecs.
func testShippedWireExamples(t *testing.T, tools map[string]api.MCPTool) {
	t.Helper()
	vectors := exampleLoadWireVectors(t)
	if len(vectors) != 48 {
		t.Fatalf("test/contracts/public/testdata/wire/operations.json has %d request/response examples; want 48", len(vectors))
	}
	seen := make(map[string]bool)
	for _, vector := range vectors {
		t.Run(vector.Operation, func(t *testing.T) {
			tool, published := tools[vector.Operation]
			if !published {
				t.Fatalf("the shipped %q request/response example names an operation the served API does not publish", vector.Operation)
			}
			exampleValidateInput(t, tool, vector.Operation, vector.Request)
			codec := exampleCodec(t, vector.Operation)
			if _, err := codec.DecodeResult(vector.Result); err != nil {
				t.Fatalf("the shipped %q response example is refused by the served result codec: %v; result=%s",
					vector.Operation, err, string(vector.Result))
			}
			seen[vector.Operation] = true
		})
	}
	if len(seen) != len(vectors) {
		t.Fatalf("the shipped request/response examples repeat operations: %d distinct of %d", len(seen), len(vectors))
	}

	// Capability example: the documented OpenAI-compatible tool-calling request
	// and response must satisfy the served model.invoke schema and codec.
	var modelExample struct {
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(exampleRead(t, "test/contracts/public/testdata/wire/model-tools.json"), &modelExample); err != nil {
		t.Fatalf("decode model tool-calling example: %v", err)
	}
	if !bytes.Contains(modelExample.Request, []byte(`"tools"`)) || !bytes.Contains(modelExample.Request, []byte(`"tool_calls"`)) {
		t.Fatal("the model tool-calling example no longer declares tools and assistant tool_calls")
	}
	if !bytes.Contains(modelExample.Response, []byte(`"tool_calls"`)) {
		t.Fatal("the model tool-calling response example no longer returns tool_calls")
	}
	var completion map[string]any
	decoder := json.NewDecoder(bytes.NewReader(modelExample.Request))
	decoder.UseNumber()
	if err := decoder.Decode(&completion); err != nil {
		t.Fatalf("decode model tool-calling request: %v", err)
	}
	completion["project_id"] = factory.ProjectID
	servedRequest, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("render model tool-calling request: %v", err)
	}
	tool, published := tools["model.invoke"]
	if !published {
		t.Fatal("the served API does not publish model.invoke")
	}
	exampleValidateInput(t, tool, "model.invoke", servedRequest)
	modelName, _ := completion["model"].(string)
	if modelName == "" {
		t.Fatal("the documented model request carries no model name")
	}
	if _, err := api.DecodeRESTRequest("model.invoke", modelExample.Request, map[string]string{
		"project": factory.ProjectID,
		"model":   modelName,
	}); err != nil {
		t.Fatalf("the documented REST model.invoke request is refused: %v", err)
	}
	codec := exampleCodec(t, "model.invoke")
	if _, err := codec.DecodeResult(modelExample.Response); err != nil {
		t.Fatalf("the documented model.invoke response is refused by the served result codec: %v", err)
	}

	// Response example: the documented event envelope must satisfy the served
	// event.read result codec.
	var envelope api.Event
	if err := json.Unmarshal(exampleRead(t, "test/contracts/public/testdata/wire/event.json"), &envelope); err != nil {
		t.Fatalf("decode the documented event envelope: %v", err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("the documented event envelope is invalid: %v", err)
	}
	cursor, err := api.EncodeCursor("1")
	if err != nil {
		t.Fatalf("encode event cursor: %v", err)
	}
	page, err := json.Marshal(api.EventReadResult{Events: []api.Event{envelope}, Next: cursor})
	if err != nil {
		t.Fatalf("render the documented event read page: %v", err)
	}
	if _, err := exampleCodec(t, "event.read").DecodeResult(page); err != nil {
		t.Fatalf("the documented event envelope is refused by the served event.read result codec: %v", err)
	}
}
