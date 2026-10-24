package webhook_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
)

// Test_webhook_destination_policy exercises the destination and redirect
// restrictions: unapproved and non-HTTPS destinations, user-info, proxy bypass,
// forbidden resolved addresses and every redirect (including one to a refused
// address) end without a second connection and without credential forwarding.
// Local HTTP/TLS fixtures only; no PostgreSQL, NATS, S3 or Git.
func Test_webhook_destination_policy(t *testing.T) {
	policy, err := webhook.NewPolicy(webhook.PolicyConfig{Hosts: []string{"hooks.example.com", "*.chat.example.com"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("approval refuses unapproved and malformed destinations before any dial", func(t *testing.T) {
		approved := []string{"https://hooks.example.com/a", "https://team.chat.example.com/x/y", "https://HOOKS.example.com/a"}
		refused := map[string]string{
			"http://hooks.example.com/a":               "non-HTTPS",
			"https://user:pw@hooks.example.com/a":      "user-info",
			"https://hooks.example.com/a?token=1":      "query",
			"https://hooks.example.com/a#f":            "fragment",
			"https://hooks.example.com:8443/a":         "port outside the policy",
			"https://other.example.com/a":              "host outside the allow-list",
			"https://chat.example.com/a":               "the bare suffix is not a *.suffix match",
			"https://evilhooks.example.com/a":          "a prefix of an approved host",
			"https://hooks.example.com.evil.test/a":    "a suffix trick",
			"https://127.0.0.1/a":                      "loopback IP literal",
			"https://[::1]/a":                          "IPv6 loopback literal",
			"https://169.254.169.254/latest/meta-data": "cloud metadata literal",
			"https://10.0.0.5/a":                       "private literal",
			"https://[::ffff:127.0.0.1]/a":             "IPv4-mapped loopback literal",
			"":                                         "empty",
		}
		for _, u := range approved {
			if err := policy.Approve(scopeA, u); err != nil {
				t.Errorf("%q: %v", u, err)
			}
		}
		for u, why := range refused {
			if err := policy.Approve(scopeA, u); !errors.Is(err, chatops.ErrPolicy) {
				t.Errorf("%s (%q): got %v, want a policy refusal", why, u, err)
			}
		}
		// With no host list any public name passes approval; the address is
		// checked only at dial time, never by a DNS precheck here.
		open, _ := webhook.NewPolicy(webhook.PolicyConfig{})
		if err := open.Approve(scopeA, "https://anything.example.org/h"); err != nil {
			t.Fatal(err)
		}
		if err := open.Approve(scopeA, "https://8.8.8.8/h"); err != nil {
			t.Fatalf("a public IP literal was refused: %v", err)
		}
	})

	t.Run("policy construction refuses blanket and dangerous allowances", func(t *testing.T) {
		for name, cfg := range map[string]webhook.PolicyConfig{
			"bad port":          {Ports: []int{0}},
			"wildcard host":     {Hosts: []string{"*"}},
			"empty suffix":      {Hosts: []string{"*."}},
			"path in host":      {Hosts: []string{"a.example/x"}},
			"no tenant":         {PrivateEndpoints: []webhook.PrivateEndpoint{{Addr: netip.MustParseAddrPort("10.0.0.5:443")}}},
			"zero address":      {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.AddrPort{}}}},
			"no port":           {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.5"), 0)}}},
			"unspecified":       {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("0.0.0.0:443")}}},
			"link-local":        {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("169.254.169.254:80")}}},
			"ipv6 link-local":   {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("[fe80::1]:443")}}},
			"multicast":         {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("224.0.0.1:443")}}},
			"invalid project":   {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: domain.Scope{TenantID: "t1", ProjectID: "bad id"}, Addr: netip.MustParseAddrPort("10.0.0.5:443")}}},
			"zoned ipv6":        {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("[fe80::1%eth0]:443")}}},
			"mapped link-local": {PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: netip.MustParseAddrPort("[::ffff:169.254.169.254]:80")}}},
		} {
			if _, err := webhook.NewPolicy(cfg); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if _, err := webhook.NewClient(webhook.Config{}); err == nil {
			t.Error("a client without a policy was accepted")
		}
	})

	t.Run("a non-HTTPS or malformed endpoint is never sent", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		c := client(t, webhook.Config{}, rc)
		plain := strings.Replace(rc.URL(), "https://", "http://", 1)
		for _, u := range []string{plain, "https://user:pw@" + rc.AddrPort().String() + "/hook", rc.URL() + "?token=1", "not a url"} {
			out := send(t, c, request(scopeA, u, ""), 2*time.Second)
			if out.Delivered || out.Class != chatops.ClassPolicyRefused {
				t.Errorf("%q: %+v", u, out)
			}
		}
		if rc.Count() != 0 {
			t.Fatalf("a refused endpoint reached the receiver %d times", rc.Count())
		}
	})

	t.Run("a resolved forbidden address is refused before connect", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		// "localhost" passes approval (a name) and resolves to loopback, which is
		// refused at dial time: no allowance covers it.
		p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: []int{int(rc.AddrPort().Port())}})
		if err != nil {
			t.Fatal(err)
		}
		c := client(t, webhook.Config{Policy: p, RootCAs: rc.Pool()})
		out := send(t, c, request(scopeA, fmt.Sprintf("https://localhost:%d/hook", rc.AddrPort().Port()), ""), 3*time.Second)
		if out.Delivered || out.Class != chatops.ClassPolicyRefused || out.Uncertain {
			t.Fatalf("outcome = %+v", out)
		}
		if rc.Count() != 0 {
			t.Fatalf("the receiver was reached %d times through a refused address", rc.Count())
		}
	})

	t.Run("private allowances are exact, tenant-scoped and project-scoped", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		other := chatopstest.NewReceiver(t, nil)
		ap := rc.AddrPort()
		ports := []int{int(ap.Port()), int(other.AddrPort().Port())}
		p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: ports, PrivateEndpoints: []webhook.PrivateEndpoint{
			{Scope: domain.Scope{TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID}, Addr: ap}}})
		if err != nil {
			t.Fatal(err)
		}
		c := client(t, webhook.Config{Policy: p, RootCAs: rc.Pool()})
		if out := send(t, c, request(scopeA, rc.URL(), ""), 3*time.Second); !out.Delivered {
			t.Fatalf("the allowed project was refused: %+v", out)
		}
		for name, r := range map[string]chatops.SendRequest{
			"another project of the tenant": request(chatopstest.ScopeA2, rc.URL(), ""),
			"another tenant":                request(scopeB, rc.URL(), ""),
			"another address":               request(scopeA, other.URL(), ""),
		} {
			if out := send(t, c, r, 3*time.Second); out.Delivered || out.Class != chatops.ClassPolicyRefused {
				t.Errorf("%s: %+v", name, out)
			}
		}
		if other.Count() != 0 || rc.Count() != 1 {
			t.Fatalf("receivers reached: allowed=%d other=%d", rc.Count(), other.Count())
		}
	})

	t.Run("every redirect is refused without a second connection or credential forwarding", func(t *testing.T) {
		target := chatopstest.NewReceiver(t, nil) // a refused address: not allowed by any policy below
		allowedTarget := chatopstest.NewReceiver(t, nil)
		for name, location := range map[string]string{
			"to a refused address":   target.URL(),
			"to an allowed address":  allowedTarget.URL(),
			"to a relative path":     "/elsewhere",
			"to a plaintext address": strings.Replace(target.URL(), "https://", "http://", 1),
		} {
			t.Run(name, func(t *testing.T) {
				var seenAuth atomic.Value
				first := chatopstest.NewReceiver(t, func(_ int, w http.ResponseWriter, r *http.Request) {
					seenAuth.Store(r.Header.Get("Authorization"))
					w.Header().Set("Location", location)
					w.WriteHeader(http.StatusFound)
				})
				p, err := webhook.NewPolicy(webhook.PolicyConfig{
					Ports: []int{int(first.AddrPort().Port()), int(allowedTarget.AddrPort().Port())},
					PrivateEndpoints: []webhook.PrivateEndpoint{
						{Scope: domain.Scope{TenantID: scopeA.TenantID}, Addr: first.AddrPort()},
						{Scope: domain.Scope{TenantID: scopeA.TenantID}, Addr: allowedTarget.AddrPort()},
					}})
				if err != nil {
					t.Fatal(err)
				}
				src := &source{fn: func(string, chatops.Recipient) (chatops.Credential, error) {
					return chatops.Credential{Bearer: "secret-token-1"}, nil
				}}
				c := client(t, webhook.Config{Policy: p, RootCAs: first.Pool(), Credentials: src})
				beforeT, beforeA := target.Count(), allowedTarget.Count()
				out := send(t, c, request(scopeA, first.URL(), "cred/ops"), 3*time.Second)
				if out.Delivered || out.Class != chatops.ClassRedirectRefused || out.HTTPStatus != 302 || !out.Class.Permanent() {
					t.Fatalf("outcome = %+v", out)
				}
				if first.Count() != 1 {
					t.Fatalf("first receiver saw %d requests", first.Count())
				}
				if target.Count() != beforeT || allowedTarget.Count() != beforeA {
					t.Fatal("a redirect was followed to a second connection")
				}
				if got := seenAuth.Load(); got != "Bearer secret-token-1" {
					t.Fatalf("the first hop's authorization = %v", got)
				}
			})
		}
	})

	t.Run("the environment proxy is never used", func(t *testing.T) {
		var hits atomic.Int64
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				hits.Add(1)
				c.Close()
			}
		}()
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
			t.Setenv(name, "http://"+ln.Addr().String())
		}
		t.Setenv("NO_PROXY", "")
		rc := chatopstest.NewReceiver(t, nil)
		c := client(t, webhook.Config{}, rc)
		out := send(t, c, request(scopeA, rc.URL(), ""), 3*time.Second)
		if !out.Delivered || rc.Count() != 1 {
			t.Fatalf("direct delivery failed: %+v receiver=%d", out, rc.Count())
		}
		if hits.Load() != 0 {
			t.Fatalf("the proxy saw %d connections", hits.Load())
		}
		// A destination that is only reachable through the proxy host fails
		// closed rather than leaving the policy.
		pu, _ := url.Parse("https://" + ln.Addr().String() + "/x")
		bad := send(t, c, request(scopeA, pu.String(), ""), 2*time.Second)
		if bad.Delivered || bad.Class != chatops.ClassPolicyRefused || hits.Load() != 0 {
			t.Fatalf("outcome = %+v proxy hits = %d", bad, hits.Load())
		}
	})
}
