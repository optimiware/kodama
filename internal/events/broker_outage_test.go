package events_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// produce is one business transaction: a state row plus the event.
func (b *bus) produce(eventID string, version int64) error {
	return storage.WithTenant(context.Background(), b.env.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t005_state (tenant_id, id) VALUES ($1, $2)`, tenantA, eventID); err != nil {
			return err
		}
		_, err := b.store.Append(ctx, tx, workItemEvent(eventID, "wi_"+eventID, version))
		return err
	})
}

// Test_broker_outage: with the broker unreachable, committed state and outbox
// rows survive with their retry history; the configured backlog limit rejects
// further admission atomically (with the business change); periodic repair keeps
// running; and delivery drains after recovery. The outage is injected with a
// test-local proxy, never by stopping the shared service. Real PostgreSQL and
// NATS. PX-05, PX-A03, AC-08, AC-A06.
func Test_broker_outage(t *testing.T) {
	const limit = 6
	b := newBus(t, busOptions{
		bindings: []postgres.Binding{ledger("audit")},
		mutate: func(o *postgres.Options) {
			o.Backlog = events.BacklogLimits{MaxEvents: limit, MaxBytes: 1 << 20}
		},
	})
	b.ownerExec(`CREATE TABLE t005_state (tenant_id kodama_id NOT NULL, id text NOT NULL, PRIMARY KEY (tenant_id, id))`)
	b.tenantTable("t005_state")

	proxy := eventstest.NewProxy(t, b.broker.Addr())
	pub := b.publisher(b.jsPublisher(proxy.Addr()), publisherOptions{timeout: 300 * time.Millisecond})

	proxy.Stop() // the broker becomes unreachable for this process
	for i := 1; i <= limit; i++ {
		if err := b.produce("evt_out_"+string(rune('a'+i-1)), int64(i)); err != nil {
			t.Fatalf("business commit %d must not depend on the broker: %v", i, err)
		}
	}
	// The next admission is rejected atomically: neither the event nor the
	// business change commits.
	err := b.produce("evt_out_overflow", 99)
	if !errors.Is(err, events.ErrBacklogFull) {
		t.Fatalf("admission beyond the limit: %v", err)
	}
	if got := b.count(`SELECT count(*) FROM t005_state`); got != limit {
		t.Fatalf("state rows = %d, want %d (the rejected change must roll back)", got, limit)
	}
	if got := b.count(`SELECT count(*) FROM outbox WHERE event_id = 'evt_out_overflow'`) + b.count(`SELECT count(*) FROM event_observations WHERE event_id = 'evt_out_overflow'`); got != 0 {
		t.Fatalf("the rejected event left %d row(s)", got)
	}
	if got := b.count(`SELECT pending_count FROM event_backlog`); got != limit {
		t.Fatalf("backlog counter = %d, want %d", got, limit)
	}

	// Periodic repair keeps running during the outage: the publisher retries with
	// recorded history and the reconciler keeps scanning.
	var passes atomic.Int64
	reconciler, err := events.NewReconciler(b.store, 50*time.Millisecond, nil, events.Reconciliation{
		Name: "count-passes", Run: func(context.Context, domain.TenantID) error { passes.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg = make(chan struct{}, 2)
	go func() { _ = pub.Run(ctx); wg <- struct{}{} }()
	go func() { _ = reconciler.Run(ctx); wg <- struct{}{} }()
	eventstest.Eventually(t, 20*time.Second, "retry history during the outage", func() bool {
		return b.count(`SELECT count(*) FROM outbox_attempts WHERE outcome = 'transient'`) >= limit
	})
	eventstest.Eventually(t, 10*time.Second, "reconciliation passes during the outage", func() bool { return passes.Load() >= 3 })

	stats, err := b.store.PublicationStats(context.Background(), tenantA, b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending+stats.Claimed != limit || stats.BacklogBytes <= 0 || stats.OldestUnpublishedAge <= 0 || stats.Attempts < limit ||
		stats.LastClass != events.ClassTransient || stats.LastError == "" || stats.NextRetryAt.IsZero() && stats.Claimed == 0 {
		t.Fatalf("stats do not describe the outage: %+v", stats)
	}
	if c := pub.Counters(); c.Failures < limit || c.Published != 0 {
		t.Fatalf("publisher counters: %+v", c)
	}
	// Still nothing was lost.
	if got := b.count(`SELECT count(*) FROM outbox WHERE state <> 'published'`); got != limit {
		t.Fatalf("unpublished rows = %d", got)
	}

	// Recovery: delivery drains and admission resumes.
	proxy.Start()
	eventstest.Eventually(t, 30*time.Second, "the outbox to drain after recovery", func() bool {
		return b.count(`SELECT count(*) FROM outbox WHERE state = 'published'`) == limit
	})
	cancel()
	<-wg
	<-wg
	if got := b.count(`SELECT pending_count FROM event_backlog`); got != 0 {
		t.Fatalf("backlog counter after the drain = %d", got)
	}
	consumer := b.consumer(proxy.Addr(), nil, "audit")
	pump(t, consumer, 20*time.Second, "all events to be consumed", func() bool {
		return b.count(`SELECT count(*) FROM event_inbox WHERE consumer = 'audit' AND state = 'completed'`) == limit
	})
	if got := b.count(`SELECT count(*) FROM t005_effects WHERE consumer = 'audit'`); got != limit {
		t.Fatalf("effects = %d, want %d", got, limit)
	}
	if err := b.produce("evt_out_after", 100); err != nil {
		t.Fatalf("admission after recovery: %v", err)
	}
}
