// Package mcphttp_test pins the bounded MCP-over-HTTP driver behind the
// mcpproxy.Transport port.
//
// The driver is constructed with an explicit, trusted per-server endpoint and
// credential mapping. It sends exactly one MCP JSON-RPC tools/call per Call,
// uses the repository's pinned protocol version and JSON-RPC framing, refuses
// redirects instead of forwarding credentials, never replays a tool call, and
// reports a proven pre-dispatch failure as DependencyUnavailable while every
// ambiguous outcome stays an uncertain external effect.
package mcphttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/drivers/mcphttp"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenantID    = id.TenantID("tenant-1")
	testSecretName  = id.SecretName("MCP_TOKEN")
	testSecretValue = "mcp-token-value-never-log"
	testKeyName     = id.SecretName("MCP_KEY")
	testKeyValue    = "mcp-api-key-value-never-log"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID}
}

// testConfig is the trusted binding used by the behavioral tests: one server,
// one credential mapped to a bearer Authorization header.
func testConfig(endpoint string) mcphttp.Config {
	return mcphttp.Config{
		Servers: []mcphttp.Server{{
			Name:     "files",
			Endpoint: endpoint,
			Credentials: []mcphttp.Credential{{
				Secret: testSecretName,
				Header: "Authorization",
				Scheme: "Bearer",
			}},
		}},
	}
}

// testRequest is the exact trusted request the mcpproxy.Transport port hands a
// driver: configuration-derived server/endpoint/tool plus the validated
// arguments and already-resolved credentials.
func testRequest(endpoint string) mcpproxy.UpstreamRequest {
	return mcpproxy.UpstreamRequest{
		Server:      "files",
		Endpoint:    endpoint,
		Tool:        "read_file",
		Arguments:   json.RawMessage(`{"path":"/etc/hosts"}`),
		Credentials: []secret.Resolved{{Name: testSecretName, Value: testSecretValue, Scope: testScope()}},
		Timeout:     5 * time.Second,
	}
}

func newTestTransport(t *testing.T, config mcphttp.Config) *mcphttp.Transport {
	t.Helper()
	transport, err := mcphttp.New(config)
	if err != nil {
		t.Fatalf("mcphttp.New: %v", err)
	}
	if transport == nil {
		t.Fatal("mcphttp.New returned a nil transport")
	}
	return transport
}

// requireUpstreamError pins the exact failure vocabulary the proxy maps: a
// definitive unavailability is Dispatched=false with DependencyUnavailable;
// every ambiguous outcome is Dispatched=true with UncertainExternalEffect.
func requireUpstreamError(t *testing.T, err error, wantDispatched bool, wantClass fault.Class) *mcpproxy.UpstreamError {
	t.Helper()
	upstream := requireNotDispatchedValue(t, err)
	if upstream.Dispatched != wantDispatched {
		t.Fatalf("UpstreamError.Dispatched = %v; want %v (%v)", upstream.Dispatched, wantDispatched, upstream)
	}
	if upstream.Class != wantClass {
		t.Fatalf("UpstreamError.Class = %q; want %q (%v)", upstream.Class, wantClass, upstream)
	}
	return upstream
}

// requireNotDispatchedValue requires the UpstreamError type without
// pinning a class. A refusal before any write only has to be provably
// pre-dispatch; the driver may classify a configuration mismatch differently
// from a dial failure.
func requireNotDispatchedValue(t *testing.T, err error) *mcpproxy.UpstreamError {
	t.Helper()
	if err == nil {
		t.Fatal("Call returned nil error; want an upstream error")
	}
	var upstream *mcpproxy.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %T (%v); want *mcpproxy.UpstreamError", err, err)
	}
	return upstream
}

// proxyResponse mirrors the exact transport response shape the mcpproxy
// decoder accepts: {"content": <any JSON>, "isError": <optional bool>}.
type proxyResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"isError"`
}

func decodeProxyResponse(t *testing.T, body io.ReadCloser) proxyResponse {
	t.Helper()
	if body == nil {
		t.Fatal("Call returned a nil response body with a nil error")
	}
	defer func() {
		if err := body.Close(); err != nil {
			t.Fatalf("close mapped response: %v", err)
		}
	}()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read mapped response: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response proxyResponse
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode mapped response %q: %v", data, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("mapped response has trailing content: %q", data)
	}
	if response.Content == nil {
		t.Fatalf("mapped response has no content member: %q", data)
	}
	return response
}

func requireJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s; want %s", got, want)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForCall(t *testing.T, result <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// TestMCPHTTPUsesConfiguredEndpointAndCredential proves one call is a single
// POST to the trusted endpoint with the MCP version/framing, the
// configured credential mapping (including a second, non-bearer header), and
// that the standard MCP result is mapped to the proxy's response shape.
func TestMCPHTTPUsesConfiguredEndpointAndCredential(t *testing.T) {
	type captured struct {
		method  string
		path    string
		headers http.Header
		body    []byte
	}
	var mu sync.Mutex
	var requests []captured
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, captured{
			method:  request.Method,
			path:    request.URL.Path,
			headers: request.Header.Clone(),
			body:    append([]byte(nil), body...),
		})
		mu.Unlock()

		var rpc struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &rpc)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":{"content":[{"type":"text","text":"hello"}],"isError":false}}`)
	}))
	defer server.Close()
	endpoint := server.URL + "/mcp"

	config := testConfig(endpoint)
	config.Servers[0].Credentials = append(config.Servers[0].Credentials, mcphttp.Credential{
		Secret: testKeyName,
		Header: "X-Api-Key",
	})
	transport := newTestTransport(t, config)

	request := testRequest(endpoint)
	request.Credentials = append(request.Credentials, secret.Resolved{Name: testKeyName, Value: testKeyValue, Scope: testScope()})

	body, err := transport.Call(context.Background(), request)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	response := decodeProxyResponse(t, body)
	if response.IsError {
		t.Fatal("Call mapped a successful tool result as an error")
	}
	requireJSONEqual(t, response.Content, `[{"type":"text","text":"hello"}]`)

	mu.Lock()
	got := append([]captured(nil), requests...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("upstream requests = %d; want exactly 1", len(got))
	}
	call := got[0]
	if call.method != http.MethodPost {
		t.Fatalf("upstream method = %q; want POST", call.method)
	}
	if call.path != "/mcp" {
		t.Fatalf("upstream path = %q; want /mcp (the configured endpoint)", call.path)
	}
	if value := call.headers.Get("Content-Type"); value != api.MediaType {
		t.Fatalf("Content-Type = %q; want %q", value, api.MediaType)
	}
	if value := call.headers.Get("Accept"); !strings.Contains(value, api.MediaType) {
		t.Fatalf("Accept = %q; want it to include %q", value, api.MediaType)
	}
	if value := call.headers.Get("MCP-Protocol-Version"); value != api.MCPProtocolVersion {
		t.Fatalf("MCP-Protocol-Version = %q; want %q", value, api.MCPProtocolVersion)
	}
	if value := call.headers.Get("Authorization"); value != "Bearer "+testSecretValue {
		t.Fatalf("Authorization = %q; want the configured bearer mapping", value)
	}
	if value := call.headers.Get("X-Api-Key"); value != testKeyValue {
		t.Fatalf("X-Api-Key = %q; want the configured raw mapping", value)
	}
	if strings.Contains(string(call.body), testSecretValue) || strings.Contains(string(call.body), testKeyValue) {
		t.Fatal("upstream request body leaked a credential value")
	}

	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(call.body, &rpc); err != nil {
		t.Fatalf("upstream body is not JSON-RPC: %v (%q)", err, call.body)
	}
	if rpc.JSONRPC != api.MCPJSONRPC {
		t.Fatalf("JSON-RPC version = %q; want %q", rpc.JSONRPC, api.MCPJSONRPC)
	}
	if rpc.Method != api.MCPMethodToolsCall {
		t.Fatalf("MCP method = %q; want %q", rpc.Method, api.MCPMethodToolsCall)
	}
	if rpc.Params.Name != "read_file" {
		t.Fatalf("tools/call name = %q; want read_file", rpc.Params.Name)
	}
	requireJSONEqual(t, rpc.Params.Arguments, `{"path":"/etc/hosts"}`)
	if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
		t.Fatalf("tools/call id = %q; want a JSON-RPC id", rpc.ID)
	}
}

// TestMCPHTTPLostResponseIsUncertainNoReplay proves a request that reached the
// upstream but lost its response is ambiguous: the driver reports a dispatched
// uncertain effect and makes exactly one attempt, never an automatic replay.
func TestMCPHTTPLostResponseIsUncertainNoReplay(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("httptest server does not support connection hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	endpoint := server.URL + "/mcp"
	transport := newTestTransport(t, testConfig(endpoint))

	body, err := transport.Call(context.Background(), testRequest(endpoint))
	if body != nil {
		_ = body.Close()
		t.Fatal("Call returned a response body with an error")
	}
	upstream := requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
	if strings.Contains(upstream.Error(), testSecretValue) {
		t.Fatalf("upstream error leaked a credential: %q", upstream.Error())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream requests = %d; want exactly 1 (no automatic tool replay)", got)
	}
}

// TestMCPHTTPRejectsRedirectAndMismatchedResponseID proves the driver refuses a
// redirect instead of forwarding credentials to its target, and refuses a
// response whose JSON-RPC id is not the request id.
func TestMCPHTTPRejectsRedirectAndMismatchedResponseID(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var targetCalls atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			targetCalls.Add(1)
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
		}))
		defer target.Close()

		var originCalls atomic.Int64
		origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			originCalls.Add(1)
			http.Redirect(writer, request, target.URL+"/mcp", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()
		endpoint := origin.URL + "/mcp"

		// A caller-supplied client that would normally follow redirects must not
		// override the driver's redirect refusal.
		config := testConfig(endpoint)
		config.Client = &http.Client{}
		transport := newTestTransport(t, config)

		body, err := transport.Call(context.Background(), testRequest(endpoint))
		if body != nil {
			_ = body.Close()
			t.Fatal("Call returned a response body for a redirect")
		}
		requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
		if got := originCalls.Load(); got != 1 {
			t.Fatalf("origin requests = %d; want exactly 1", got)
		}
		if got := targetCalls.Load(); got != 0 {
			t.Fatalf("redirect target requests = %d; want 0 (credentials are never forwarded)", got)
		}
	})

	t.Run("mismatched response id", func(t *testing.T) {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			body, _ := io.ReadAll(request.Body)
			var rpc struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(body, &rpc)
			wrong := "1"
			if string(rpc.ID) == wrong {
				wrong = "2"
			}
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+wrong+`,"result":{"content":[{"type":"text","text":"wrong call"}],"isError":false}}`)
		}))
		defer server.Close()
		endpoint := server.URL + "/mcp"
		transport := newTestTransport(t, testConfig(endpoint))

		body, err := transport.Call(context.Background(), testRequest(endpoint))
		if body != nil {
			_ = body.Close()
			t.Fatal("Call returned a response body for a mismatched response id")
		}
		requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
		if got := calls.Load(); got != 1 {
			t.Fatalf("upstream requests = %d; want exactly 1", got)
		}
	})
}

// trackingRoundTripper wraps a real transport so the test can observe whether
// the driver closes the upstream response body after cancellation.
type trackingRoundTripper struct {
	base http.RoundTripper

	mu         sync.Mutex
	bodies     []*trackingBody
	bodiesSeen chan struct{}
}

func (transport *trackingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if response != nil && response.Body != nil {
		body := &trackingBody{ReadCloser: response.Body, closed: make(chan struct{})}
		response.Body = body
		transport.mu.Lock()
		if transport.bodiesSeen == nil {
			transport.bodiesSeen = make(chan struct{})
		}
		transport.bodies = append(transport.bodies, body)
		if len(transport.bodies) == 1 {
			close(transport.bodiesSeen)
		}
		transport.mu.Unlock()
	}
	return response, err
}

// waitForBody waits until the transport has observed at least one response
// body. Cancelling before the client received the response headers would test
// an aborted dial, not cancellation of a dispatched call with a live response.
func (transport *trackingRoundTripper) waitForBody(t *testing.T, what string) {
	t.Helper()
	transport.mu.Lock()
	if transport.bodiesSeen == nil {
		transport.bodiesSeen = make(chan struct{})
	}
	seen := transport.bodiesSeen
	transport.mu.Unlock()
	waitForSignal(t, seen, what)
}

func (transport *trackingRoundTripper) onlyBody(t *testing.T) *trackingBody {
	t.Helper()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.bodies) != 1 {
		t.Fatalf("observed response bodies = %d; want exactly 1", len(transport.bodies))
	}
	return transport.bodies[0]
}

type trackingBody struct {
	io.ReadCloser

	mu     sync.Mutex
	closes int
	closed chan struct{}
}

func (body *trackingBody) Close() error {
	body.mu.Lock()
	body.closes++
	first := body.closes == 1
	body.mu.Unlock()
	if !first {
		return nil
	}
	close(body.closed)
	return body.ReadCloser.Close()
}

func (body *trackingBody) closeCount() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

// TestMCPHTTPCancellationClosesResponse proves a cancellation after the request
// was dispatched unblocks the call, closes the upstream response body exactly
// once, and leaves the outcome explicitly uncertain.
func TestMCPHTTPCancellationClosesResponse(t *testing.T) {
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var doneOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer doneOnce.Do(func() { close(handlerDone) })
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0",`)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		startOnce.Do(func() { close(started) })
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	endpoint := server.URL + "/mcp"

	tracking := &trackingRoundTripper{}
	config := testConfig(endpoint)
	transport := newTestTransport(t, config)
	mcphttp.WrapClientTransportForTest(t, transport, func(base http.RoundTripper) http.RoundTripper {
		tracking.base = base
		return tracking
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		body, err := transport.Call(ctx, testRequest(endpoint))
		if body != nil {
			_ = body.Close()
		}
		result <- err
	}()

	waitForSignal(t, started, "the upstream to receive the request")
	tracking.waitForBody(t, "the client to receive the upstream response")
	cancel()

	err := waitForCall(t, result, "Call to return after cancellation")
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)

	body := tracking.onlyBody(t)
	waitForSignal(t, body.closed, "the upstream response body to close")
	if got := body.closeCount(); got != 1 {
		t.Fatalf("upstream response body closes = %d; want exactly 1", got)
	}
	waitForSignal(t, handlerDone, "the upstream handler to observe cancellation")
}

// TestMCPHTTPRefusesUnmappedCredentialBeforeDispatch proves the credential
// mapping is explicit and fail-closed: a configured credential that is not
// presented and a presented credential with no mapping are both refused before
// any network write.
func TestMCPHTTPRefusesUnmappedCredentialBeforeDispatch(t *testing.T) {
	cases := []struct {
		name        string
		credentials []secret.Resolved
	}{
		{
			name: "configured credential missing",
			credentials: []secret.Resolved{{
				Name:  testKeyName,
				Value: testKeyValue,
				Scope: testScope(),
			}},
		},
		{
			name: "presented credential unmapped",
			credentials: []secret.Resolved{
				{Name: testSecretName, Value: testSecretValue, Scope: testScope()},
				{Name: testKeyName, Value: testKeyValue, Scope: testScope()},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", api.MediaType)
				_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp"
			transport := newTestTransport(t, testConfig(endpoint))
			request := testRequest(endpoint)
			request.Credentials = testCase.credentials

			body, err := transport.Call(context.Background(), request)
			if body != nil {
				_ = body.Close()
				t.Fatal("Call returned a response body for an unmapped credential")
			}
			upstream := requireNotDispatchedValue(t, err)
			if upstream.Dispatched {
				t.Fatal("UpstreamError.Dispatched = true for an unmapped credential; want a proven pre-dispatch refusal")
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("upstream requests = %d; want 0", got)
			}
		})
	}
}

// TestMCPHTTPPreDispatchFailureIsDefinitiveUnavailable proves only a failure
// proven to precede the request write is reported as a retryable
// DependencyUnavailable. A closed loopback listener refuses the dial before any
// byte can be written.
func TestMCPHTTPPreDispatchFailureIsDefinitiveUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := "http://" + listener.Addr().String() + "/mcp"
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	transport := newTestTransport(t, testConfig(endpoint))

	body, err := transport.Call(context.Background(), testRequest(endpoint))
	if body != nil {
		_ = body.Close()
		t.Fatal("Call returned a response body for a pre-dispatch failure")
	}
	upstream := requireUpstreamError(t, err, false, fault.DependencyUnavailable)
	if strings.Contains(upstream.Error(), testSecretValue) {
		t.Fatalf("upstream error leaked a credential: %q", upstream.Error())
	}
}

// TestMCPHTTPRefusesUntrustedEndpointBeforeDispatch proves the driver uses the
// configured endpoint and refuses a request naming a different one before any
// network write.
func TestMCPHTTPRefusesUntrustedEndpointBeforeDispatch(t *testing.T) {
	var trustedCalls atomic.Int64
	trusted := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trustedCalls.Add(1)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	defer trusted.Close()

	var untrustedCalls atomic.Int64
	untrusted := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		untrustedCalls.Add(1)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	defer untrusted.Close()

	endpoint := trusted.URL + "/mcp"
	transport := newTestTransport(t, testConfig(endpoint))
	request := testRequest(endpoint)
	request.Endpoint = untrusted.URL + "/mcp"

	body, err := transport.Call(context.Background(), request)
	if body != nil {
		_ = body.Close()
		t.Fatal("Call returned a response body for an untrusted endpoint")
	}
	upstream := requireNotDispatchedValue(t, err)
	if upstream.Dispatched {
		t.Fatalf("UpstreamError.Dispatched = true for an untrusted endpoint; want a proven pre-dispatch refusal")
	}
	if got := trustedCalls.Load(); got != 0 {
		t.Fatalf("trusted endpoint requests = %d; want 0", got)
	}
	if got := untrustedCalls.Load(); got != 0 {
		t.Fatalf("untrusted endpoint requests = %d; want 0", got)
	}
}
