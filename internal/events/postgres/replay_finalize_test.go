package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// crashAfterDB fails every transaction begun after trip is set, as a process
// that died at that point would: nothing further is persisted.
type crashAfterDB struct {
	postgres.DB
	trip *atomic.Bool
}

var errCrashed = errors.New("injected crash")

func (d crashAfterDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	if d.trip.Load() {
		return nil, errCrashed
	}
	return d.DB.BeginTx(ctx, o)
}

func countAudit(f *fixture, action string) int {
	return f.count(tenantA, `SELECT count(*) FROM audit WHERE action = $1`, action)
}

// TestReplayFinalizationResumes: a runner that dies after the last event's
// transaction committed but before the request is finalized leaves the request
// running with next_seq past its range. The next runner must complete it
// exactly once, with its audit row and durable counters, without repeating any
// effect, and the request must stop blocking overlapping replays.
func TestReplayFinalizationResumes(t *testing.T) {
	ctx := context.Background()
	var trip atomic.Bool
	const last = "fin02"
	binding := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'ledger', $2)`, string(env.TenantID), env.ID); err != nil {
				return err
			}
			if env.ID == last {
				trip.Store(true) // the process dies once this event's transaction ends
			}
			return nil
		},
	}
	f := newFixture(t, func(o *postgres.Options) { o.ReplayBatch = 10; o.ReplayLease = 300 * time.Millisecond }, binding)
	effectTable(f)
	appendN(f, "fin", 3) // fin00..fin02

	rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 3))
	if err != nil {
		t.Fatal(err)
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = f.clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	opts.ReplayBatch, opts.ReplayLease = 10, 300*time.Millisecond
	dying, err := postgres.NewStore(crashAfterDB{DB: f.env.Runtime, trip: &trip}, opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dying.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, errCrashed) {
		t.Fatalf("the injected crash did not surface: %v", err)
	}
	stuck, err := f.store.GetReplay(ctx, tenantA, rec.ID)
	if err != nil || stuck.State != "running" || stuck.NextSeq != 4 || stuck.Processed != 3 {
		t.Fatalf("state in the crash window: %+v %v", stuck, err)
	}
	if countAudit(f, "event.replay_completed") != 0 {
		t.Fatal("completion was audited before finalization")
	}
	// The stuck request keeps overlapping replays out.
	if _, err := f.store.RequestReplay(ctx, replayReq("ledger", 2, 3)); !errors.Is(err, events.ErrReplayUnsafe) {
		t.Fatalf("overlap while the request is active: %v", err)
	}

	time.Sleep(400 * time.Millisecond) // the dead runner's lease lapses
	if n, err := f.store.ResumeReplays(ctx, tenantA); err != nil || n != 1 {
		t.Fatalf("resume: %d %v", n, err)
	}
	done, err := f.store.GetReplay(ctx, tenantA, rec.ID)
	if err != nil || done.State != "completed" || done.CompletedAt.IsZero() || done.Processed != 3 || done.Skipped != 0 || done.Failed != 0 {
		t.Fatalf("after restart: %+v %v", done, err)
	}
	if got := countAudit(f, "event.replay_completed"); got != 1 {
		t.Fatalf("completion audit rows = %d, want 1", got)
	}
	var processed float64
	if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (metadata->>'processed')::float8 FROM audit WHERE action = 'event.replay_completed'`).Scan(&processed)
	}); err != nil || processed != 3 {
		t.Fatalf("audit reports the whole request, got processed=%v err=%v", processed, err)
	}
	for _, id := range []string{"fin00", "fin01", "fin02"} {
		if got := f.effects("ledger", id); got != 1 {
			t.Fatalf("%s has %d effect(s)", id, got)
		}
	}
	// Exactly once: a further resume or run finds nothing to do and audits nothing.
	if n, err := f.store.ResumeReplays(ctx, tenantA); err != nil || n != 0 {
		t.Fatalf("second resume: %d %v", n, err)
	}
	if again, err := f.store.RunReplay(ctx, tenantA, rec.ID); err != nil || again.State != "completed" {
		t.Fatalf("run of a completed request: %+v %v", again, err)
	}
	if got := countAudit(f, "event.replay_completed"); got != 1 {
		t.Fatalf("completion audited %d times", got)
	}
	// The request no longer blocks overlapping replays.
	if _, err := f.store.RequestReplay(ctx, replayReq("ledger", 2, 3)); err != nil {
		t.Fatalf("a completed request must not block a new one: %v", err)
	}
}

// TestQuarantineDiscardCycles: quarantine, discard, duplicate delivery and a
// second resolution keep working, with both resolutions audited. A redelivery
// of discarded raw poison keeps its terminal outcome; an inbox entry reopened
// by a replay that fails again starts a new cycle with its own audit identity.
func TestQuarantineDiscardCycles(t *testing.T) {
	ctx := context.Background()
	reject := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(context.Context, pgx.Tx, eventsv1.Envelope) error {
			return events.Permanent(errors.New("cannot apply"))
		},
	}
	ops := events.Operator{ID: "ops_1"}

	t.Run("raw poison", func(t *testing.T) {
		f := newFixture(t, nil, reject)
		poison := []byte(`{"id":"evt_garbled","type":`)
		deliver := func() events.Outcome {
			out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: poison})
			if err != nil || out.Kind != events.OutcomeQuarantined {
				t.Fatalf("delivery: %+v %v", out, err)
			}
			return out
		}
		first := deliver()
		if err := f.store.DiscardQuarantine(ctx, tenantA, first.QuarantineID, ops, "garbage"); err != nil {
			t.Fatal(err)
		}
		// A duplicate of the discarded message does not reopen the decision.
		if again := deliver(); again.QuarantineID != first.QuarantineID {
			t.Fatalf("duplicate got a different entry: %s vs %s", again.QuarantineID, first.QuarantineID)
		}
		if left, _ := f.store.ListQuarantine(ctx, tenantA, "ledger", 10); len(left) != 0 {
			t.Fatalf("a discarded message was reopened by a duplicate delivery: %+v", left)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE state = 'resolved' AND resolution = 'discarded'`); got != 1 {
			t.Fatalf("discarded records = %d", got)
		}
		if got := countAudit(f, "event.quarantine_discarded"); got != 1 {
			t.Fatalf("discard audit rows = %d", got)
		}
		// The discarded record can still be removed by retention, and a message
		// arriving after that is a fresh quarantine with its own audit identity.
		if res, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(3*time.Hour)); err != nil || res.Quarantine != 1 {
			t.Fatalf("prune: %+v %v", res, err)
		}
		third := deliver()
		if err := f.store.DiscardQuarantine(ctx, tenantA, third.QuarantineID, ops, "garbage again"); err != nil {
			t.Fatalf("second resolution: %v", err)
		}
		if got := countAudit(f, "event.quarantine_discarded"); got != 2 {
			t.Fatalf("discard audit rows = %d", got)
		}
	})

	t.Run("replayed event that fails again", func(t *testing.T) {
		f := newFixture(t, nil, reject)
		f.append(eventA("evt_cyc", 1))
		payload := readAll(t, f)[0].Payload
		out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
		if err != nil || out.Kind != events.OutcomeQuarantined || out.QuarantineID == "" {
			t.Fatalf("first delivery: %+v %v", out, err)
		}
		if err := f.store.DiscardQuarantine(ctx, tenantA, out.QuarantineID, ops, "first look"); err != nil {
			t.Fatal(err)
		}
		// A duplicate does not reopen the discarded entry.
		if dup, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload}); err != nil || dup.Kind != events.OutcomeQuarantined {
			t.Fatalf("duplicate: %+v %v", dup, err)
		}
		if left, _ := f.store.ListQuarantine(ctx, tenantA, "ledger", 10); len(left) != 0 {
			t.Fatalf("duplicate reopened the entry: %+v", left)
		}
		// An operator replays the event; it fails again, so the entry reopens.
		rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if done, err := f.store.RunReplay(ctx, tenantA, rec.ID); err != nil || done.State != "completed" || done.Failed != 1 {
			t.Fatalf("replay: %+v %v", done, err)
		}
		left, _ := f.store.ListQuarantine(ctx, tenantA, "ledger", 10)
		if len(left) != 1 || left[0].ID != out.QuarantineID {
			t.Fatalf("the failed replay must reopen the entry: %+v", left)
		}
		if err := f.store.DiscardQuarantine(ctx, tenantA, left[0].ID, ops, "second look"); err != nil {
			t.Fatalf("second discard (new cycle): %v", err)
		}
		if got := countAudit(f, "event.quarantine_discarded"); got != 2 {
			t.Fatalf("discard audit rows = %d, want one per cycle", got)
		}
		if got := f.count(tenantA, `SELECT count(DISTINCT id) FROM audit WHERE action = 'event.quarantine_discarded'`); got != 2 {
			t.Fatalf("audit identities are not distinct")
		}
	})
}
