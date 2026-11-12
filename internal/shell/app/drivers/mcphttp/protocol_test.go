// Protocol behavior: the bounded MCP-over-HTTP driver uses the repository's
// JSON-RPC 2.0 tools/call framing, maps the standard MCP tool result to
// the proxy's {"content","isError"} response shape, enforces its response
// bound, and refuses malformed or mismatched upstream responses without
// replaying the tool call.
package mcphttp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/app/drivers/mcphttp"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
)

// TestMCPHTTPMapsToolResults pins the exact mapping from a standard MCP
// tools/call result to the mcpproxy.Transport response shape. A tool error is a
// settled result, not a transport failure.
func TestMCPHTTPMapsToolResults(t *testing.T) {
	cases := []struct {
		name        string
		result      string
		wantContent string
		wantIsError bool
	}{
		{
			name:        "success",
			result:      `{"content":[{"type":"text","text":"hello"}],"isError":false}`,
			wantContent: `[{"type":"text","text":"hello"}]`,
			wantIsError: false,
		},
		{
			name:        "tool error",
			result:      `{"content":{"message":"bad input"},"isError":true}`,
			wantContent: `{"message":"bad input"}`,
			wantIsError: true,
		},
		{
			name:        "absent isError defaults false",
			result:      `{"content":{"text":"ok"}}`,
			wantContent: `{"text":"ok"}`,
			wantIsError: false,
		},
		{
			name:        "null content is a present member",
			result:      `{"content":null}`,
			wantContent: `null`,
			wantIsError: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				var rpc struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.Unmarshal(body, &rpc)
				writer.Header().Set("Content-Type", api.MediaType)
				_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":`+testCase.result+`}`)
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp"
			transport := newTestTransport(t, testConfig(endpoint))

			body, err := transport.Call(context.Background(), testRequest(endpoint))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			response := decodeProxyResponse(t, body)
			if response.IsError != testCase.wantIsError {
				t.Fatalf("IsError = %v; want %v", response.IsError, testCase.wantIsError)
			}
			requireJSONEqual(t, response.Content, testCase.wantContent)
		})
	}
}

// TestMCPHTTPRejectsMalformedResponses pins the response framing. Every case is
// a response that arrived after the request was written, so the outcome is
// uncertain and the tool call is never replayed.
func TestMCPHTTPRejectsMalformedResponses(t *testing.T) {
	const validResult = `{"content":[],"isError":false}`
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{
			name:        "wrong jsonrpc version",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"1.0","id":{{id}},"result":` + validResult + `}`,
		},
		{
			name:        "missing response id",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","result":` + validResult + `}`,
		},
		{
			name:        "both result and error",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":` + validResult + `,"error":{"code":-32603,"message":"both"}}`,
		},
		{
			name:        "neither result nor error",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}}}`,
		},
		{
			name:        "jsonrpc error response",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"error":{"code":-32602,"message":"bad params"}}`,
		},
		{
			name:        "result without content",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":{"isError":false}}`,
		},
		{
			name:        "non-object result",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":[]}`,
		},
		{
			name:        "trailing content",
			status:      http.StatusOK,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":` + validResult + `}trailing`,
		},
		{
			name:        "wrong content type",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":` + validResult + `}`,
		},
		{
			name:        "non-success status",
			status:      http.StatusInternalServerError,
			contentType: api.MediaType,
			body:        `{"jsonrpc":"2.0","id":{{id}},"result":` + validResult + `}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(request.Body)
				var rpc struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.Unmarshal(body, &rpc)
				responseBody := strings.ReplaceAll(testCase.body, "{{id}}", string(rpc.ID))
				writer.Header().Set("Content-Type", testCase.contentType)
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, responseBody)
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp"
			transport := newTestTransport(t, testConfig(endpoint))

			body, err := transport.Call(context.Background(), testRequest(endpoint))
			if body != nil {
				_ = body.Close()
				t.Fatal("Call returned a response body for a malformed response")
			}
			requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream requests = %d; want exactly 1", got)
			}
		})
	}
}

// TestMCPHTTPResponseBound proves the exact configured response bound is
// accepted and one byte more is refused and closed. Trailing JSON whitespace is
// used only to make the response length exact; it is insignificant to JSON.
func TestMCPHTTPResponseBound(t *testing.T) {
	const bound = 4096
	responseOfSize := func(id string, size int) string {
		base := `{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[],"isError":false}}`
		if len(base) > size {
			return base
		}
		return base + strings.Repeat(" ", size-len(base))
	}
	serverForSize := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var rpc struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(body, &rpc)
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = io.WriteString(writer, responseOfSize(string(rpc.ID), size))
		}))
	}

	t.Run("exact bound accepted", func(t *testing.T) {
		server := serverForSize(bound)
		defer server.Close()
		endpoint := server.URL + "/mcp"
		config := testConfig(endpoint)
		config.MaxResponseBytes = bound
		transport := newTestTransport(t, config)

		body, err := transport.Call(context.Background(), testRequest(endpoint))
		if err != nil {
			t.Fatalf("Call at the exact response bound: %v", err)
		}
		response := decodeProxyResponse(t, body)
		requireJSONEqual(t, response.Content, `[]`)
	})

	t.Run("one byte over bound refused", func(t *testing.T) {
		server := serverForSize(bound + 1)
		defer server.Close()
		endpoint := server.URL + "/mcp"
		tracking := &trackingRoundTripper{}
		config := testConfig(endpoint)
		config.MaxResponseBytes = bound
		transport := newTestTransport(t, config)
		mcphttp.WrapClientTransportForTest(t, transport, func(base http.RoundTripper) http.RoundTripper {
			tracking.base = base
			return tracking
		})

		body, err := transport.Call(context.Background(), testRequest(endpoint))
		if body != nil {
			_ = body.Close()
			t.Fatal("Call returned a response body for an oversize response")
		}
		requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
		upstreamBody := tracking.onlyBody(t)
		waitForSignal(t, upstreamBody.closed, "the oversize response body to close")
		if got := upstreamBody.closeCount(); got != 1 {
			t.Fatalf("upstream response body closes = %d; want exactly 1", got)
		}
	})
}

// TestMCPHTTPValidatesConfig proves the trusted endpoint and credential mapping
// are explicit and validated at construction, before any call can dispatch.
func TestMCPHTTPValidatesConfig(t *testing.T) {
	validEndpoint := "https://mcp.example.com/mcp"
	validServer := mcphttp.Server{
		Name:     "files",
		Endpoint: validEndpoint,
		Credentials: []mcphttp.Credential{{
			Secret: testSecretName,
			Header: "Authorization",
			Scheme: "Bearer",
		}},
	}
	cases := []struct {
		name       string
		config     mcphttp.Config
		wantNoLeak string
	}{
		{
			name:   "no servers",
			config: mcphttp.Config{},
		},
		{
			name: "empty server name",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Endpoint: validEndpoint,
			}}},
		},
		{
			name:   "duplicate server name",
			config: mcphttp.Config{Servers: []mcphttp.Server{validServer, validServer}},
		},
		{
			name: "empty endpoint",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name: "files",
			}}},
		},
		{
			name: "relative endpoint",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "mcp",
			}}},
		},
		{
			name: "endpoint userinfo",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "https://user:password@mcp.example.com/mcp",
			}}},
			wantNoLeak: "password",
		},
		{
			name: "endpoint fragment",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "https://mcp.example.com/mcp#fragment",
			}}},
		},
		{
			name: "unsupported scheme",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "ftp://mcp.example.com/mcp",
			}}},
		},
		{
			name: "credential without header",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:        "files",
				Endpoint:    validEndpoint,
				Credentials: []mcphttp.Credential{{Secret: testSecretName}},
			}}},
		},
		{
			name: "invalid credential header",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:        "files",
				Endpoint:    validEndpoint,
				Credentials: []mcphttp.Credential{{Secret: testSecretName, Header: "Bad Header"}},
			}}},
		},
		{
			name: "duplicate credential mapping",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: validEndpoint,
				Credentials: []mcphttp.Credential{
					{Secret: testSecretName, Header: "Authorization", Scheme: "Bearer"},
					{Secret: testSecretName, Header: "X-Api-Key"},
				},
			}}},
		},
		{
			name: "duplicate header mapping",
			config: mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: validEndpoint,
				Credentials: []mcphttp.Credential{
					{Secret: testSecretName, Header: "Authorization", Scheme: "Bearer"},
					{Secret: testKeyName, Header: "Authorization", Scheme: "Bearer"},
				},
			}}},
		},
		{
			name: "negative response bound",
			config: mcphttp.Config{
				Servers:          []mcphttp.Server{validServer},
				MaxResponseBytes: -1,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport, err := mcphttp.New(testCase.config)
			if err == nil {
				t.Fatalf("mcphttp.New accepted invalid config; transport = %+v", transport)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("New error class = %q (ok=%v); want %q (%v)", class, ok, fault.InvalidInput, err)
			}
			if testCase.wantNoLeak != "" && strings.Contains(err.Error(), testCase.wantNoLeak) {
				t.Fatalf("New diagnostic leaked %q: %q", testCase.wantNoLeak, err.Error())
			}
		})
	}

	t.Run("valid config constructs", func(t *testing.T) {
		transport, err := mcphttp.New(mcphttp.Config{Servers: []mcphttp.Server{validServer}})
		if err != nil {
			t.Fatalf("mcphttp.New rejected a valid config: %v", err)
		}
		if transport == nil {
			t.Fatal("mcphttp.New returned a nil transport for a valid config")
		}
	})
}

// TestMCPHTTPDiagnosticsNeverCarryCredentials proves neither a transport error
// nor a malformed upstream body is echoed into a diagnostic.
func TestMCPHTTPDiagnosticsNeverCarryCredentials(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "upstream error status",
			status: http.StatusBadGateway,
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`,
		},
		{
			name:   "malformed body echoing the secret",
			status: http.StatusOK,
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":"` + testSecretValue + `"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", api.MediaType)
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, testCase.body)
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp"
			transport := newTestTransport(t, testConfig(endpoint))

			body, err := transport.Call(context.Background(), testRequest(endpoint))
			if body != nil {
				_ = body.Close()
				t.Fatal("Call returned a response body for a failed response")
			}
			if err == nil {
				t.Fatal("Call returned nil error")
			}
			if strings.Contains(err.Error(), testSecretValue) {
				t.Fatalf("transport diagnostic leaked a credential: %q", err.Error())
			}
			requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
		})
	}
}

// TestMCPHTTPConcurrentCallsUseDistinctIDs proves the driver is safe for
// concurrent use: each call carries its own JSON-RPC id and only accepts the
// response with that id, so results never cross between calls.
func TestMCPHTTPConcurrentCallsUseDistinctIDs(t *testing.T) {
	const calls = 16
	var mu sync.Mutex
	seen := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		var rpc struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil || len(rpc.ID) == 0 {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen[string(rpc.ID)] = true
		mu.Unlock()
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":{"content":`+string(rpc.ID)+`,"isError":false}}`)
	}))
	defer server.Close()
	endpoint := server.URL + "/mcp"
	transport := newTestTransport(t, testConfig(endpoint))

	var wait sync.WaitGroup
	errs := make(chan error, calls)
	contents := make(chan string, calls)
	for range calls {
		wait.Go(func() {
			body, err := transport.Call(context.Background(), testRequest(endpoint))
			if err != nil {
				errs <- err
				return
			}
			response := proxyResponse{}
			data, readErr := io.ReadAll(body)
			_ = body.Close()
			if readErr != nil {
				errs <- readErr
				return
			}
			if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
				errs <- unmarshalErr
				return
			}
			contents <- string(response.Content)
		})
	}
	wait.Wait()
	close(errs)
	close(contents)
	for err := range errs {
		t.Fatalf("concurrent Call: %v", err)
	}

	mu.Lock()
	recorded := make(map[string]bool, len(seen))
	for key := range seen {
		recorded[key] = true
	}
	mu.Unlock()
	if len(recorded) != calls {
		t.Fatalf("distinct JSON-RPC ids = %d; want %d", len(recorded), calls)
	}
	gotContents := 0
	for content := range contents {
		if !recorded[content] {
			t.Fatalf("concurrent content id %q was not sent by this transport: %v", content, recorded)
		}
		gotContents++
	}
	if gotContents != calls {
		t.Fatalf("concurrent results = %d; want %d", gotContents, calls)
	}
}

// The compile-time contract: the driver must satisfy the port exactly.
var _ mcpproxy.Transport = (*mcphttp.Transport)(nil)
