package events_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// crashStore simulates a publisher process killed after the broker acknowledged
// but before the publication was marked: Complete never reaches PostgreSQL.
type crashStore struct {
	events.PublicationStore
	completions int
}

func (c *crashStore) Complete(context.Context, events.Claim, time.Time) error {
	c.completions++
	return errors.New("process killed before marking the publication complete")
}

func streamInfo(t *testing.T, b *bus) (natsjs.Stream, *natsjs.StreamInfo) {
	t.Helper()
	js, err := natsjs.New(b.conn(b.broker.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, b.cfg.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, info
}

// Test_publish_ack_crash: a crash after JetStream acknowledged but before the
// publication was marked leaves a claim that expires; the restarted publisher
// re-publishes the same event ID, and even when the broker no longer suppresses
// the duplicate (window elapsed) the consumer commits its effect exactly once.
// Real PostgreSQL and NATS. PX-05, AC-08.
func Test_publish_ack_crash(t *testing.T) {
	b := newBus(t, busOptions{duplicateWindow: 300 * time.Millisecond, bindings: []postgres.Binding{ledger("audit")}})
	const id = "evt_ack_crash"
	b.append(workItemEvent(id, "wi_1", 1))

	// Process A: publishes, receives the durable acknowledgment, "crashes".
	crash := &crashStore{PublicationStore: b.store}
	a := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{store: crash})
	res, err := a.RunOnce(context.Background())
	if err == nil || res.Published != 0 || crash.completions != 1 {
		t.Fatalf("crash pass: res=%+v err=%v completions=%d", res, err, crash.completions)
	}
	if got := b.count(`SELECT count(*) FROM outbox WHERE event_id = $1 AND state = 'claimed' AND claim_token = 1`, id); got != 1 {
		t.Fatalf("the unmarked publication must stay claimed under token 1, got %d", got)
	}
	if _, info := streamInfo(t, b); info.State.Msgs != 1 {
		t.Fatalf("broker holds %d messages, want 1", info.State.Msgs)
	}

	// While the lease is live, another publisher must not touch it.
	other := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	if r, _ := other.RunOnce(context.Background()); r.Claimed != 0 {
		t.Fatalf("a live claim was handed out again: %+v", r)
	}

	// Restart after the lease expired and after the broker's duplicate window:
	// the broker no longer deduplicates, so the inbox is the correctness boundary.
	time.Sleep(600 * time.Millisecond)
	restarted := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{clock: func() time.Time { return b.clock.Now().Add(2 * time.Minute) }})
	res, err = restarted.RunOnce(context.Background())
	if err != nil || res.Published != 1 {
		t.Fatalf("restarted pass: res=%+v err=%v", res, err)
	}
	if got := b.count(`SELECT count(*) FROM outbox WHERE event_id = $1 AND state = 'published' AND claim_token = 2 AND attempts = 2`, id); got != 1 {
		t.Fatalf("expected the event published under claim token 2 after 2 attempts")
	}
	// The unknown first attempt is kept in the durable history.
	if got := b.count(`SELECT count(*) FROM outbox_attempts WHERE event_id = $1 AND ((attempt = 1 AND outcome = 'transient') OR (attempt = 2 AND outcome = 'published'))`, id); got != 2 {
		t.Fatalf("attempt history incomplete: %d rows", got)
	}
	stream, info := streamInfo(t, b)
	if info.State.Msgs != 2 {
		t.Fatalf("the broker stored %d messages; this test needs the duplicate to get through suppression", info.State.Msgs)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		raw, err := stream.GetMsg(context.Background(), seq)
		if err != nil {
			t.Fatal(err)
		}
		if got := raw.Header.Get(nats.MsgIdHdr); got != id {
			t.Fatalf("message %d carries ID %q, want the stable event ID %q", seq, got, id)
		}
	}

	// The consumer receives both copies and commits the effect once.
	c := b.consumer(b.broker.Addr(), nil, "audit")
	pump(t, c, 15*time.Second, "both copies to be acknowledged", func() bool { return c.Counters().Acked >= 2 })
	if got := b.effects("audit", id); got != 1 {
		t.Fatalf("effect committed %d times, want exactly once", got)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox WHERE event_id = $1 AND state = 'completed'`, id); got != 1 {
		t.Fatalf("inbox results = %d, want one", got)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE event_id = $1 AND outcome = 'completed'`, id); got != 1 {
		t.Fatalf("completion recorded %d times", got)
	}
}
