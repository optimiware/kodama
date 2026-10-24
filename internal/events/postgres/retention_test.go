package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

func admit(f *fixture, envs []eventsv1.Envelope) (admitted, rejected int) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range envs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.store.Append(ctx, tx, e)
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, events.ErrBacklogFull):
				rejected++
			default:
				f.t.Errorf("append %s: %v", e.ID, err)
			}
		}()
	}
	wg.Wait()
	return admitted, rejected
}

func manyEvents(prefix string, n int) []eventsv1.Envelope {
	out := make([]eventsv1.Envelope, n)
	for i := range out {
		out[i] = eventA(fmt.Sprintf("%s%02d", prefix, i), int64(i+1))
	}
	return out
}

func noop(name string) postgres.Binding {
	return postgres.Binding{
		Spec:   events.ConsumerSpec{Name: name, Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(context.Context, pgx.Tx, eventsv1.Envelope) error { return nil },
	}
}

// TestRetentionBackpressure: independent retention preserves pending work and
// deduplication safety; concurrent admission respects count and byte caps;
// cleanup and replay races remain safe.
func TestRetentionBackpressure(t *testing.T) {
	ctx := context.Background()

	t.Run("concurrent admission respects the count cap", func(t *testing.T) {
		f := newFixture(t, func(o *postgres.Options) { o.Backlog = events.BacklogLimits{MaxEvents: 5, MaxBytes: 1 << 20} })
		admitted, rejected := admit(f, manyEvents("cap", 20))
		if admitted != 5 || rejected != 15 {
			t.Fatalf("admitted=%d rejected=%d", admitted, rejected)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox`); got != 5 {
			t.Fatalf("outbox rows = %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_observations`); got != 5 {
			t.Fatalf("observations = %d (rejected events must leave nothing)", got)
		}
		// Draining by publication frees capacity, so publishers are never blocked by a full backlog.
		now := f.clock.Now().Add(time.Second)
		claims, err := f.store.ClaimDue(ctx, tenantA, now, time.Minute, 2)
		if err != nil || len(claims) != 2 {
			t.Fatalf("claim while full: %+v %v", claims, err)
		}
		for _, c := range claims {
			if err := f.store.Complete(ctx, c, now); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.count(tenantA, `SELECT pending_count FROM event_backlog`); got != 3 {
			t.Fatalf("backlog after publishing 2 = %d", got)
		}
		admitted, rejected = admit(f, manyEvents("refill", 6))
		if admitted != 2 || rejected != 4 {
			t.Fatalf("after draining: admitted=%d rejected=%d", admitted, rejected)
		}
	})

	t.Run("concurrent admission respects the byte cap", func(t *testing.T) {
		payload, err := eventsv1.Encode(eventA("size00", 1))
		if err != nil {
			t.Fatal(err)
		}
		f := newFixture(t, func(o *postgres.Options) {
			o.Backlog = events.BacklogLimits{MaxEvents: 1000, MaxBytes: int64(len(payload))*3 + 40}
		})
		admitted, rejected := admit(f, manyEvents("size", 12))
		if admitted != 3 || rejected != 9 {
			t.Fatalf("admitted=%d rejected=%d (payload %d bytes)", admitted, rejected, len(payload))
		}
		if got := f.count(tenantA, `SELECT pending_bytes FROM event_backlog`); got > int(int64(len(payload))*3+40) {
			t.Fatalf("pending bytes %d exceed the cap", got)
		}
	})

	t.Run("independent lifetimes preserve pending work and deduplication", func(t *testing.T) {
		f := newFixture(t, nil, noop("audit"))
		for _, e := range manyEvents("life", 4) {
			f.append(e)
		}
		obs := readAll(t, f)
		// Publish three of the four; one stays pending.
		now := f.clock.Now().Add(time.Second)
		claims, _ := f.store.ClaimDue(ctx, tenantA, now, time.Minute, 3)
		for _, c := range claims {
			if err := f.store.Complete(ctx, c, now); err != nil {
				t.Fatal(err)
			}
		}
		// Deduplication evidence for every event, a retry-pending entry and a quarantine.
		for _, o := range obs {
			if out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "audit", Payload: o.Payload}); err != nil || out.Kind != events.OutcomeCompleted {
				t.Fatalf("process: %+v %v", out, err)
			}
		}
		poison := postgres.Binding{Spec: events.ConsumerSpec{Name: "poison", Mode: events.ModeHistorical, Effect: events.EffectLocal},
			Handle: func(context.Context, pgx.Tx, eventsv1.Envelope) error { return events.Permanent(errors.New("poison")) }}
		flaky := postgres.Binding{Spec: events.ConsumerSpec{Name: "flaky", Mode: events.ModeHistorical, Effect: events.EffectLocal},
			Handle: func(context.Context, pgx.Tx, eventsv1.Envelope) error { return events.Transient(errors.New("later")) }}
		s2 := f.env.NewStore(t, func() postgres.Options { o := eventstestDefaults(f); return o }(), noop("audit"), poison, flaky)
		if out, _ := s2.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "poison", Payload: obs[0].Payload}); out.Kind != events.OutcomeQuarantined {
			t.Fatalf("poison: %+v", out)
		}
		if out, _ := s2.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "flaky", Payload: obs[1].Payload}); out.Kind != events.OutcomeRetry {
			t.Fatalf("flaky: %+v", out)
		}

		// 2h later: the configured outbox retention (1h) has passed, but a
		// published row is the persisted record of the last broker acceptance and
		// is kept for the broker redelivery horizon (24h), so nothing is pruned.
		res, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(2*time.Hour))
		if err != nil || res.Outbox != 0 || res.Inbox != 0 || res.Observations != 0 {
			t.Fatalf("prune at +2h: %+v %v", res, err)
		}
		// 25h later the broker horizon has passed: published history is gone;
		// everything else stays.
		res, err = f.store.Prune(ctx, tenantA, f.clock.Now().Add(25*time.Hour))
		if err != nil || res.Outbox != 3 || res.Inbox != 0 || res.Observations != 0 {
			t.Fatalf("prune at +25h: %+v %v", res, err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE state <> 'published'`); got != 1 {
			t.Fatalf("the pending publication was pruned: %d left", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox_attempts`); got != 0 {
			t.Fatalf("attempt history follows only its pruned outbox rows, %d left", got) // the pending event was never attempted
		}
		// Deduplication survives the outbox pruning: a redelivery is still a duplicate.
		if out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "audit", Payload: obs[2].Payload}); err != nil || out.Kind != events.OutcomeDuplicate {
			t.Fatalf("redelivery after outbox pruning: %+v %v", out, err)
		}

		// 60h later: observation history and the completed inbox evidence of the
		// published events expire. The evidence of the still-unpublished event
		// stays (a late publication starts a new broker lifetime), and the
		// pending publication, retry entry and quarantine never expire.
		res, err = f.store.Prune(ctx, tenantA, f.clock.Now().Add(60*time.Hour))
		if err != nil || res.Inbox != 3 || res.Observations != 4 {
			t.Fatalf("prune at +60h: %+v %v", res, err)
		}
		var unpublished string
		if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT event_id FROM outbox WHERE state <> 'published'`).Scan(&unpublished)
		}); err != nil {
			t.Fatal(err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'completed' AND consumer = 'audit' AND event_id = $1`, unpublished); got != 1 {
			t.Fatalf("deduplication evidence of the unpublished event %s was forgotten", unpublished)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'quarantined'`); got != 1 {
			t.Fatalf("quarantined entry pruned: %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'pending'`); got != 1 {
			t.Fatalf("pending retry entry pruned: %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE state = 'quarantined'`); got != 1 {
			t.Fatalf("unresolved quarantine pruned: %d", got)
		}
		// The unpublished event outlived its observation record and is still publishable.
		lateAt := f.clock.Now().Add(61 * time.Hour)
		late, err := f.store.ClaimDue(ctx, tenantA, lateAt, time.Minute, 10)
		if err != nil || len(late) != 1 || len(late[0].Payload) == 0 {
			t.Fatalf("pending publication after observation pruning: %+v %v", late, err)
		}
		// Once it is published, its evidence is kept for the broker horizon from
		// that publication, then forgotten.
		if err := f.store.Complete(ctx, late[0], lateAt); err != nil {
			t.Fatal(err)
		}
		if res, err := f.store.Prune(ctx, tenantA, lateAt.Add(time.Hour)); err != nil || res.Inbox != 0 {
			t.Fatalf("prune within the horizon of the late publication: %+v %v", res, err)
		}
		if res, err := f.store.Prune(ctx, tenantA, lateAt.Add(25*time.Hour)); err != nil || res.Inbox != 1 || res.Outbox != 1 {
			t.Fatalf("prune after the horizon of the late publication: %+v %v", res, err)
		}
		// The database itself refuses to delete unresolved work.
		for _, stmt := range []string{`DELETE FROM event_quarantine`, `DELETE FROM event_inbox WHERE state <> 'completed'`, `DELETE FROM outbox WHERE state <> 'published'`} {
			var affected int64
			if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, stmt)
				affected = tag.RowsAffected()
				return err
			}); err != nil || affected != 0 {
				t.Fatalf("%s deleted %d row(s): %v", stmt, affected, err)
			}
		}
	})

	t.Run("cleanup never removes what an active replay still needs", func(t *testing.T) {
		f := newFixture(t, func(o *postgres.Options) { o.ReplayBatch = 2 }, noop("audit"))
		for _, e := range manyEvents("rp", 8) {
			f.append(e)
		}
		req := events.ReplayRequest{Scope: events.Scope{TenantID: tenantA, ProjectID: projectA}, Consumer: "audit", FromSeq: 4, ToSeq: 8, Reason: "backfill", Operator: events.Operator{ID: "ops_1"}}
		rec, err := f.store.RequestReplay(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		// Cleanup after the retention elapsed removes only history before the replay's range.
		res, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(60*time.Hour))
		if err != nil || res.Observations != 3 {
			t.Fatalf("prune with an active replay: %+v %v", res, err)
		}
		if _, w, _ := readPageRaw(f); w.Floor != 3 {
			t.Fatalf("floor = %d, want 3", w.Floor)
		}
		// Race: cleanup keeps running while the replay is processed.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(60*time.Hour)); err != nil {
						t.Errorf("prune: %v", err)
						return
					}
				}
			}
		}()
		done, err := f.store.RunReplay(ctx, tenantA, rec.ID)
		close(stop)
		wg.Wait()
		if err != nil || done.State != "completed" || done.Processed != 5 || done.Failed != 0 {
			t.Fatalf("replay racing cleanup: %+v %v", done, err)
		}
		// With the replay completed the rest becomes prunable.
		if _, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(60*time.Hour)); err != nil {
			t.Fatalf("final prune: %v", err)
		}
		if _, w, _ := readPageRaw(f); w.Floor != 8 {
			t.Fatalf("floor after the replay finished = %d, want 8", w.Floor)
		}
		// Completed replay requests expire by their own retention (the racing
		// cleanup above already removed it); the merged audit trail is untouched.
		if got := f.count(tenantA, `SELECT count(*) FROM event_replay_requests`); got != 0 {
			t.Fatalf("completed replay request outlived its retention: %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM audit WHERE action IN ('event.replay_requested', 'event.replay_completed')`); got != 2 {
			t.Fatalf("replay audit rows = %d, want 2 (audit retention is not ours)", got)
		}
	})
}
