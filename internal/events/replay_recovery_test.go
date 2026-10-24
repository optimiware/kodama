package events_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// TestReplayRecoveryGuard: replay beyond retained
// inbox evidence, or after a simulated restore, needs an explicit safe
// classification and reconciliation; an unknown external outcome is never
// repeated automatically. Real PostgreSQL and NATS. PX-05, PX-A03, AC-08.
func TestReplayRecoveryGuard(t *testing.T) {
	local := func(name string, idempotent bool) postgres.Binding {
		return postgres.Binding{
			Spec: events.ConsumerSpec{Name: name, Mode: events.ModeHistorical, Effect: events.EffectLocal, IdempotentBeyondHorizon: idempotent},
			Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
				// An idempotent effect: repeating it changes nothing.
				_, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id)
					SELECT $1, $2, $3 WHERE NOT EXISTS (SELECT 1 FROM t005_effects WHERE consumer = $2 AND event_id = $3)`, string(env.TenantID), name, env.ID)
				return err
			},
		}
	}
	notifier := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "notifier", Mode: events.ModeHistorical, Effect: events.EffectExternalIntent},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			// A durable notification intent; the external call is another worker's job.
			_, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id) VALUES ($1, 'notifier', $2)`, string(env.TenantID), env.ID)
			return err
		},
	}
	mystery := postgres.Binding{
		Spec:   events.ConsumerSpec{Name: "mystery", Mode: events.ModeHistorical, Effect: events.EffectUnknown},
		Handle: func(context.Context, pgx.Tx, eventsv1.Envelope) error { return nil },
	}
	b := newBus(t, busOptions{
		bindings: []postgres.Binding{local("ledger", false), local("idem", true), notifier, mystery},
		mutate: func(o *postgres.Options) {
			o.Retention.Observation = 100 * time.Hour
			o.Retention.Inbox = 48 * time.Hour
		},
	})
	ctx := context.Background()
	scope := events.Scope{TenantID: tenantA, ProjectID: projectA}

	// Deliver three events through the real path to the ledger consumer; the
	// other consumers handle the same payloads directly.
	for i, id := range []string{"evt_g1", "evt_g2", "evt_g3"} {
		b.append(workItemEvent(id, "wi_g", int64(i+1)))
	}
	pub := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	b.drainPublisher(pub, 10*time.Second)
	consumer := b.consumer(b.broker.Addr(), nil, "ledger")
	pump(t, consumer, 15*time.Second, "ledger deliveries", func() bool { return consumer.Counters().Acked >= 3 })
	obs := readAll(t, b, scope)
	for _, o := range obs {
		for _, name := range []string{"idem", "notifier"} {
			if out, err := b.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: name, Payload: o.Payload}); err != nil || out.Kind != events.OutcomeCompleted {
				t.Fatalf("%s: %+v %v", name, out, err)
			}
		}
	}
	req := func(consumer string, mutate func(*events.ReplayRequest)) events.ReplayRequest {
		r := events.ReplayRequest{Scope: scope, Consumer: consumer, FromSeq: 1, ToSeq: 3, Reason: "recover after restore", Operator: events.Operator{ID: "ops_1"}}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	// Inside the horizon the inbox still proves what ran: nothing would repeat.
	if c, err := b.store.DryRun(ctx, req("ledger", nil)); err != nil || c.AlreadyCompleted != 3 || c.BeyondHorizon != 0 {
		t.Fatalf("within the horizon: %+v %v", c, err)
	}

	// Time passes beyond the inbox retention: deduplication evidence is pruned,
	// while observation history is still retained.
	b.clock.Advance(60 * time.Hour)
	pruned, err := b.store.Prune(ctx, tenantA, b.clock.Now())
	if err != nil || pruned.Inbox < 9 || pruned.Observations != 0 {
		t.Fatalf("prune: %+v %v", pruned, err)
	}
	if got := b.count(`SELECT count(*) FROM event_observations`); got != 3 {
		t.Fatalf("observation history was pruned with the inbox: %d", got)
	}

	cBeyond, err := b.store.DryRun(ctx, req("ledger", nil))
	if !errors.Is(err, events.ErrReplayBeyondHorizon) || cBeyond.BeyondHorizon != 3 {
		t.Fatalf("replay beyond retained inbox evidence: %+v %v", cBeyond, err)
	}
	if _, err := b.store.RequestReplay(ctx, req("ledger", func(r *events.ReplayRequest) { r.AllowBeyondHorizon = true })); !errors.Is(err, events.ErrReplayBeyondHorizon) {
		t.Fatalf("a non-idempotent consumer must not be replayed beyond the horizon even when acknowledged: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, req("idem", nil)); !errors.Is(err, events.ErrReplayBeyondHorizon) {
		t.Fatalf("beyond the horizon needs the explicit acknowledgment: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, req("notifier", func(r *events.ReplayRequest) { r.AllowBeyondHorizon = true })); !errors.Is(err, events.ErrReplayBeyondHorizon) {
		t.Fatalf("external intents must never be repeated beyond the horizon: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, req("mystery", func(r *events.ReplayRequest) { r.AllowBeyondHorizon = true })); !errors.Is(err, events.ErrReplayUnsafe) {
		t.Fatalf("unclassified consumer: %v", err)
	}
	for _, id := range []string{"evt_g1", "evt_g2", "evt_g3"} {
		if got := b.effects("notifier", id); got != 1 {
			t.Fatalf("the unknown external outcome of %s was repeated: %d intents", id, got)
		}
	}

	// A consumer whose owner declared the effect idempotent can be replayed
	// beyond the horizon, deliberately and audited. After a restore it is first
	// fenced; replay stays refused until its owner confirms reconciliation.
	if err := b.store.FenceConsumer(ctx, tenantA, "idem", events.Operator{ID: "ops_1"}, "database restored from backup"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.FenceConsumer(ctx, tenantA, "idem", events.Operator{ID: "mallory"}, "fence"); !errors.Is(err, events.ErrUnauthorized) {
		t.Fatalf("unauthorized fence: %v", err)
	}
	safe := func(r *events.ReplayRequest) { r.AllowBeyondHorizon = true }
	if _, err := b.store.RequestReplay(ctx, req("idem", safe)); !errors.Is(err, events.ErrReplayFenced) {
		t.Fatalf("replay while fenced: %v", err)
	}
	if err := b.store.ClearFence(ctx, tenantA, "idem", events.Operator{ID: "mallory"}, "trust me"); !errors.Is(err, events.ErrUnauthorized) {
		t.Fatalf("unauthorized fence clearing: %v", err)
	}
	if err := b.store.ClearFence(ctx, tenantA, "idem", events.Operator{ID: "ops_1"}, "external effects reconciled, old controllers fenced"); err != nil {
		t.Fatal(err)
	}
	rec, err := b.store.RequestReplay(ctx, req("idem", safe))
	if err != nil {
		t.Fatalf("explicit safe replay: %v", err)
	}
	// A fence set after the request stops the run before anything is processed.
	if err := b.store.FenceConsumer(ctx, tenantA, "idem", events.Operator{ID: "ops_1"}, "second restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrReplayFenced) {
		t.Fatalf("run while fenced: %v", err)
	}
	if got, _ := b.store.GetReplay(ctx, tenantA, rec.ID); got.State != "requested" || got.NextSeq != 1 || got.Processed != 0 {
		t.Fatalf("a fenced replay made progress: %+v", got)
	}
	if err := b.store.ClearFence(ctx, tenantA, "idem", events.Operator{ID: "ops_1"}, "reconciled again"); err != nil {
		t.Fatal(err)
	}
	if got, err := b.store.RunReplay(ctx, tenantA, rec.ID); err != nil || got.State != "completed" || got.Processed != 3 {
		t.Fatalf("replay after clearing: %+v %v", got, err)
	}
	for _, id := range []string{"evt_g1", "evt_g2", "evt_g3"} {
		if got := b.effects("idem", id); got != 1 {
			t.Fatalf("idempotent replay changed the effect count of %s: %d", id, got)
		}
	}
	for action, want := range map[string]int{"event.consumer_fenced": 2, "event.consumer_fence_cleared": 2, "event.replay_requested": 1, "event.replay_completed": 1} {
		if got := b.auditRows(action); len(got) != want {
			t.Fatalf("audit %s = %d row(s), want %d", action, len(got), want)
		}
	}

	// Past observation retention the range is gone and cannot be replayed.
	b.clock.Advance(100 * time.Hour)
	if _, err := b.store.Prune(ctx, tenantA, b.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.RequestReplay(ctx, req("idem", safe)); !errors.Is(err, events.ErrReplayRangeExpired) {
		t.Fatalf("replay of pruned history: %v", err)
	}
}

func readAll(t *testing.T, b *bus, scope events.Scope) []postgres.Observation {
	t.Helper()
	var out []postgres.Observation
	err := storageWithTenant(b, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, _, err = b.store.ReadObservations(ctx, tx, scope, 0, 100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
