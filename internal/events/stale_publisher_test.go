package events_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// pausedStore models a publisher process that is frozen (stopped, partitioned,
// starved) right after it claimed its batch: ClaimDue returns the claims only
// once the test lets the process continue.
type pausedStore struct {
	events.PublicationStore
	claimed chan struct{}
	resume  chan struct{}
}

func (p *pausedStore) ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int) ([]events.Claim, error) {
	claims, err := p.PublicationStore.ClaimDue(ctx, tenant, now, lease, limit)
	if len(claims) > 0 {
		close(p.claimed)
		<-p.resume
	}
	return claims, err
}

// TestStalePublisherCannotRepublishAfterRetention: a publisher claims a batch
// and stalls past its lease; another publisher reclaims and publishes the
// events, the consumer completes them, and retention forgets the publication
// and deduplication evidence. The stale publisher then resumes with claims that
// have long expired: it must not send them, so no copy appears after the
// evidence is gone and every effect stays single. Real PostgreSQL and NATS.
// PX-05, AC-08.
func TestStalePublisherCannotRepublishAfterRetention(t *testing.T) {
	const n = 3
	b := newBus(t, busOptions{duplicateWindow: 300 * time.Millisecond, bindings: []postgres.Binding{ledger("audit")}})
	ctx := context.Background()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("evt_stale%d", i)
		b.append(workItemEvent(ids[i], fmt.Sprintf("wi_stale%d", i), 1))
	}

	paused := &pausedStore{PublicationStore: b.store, claimed: make(chan struct{}), resume: make(chan struct{})}
	stale := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{store: paused})
	type outcome struct {
		res events.PublishResult
		err error
	}
	staleDone := make(chan outcome, 1)
	go func() {
		res, err := stale.RunOnce(ctx)
		staleDone <- outcome{res, err}
	}()
	<-paused.claimed
	if got := b.count(`SELECT count(*) FROM outbox WHERE state = 'claimed' AND claim_token = 1`); got != n {
		t.Fatalf("the stalled publisher holds %d claims, want %d", got, n)
	}

	// Time passes beyond the lease; a healthy publisher takes over and the
	// consumer completes every event.
	b.clock.Advance(5 * time.Minute)
	healthy := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	b.drainPublisher(healthy, 10*time.Second)
	c := b.consumer(b.broker.Addr(), nil, "audit")
	pump(t, c, 15*time.Second, "all copies", func() bool { return c.Counters().Acked >= n })
	for _, id := range ids {
		if got := b.effects("audit", id); got != 1 {
			t.Fatalf("%s: %d effects before the stale resume", id, got)
		}
	}

	// Retention runs: publication records and deduplication evidence are gone.
	b.clock.Advance(50 * time.Hour)
	res, err := b.store.Prune(ctx, tenantA, b.clock.Now())
	if err != nil || res.Inbox != n || res.Outbox != n {
		t.Fatalf("prune: %+v %v", res, err)
	}
	time.Sleep(600 * time.Millisecond) // the broker's duplicate window is over too

	close(paused.resume)
	got := <-staleDone
	if got.err != nil {
		t.Fatalf("stale pass: %v", got.err)
	}
	if got.res.Claimed != n || got.res.Published != 0 || got.res.Expired != n {
		t.Fatalf("the stale publisher must send nothing: %+v", got.res)
	}
	if _, info := streamInfo(t, b); info.State.Msgs != n {
		t.Fatalf("the broker holds %d messages, want %d: a stale claim was published", info.State.Msgs, n)
	}
	ctxPump, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, _ = c.RunOnce(ctxPump)
	cancel()
	for _, id := range ids {
		if got := b.effects("audit", id); got != 1 {
			t.Fatalf("%s: %d effects after the stale resume, want 1", id, got)
		}
	}
	if c := stale.Counters(); c.Attempts != 0 {
		t.Fatalf("the stale publisher attempted %d publication(s)", c.Attempts)
	}
}
