package keys

// White-box transport pins for the Cloud KMS client. The decrypt tests in
// gcp_kms_test.go observe behavior; these assertions pin the two owned client
// boundaries that are otherwise unobservable: no proxy function on either
// transport, fixed request timeouts, redirect refusals, preserved caller TLS
// settings, and the dial target of both requests when a proxy is configured.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestNewGCPKMSOwnsPrivateTransport(t *testing.T) {
	client, err := NewGCPKMS(GCPKMSConfig{})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T; want *http.Transport", client.client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("client reuses the shared http.DefaultTransport")
	}
	if transport.Proxy != nil {
		t.Fatal("client transport has a proxy function; want nil so environment proxies never route a request that carries the token")
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("client transport disables TLS verification")
	}
	if client.client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %v; want the literal 30s", client.client.Timeout)
	}
	// Pin the literals, not the constants they are derived from: a mutant that
	// shortens or lengthens a constant must fail.
	if transport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLS handshake timeout = %v; want the literal 10s", transport.TLSHandshakeTimeout)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("idle connection timeout = %v; want the literal 90s", transport.IdleConnTimeout)
	}
	if transport.ExpectContinueTimeout != time.Second {
		t.Fatalf("expect-continue timeout = %v; want the literal 1s", transport.ExpectContinueTimeout)
	}
	if transport.DialContext == nil {
		t.Fatal("transport has no explicit dial function")
	}
	if gcpKMSDialTimeout != 10*time.Second {
		t.Fatalf("dial timeout = %v; want the literal 10s", gcpKMSDialTimeout)
	}
	if client.client.CheckRedirect == nil {
		t.Fatal("client has no redirect policy")
	}
	if err := client.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy = %v; want http.ErrUseLastResponse", err)
	}
}

// TestNewGCPKMSOwnsPrivateMetadataTransport pins the token source's own
// client: it is never the Cloud KMS client or a caller's, has no proxy
// function, refuses redirects, keeps no idle connection and has the short
// timeouts of a node-local dependency.
func TestNewGCPKMSOwnsPrivateMetadataTransport(t *testing.T) {
	callerTransport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	caller := &http.Client{Transport: callerTransport}
	client, err := NewGCPKMS(GCPKMSConfig{HTTPClient: caller})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	metadata := client.tokens.client
	if metadata == client.client || metadata == caller {
		t.Fatal("metadata requests share the Cloud KMS or the caller's HTTP client")
	}
	transport, ok := metadata.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("metadata transport = %T; want *http.Transport", metadata.Transport)
	}
	if transport == http.DefaultTransport || transport == callerTransport || http.RoundTripper(transport) == client.client.Transport {
		t.Fatal("metadata transport is shared; want a private one")
	}
	if transport.Proxy != nil {
		t.Fatal("metadata transport has a proxy function; the token request must reach the node directly")
	}
	if transport.TLSClientConfig != nil {
		t.Fatal("metadata transport carries a TLS configuration; the metadata server is plain HTTP")
	}
	if !transport.DisableKeepAlives {
		t.Fatal("metadata transport keeps idle connections between the rare token reads")
	}
	if transport.DialContext == nil {
		t.Fatal("metadata transport has no explicit dial function")
	}
	if metadata.Timeout != 10*time.Second {
		t.Fatalf("metadata timeout = %v; want the literal 10s", metadata.Timeout)
	}
	if gcpMetadataDialTimeout != 2*time.Second {
		t.Fatalf("metadata dial timeout = %v; want the literal 2s", gcpMetadataDialTimeout)
	}
	if metadata.CheckRedirect == nil {
		t.Fatal("metadata client has no redirect policy")
	}
	if err := metadata.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("metadata redirect policy = %v; want http.ErrUseLastResponse", err)
	}
}

func TestNewGCPKMSPreservesCallerTLSWithoutProxyOrMutation(t *testing.T) {
	callerTransport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	}
	caller := &http.Client{Transport: callerTransport, Timeout: time.Minute}

	client, err := NewGCPKMS(GCPKMSConfig{HTTPClient: caller})
	if err != nil {
		t.Fatalf("NewGCPKMS: %v", err)
	}
	if client.client == caller {
		t.Fatal("client reused the caller's http.Client")
	}
	owned, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T; want *http.Transport", client.client.Transport)
	}
	if owned == callerTransport {
		t.Fatal("client reused the caller's transport instead of a private clone")
	}
	if owned.Proxy != nil {
		t.Fatal("private transport kept a proxy function")
	}
	if owned.TLSClientConfig == nil || owned.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("private transport did not preserve the caller's explicit TLS configuration")
	}
	if callerTransport.Proxy == nil {
		t.Fatal("constructor mutated the caller's transport")
	}
	if caller.CheckRedirect != nil || caller.Timeout != time.Minute {
		t.Fatal("constructor mutated the caller's client")
	}
	if client.client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %v; want the literal 30s", client.client.Timeout)
	}
}

// gcpDialRecorder is a dial function that records the address it was asked to
// connect to and refuses, so a test learns where a request would have gone
// without opening a socket.
type gcpDialRecorder struct {
	mu        sync.Mutex
	addresses []string
}

var errGCPDialRefused = errors.New("gcp test dial refused")

func (recorder *gcpDialRecorder) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.addresses = append(recorder.addresses, address)
	return nil, errGCPDialRefused
}

func (recorder *gcpDialRecorder) dialed() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.addresses...)
}

// TestGCPKMSDialsBothEndpointsDirectlyDespiteProxies proves neither request is
// routed through a proxy, by the address each transport dials. The process
// environment names a proxy for both schemes and the caller's transport names
// one explicitly; the metadata request must still dial the metadata host and
// the decrypt request the Cloud KMS host.
//
// The hosts are deliberately not loopback addresses: net/http never proxies a
// loopback request, which would make a loopback test pass with a proxy
// function installed. net/http reads the proxy environment once per process,
// so the environment half is only as strong as this test being the first to
// consult it; the explicit caller proxy and the Proxy == nil pins above do not
// depend on that.
func TestGCPKMSDialsBothEndpointsDirectlyDespiteProxies(t *testing.T) {
	const proxy = "http://proxy.example.invalid:3128"
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, proxy)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	t.Run("metadata server", func(t *testing.T) {
		client, err := NewGCPKMS(GCPKMSConfig{MetadataEndpoint: "http://metadata.google.internal"})
		if err != nil {
			t.Fatalf("NewGCPKMS: %v", err)
		}
		var dials gcpDialRecorder
		client.tokens.client.Transport.(*http.Transport).DialContext = dials.DialContext
		_, err = client.Decrypt(context.Background(), secret.KeyRef("gcp-kms:projects/kodama-prod/locations/global/keyRings/ring/cryptoKeys/kek"), []byte("blob"))
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("refused metadata dial error = %v; want %q", err, fault.DependencyUnavailable)
		}
		if !errors.Is(err, errGCPDialRefused) {
			t.Fatalf("metadata transport failure dropped its private cause: %v", err)
		}
		if dialed := dials.dialed(); len(dialed) != 1 || dialed[0] != "metadata.google.internal:80" {
			t.Fatalf("metadata request dialed %q; want exactly metadata.google.internal:80", dialed)
		}
	})

	t.Run("cloud kms", func(t *testing.T) {
		metadata := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Metadata-Flavor", "Google")
			_, _ = io.WriteString(writer, `{"access_token":"gcp-internal-token","expires_in":3600,"token_type":"Bearer"}`)
		}))
		t.Cleanup(metadata.Close)
		var dials gcpDialRecorder
		for name, callerProxy := range map[string]func(*http.Request) (*url.URL, error){
			"explicit caller proxy": http.ProxyURL(proxyURL),
			"environment proxy":     http.ProxyFromEnvironment,
		} {
			client, err := NewGCPKMS(GCPKMSConfig{
				KMSEndpoint:      "https://cloudkms.example.invalid",
				MetadataEndpoint: metadata.URL,
				HTTPClient: &http.Client{Transport: &http.Transport{
					Proxy:       callerProxy,
					DialContext: dials.DialContext,
				}},
			})
			if err != nil {
				t.Fatalf("%s: NewGCPKMS: %v", name, err)
			}
			_, err = client.Decrypt(context.Background(), secret.KeyRef("gcp-kms:projects/kodama-prod/locations/global/keyRings/ring/cryptoKeys/kek"), []byte("blob"))
			if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
				t.Fatalf("%s: refused dial error = %v; want %q", name, err, fault.DependencyUnavailable)
			}
		}
		dialed := dials.dialed()
		if len(dialed) != 2 {
			t.Fatalf("decrypt requests dialed %q; want exactly 2 dials", dialed)
		}
		for _, address := range dialed {
			if address != "cloudkms.example.invalid:443" {
				t.Fatalf("decrypt request dialed %q; want cloudkms.example.invalid:443 (never the proxy)", address)
			}
		}
	})
}
