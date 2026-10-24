package protocol_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/protocol"
	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
)

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	lookups map[string]int
}

func newResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][]netip.Addr{}, lookups: map[string]int{}}
}

func (f *fakeResolver) set(host string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = nil
	for _, a := range addrs {
		f.answers[host] = append(f.answers[host], netip.MustParseAddr(a))
	}
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups[host]++
	a, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return append([]netip.Addr(nil), a...), nil
}

func (f *fakeResolver) count(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups[host]
}

func dests(t *testing.T, r protocol.Resolver, private bool, allowed ...string) *protocol.Destinations {
	t.Helper()
	d, err := protocol.NewDestinations(protocol.DestinationConfig{Allowed: allowed, AllowPrivate: private, Resolver: r})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newRemote(t *testing.T, d *protocol.Destinations, fx *mcpfixture.Server) *protocol.Remote {
	t.Helper()
	r, err := protocol.NewRemote(protocol.RemoteConfig{Destinations: d, RootCAs: fx.Roots(), ConnectTimeout: 5 * time.Second, MaxResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func target(fx *mcpfixture.Server, endpoint string) mcpproxy.RemoteTarget {
	return mcpproxy.RemoteTarget{Endpoint: endpoint, Recipient: fx.Origin(), Bearer: []byte(fx.Token())}
}

// named is a target addressed by host name, with the matching recipient.
func named(fx *mcpfixture.Server, host string) mcpproxy.RemoteTarget {
	return mcpproxy.RemoteTarget{Endpoint: "https://" + host + ":" + fx.Port() + "/mcp", Recipient: "https://" + host + ":" + fx.Port(), Bearer: []byte(fx.Token())}
}

func Test_destination_checks(t *testing.T) {
	t.Run("approval is exact host and port over https", func(t *testing.T) {
		d := dests(t, newResolver(), false, "mcp.example.com", "mcp2.example.com:8443")
		for ep, ok := range map[string]bool{
			"https://mcp.example.com/mcp":      true,
			"https://MCP.example.com:443/mcp":  true,
			"https://mcp2.example.com:8443/x":  true,
			"https://mcp2.example.com/x":       false,
			"https://evil.example.com/mcp":     false,
			"https://mcp.example.com.evil/mcp": false,
			"http://mcp.example.com/mcp":       false,
			"https://u:p@mcp.example.com/mcp":  false,
			"https://mcp.example.com:444/mcp":  false,
		} {
			err := d.Approve(ep)
			if ok && err != nil || !ok && !errors.Is(err, mcpproxy.ErrDestination) {
				t.Errorf("%s: got %v, want approved=%v", ep, err, ok)
			}
		}
		if err := dests(t, newResolver(), false).Approve("https://mcp.example.com/mcp"); !errors.Is(err, mcpproxy.ErrDestination) {
			t.Error("an empty policy must approve nothing")
		}
	})

	t.Run("address classes: mixed answers, IPv6, metadata and private ranges", func(t *testing.T) {
		res := newResolver()
		res.set("mcp.example.com", "93.184.216.34")
		res.set("mixed.example.com", "93.184.216.34", "10.0.0.5")
		res.set("v6.example.com", "2606:2800:220:1:248:1893:25c8:1946")
		res.set("v6loop.example.com", "::1")
		res.set("mapped.example.com", "::ffff:127.0.0.1")
		res.set("meta.example.com", "169.254.169.254")
		res.set("linklocal6.example.com", "fe80::1")
		res.set("private.example.com", "192.168.1.9")
		res.set("cgnat.example.com", "100.64.0.1")
		res.set("zero.example.com", "0.0.0.0")
		res.set("multicast.example.com", "224.0.0.1")
		res.set("empty.example.com")
		all := []string{"mcp.example.com", "mixed.example.com", "v6.example.com", "v6loop.example.com", "mapped.example.com", "meta.example.com",
			"linklocal6.example.com", "private.example.com", "cgnat.example.com", "zero.example.com", "multicast.example.com", "empty.example.com"}
		strict := dests(t, res, false, all...)
		lenient := dests(t, res, true, all...)
		for host, want := range map[string][2]bool{ // {strict allows, private-permitting allows}
			"mcp.example.com": {true, true}, "v6.example.com": {true, true},
			"mixed.example.com": {false, true}, "v6loop.example.com": {false, true}, "mapped.example.com": {false, true},
			"private.example.com": {false, true}, "cgnat.example.com": {false, true},
			"meta.example.com": {false, false}, "linklocal6.example.com": {false, false}, "zero.example.com": {false, false},
			"multicast.example.com": {false, false}, "empty.example.com": {false, false},
		} {
			if _, err := strict.Resolve(context.Background(), host); (err == nil) != want[0] {
				t.Errorf("strict %s: %v, want allowed=%v", host, err, want[0])
			}
			if _, err := lenient.Resolve(context.Background(), host); (err == nil) != want[1] {
				t.Errorf("private-permitting %s: %v, want allowed=%v", host, err, want[1])
			}
		}
	})

	t.Run("a mixed DNS answer cannot select the internal address when connecting", func(t *testing.T) {
		fx := mcpfixture.New(t, mcpfixture.Options{})
		res := newResolver()
		res.set("example.com", "93.184.216.34", "127.0.0.1")
		d := dests(t, res, false, "example.com:"+fx.Port())
		_, err := newRemote(t, d, fx).Open(context.Background(), named(fx, "example.com"))
		if err == nil {
			t.Fatal("connected through a mixed answer")
		}
		if reqs := fx.Requests(); len(reqs) != 0 {
			t.Fatalf("the fixture received %d requests", len(reqs))
		}
	})

	t.Run("the validated answer is dialed directly: one lookup per dial, no rebinding", func(t *testing.T) {
		fx := mcpfixture.New(t, mcpfixture.Options{})
		res := newResolver()
		res.set("example.com", "127.0.0.1")
		d := dests(t, res, true, "example.com:"+fx.Port())
		conn, err := newRemote(t, d, fx).Open(context.Background(), named(fx, "example.com"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// The address now rebinds to a prohibited one; the open connection and
		// any new dial must not follow it.
		res.set("example.com", "169.254.169.254")
		if _, err := conn.ListTools(context.Background(), ""); err != nil {
			t.Fatalf("an established connection keeps its validated peer: %v", err)
		}
		_, err = newRemote(t, d, fx).Open(context.Background(), named(fx, "example.com"))
		if err == nil {
			t.Fatal("a rebound answer was dialed")
		}
	})

	t.Run("ambient proxies are not used", func(t *testing.T) {
		var proxied int
		var mu sync.Mutex
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			proxied++
			mu.Unlock()
			http.Error(w, "proxy", http.StatusBadGateway)
		}))
		defer proxy.Close()
		t.Setenv("HTTPS_PROXY", proxy.URL)
		t.Setenv("HTTP_PROXY", proxy.URL)
		t.Setenv("ALL_PROXY", proxy.URL)
		t.Setenv("NO_PROXY", "")
		fx := mcpfixture.New(t, mcpfixture.Options{})
		d := dests(t, newResolver(), true, fx.HostPort())
		conn, err := newRemote(t, d, fx).Open(context.Background(), target(fx, fx.URL()))
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		mu.Lock()
		defer mu.Unlock()
		if proxied != 0 {
			t.Fatalf("%d requests went through an ambient proxy", proxied)
		}
	})

	t.Run("redirects are not followed and never carry the credential", func(t *testing.T) {
		fx := mcpfixture.New(t, mcpfixture.Options{})
		sink := mcpfixture.NewSink(t)
		fx.RedirectTo(sink)
		d := dests(t, newResolver(), true, fx.HostPort())
		if _, err := newRemote(t, d, fx).Open(context.Background(), target(fx, fx.RedirectURL())); err == nil {
			t.Fatal("opened through a redirect")
		}
		if got := sink.Requests(); len(got) != 0 {
			t.Fatalf("the redirect target received %d requests: %+v", len(got), got)
		}
	})

	t.Run("a certificate for another name is refused", func(t *testing.T) {
		fx := mcpfixture.New(t, mcpfixture.Options{})
		res := newResolver()
		res.set("localhost.test", "127.0.0.1") // the fixture certificate does not name this host
		d := dests(t, res, true, "localhost.test:"+fx.Port())
		tg := target(fx, "https://localhost.test:"+fx.Port()+"/mcp")
		tg.Recipient = "https://localhost.test:" + fx.Port()
		if _, err := newRemote(t, d, fx).Open(context.Background(), tg); err == nil {
			t.Fatal("connected despite a TLS name mismatch")
		}
		if reqs := fx.Requests(); len(reqs) != 0 {
			t.Fatalf("the credential reached a server it was not valid for: %d requests", len(reqs))
		}
	})

	t.Run("an unapproved host is never contacted and a wrong recipient discloses nothing", func(t *testing.T) {
		fx := mcpfixture.New(t, mcpfixture.Options{})
		d := dests(t, newResolver(), true, "other.example.com")
		if _, err := newRemote(t, d, fx).Open(context.Background(), target(fx, fx.URL())); !errors.Is(err, mcpproxy.ErrDestination) {
			t.Fatalf("unapproved: %v", err)
		}
		d = dests(t, newResolver(), true, fx.HostPort())
		tg := target(fx, fx.URL())
		tg.Recipient = "https://other.example.com"
		if _, err := newRemote(t, d, fx).Open(context.Background(), tg); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("wrong recipient: %v", err)
		}
		if reqs := fx.Requests(); len(reqs) != 0 {
			t.Fatalf("requests reached the server: %d", len(reqs))
		}
	})
}
