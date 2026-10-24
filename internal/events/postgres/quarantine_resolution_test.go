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
	"github.com/optimiware/kodama/internal/events/postgres"
)

// TestQuarantineResolvedWhenReplayedEventCompletesLater: an authorized replay
// reopens a quarantined event, its handler fails transiently, and the replay
// moves on. The event is completed afterwards by a path that carries no replay
// (repair after a restart, or an ordinary delivery). The completion must
// resolve the quarantine record for the operator who authorized the replay, in
// the same transaction as the effect, so that exactly one effect exists, no
// unresolved failure is reported for it, and retention can clean the record up.
// Real PostgreSQL.
func TestQuarantineResolvedWhenReplayedEventCompletesLater(t *testing.T) {
	const (
		permanent = iota
		transient
		healthy
	)
	completions := map[string]func(t *testing.T, restarted *postgres.Store, payload []byte){
		"repair after restart": func(t *testing.T, restarted *postgres.Store, _ []byte) {
			res, err := restarted.Repair(context.Background(), tenantA, "ledger", 0, 10)
			if err != nil || res.Retried != 1 || res.Failed != 0 {
				t.Fatalf("repair: %+v %v", res, err)
			}
		},
		"ordinary delivery after restart": func(t *testing.T, restarted *postgres.Store, payload []byte) {
			out, err := restarted.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
			if err != nil || out.Kind != events.OutcomeCompleted {
				t.Fatalf("delivery: %+v %v", out, err)
			}
		},
	}
	for name, complete := range completions {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var mode atomic.Int32
			binding := func() postgres.Binding {
				return postgres.Binding{
					Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal},
					Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
						switch mode.Load() {
						case permanent:
							return events.Permanent(errors.New("cannot apply"))
						case transient:
							return events.Transient(errors.New("dependency unavailable"))
						}
						_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'ledger', $2)`, string(env.TenantID), env.ID)
						return err
					},
				}
			}
			f := newFixture(t, nil, binding())
			effectTable(f)
			f.append(eventA("evt_qr", 1))
			payload := readAll(t, f)[0].Payload

			// The event is quarantined as a permanent failure.
			out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
			if err != nil || out.Kind != events.OutcomeQuarantined {
				t.Fatalf("first delivery: %+v %v", out, err)
			}

			// An authorized replay reopens it; the handler now fails transiently.
			mode.Store(transient)
			rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 1))
			if err != nil {
				t.Fatal(err)
			}
			if done, err := f.store.RunReplay(ctx, tenantA, rec.ID); err != nil || done.State != "completed" || done.Failed != 1 {
				t.Fatalf("replay: %+v %v", done, err)
			}
			if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'pending' AND event_id = 'evt_qr'`); got != 1 {
				t.Fatalf("the failed replay must leave a pending retry, got %d", got)
			}
			// Still unresolved: the effect has not happened yet.
			if open, _ := f.store.ListQuarantine(ctx, tenantA, "ledger", 10); len(open) != 1 {
				t.Fatalf("quarantine must stay open until the effect completes: %+v", open)
			}
			if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE replay_by = 'ops_1' AND replay_id = $1`, rec.ID); got != 1 {
				t.Fatalf("the quarantine record does not carry the authorizing replay")
			}

			// The process restarts, the handler is repaired and the retry is due.
			mode.Store(healthy)
			f.clock.Advance(time.Minute)
			restarted := f.env.NewStore(t, eventstestDefaults(f), binding())
			complete(t, restarted, payload)

			if got := f.effects("ledger", "evt_qr"); got != 1 {
				t.Fatalf("effects = %d, want exactly one", got)
			}
			if open, _ := restarted.ListQuarantine(ctx, tenantA, "ledger", 10); len(open) != 0 {
				t.Fatalf("a completed event is still reported as an unresolved failure: %+v", open)
			}
			if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE state = 'resolved' AND resolution = 'replayed' AND resolved_by = 'ops_1' AND replay_id = $1`, rec.ID); got != 1 {
				t.Fatalf("the quarantine record was not resolved for the replay operator")
			}
			// Replay history is intact: the reopen is recorded with its replay.
			if got := f.count(tenantA, `SELECT count(*) FROM event_inbox_attempts WHERE event_id = 'evt_qr' AND outcome = 'replay_reopened' AND replay_id = $1`, rec.ID); got != 1 {
				t.Fatalf("replay history lost")
			}
			if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE event_id = 'evt_qr' AND state = 'completed'`); got != 1 {
				t.Fatalf("inbox entry is not completed")
			}
			// A further replay of the completed event neither repeats the effect
			// nor disturbs the resolution.
			if again, err := restarted.RequestReplay(ctx, replayReq("ledger", 1, 1)); err == nil {
				if _, err := restarted.RunReplay(ctx, tenantA, again.ID); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.effects("ledger", "evt_qr"); got != 1 {
				t.Fatalf("effects after another replay = %d", got)
			}

			// Retention can now remove the resolved record without a manual discard.
			if res, err := restarted.Prune(ctx, tenantA, f.clock.Now().Add(2*time.Hour)); err != nil || res.Quarantine != 1 {
				t.Fatalf("prune: %+v %v", res, err)
			}
			if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine`); got != 0 {
				t.Fatalf("quarantine rows left after retention: %d", got)
			}
			if got := countAudit(f, "event.quarantine_discarded"); got != 0 {
				t.Fatalf("resolution must not look like a manual discard: %d", got)
			}
		})
	}
}
