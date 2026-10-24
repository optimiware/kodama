package openai

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

// fakeResolver maps names to scripted answers and counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // per name: consecutive answers; the last repeats
	calls   map[string]int
}

func newResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
}

func (r *fakeResolver) set(host string, answers ...[]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[host] = nil
	for _, a := range answers {
		var addrs []netip.Addr
		for _, s := range a {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		r.answers[host] = append(r.answers[host], addrs)
	}
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq, ok := r.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := r.calls[host]
	r.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], nil
}

func (r *fakeResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// provider is a TLS server recording what reaches it.
type provider struct {
	*httptest.Server
	hits    atomic.Int64
	mu      sync.Mutex
	headers []http.Header
	handler http.HandlerFunc
}

func newProvider(t *testing.T, handler http.HandlerFunc) *provider {
	t.Helper()
	p := &provider{handler: handler}
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.mu.Lock()
		p.headers = append(p.headers, r.Header.Clone())
		p.mu.Unlock()
		p.handler(w, r)
	}))
	p.Server.StartTLS()
	t.Cleanup(p.Close)
	return p
}

func (p *provider) port() int { return p.Listener.Addr().(*net.TCPAddr).Port }

func (p *provider) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.Certificate())
	return pool
}

func (p *provider) seen() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.headers...)
}

const secret = "sk-t032-sentinel-credential"

func sseOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
}

func loopback() AddressPolicy { return AllowPrefixes(PublicOnly, netip.MustParsePrefix("127.0.0.0/8")) }

type rig struct {
	client   *Client
	resolver *fakeResolver
}

func newRig(t *testing.T, p *provider, addr AddressPolicy, origins ...string) rig {
	t.Helper()
	res := newResolver()
	res.set("example.com", []string{"127.0.0.1"})
	res.set("evil.test", []string{"127.0.0.1"})
	var approved []ApprovedOrigin
	for _, o := range origins {
		approved = append(approved, ApprovedOrigin{Origin: o, PathPrefix: "/v1"})
	}
	pol, err := NewPolicy(PolicyConfig{Origins: approved, Addresses: addr, Resolver: res})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{}
	if p != nil {
		tlsCfg.RootCAs = p.roots()
	}
	c, err := NewClient(Config{Policy: pol, TLS: tlsCfg})
	if err != nil {
		t.Fatal(err)
	}
	return rig{client: c, resolver: res}
}

func endpoint(t *testing.T, host string, port int, path string) gateway.Endpoint {
	t.Helper()
	e, err := gateway.ParseEndpoint(fmt.Sprintf("https://%s:%d%s", host, port, path))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func call(t *testing.T, e gateway.Endpoint) gateway.ProviderCall {
	t.Helper()
	c, err := DecodeRequest(fixture(t, "requests/minimal.json"), DefaultRequestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return gateway.ProviderCall{Endpoint: e, Credential: gateway.NewCredential([]byte(secret)), Completion: c, MaxTokens: 32}
}

func perr(t *testing.T, err error) *gateway.ProviderError {
	t.Helper()
	var pe *gateway.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a provider error", err)
	}
	return pe
}

// Test_redirect_credential proves GM-02/GM-09: a redirect cannot carry the
// provider credential anywhere, destinations and resolved addresses are checked
// at every connection, and the transport ignores process proxies.
func Test_redirect_credential(t *testing.T) {
	t.Run("a stream from the approved origin carries exactly the gateway's headers", func(t *testing.T) {
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		s, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if d := s.Destination(); d.Origin != fmt.Sprintf("https://example.com:%d", p.port()) || d.IP != "127.0.0.1" {
			t.Fatalf("destination = %+v", d)
		}
		n := 0
		for {
			ch, err := s.Next(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !ch.Done {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("%d chunks", n)
		}
		h := p.seen()[0]
		if h.Get("Authorization") != "Bearer "+secret || h.Get("Cookie") != "" || h.Get("Accept-Encoding") != "identity" {
			t.Fatalf("headers: %v", h)
		}
		for k := range h {
			switch k {
			case "Connection", "Authorization", "Content-Type", "Accept", "Accept-Encoding", "User-Agent", "Content-Length":
			default:
				t.Errorf("unexpected outbound header %s", k)
			}
		}
	})

	t.Run("redirects are never followed and their targets see nothing", func(t *testing.T) {
		foreign := newProvider(t, sseOK)
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("downgrade target was contacted") }))
		defer plain.Close()
		targets := map[string]func(origin *provider) string{
			"same origin":       func(o *provider) string { return o.URL + "/v1/other" },
			"foreign origin":    func(*provider) string { return foreign.URL + "/steal" },
			"foreign by name":   func(*provider) string { return fmt.Sprintf("https://evil.test:%d/steal", foreign.port()) },
			"scheme downgrade":  func(*provider) string { return plain.URL + "/steal" },
			"protocol-relative": func(*provider) string { return "//" + strings.TrimPrefix(foreign.URL, "https://") + "/steal" },
		}
		for _, status := range []int{301, 302, 303, 307, 308} {
			for name, target := range targets {
				var origin *provider
				origin = newProvider(t, func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target(origin), status)
				})
				before := foreign.hits.Load()
				r := newRig(t, origin, loopback(), fmt.Sprintf("https://example.com:%d", origin.port()))
				_, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", origin.port(), "/v1")))
				pe := perr(t, err)
				if pe.Kind != gateway.ProviderRedirect || pe.Status != status || !pe.Sent {
					t.Errorf("%s/%d: %+v", name, status, pe)
				}
				if origin.hits.Load() != 1 {
					t.Errorf("%s/%d: the origin saw %d requests; a redirect must not be followed even within the origin", name, status, origin.hits.Load())
				}
				if foreign.hits.Load() != before {
					t.Errorf("%s/%d: the redirect target received a request", name, status)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s/%d: the credential leaked into the error", name, status)
				}
			}
		}
		for _, h := range foreign.seen() {
			t.Errorf("foreign server saw headers %v", h)
		}
	})

	t.Run("destinations must be approved before any resolution", func(t *testing.T) {
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		for name, e := range map[string]gateway.Endpoint{
			"foreign host":     endpoint(t, "evil.test", p.port(), "/v1"),
			"foreign port":     endpoint(t, "example.com", p.port()+1, "/v1"),
			"path outside":     endpoint(t, "example.com", p.port(), "/internal"),
			"prefix sibling":   endpoint(t, "example.com", p.port(), "/v10"),
			"path traversal-y": endpoint(t, "example.com", p.port(), "/v1x/y"),
		} {
			_, err := r.client.Open(context.Background(), call(t, e))
			if pe := perr(t, err); pe.Kind != gateway.ProviderDestination || pe.Sent {
				t.Errorf("%s: %+v", name, pe)
			}
		}
		if r.resolver.lookups("example.com") != 0 || r.resolver.lookups("evil.test") != 0 || p.hits.Load() != 0 {
			t.Fatal("an unapproved destination must not be resolved or contacted")
		}
		if err := r.client.policy.Approve(context.Background(), domain.Scope{}, endpoint(t, "example.com", p.port(), "/v1/sub")); err != nil {
			t.Fatalf("a path below the prefix is approved: %v", err)
		}
		if err := r.client.policy.Approve(context.Background(), domain.Scope{}, endpoint(t, "evil.test", p.port(), "/v1")); !errors.Is(err, gateway.ErrDestination) {
			t.Fatalf("approve: %v", err)
		}
	})

	t.Run("forbidden addresses are refused before dialing", func(t *testing.T) {
		p := newProvider(t, sseOK)
		for _, addr := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
			"::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "224.0.0.1", "192.0.2.5", "198.18.0.1", "240.0.0.1"} {
			r := newRig(t, p, nil, fmt.Sprintf("https://example.com:%d", p.port())) // default PublicOnly
			r.resolver.set("example.com", []string{addr})
			_, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
			if pe := perr(t, err); pe.Kind != gateway.ProviderDestination || pe.Sent {
				t.Errorf("%s: %+v", addr, pe)
			}
		}
		if p.hits.Load() != 0 {
			t.Fatal("a forbidden address was dialed")
		}
		for _, ok := range []string{"8.8.8.8", "2606:4700:4700::1111", "93.184.216.34"} {
			if !PublicOnly(netip.MustParseAddr(ok)) {
				t.Errorf("%s must be allowed", ok)
			}
		}
		// An IP-literal endpoint is judged like a resolved address.
		r := newRig(t, p, nil, "https://127.0.0.1:"+fmt.Sprint(p.port()))
		_, err := r.client.Open(context.Background(), call(t, endpoint(t, "127.0.0.1", p.port(), "/v1")))
		if pe := perr(t, err); pe.Kind != gateway.ProviderDestination {
			t.Errorf("literal: %+v", pe)
		}
	})

	t.Run("a mixed answer is refused as a whole", func(t *testing.T) {
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		r.resolver.set("example.com", []string{"127.0.0.1", "10.0.0.5"})
		_, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
		if pe := perr(t, err); pe.Kind != gateway.ProviderDestination || p.hits.Load() != 0 {
			t.Fatalf("%+v hits=%d", pe, p.hits.Load())
		}
	})

	t.Run("DNS rebinding between calls is caught at the next connection", func(t *testing.T) {
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		r.resolver.set("example.com", []string{"127.0.0.1"}, []string{"169.254.169.254"})
		e := endpoint(t, "example.com", p.port(), "/v1")
		s, err := r.client.Open(context.Background(), call(t, e))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		_, err = r.client.Open(context.Background(), call(t, e))
		if pe := perr(t, err); pe.Kind != gateway.ProviderDestination || pe.Sent {
			t.Fatalf("second call: %+v", pe)
		}
		if p.hits.Load() != 1 {
			t.Fatalf("hits = %d", p.hits.Load())
		}
	})

	t.Run("the dialed address is the validated one and the name is verified by TLS", func(t *testing.T) {
		p := newProvider(t, sseOK)
		// The certificate is valid for example.com only.
		r := newRig(t, p, loopback(), fmt.Sprintf("https://evil.test:%d", p.port()))
		_, err := r.client.Open(context.Background(), call(t, endpoint(t, "evil.test", p.port(), "/v1")))
		if pe := perr(t, err); pe.Kind != gateway.ProviderConnect || pe.Sent {
			t.Fatalf("hostname mismatch: %+v", pe)
		}
		if p.hits.Load() != 0 {
			t.Fatal("an HTTP request was sent over a connection with a wrong server name")
		}
		// Untrusted certificate.
		untrusted := newRig(t, nil, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		_, err = untrusted.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
		if pe := perr(t, err); pe.Kind != gateway.ProviderConnect || pe.Sent || p.hits.Load() != 0 {
			t.Fatalf("untrusted certificate: %+v hits=%d", pe, p.hits.Load())
		}
	})

	t.Run("process proxies are ignored", func(t *testing.T) {
		var proxied atomic.Int64
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1) }))
		defer proxy.Close()
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
			t.Setenv(k, proxy.URL)
		}
		t.Setenv("NO_PROXY", "")
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		s, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		tr := r.client.http.Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Fatal("the transport must not consult the environment for a proxy")
		}
		if proxied.Load() != 0 || p.hits.Load() != 1 {
			t.Fatalf("proxy saw %d requests, provider %d", proxied.Load(), p.hits.Load())
		}
		if !tr.DisableKeepAlives || !tr.DisableCompression {
			t.Fatal("connections must not be reused across calls and compression is disabled")
		}
	})

	t.Run("responses outside the supported stream are refused", func(t *testing.T) {
		cases := map[string]struct {
			h    http.HandlerFunc
			kind gateway.ProviderFault
		}{
			"unauthorized": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad key "+secret, 401) }, gateway.ProviderStatus},
			"rate limited": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "slow down", 429) }, gateway.ProviderStatus},
			"server error": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 503) }, gateway.ProviderStatus},
			"json body": {func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{}")
			}, gateway.ProviderProtocol},
			"compressed": {func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
			}, gateway.ProviderProtocol},
			"no content": {func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, gateway.ProviderStatus},
		}
		for name, c := range cases {
			p := newProvider(t, c.h)
			r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
			_, err := r.client.Open(context.Background(), call(t, endpoint(t, "example.com", p.port(), "/v1")))
			pe := perr(t, err)
			if pe.Kind != c.kind || !pe.Sent {
				t.Errorf("%s: %+v", name, pe)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%+v", pe), "bad key") {
				t.Errorf("%s: provider text leaked into the error", name)
			}
		}
	})

	t.Run("credentials that could inject headers are unusable", func(t *testing.T) {
		p := newProvider(t, sseOK)
		r := newRig(t, p, loopback(), fmt.Sprintf("https://example.com:%d", p.port()))
		for _, bad := range []string{"key\r\nX-Evil: 1", "key with space", "", strings.Repeat("a", 5000), "k\x00y"} {
			c := call(t, endpoint(t, "example.com", p.port(), "/v1"))
			c.Credential = gateway.NewCredential([]byte(bad))
			if _, err := r.client.Open(context.Background(), c); !errors.Is(err, gateway.ErrCredential) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if p.hits.Load() != 0 {
			t.Fatal("an unusable credential reached the provider")
		}
	})

	t.Run("construction is strict", func(t *testing.T) {
		if _, err := NewPolicy(PolicyConfig{}); err == nil {
			t.Error("an empty policy approves nothing and is refused")
		}
		for _, bad := range []ApprovedOrigin{{Origin: "http://example.com"}, {Origin: "https://example.com/"}, {Origin: "https://EXAMPLE.com"}, {Origin: "https://example.com", PathPrefix: "/a/../b"}} {
			if _, err := NewPolicy(PolicyConfig{Origins: []ApprovedOrigin{bad}}); err == nil {
				t.Errorf("%+v was accepted", bad)
			}
		}
		pol, _ := NewPolicy(PolicyConfig{Origins: []ApprovedOrigin{{Origin: "https://example.com"}}})
		if _, err := NewClient(Config{Policy: pol, TLS: &tls.Config{InsecureSkipVerify: true}}); err == nil {
			t.Error("certificate verification cannot be disabled")
		}
		if _, err := NewClient(Config{}); err == nil {
			t.Error("a policy is required")
		}
		u, _ := url.Parse("https://example.com")
		_ = u
	})
}
