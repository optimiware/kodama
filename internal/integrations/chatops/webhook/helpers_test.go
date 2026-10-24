package webhook_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
)

var (
	scopeA = chatopstest.ScopeA
	scopeB = chatopstest.ScopeB
	t0     = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
)

// request builds an attempt of a credential-free destination at the endpoint.
func request(scope domain.Scope, endpoint, credential string) chatops.SendRequest {
	d := chatops.Destination{Scope: scope, ID: "cdst_t041_1", Name: "ops", Endpoint: endpoint, Events: []string{"gate.requested"}, Credential: credential,
		Enabled: true, BindingRevision: 3, Version: 1, CreatedAt: t0, UpdatedAt: t0}
	return chatops.SendRequest{Scope: scope, Destination: d, DeliveryID: "cdl_t041_1", IdempotencyKey: "cdl_t041_1", EventID: "evt_1", EventType: "gate.requested",
		Attempt: 1, Payload: []byte(`{"schemaVersion":1}`)}
}

// client builds a client allowing exactly the receivers' addresses for tenant A.
func client(t *testing.T, cfg webhook.Config, rcs ...*chatopstest.Receiver) *webhook.Client {
	t.Helper()
	var private []webhook.PrivateEndpoint
	ports := []int{443}
	for _, rc := range rcs {
		private = append(private, webhook.PrivateEndpoint{Scope: domain.Scope{TenantID: scopeA.TenantID}, Addr: rc.AddrPort()})
		ports = append(ports, int(rc.AddrPort().Port()))
	}
	if cfg.Policy == nil {
		p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: ports, PrivateEndpoints: private})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Policy = p
	}
	if cfg.RootCAs == nil && len(rcs) > 0 {
		cfg.RootCAs = rcs[0].Pool()
	}
	c, err := webhook.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func send(t *testing.T, c *webhook.Client, r chatops.SendRequest, timeout time.Duration) chatops.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Send(ctx, r)
}

// closedAddr returns an address nothing listens on.
func closedAddr(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ap := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	return ap
}

// source is a recording credential source.
type source struct {
	mu    sync.Mutex
	calls []chatops.Recipient
	refs  []string
	fn    func(ref string, r chatops.Recipient) (chatops.Credential, error)
}

func (s *source) Resolve(_ context.Context, _ domain.Scope, ref string, r chatops.Recipient) (chatops.Credential, error) {
	s.mu.Lock()
	s.calls = append(s.calls, r)
	s.refs = append(s.refs, ref)
	s.mu.Unlock()
	return s.fn(ref, r)
}

// mustPolicy builds a policy allowing exactly the receivers' addresses for tenant A.
func mustPolicy(t *testing.T, rcs ...*chatopstest.Receiver) *webhook.Policy {
	t.Helper()
	var private []webhook.PrivateEndpoint
	ports := []int{443}
	for _, rc := range rcs {
		private = append(private, webhook.PrivateEndpoint{Scope: domain.Scope{TenantID: scopeA.TenantID}, Addr: rc.AddrPort()})
		ports = append(ports, int(rc.AddrPort().Port()))
	}
	p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: ports, PrivateEndpoints: private})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
