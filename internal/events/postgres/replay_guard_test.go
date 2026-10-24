package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

var scopeA = events.Scope{TenantID: tenantA, ProjectID: projectA}

// effectTable creates a business table without any uniqueness, so a repeated
// effect is visible as a second row rather than as a constraint failure.
func effectTable(f *fixture) {
	f.t.Helper()
	f.ownerExec(`CREATE TABLE t005_eff (tenant_id kodama_id NOT NULL, consumer text NOT NULL, event_id text NOT NULL)`)
	f.ownerExec(`ALTER TABLE t005_eff ENABLE ROW LEVEL SECURITY; ALTER TABLE t005_eff FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_all ON t005_eff USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
		GRANT SELECT, INSERT, UPDATE ON t005_eff TO ` + pgx.Identifier{f.env.DB.RuntimeRole}.Sanitize())
}

func (f *fixture) effects(consumer, eventID string) int {
	return f.count(tenantA, `SELECT count(*) FROM t005_eff WHERE consumer = $1 AND event_id = $2`, consumer, eventID)
}

func effectBinding(name string, effect events.EffectClass, hook func(ctx context.Context, env eventsv1.Envelope)) postgres.Binding {
	return postgres.Binding{
		Spec: events.ConsumerSpec{Name: name, Mode: events.ModeHistorical, Effect: effect},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			if hook != nil {
				hook(ctx, env)
			}
			_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, $2, $3)`, string(env.TenantID), name, env.ID)
			return err
		},
	}
}

// deliverAll hands every retained observation to the consumers, as live
// deliveries, so each has completed deduplication evidence.
func deliverAll(t *testing.T, f *fixture, consumers ...string) []postgres.Observation {
	t.Helper()
	obs := readAll(t, f)
	for _, o := range obs {
		for _, c := range consumers {
			out, err := f.store.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: c, Payload: o.Payload})
			if err != nil || out.Kind != events.OutcomeCompleted {
				t.Fatalf("deliver %s to %s: %+v %v", o.Envelope.ID, c, out, err)
			}
		}
	}
	return obs
}

// publishAll marks every pending event published, as a healthy publisher would.
func publishAll(t *testing.T, f *fixture) {
	t.Helper()
	now := f.clock.Now().Add(time.Second)
	claims, err := f.store.ClaimDue(context.Background(), tenantA, now, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if err := f.store.Complete(context.Background(), c, now); err != nil {
			t.Fatal(err)
		}
	}
}

func replayReq(consumer string, from, to int64) events.ReplayRequest {
	return events.ReplayRequest{Scope: scopeA, Consumer: consumer, FromSeq: from, ToSeq: to, Reason: "recovery", Operator: events.Operator{ID: "ops_1"}}
}

func appendN(f *fixture, prefix string, n int) {
	for _, e := range manyEvents(prefix, n) {
		f.append(e)
	}
}

// TestRepairRecoversRetryAfterHistoryExpired: a transient failure leaves a
// pending inbox entry; the broker copy and the observation history expire, the
// handler is repaired, and periodic repair completes the event from the
// payload kept on the inbox entry.
func TestRepairRecoversRetryAfterHistoryExpired(t *testing.T) {
	ctx := context.Background()
	var healthy atomic.Bool
	flaky := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "flaky", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			if !healthy.Load() {
				return events.Transient(errors.New("downstream down"))
			}
			_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'flaky', $2)`, string(env.TenantID), env.ID)
			return err
		},
	}
	f := newFixture(t, nil, flaky)
	effectTable(f)
	appendN(f, "rep", 2)
	obs := readAll(t, f)
	if out, err := f.store.Process(ctx, events.Delivery{Tenant: tenantA, Consumer: "flaky", Payload: obs[0].Payload}); err != nil || out.Kind != events.OutcomeRetry {
		t.Fatalf("first attempt: %+v %v", out, err)
	}
	if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'pending' AND payload IS NOT NULL`); got != 1 {
		t.Fatalf("the pending entry must keep its payload, got %d", got)
	}

	// Broker and observation retention elapse; cleanup removes the observations.
	later := f.clock.Now().Add(100 * time.Hour)
	res, err := f.store.Prune(ctx, tenantA, later)
	if err != nil || res.Observations != 2 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	if got := f.count(tenantA, `SELECT count(*) FROM event_observations`); got != 0 {
		t.Fatalf("observations left: %d", got)
	}
	if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'pending'`); got != 1 {
		t.Fatalf("the pending retry did not survive: %d", got)
	}

	// The handler is restored; the periodic repair pass finishes the event.
	healthy.Store(true)
	f.clock.Advance(100 * time.Hour)
	r, err := f.store.Repair(ctx, tenantA, "flaky", 0, 10)
	if err != nil || r.Retried != 1 || r.Failed != 0 {
		t.Fatalf("repair: %+v %v", r, err)
	}
	if got := f.effects("flaky", obs[0].Envelope.ID); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}
	if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'completed' AND payload IS NULL`); got != 1 {
		t.Fatalf("a completed entry must hold evidence, not the payload")
	}
	if r, err := f.store.Repair(ctx, tenantA, "flaky", 0, 10); err != nil || r.Retried != 0 {
		t.Fatalf("second repair pass: %+v %v", r, err)
	}
}

// TestReplayAdmissionRacesPruning: a replay request is admitted under the same
// scope lock pruning takes, so the range it classified cannot be pruned before
// the request pins it, and a lost range is never reported as a completed replay.
func TestReplayAdmissionRacesPruning(t *testing.T) {
	ctx := context.Background()

	t.Run("admission waits for a pruning transaction holding the scope", func(t *testing.T) {
		f := newFixture(t, nil, effectBinding("audit", events.EffectLocal, nil))
		effectTable(f)
		appendN(f, "adm", 6)
		deliverAll(t, f, "audit")

		locked, release, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			// What pruneObservations does: lock the scope, delete, raise the floor.
			held <- f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				var floor int64
				if err := tx.QueryRow(ctx, `SELECT floor_seq FROM event_scopes WHERE tenant_id = $1 AND scope_key = $2 FOR UPDATE`, tenantA, projectA).Scan(&floor); err != nil {
					return err
				}
				close(locked)
				<-release
				if _, err := tx.Exec(ctx, `DELETE FROM event_observations WHERE seq <= 3`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE event_scopes SET floor_seq = 3 WHERE scope_key = $1`, projectA)
				return err
			})
		}()
		<-locked
		admitted := make(chan error, 1)
		go func() {
			_, err := f.store.RequestReplay(ctx, replayReq("audit", 2, 5))
			admitted <- err
		}()
		select {
		case err := <-admitted:
			t.Fatalf("admission completed while pruning held the scope lock: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		close(release)
		if err := <-held; err != nil {
			t.Fatal(err)
		}
		if err := <-admitted; !errors.Is(err, events.ErrReplayRangeExpired) {
			t.Fatalf("admission after the prune committed: %v", err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_replay_requests`); got != 0 {
			t.Fatalf("a request for a pruned range was persisted: %d", got)
		}
	})

	t.Run("concurrent admission and pruning never lose an admitted range", func(t *testing.T) {
		// Only observation retention is under test: a very long inbox retention
		// keeps the deduplication boundary that the future-dated prune persists
		// from covering events the real-time clock still treats as fresh.
		f := newFixture(t, func(o *postgres.Options) { o.Retention.Inbox = 1000 * time.Hour }, effectBinding("audit", events.EffectLocal, nil))
		effectTable(f)
		late := func() time.Time { return f.clock.Now().Add(60 * time.Hour) } // observation retention has elapsed
		admittedRounds, expiredRounds := 0, 0
		for round := 0; round < 8; round++ {
			appendN(f, "rc"+string(rune('a'+round)), 4)
			var floor, high int64
			if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				wm, err := f.store.Watermark(ctx, tx, scopeA)
				floor, high = wm.Floor, wm.HighWater
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var rec postgres.ReplayRecord
			var reqErr, pruneErr error
			wg.Add(2)
			go func() { defer wg.Done(); rec, reqErr = f.store.RequestReplay(ctx, replayReq("audit", floor+1, high)) }()
			go func() { defer wg.Done(); _, pruneErr = f.store.Prune(ctx, tenantA, late()) }()
			wg.Wait()
			if pruneErr != nil {
				t.Fatalf("prune: %v", pruneErr)
			}
			if errors.Is(reqErr, events.ErrReplayRangeExpired) {
				expiredRounds++
				continue
			}
			if reqErr != nil {
				t.Fatalf("round %d: request: %v", round, reqErr)
			}
			admittedRounds++
			done, err := f.store.RunReplay(ctx, tenantA, rec.ID)
			if err != nil || done.State != "completed" || done.Processed+done.Skipped != rec.Classification.Events || rec.Classification.Events != int(high-floor) {
				t.Fatalf("round %d: an admitted range of %d event(s) ended as %+v (%v)", round, high-floor, done, err)
			}
			for _, o := range readAllSince(t, f, floor) {
				if got := f.effects("audit", o.Envelope.ID); got != 1 {
					t.Fatalf("round %d: %s has %d effect(s)", round, o.Envelope.ID, got)
				}
			}
		}
		t.Logf("admitted %d, expired %d", admittedRounds, expiredRounds)
	})

	t.Run("a lost range stops the run instead of completing it", func(t *testing.T) {
		var once sync.Once
		var f *fixture
		hook := func(ctx context.Context, env eventsv1.Envelope) {}
		f = newFixture(t, func(o *postgres.Options) { o.ReplayBatch = 2 }, postgres.Binding{
			Spec: events.ConsumerSpec{Name: "audit", Mode: events.ModeHistorical, Effect: events.EffectLocal},
			Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
				hook(ctx, env)
				once.Do(func() {
					// Observations beyond the first batch vanish through some path that
					// bypasses retention (a bug, an operator, a partial restore).
					if _, err := tx.Exec(ctx, `DELETE FROM event_observations WHERE seq BETWEEN 3 AND 4`); err != nil {
						t.Errorf("tamper: %v", err)
					}
					if _, err := tx.Exec(ctx, `UPDATE event_scopes SET floor_seq = 4`); err != nil {
						t.Errorf("tamper: %v", err)
					}
				})
				_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'audit', $2)`, string(env.TenantID), env.ID)
				return err
			},
		})
		effectTable(f)
		appendN(f, "lost", 6)
		rec, err := f.store.RequestReplay(ctx, replayReq("audit", 1, 6))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrReplayRangeExpired) {
			t.Fatalf("run over a lost range: %v", err)
		}
		if got, _ := f.store.GetReplay(ctx, tenantA, rec.ID); got.State == "completed" || got.NextSeq != 3 {
			t.Fatalf("a lost range was reported as completed: %+v", got)
		}
	})
}

func readAllSince(t *testing.T, f *fixture, after int64) []postgres.Observation {
	t.Helper()
	var out []postgres.Observation
	err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, _, err = f.store.ReadObservations(ctx, tx, scopeA, after, 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReplayRevalidation: a replay requested while the evidence exists and run
// after it would have expired neither repeats a completed effect nor runs when
// its admission-time safety no longer holds; a fence or a takeover stops a
// running replay before the next effect.
func TestReplayRevalidation(t *testing.T) {
	ctx := context.Background()
	newTwo := func(t *testing.T, mutate func(*postgres.Options)) *fixture {
		f := newFixture(t, mutate,
			effectBinding("ledger", events.EffectLocal, nil),
			effectBinding("notifier", events.EffectExternalIntent, nil))
		effectTable(f)
		return f
	}

	t.Run("request before expiry, prune, then run repeats nothing", func(t *testing.T) {
		f := newTwo(t, nil)
		appendN(f, "pin", 4)
		publishAll(t, f)
		deliverAll(t, f, "ledger", "notifier")
		recs := map[string]postgres.ReplayRecord{}
		for _, c := range []string{"ledger", "notifier"} {
			rec, err := f.store.RequestReplay(ctx, replayReq(c, 1, 4))
			if err != nil {
				t.Fatalf("request for %s: %v", c, err)
			}
			recs[c] = rec
		}
		// The deduplication and observation retention elapse while both requests wait.
		res, err := f.store.Prune(ctx, tenantA, f.clock.Now().Add(60*time.Hour))
		if err != nil || res.Inbox != 0 || res.Observations != 0 {
			t.Fatalf("an active replay's evidence and range were pruned: %+v %v", res, err)
		}
		f.clock.Advance(60 * time.Hour)
		for c, rec := range recs {
			done, err := f.store.RunReplay(ctx, tenantA, rec.ID)
			if err != nil || done.State != "completed" || done.Skipped != 4 || done.Processed != 0 {
				t.Fatalf("%s replay: %+v %v", c, done, err)
			}
			for _, o := range readAll(t, f) {
				if got := f.effects(c, o.Envelope.ID); got != 1 {
					t.Fatalf("%s repeated the effect of %s: %d", c, o.Envelope.ID, got)
				}
			}
		}
		// With the requests completed, the evidence and history expire normally.
		res, err = f.store.Prune(ctx, tenantA, f.clock.Now())
		if err != nil || res.Inbox != 8 || res.Observations != 4 {
			t.Fatalf("prune after the replays: %+v %v", res, err)
		}
	})

	t.Run("lost evidence fails closed for local and external-intent consumers", func(t *testing.T) {
		f := newTwo(t, nil)
		appendN(f, "gone", 3)
		deliverAll(t, f, "ledger", "notifier")
		// Admitted when the events are already older than the deduplication horizon
		// but their evidence still exists (cleanup has not run).
		f.clock.Advance(60 * time.Hour)
		recs := map[string]postgres.ReplayRecord{}
		for _, c := range []string{"ledger", "notifier"} {
			rec, err := f.store.RequestReplay(ctx, replayReq(c, 1, 3))
			if err != nil {
				t.Fatalf("request for %s: %v", c, err)
			}
			recs[c] = rec
		}
		// The evidence disappears by a path that bypasses retention.
		if n := f.count(tenantA, `WITH d AS (DELETE FROM event_inbox WHERE state = 'completed' RETURNING 1) SELECT count(*) FROM d`); n != 6 {
			t.Fatalf("setup deleted %d inbox entries", n)
		}
		for c, rec := range recs {
			if _, err := f.store.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrReplayBeyondHorizon) {
				t.Fatalf("%s: replay without evidence: %v", c, err)
			}
			for _, o := range readAll(t, f) {
				if got := f.effects(c, o.Envelope.ID); got != 1 {
					t.Fatalf("%s: effect of %s repeated or lost: %d", c, o.Envelope.ID, got)
				}
			}
			if got, _ := f.store.GetReplay(ctx, tenantA, rec.ID); got.State == "completed" {
				t.Fatalf("%s: an unsafe replay completed: %+v", c, got)
			}
		}
	})

	t.Run("a changed consumer classification fails closed", func(t *testing.T) {
		f := newTwo(t, nil)
		appendN(f, "cls", 2)
		deliverAll(t, f, "ledger", "notifier")
		recL, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 2))
		if err != nil {
			t.Fatal(err)
		}
		recN, err := f.store.RequestReplay(ctx, replayReq("notifier", 1, 2))
		if err != nil {
			t.Fatal(err)
		}
		// A new deployment reclassifies both consumers.
		restarted := f.env.NewStore(t, eventstestDefaults(f),
			effectBinding("ledger", events.EffectUnknown, nil),
			effectBinding("notifier", events.EffectLocal, nil))
		for _, rec := range []postgres.ReplayRecord{recL, recN} {
			if _, err := restarted.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrReplayUnsafe) {
				t.Fatalf("replay %s after reclassification: %v", rec.ID, err)
			}
		}
	})

	t.Run("a fence committing between events stops the run before the next effect", func(t *testing.T) {
		var f *fixture
		fenced := make(chan error, 1)
		var once sync.Once
		hook := func(ctx context.Context, env eventsv1.Envelope) {
			if env.ID != "fnc01" { // the second event
				return
			}
			once.Do(func() {
				go func() {
					fenced <- f.store.FenceConsumer(context.Background(), tenantA, "ledger", events.Operator{ID: "ops_1"}, "restored from backup")
				}()
				time.Sleep(400 * time.Millisecond) // the fence queues behind this event's transaction
			})
		}
		f = newFixture(t, func(o *postgres.Options) { o.ReplayBatch = 5 }, effectBinding("ledger", events.EffectLocal, hook))
		effectTable(f)
		appendN(f, "fnc", 5)
		deliverOnly := readAll(t, f)
		rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 5))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrReplayFenced) {
			t.Fatalf("run with a fence committing mid-run: %v", err)
		}
		if err := <-fenced; err != nil {
			t.Fatalf("fence: %v", err)
		}
		for i, o := range deliverOnly {
			want := 0
			if i < 2 {
				want = 1
			}
			if got := f.effects("ledger", o.Envelope.ID); got != want {
				t.Fatalf("event %d (%s): %d effect(s), want %d", i+1, o.Envelope.ID, got, want)
			}
		}
		if got, _ := f.store.GetReplay(ctx, tenantA, rec.ID); got.State != "running" || got.NextSeq != 3 || got.Processed != 2 {
			t.Fatalf("progress after the fence: %+v", got)
		}
		// After the owner clears the fence the request resumes and finishes once.
		if err := f.store.ClearFence(ctx, tenantA, "ledger", events.Operator{ID: "ops_1"}, "reconciled"); err != nil {
			t.Fatal(err)
		}
		done, err := f.store.RunReplay(ctx, tenantA, rec.ID)
		if err != nil || done.State != "completed" || done.Processed != 5 {
			t.Fatalf("resume after clearing: %+v %v", done, err)
		}
		for _, o := range deliverOnly {
			if got := f.effects("ledger", o.Envelope.ID); got != 1 {
				t.Fatalf("%s has %d effect(s)", o.Envelope.ID, got)
			}
		}
	})

	t.Run("a takeover stops the superseded runner before its next effect", func(t *testing.T) {
		var f *fixture
		var storeB *postgres.Store
		var recID atomic.Value
		resB := make(chan error, 1)
		var once sync.Once
		var invokedMu sync.Mutex
		var invoked []string
		hook := func(ctx context.Context, env eventsv1.Envelope) {
			invokedMu.Lock()
			invoked = append(invoked, env.ID)
			invokedMu.Unlock()
			if env.ID != "tko00" { // the first event
				return
			}
			once.Do(func() {
				go func() {
					f.clock.Advance(2 * time.Minute) // runner A's lease is now expired
					_, err := storeB.RunReplay(context.Background(), tenantA, recID.Load().(string))
					resB <- err
				}()
				time.Sleep(400 * time.Millisecond) // B queues behind A's transaction
			})
		}
		f = newFixture(t, func(o *postgres.Options) { o.ReplayBatch = 5 }, effectBinding("ledger", events.EffectLocal, hook))
		effectTable(f)
		storeB = f.env.NewStore(t, eventstestDefaults(f), effectBinding("ledger", events.EffectLocal, nil))
		appendN(f, "tko", 4)
		obs := readAll(t, f)
		rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 4))
		if err != nil {
			t.Fatal(err)
		}
		recID.Store(rec.ID)
		if _, err := f.store.RunReplay(ctx, tenantA, rec.ID); !errors.Is(err, events.ErrStaleClaim) {
			t.Fatalf("the superseded runner: %v", err)
		}
		if err := <-resB; err != nil {
			t.Fatalf("the new runner: %v", err)
		}
		invokedMu.Lock()
		if len(invoked) != 1 || invoked[0] != "tko00" {
			t.Fatalf("the superseded runner invoked the handler for %v; it must stop before any further effect", invoked)
		}
		invokedMu.Unlock()
		got, err := f.store.GetReplay(ctx, tenantA, rec.ID)
		if err != nil || got.State != "completed" || got.Processed != 4 || got.Skipped != 0 {
			t.Fatalf("final state: %+v %v", got, err)
		}
		for _, o := range obs {
			if n := f.effects("ledger", o.Envelope.ID); n != 1 {
				t.Fatalf("%s has %d effect(s)", o.Envelope.ID, n)
			}
		}
	})
}
