package events_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// TestLateBufferedPublicationIsHarmless: publisher A loses its broker
// connection and its publication is buffered by the client for the reconnect;
// A's publish times out (cancelling the context removes only the response
// waiter, not the buffered message) and the claim is released. Publisher B then
// publishes the event, the consumer completes it, and retention prunes the
// outbox row and the deduplication evidence; the broker's own duplicate window
// is over too. When A reconnects the client flushes the old message to the
// stream. The consumer must refuse that copy (quarantined for an operator, no
// inbox entry, no effect), so every effect stays single. Real PostgreSQL and
// JetStream; the outage is a test-local proxy. PX-05, AC-08.
func TestLateBufferedPublicationIsHarmless(t *testing.T) {
	const n = 2
	b := newBus(t, busOptions{duplicateWindow: 300 * time.Millisecond, bindings: []postgres.Binding{ledger("audit")}})
	ctx := context.Background()
	ids := []string{"evt_late0", "evt_late1"}
	for i, id := range ids {
		b.append(workItemEvent(id, []string{"wi_late0", "wi_late1"}[i], 1))
	}

	// Publisher A talks to the broker through a proxy it can lose.
	proxy := eventstest.NewProxy(t, b.broker.Addr())
	ncA := b.conn(proxy.Addr())
	brokerA, err := jetstream.NewPublisher(ncA, b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = brokerA.EnsureStream(ectx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	proxy.Stop()
	eventstest.Eventually(t, 5*time.Second, "A to notice the outage", func() bool { return !ncA.IsConnected() })

	pubA := b.publisher(brokerA, publisherOptions{timeout: 300 * time.Millisecond})
	res, err := pubA.RunOnce(ctx)
	if err != nil || res.Published != 0 || res.Failed != n {
		t.Fatalf("A while disconnected: %+v %v", res, err)
	}
	if got := b.count(`SELECT count(*) FROM outbox WHERE state = 'published'`); got != 0 {
		t.Fatalf("%d events published while A was disconnected", got)
	}

	// B publishes; the consumer completes every event.
	pubB := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	b.drainPublisher(pubB, 10*time.Second)
	c := b.consumer(b.broker.Addr(), nil, "audit")
	pump(t, c, 15*time.Second, "the first copies", func() bool { return c.Counters().Acked >= n })
	for _, id := range ids {
		if got := b.effects("audit", id); got != 1 {
			t.Fatalf("%s: %d effects before the late copy", id, got)
		}
	}

	// Retention forgets the publication and the deduplication evidence.
	b.clock.Advance(50 * time.Hour)
	pr, err := b.store.Prune(ctx, tenantA, b.clock.Now())
	if err != nil || pr.Inbox != n || pr.Outbox != n {
		t.Fatalf("prune: %+v %v", pr, err)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox`) + b.count(`SELECT count(*) FROM outbox`); got != 0 {
		t.Fatalf("evidence left after prune: %d", got)
	}
	time.Sleep(600 * time.Millisecond) // the broker's duplicate window is over

	// A reconnects: the client flushes its buffered publications.
	proxy.Start()
	eventstest.Eventually(t, 15*time.Second, "the buffered copies to reach the stream", func() bool {
		_, info := streamInfo(t, b)
		return info.State.Msgs >= 2*n
	})
	pump(t, c, 15*time.Second, "the late copies", func() bool { return c.Counters().Acked >= 2*n })

	for _, id := range ids {
		if got := b.effects("audit", id); got != 1 {
			t.Fatalf("%s: %d effects after the late copy, want 1", id, got)
		}
	}
	if got := b.count(`SELECT count(*) FROM event_inbox`); got != 0 {
		t.Fatalf("the late copies were admitted as new: %d inbox entries", got)
	}
	// They are kept for an operator, visibly, with the reason.
	entries, err := b.store.ListQuarantine(ctx, tenantA, "audit", 10)
	if err != nil || len(entries) != n {
		t.Fatalf("quarantine entries: %d %v", len(entries), err)
	}
	for _, e := range entries {
		if e.Reason != postgres.ReasonBeyondHorizon {
			t.Fatalf("reason = %s", e.Reason)
		}
	}
	if k := c.Counters(); k.Retried != 0 || k.Errors != 0 {
		t.Fatalf("late copies must be settled, not retried: %+v", k)
	}
}

// pausingProcessor models a consumer that fetched a delivery and stalls before
// its transaction: the delivery is handed to the store only when released.
type pausingProcessor struct {
	inner   events.Processor
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (p *pausingProcessor) Process(ctx context.Context, d events.Delivery) (events.Outcome, error) {
	paused := false
	p.once.Do(func() { paused = true })
	if paused {
		close(p.reached)
		<-p.release
	}
	return p.inner.Process(ctx, d)
}

// TestPausedDeliveryCannotRerunPrunedEffect: consumer one fetched a delivery
// and stalls before its inbox transaction. Another consumer instance completes
// the event, retention prunes the inbox entry, the outbox row and the broker
// horizon are long gone. When the first resumes, the missing entry must not be
// taken as "never processed": the effect stays single and the delivery is
// quarantined, not retried forever. Real PostgreSQL and JetStream. PX-05, AC-08.
func TestPausedDeliveryCannotRerunPrunedEffect(t *testing.T) {
	b := newBus(t, busOptions{bindings: []postgres.Binding{ledger("audit")}})
	ctx := context.Background()
	b.append(workItemEvent("evt_paused", "wi_paused", 1))
	b.drainPublisher(b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{}), 10*time.Second)

	pause := &pausingProcessor{inner: b.store, reached: make(chan struct{}), release: make(chan struct{})}
	first := b.consumer(b.broker.Addr(), pause, "audit")
	done := make(chan struct{})
	go func() {
		defer close(done)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, _ = first.RunOnce(cctx)
	}()
	<-pause.reached

	second := b.consumer(b.broker.Addr(), nil, "audit")
	pump(t, second, 15*time.Second, "the other consumer", func() bool { return second.Counters().Acked >= 1 })
	if got := b.effects("audit", "evt_paused"); got != 1 {
		t.Fatalf("effects after the other consumer: %d", got)
	}
	b.clock.Advance(50 * time.Hour)
	pr, err := b.store.Prune(ctx, tenantA, b.clock.Now())
	if err != nil || pr.Inbox != 1 || pr.Outbox != 1 {
		t.Fatalf("prune: %+v %v", pr, err)
	}

	close(pause.release)
	<-done
	if got := b.effects("audit", "evt_paused"); got != 1 {
		t.Fatalf("the resumed delivery repeated the effect: %d", got)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox`); got != 0 {
		t.Fatalf("the resumed delivery recreated the inbox entry: %d", got)
	}
	if got := b.count(`SELECT count(*) FROM event_quarantine WHERE reason_code = 'beyond_horizon'`); got != 1 {
		t.Fatalf("beyond_horizon quarantine entries: %d", got)
	}
	if k := first.Counters(); k.Acked != 1 || k.Retried != 0 {
		t.Fatalf("the resumed delivery must be settled once: %+v", k)
	}
}
