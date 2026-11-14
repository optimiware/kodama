package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

func TestMCPInitializePinsProtocolVersionAndServerInfo(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	responseBytes := edge.Handle(context.Background(), "", initializeRequest(api.MCPProtocolVersion))
	response := decodeResponse(t, responseBytes)
	if err := response.Validate(); err != nil {
		t.Fatalf("initialize response is invalid: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("initialize refused: %+v", response.Error)
	}

	var result api.MCPInitializeResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("initialize result is invalid: %v", err)
	}
	if result.ProtocolVersion != api.MCPProtocolVersion {
		t.Fatalf("protocol version = %q; want %q", result.ProtocolVersion, api.MCPProtocolVersion)
	}
	if result.ServerInfo.Name != "kodama" || result.ServerInfo.Version != "test" {
		t.Fatalf("server info = %+v; want the configured identity", result.ServerInfo)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0 for initialize", got)
	}
}

func TestMCPInitializeNegotiatesProtocolVersion(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	for _, version := range api.SupportedMCPProtocolVersions() {
		t.Run(version, func(t *testing.T) {
			response := decodeResponse(t, edge.Handle(context.Background(), "", initializeRequest(version)))
			if err := response.Validate(); err != nil {
				t.Fatalf("initialize response is invalid: %v", err)
			}
			if response.Error != nil {
				t.Fatalf("initialize(%s) refused: %+v", version, response.Error)
			}
			var result api.MCPInitializeResult
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatalf("decode initialize result: %v", err)
			}
			if result.ProtocolVersion != version {
				t.Fatalf("protocolVersion = %q; want echoed %q", result.ProtocolVersion, version)
			}
		})
	}

	// An unknown 202x-xx-xx revision falls back to MCPProtocolVersion per MCP
	// version negotiation rules.
	fallback := decodeResponse(t, edge.Handle(context.Background(), "", initializeRequest("2026-01-15")))
	if fallback.Error != nil {
		t.Fatalf("fallback initialize refused: %+v", fallback.Error)
	}
	var fallbackResult api.MCPInitializeResult
	if err := json.Unmarshal(fallback.Result, &fallbackResult); err != nil {
		t.Fatalf("decode fallback initialize result: %v", err)
	}
	if fallbackResult.ProtocolVersion != api.MCPProtocolVersion {
		t.Fatalf("fallback protocolVersion = %q; want %q", fallbackResult.ProtocolVersion, api.MCPProtocolVersion)
	}

	for _, bad := range []string{"", "1.0", "not-a-date", "2025-99-99", "1999-01-01", "2025-+1-+1"} {
		t.Run("bad_"+bad, func(t *testing.T) {
			response := decodeResponse(t, edge.Handle(context.Background(), "", initializeRequest(bad)))
			if err := response.Validate(); err != nil {
				t.Fatalf("error response is invalid: %v", err)
			}
			if response.Error == nil {
				t.Fatalf("malformed protocol version %q was accepted", bad)
			}
			if response.Error.Code != -32602 {
				t.Fatalf("MCP error code = %d; want -32602", response.Error.Code)
			}
			if got := decodeErrorData(t, response); got.Code != api.CodeInvalidInput {
				t.Fatalf("public code = %q; want %q", got.Code, api.CodeInvalidInput)
			}
		})
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPNotificationsAndPing(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user cancelled"}}`,
	} {
		out := edge.Handle(context.Background(), "", []byte(body))
		if len(out) != 0 {
			t.Fatalf("notification %s returned %q; want empty slice", body, string(out))
		}
	}

	for _, body := range []string{
		`{"jsonrpc":"2.0","id":9,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"p-1","method":"ping","params":{}}`,
	} {
		response := decodeResponse(t, edge.Handle(context.Background(), "", []byte(body)))
		if err := response.Validate(); err != nil {
			t.Fatalf("ping response invalid: %v", err)
		}
		if response.Error != nil {
			t.Fatalf("ping refused: %+v", response.Error)
		}
		if string(response.Result) != `{}` {
			t.Fatalf("ping result = %s; want {}", string(response.Result))
		}
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPToolsCallAcceptsUnderscoreAndDottedToolNamesAndEncodesCallToolResult(t *testing.T) {
	cases := []struct {
		toolName  string
		canonical string
	}{
		{"task_inbox", "task.inbox"},
		{"task.inbox", "task.inbox"},
		{"task_create_and_start", "task.create_and_start"},
		{"resource_binding_read", "resource.binding.read"},
		{"principal_whoami", "principal.whoami"},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			responseBytes := edge.Handle(context.Background(), "token-1",
				toolsCallRequest(t, 1, tc.toolName, json.RawMessage(`{"input":{},"idempotency_key":"k-1"}`)))
			response := decodeResponse(t, responseBytes)
			if response.Error != nil {
				t.Fatalf("%s refused: %+v", tc.toolName, response.Error)
			}
			var callResult api.MCPCallToolResult
			if err := json.Unmarshal(response.Result, &callResult); err != nil {
				t.Fatalf("decode CallToolResult: %v", err)
			}
			if callResult.IsError || len(callResult.Content) != 1 || callResult.Content[0].Type != "text" {
				t.Fatalf("CallToolResult = %+v; want single text content and isError=false", callResult)
			}
			if len(callResult.StructuredContent) == 0 {
				t.Fatal("CallToolResult structuredContent is empty")
			}
			decoded, err := api.DecodeMCPResult(response.Result)
			if err != nil {
				t.Fatalf("DecodeMCPResult: %v", err)
			}
			if decoded.Operation != tc.canonical {
				t.Fatalf("decoded operation = %q; want %q", decoded.Operation, tc.canonical)
			}
			if string(router.lastInvocation().Operation) != tc.canonical {
				t.Fatalf("routed operation = %q; want %q", router.lastInvocation().Operation, tc.canonical)
			}
		})
	}
}

func TestMCPToolsCallRoutesOperation(t *testing.T) {
	cases := []string{
		"task.read",
		"execution.action.outputs",
		"actor.verify",
		"service.credential.issue",
	}

	for _, operation := range cases {
		t.Run(operation, func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			request := api.Request{
				Input:          json.RawMessage(`{"id":"value-1"}`),
				IdempotencyKey: "key-1",
			}
			arguments, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}

			responseBytes := edge.Handle(context.Background(), "token-1",
				toolsCallRequest(t, 1, operation, arguments))
			response := decodeResponse(t, responseBytes)
			if err := response.Validate(); err != nil {
				t.Fatalf("response is invalid: %v", err)
			}
			if response.Error != nil {
				t.Fatalf("tools/call refused: %+v", response.Error)
			}
			result, err := api.DecodeMCPResult(response.Result)
			if err != nil {
				t.Fatalf("decode tools/call result: %v", err)
			}
			if result.Operation != operation {
				t.Fatalf("result operation = %q; want %q", result.Operation, operation)
			}

			invocation := router.lastInvocation()
			if string(invocation.Operation) != operation {
				t.Fatalf("routed operation = %q; want %q", invocation.Operation, operation)
			}
			if invocation.Credential != "token-1" {
				t.Fatalf("credential = %q; want token-1", invocation.Credential)
			}
			if string(invocation.Request.Input) != string(request.Input) {
				t.Fatalf("request input = %s; want %s", invocation.Request.Input, request.Input)
			}
			if invocation.Request.IdempotencyKey != request.IdempotencyKey {
				t.Fatalf("idempotency key = %q; want %q", invocation.Request.IdempotencyKey, request.IdempotencyKey)
			}
		})
	}
}

func TestMCPToolsCallRefusesUnknownOperation(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
		toolsCallRequest(t, 1, "task.unknown", json.RawMessage(`{}`))))
	if err := response.Validate(); err != nil {
		t.Fatalf("error response is invalid: %v", err)
	}
	if response.Error == nil || response.Error.Code != -32602 {
		t.Fatalf("error = %+v; want MCP invalid_input -32602", response.Error)
	}
	if got := decodeErrorData(t, response); got.Code != api.CodeInvalidInput {
		t.Fatalf("public code = %q; want %q", got.Code, api.CodeInvalidInput)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPToolsCallRefusesMalformedArguments(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "non-object arguments",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","arguments":[]}}`,
		},
		{
			name: "non-object input",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","arguments":{"input":[]}}}`,
		},
		{
			name: "unknown request field",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","arguments":{"unknown":1}}}`,
		},
		{
			name: "unknown parameter field",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","extra":true}}`,
		},
		{
			name: "wrong jsonrpc version",
			body: `{"jsonrpc":"1.0","id":1,"method":"tools/call","params":{"name":"task.read"}}`,
		},
		{
			name: "missing id",
			body: `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"task.read"}}`,
		},
		{
			name: "string arguments",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task.read","arguments":"text"}}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			response := decodeResponse(t, edge.Handle(context.Background(), "token-1", []byte(testCase.body)))
			if err := response.Validate(); err != nil {
				t.Fatalf("error response is invalid: %v", err)
			}
			if response.Error == nil || response.Error.Code != -32602 {
				t.Fatalf("error = %+v; want MCP invalid_input -32602", response.Error)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

func TestMCPUnsupportedMethodRefused(t *testing.T) {
	for _, method := range []string{"notifications/initialized", "unknown/method"} {
		t.Run(method, func(t *testing.T) {
			router := &fakeRouter{}
			edge := newEdge(t, router)
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`)
			response := decodeResponse(t, edge.Handle(context.Background(), "", body))
			if err := response.Validate(); err != nil {
				t.Fatalf("error response is invalid: %v", err)
			}
			if response.Error == nil || response.Error.Code != -32602 {
				t.Fatalf("error = %+v; want MCP invalid_input -32602", response.Error)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestMCPErrorParityAllPublicCodes pins the declared public-code to MCP error
// mapping and the api.Error data body for every code.
func TestMCPErrorParityAllPublicCodes(t *testing.T) {
	for _, code := range api.ErrorCodes() {
		t.Run(code, func(t *testing.T) {
			router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
				return api.Result{}, fault.New(fault.Class(code), "public message")
			}}
			edge := newEdge(t, router)
			response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
				toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`))))
			if err := response.Validate(); err != nil {
				t.Fatalf("error response is invalid: %v", err)
			}
			if response.Error == nil {
				t.Fatal("refusal produced no MCP error")
			}
			wantCode, ok := api.MCPErrorCode(code)
			if !ok {
				t.Fatalf("no MCP error code for public code %q", code)
			}
			if response.Error.Code != wantCode {
				t.Fatalf("MCP error code = %d; want %d", response.Error.Code, wantCode)
			}
			if response.Error.Message != "public message" {
				t.Fatalf("MCP error message = %q; want the public message", response.Error.Message)
			}
			publicError := decodeErrorData(t, response)
			if publicError.Code != code || publicError.Message != "public message" {
				t.Fatalf("error data = %+v; want the public code/message", publicError)
			}
		})
	}
}

// TestMCPRequestBound pins the inclusive 1 MiB request bound and the +1
// refusal.
func TestMCPRequestBound(t *testing.T) {
	t.Run("exactly the bound", func(t *testing.T) {
		router := &fakeRouter{}
		edge := newEdge(t, router)
		response := decodeResponse(t, edge.Handle(context.Background(), "",
			requestBodyOfSize(t, api.MaxRequestBytes)))
		if err := response.Validate(); err != nil {
			t.Fatalf("response is invalid: %v", err)
		}
		if response.Error != nil {
			t.Fatalf("exactly-bounded request refused: %+v", response.Error)
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0 for initialize", got)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		router := &fakeRouter{}
		edge := newEdge(t, router)
		response := decodeResponse(t, edge.Handle(context.Background(), "",
			requestBodyOfSize(t, api.MaxRequestBytes+1)))
		if response.Error == nil || response.Error.Code != -32602 {
			t.Fatalf("error = %+v; want MCP invalid_input -32602", response.Error)
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0", got)
		}
	})
}

// TestMCPResultBound pins the inclusive 8 MiB encoded result bound and the +1
// refusal.
func TestMCPResultBound(t *testing.T) {
	t.Run("exactly the bound", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return resultOfEncodedSize(t, "task.read", api.MaxResponseBytes), nil
		}}
		edge := newEdge(t, router)
		response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
			toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`))))
		if response.Error != nil {
			t.Fatalf("exactly-bounded result refused: %+v", response.Error)
		}
		result, err := api.DecodeMCPResult(response.Result)
		if err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.Operation != "task.read" {
			t.Fatalf("result operation = %q; want task.read", result.Operation)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return resultOfEncodedSize(t, "task.read", api.MaxResponseBytes+1), nil
		}}
		edge := newEdge(t, router)
		response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
			toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`))))
		if response.Error == nil || response.Error.Code != -32603 {
			t.Fatalf("error = %+v; want MCP internal_failure -32603", response.Error)
		}
		if got := decodeErrorData(t, response); got.Code != api.CodeInternalFailure {
			t.Fatalf("public code = %q; want %q", got.Code, api.CodeInternalFailure)
		}
	})
}

// TestMCPSanitizesPrivateCauses pins that a private fault cause and an
// unexpected handler error are never rendered in the MCP message or data.
func TestMCPSanitizesPrivateCauses(t *testing.T) {
	t.Run("fault cause", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return api.Result{}, fault.Wrap(fault.Unauthenticated, "access token verification failed",
				errors.New("SECRET-SENTINEL"))
		}}
		edge := newEdge(t, router)
		responseBytes := edge.Handle(context.Background(), "token-1",
			toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`)))
		if strings.Contains(string(responseBytes), "SECRET-SENTINEL") {
			t.Fatalf("response rendered the private cause: %s", string(responseBytes))
		}
		response := decodeResponse(t, responseBytes)
		if response.Error == nil || response.Error.Code != -32001 {
			t.Fatalf("error = %+v; want unauthenticated -32001", response.Error)
		}
		if response.Error.Message != "access token verification failed" {
			t.Fatalf("message = %q; want the public message", response.Error.Message)
		}
	})

	t.Run("non-fault error", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return api.Result{}, errors.New("SECRET-SENTINEL")
		}}
		edge := newEdge(t, router)
		responseBytes := edge.Handle(context.Background(), "token-1",
			toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`)))
		if strings.Contains(string(responseBytes), "SECRET-SENTINEL") {
			t.Fatalf("response rendered the handler error: %s", string(responseBytes))
		}
		response := decodeResponse(t, responseBytes)
		if response.Error == nil || response.Error.Code != -32603 {
			t.Fatalf("error = %+v; want internal_failure -32603", response.Error)
		}
	})
}

// TestMCPCredentialForwardedAndNotLeaked pins that the presented credential is
// handed to the router and never echoed in an error response.
func TestMCPCredentialForwardedAndNotLeaked(t *testing.T) {
	const credential = "token-SECRET-1"
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{}, fault.Wrap(fault.Unauthenticated, "access token verification failed",
			errors.New(credential))
	}}
	edge := newEdge(t, router)

	responseBytes := edge.Handle(context.Background(), credential,
		toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`)))
	if got := router.lastInvocation().Credential; got != credential {
		t.Fatalf("router credential = %q; want %q", got, credential)
	}
	if strings.Contains(string(responseBytes), credential) {
		t.Fatalf("response echoed the credential: %s", string(responseBytes))
	}
}

// TestMCPReplayedPreserved pins that the transport does not strip the replay
// fact from a capability result.
func TestMCPReplayedPreserved(t *testing.T) {
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{
			Operation: "task.read",
			Body:      json.RawMessage(`{"title":"task"}`),
			Replayed:  true,
		}, nil
	}}
	edge := newEdge(t, router)
	response := decodeResponse(t, edge.Handle(context.Background(), "token-1",
		toolsCallRequest(t, 1, "task.read", json.RawMessage(`{}`))))
	if response.Error != nil {
		t.Fatalf("tools/call refused: %+v", response.Error)
	}
	result, err := api.DecodeMCPResult(response.Result)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Replayed {
		t.Fatal("Replayed was not preserved")
	}
	if result.Operation != "task.read" || string(result.Body) != `{"title":"task"}` {
		t.Fatalf("result = %+v; want the original operation and body", result)
	}
}

// TestMCPMalformedJSON pins that an unparseable message produces a JSON-RPC
// error with no fabricated request id.
func TestMCPMalformedJSON(t *testing.T) {
	for _, body := range []string{"", "not json"} {
		router := &fakeRouter{}
		edge := newEdge(t, router)
		responseBytes := edge.Handle(context.Background(), "", []byte(body))

		var response map[string]any
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			t.Fatalf("malformed response %q: %v", string(responseBytes), err)
		}
		errorObject, ok := response["error"].(map[string]any)
		if !ok {
			t.Fatalf("response has no error object: %s", string(responseBytes))
		}
		if code, ok := errorObject["code"].(float64); !ok || int(code) != -32602 {
			t.Fatalf("error code = %v; want -32602", errorObject["code"])
		}
		if id, present := response["id"]; present && id != nil {
			t.Fatalf("malformed request response carries id %v; want none", id)
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0", got)
		}
	}
}

func TestMCPNewEdgeRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		config mcp.Config
	}{
		{"nil router", mcp.Config{ServerInfo: api.MCPImplementation{Name: "kodama", Version: "test"}}},
		{"empty server info", mcp.Config{Router: &fakeRouter{}}},
		{"empty server name", mcp.Config{Router: &fakeRouter{}, ServerInfo: api.MCPImplementation{Version: "test"}}},
		{"empty server version", mcp.Config{Router: &fakeRouter{}, ServerInfo: api.MCPImplementation{Name: "kodama"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := mcp.NewEdge(testCase.config); err == nil {
				t.Fatalf("NewEdge accepted %s", testCase.name)
			}
		})
	}
}
