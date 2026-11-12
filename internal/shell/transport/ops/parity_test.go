package ops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

var pathParameters = map[string]string{
	"tenant":    "tenant-a",
	"project":   "project-a",
	"principal": "principal-1",
	"binding":   "binding-1",
	"pipeline":  "pipeline-1",
	"secret":    "secret-1",
	"service":   "service-1",
	"task":      "task-1",
	"execution": "exec-1",
	"action":    "action-1",
	"model":     "model-1",
	"tool":      "tool-1",
}

func concretePath(template string) string {
	path := template
	for name, value := range pathParameters {
		path = strings.ReplaceAll(path, "{"+name+"}", value)
	}
	return path
}

// recorder is an in-process http.ResponseWriter; no server or httptest stack is
// used.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{header: make(http.Header)}
}

func (recorder *recorder) Header() http.Header { return recorder.header }

func (recorder *recorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
}

func (recorder *recorder) Write(data []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.body.Write(data)
}

// TestEveryRegistryOperationRoutesOnceThroughRESTAndMCP is the cross-edge
// parity check: the declared registry and the public route table agree
// one-to-one, and every operation reaches exactly one handler invocation
// through each edge.
func TestEveryRegistryOperationRoutesOnceThroughRESTAndMCP(t *testing.T) {
	operations := authorize.Operations()
	routes := api.Routes()

	if len(operations) != 51 {
		t.Fatalf("registry operations = %d; want 51", len(operations))
	}
	if len(routes) != len(operations) {
		t.Fatalf("routes = %d; want %d", len(routes), len(operations))
	}
	for index := range operations {
		if string(operations[index].Name) != routes[index].Operation {
			t.Fatalf("route %d = %q; want registry operation %q", index, routes[index].Operation, operations[index].Name)
		}
		if routes[index].MCPTool != string(operations[index].Name) {
			t.Fatalf("route %q MCP tool = %q; want the operation name", routes[index].Operation, routes[index].MCPTool)
		}
		if routes[index].InputSchema != string(operations[index].Name)+".input.v1" {
			t.Fatalf("route %q input schema = %q; want the schema-name convention", routes[index].Operation, routes[index].InputSchema)
		}
	}

	h := newHarness(t)
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

	for index, route := range routes {
		operation, ok := authorize.LookupOperation(authorize.OperationName(route.Operation))
		if !ok {
			t.Fatalf("route %q has no registry operation", route.Operation)
		}
		h.prepare(operation)

		request := api.Request{Input: json.RawMessage(`{}`)}
		if operation.Receipt == authorize.ReceiptRequired {
			request.IdempotencyKey = "key-" + route.Operation
		}

		restRequest, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal REST request for %s: %v", route.Operation, err)
		}
		httpRequest, err := http.NewRequest(string(route.Method), concretePath(route.Path), bytes.NewReader(restRequest))
		if err != nil {
			t.Fatalf("build REST request for %s: %v", route.Operation, err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+testToken)
		responseRecorder := newRecorder()
		restEdge.ServeHTTP(responseRecorder, httpRequest)
		if responseRecorder.status != http.StatusOK {
			t.Fatalf("REST %s %s status = %d; want 200 (body %s)",
				route.Method, concretePath(route.Path), responseRecorder.status, responseRecorder.body.String())
		}
		var restResult api.Result
		if err := json.Unmarshal(responseRecorder.body.Bytes(), &restResult); err != nil {
			t.Fatalf("decode REST result for %s: %v", route.Operation, err)
		}
		if err := restResult.Validate(); err != nil {
			t.Fatalf("REST result for %s is invalid: %v", route.Operation, err)
		}
		if restResult.Operation != route.Operation {
			t.Fatalf("REST result operation = %q; want %q", restResult.Operation, route.Operation)
		}

		mcpRequest, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      index + 1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      route.Operation,
				"arguments": request,
			},
		})
		if err != nil {
			t.Fatalf("marshal MCP request for %s: %v", route.Operation, err)
		}
		mcpResponseBytes := mcpEdge.Handle(context.Background(), testToken, mcpRequest)
		var mcpResponse api.MCPResponse
		if err := json.Unmarshal(mcpResponseBytes, &mcpResponse); err != nil {
			t.Fatalf("decode MCP response for %s: %v", route.Operation, err)
		}
		if err := mcpResponse.Validate(); err != nil {
			t.Fatalf("MCP response for %s is invalid: %v", route.Operation, err)
		}
		if mcpResponse.Error != nil {
			t.Fatalf("MCP %s refused: %+v", route.Operation, mcpResponse.Error)
		}
		var mcpResult api.Result
		if err := json.Unmarshal(mcpResponse.Result, &mcpResult); err != nil {
			t.Fatalf("decode MCP result for %s: %v", route.Operation, err)
		}
		if err := mcpResult.Validate(); err != nil {
			t.Fatalf("MCP result for %s is invalid: %v", route.Operation, err)
		}
		if mcpResult.Operation != route.Operation {
			t.Fatalf("MCP result operation = %q; want %q", mcpResult.Operation, route.Operation)
		}
	}

	for _, operation := range operations {
		if got := h.probeFor(operation.Name).handler.callCount(); got != 2 {
			t.Fatalf("handler calls for %s = %d; want exactly 2 (one per edge)", operation.Name, got)
		}
	}
}
