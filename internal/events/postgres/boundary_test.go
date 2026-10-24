package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// longObservation keeps observations (the origin record of an event whose
// outbox row is gone) longer than the inbox evidence, a supported configuration.
func longObservation(o *postgres.Options) { o.Retention.Observation = 100 * time.Hour }

// TestPausedAdmissionCannotRepeatPrunedEffect: a delivery stalls inside its
// transaction right after its first clock reading, before the inbox insert.
// Meanwhile another worker completes the event and retention prunes the inbox
// entry and the outbox row while the observation is retained. Resumed, the
// delivery must be refused: neither the stale reading nor the surviving
// observation may readmit the event. Real PostgreSQL.
func TestPausedAdmissionCannotRepeatPrunedEffect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, longObservation, ledgerBinding())
	effectTable(f)
	env := eventA("pa_evt", 1)
	f.append(env)
	markPublished(t, f)

	reached, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	opts := eventstestDefaults(f)
	longObservation(&opts)
	opts.Clock = func() time.Time {
		now := f.clock.Now()
		if calls.Add(1) == 1 { // the transaction's first reading
			close(reached)
			<-release
		}
		return now // the reading taken before the pause, then live ones
	}
	stalled, err := postgres.NewStore(f.env.Runtime, opts, ledgerBinding())
	if err != nil {
		t.Fatal(err)
	}
	payload := mustPayload(t, env)
	type result struct {
		out events.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := stalled.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
		done <- result{out, err}
	}()
	<-reached

	if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
		t.Fatalf("other worker: %+v", out)
	}
	f.clock.Advance(60 * time.Hour)
	pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Inbox != 1 || pr.Outbox != 1 || pr.Observations != 0 {
		t.Fatalf("prune: %+v %v (the observation must be retained)", pr, err)
	}

	close(release)
	got := <-done
	if got.err != nil || got.out.Kind != events.OutcomeQuarantined || got.out.Reason != postgres.ReasonBeyondHorizon {
		t.Fatalf("resumed delivery: %+v %v", got.out, got.err)
	}
	if n := f.effects("ledger", "pa_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM event_inbox`); n != 0 {
		t.Fatalf("inbox entry recreated: %d", n)
	}
}

// TestRetentionIncreaseDoesNotReadmitPrunedEffects: evidence pruned under a
// shorter Inbox cannot be restored by restarting with a longer one. Live
// delivery, repair and replay admission all keep treating the event as having
// lost its evidence. Real PostgreSQL.
func TestRetentionIncreaseDoesNotReadmitPrunedEffects(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, longObservation, ledgerBinding())
	effectTable(f)
	old := eventA("rt_old", 1)
	f.append(old)
	if out := deliver(t, f, old); out.Kind != events.OutcomeCompleted {
		t.Fatalf("first delivery: %+v", out)
	}
	markPublished(t, f)
	f.clock.Advance(60 * time.Hour)
	if pr, err := f.store.Prune(ctx, tenantA, f.clock.Now()); err != nil || pr.Inbox != 1 || pr.Outbox != 1 || pr.Observations != 0 {
		t.Fatalf("prune: %+v %v", pr, err)
	}

	// Restart with a larger inbox retention: both settings validate.
	opts := eventstestDefaults(f)
	longObservation(&opts)
	opts.Retention.Inbox = 96 * time.Hour
	restarted, err := postgres.NewStore(f.env.Runtime, opts, ledgerBinding())
	if err != nil {
		t.Fatal(err)
	}

	// Repair does not select it, and a delivery of it is refused.
	if res, err := restarted.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 0 || res.Retried != 0 {
		t.Fatalf("repair: %+v %v", res, err)
	}
	payload := mustPayload(t, old)
	out, err := restarted.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
	if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon {
		t.Fatalf("redelivery after restart: %+v %v", out, err)
	}

	// Replay admission classifies it as beyond the horizon and refuses it, with
	// or without the beyond-horizon acknowledgment (the consumer is not declared
	// idempotent).
	for _, allow := range []bool{false, true} {
		req := replayReq("ledger", 1, 1)
		req.AllowBeyondHorizon = allow
		c, err := restarted.DryRun(ctx, req)
		if !errors.Is(err, events.ErrReplayBeyondHorizon) || c.BeyondHorizon != 1 || c.Undelivered != 0 {
			t.Fatalf("dry run (allow=%v): %+v %v", allow, c, err)
		}
		if _, err := restarted.RequestReplay(ctx, req); !errors.Is(err, events.ErrReplayBeyondHorizon) {
			t.Fatalf("request (allow=%v): %v", allow, err)
		}
	}
	if n := f.effects("ledger", "rt_old"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}

	// Events created after the boundary are still admitted by the larger window.
	fresh := f.fresh(eventA("rt_new", 2))
	f.append(fresh)
	out, err = restarted.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: mustPayload(t, fresh)})
	if err != nil || out.Kind != events.OutcomeCompleted {
		t.Fatalf("delivery of a fresh event: %+v %v", out, err)
	}
	if n := f.effects("ledger", "rt_new"); n != 1 {
		t.Fatalf("fresh effects = %d", n)
	}

	// The boundary is monotonic in the database itself.
	err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE event_dedup_boundary SET inbox_before = inbox_before - interval '1 hour' WHERE tenant_id = $1`, tenantA)
		return err
	})
	if pgCode(err) != "23514" {
		t.Fatalf("lowering the boundary: %v", err)
	}
}

// TestBoundaryDoesNotBlockAdmittedReplay: a replay admitted for an undelivered
// event keeps its admission-time horizon even though later pruning advances the
// durable boundary past the event, because no evidence for it can have been
// forgotten while the request pinned it. Real PostgreSQL.
func TestBoundaryDoesNotBlockAdmittedReplay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, longObservation, ledgerBinding())
	effectTable(f)
	ev := eventA("pn_evt", 1)
	f.append(ev)
	rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 1))
	if err != nil || rec.Classification.Undelivered != 1 || rec.EvidenceFloor.IsZero() {
		t.Fatalf("request: %+v %v", rec, err)
	}
	markPublished(t, f)
	f.clock.Advance(60 * time.Hour)
	if _, err := f.store.Prune(ctx, tenantA, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.RunReplay(ctx, tenantA, rec.ID)
	if err != nil || got.State != "completed" || got.Processed != 1 || f.effects("ledger", "pn_evt") != 1 {
		t.Fatalf("replay: %+v %v effects=%d", got, err, f.effects("ledger", "pn_evt"))
	}
}

func mustPayload(t testing.TB, env eventsv1Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
