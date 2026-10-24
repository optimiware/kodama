package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// projection is a current-state consumer: it reloads the authoritative
// aggregate and applies a version guard, never trusting the event's version as
// the state to apply.
func projection() postgres.Binding {
	return postgres.Binding{
		Spec: events.ConsumerSpec{Name: "projection", Mode: events.ModeCurrentState, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			return reconcileProjection(ctx, tx, string(env.TenantID), env.Resource.ID)
		},
	}
}

func reconcileProjection(ctx context.Context, tx pgx.Tx, tenant, resource string) error {
	var authoritative int64
	if err := tx.QueryRow(ctx, `SELECT version FROM t005_aggregates WHERE tenant_id = $1 AND resource_id = $2`, tenant, resource).Scan(&authoritative); err != nil {
		return err
	}
	var applied int64
	err := tx.QueryRow(ctx, `SELECT applied FROM t005_projection WHERE tenant_id = $1 AND resource_id = $2 FOR UPDATE`, tenant, resource).Scan(&applied)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if !events.ModeCurrentState.Observes(applied, authoritative) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO t005_projection (tenant_id, resource_id, applied) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, resource_id) DO UPDATE SET applied = EXCLUDED.applied`, tenant, resource, authoritative); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO t005_projection_log (tenant_id, resource_id, applied) VALUES ($1, $2, $3)`, tenant, resource, authoritative)
	return err
}

// commit is a producer business transaction: it bumps the aggregate and appends
// the event atomically.
func (b *bus) commit(eventID, workItem string, version int64) eventsv1.Envelope {
	b.t.Helper()
	env := workItemEvent(eventID, workItem, version)
	err := storage.WithTenant(context.Background(), b.env.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t005_aggregates (tenant_id, resource_id, version) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id, resource_id) DO UPDATE SET version = GREATEST(t005_aggregates.version, EXCLUDED.version)`, tenantA, workItem, version); err != nil {
			return err
		}
		_, err := b.store.Append(ctx, tx, env)
		return err
	})
	if err != nil {
		b.t.Fatalf("commit %s: %v", eventID, err)
	}
	return env
}

// Test_reordered_events: newer-before-older and several unique events at one
// version are delivered out of order and with a lost wakeup; current-state
// processing never regresses, historical processing is complete, and periodic
// reconciliation converges without any broker message. Real PostgreSQL and
// NATS. PX-05, PX-A03.
func Test_reordered_events(t *testing.T) {
	b := newBus(t, busOptions{bindings: []postgres.Binding{projection(), ledger("history")}})
	b.ownerExec(`CREATE TABLE t005_aggregates (tenant_id kodama_id NOT NULL, resource_id text NOT NULL, version bigint NOT NULL, PRIMARY KEY (tenant_id, resource_id))`)
	b.ownerExec(`CREATE TABLE t005_projection (tenant_id kodama_id NOT NULL, resource_id text NOT NULL, applied bigint NOT NULL, PRIMARY KEY (tenant_id, resource_id))`)
	b.ownerExec(`CREATE TABLE t005_projection_log (seq bigint GENERATED ALWAYS AS IDENTITY, tenant_id kodama_id NOT NULL, resource_id text NOT NULL, applied bigint NOT NULL)`)
	for _, table := range []string{"t005_aggregates", "t005_projection", "t005_projection_log"} {
		b.tenantTable(table)
	}
	b.jsPublisher(b.broker.Addr()) // provisions the stream
	token, err := b.store.SubjectToken(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}

	e1 := b.commit("evt_r1", "wi_9", 1)
	e2 := b.commit("evt_r2", "wi_9", 2)
	e3 := b.commit("evt_r3", "wi_9", 3)
	e4 := b.commit("evt_r4", "wi_9", 3) // distinct events at the same aggregate version
	e5 := b.commit("evt_r5", "wi_9", 3)

	// Deliver the newest first, then older ones, then the same-version events.
	for _, e := range []eventsv1.Envelope{e3, e1, e2, e5, e4} {
		rawPublish(t, b, token, e.Type, payloadOf(t, e))
	}
	projectionConsumer := b.consumer(b.broker.Addr(), nil, "projection")
	historyConsumer := b.consumer(b.broker.Addr(), nil, "history")
	pump(t, projectionConsumer, 15*time.Second, "projection deliveries", func() bool { return projectionConsumer.Counters().Acked >= 5 })
	pump(t, historyConsumer, 15*time.Second, "history deliveries", func() bool { return historyConsumer.Counters().Acked >= 5 })

	if got := b.count(`SELECT applied FROM t005_projection WHERE resource_id = 'wi_9'`); got != 3 {
		t.Fatalf("projection applied version = %d, want 3", got)
	}
	// The applied version is monotonic: later (older) observations never regress it.
	last := int64(0)
	err = storage.WithTenant(context.Background(), b.env.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT applied FROM t005_projection_log ORDER BY seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return err
			}
			if v < last {
				t.Errorf("projection regressed from %d to %d", last, v)
			}
			last = v
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	// History is complete: every unique event, including older observations
	// and the three distinct events at version 3.
	for _, e := range []eventsv1.Envelope{e1, e2, e3, e4, e5} {
		if got := b.effects("history", e.ID); got != 1 {
			t.Errorf("history has %d record(s) of %s, want 1", got, e.ID)
		}
	}

	// A lost wakeup: committed state and outbox exist, the message never arrives.
	e6 := b.commit("evt_r6", "wi_9", 4)
	if got := b.effects("history", e6.ID); got != 0 {
		t.Fatalf("history already has the undelivered event")
	}
	reconciler, err := events.NewReconciler(b.store, 50*time.Millisecond, nil,
		events.Reconciliation{Name: "projection-scan", Run: func(ctx context.Context, tenant domain.TenantID) error {
			return storage.WithTenant(ctx, b.env.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT a.resource_id FROM t005_aggregates a LEFT JOIN t005_projection p
					ON p.tenant_id = a.tenant_id AND p.resource_id = a.resource_id WHERE COALESCE(p.applied, 0) < a.version`)
				if err != nil {
					return err
				}
				var stale []string
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					stale = append(stale, id)
				}
				rows.Close()
				for _, id := range stale {
					if err := reconcileProjection(ctx, tx, string(tenant), id); err != nil {
						return err
					}
				}
				return rows.Err()
			})
		}},
		b.store.RepairReconciliation("history", 0, 20),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := b.count(`SELECT applied FROM t005_projection WHERE resource_id = 'wi_9'`); got != 4 {
		t.Fatalf("reconciliation did not converge the projection: %d", got)
	}
	if got := b.effects("history", e6.ID); got != 1 {
		t.Fatalf("repair did not backfill the undelivered event: %d", got)
	}
	// Repeating the reconciliation is idempotent.
	if err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.effects("history", e6.ID); got != 1 {
		t.Fatalf("repeat repair duplicated the effect: %d", got)
	}

	// The periodic loop converges on its own, with and without wakeups, and
	// coalesces bursts of wakeups.
	b.commit("evt_r7", "wi_9", 5)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	for i := 0; i < 100; i++ {
		reconciler.Wake()
	}
	eventstest.Eventually(t, 10*time.Second, "periodic reconciliation to converge", func() bool {
		return b.count(`SELECT applied FROM t005_projection WHERE resource_id = 'wi_9'`) == 5 && b.effects("history", "evt_r7") == 1
	})
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run returned %v", err)
	}
}
