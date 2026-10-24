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

// TestStaleReappendCannotRepeatPrunedEffect: an event is appended, published,
// consumed, and every record of it (outbox row, observation, inbox entry) is
// pruned. A producer retry of the identical envelope must not be given a fresh
// origin: a new unpublished outbox row would make its delivery admissible and
// repeat the effect. The durable boundary refuses the append, and a stray copy
// of the event is refused where it would act. Real PostgreSQL.
func TestStaleReappendCannotRepeatPrunedEffect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, ledgerBinding())
	effectTable(f)
	env := f.fresh(eventA("sr_evt", 1))
	f.append(env)
	markPublished(t, f)
	if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
		t.Fatalf("first delivery: %+v", out)
	}
	// Before pruning the retained records still recognize the retry.
	if res := f.append(env); !res.Duplicate {
		t.Fatalf("retry before pruning: %+v", res)
	}

	f.clock.Advance(60 * time.Hour)
	pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Inbox != 1 || pr.Outbox != 1 || pr.Observations != 1 {
		t.Fatalf("prune: %+v %v (all evidence must be gone)", pr, err)
	}
	for _, table := range []string{"outbox", "event_observations", "event_inbox"} {
		if n := f.count(tenantA, `SELECT count(*) FROM `+table); n != 0 {
			t.Fatalf("%s still holds %d row(s)", table, n)
		}
	}

	// The retry is refused and leaves nothing behind.
	err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.store.Append(ctx, tx, env)
		return err
	})
	if !errors.Is(err, events.ErrStaleAppend) {
		t.Fatalf("stale re-append: %v", err)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM outbox`) + f.count(tenantA, `SELECT count(*) FROM event_observations`); n != 0 {
		t.Fatalf("a refused append left %d row(s)", n)
	}
	// Nothing can be published, and a copy that still reaches the consumer is
	// refused: the effect stays single.
	if claims, err := f.store.ClaimDue(ctx, tenantA, f.clock.Now(), time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("claims after the refused append: %v %v", claims, err)
	}
	if out := deliver(t, f, env); out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon {
		t.Fatalf("stray copy: %+v", out)
	}
	if n := f.effects("ledger", "sr_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}

	// Events produced after the boundary are admitted and delivered normally.
	next := f.fresh(eventA("sr_next", 2))
	if res := f.append(next); res.Duplicate || res.Sequence == 0 {
		t.Fatalf("fresh append: %+v", res)
	}
	if out := deliver(t, f, next); out.Kind != events.OutcomeCompleted {
		t.Fatalf("fresh delivery: %+v", out)
	}
	if n := f.effects("ledger", "sr_next"); n != 1 {
		t.Fatalf("fresh effects = %d", n)
	}

	// A larger retention after a restart does not lower the boundary.
	opts := eventstestDefaults(f)
	opts.Retention.Inbox, opts.Retention.Observation = 96*time.Hour, 96*time.Hour
	restarted, err := postgres.NewStore(f.env.Runtime, opts, ledgerBinding())
	if err != nil {
		t.Fatal(err)
	}
	err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := restarted.Append(ctx, tx, env)
		return err
	})
	if !errors.Is(err, events.ErrStaleAppend) {
		t.Fatalf("re-append after a restart with longer retention: %v", err)
	}
}

// TestAppendAdmissionSerializesWithBoundary: an append holds the tenant's
// admission lock (shared) until its transaction ends, so inbox pruning, which
// advances the durable boundary, cannot slip between the append's boundary
// verdict and its commit. Real PostgreSQL.
func TestAppendAdmissionSerializesWithBoundary(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, ledgerBinding())
	effectTable(f)
	old := f.fresh(eventA("ar_old", 1))
	f.append(old)
	markPublished(t, f)
	if out := deliver(t, f, old); out.Kind != events.OutcomeCompleted {
		t.Fatalf("delivery: %+v", out)
	}
	f.clock.Advance(60 * time.Hour)
	now := f.clock.Now()

	appended, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause()
	txDone := make(chan error, 1)
	go func() {
		txDone <- f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := f.store.Append(ctx, tx, f.fresh(eventA("ar_new", 2))); err != nil {
				return err
			}
			close(appended)
			<-release
			return nil
		})
	}()
	<-appended

	type result struct {
		pr  postgres.PruneResult
		err error
	}
	pruned := make(chan result, 1)
	go func() {
		pr, err := f.store.Prune(ctx, tenantA, now)
		pruned <- result{pr, err}
	}()
	select {
	case r := <-pruned:
		t.Fatalf("pruning completed while an append was in flight: %+v %v", r.pr, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	// The inbox step in particular must be waiting: the boundary it records is
	// still unwritten. (Observation pruning waits on the scope row regardless.)
	if n := f.count(tenantA, `SELECT count(*) FROM event_dedup_boundary`); n != 0 {
		t.Fatalf("the deduplication boundary advanced under an in-flight append")
	}
	unpause()
	if err := <-txDone; err != nil {
		t.Fatalf("append transaction: %v", err)
	}
	if r := <-pruned; r.err != nil || r.pr.Inbox != 1 {
		t.Fatalf("prune after the append committed: %+v %v", r.pr, r.err)
	}
	// The event appended before the boundary moved is still deliverable.
	if n := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = 'ar_new'`); n != 1 {
		t.Fatalf("outbox rows for the appended event = %d", n)
	}
}

// TestFenceSerializesWithReplayAdmission: a fence that has taken its place in
// the admission order cannot be bypassed by a request admitted concurrently.
// The fencing transaction is paused after it began; a new request must wait for
// it (not be admitted without seeing the fence), and is refused once the fence
// commits. Fencing then returns only after no admitted request can run an
// effect unfenced. Real PostgreSQL.
func TestFenceSerializesWithReplayAdmission(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, ledgerBinding())
	effectTable(f)
	appendN(f, "fa", 2)

	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause() // a failing assertion must not leave the transaction open
	var armed, calls atomic.Int32
	opts := eventstestDefaults(f)
	opts.Clock = func() time.Time {
		// The fencing store's only reading is taken inside its transaction.
		if armed.Load() == 1 && calls.Add(1) == 1 {
			close(reached)
			<-release
		}
		return f.clock.Now()
	}
	fencer, err := postgres.NewStore(f.env.Runtime, opts, ledgerBinding())
	if err != nil {
		t.Fatal(err)
	}

	armed.Store(1)
	fenced := make(chan error, 1)
	go func() {
		fenced <- fencer.FenceConsumer(ctx, tenantA, "ledger", events.Operator{ID: "ops_1"}, "restored from backup")
	}()
	<-reached

	type result struct {
		rec postgres.ReplayRecord
		err error
	}
	requested := make(chan result, 1)
	go func() {
		rec, err := f.store.RequestReplay(ctx, replayReq("ledger", 1, 2))
		requested <- result{rec, err}
	}()
	select {
	case r := <-requested:
		t.Fatalf("a replay was admitted while a fence was being recorded: %+v %v", r.rec, r.err)
	case <-time.After(500 * time.Millisecond):
	}

	unpause()
	if err := <-fenced; err != nil {
		t.Fatalf("fence: %v", err)
	}
	if r := <-requested; !errors.Is(r.err, events.ErrReplayFenced) {
		t.Fatalf("request after the fence committed: %+v %v", r.rec, r.err)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM event_replay_requests`); n != 0 {
		t.Fatalf("%d request(s) admitted", n)
	}
	for _, o := range readAll(t, f) {
		if n := f.effects("ledger", o.Envelope.ID); n != 0 {
			t.Fatalf("%s has %d effect(s)", o.Envelope.ID, n)
		}
	}
}

// TestStaleReappendHoldsUnderProducerClockSkew: a producer whose clock runs
// ahead stamps an event slightly after the moment the consumer completes it. The
// retention clock of the evidence must still not start before the event's own
// occurrence, so a prune whose cutoff lies between completion and occurrence
// does not forget it; and once everything is forgotten, the identical retry is
// refused. Real PostgreSQL.
func TestStaleReappendHoldsUnderProducerClockSkew(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, ledgerBinding())
	effectTable(f)
	// Occurs two seconds after the process clock: within MaxClockSkew, but later
	// than the completion time a naive consumer would stamp.
	env := eventA("sk_evt", 1)
	env.OccurredAt = f.clock.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	f.append(env)
	markPublished(t, f)
	if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
		t.Fatalf("first delivery: %+v", out)
	}

	// The inbox cutoff of this prune falls about one second after the event was
	// processed and one second before it "occurred".
	f.clock.Advance(48*time.Hour + time.Second)
	pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pr.Inbox != 0 {
		t.Fatalf("the evidence of an event that has not occurred yet by the cutoff was forgotten: %+v", pr)
	}
	if pr.Outbox != 1 || pr.Observations != 1 {
		t.Fatalf("the other records must be pruned for this scenario to be meaningful: %+v", pr)
	}
	// The retry is recognized by the retained inbox entry: no new origin is
	// created, and a redelivery is absorbed.
	if res := f.append(env); !res.Duplicate {
		t.Fatalf("retry while the evidence is retained: %+v", res)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM outbox`) + f.count(tenantA, `SELECT count(*) FROM event_observations`); n != 0 {
		t.Fatalf("the retry re-originated the event: %d row(s)", n)
	}
	if out := deliver(t, f, env); out.Kind != events.OutcomeDuplicate {
		t.Fatalf("redelivery: %+v", out)
	}
	if n := f.effects("ledger", "sk_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}

	// Everything is forgotten eventually: the boundary then covers the
	// occurrence, and the retry is refused.
	f.clock.Advance(60 * time.Hour)
	pr, err = f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Inbox != 1 {
		t.Fatalf("final prune: %+v %v", pr, err)
	}
	err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.store.Append(ctx, tx, env)
		return err
	})
	if !errors.Is(err, events.ErrStaleAppend) {
		t.Fatalf("re-append after everything was forgotten: %v", err)
	}
	if n := f.effects("ledger", "sk_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
}

// TestReappendAfterPartialPruneKeepsOneOrigin: the retention lifetimes are
// independent, so the inbox entry (deduplication evidence) can outlive both the
// outbox row and the observation of its event. An identical re-append in that
// window must be recognized by the surviving inbox entry instead of recreating
// the outbox row and observation with a fresh origin: once the entry is pruned
// later, a recreated observation would be the only record of the event, and its
// fresh recorded_at would make repair or delivery treat the event as
// unprocessed and repeat the completed effect. Real PostgreSQL.
func TestReappendAfterPartialPruneKeepsOneOrigin(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(o *postgres.Options) {
		o.Retention.Inbox = 10 * time.Hour
		o.Retention.Observation = 2 * time.Hour
		o.Retention.PublishedOutbox = time.Hour
		o.Retention.BrokerRedelivery = time.Hour
		o.Retention.ReplayWindow = time.Hour
	}, ledgerBinding())
	effectTable(f)
	env := f.fresh(eventA("pp_evt", 1))
	f.append(env)
	markPublished(t, f)
	if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
		t.Fatalf("first delivery: %+v", out)
	}

	// Partial retention: outbox row and observation are forgotten, the inbox
	// entry is not.
	f.clock.Advance(9 * time.Hour)
	pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Outbox != 1 || pr.Observations != 1 || pr.Inbox != 0 {
		t.Fatalf("partial prune: %+v %v", pr, err)
	}

	// The identical retry is the same event: nothing is recreated.
	if res := f.append(env); !res.Duplicate || res.Sequence != 0 {
		t.Fatalf("identical re-append: %+v", res)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM outbox`) + f.count(tenantA, `SELECT count(*) FROM event_observations`); n != 0 {
		t.Fatalf("the re-append recreated %d row(s)", n)
	}
	// A different envelope under the same ID is still a conflict.
	changed := env
	changed.Resource.Version++
	if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.store.Append(ctx, tx, changed)
		return err
	}); !errors.Is(err, events.ErrConflict) {
		t.Fatalf("conflicting re-append: %v", err)
	}
	markPublished(t, f) // nothing to publish: the retry created no publication intent

	// The surviving evidence expires; every path stays single-effect.
	f.clock.Advance(time.Hour + time.Minute)
	pr, err = f.store.Prune(ctx, tenantA, f.clock.Now())
	if err != nil || pr.Inbox != 1 || pr.Observations != 0 || pr.Outbox != 0 {
		t.Fatalf("final prune: %+v %v", pr, err)
	}
	if res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 0 || res.Retried != 0 {
		t.Fatalf("repair: %+v %v", res, err)
	}
	if out := deliver(t, f, env); out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon {
		t.Fatalf("live delivery of a stray copy: %+v", out)
	}
	err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.store.Append(ctx, tx, env)
		return err
	})
	if !errors.Is(err, events.ErrStaleAppend) {
		t.Fatalf("re-append after the evidence expired: %v", err)
	}
	if n := f.effects("ledger", "pp_evt"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
}

// TestAppendRefusesEventsBeyondClockSkew: the producer clock's lead is bounded
// by the configured MaxClockSkew, so an event cannot hold its own deduplication
// evidence indefinitely. Real PostgreSQL.
func TestAppendRefusesEventsBeyondClockSkew(t *testing.T) {
	f := newFixture(t, nil)
	late := eventA("fut_late", 1)
	late.OccurredAt = f.clock.Now().Add(10 * time.Minute)
	err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.store.Append(ctx, tx, late)
		return err
	})
	if !errors.Is(err, events.ErrFutureEvent) {
		t.Fatalf("append beyond the skew bound: %v", err)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM outbox`) + f.count(tenantA, `SELECT count(*) FROM event_observations`); n != 0 {
		t.Fatalf("a refused append left %d row(s)", n)
	}
	ok := eventA("fut_ok", 2)
	ok.OccurredAt = f.clock.Now().Add(10 * time.Second)
	f.append(ok)
}

// TestHandlerAppendDoesNotDeadlockWithFence: a replayed event's local effect
// appends a downstream event in the replay step's transaction while an operator
// fences the consumer. Every processing transaction takes the tenant admission
// lock before the replay request row, as fencing does, so the handler's append
// and the fence finish in sequence: no deadlock victim, no failed delivery, no
// quarantine. The step is paused inside the handler, before its append, while
// the fence is already waiting. Real PostgreSQL.
func TestHandlerAppendDoesNotDeadlockWithFence(t *testing.T) {
	ctx := context.Background()
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause()
	var store *postgres.Store
	chain := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "chain", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			if env.ID != "hd_1" {
				return nil
			}
			close(reached)
			<-release
			_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'chain', $2)`, string(env.TenantID), env.ID)
			if err != nil {
				return err
			}
			_, err = store.Append(ctx, tx, eventA("hd_down", 50))
			return err
		},
	}
	f := newFixture(t, nil, chain)
	store = f.store
	effectTable(f)
	f.append(eventA("hd_1", 1))
	rec, err := f.store.RequestReplay(ctx, replayReq("chain", 1, 1))
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	type replayResult struct {
		rec postgres.ReplayRecord
		err error
	}
	ran := make(chan replayResult, 1)
	go func() {
		r, err := f.store.RunReplay(ctx, tenantA, rec.ID)
		ran <- replayResult{r, err}
	}()
	select {
	case <-reached:
	case rr := <-ran:
		t.Fatalf("the replay finished without reaching the handler: %+v %v", rr.rec, rr.err)
	}

	fenced := make(chan error, 1)
	go func() {
		fenced <- f.store.FenceConsumer(ctx, tenantA, "chain", events.Operator{ID: "ops_1"}, "restored from backup")
	}()
	select {
	case err := <-fenced:
		t.Fatalf("the fence returned while a replayed event was in flight: %v", err)
	case <-time.After(1500 * time.Millisecond): // longer than PostgreSQL's deadlock check
	}
	unpause()

	var rr replayResult
	select {
	case rr = <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("replay did not finish")
	}
	if rr.err != nil || rr.rec.State != "completed" || rr.rec.Processed != 1 || rr.rec.Failed != 0 {
		t.Fatalf("replay: %+v %v", rr.rec, rr.err)
	}
	select {
	case err := <-fenced:
		if err != nil {
			t.Fatalf("fence: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fence did not finish")
	}
	if n := f.effects("chain", "hd_1"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = 'hd_down'`); n != 1 {
		t.Fatalf("downstream outbox rows = %d, want 1", n)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM event_quarantine`); n != 0 {
		t.Fatalf("%d quarantined delivery(ies)", n)
	}
	if n := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE state = 'completed' AND attempts <= 1`); n != 1 {
		t.Fatalf("inbox entries completed on the first attempt = %d", n)
	}
	// The consumer is fenced afterwards: a new request is refused.
	if _, err := f.store.RequestReplay(ctx, replayReq("chain", 1, 1)); !errors.Is(err, events.ErrReplayFenced) {
		t.Fatalf("request after the fence: %v", err)
	}
}
