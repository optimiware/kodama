package bus_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/shell/bus"
)

// TestJetStreamBus runs against a NATS server with JetStream enabled when
// KODAMA_NATS_URL names one (skipped otherwise): a publication is acknowledged
// by the stream, the event ID deduplicates a repeated publication inside the
// broker's window, a handler error redelivers, and a settled delivery is not
// delivered again.
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const stream = "KODAMA_BUS_TEST"
	js, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	_ = js.DeleteStream(ctx, stream)
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()

	b, err := bus.NewJetStream(ctx, nc, bus.JetStreamConfig{Stream: stream, RetryDelay: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := map[string]int{}
	failFirst := errors.New("not yet")
	stop, err := b.Subscribe(ctx, "dispatch-test", "kodama.v1.*.*.git.refs_updated", func(_ context.Context, m bus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		calls[m.ID]++
		if m.ID == "evt_retry" && calls[m.ID] == 1 {
			return failFirst
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	msg := func(id string) bus.Message {
		return bus.Message{ID: id, TenantID: "t_acme", ProjectID: "p_erp", Type: "git.refs_updated", ResourceType: "git_push", ResourceID: "rcv_1",
			Actor: bus.Actor{Type: "user", ID: "u_1"}, OccurredAt: time.Now().UTC(), Data: []byte(`{"a":1}`)}
	}
	subject := "kodama.v1.acme.erp.git.refs_updated"
	for _, id := range []string{"evt_once", "evt_once", "evt_retry"} {
		if err := b.Publish(ctx, subject, msg(id)); err != nil {
			t.Fatal(err)
		}
	}
	// Another subject of the stream is not delivered to this consumer.
	if err := b.Publish(ctx, "kodama.v1.acme._.repository.created", msg("evt_other")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := calls["evt_once"] == 1 && calls["evt_retry"] >= 2
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a wrongly repeated delivery would show now
	mu.Lock()
	defer mu.Unlock()
	if calls["evt_once"] != 1 || calls["evt_retry"] != 2 || calls["evt_other"] != 0 {
		t.Fatalf("deliveries = %v", calls)
	}
}
