// Package mcp_test pins the MCP edge API and the behavior these tests require.
//
// # Exported API
//
//	type Router interface {
//	    Invoke(context.Context, ops.Invocation) (api.Result, error)
//	}
//	type Tool struct {
//	    Name        string          `json:"name"`
//	    Description string          `json:"description,omitempty"`
//	    InputSchema json.RawMessage `json:"inputSchema"`
//	}
//	func ToolCatalog() []Tool
//	type Config struct {
//	    Router     Router
//	    Principals authorize.CurrentPrincipal
//	    ServerInfo api.MCPImplementation
//	}
//	func NewEdge(Config) (*Edge, error)
//	func (*Edge) Handle(context.Context, string, []byte) []byte
//
// # Pinned behavior
//
// The MCP edge is a thin JSON-RPC 2.0 transport over the shared router. It
// supports initialize (with embedded Kodema instructions), ping, tools/call,
// tools/list, resources/list, resources/templates/list, resources/read,
// prompts/list, and prompts/get; every other method refuses with the stable
// invalid_input mapping (-32602).
// When Principals is configured, initialize, tools/list, resources/*, and
// prompts/* authenticate the presented credential before returning server
// metadata, tool schemas, or embedded documentation, while never calling the
// router. initialize echoes the negotiated MCP protocol version and the
// configured server info. tools/call names a registry operation and passes
// either the canonical api.Request envelope or flat top-level operation fields
// as arguments to the router with the presented credential; the success result
// is an MCP CallToolResult carrying the JSON api.Result, domain/authorization
// refusals return an MCP CallToolResult with isError=true carrying the stable
// api.Error in text content and structuredContent, and protocol,
// unauthenticated and internal failures return JSON-RPC errors carrying the
// declared api.MCPErrorCode mapping plus the api.Error body in data.
// tools/list returns exactly ToolCatalog(), whose entries are the 58 registry
// operations in registry order with InputSchema the field-level
// api.OperationInputSchema object: the strict shared envelope that keeps the
// {"type":"object","title":"<operation>.input.v1"} identity, refuses unknown
// envelope and input members and types the operation's request DTO.
//
// The edge enforces the YAML-aware request bound (a hard 2 MiB cap and then the
// named operation's api.MaxEnvelopeBytes bound for tools/call, the declared 1
// MiB bound for every other method) and the api.MaxResponseBytes result bound,
// preserves Replayed, and never renders a private fault cause or a credential
// sentinel.
package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/mcp"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

type fakeRouter struct {
	mu    sync.Mutex
	fn    func(context.Context, ops.Invocation) (api.Result, error)
	calls int
	last  ops.Invocation
}

func (fake *fakeRouter) Invoke(ctx context.Context, invocation ops.Invocation) (api.Result, error) {
	fake.mu.Lock()
	fake.calls++
	fake.last = invocation
	fn := fake.fn
	fake.mu.Unlock()
	if fn != nil {
		return fn(ctx, invocation)
	}
	return api.Result{
		Operation: string(invocation.Operation),
		Body:      json.RawMessage(`{"ok":true}`),
	}, nil
}

func (fake *fakeRouter) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeRouter) lastInvocation() ops.Invocation {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.last
}

func decodeResponse(t *testing.T, body []byte) api.MCPResponse {
	t.Helper()
	var response api.MCPResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode MCP response %q: %v", string(body), err)
	}
	return response
}

func decodeErrorData(t *testing.T, response api.MCPResponse) api.Error {
	t.Helper()
	if response.Error == nil {
		t.Fatalf("response has no error: %+v", response)
	}
	publicError, err := api.DecodeMCPErrorData(response.Error.Data)
	if err != nil {
		t.Fatalf("decode MCP error data: %v", err)
	}
	return publicError
}

func initializeRequest(protocolVersion string) []byte {
	return []byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"test-client","version":"1"}}}`,
		protocolVersion))
}

func toolsCallRequest(t *testing.T, id int, name string, arguments json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		t.Fatalf("marshal tools/call request: %v", err)
	}
	return body
}

func toolsListRequest() []byte {
	return []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
}

// requestBodyOfSize builds a valid initialize request padded with insignificant
// JSON whitespace to an exact byte size.
func requestBodyOfSize(t *testing.T, size int) []byte {
	t.Helper()
	base := initializeRequest(api.MCPProtocolVersion)
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

// resultOfEncodedSize builds an api.Result whose exact JSON encoding is size
// bytes.
func resultOfEncodedSize(t *testing.T, operation string, size int) api.Result {
	t.Helper()
	result := api.Result{Operation: operation, Body: json.RawMessage(`{"pad":""}`)}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal base result: %v", err)
	}
	if size < len(raw) {
		t.Fatalf("result size %d is smaller than the minimal encoding %d", size, len(raw))
	}
	padding := size - len(raw)
	result.Body = json.RawMessage(fmt.Sprintf(`{"pad":"%s"}`, strings.Repeat("x", padding)))
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal padded result: %v", err)
	}
	if len(raw) != size {
		t.Fatalf("encoded result = %d bytes; want %d", len(raw), size)
	}
	return result
}

type fakePrincipals struct {
	mu    sync.Mutex
	fn    func(context.Context, string) (authorize.Principal, error)
	calls int
	last  string
}

func (fake *fakePrincipals) Resolve(ctx context.Context, credential string) (authorize.Principal, error) {
	fake.mu.Lock()
	fake.calls++
	fake.last = credential
	fn := fake.fn
	fake.mu.Unlock()
	if fn != nil {
		return fn(ctx, credential)
	}
	if credential == "" {
		return authorize.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	return authorize.Principal{
		ID:      "human-1",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-1"},
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}, nil
}

func (fake *fakePrincipals) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func newEdge(t *testing.T, router *fakeRouter) *mcp.Edge {
	t.Helper()
	edge, err := mcp.NewEdge(mcp.Config{
		Router:     router,
		ServerInfo: api.MCPImplementation{Name: "kodema", Version: "test"},
	})
	if err != nil {
		t.Fatalf("mcp.NewEdge: %v", err)
	}
	return edge
}

func newAuthenticatedEdge(t *testing.T, router *fakeRouter, principals authorize.CurrentPrincipal) *mcp.Edge {
	t.Helper()
	edge, err := mcp.NewEdge(mcp.Config{
		Router:     router,
		Principals: principals,
		ServerInfo: api.MCPImplementation{Name: "kodema", Version: "0.4.0"},
	})
	if err != nil {
		t.Fatalf("mcp.NewEdge: %v", err)
	}
	return edge
}
