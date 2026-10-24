package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// verifyingProcessor asserts the ordering the transport relies on: a delivery
// reported as quarantined is already durable in PostgreSQL when Process returns,
// i.e. before the transport acknowledges.
type verifyingProcessor struct {
	t     *testing.T
	b     *bus
	inner events.Processor
	seen  atomic.Int64
}

func (p *verifyingProcessor) Process(ctx context.Context, d events.Delivery) (events.Outcome, error) {
	out, err := p.inner.Process(ctx, d)
	if err == nil && out.Kind == events.OutcomeQuarantined && out.QuarantineID != "" {
		p.seen.Add(1)
		if got := p.b.count(`SELECT count(*) FROM event_quarantine WHERE id = $1 AND state = 'quarantined'`, out.QuarantineID); got != 1 {
			p.t.Errorf("quarantine %s was reported before it was durable", out.QuarantineID)
		}
	}
	return out, err
}

func (b *bus) auditRows(action string) []map[string]any {
	b.t.Helper()
	var out []map[string]any
	err := storage.WithTenant(context.Background(), b.env.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT actor_type, actor_id, resource_type, resource_id, COALESCE(reason, ''), metadata FROM audit WHERE action = $1 ORDER BY seq`, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var at, ai, rt, ri, reason string
			var meta []byte
			if err := rows.Scan(&at, &ai, &rt, &ri, &reason, &meta); err != nil {
				return err
			}
			m := map[string]any{}
			_ = json.Unmarshal(meta, &m)
			out = append(out, map[string]any{"actor_type": at, "actor_id": ai, "resource_type": rt, "resource_id": ri, "reason": reason, "metadata": m})
		}
		return rows.Err()
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// Test_quarantine_replay: poison and unsupported-version deliveries are durably
// quarantined before acknowledgment; a scoped dry-run and an audited, targeted
// replay survive a restart, refuse unsafe requests and never repeat completed
// effects. Real PostgreSQL and NATS. PX-05, AC-08.
func Test_quarantine_replay(t *testing.T) {
	var fixed atomic.Bool
	var interrupt atomic.Pointer[context.CancelFunc]
	handler := func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		if strings.HasPrefix(env.ID, "evt_poison") && !fixed.Load() {
			return events.Permanent(errors.New("handler cannot apply this event yet"))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id) VALUES ($1, 'ledger', $2)`, string(env.TenantID), env.ID); err != nil {
			return err
		}
		if env.ID == "evt_poison2" {
			if c := interrupt.Load(); c != nil {
				(*c)() // the replay runner dies while handling this event
			}
		}
		return nil
	}
	ledgerBinding := postgres.Binding{Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal}, Handle: handler}
	mystery := postgres.Binding{Spec: events.ConsumerSpec{Name: "mystery", Mode: events.ModeHistorical, Effect: events.EffectUnknown}, Handle: handler}
	bindings := []postgres.Binding{ledgerBinding, mystery}
	b := newBus(t, busOptions{bindings: bindings})

	envs := []eventsv1.Envelope{
		b.append2(workItemEvent("evt_ok1", "wi_1", 1)),
		b.append2(workItemEvent("evt_poison1", "wi_2", 1)),
		b.append2(workItemEvent("evt_ok2", "wi_3", 1)),
		b.append2(workItemEvent("evt_poison2", "wi_4", 1)),
	}
	_ = envs

	// Real delivery path, plus a message from a newer contract and one that is
	// not an event at all.
	pub := b.publisher(b.jsPublisher(b.broker.Addr()), publisherOptions{})
	b.drainPublisher(pub, 10*time.Second)
	token, err := b.store.SubjectToken(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	future := workItemEvent("evt_future", "wi_5", 1)
	futureJSON, _ := json.Marshal(future)
	rawPublish(t, b, token, future.Type, []byte(strings.Replace(string(futureJSON), `"schemaVersion":1`, `"schemaVersion":2`, 1)))
	rawPublish(t, b, token, future.Type, []byte(`{"id":"evt_garbage","type":`))

	verify := &verifyingProcessor{t: t, b: b, inner: b.store}
	consumer := b.consumer(b.broker.Addr(), verify, "ledger")
	pump(t, consumer, 20*time.Second, "all six deliveries to be settled", func() bool { return consumer.Counters().Acked >= 6 })

	if verify.seen.Load() != 4 {
		t.Fatalf("quarantined deliveries observed = %d, want 4", verify.seen.Load())
	}
	for _, id := range []string{"evt_ok1", "evt_ok2"} {
		if b.effects("ledger", id) != 1 {
			t.Fatalf("%s must have one effect", id)
		}
	}
	for _, id := range []string{"evt_poison1", "evt_poison2"} {
		if b.effects("ledger", id) != 0 {
			t.Fatalf("poison %s left an effect", id)
		}
	}
	entries, err := b.store.ListQuarantine(context.Background(), tenantA, "ledger", 100)
	if err != nil || len(entries) != 4 {
		t.Fatalf("quarantine entries = %d, %v", len(entries), err)
	}
	reasons := map[string]int{}
	for _, e := range entries {
		reasons[e.Reason]++
		if e.Stream == "" || e.StreamSeq == 0 || e.Subject == "" || e.DeliveryCount < 1 || len(e.Diagnostic) > events.MaxFailureMessage {
			t.Errorf("quarantine entry lacks bounded diagnostics or original delivery identity: %+v", e)
		}
	}
	if reasons[postgres.ReasonPermanentFailure] != 2 || reasons[postgres.ReasonUnsupportedVersion] != 1 || reasons[postgres.ReasonMalformed] != 1 {
		t.Fatalf("quarantine reasons: %v", reasons)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox WHERE state = 'quarantined' AND consumer = 'ledger'`); got != 2 {
		t.Fatalf("quarantined inbox entries = %d", got)
	}

	// Unauthorized, unsafe and out-of-range replays are refused and change nothing.
	scope := events.Scope{TenantID: tenantA, ProjectID: projectA}
	request := func(mutate func(*events.ReplayRequest)) events.ReplayRequest {
		r := events.ReplayRequest{Scope: scope, Consumer: "ledger", FromSeq: 1, ToSeq: 4, Reason: "handler fixed", Operator: events.Operator{ID: "ops_1"}}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}
	ctx := context.Background()
	if _, err := b.store.DryRun(ctx, request(func(r *events.ReplayRequest) { r.Operator.ID = "mallory" })); !errors.Is(err, events.ErrUnauthorized) {
		t.Fatalf("unauthorized dry-run: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, request(func(r *events.ReplayRequest) { r.Operator.ID = "mallory" })); !errors.Is(err, events.ErrUnauthorized) {
		t.Fatalf("unauthorized replay: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, request(func(r *events.ReplayRequest) { r.ToSeq = 99 })); !errors.Is(err, events.ErrReplayUnsafe) {
		t.Fatalf("range beyond the high water: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, request(func(r *events.ReplayRequest) { r.Consumer = "mystery" })); !errors.Is(err, events.ErrReplayUnsafe) {
		t.Fatalf("unclassified effects: %v", err)
	}
	if _, err := b.store.RequestReplay(ctx, request(func(r *events.ReplayRequest) { r.Reason = "" })); err == nil {
		t.Fatal("a replay without a reason was accepted")
	}
	if got := b.count(`SELECT count(*) FROM event_replay_requests`); got != 0 {
		t.Fatalf("refused requests persisted %d record(s)", got)
	}

	// Scoped dry-run: classification only, nothing changes.
	fixed.Store(true)
	c, err := b.store.DryRun(ctx, request(nil))
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if c.Events != 4 || c.AlreadyCompleted != 2 || c.Quarantined != 2 || c.Undelivered != 0 || c.Effect != events.EffectLocal {
		t.Fatalf("classification: %+v", c)
	}
	if b.effects("ledger", "evt_poison1") != 0 {
		t.Fatal("the dry-run changed state")
	}

	// Audited, durable request.
	rec, err := b.store.RequestReplay(ctx, request(nil))
	if err != nil {
		t.Fatal(err)
	}
	audit := b.auditRows("event.replay_requested")
	if len(audit) != 1 || audit[0]["actor_type"] != "operator" || audit[0]["actor_id"] != "ops_1" || audit[0]["resource_id"] != rec.ID || audit[0]["reason"] != "handler fixed" {
		t.Fatalf("replay audit: %v", audit)
	}
	// Overlapping active replays are refused.
	if _, err := b.store.RequestReplay(ctx, request(func(r *events.ReplayRequest) { r.FromSeq = 3 })); !errors.Is(err, events.ErrReplayUnsafe) {
		t.Fatalf("overlapping replay: %v", err)
	}

	// The runner dies mid-replay; a restarted process (new Store, short lease,
	// one event per step) resumes after the lease and finishes.
	restartOpts := func(o *postgres.Options) { o.ReplayBatch = 1; o.ReplayLease = 400 * time.Millisecond }
	opts := eventstest.DefaultOptions()
	restartOpts(&opts)
	opts.Authorizer = eventstest.Operators{"ops_1"}
	first := b.env.NewStore(t, opts, bindings...)
	runCtx, cancel := context.WithCancel(ctx)
	interrupt.Store(&cancel)
	if _, err := first.RunReplay(runCtx, tenantA, rec.ID); err == nil {
		t.Fatal("the interrupted replay reported success")
	}
	interrupt.Store(nil)
	partial, err := b.store.GetReplay(ctx, tenantA, rec.ID)
	if err != nil || partial.State != "running" || partial.NextSeq != 4 || partial.Processed != 1 || partial.Skipped != 2 {
		t.Fatalf("progress after the crash: %+v %v", partial, err)
	}
	second := b.env.NewStore(t, opts, bindings...)
	if n, err := second.ResumeReplays(ctx, tenantA); err != nil || n != 0 {
		t.Fatalf("a live lease must not be taken over: resumed=%d err=%v", n, err)
	}
	time.Sleep(600 * time.Millisecond)
	if n, err := second.ResumeReplays(ctx, tenantA); err != nil || n != 1 {
		t.Fatalf("resume: %d %v", n, err)
	}
	done, err := b.store.GetReplay(ctx, tenantA, rec.ID)
	if err != nil || done.State != "completed" || done.Processed != 2 || done.Skipped != 2 || done.Failed != 0 {
		t.Fatalf("final state: %+v %v", done, err)
	}
	// Exactly-once effects: completed events were skipped, quarantined ones ran once.
	for _, id := range []string{"evt_ok1", "evt_ok2", "evt_poison1", "evt_poison2"} {
		if got := b.effects("ledger", id); got != 1 {
			t.Errorf("%s has %d effect(s) after the replay, want exactly 1", id, got)
		}
	}
	if got := b.count(`SELECT count(*) FROM event_quarantine WHERE consumer = 'ledger' AND resolution = 'replayed' AND resolved_by = 'ops_1'`); got != 2 {
		t.Fatalf("replayed quarantine records = %d", got)
	}
	if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE outcome = 'replay_reopened' AND replay_id = $1`, rec.ID); got != 2 {
		t.Fatalf("replay history rows = %d", got)
	}
	if got := b.auditRows("event.replay_completed"); len(got) != 1 {
		t.Fatalf("completion audit: %v", got)
	}
	// Other consumers were not touched by the targeted replay.
	if got := b.count(`SELECT count(*) FROM event_inbox WHERE consumer <> 'ledger'`); got != 0 {
		t.Fatalf("replay reached another consumer: %d inbox rows", got)
	}
	// The remaining quarantine entries (not replayable events) are discarded by an
	// authorized operator, audited; an unauthorized one cannot.
	left, _ := b.store.ListQuarantine(ctx, tenantA, "ledger", 10)
	if len(left) != 2 {
		t.Fatalf("remaining quarantine entries = %d", len(left))
	}
	if err := b.store.DiscardQuarantine(ctx, tenantA, left[0].ID, events.Operator{ID: "mallory"}, "cleanup"); !errors.Is(err, events.ErrUnauthorized) {
		t.Fatalf("unauthorized discard: %v", err)
	}
	if err := b.store.DiscardQuarantine(ctx, tenantA, left[0].ID, events.Operator{ID: "ops_1"}, "unsupported future event"); err != nil {
		t.Fatal(err)
	}
	if got := b.auditRows("event.quarantine_discarded"); len(got) != 1 {
		t.Fatalf("discard audit: %v", got)
	}
	if after, _ := b.store.ListQuarantine(ctx, tenantA, "ledger", 10); len(after) != 1 {
		t.Fatalf("entries after discard = %d", len(after))
	}
}
