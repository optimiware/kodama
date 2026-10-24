package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// recordCancel is the fixture execution adapter: an idempotent, durable
// cancellation request keyed by work item. No Kubernetes, Git or S3.
func recordCancel(ctx context.Context, tx pgx.Tx, req work.CancelRequest) error {
	_, err := tx.Exec(ctx, `INSERT INTO t011_cancel_requests (tenant_id, project_id, work_item_id, version) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, work_item_id) DO UPDATE SET requests = t011_cancel_requests.requests + 1`,
		string(req.Scope.TenantID), string(req.Scope.ProjectID), string(req.WorkItemID), int64(req.Version))
	return err
}

type fixtureRequester struct{ e *env }

func (f fixtureRequester) RequestCancellation(ctx context.Context, req work.CancelRequest) error {
	return storage.WithTenant(ctx, f.e.rt, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return recordCancel(ctx, tx, req)
	})
}

func (e *env) cancelRequests(it work.Item) (rows, requests int) {
	e.t.Helper()
	rows = e.count(string(it.Scope.TenantID), `SELECT count(*) FROM t011_cancel_requests WHERE work_item_id = $1`, string(it.ID))
	requests = e.count(string(it.Scope.TenantID), `SELECT coalesce(max(requests), 0) FROM t011_cancel_requests WHERE work_item_id = $1`, string(it.ID))
	return
}

// Test_cancel_no_new_work: cancellation commits and survives a broker outage,
// serializes against dispatch admission, rejects later claims and yields a
// durable cancellation intent recoverable by the periodic scan and by event
// delivery; duplicate delivery is harmless. Real PostgreSQL and NATS (the
// outage is a test-local proxy), fixture execution adapter. WM-08, WM-09, WM-A06.
func Test_cancel_no_new_work(t *testing.T) {
	binding, err := workpg.CancellationBinding(workpg.CancellationConsumerName, recordCancel)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, envOptions{bindings: []eventspg.Binding{binding}})
	ctx := context.Background()
	broker := eventstest.NewBroker(t)
	cfg := broker.StreamConfig(t, time.Minute)
	proxy := eventstest.NewProxy(t, broker.Addr())

	pubConn := broker.Connect(t, proxy.Addr())
	jsPub, err := jetstream.NewPublisher(pubConn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := jsPub.EnsureStream(ectx); err != nil {
		t.Fatal(err)
	}
	pub, err := events.NewPublisher(e.events, e.events, jsPub, events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: 500 * time.Millisecond, BatchSize: 50, Interval: 50 * time.Millisecond,
		Retry: events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Drain whatever creation published before the outage starts.
	item := e.activate(scopeA, "runs when cancelled")
	for i := 0; i < 20 && e.count(tenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`) > 0; i++ {
		_, _ = pub.RunOnce(ctx)
		time.Sleep(20 * time.Millisecond)
	}

	t.Run("cancellation_commits_during_a_broker_outage", func(t *testing.T) {
		proxy.Stop()
		cancelled, err := e.move(item, engineer, work.StateCancelled, func(r *work.TransitionRequest) { r.Reason = "no longer needed" })
		if err != nil {
			t.Fatalf("cancellation must not depend on the broker: %v", err)
		}
		if cancelled.State != work.StateCancelled {
			t.Fatalf("state = %s", cancelled.State)
		}
		// The event is durably pending; publication fails while the broker is out.
		for i := 0; i < 3; i++ {
			_, _ = pub.RunOnce(ctx)
		}
		if n := e.count(tenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`); n != 1 {
			t.Fatalf("unpublished events during the outage = %d, want the cancellation", n)
		}
		item = cancelled
	})

	t.Run("new_dispatch_is_refused_immediately", func(t *testing.T) {
		err := storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := workpg.LockActionable(ctx, tx, scopeA, item.ID)
			return err
		})
		if !is(err, work.ErrCancelled) {
			t.Fatalf("claim after cancellation: %v", err)
		}
		if _, err := e.move(item, engineer, work.StateActive); !is(err, work.ErrInvalidTransition) {
			t.Fatalf("a cancelled item re-entering active: %v", err)
		}
	})

	t.Run("the_scan_recovers_the_intent_without_the_broker", func(t *testing.T) {
		n, err := work.ReconcileCancellations(ctx, e.store, fixtureRequester{e}, tenantA, 1)
		if err != nil || n != 1 {
			t.Fatalf("reconcile: %d %v", n, err)
		}
		if rows, reqs := e.cancelRequests(item); rows != 1 || reqs != 1 {
			t.Fatalf("requests rows %d count %d", rows, reqs)
		}
		// Idempotent: another pass repeats the request without a second record.
		if _, err := work.ReconcileCancellations(ctx, e.store, fixtureRequester{e}, tenantA, 5); err != nil {
			t.Fatal(err)
		}
		if rows, _ := e.cancelRequests(item); rows != 1 {
			t.Fatalf("duplicate request created %d records", rows)
		}
		// Other tenants' cancelled items are never scanned.
		other := e.mustMove(e.create(scopeB, work.KindTask, "b"), engineer, work.StateCancelled)
		if _, err := work.ReconcileCancellations(ctx, e.store, fixtureRequester{e}, tenantA, 5); err != nil {
			t.Fatal(err)
		}
		if rows, _ := e.cancelRequests(other); rows != 0 {
			t.Fatalf("tenant A's scan touched tenant B's item")
		}
	})

	t.Run("event_delivery_records_the_intent_and_duplicates_are_harmless", func(t *testing.T) {
		// A second item whose only path to execution is the event: no scan runs.
		viaEvent := e.mustMove(e.activate(scopeA, "cancelled via event"), engineer, work.StateCancelled)
		proxy.Start()
		for i := 0; i < 100 && e.count(tenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`) > 0; i++ {
			_, _ = pub.RunOnce(ctx)
			time.Sleep(30 * time.Millisecond)
		}
		if n := e.count(tenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`); n != 0 {
			t.Fatalf("outbox not drained after recovery: %d", n)
		}
		cons, err := jetstream.NewConsumer(broker.Connect(t, broker.Addr()), cfg, jetstream.ConsumerConfig{
			Consumer: workpg.CancellationConsumerName, Durable: "t011_cancel_" + eventstest.RandomHex(t, 3),
			AckWait: 5 * time.Second, MaxAckPending: 50, FetchBatch: 20, FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond,
		}, e.events, e.events, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cons.Provision(ectx); err != nil {
			t.Fatal(err)
		}
		eventstest.Eventually(t, 30*time.Second, "the cancellation event to be consumed", func() bool {
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, _ = cons.RunOnce(rctx)
			rows, _ := e.cancelRequests(viaEvent)
			return rows == 1
		})
		rows, reqs := e.cancelRequests(viaEvent)
		if rows != 1 || reqs != 1 {
			t.Fatalf("event-driven request: rows %d count %d", rows, reqs)
		}
		// Only cancellations produce requests: the activation events created none.
		if n := e.count(tenantA, `SELECT count(*) FROM t011_cancel_requests`); n != 2 {
			t.Fatalf("cancel requests = %d, want 2 (one per cancelled item)", n)
		}

		// Duplicate delivery of the same event: the inbox absorbs it.
		var payload []byte
		if err := storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT payload FROM event_observations WHERE tenant_id = $1 AND resource_id = $2 AND event_type = 'work_item.transitioned'
				ORDER BY resource_version DESC LIMIT 1`, tenantA, string(viaEvent.ID)).Scan(&payload)
		}); err != nil {
			t.Fatal(err)
		}
		out, err := e.events.Process(ctx, events.Delivery{Tenant: domain.TenantID(tenantA), Consumer: workpg.CancellationConsumerName, Payload: payload})
		if err != nil || out.Kind != events.OutcomeDuplicate {
			t.Fatalf("duplicate delivery: %+v %v", out, err)
		}
		if rows, reqs := e.cancelRequests(viaEvent); rows != 1 || reqs != 1 {
			t.Fatalf("duplicate delivery changed the request: rows %d count %d", rows, reqs)
		}
	})

	t.Run("cancellation_serializes_against_dispatch_admission", func(t *testing.T) {
		running := e.activate(scopeA, "being dispatched")
		h := e.hold(tenantA, lockActionable(scopeA, running.ID))
		cancelled := e.moveAsync(running, engineer, work.StateCancelled)
		e.waitBlocked(1)
		select {
		case r := <-cancelled:
			t.Fatalf("cancellation completed while a dispatch held the admission lock: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		if err := h.finish(func(ctx context.Context, tx pgx.Tx) error {
			_, err := e.reserve(ctx, tx, scopeA, running.ID, "dispatch")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r := <-cancelled
		if r.err != nil || r.v.State != work.StateCancelled {
			t.Fatalf("cancellation: %+v", r)
		}
		// The dispatched work is visible to the cancellation scan (its reservation
		// committed before the cancellation did), and no further claim is admitted.
		if n, err := work.ReconcileCancellations(ctx, e.store, fixtureRequester{e}, tenantA, 10); err != nil || n < 1 {
			t.Fatalf("reconcile: %d %v", n, err)
		}
		if rows, _ := e.cancelRequests(r.v); rows != 1 {
			t.Fatalf("no request for the item cancelled after dispatch")
		}
	})
}
