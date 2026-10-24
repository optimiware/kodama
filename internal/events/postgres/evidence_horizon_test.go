package postgres_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
)

func ledgerBinding() postgres.Binding {
	return effectBinding("ledger", events.EffectLocal, nil)
}

// markPublished claims and marks every due outbox row published at the fixture's
// current time, as a healthy publisher would.
func markPublished(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	claims, err := f.store.ClaimDue(ctx, tenantA, f.clock.Now(), time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if err := f.store.Complete(ctx, c, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func deliver(t *testing.T, f *fixture, env eventsv1.Envelope) events.Outcome {
	t.Helper()
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.store.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
	if err != nil {
		t.Fatalf("process %s: %v", env.ID, err)
	}
	return out
}

// TestEvidenceHorizonBoundary: a delivery with no inbox entry is admitted as
// new only while the entry cannot have been pruned; the decision is taken in
// the processing transaction from durable facts. Real PostgreSQL.
func TestEvidenceHorizonBoundary(t *testing.T) {
	// Retention: Inbox 48h, BrokerRedelivery 24h (eventstest defaults).
	cases := []struct {
		name     string
		setup    func(t *testing.T, f *fixture) // after Append of the event
		admitted bool
	}{
		{"fresh event", func(t *testing.T, f *fixture) {}, true},
		{"unpublished event older than the inbox horizon", func(t *testing.T, f *fixture) {
			f.clock.Advance(100 * time.Hour) // still pending/claimable: its evidence cannot be pruned
		}, true},
		{"published within the broker horizon, origin older than the inbox horizon", func(t *testing.T, f *fixture) {
			f.clock.Advance(60 * time.Hour)
			markPublished(t, f)
			f.clock.Advance(time.Hour)
		}, true},
		{"published beyond the broker horizon, origin within the inbox horizon", func(t *testing.T, f *fixture) {
			markPublished(t, f)
			f.clock.Advance(30 * time.Hour)
		}, true},
		{"published beyond the broker horizon, origin beyond the inbox horizon", func(t *testing.T, f *fixture) {
			markPublished(t, f)
			f.clock.Advance(60 * time.Hour)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, ledgerBinding())
			effectTable(f)
			env := eventA("hz_evt", 1)
			f.append(env)
			tc.setup(t, f)
			out := deliver(t, f, env)
			if tc.admitted {
				if out.Kind != events.OutcomeCompleted || f.effects("ledger", "hz_evt") != 1 {
					t.Fatalf("not admitted: %+v effects=%d", out, f.effects("ledger", "hz_evt"))
				}
				return
			}
			if out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon {
				t.Fatalf("outcome: %+v", out)
			}
			if f.effects("ledger", "hz_evt") != 0 || f.count(tenantA, `SELECT count(*) FROM event_inbox`) != 0 {
				t.Fatal("a refused delivery left an effect or an inbox entry")
			}
			if f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE reason_code = 'beyond_horizon'`) != 1 {
				t.Fatal("refusal was not recorded for the operator")
			}
		})
	}

	t.Run("event with no outbox row and no observation", func(t *testing.T) {
		f := newFixture(t, nil, ledgerBinding())
		effectTable(f)
		out := deliver(t, f, eventA("hz_unknown", 1)) // never appended: origin unknown
		if out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon || f.effects("ledger", "hz_unknown") != 0 {
			t.Fatalf("outcome: %+v", out)
		}
	})

	t.Run("completed entry is still a duplicate, never refused", func(t *testing.T) {
		f := newFixture(t, nil, ledgerBinding())
		effectTable(f)
		env := eventA("hz_dup", 1)
		f.append(env)
		if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
			t.Fatalf("first: %+v", out)
		}
		markPublished(t, f)
		f.clock.Advance(30 * time.Hour) // evidence exists although the outbox row is past the broker horizon
		if out := deliver(t, f, env); out.Kind != events.OutcomeDuplicate || f.effects("ledger", "hz_dup") != 1 {
			t.Fatalf("duplicate: %+v", out)
		}
	})
}

// pausingDB holds the n-th transaction begun through it until released, as a
// worker stalled between two transactions would.
type pausingDB struct {
	postgres.DB
	pauseAt int32
	calls   *atomic.Int32
	reached chan struct{}
	release chan struct{}
}

func (d pausingDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	if d.calls.Add(1) == d.pauseAt {
		close(d.reached)
		<-d.release
	}
	return d.DB.BeginTx(ctx, o)
}

// TestRepairRevalidatesEvidenceHorizon: a repair worker selects a backfill item
// and stalls before processing it. Meanwhile the event is completed elsewhere
// and retention forgets the inbox entry, the outbox row and the observation.
// Resumed, the worker must not run the effect again.
func TestRepairRevalidatesEvidenceHorizon(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, ledgerBinding())
	effectTable(f)
	env := eventA("rp_evt", 1)
	f.append(env)

	opts := eventstest.DefaultOptions()
	opts.Clock = f.clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	pause := pausingDB{DB: f.env.Runtime, pauseAt: 2, calls: new(atomic.Int32), reached: make(chan struct{}), release: make(chan struct{})}
	stalled, err := postgres.NewStore(pause, opts, ledgerBinding())
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res postgres.RepairResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := stalled.Repair(ctx, tenantA, "ledger", 0, 10)
		done <- outcome{res, err}
	}()
	<-pause.reached // selected, not yet processed

	if res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 1 {
		t.Fatalf("repair elsewhere: %+v %v", res, err)
	}
	markPublished(t, f)
	f.clock.Advance(50 * time.Hour)
	pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Inbox != 1 || pr.Outbox != 1 || pr.Observations != 1 {
		t.Fatalf("prune: %+v %v", pr, err)
	}

	close(pause.release)
	got := <-done
	if got.err != nil || got.res.Refused != 1 || got.res.Backfilled != 0 {
		t.Fatalf("resumed repair: %+v %v", got.res, got.err)
	}
	if n := f.effects("ledger", "rp_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM event_inbox`); n != 0 {
		t.Fatalf("inbox entries recreated: %d", n)
	}
}
