package chatopstest

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
)

// Bus is the real outbox -> JetStream -> inbox path over the environment's
// database and the shared NATS broker: a fresh stream, a publisher and the
// durable ChatOps consumer, wired through the production adapters. Streams,
// subjects and durable names carry the t041 prefix and a random suffix and are
// removed on cleanup.
type Bus struct {
	Env       *Env
	Broker    *eventstest.Broker
	Config    jetstream.Config
	Conn      *nats.Conn
	Publisher *events.Publisher
	Consumer  *jetstream.Consumer
}

// NewBus provisions the stream and the consumer. It skips (or fails with
// KODAMA_REQUIRE_POSTGRES=1) when the broker is not configured.
func (e *Env) NewBus(t testing.TB) *Bus {
	t.Helper()
	broker := eventstest.NewBroker(t)
	cfg := broker.StreamConfig(t, time.Minute)
	nc := broker.Connect(t, broker.Addr())
	pub, err := jetstream.NewPublisher(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pub.EnsureStream(ctx); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	publisher, err := events.NewPublisher(e.Events, e.Events, pub, events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: 2 * time.Second, BatchSize: 50, Interval: 50 * time.Millisecond,
		Retry: events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3}, Clock: e.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := jetstream.NewConsumer(nc, cfg, jetstream.ConsumerConfig{
		Consumer: "chatops", Durable: Prefix + "_chatops_" + RandomHex(t, 3),
		AckWait: 5 * time.Second, MaxAckPending: 50, FetchBatch: 10, FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond,
	}, e.Events, e.Events, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return &Bus{Env: e, Broker: broker, Config: cfg, Conn: nc, Publisher: publisher, Consumer: consumer}
}

// Pump publishes and consumes until cond holds or the timeout elapses.
func (b *Bus) Pump(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		_, _ = b.Publisher.RunOnce(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = b.Consumer.RunOnce(ctx)
		cancel()
	}
}
