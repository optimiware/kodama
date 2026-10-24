package eventstest_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
)

// TestUnitMemoryEventsContract runs the shared events contract on the memory
// adapter: no database, no broker.
func TestUnitMemoryEventsContract(t *testing.T) {
	eventstest.Contract(t, eventstest.OpenMem)
}

// outboxRig is the whole outbox -> broker -> inbox flow in memory.
type outboxRig struct {
	t       *testing.T
	clock   *eventstest.Clock
	store   *memdb.Store
	mem     *eventstest.Mem
	broker  *eventstest.InProcessBroker
	pub     *events.Publisher
	effects atomic.Int64
	fault   *completeFault
}

// completeFault fails Complete after the broker acknowledged, once, as a crash
// between acknowledgment and the mark would.
type completeFault struct {
	events.PublicationStore
	failNext atomic.Bool
	failed   atomic.Int64
}

func (f *completeFault) Complete(ctx context.Context, c events.Claim, now time.Time) error {
	if f.failNext.CompareAndSwap(true, false) {
		f.failed.Add(1)
		return errors.New("lost the acknowledgment bookkeeping")
	}
	return f.PublicationStore.Complete(ctx, c, now)
}

func newOutboxRig(t *testing.T, opts ...memdb.Option) *outboxRig {
	t.Helper()
	r := &outboxRig{t: t, clock: &eventstest.Clock{}}
	r.store = memdb.New(opts...)
	mo := eventstest.DefaultMemOptions()
	mo.Clock = r.clock.Now
	var err error
	r.mem, err = eventstest.NewMem(r.store, mo, eventstest.MemBinding{
		Spec: events.ConsumerSpec{Name: eventstest.ConsumerName, Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(_ context.Context, tx memdb.Tx, env eventsv1.Envelope) error {
			r.effects.Add(1)
			return eventstest.Note(tx, "ledger:"+env.ID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.mem.AddTenant(eventstest.TenantA)
	if r.broker, err = eventstest.NewInProcessBroker(r.mem); err != nil {
		t.Fatal(err)
	}
	r.broker.Subscribe(eventstest.ConsumerName, r.mem)
	r.fault = &completeFault{PublicationStore: r.mem}
	r.pub, err = events.NewPublisher(r.mem, r.fault, r.broker, events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: time.Second, BatchSize: 10, Interval: time.Second,
		Retry: eventstest.DefaultMemOptions().Retry, Clock: r.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *outboxRig) write(id string, version int64, fail error) error {
	return r.store.Update(context.Background(), eventstest.TenantA, func(tx memdb.Tx) error {
		if err := eventstest.Note(tx, "state:"+id); err != nil {
			return err
		}
		env := eventstest.WorkItemTransition(string(eventstest.TenantA), string(eventstest.ProjectA), id, "wi_"+id, version)
		if _, err := r.mem.Append(tx, env); err != nil {
			return err
		}
		return fail
	})
}

func (r *outboxRig) run() events.PublishResult {
	r.t.Helper()
	res, err := r.pub.RunOnce(context.Background())
	if err != nil {
		r.t.Fatalf("publish pass: %v", err)
	}
	return res
}

func (r *outboxRig) pump() int {
	r.t.Helper()
	n, err := r.broker.Pump(context.Background())
	if err != nil {
		r.t.Fatalf("pump: %v", err)
	}
	return n
}

// TestUnitMemoryEventsOutbox proves an event written in one Update is
// published once, deduplicated on redelivery and not published when the update
// rolls back (SD-A11, SD-A12).
func TestUnitMemoryEventsOutbox(t *testing.T) {
	t.Run("an event written in one update is published once", func(t *testing.T) {
		r := newOutboxRig(t)
		if err := r.write("evt_o1", 1, nil); err != nil {
			t.Fatal(err)
		}
		if res := r.run(); res.Claimed != 1 || res.Published != 1 {
			t.Fatalf("publish pass: %+v", res)
		}
		if res := r.run(); res.Claimed != 0 {
			t.Fatalf("a published event was claimed again: %+v", res)
		}
		if n := r.pump(); n != 1 {
			t.Fatalf("delivered %d", n)
		}
		if c := r.broker.Counters(); c.Published != 1 || c.Suppressed != 0 || c.Acked != 1 {
			t.Fatalf("broker: %+v", c)
		}
		if got := r.effects.Load(); got != 1 {
			t.Fatalf("effect ran %d times", got)
		}
	})

	t.Run("redelivery is deduplicated by the broker and by the inbox", func(t *testing.T) {
		r := newOutboxRig(t)
		if err := r.write("evt_o2", 1, nil); err != nil {
			t.Fatal(err)
		}
		// The broker acknowledged but the publisher could not record it: the
		// claim expires and the row is published again under the same event ID.
		r.fault.failNext.Store(true)
		pass, err := r.pub.RunOnce(context.Background())
		if err == nil || pass.Published != 0 || r.fault.failed.Load() != 1 {
			t.Fatalf("pass with a lost mark: %+v %v", pass, err)
		}
		r.clock.Advance(2 * time.Minute)
		if res := r.run(); res.Published != 1 {
			t.Fatalf("republication: %+v", res)
		}
		if c := r.broker.Counters(); c.Published != 1 || c.Suppressed != 1 {
			t.Fatalf("the broker did not suppress the repeated message ID: %+v", c)
		}
		if n := r.pump(); n != 1 || r.effects.Load() != 1 {
			t.Fatalf("delivered %d, effects %d", n, r.effects.Load())
		}
		// A copy the broker itself redelivers is absorbed by the inbox.
		if !r.broker.Redeliver("evt_o2") {
			t.Fatal("unknown message ID")
		}
		r.broker.Redeliver("evt_o2")
		if n := r.pump(); n != 2 {
			t.Fatalf("redelivered %d", n)
		}
		if got := r.effects.Load(); got != 1 {
			t.Fatalf("a redelivered event ran its effect %d times", got)
		}
		notes, _ := r.mem.Notes(context.Background(), eventstest.TenantA)
		if len(notes) != 2 { // the producer's state and the one effect
			t.Fatalf("notes: %v", notes)
		}
	})

	t.Run("a rolled-back update publishes nothing", func(t *testing.T) {
		r := newOutboxRig(t)
		boom := errors.New("producer failed")
		if err := r.write("evt_o3", 1, boom); !errors.Is(err, boom) {
			t.Fatalf("update: %v", err)
		}
		if res := r.run(); res.Claimed != 0 || res.Published != 0 {
			t.Fatalf("publish pass: %+v", res)
		}
		if n := r.pump(); n != 0 || r.broker.Counters().Published != 0 || r.effects.Load() != 0 {
			t.Fatalf("a rolled-back event reached the broker or a consumer (delivered %d, %+v)", n, r.broker.Counters())
		}
		if notes, _ := r.mem.Notes(context.Background(), eventstest.TenantA); len(notes) != 0 {
			t.Fatalf("producer state survived the rollback: %v", notes)
		}
	})

	t.Run("a failed commit publishes nothing", func(t *testing.T) {
		var fail atomic.Bool
		r := newOutboxRig(t, memdb.WithCommitFault(func() error {
			if fail.Load() {
				return errors.New("commit refused")
			}
			return nil
		}))
		fail.Store(true)
		if err := r.write("evt_o4", 1, nil); err == nil {
			t.Fatal("the commit fault was ignored")
		}
		fail.Store(false)
		if res := r.run(); res.Claimed != 0 {
			t.Fatalf("publish pass: %+v", res)
		}
		if err := r.write("evt_o4", 1, nil); err != nil {
			t.Fatalf("the identity stayed taken after a failed commit: %v", err)
		}
		if res := r.run(); res.Published != 1 {
			t.Fatalf("publish pass: %+v", res)
		}
	})

	t.Run("a broker outage leaves the row retryable", func(t *testing.T) {
		r := newOutboxRig(t)
		if err := r.write("evt_o5", 1, nil); err != nil {
			t.Fatal(err)
		}
		r.broker.SetDown(errors.New("broker down"))
		if res := r.run(); res.Failed != 1 || res.Published != 0 {
			t.Fatalf("pass during the outage: %+v", res)
		}
		st, _ := r.mem.PublicationStats(context.Background(), eventstest.TenantA, r.clock.Now())
		if st.Pending != 1 || st.LastClass != events.ClassTransient {
			t.Fatalf("stats: %+v", st)
		}
		r.broker.SetDown(nil)
		r.clock.Advance(time.Second)
		if res := r.run(); res.Published != 1 {
			t.Fatalf("pass after recovery: %+v", res)
		}
		if r.pump(); r.effects.Load() != 1 {
			t.Fatalf("effects %d", r.effects.Load())
		}
	})
}

func TestUnitMemoryEventsAreDevelopmentOnly(t *testing.T) {
	mem, err := eventstest.NewMem(memdb.New(), eventstest.DefaultMemOptions())
	if err != nil {
		t.Fatal(err)
	}
	broker, err := eventstest.NewInProcessBroker(mem)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{mem, broker} {
		d, ok := v.(interface{ DevelopmentOnly() bool })
		if !ok || !d.DevelopmentOnly() {
			t.Fatalf("%T is not flagged development-only", v)
		}
	}
	if _, err := eventstest.NewMem(nil, eventstest.DefaultMemOptions()); !errors.Is(err, events.ErrInvalidConfiguration) {
		t.Fatalf("no store: %v", err)
	}
	bad := eventstest.DefaultMemOptions()
	bad.Backlog.MaxEvents = 0
	if _, err := eventstest.NewMem(memdb.New(), bad); !errors.Is(err, events.ErrInvalidConfiguration) {
		t.Fatalf("no backlog limit: %v", err)
	}
	if _, err := eventstest.NewInProcessBroker(nil); !errors.Is(err, events.ErrInvalidConfiguration) {
		t.Fatalf("no resolver: %v", err)
	}
}

func TestUnitMemoryEventsBacklogAndFutureEvents(t *testing.T) {
	opts := eventstest.DefaultMemOptions()
	opts.Backlog = events.BacklogLimits{MaxEvents: 1, MaxBytes: 1 << 20}
	store := memdb.New()
	mem, err := eventstest.NewMem(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := func(id string) eventsv1.Envelope {
		return eventstest.WorkItemTransition(string(eventstest.TenantA), string(eventstest.ProjectA), id, "wi_"+id, 1)
	}
	if err := store.Update(ctx, eventstest.TenantA, func(tx memdb.Tx) error { _, err := mem.Append(tx, env("evt_b1")); return err }); err != nil {
		t.Fatal(err)
	}
	err = store.Update(ctx, eventstest.TenantA, func(tx memdb.Tx) error {
		if err := eventstest.Note(tx, "state"); err != nil {
			return err
		}
		_, err := mem.Append(tx, env("evt_b2"))
		return err
	})
	if !errors.Is(err, events.ErrBacklogFull) {
		t.Fatalf("a full backlog admitted an event: %v", err)
	}
	if notes, _ := mem.Notes(ctx, eventstest.TenantA); len(notes) != 0 {
		t.Fatalf("the refused update left state behind: %v", notes)
	}
	// Another tenant's backlog is its own.
	other := eventstest.WorkItemTransition(string(eventstest.TenantB), string(eventstest.ProjectB), "evt_b3", "wi_b3", 1)
	if err := store.Update(ctx, eventstest.TenantB, func(tx memdb.Tx) error { _, err := mem.Append(tx, other); return err }); err != nil {
		t.Fatalf("tenant B was held back by tenant A's backlog: %v", err)
	}

	future := env("evt_b4")
	future.OccurredAt = time.Now().UTC().Add(time.Hour)
	err = store.Update(ctx, eventstest.TenantA, func(tx memdb.Tx) error { _, err := mem.Append(tx, future); return err })
	if !errors.Is(err, events.ErrFutureEvent) {
		t.Fatalf("an event from the future: %v", err)
	}
}

func TestUnitMemoryEventsReadObservations(t *testing.T) {
	store := memdb.New()
	mem, err := eventstest.NewMem(store, eventstest.DefaultMemOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, id := range []string{"evt_v1", "evt_v2", "evt_v3"} {
		env := eventstest.WorkItemTransition(string(eventstest.TenantA), string(eventstest.ProjectA), id, "wi_x", int64(i+1))
		if err := store.Update(ctx, eventstest.TenantA, func(tx memdb.Tx) error { _, err := mem.Append(tx, env); return err }); err != nil {
			t.Fatal(err)
		}
	}
	err = store.View(ctx, eventstest.TenantA, func(tx memdb.Tx) error {
		high, err := mem.Watermark(tx, eventstest.ScopeA)
		if err != nil || high != 3 {
			t.Fatalf("watermark %d: %v", high, err)
		}
		page, err := mem.ReadObservations(tx, eventstest.ScopeA, 1, 1)
		if err != nil || len(page) != 1 || page[0].Sequence != 2 || page[0].Envelope.ID != "evt_v2" {
			t.Fatalf("page: %+v %v", page, err)
		}
		if _, err := mem.ReadObservations(tx, events.Scope{TenantID: eventstest.TenantB}, 0, 1); !errors.Is(err, events.ErrScopeMismatch) {
			t.Fatalf("another tenant's scope: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var _ domain.TenantID = eventstest.TenantA
}

// TestUnitMemoryBrokerContract runs the shared publisher/transport contract on
// the in-process broker.
func TestUnitMemoryBrokerContract(t *testing.T) {
	eventstest.BrokerContract(t, eventstest.OpenMemBroker)
}

// replayRig is a memory bus with three undelivered events, a consumer whose local
// effect is a committed marker, a hook that runs inside the effect, and a commit
// fault that fires on the nth Update from the moment it is armed.
type replayRig struct {
	t       *testing.T
	store   *memdb.Store
	mem     *eventstest.Mem
	armed   atomic.Int64
	onEvent func(id string)
}

func newReplayRig(t *testing.T) *replayRig {
	t.Helper()
	r := &replayRig{t: t}
	r.store = memdb.New(memdb.WithCommitFault(func() error {
		if r.armed.Add(-1) == 0 {
			return errors.New("commit refused")
		}
		return nil
	}))
	opts := eventstest.DefaultMemOptions()
	opts.Authorizer = eventstest.Operators{eventstest.OperatorID}
	var err error
	r.mem, err = eventstest.NewMem(r.store, opts, eventstest.MemBinding{
		Spec: events.ConsumerSpec{Name: eventstest.ConsumerName, Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(_ context.Context, tx memdb.Tx, env eventsv1.Envelope) error {
			if r.onEvent != nil {
				r.onEvent(env.ID)
			}
			return eventstest.Note(tx, "ledger:"+env.ID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"evt_1", "evt_2", "evt_3"} {
		env := eventstest.WorkItemTransition(string(eventstest.TenantA), string(eventstest.ProjectA), id, "wi_"+id, int64(i+1))
		if err := r.store.Update(context.Background(), eventstest.TenantA, func(tx memdb.Tx) error { _, err := r.mem.Append(tx, env); return err }); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *replayRig) request() events.ReplayRequest {
	return events.ReplayRequest{Scope: eventstest.ScopeA, Consumer: eventstest.ConsumerName, FromSeq: 1, ToSeq: 3, Reason: "recover", Operator: events.Operator{ID: eventstest.OperatorID}}
}

func (r *replayRig) notes() []string {
	r.t.Helper()
	n, err := r.mem.Notes(context.Background(), eventstest.TenantA)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// An interrupted replay releases its range: repeating the request resumes
// through the inbox and no effect is repeated or lost.
func TestUnitMemoryReplayResumesAfterAnInterruption(t *testing.T) {
	all := []string{"ledger:evt_1", "ledger:evt_2", "ledger:evt_3"}

	t.Run("cancellation while an event is processed", func(t *testing.T) {
		r := newReplayRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.onEvent = func(id string) {
			if id == "evt_2" {
				cancel()
			}
		}
		if _, err := r.mem.Replay(ctx, r.request()); err == nil {
			t.Fatal("a cancelled replay reported success")
		}
		if got := r.notes(); len(got) != 1 || got[0] != "ledger:evt_1" {
			t.Fatalf("effects after the interruption: %v", got)
		}
		r.onEvent = nil
		res, err := r.mem.Replay(context.Background(), r.request())
		if err != nil || res.State != "completed" || res.Skipped != 1 || res.Processed != 2 {
			t.Fatalf("the retry: %+v %v", res, err)
		}
		if got := r.notes(); len(got) != 3 || got[0] != all[0] || got[1] != all[1] || got[2] != all[2] {
			t.Fatalf("effects after the retry: %v", got)
		}
		if res, err = r.mem.Replay(context.Background(), r.request()); err != nil || res.Processed != 0 || res.Skipped != 3 {
			t.Fatalf("a replay of finished work: %+v %v", res, err)
		}
	})

	for name, tc := range map[string]struct {
		failAt         int64 // the nth Update after arming fails
		processedFirst int   // effects committed before the failure
		wantSkipped    int
		wantProcessed  int
	}{
		"commit failure at admission":    {failAt: 1, processedFirst: 0, wantSkipped: 0, wantProcessed: 3},
		"commit failure on an event":     {failAt: 3, processedFirst: 1, wantSkipped: 1, wantProcessed: 2},
		"commit failure on the settling": {failAt: 5, processedFirst: 3, wantSkipped: 3, wantProcessed: 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReplayRig(t)
			r.armed.Store(tc.failAt)
			if _, err := r.mem.Replay(context.Background(), r.request()); err == nil {
				t.Fatal("a replay whose commit failed reported success")
			}
			if got := r.notes(); len(got) != tc.processedFirst {
				t.Fatalf("effects after the failure: %v", got)
			}
			res, err := r.mem.Replay(context.Background(), r.request())
			if err != nil || res.State != "completed" || res.Skipped != tc.wantSkipped || res.Processed != tc.wantProcessed {
				t.Fatalf("the retry: %+v %v", res, err)
			}
			if got := r.notes(); len(got) != 3 || got[0] != all[0] || got[1] != all[1] || got[2] != all[2] {
				t.Fatalf("effects after the retry: %v", got)
			}
		})
	}

	t.Run("a replay that is still running blocks an overlapping one", func(t *testing.T) {
		r := newReplayRig(t)
		inner := make(chan error, 1)
		r.onEvent = func(id string) {
			if id != "evt_1" {
				return
			}
			_, err := r.mem.Replay(context.Background(), r.request())
			inner <- err
		}
		// The nested request would wait for the writer slot its caller holds, so
		// give it a bounded context: it must be refused before it waits at all.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.mem.Replay(ctx, r.request()); err != nil {
			t.Fatalf("the outer replay: %v", err)
		}
		if err := <-inner; !errors.Is(err, events.ErrReplayUnsafe) {
			t.Fatalf("an overlapping replay of a running range: %v", err)
		}
	})
}
