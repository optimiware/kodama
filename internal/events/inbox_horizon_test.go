package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// TestInboxEvidenceOutlivesUnpublishedOutbox: an event consumed after the
// broker accepted it but before the publisher marked it published (here: the
// publisher died after the acknowledgment) must keep its deduplication
// evidence for as long as the outbox row can still be published, however long
// the outage lasts and whatever the inbox retention is. Late recovery starts a
// new broker lifetime, republishes the same ID and must not repeat the effect.
// Real PostgreSQL and NATS. PX-05, AC-08.
func TestInboxEvidenceOutlivesUnpublishedOutbox(t *testing.T) {
	b := newBus(t, busOptions{duplicateWindow: 300 * time.Millisecond, bindings: []postgres.Binding{ledger("audit")}})
	ctx := context.Background()
	const id = "evt_late_publish"
	b.append(workItemEvent(id, "wi_late", 1))

	// The broker accepts the event, the publisher dies before marking it.
	crash := &crashStore{PublicationStore: b.store}
	a := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{store: crash})
	if _, err := a.RunOnce(ctx); err == nil || crash.completions != 1 {
		t.Fatalf("crash pass: err=%v completions=%d", err, crash.completions)
	}

	// The consumer commits its effect from that first copy.
	c := b.consumer(b.broker.Addr(), nil, "audit")
	pump(t, c, 15*time.Second, "the first copy", func() bool { return c.Counters().Acked >= 1 })
	if got := b.effects("audit", id); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}

	// A long outage: far beyond inbox, observation and broker retention.
	b.clock.Advance(500 * time.Hour)
	res, err := b.store.Prune(ctx, tenantA, b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Inbox != 0 || res.Outbox != 0 {
		t.Fatalf("prune with an unpublished outbox row removed evidence or the row: %+v", res)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox WHERE event_id = $1 AND state = 'completed'`, id); got != 1 {
		t.Fatalf("deduplication evidence of an unpublished event was forgotten")
	}
	if got := b.count(`SELECT count(*) FROM event_observations WHERE event_id = $1`, id); got != 0 {
		t.Fatalf("this scenario expects the observation history to have expired independently, %d row(s) left", got)
	}

	// The publisher recovers (the claim lease expired; the broker's duplicate
	// window elapsed) and republishes the same ID.
	time.Sleep(600 * time.Millisecond)
	recovered := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	if r, err := recovered.RunOnce(ctx); err != nil || r.Published != 1 {
		t.Fatalf("recovered publisher: %+v %v", r, err)
	}
	if _, info := streamInfo(t, b); info.State.Msgs != 2 {
		t.Fatalf("the broker holds %d copies; this test needs the republication to get through", info.State.Msgs)
	}
	pump(t, c, 15*time.Second, "the republished copy", func() bool { return c.Counters().Acked >= 2 })
	if got := b.effects("audit", id); got != 1 {
		t.Fatalf("the republished event repeated the effect: %d rows", got)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE event_id = $1 AND outcome = 'completed'`, id); got != 1 {
		t.Fatalf("completions recorded = %d", got)
	}

	// Published now: the evidence is kept for the broker horizon from the
	// publication, then forgotten together with the publication record.
	res, err = b.store.Prune(ctx, tenantA, b.clock.Now().Add(time.Hour))
	if err != nil || res.Inbox != 0 {
		t.Fatalf("prune within the broker horizon of the republication: %+v %v", res, err)
	}
	res, err = b.store.Prune(ctx, tenantA, b.clock.Now().Add(49*time.Hour))
	if err != nil || res.Inbox != 1 || res.Outbox != 1 {
		t.Fatalf("prune after the broker horizon: %+v %v", res, err)
	}
	var _ events.PublicationStore = crash
}
