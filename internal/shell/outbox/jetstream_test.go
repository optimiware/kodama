package outbox_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/outbox"
)

// cutProxy forwards TCP to the broker and can cut every connection, a broker
// outage the client sees without touching the server.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	cut    bool
	conns  []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close(); p.setCut(true) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.cut {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	return p
}

func (p *cutProxy) setCut(cut bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = cut
	if cut {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}

// TestJetStreamOutboxOutageRedeliveryReconciliation is the real-broker evidence
// for T-188, run when KODAMA_NATS_URL names a NATS server with JetStream
// (skipped otherwise): an outage keeps committed intent and recovery publishes
// it; a lost acknowledgment and a repeated reconciliation deliver the same event
// id again and the consumer applies it once; reconciliation republishes rows the
// drain's backoff left far out.
func TestJetStreamOutboxOutageRedeliveryReconciliation(t *testing.T) {
	raw := os.Getenv("KODAMA_NATS_URL")
	if raw == "" {
		t.Skip("set KODAMA_NATS_URL to a NATS server with JetStream to run the real-broker outbox test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newCutProxy(t, u.Host)
	nc, err := nats.Connect("nats://"+proxy.ln.Addr().String(), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond),
		nats.ReconnectBufSize(-1), nats.UserInfo(u.User.Username(), password(u)))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	const stream = "KODAMA_OUTBOX_TEST"
	js, _ := natsjs.New(nc)
	_ = js.DeleteStream(ctx, stream)
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()
	broker, err := natsbus.Open(ctx, natsbus.Options{Profile: natsbus.Test, Conn: nc,
		JetStream: natsbus.JetStreamConfig{Stream: stream, Subjects: "kodama.v1.outboxtest.>", RetryDelay: 100 * time.Millisecond, Duplicates: time.Second}})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	runs := map[string]int{}
	deliveries := map[string]int{}
	name, _ := natsbus.ConsumerName("shell", "run-admission")
	ledger := &outbox.MemoryLedger{}
	cons := outbox.Consumer{Name: name, Ledger: ledger, Apply: func(_ context.Context, m bus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		runs[m.ResourceID]++
		return nil
	}}
	stop, err := broker.Subscribe(ctx, name, "kodama.v1.outboxtest.*.run.created", func(c context.Context, m bus.Message) error {
		mu.Lock()
		deliveries[m.ID]++
		mu.Unlock()
		return cons.Handle(c, m)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clk := newClock()
	store := outbox.NewMemoryStore()
	w := outbox.Writer{Store: store, Now: clk.now}
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: store, Bus: &timeoutSink{broker}, Now: clk.now, Grace: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	commit := func(run string) string {
		in := intent("run.created", "trg_"+run, run, "")
		in.TenantKey = "outboxtest"
		if err := w.Commit(ctx, put(run, "queued"), in); err != nil {
			t.Fatal(err)
		}
		id, _ := outbox.EventID(tenant, "run.created", "trg_"+run)
		return id
	}

	// Outage: the commit succeeds, publication fails, the row stays.
	proxy.setCut(true)
	idA := commit("run_a")
	if res, _ := p.Drain(ctx); res.Failed != 1 || store.Published(idA) {
		t.Fatalf("outage drain = %+v", res)
	}
	proxy.setCut(false)
	waitFor(t, func() bool { return nc.IsConnected() })
	clk.advance(p.Backoff(1) + time.Second)
	if res, err := p.Drain(ctx); err != nil || res.Published != 1 || !store.Published(idA) {
		t.Fatalf("recovery drain = %+v, %v", res, err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return runs["run_a"] == 1 })

	// Redelivery: an outbox row republished after the broker's duplicate window
	// (the crash between acknowledgment and mark) is a second broker message with
	// the same id; the consumer applies it once.
	idB := commit("run_b")
	store.FailMarkPublished(errors.New("crash"))
	_, _ = p.Drain(ctx)
	store.FailMarkPublished(nil)
	time.Sleep(1500 * time.Millisecond) // beyond the 1s duplicate window
	clk.advance(time.Minute)
	if res, _ := p.Drain(ctx); res.Published != 1 || !store.Published(idB) {
		t.Fatalf("redelivery drain = %+v", res)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return deliveries[idB] >= 2 })
	time.Sleep(300 * time.Millisecond)

	// Reconciliation: a row deferred far out is found and republished.
	idC := commit("run_c")
	clk.advance(2 * time.Minute)
	_ = store.MarkFailed(ctx, tenant, idC, clk.now().Add(24*time.Hour))
	if res, _ := p.Drain(ctx); res.Published != 0 {
		t.Fatalf("drain published a deferred row: %+v", res)
	}
	if res, err := p.Reconcile(ctx); err != nil || res.Published != 1 || !store.Published(idC) {
		t.Fatalf("reconcile = %+v, %v", res, err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return runs["run_c"] == 1 })
	mu.Lock()
	defer mu.Unlock()
	if runs["run_a"] != 1 || runs["run_b"] != 1 || runs["run_c"] != 1 || deliveries[idB] < 2 {
		t.Fatalf("runs = %v deliveries = %v", runs, deliveries)
	}
}

func password(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}

// timeoutSink bounds each publication so an outage fails fast.
type timeoutSink struct{ b bus.Sink }

func (s *timeoutSink) Publish(ctx context.Context, subject string, m bus.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.b.Publish(ctx, subject, m)
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
