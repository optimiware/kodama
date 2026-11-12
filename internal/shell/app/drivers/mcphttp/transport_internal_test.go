package mcphttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// internalEndpoint starts one loopback MCP upstream and returns its trusted
// endpoint. The handler answers every tools/call with a successful result.
func internalEndpoint(t *testing.T) string {
	t.Helper()
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
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":{"content":[],"isError":false}}`)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/mcp"
}

// internalConfig binds one credential-free loopback server.
func internalConfig(endpoint string) Config {
	return Config{Servers: []Server{{Name: "files", Endpoint: endpoint}}}
}

// internalCall performs one credential-free call and requires success.
func internalCall(t *testing.T, transport *Transport, endpoint string) {
	t.Helper()
	request := mcpproxy.UpstreamRequest{
		Server:    "files",
		Endpoint:  endpoint,
		Tool:      "read_file",
		Arguments: json.RawMessage(`{"path":"/etc/hosts"}`),
		Timeout:   5 * time.Second,
	}
	body, err := transport.Call(context.Background(), request)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if body == nil {
		t.Fatal("Call returned a nil response body with a nil error")
	}
	if _, err := io.ReadAll(body); err != nil {
		_ = body.Close()
		t.Fatalf("read mapped response: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close mapped response: %v", err)
	}
}

// requireOwnedTransport requires a private concrete proxy-free transport and
// returns it.
func requireOwnedTransport(t *testing.T, transport *Transport) *http.Transport {
	t.Helper()
	if transport == nil || transport.client == nil {
		t.Fatal("New returned a transport without a client")
	}
	owned, ok := transport.client.Transport.(*http.Transport)
	if !ok || owned == nil {
		t.Fatalf("private client transport = %T; want *http.Transport", transport.client.Transport)
	}
	if owned.Proxy != nil {
		t.Fatal("private transport has a proxy function; want nil so environment and caller proxies never determine routing")
	}
	return owned
}

// requireFreshDefaults pins the documented explicit defaults of a driver-built
// transport. A zero-value transport (nil dialer, no HTTP/2 attempt, no idle or
// TLS-handshake bound) is not acceptable.
func requireFreshDefaults(t *testing.T, owned *http.Transport) {
	t.Helper()
	if owned.DialContext == nil {
		t.Fatal("driver-built transport has a nil DialContext; want an explicit dialer with a timeout and keepalive")
	}
	if !owned.ForceAttemptHTTP2 {
		t.Fatal("driver-built transport does not attempt HTTP/2")
	}
	if owned.MaxIdleConns != 100 {
		t.Fatalf("driver-built transport MaxIdleConns = %d; want 100", owned.MaxIdleConns)
	}
	if owned.IdleConnTimeout != 90*time.Second {
		t.Fatalf("driver-built transport IdleConnTimeout = %v; want 90s", owned.IdleConnTimeout)
	}
	if owned.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("driver-built transport TLSHandshakeTimeout = %v; want 10s", owned.TLSHandshakeTimeout)
	}
	if owned.ExpectContinueTimeout != time.Second {
		t.Fatalf("driver-built transport ExpectContinueTimeout = %v; want 1s", owned.ExpectContinueTimeout)
	}
}

// TestNewOwnsTransportWithoutProxy pins every successful construction path: a
// nil client, a client with a nil transport, and an explicit transport carrying
// an environment or fixed proxy function all yield a private, concrete
// *http.Transport with Proxy=nil.
func TestNewOwnsTransportWithoutProxy(t *testing.T) {
	const endpoint = "https://mcp.example.com/mcp"
	fixedProxy, err := url.Parse("http://proxy.example.com:3128")
	if err != nil {
		t.Fatalf("parse fixed proxy URL: %v", err)
	}

	environmentProxyTransport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	fixedProxyTransport := &http.Transport{Proxy: http.ProxyURL(fixedProxy)}

	cases := []struct {
		name   string
		client *http.Client
		caller *http.Transport
		fresh  bool
	}{
		{name: "nil client", client: nil, fresh: true},
		{name: "client with nil transport", client: &http.Client{}, fresh: true},
		{name: "explicit transport with environment proxy", client: &http.Client{Transport: environmentProxyTransport}, caller: environmentProxyTransport},
		{name: "explicit transport with fixed proxy", client: &http.Client{Transport: fixedProxyTransport}, caller: fixedProxyTransport},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := internalConfig(endpoint)
			config.Client = testCase.client
			transport, err := New(config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			owned := requireOwnedTransport(t, transport)
			if testCase.fresh {
				requireFreshDefaults(t, owned)
			}
			if testCase.caller != nil {
				if owned == testCase.caller {
					t.Fatal("private client transport is the caller's transport; want a private copy")
				}
				if testCase.caller.Proxy == nil {
					t.Fatal("New cleared the caller transport's proxy policy; the caller must stay unmodified")
				}
			}
			if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && owned == defaultTransport {
				t.Fatal("private client transport is http.DefaultTransport; want a driver-built transport")
			}

			// Two constructions must never share one transport: a private
			// transport per New is the invariant, not a package singleton.
			second, err := New(config)
			if err != nil {
				t.Fatalf("second New: %v", err)
			}
			secondOwned := requireOwnedTransport(t, second)
			if owned == secondOwned {
				t.Fatal("two constructions share one transport; want a private transport per New")
			}
		})
	}
}

// opaqueRoundTripper is a caller-supplied RoundTripper that the driver cannot
// classify: it might not run any httptrace callback, so dispatch evidence would
// be unsound. Counted to prove New never dispatches through it.
type opaqueRoundTripper struct {
	roundTrips atomic.Int64
}

func (roundTripper *opaqueRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	roundTripper.roundTrips.Add(1)
	return nil, errors.New("opaque round tripper must never run")
}

// TestNewRejectsUnsupportedClientTransport pins the refusal boundary: a typed
// nil *http.Transport, an opaque RoundTripper and the shared
// http.DefaultTransport are all refused with InvalidInput, without panicking,
// without dispatching, and without echoing configuration material.
func TestNewRejectsUnsupportedClientTransport(t *testing.T) {
	const endpointMarker = "endpoint-path-token-never-log"
	opaque := &opaqueRoundTripper{}
	var typedNil *http.Transport

	cases := []struct {
		name      string
		transport http.RoundTripper
	}{
		{name: "typed nil transport", transport: typedNil},
		{name: "opaque round tripper", transport: opaque},
		{name: "actual default transport", transport: http.DefaultTransport},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := Config{Servers: []Server{{
				Name:     "files",
				Endpoint: "https://mcp.example.com/mcp/" + endpointMarker,
			}}}
			config.Client = &http.Client{Transport: testCase.transport}
			transport, err := New(config)
			if err == nil {
				t.Fatalf("New accepted an unsupported client transport; transport = %+v", transport)
			}
			if transport != nil {
				t.Fatalf("New returned a transport with an error: %+v", transport)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("New error class = %q (ok=%v); want %q (%v)", class, ok, fault.InvalidInput, err)
			}
			for _, forbidden := range []string{endpointMarker, "mcp.example.com", "proxy.example.com"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("diagnostic leaked %q: %q", forbidden, err.Error())
				}
			}
		})
	}
	if got := opaque.roundTrips.Load(); got != 0 {
		t.Fatalf("opaque RoundTripper round trips = %d; want 0 (New must refuse, never dispatch)", got)
	}
}

// TestNewDoesNotInheritDefaultTransport poisons the ambient
// http.DefaultTransport with a proxy and a dial function that always fails. A
// successful call through the driver-built transport proves New neither uses
// nor clones the ambient default.
func TestNewDoesNotInheritDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })

	var poisonedDials atomic.Int64
	poisonedProxy, err := url.Parse("http://poisoned-proxy.invalid:3128")
	if err != nil {
		t.Fatalf("parse poisoned proxy URL: %v", err)
	}
	http.DefaultTransport = &http.Transport{
		Proxy: http.ProxyURL(poisonedProxy),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			poisonedDials.Add(1)
			return nil, errors.New("ambient default transport dial must never be used")
		},
	}

	endpoint := internalEndpoint(t)
	cases := []struct {
		name   string
		client *http.Client
	}{
		{name: "nil client", client: nil},
		{name: "client with nil transport", client: &http.Client{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := internalConfig(endpoint)
			config.Client = testCase.client
			transport, err := New(config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			owned := requireOwnedTransport(t, transport)
			requireFreshDefaults(t, owned)
			// A poisoned ambient dialer would make this fail; a successful
			// direct call proves the driver-built transport owns its routing.
			internalCall(t, transport, endpoint)
		})
	}
	if got := poisonedDials.Load(); got != 0 {
		t.Fatalf("poisoned ambient dialer calls = %d; want 0 (New must not inherit http.DefaultTransport)", got)
	}
}

// TestNewCopiesCallerClient proves New copies the caller client, clones the
// caller transport into a private proxy-free transport, preserves explicit
// TLS/dial/HTTP2/timeout settings, and leaves the original client and transport
// completely unmodified.
func TestNewCopiesCallerClient(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":{"content":[],"isError":false}}`)
	}))
	defer server.Close()
	endpoint := server.URL + "/mcp"

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	tlsConfig := &tls.Config{RootCAs: pool}

	var dials atomic.Int64
	callerTransport := &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid:3128"}),
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	var callerRedirects atomic.Int64
	callerRedirect := func(*http.Request, []*http.Request) error {
		callerRedirects.Add(1)
		return http.ErrUseLastResponse
	}
	caller := &http.Client{
		Transport:     callerTransport,
		Timeout:       7 * time.Second,
		CheckRedirect: callerRedirect,
	}

	config := internalConfig(endpoint)
	config.Client = caller
	transport, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if transport.client == caller {
		t.Fatal("transport.client is the caller's client; want a private copy")
	}
	if transport.client.Transport == callerTransport {
		t.Fatal("transport.client.Transport is the caller's transport; want a private clone")
	}
	owned := requireOwnedTransport(t, transport)
	if owned.TLSClientConfig == nil || owned.TLSClientConfig.RootCAs != pool {
		t.Fatal("private transport did not preserve the caller's explicit TLS client configuration")
	}
	if !owned.ForceAttemptHTTP2 {
		t.Fatal("private transport did not preserve the caller's ForceAttemptHTTP2 setting")
	}
	if owned.DialContext == nil {
		t.Fatal("private transport did not preserve the caller's explicit dial function")
	}
	if transport.client.Timeout != caller.Timeout {
		t.Fatalf("private client Timeout = %v; want the caller's %v", transport.client.Timeout, caller.Timeout)
	}
	if transport.client.CheckRedirect == nil {
		t.Fatal("the private client copy has no redirect refusal")
	}

	// The caller's client and transport are untouched: same transport pointer,
	// same explicit TLS/dial settings, same timeout and redirect policy, and
	// its proxy function still configured.
	if caller.Transport != callerTransport {
		t.Fatal("New replaced the caller's transport")
	}
	if caller.CheckRedirect == nil {
		t.Fatal("New cleared the caller's CheckRedirect")
	}
	caller.CheckRedirect(nil, nil)
	if got := callerRedirects.Load(); got != 1 {
		t.Fatalf("caller redirect policy calls = %d; want 1 (it must stay the caller's function)", got)
	}
	if caller.Timeout != 7*time.Second {
		t.Fatalf("caller Timeout = %v; want 7s", caller.Timeout)
	}
	if callerTransport.Proxy == nil {
		t.Fatal("New cleared the caller transport's proxy policy")
	}
	if callerTransport.TLSClientConfig != tlsConfig {
		t.Fatal("New replaced the caller transport's TLS configuration")
	}

	// The explicit TLS root and dial function still work through the private
	// clone: the call completes against the TLS upstream.
	internalCall(t, transport, endpoint)
	if got := dials.Load(); got == 0 {
		t.Fatal("private transport did not use the caller's explicit dial function")
	}
}

// tlsPostureServer starts one TLS MCP upstream, optionally with an explicit TLS
// version policy, and counts handler hits.
func tlsPostureServer(t *testing.T, tlsConfig *tls.Config) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
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
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":`+string(rpc.ID)+`,"result":{"content":[],"isError":false}}`)
	}))
	if tlsConfig != nil {
		server.TLS = tlsConfig
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &hits
}

// TestTransportTLSVerificationPosture pins the certificate-verification posture
// of both private transport paths and the clone's protocol floor. The httptest
// certificate is self-signed, so a call that verifies it must fail before the
// handler runs, and a clone that lowers the floor must not reach a TLS 1.0/1.1
// server. The trusted-pool case is the positive control: it proves the endpoint
// and credentials work when the certificate is actually trusted.
func TestTransportTLSVerificationPosture(t *testing.T) {
	selfSigned, selfSignedHits := tlsPostureServer(t, nil)
	legacy, legacyHits := tlsPostureServer(t, &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})

	trusted := x509.NewCertPool()
	trusted.AddCert(selfSigned.Certificate())
	trustedLegacy := x509.NewCertPool()
	trustedLegacy.AddCert(legacy.Certificate())

	cases := []struct {
		name       string
		server     *httptest.Server
		hits       *atomic.Int64
		client     *http.Client
		fresh      bool
		wantErr    bool
		minVersion uint16
	}{
		{name: "fresh nil client rejects self-signed", server: selfSigned, hits: selfSignedHits, fresh: true, wantErr: true},
		{name: "fresh zero client rejects self-signed", server: selfSigned, hits: selfSignedHits, client: &http.Client{}, fresh: true, wantErr: true},
		{name: "clone with empty roots rejects self-signed", server: selfSigned, hits: selfSignedHits, client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}}, wantErr: true},
		{name: "clone with trusted roots reaches self-signed", server: selfSigned, hits: selfSignedHits, client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trusted}}}},
		{name: "clone with trusted roots rejects legacy TLS", server: legacy, hits: legacyHits, client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trustedLegacy}}}, wantErr: true},
		{name: "clone preserves explicit minimum version", server: selfSigned, hits: selfSignedHits, client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trusted, MinVersion: tls.VersionTLS13}}}, minVersion: tls.VersionTLS13},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := testCase.server.URL + "/mcp"
			config := Config{
				Servers: []Server{{
					Name:        "files",
					Endpoint:    endpoint,
					Credentials: []Credential{{Secret: id.SecretName("MCP_TOKEN"), Header: "Authorization", Scheme: "Bearer"}},
				}},
				Client: testCase.client,
			}
			transport, err := New(config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			owned := requireOwnedTransport(t, transport)
			if testCase.fresh {
				if configured := owned.TLSClientConfig; configured != nil && (configured.InsecureSkipVerify || (configured.MinVersion != 0 && configured.MinVersion < tls.VersionTLS12)) {
					t.Fatalf("fresh transport TLS client config = %+v; want verification on and no lowered protocol floor", configured)
				}
			}
			if testCase.minVersion != 0 && (owned.TLSClientConfig == nil || owned.TLSClientConfig.MinVersion != testCase.minVersion) {
				t.Fatalf("clone TLS client config = %+v; want the caller's MinVersion %x preserved", owned.TLSClientConfig, testCase.minVersion)
			}
			before := testCase.hits.Load()
			body, callErr := transport.Call(context.Background(), mcpproxy.UpstreamRequest{
				Server:      "files",
				Endpoint:    endpoint,
				Tool:        "read_file",
				Arguments:   json.RawMessage(`{"path":"/etc/hosts"}`),
				Credentials: []secret.Resolved{{Name: id.SecretName("MCP_TOKEN"), Value: "tls-value-never-log"}},
				Timeout:     5 * time.Second,
			})
			if body != nil {
				_ = body.Close()
			}
			if testCase.wantErr {
				if callErr == nil {
					t.Fatal("Call succeeded; want a certificate-verification or protocol-floor failure")
				}
				if got := testCase.hits.Load(); got != before {
					t.Fatalf("handler hits = %d -> %d; want no request before the TLS failure", before, got)
				}
				return
			}
			if callErr != nil {
				t.Fatalf("Call: %v", callErr)
			}
			if got := testCase.hits.Load(); got != before+1 {
				t.Fatalf("handler hits = %d -> %d; want exactly one request", before, got)
			}
		})
	}
}
