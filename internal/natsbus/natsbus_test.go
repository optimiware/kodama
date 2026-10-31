package natsbus_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
)

func TestDeployedProfilesRequireNATS(t *testing.T) {
	dialed := 0
	failing := func(string, ...nats.Option) (*nats.Conn, error) {
		dialed++
		return nil, errors.New("connection refused")
	}
	for _, p := range []natsbus.Profile{natsbus.Test, natsbus.Production} {
		// No broker configured: a startup failure, never the in-process bus.
		b, err := natsbus.Open(t.Context(), natsbus.Options{Profile: p})
		if !errors.Is(err, natsbus.ErrBrokerRequired) || b != nil {
			t.Errorf("%s without NATS = %v, %v; want ErrBrokerRequired", p, b, err)
		}
		// A configured but unreachable broker is a startup failure too.
		b, err = natsbus.Open(t.Context(), natsbus.Options{Profile: p, URL: "nats://127.0.0.1:1", Dial: failing})
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) || b != nil {
			t.Errorf("%s with an unreachable NATS = %v, %v; want ErrBrokerUnavailable", p, b, err)
		}
	}
	if dialed != 2 {
		t.Fatalf("dialed %d times", dialed)
	}
	// Development with an unreachable configured NATS is also an error: only
	// "nothing configured" selects the in-process bus.
	if _, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development, URL: "nats://127.0.0.1:1", Dial: failing}); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("development with a dead NATS = %v", err)
	}
	for _, bad := range []natsbus.Profile{"", "prod", "Production", "staging"} {
		if _, err := natsbus.Open(t.Context(), natsbus.Options{Profile: bad}); !errors.Is(err, natsbus.ErrProfileUnknown) {
			t.Errorf("profile %q = %v", bad, err)
		}
	}
}

func TestDevelopmentFallbackIsFlaggedInProcess(t *testing.T) {
	b, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.InProcess() || !b.DevelopmentOnly() || b.Profile() != natsbus.Development {
		t.Fatalf("broker = in-process %v, development-only %v", b.InProcess(), b.DevelopmentOnly())
	}
	var got []string
	if _, err := b.Subscribe(t.Context(), "kodama-test-x", "kodama.v1.>", func(_ context.Context, m bus.Message) error { got = append(got, m.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context(), "kodama.v1.a.b.x.y", bus.Message{ID: "evt_1", TenantID: "t_a", Type: "x.y"}); err != nil || len(got) != 1 {
		t.Fatalf("in-process publish = %v, %v", got, err)
	}
}

func TestParseProfile(t *testing.T) {
	for _, s := range []string{"development", "test", "production"} {
		if p, err := natsbus.ParseProfile(s); err != nil || string(p) != s {
			t.Errorf("ParseProfile(%q) = %q, %v", s, p, err)
		}
	}
	for _, s := range []string{"", "Test", " test", "prod"} {
		if _, err := natsbus.ParseProfile(s); !errors.Is(err, natsbus.ErrProfileUnknown) {
			t.Errorf("ParseProfile(%q) = %v", s, err)
		}
	}
	if natsbus.Development.RequiresBroker() || !natsbus.Test.RequiresBroker() || !natsbus.Production.RequiresBroker() {
		t.Fatal("broker requirement per profile")
	}
}

func TestConsumerName(t *testing.T) {
	got, err := natsbus.ConsumerName("shell", "run-admission")
	if err != nil || got != "kodama-shell-run-admission" {
		t.Fatalf("= %q, %v", got, err)
	}
	for _, bad := range [][2]string{{"", "x"}, {"shell", ""}, {"Shell", "x"}, {"shell", "a.b"}, {"shell", "a*"}, {"shell", "a>"}, {"shell", "a b"}, {"shell", "-x"}, {"shell", "x--y"}} {
		if n, err := natsbus.ConsumerName(bad[0], bad[1]); err == nil {
			t.Errorf("ConsumerName(%q,%q) = %q", bad[0], bad[1], n)
		}
	}
}

// TestJetStreamBus runs against a NATS server with JetStream enabled when
// KODAMA_NATS_URL names one (skipped otherwise): a publication is acknowledged
// by the stream, the event ID deduplicates a repeated publication inside the
// broker's window, a handler error redelivers, a settled delivery is not
// delivered again, and a durable consumer resumes after a stop without a loss
// or a repeat.
func TestJetStreamBus(t *testing.T) {
	url := os.Getenv("KODAMA_NATS_URL")
	if url == "" {
		t.Skip("set KODAMA_NATS_URL to a NATS server with JetStream to run the JetStream adapter test")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const stream = "KODAMA_NATSBUS_TEST"
	js, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	_ = js.DeleteStream(ctx, stream)
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()

	b, err := natsbus.Open(ctx, natsbus.Options{Profile: natsbus.Test, Conn: nc, JetStream: natsbus.JetStreamConfig{Stream: stream, Subjects: "kodama.v1.natsbustest.>", RetryDelay: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if b.InProcess() {
		t.Fatal("a test profile with NATS composed the in-process bus")
	}
	var mu sync.Mutex
	calls := map[string]int{}
	handler := func(_ context.Context, m bus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		calls[m.ID]++
		if m.ID == "evt_retry" && calls[m.ID] == 1 {
			return errors.New("not yet")
		}
		return nil
	}
	const name = "kodama-test-dispatch"
	stop, err := b.Subscribe(ctx, name, "kodama.v1.natsbustest.*.git.refs_updated", handler)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(ctx, "bad.name", "kodama.v1.natsbustest.>", handler); err == nil {
		t.Fatal("a consumer name with a dot was accepted")
	}
	msg := func(id string) bus.Message {
		return bus.Message{ID: id, TenantID: "t_acme", ProjectID: "p_erp", Type: "git.refs_updated", ResourceType: "git_push", ResourceID: "rcv_1",
			Actor: bus.Actor{Type: "user", ID: "u_1"}, OccurredAt: time.Now().UTC(), Data: []byte(`{"a":1}`)}
	}
	subject := "kodama.v1.natsbustest.erp.git.refs_updated"
	for _, id := range []string{"evt_once", "evt_once", "evt_retry"} {
		if err := b.Publish(ctx, subject, msg(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Publish(ctx, "kodama.v1.natsbustest._.repository.created", msg("evt_other")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls["evt_once"] == 1 && calls["evt_retry"] >= 2 })
	stop()
	// Published while the consumer is stopped; the durable consumer resumes at it
	// and does not repeat the settled ones.
	if err := b.Publish(ctx, subject, msg("evt_while_stopped")); err != nil {
		t.Fatal(err)
	}
	stop2, err := b.Subscribe(ctx, name, "kodama.v1.natsbustest.*.git.refs_updated", handler)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls["evt_while_stopped"] == 1 })
	time.Sleep(500 * time.Millisecond) // a wrongly repeated delivery would show now
	mu.Lock()
	defer mu.Unlock()
	if calls["evt_once"] != 1 || calls["evt_retry"] != 2 || calls["evt_other"] != 0 || calls["evt_while_stopped"] != 1 {
		t.Fatalf("deliveries = %v", calls)
	}
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

func TestOpenAgainstUnreachableURLFailsAtStartup(t *testing.T) {
	// A real dial of a closed port: the deployed profile stops, it does not fall
	// back to memory.
	_, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Production, URL: "nats://127.0.0.1:1"})
	if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamSubjectsMustLieUnderTheRoot(t *testing.T) {
	_, err := natsbus.NewJetStream(t.Context(), &nats.Conn{}, natsbus.JetStreamConfig{Subjects: "other.>"})
	if err == nil {
		t.Fatal("a stream outside kodama.v1 was accepted")
	}
	if _, err := natsbus.NewJetStream(t.Context(), nil, natsbus.JetStreamConfig{}); err == nil {
		t.Fatal("a nil connection was accepted")
	}
}
