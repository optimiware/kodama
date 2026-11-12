// Coverage for the dispatch-evidence, non-replay and diagnostic invariants:
//
//   - a failure after a connection was obtained is never proven pre-dispatch,
//     even when WroteRequest did not fire (HTTP/2 write race, failed HEADERS
//     write);
//   - the tools/call POST is non-replayable because it has no rewind body and
//     refuses every header net/http treats as an idempotency marker;
//   - configuration and pre-dispatch refusals never echo the endpoint, a
//     credential value or request body text, and the owned transport never
//     honours an environment proxy;
//   - a credentialed non-loopback upstream must use https;
//   - request.Timeout bounds the call, a truncated-but-valid-JSON response is
//     refused, and a string JSON-RPC id never matches a numeric id.
package mcphttp_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"strconv"
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
)

// requireNoLeak fails when a diagnostic contains any forbidden value. Empty
// values are ignored so a caller can pass optional markers.
func requireNoLeak(t *testing.T, message string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if value == "" {
			continue
		}
		if strings.Contains(message, value) {
			t.Fatalf("diagnostic leaked %q: %q", value, message)
		}
	}
}

func requireInvalidConfig(t *testing.T, err error) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("New error class = %q (ok=%v); want %q (%v)", class, ok, fault.InvalidInput, err)
	}
}

func validResultForID(id json.RawMessage) string {
	return `{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[],"isError":false}}`
}

// failWriteAtConn fails and closes the connection on the Nth Write. Dialing and
// the TLS handshake still complete, so httptrace.GotConn fires, but the first
// HTTP/2 frame write after the connection was obtained fails and net/http never
// reaches WroteRequest.
type failWriteAtConn struct {
	net.Conn
	writes *atomic.Int64
	at     int64
}

func (connection *failWriteAtConn) Write(p []byte) (int, error) {
	if connection.writes.Add(1) == connection.at {
		_ = connection.Conn.Close()
		return 0, errors.New("forced write failure")
	}
	return connection.Conn.Write(p)
}

// TestMCPHTTPHTTP2SkippedWroteRequestIsUncertain deterministically pins the
// dispatch-evidence rule on a real HTTP/2 TLS connection: obtaining a
// connection is enough to make a later failure ambiguous even when net/http
// skips WroteRequest because the first frame write failed. Classifying from
// WroteRequest alone would report this failure as safe to replay.
func TestMCPHTTPHTTP2SkippedWroteRequestIsUncertain(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	endpoint := server.URL + "/mcp"

	skippedWroteRequest := 0
	for at := int64(1); at <= 8; at++ {
		var gotConn, wroteRequest atomic.Bool
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn:      func(httptrace.GotConnInfo) { gotConn.Store(true) },
			WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
		})
		var writes atomic.Int64
		clientTransport := &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool},
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &failWriteAtConn{Conn: connection, writes: &writes, at: at}, nil
			},
		}
		config := testConfig(endpoint)
		config.Client = &http.Client{Transport: clientTransport}
		transport := newTestTransport(t, config)

		body, err := transport.Call(ctx, testRequest(endpoint))
		if body != nil {
			_ = body.Close()
		}
		if gotConn.Load() && !wroteRequest.Load() {
			skippedWroteRequest++
			if err == nil {
				t.Fatalf("at=%d: Call succeeded although the first frame write failed", at)
			}
			upstream := requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
			requireNoLeak(t, upstream.Error(), endpoint, testSecretValue)
		}
		mcphttp.CloseIdleConnectionsForTest(t, transport)
	}
	if skippedWroteRequest == 0 {
		t.Fatal("test did not exercise the HTTP/2 WroteRequest-skip path")
	}
}

// cancelAfterWriteConn cancels the call context immediately after the Nth
// connection write. On HTTP/2 the last request write is not followed
// synchronously by WroteRequest, so the cancellation can land in the gap where
// the request is already on the wire.
type cancelAfterWriteConn struct {
	net.Conn
	writes   *atomic.Int64
	cancelAt int64
	cancel   context.CancelFunc
}

func (connection *cancelAfterWriteConn) Write(p []byte) (int, error) {
	n, err := connection.Conn.Write(p)
	if connection.writes.Add(1) == connection.cancelAt {
		connection.cancel()
	}
	return n, err
}

// TestMCPHTTPHTTP2CancellationAfterLastWriteNeverPreDispatch reproduces the
// HTTP/2 write race over a real TLS server: cancellation lands after the last
// request write, and a request the server fully received must never be reported
// as not dispatched. The loop is bounded; the deterministic failing-write test
// above pins the same rule.
func TestMCPHTTPHTTP2CancellationAfterLastWriteNeverPreDispatch(t *testing.T) {
	const iterations = 40

	var handled sync.Map
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err == nil && len(body) > 0 {
			handled.Store(request.Header.Get("Authorization"), true)
		}
		// Widen the response window so cancellation reliably follows the
		// request write instead of racing a fast response.
		time.Sleep(time.Millisecond)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	endpoint := server.URL + "/mcp"

	var mu sync.Mutex
	var misclassified []string
	for _, cancelAt := range []int64{3, 4, 5, 6} {
		for iteration := range iterations {
			ctx, cancel := context.WithCancel(context.Background())
			var writes atomic.Int64
			clientTransport := &http.Transport{
				TLSClientConfig:   &tls.Config{RootCAs: pool},
				ForceAttemptHTTP2: true,
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &cancelAfterWriteConn{Conn: connection, writes: &writes, cancelAt: cancelAt, cancel: cancel}, nil
				},
			}
			config := testConfig(endpoint)
			config.Client = &http.Client{Transport: clientTransport}
			transport := newTestTransport(t, config)

			token := fmt.Sprintf("h2-%d-%d", cancelAt, iteration)
			request := testRequest(endpoint)
			request.Credentials[0].Value = token
			body, err := transport.Call(ctx, request)
			if body != nil {
				_ = body.Close()
			}
			if err != nil {
				var upstream *mcpproxy.UpstreamError
				if errors.As(err, &upstream) && !upstream.Dispatched {
					if _, received := handled.Load("Bearer " + token); received {
						mu.Lock()
						misclassified = append(misclassified, fmt.Sprintf("%s cancelAt=%d: %v", token, cancelAt, err))
						mu.Unlock()
					}
				}
			}
			cancel()
			mcphttp.CloseIdleConnectionsForTest(t, transport)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(misclassified) != 0 {
		t.Fatalf("Dispatched=false after the server received the full body: %d cases; first: %s", len(misclassified), misclassified[0])
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// TestMCPHTTPGetBodyIsCleared proves the request handed to the HTTP stack has
// no rewind body, so net/http cannot replay the tools/call POST even if an
// idempotency header were ever configured or accepted.
func TestMCPHTTPGetBodyIsCleared(t *testing.T) {
	var hasGetBody atomic.Bool
	config := testConfig("https://mcp.example.com/mcp")
	transport := newTestTransport(t, config)
	mcphttp.WrapClientTransportForTest(t, transport, func(http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			hasGetBody.Store(request.GetBody != nil)
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read outgoing request body: %v", err)
			}
			var rpc struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(body, &rpc); err != nil {
				t.Errorf("outgoing request is not JSON: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{api.MediaType}},
				Body:       io.NopCloser(strings.NewReader(validResultForID(rpc.ID))),
				Request:    request,
			}, nil
		})
	})

	responseBody, err := transport.Call(context.Background(), mcpproxy.UpstreamRequest{
		Server:      "files",
		Endpoint:    "https://mcp.example.com/mcp",
		Tool:        "read_file",
		Arguments:   json.RawMessage(`{"path":"/etc/hosts"}`),
		Credentials: []secret.Resolved{{Name: testSecretName, Value: testSecretValue, Scope: testScope()}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if responseBody != nil {
		_ = responseBody.Close()
	}
	if hasGetBody.Load() {
		t.Fatal("outgoing request kept a GetBody rewind function; net/http could replay the tools/call POST")
	}
}

// TestMCPHTTPKeepAliveReusedLostResponseIsOneHit proves a lost response on a
// reused keep-alive connection is not retried: the second call reaches the
// upstream exactly once even though the server closes without responding.
func TestMCPHTTPKeepAliveReusedLostResponseIsOneHit(t *testing.T) {
	var mu sync.Mutex
	var remoteAddrs []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		remoteAddrs = append(remoteAddrs, request.RemoteAddr)
		hit := len(remoteAddrs)
		mu.Unlock()
		if hit == 1 {
			var rpc struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(body, &rpc)
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = io.WriteString(writer, validResultForID(rpc.ID))
			return
		}
		// The request was fully received, then the response is lost.
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

	first, err := transport.Call(context.Background(), testRequest(endpoint))
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	_ = decodeProxyResponse(t, first)

	second, err := transport.Call(context.Background(), testRequest(endpoint))
	if second != nil {
		_ = second.Close()
		t.Fatal("second Call returned a response body for a lost response")
	}
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)

	mu.Lock()
	got := append([]string(nil), remoteAddrs...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream hits = %d; want exactly 2 (the lost second call must not be replayed)", len(got))
	}
	if got[0] != got[1] {
		t.Fatalf("upstream connections = %q then %q; want the second call to reuse the keep-alive connection", got[0], got[1])
	}
}

// idempotencyHeaderRoundTripper injects the header net/http treats as a replay
// marker just before the real transport runs. This simulates any configured or
// future header that would otherwise make the POST replayable.
type idempotencyHeaderRoundTripper struct {
	base http.RoundTripper
}

func (roundTripper *idempotencyHeaderRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Header.Set("Idempotency-Key", "injected-by-test")
	return roundTripper.base.RoundTrip(request)
}

// TestMCPHTTPIdempotencyHeaderNeverReplaysPost proves clearing GetBody is what
// makes the tools/call POST non-replayable: even with an idempotency marker on
// the wire, a lost response on a reused connection reaches the upstream once.
func TestMCPHTTPIdempotencyHeaderNeverReplaysPost(t *testing.T) {
	var mu sync.Mutex
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		hits++
		hit := hits
		mu.Unlock()
		if hit == 1 {
			var rpc struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(body, &rpc)
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = io.WriteString(writer, validResultForID(rpc.ID))
			return
		}
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

	config := testConfig(endpoint)
	transport := newTestTransport(t, config)
	mcphttp.WrapClientTransportForTest(t, transport, func(base http.RoundTripper) http.RoundTripper {
		return &idempotencyHeaderRoundTripper{base: base}
	})

	first, err := transport.Call(context.Background(), testRequest(endpoint))
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	_ = decodeProxyResponse(t, first)

	second, err := transport.Call(context.Background(), testRequest(endpoint))
	if second != nil {
		_ = second.Close()
		t.Fatal("second Call returned a response body for a lost response")
	}
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream hits = %d; want exactly 2 (an idempotency header must not make the POST replayable)", got)
	}
}

// proxyIsolationSubprocessEnv marks the re-executed child that owns a fresh
// process-wide proxy environment cache.
const proxyIsolationSubprocessEnv = "KODAMA_MCPHTTP_PROXY_ISOLATION"

// proxyControlObservedMarker is printed by the child only after its positive
// control request reached the recording proxy.
const proxyControlObservedMarker = "positive control reached the proxy"

// TestMCPHTTPClientPathsNeverUseEnvironmentProxy proves in a fresh subprocess
// that no accepted construction path lets the environment or a caller proxy
// determine routing. ProxyFromEnvironment caches the process environment on
// first use, so only a child process that installs its proxy environment before
// that first call can distinguish a real suppression from a cache miss. The
// child first makes a control request through ProxyFromEnvironment that must
// reach the recording proxy; only then does a zero driver proxy hit prove
// anything.
func TestMCPHTTPClientPathsNeverUseEnvironmentProxy(t *testing.T) {
	if os.Getenv(proxyIsolationSubprocessEnv) == "1" {
		runProxyIsolationChild(t)
		return
	}
	command := exec.Command(os.Args[0],
		"-test.run=^TestMCPHTTPClientPathsNeverUseEnvironmentProxy$",
		"-test.v",
	)
	command.Env = append(os.Environ(), proxyIsolationSubprocessEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy-isolation subprocess failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), proxyControlObservedMarker) {
		t.Fatalf("proxy-isolation subprocess did not observe the positive control:\n%s", output)
	}
}

// runProxyIsolationChild runs in the fresh subprocess. It installs the proxy
// environment, proves the environment is honored with a success control, then
// requires every accepted construction path to reach no proxy at all.
func runProxyIsolationChild(t *testing.T) {
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyHits.Add(1)
		http.Error(writer, "the driver must never route through this proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()

	// The fresh process has an empty ProxyFromEnvironment cache, so installing
	// the environment now makes the positive control meaningful.
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("REQUEST_METHOD", "")

	control := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		Timeout:   2 * time.Second,
	}
	response, err := control.Get("http://proxy-control.kodama.invalid/")
	if err != nil {
		t.Fatalf("positive control request through the environment proxy: %v", err)
	}
	_ = response.Body.Close()
	controlHits := proxyHits.Load()
	if controlHits == 0 {
		t.Fatal("positive control did not reach the recording proxy; the environment proxy is not being exercised")
	}
	t.Logf("%s", proxyControlObservedMarker)

	fixedProxy, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse recording proxy URL: %v", err)
	}
	// A credential-free documentation-range endpoint is accepted at
	// construction and is never reachable directly, so the only question is
	// whether it is dialed or handed to a proxy. request.Timeout bounds it.
	endpoint := "http://192.0.2.1:9/mcp"
	cases := []struct {
		name   string
		client *http.Client
	}{
		{name: "nil client"},
		{name: "client with nil transport", client: &http.Client{}},
		{name: "explicit transport with environment proxy", client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}},
		{name: "explicit transport with fixed proxy", client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(fixedProxy)}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := mcphttp.Config{
				Servers: []mcphttp.Server{{Name: "files", Endpoint: endpoint}},
				Client:  testCase.client,
			}
			transport := newTestTransport(t, config)
			request := mcpproxy.UpstreamRequest{
				Server:    "files",
				Endpoint:  endpoint,
				Tool:      "read_file",
				Arguments: json.RawMessage(`{"path":"/etc/hosts"}`),
				Timeout:   500 * time.Millisecond,
			}
			started := time.Now()
			body, _ := transport.Call(context.Background(), request)
			if body != nil {
				_ = body.Close()
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("bounded non-loopback call took %v; want it bounded by request.Timeout", elapsed)
			}
			if got := proxyHits.Load(); got != controlHits {
				t.Fatalf("proxy requests = %d -> %d; the %s path used a proxy", controlHits, got, testCase.name)
			}
		})
	}
}

// TestMCPHTTPCallerProxySuppressedAndUnmodified pins the injected-client
// hardening: an explicit caller proxy function is never honored, and the
// caller's client and transport are never mutated.
func TestMCPHTTPCallerProxySuppressedAndUnmodified(t *testing.T) {
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyHits.Add(1)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	defer proxy.Close()

	var originHits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		originHits.Add(1)
		var rpc struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &rpc)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, validResultForID(rpc.ID))
	}))
	defer origin.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	callerTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	caller := &http.Client{Transport: callerTransport}
	endpoint := origin.URL + "/mcp"
	config := testConfig(endpoint)
	config.Client = caller
	transport := newTestTransport(t, config)

	body, err := transport.Call(context.Background(), testRequest(endpoint))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = decodeProxyResponse(t, body)
	if got := originHits.Load(); got != 1 {
		t.Fatalf("origin requests = %d; want exactly 1 (the driver dials the trusted endpoint directly)", got)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("caller proxy requests = %d; want 0 (an injected proxy policy must be suppressed)", got)
	}

	// The caller's own configuration is untouched and still functional: its
	// transport pointer, redirect policy and proxy function are unchanged.
	if caller.Transport != callerTransport {
		t.Fatal("New replaced the caller's transport")
	}
	if caller.CheckRedirect != nil {
		t.Fatal("New mutated the caller's client CheckRedirect")
	}
	if callerTransport.Proxy == nil {
		t.Fatal("New cleared the caller transport's proxy policy")
	}
	controlResponse, err := caller.Get("http://caller-proxy-control.kodama.invalid/")
	if err != nil {
		t.Fatalf("the caller's unchanged proxy policy no longer works: %v", err)
	}
	_ = controlResponse.Body.Close()
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("caller proxy requests after the control = %d; want 1 (the caller keeps its proxy policy)", got)
	}
}

// TestMCPHTTPRefusesIdempotencyHeaders proves every header net/http treats as
// an idempotency marker is refused at construction.
func TestMCPHTTPRefusesIdempotencyHeaders(t *testing.T) {
	for _, header := range []string{
		"Idempotency-Key",
		"idempotency-key",
		"IDEMPOTENCY-KEY",
		"X-Idempotency-Key",
		"x-idempotency-key",
	} {
		t.Run(header, func(t *testing.T) {
			config := mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "https://mcp.example.com/mcp",
				Credentials: []mcphttp.Credential{{
					Secret: testSecretName,
					Header: header,
				}},
			}}}
			transport, err := mcphttp.New(config)
			if err == nil {
				t.Fatalf("mcphttp.New accepted credential header %q; transport = %+v", header, transport)
			}
			requireInvalidConfig(t, err)
			requireNoLeak(t, err.Error(), header)
		})
	}
}

// TestMCPHTTPRefusesReservedProtocolHeaders proves a credential cannot replace
// a protocol header the driver owns.
func TestMCPHTTPRefusesReservedProtocolHeaders(t *testing.T) {
	for _, header := range []string{"Content-Type", "Accept", "MCP-Protocol-Version", "Host", "Content-Length", "Transfer-Encoding", "Connection"} {
		t.Run(header, func(t *testing.T) {
			config := mcphttp.Config{Servers: []mcphttp.Server{{
				Name:     "files",
				Endpoint: "https://mcp.example.com/mcp",
				Credentials: []mcphttp.Credential{{
					Secret: testSecretName,
					Header: header,
				}},
			}}}
			if transport, err := mcphttp.New(config); err == nil {
				t.Fatalf("mcphttp.New accepted reserved credential header %q; transport = %+v", header, transport)
			} else {
				requireInvalidConfig(t, err)
			}
		})
	}
}

// TestMCPHTTPConfigValidationNeverEchoesEndpoint pins the configuration
// refusals, including a query string that could carry
// an inline token, and the https rule for credentialed non-loopback upstreams.
func TestMCPHTTPConfigValidationNeverEchoesEndpoint(t *testing.T) {
	const token = "endpoint-query-token-never-log"
	credential := []mcphttp.Credential{{Secret: testSecretName, Header: "Authorization", Scheme: "Bearer"}}
	server := func(endpoint string, credentials []mcphttp.Credential) mcphttp.Config {
		return mcphttp.Config{Servers: []mcphttp.Server{{Name: "files", Endpoint: endpoint, Credentials: credentials}}}
	}

	refused := []struct {
		name   string
		config mcphttp.Config
	}{
		{
			name:   "endpoint query string refused",
			config: server("https://mcp.example.com/mcp?token="+token, credential),
		},
		{
			name:   "endpoint query token not echoed on scheme refusal",
			config: server("ftp://mcp.example.com/mcp?token="+token, credential),
		},
		{
			name:   "endpoint empty host refused",
			config: server("http:///mcp", nil),
		},
		{
			name:   "endpoint host lookalike is not loopback",
			config: server("http://localhost.evil.com/mcp", credential),
		},
		{
			name:   "credentialed plain http non-loopback refused",
			config: server("http://mcp.example.com/mcp", credential),
		},
		{
			name: "invalid secret name refused",
			config: server("https://mcp.example.com/mcp", []mcphttp.Credential{{
				Secret: id.SecretName("not a secret name"),
				Header: "Authorization",
			}}),
		},
		{
			name: "invalid credential scheme refused",
			config: server("https://mcp.example.com/mcp", []mcphttp.Credential{{
				Secret: testSecretName,
				Header: "Authorization",
				Scheme: "Bearer Token",
			}}),
		},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			transport, err := mcphttp.New(testCase.config)
			if err == nil {
				t.Fatalf("mcphttp.New accepted invalid config; transport = %+v", transport)
			}
			requireInvalidConfig(t, err)
			requireNoLeak(t, err.Error(), token, testSecretValue, "mcp.example.com", "localhost.evil.com", "not a secret name")
		})
	}

	accepted := []struct {
		name   string
		config mcphttp.Config
	}{
		{
			name:   "credentialed http loopback IPv4 accepted",
			config: server("http://127.0.0.1:9/mcp", credential),
		},
		{
			name:   "credentialed http localhost accepted",
			config: server("http://localhost:9/mcp", credential),
		},
		{
			name:   "credentialed http loopback IPv6 accepted",
			config: server("http://[::1]:9/mcp", credential),
		},
		{
			name:   "credential-free plain http non-loopback accepted",
			config: server("http://mcp.example.com/mcp", nil),
		},
	}
	for _, testCase := range accepted {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := mcphttp.New(testCase.config); err != nil {
				t.Fatalf("mcphttp.New refused a valid config: %v", err)
			}
		})
	}
}

// TestMCPHTTPPreDispatchRefusalsCarryNoSecrets pins empty tool/arguments
// refusals: zero upstream hits, Dispatched=false, and no endpoint, credential
// or body text in the diagnostic.
func TestMCPHTTPPreDispatchRefusalsCarryNoSecrets(t *testing.T) {
	const argumentMarker = "argument-body-marker-never-log"
	cases := []struct {
		name      string
		tool      string
		arguments json.RawMessage
	}{
		{name: "empty tool", tool: "", arguments: json.RawMessage(`{"path":"` + argumentMarker + `"}`)},
		{name: "nil arguments", tool: "read_file", arguments: nil},
		{name: "empty arguments", tool: "read_file", arguments: json.RawMessage{}},
		{name: "null arguments", tool: "read_file", arguments: json.RawMessage("null")},
		{name: "invalid arguments", tool: "read_file", arguments: json.RawMessage(`{"path":` + argumentMarker)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var hits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				hits.Add(1)
				writer.Header().Set("Content-Type", api.MediaType)
				_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp"
			transport := newTestTransport(t, testConfig(endpoint))

			request := testRequest(endpoint)
			request.Tool = testCase.tool
			request.Arguments = testCase.arguments
			body, err := transport.Call(context.Background(), request)
			if body != nil {
				_ = body.Close()
				t.Fatal("Call returned a response body for a pre-dispatch refusal")
			}
			requireUpstreamError(t, err, false, fault.DependencyUnavailable)
			requireNoLeak(t, err.Error(), endpoint, "127.0.0.1", testSecretValue, argumentMarker)
			if got := hits.Load(); got != 0 {
				t.Fatalf("upstream requests = %d; want 0", got)
			}
		})
	}
}

// TestMCPHTTPRefusesEveryRedirectStatus proves the redirect refusal is the
// enforcement, not the missing rewind body: 301/302/303 replace the POST with a
// bodyless GET, which net/http would follow even without GetBody.
func TestMCPHTTPRefusesEveryRedirectStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
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
				http.Redirect(writer, request, target.URL+"/mcp", status)
			}))
			defer origin.Close()
			endpoint := origin.URL + "/mcp"

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
	}
}

// TestMCPHTTPCredentialRefusalNeverEchoesValue pins credential-resolution
// refusals: zero upstream hits, Dispatched=false, and no credential value in
// the diagnostic.
func TestMCPHTTPCredentialRefusalNeverEchoesValue(t *testing.T) {
	const injectedValue = "credential-value-secret-marker"
	cases := []struct {
		name        string
		credentials []secret.Resolved
	}{
		{
			name: "invalid header value",
			credentials: []secret.Resolved{{
				Name:  testSecretName,
				Value: injectedValue + "\r\nX-Injected: 1",
				Scope: testScope(),
			}},
		},
		{
			name: "configured credential missing",
			credentials: []secret.Resolved{{
				Name:  testKeyName,
				Value: injectedValue,
				Scope: testScope(),
			}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var hits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				hits.Add(1)
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
				t.Fatal("Call returned a response body for a credential refusal")
			}
			requireUpstreamError(t, err, false, fault.DependencyUnavailable)
			requireNoLeak(t, err.Error(), endpoint, "127.0.0.1", injectedValue, testSecretValue)
			if got := hits.Load(); got != 0 {
				t.Fatalf("upstream requests = %d; want 0", got)
			}
		})
	}
}

// TestMCPHTTPTransportErrorNeverEchoesURL proves a Do failure never renders the
// endpoint URL (which can carry a path token) or a credential.
func TestMCPHTTPTransportErrorNeverEchoesURL(t *testing.T) {
	const pathToken = "endpoint-path-token-never-log"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	endpoint := "http://" + address + "/mcp/" + pathToken
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	transport := newTestTransport(t, testConfig(endpoint))

	body, err := transport.Call(context.Background(), testRequest(endpoint))
	if body != nil {
		_ = body.Close()
		t.Fatal("Call returned a response body for a dial failure")
	}
	requireUpstreamError(t, err, false, fault.DependencyUnavailable)
	requireNoLeak(t, err.Error(), endpoint, pathToken, address, "127.0.0.1", testSecretValue)
}

// TestMCPHTTPTimeoutBoundsCall proves request.Timeout bounds the call even when
// the upstream stalls after receiving the request.
func TestMCPHTTPTimeoutBoundsCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		startOnce.Do(func() { close(started) })
		<-release
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":false}}`)
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	endpoint := server.URL + "/mcp"
	transport := newTestTransport(t, testConfig(endpoint))

	request := testRequest(endpoint)
	request.Timeout = 100 * time.Millisecond
	begin := time.Now()
	result := make(chan error, 1)
	go func() {
		body, err := transport.Call(context.Background(), request)
		if body != nil {
			_ = body.Close()
		}
		result <- err
	}()
	waitForSignal(t, started, "the upstream to receive the request")

	err := waitForCall(t, result, "Call to return after request.Timeout")
	elapsed := time.Since(begin)
	if elapsed > 2*time.Second {
		t.Fatalf("Call took %v; request.Timeout = %v was not honoured", elapsed, request.Timeout)
	}
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
}

// TestMCPHTTPTruncatedButValidJSONIsRefused proves a read error is never ignored
// even when the bytes read so far happen to be valid JSON.
func TestMCPHTTPTruncatedButValidJSONIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var rpc struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &rpc)
		response := validResultForID(rpc.ID)
		writer.Header().Set("Content-Type", api.MediaType)
		writer.Header().Set("Content-Length", strconv.Itoa(len(response)+64))
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, response)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
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
		t.Fatal("Call accepted a truncated response whose prefix was valid JSON")
	}
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
	requireNoLeak(t, err.Error(), endpoint, testSecretValue)
}

// TestMCPHTTPStringIDNeverMatchesNumericID proves JSON-RPC id comparison is
// type-aware: the string "1" is not the request's numeric id 1.
func TestMCPHTTPStringIDNeverMatchesNumericID(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"confused"}],"isError":false}}`)
	}))
	defer server.Close()
	endpoint := server.URL + "/mcp"
	transport := newTestTransport(t, testConfig(endpoint))

	body, err := transport.Call(context.Background(), testRequest(endpoint))
	if body != nil {
		_ = body.Close()
		t.Fatal("Call accepted a string JSON-RPC id for a numeric request id")
	}
	requireUpstreamError(t, err, true, fault.UncertainExternalEffect)
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream requests = %d; want exactly 1", got)
	}
}
