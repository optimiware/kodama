package eventstest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/jetstream"
)

// Transport is an events.BrokerPublisher together with the means to deliver what
// it accepted to subscribed consumers, as one transport would.
type Transport interface {
	events.BrokerPublisher
	// Subscribe attaches a consumer that receives every message accepted from
	// now on, through the given resolver and processor.
	Subscribe(consumer string, proc events.Processor) error
	// Deliver runs delivery until the transport has nothing ready and returns how
	// many deliveries it settled. Redelivery after an unsettled message may need
	// further calls.
	Deliver(ctx context.Context) (int, error)
}

// OpenTransport builds a broker whose subscribers resolve subject tokens with
// resolver.
type OpenTransport func(t testing.TB, resolver events.SubjectResolver) Transport

type staticResolver map[string]domain.TenantID

func (r staticResolver) ResolveSubjectToken(_ context.Context, token string) (domain.TenantID, error) {
	if t, ok := r[token]; ok {
		return t, nil
	}
	return "", events.ErrUnknownSubject
}

type delivered struct {
	Tenant  domain.TenantID
	Payload string
}

// recorder is a Processor that records deliveries and can refuse to record a
// durable outcome (a processor error) a number of times.
type recorder struct {
	mu      sync.Mutex
	got     []delivered
	refuse  int
	refused int
}

func (r *recorder) Process(_ context.Context, d events.Delivery) (events.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refuse > 0 {
		r.refuse--
		r.refused++
		return events.Outcome{}, errors.New("no durable outcome could be recorded")
	}
	r.got = append(r.got, delivered{Tenant: d.Tenant, Payload: string(d.Payload)})
	return events.Outcome{Kind: events.OutcomeCompleted}, nil
}

func (r *recorder) deliveries() []delivered {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivered(nil), r.got...)
}

const (
	tokenA = "a0b1c2d3e4f5a6b7c8d9e0f1"
	tokenB = "b0b1c2d3e4f5a6b7c8d9e0f1"
)

// BrokerContract is the behavior every events.BrokerPublisher adapter shares:
// a nil Publish is a durable acknowledgment, the consumer sees the trusted
// tenant of the subject token and the unchanged payload, a repeated message ID
// is delivered once, an unmapped subject never reaches a processor, an
// unsettled delivery is redelivered, and a message the broker can never accept
// is a permanent failure. Exactly-once effects remain the inbox's job; this
// suite does not assert delivery order or dedup windows.
func BrokerContract(t *testing.T, open OpenTransport) {
	t.Helper()
	ctx := context.Background()
	resolver := staticResolver{tokenA: TenantA, tokenB: TenantB}
	msg := func(id, token string, data string) events.Message {
		return events.Message{ID: id, SubjectToken: token, EventType: "work_item.transitioned", Data: []byte(data)}
	}
	eventually := func(t *testing.T, b Transport, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			if _, err := b.Deliver(ctx); err != nil {
				t.Logf("deliver: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	t.Run("an acknowledged message reaches the subscriber under its trusted tenant", func(t *testing.T) {
		b := open(t, resolver)
		rec := &recorder{}
		if err := b.Subscribe("ledger", rec); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, msg("evt_b1", tokenA, `{"id":"evt_b1"}`)); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, msg("evt_b2", tokenB, `{"id":"evt_b2"}`)); err != nil {
			t.Fatal(err)
		}
		eventually(t, b, "two deliveries", func() bool { return len(rec.deliveries()) >= 2 })
		got := map[string]domain.TenantID{}
		for _, d := range rec.deliveries() {
			got[d.Payload] = d.Tenant
		}
		if got[`{"id":"evt_b1"}`] != TenantA || got[`{"id":"evt_b2"}`] != TenantB {
			t.Fatalf("tenants of the deliveries: %v", got)
		}
	})

	t.Run("a repeated message ID is delivered once", func(t *testing.T) {
		b := open(t, resolver)
		rec := &recorder{}
		if err := b.Subscribe("ledger", rec); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := b.Publish(ctx, msg("evt_b3", tokenA, `{"id":"evt_b3"}`)); err != nil {
				t.Fatalf("publication %d: %v", i, err)
			}
		}
		eventually(t, b, "the delivery", func() bool { return len(rec.deliveries()) >= 1 })
		for i := 0; i < 3; i++ {
			if _, err := b.Deliver(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(rec.deliveries()); n != 1 {
			t.Fatalf("one message ID was delivered %d times", n)
		}
	})

	t.Run("an unsettled delivery is redelivered", func(t *testing.T) {
		b := open(t, resolver)
		rec := &recorder{refuse: 1}
		if err := b.Subscribe("ledger", rec); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, msg("evt_b4", tokenA, `{"id":"evt_b4"}`)); err != nil {
			t.Fatal(err)
		}
		eventually(t, b, "the redelivery", func() bool { return len(rec.deliveries()) >= 1 })
		if rec.refused != 1 {
			t.Fatalf("the processor was asked to refuse once, refused %d times", rec.refused)
		}
	})

	t.Run("a subject token that maps to no tenant never reaches a processor", func(t *testing.T) {
		b := open(t, resolver)
		rec := &recorder{}
		if err := b.Subscribe("ledger", rec); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, msg("evt_b5", "ffffffffffffffffffffffff", `{"id":"evt_b5"}`)); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, msg("evt_b6", tokenB, `{"id":"evt_b6"}`)); err != nil {
			t.Fatal(err)
		}
		eventually(t, b, "the delivery after the unmapped one", func() bool { return len(rec.deliveries()) >= 1 })
		for _, d := range rec.deliveries() {
			if d.Payload == `{"id":"evt_b5"}` {
				t.Fatal("a message of an unknown subject token was processed")
			}
		}
	})

	t.Run("a message the broker can never accept is a permanent failure", func(t *testing.T) {
		b := open(t, resolver)
		bad := msg("evt_b7", "NOT-A-TOKEN", `{}`)
		if err := b.Publish(ctx, bad); events.ClassOf(err) != events.ClassPermanent {
			t.Fatalf("bad subject token: %v", err)
		}
		bad = msg("evt_b8", tokenA, `{}`)
		bad.EventType = "not.an.event"
		if err := b.Publish(ctx, bad); events.ClassOf(err) != events.ClassPermanent {
			t.Fatalf("unknown event type: %v", err)
		}
	})
}

// memBroker adapts InProcessBroker to Transport.
type memBroker struct{ *InProcessBroker }

func (b memBroker) Subscribe(consumer string, proc events.Processor) error {
	b.InProcessBroker.Subscribe(consumer, proc)
	return nil
}

func (b memBroker) Deliver(ctx context.Context) (int, error) { return b.Pump(ctx) }

// OpenMemBroker is the OpenTransport of the in-process broker.
func OpenMemBroker(t testing.TB, resolver events.SubjectResolver) Transport {
	t.Helper()
	b, err := NewInProcessBroker(resolver)
	if err != nil {
		t.Fatal(err)
	}
	return memBroker{b}
}

// jsBroker adapts the JetStream publisher and consumer to Transport.
type jsBroker struct {
	t        testing.TB
	nc       *nats.Conn
	cfg      jetstream.Config
	resolver events.SubjectResolver
	*jetstream.Publisher
	consumers []*jetstream.Consumer
}

func (b *jsBroker) Subscribe(consumer string, proc events.Processor) error {
	cc := jetstream.ConsumerConfig{Consumer: consumer, Durable: Prefix + "_" + consumer + "_" + RandomHex(b.t, 3), AckWait: 5 * time.Second,
		MaxAckPending: 10, FetchBatch: 5, FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond}
	c, err := jetstream.NewConsumer(b.nc, b.cfg, cc, b.resolver, proc, nil)
	if err != nil {
		return err
	}
	if err := c.Provision(context.Background()); err != nil {
		return err
	}
	b.consumers = append(b.consumers, c)
	return nil
}

func (b *jsBroker) Deliver(ctx context.Context) (int, error) {
	total := 0
	for _, c := range b.consumers {
		n, err := c.RunOnce(ctx)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// OpenJetStream is the OpenTransport of the JetStream adapter: a per-test stream
// on the shared NATS server. It skips without NATS, and always in the unit
// layer.
func OpenJetStream(t testing.TB, resolver events.SubjectResolver) Transport {
	t.Helper()
	nb := NewBroker(t)
	cfg := nb.StreamConfig(t, time.Minute)
	nc := nb.Connect(t, nb.Addr())
	pub, err := jetstream.NewPublisher(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.EnsureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &jsBroker{t: t, nc: nc, cfg: cfg, resolver: resolver, Publisher: pub}
}
