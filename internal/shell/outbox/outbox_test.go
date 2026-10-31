package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/outbox"
)

const tenant = domain.TenantID("t_acme")

// fakeBroker is a Sink and Subscriber with the faults of a real broker: an
// outage, a publication stored whose acknowledgment is lost, and injected
// duplicate and out-of-order deliveries. It does not deduplicate by message ID,
// the worst case for consumers.
type fakeBroker struct {
	mu       sync.Mutex
	stream   []entry
	down     error
	loseAcks int
	subs     map[string]*sub
	acked    map[string]map[int]bool
}

type entry struct {
	subject string
	msg     bus.Message
}

type sub struct {
	filter string
	h      bus.Handler
}

func newBroker() *fakeBroker {
	return &fakeBroker{subs: map[string]*sub{}, acked: map[string]map[int]bool{}}
}

func (b *fakeBroker) Publish(_ context.Context, subject string, m bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down != nil {
		return b.down
	}
	b.stream = append(b.stream, entry{subject, m})
	if b.loseAcks > 0 {
		b.loseAcks--
		return errors.New("acknowledgment lost")
	}
	return nil
}

func (b *fakeBroker) Subscribe(_ context.Context, name, filter string, h bus.Handler) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[name] = &sub{filter, h}
	if b.acked[name] == nil {
		b.acked[name] = map[int]bool{}
	}
	return func() {}, nil
}

func (b *fakeBroker) setDown(err error) { b.mu.Lock(); b.down = err; b.mu.Unlock() }
func (b *fakeBroker) len() int          { b.mu.Lock(); defer b.mu.Unlock(); return len(b.stream) }

func (b *fakeBroker) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, e := range b.stream {
		out = append(out, e.msg.ID)
	}
	return out
}

// deliver hands stream entry i to the durable consumer, acknowledging it when
// the handler succeeds. Calling it again is a duplicate delivery.
func (b *fakeBroker) deliver(name string, i int) error {
	b.mu.Lock()
	s, e := b.subs[name], b.stream[i]
	b.mu.Unlock()
	if !bus.MatchSubject(s.filter, e.subject) {
		return nil
	}
	if err := s.h(context.Background(), e.msg); err != nil {
		return err
	}
	b.mu.Lock()
	b.acked[name][i] = true
	b.mu.Unlock()
	return nil
}

// drive delivers every unacknowledged entry once, in stream order, and reports
// how many handlers failed (those stay unacknowledged for the next drive).
func (b *fakeBroker) drive(name string) (failed int) {
	for i := 0; i < b.len(); i++ {
		b.mu.Lock()
		done := b.acked[name][i]
		b.mu.Unlock()
		if !done && b.deliver(name, i) != nil {
			failed++
		}
	}
	return failed
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)} }

func intent(typ, dedup, resource string, data string) outbox.Intent {
	return outbox.Intent{TenantID: tenant, ProjectID: "p_erp", TenantKey: "acme", ProjectKey: "erp", Type: typ, DedupKey: dedup,
		ResourceType: "run", ResourceID: resource, Actor: bus.Actor{Type: "user", ID: "u_1"}, Data: json.RawMessage(data)}
}

func newPublisher(t *testing.T, s outbox.Store, b bus.Sink, c *clock) *outbox.Publisher {
	t.Helper()
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: s, Bus: b, Now: c.now, Batch: 2, Grace: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func put(key, value string) func(context.Context, outbox.Tx) error {
	return func(_ context.Context, tx outbox.Tx) error { tx.(*outbox.MemoryTx).Put(key, value); return nil }
}

// Test_unit_outbox_atomic_publish: state and intent commit together or not at
// all, and publication afterwards is at least once under the same event id.
func Test_unit_outbox_atomic_publish(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, broker := outbox.NewMemoryStore(), newBroker()
	w := outbox.Writer{Store: store, Now: c.now}
	p := newPublisher(t, store, broker, c)

	t.Run("state and intent commit together and are published afterwards", func(t *testing.T) {
		if err := w.Commit(ctx, put("run_1", "queued"), intent("run.created", "trg_1", "run_1", `{"runId":"run_1"}`)); err != nil {
			t.Fatal(err)
		}
		if v, _ := store.Get("run_1"); v != "queued" || len(store.Rows()) != 1 || broker.len() != 0 {
			t.Fatalf("after commit: state=%q rows=%d published=%d (publication must follow the commit)", v, len(store.Rows()), broker.len())
		}
		res, err := p.Drain(ctx)
		if err != nil || res.Published != 1 || broker.len() != 1 {
			t.Fatalf("drain = %+v, %v; broker has %d", res, err, broker.len())
		}
		want, _ := outbox.EventID(tenant, "run.created", "trg_1")
		if got := broker.ids()[0]; got != want || !store.Published(want) {
			t.Fatalf("published id %q, want %q (marked: %v)", got, want, store.Published(want))
		}
		if broker.stream[0].subject != "kodama.v1.acme.erp.run.created" {
			t.Fatalf("subject = %q", broker.stream[0].subject)
		}
		if res, _ := p.Drain(ctx); res.Published != 0 || broker.len() != 1 {
			t.Fatalf("a published row was published again: %+v, %d", res, broker.len())
		}
	})

	t.Run("a failing mutation leaves no intent", func(t *testing.T) {
		boom := errors.New("constraint violation")
		err := w.Commit(ctx, func(_ context.Context, tx outbox.Tx) error {
			tx.(*outbox.MemoryTx).Put("run_2", "queued")
			return boom
		}, intent("run.created", "trg_2", "run_2", ""))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if _, ok := store.Get("run_2"); ok || len(store.Rows()) != 1 {
			t.Fatal("a failed mutation left state or an event behind")
		}
	})

	t.Run("a failed commit loses neither state nor intent half-way", func(t *testing.T) {
		store.FailCommit(errors.New("connection lost"))
		if err := w.Commit(ctx, put("run_3", "queued"), intent("run.created", "trg_3", "run_3", "")); err == nil {
			t.Fatal("commit failure was swallowed")
		}
		if _, ok := store.Get("run_3"); ok || len(store.Rows()) != 1 {
			t.Fatal("a rolled-back transaction left state or an event behind")
		}
	})

	t.Run("an unpublishable intent fails the command before any mutation", func(t *testing.T) {
		called := false
		bad := []outbox.Intent{
			func() outbox.Intent { i := intent("run.created", "k", "r", ""); i.TenantKey = "acme.other"; return i }(),
			intent("run.created", "", "r", ""),
			intent("run.created", "k", "r", "{bad"),
			func() outbox.Intent { i := intent("run.created", "k", "r", ""); i.TenantID = ""; return i }(),
			func() outbox.Intent { i := intent("run.created", "k", "r", ""); i.ProjectKey = ""; return i }(),
		}
		for _, in := range bad {
			err := w.Commit(ctx, func(context.Context, outbox.Tx) error { called = true; return nil }, in)
			if err == nil {
				t.Fatalf("intent %+v was accepted", in)
			}
		}
		if called {
			t.Fatal("the mutation ran although its intent was invalid")
		}
	})

	t.Run("a replayed command does not double the event", func(t *testing.T) {
		in := intent("wait.resolved", "wait_1", "run_1", `{"outcome":"approved"}`)
		for range 3 {
			if err := w.Commit(ctx, put("wait_1", "resolved"), in); err != nil {
				t.Fatal(err)
			}
		}
		n := 0
		for _, r := range store.Rows() {
			if r.Type == "wait.resolved" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d wait.resolved rows for one wait", n)
		}
	})

	t.Run("wake runs only after a commit that enqueued", func(t *testing.T) {
		wakes := 0
		w2 := outbox.Writer{Store: store, Now: c.now, Wake: func() { wakes++ }}
		in := intent("run.state_changed", "run_9/1", "run_9", "")
		_ = w2.Commit(ctx, func(context.Context, outbox.Tx) error { return errors.New("no") }, in)
		_ = w2.Commit(ctx, nil, in)
		_ = w2.Commit(ctx, nil, in) // duplicate id: nothing enqueued
		if wakes != 1 {
			t.Fatalf("wakes = %d", wakes)
		}
	})

	t.Run("a broker outage keeps the intent and defers it with backoff", func(t *testing.T) {
		_, _ = p.Drain(ctx) // flush the row of the previous case
		_ = w.Commit(ctx, nil, intent("run.state_changed", "run_1/2", "run_1", ""))
		id, _ := outbox.EventID(tenant, "run.state_changed", "run_1/2")
		broker.setDown(errors.New("broker down"))
		res, err := p.Drain(ctx)
		if err != nil || res.Failed == 0 || store.Published(id) {
			t.Fatalf("outage drain = %+v, %v", res, err)
		}
		// Not due again until the backoff passed.
		before := broker.len()
		if res, _ := p.Drain(ctx); res.Failed != 0 || res.Published != 0 {
			t.Fatalf("a deferred row was retried at once: %+v", res)
		}
		broker.setDown(nil)
		c.advance(p.Backoff(1) + time.Second)
		if res, err := p.Drain(ctx); err != nil || res.Published == 0 || !store.Published(id) || broker.len() <= before {
			t.Fatalf("recovery drain = %+v, %v", res, err)
		}
	})
}

// Test_unit_outbox_redelivery: lost acknowledgments, crashes after publication,
// duplicate, out-of-order and replayed delivery, and reconciliation converge
// without duplicating a run or regressing a projection.
func Test_unit_outbox_redelivery(t *testing.T) {
	ctx := context.Background()

	// The consumers: run admission owns runs keyed by the trigger key; the
	// projection compares versions.
	type world struct {
		mu     sync.Mutex
		runs   map[string]int // trigger key -> creations
		applic int            // handler executions that did work
		view   map[string]string
		proj   *outbox.Projection
	}
	newWorld := func() *world {
		return &world{runs: map[string]int{}, view: map[string]string{}, proj: &outbox.Projection{}}
	}
	handler := func(w *world) bus.Handler {
		return func(_ context.Context, m bus.Message) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			var d struct {
				Trigger string `json:"trigger"`
				Run     string `json:"run"`
				State   string `json:"state"`
				Version int64  `json:"version"`
			}
			if err := json.Unmarshal(m.Data, &d); err != nil {
				return err
			}
			switch m.Type {
			case "run.created":
				w.applic++
				w.runs[d.Trigger]++
			case "run.state_changed":
				if w.proj.Advance(d.Run, d.Version) {
					w.view[d.Run] = d.State
				}
			}
			return nil
		}
	}

	t.Run("a lost acknowledgment republishes the same id and the consumer applies it once", func(t *testing.T) {
		c := newClock()
		store, broker, w := outbox.NewMemoryStore(), newBroker(), newWorld()
		ledger := &outbox.MemoryLedger{}
		name := "kodama_shell_run-admission"
		_, _ = broker.Subscribe(ctx, name, "kodama.v1.*.*.run.created", outbox.Consumer{Name: name, Ledger: ledger, Apply: handler(w)}.Handle)
		wr := outbox.Writer{Store: store, Now: c.now}
		if err := wr.Commit(ctx, nil, intent("run.created", "trg_1", "run_1", `{"trigger":"trg_1"}`)); err != nil {
			t.Fatal(err)
		}
		p := newPublisher(t, store, broker, c)
		broker.loseAcks = 1
		if res, _ := p.Drain(ctx); res.Failed != 1 || broker.len() != 1 {
			t.Fatalf("first drain = %+v, stream %d", res, broker.len())
		}
		c.advance(p.Backoff(1) + time.Second)
		if res, _ := p.Drain(ctx); res.Published != 1 || broker.len() != 2 {
			t.Fatalf("second drain = %+v, stream %d", res, broker.len())
		}
		ids := broker.ids()
		if ids[0] != ids[1] {
			t.Fatalf("republication changed the event id: %v", ids)
		}
		if f := broker.drive(name); f != 0 {
			t.Fatalf("drive failed %d", f)
		}
		if w.runs["trg_1"] != 1 || w.applic != 1 {
			t.Fatalf("run created %d times (%d applications)", w.runs["trg_1"], w.applic)
		}
	})

	t.Run("a crash between acknowledgment and mark republishes with the same id", func(t *testing.T) {
		c := newClock()
		store, broker := outbox.NewMemoryStore(), newBroker()
		_ = outbox.Writer{Store: store, Now: c.now}.Commit(ctx, nil, intent("run.created", "trg_c", "run_c", ""))
		p := newPublisher(t, store, broker, c)
		store.FailMarkPublished(errors.New("process crashed"))
		if _, err := p.Drain(ctx); err == nil {
			t.Fatal("the failed mark was not reported")
		}
		store.FailMarkPublished(nil)
		if res, err := p.Drain(ctx); err != nil || res.Published != 1 {
			t.Fatalf("drain = %+v, %v", res, err)
		}
		if ids := broker.ids(); len(ids) != 2 || ids[0] != ids[1] {
			t.Fatalf("ids = %v", ids)
		}
	})

	t.Run("duplicate, out-of-order and replayed delivery converge", func(t *testing.T) {
		c := newClock()
		store, broker, w := outbox.NewMemoryStore(), newBroker(), newWorld()
		ledger := &outbox.MemoryLedger{}
		name := "kodama_shell_projection"
		_, _ = broker.Subscribe(ctx, name, "kodama.v1.>", outbox.Consumer{Name: name, Ledger: ledger, Apply: handler(w)}.Handle)
		wr := outbox.Writer{Store: store, Now: c.now}
		for v, st := range []string{"queued", "running", "succeeded"} {
			dk := fmt.Sprintf("run_1/%d", v+1)
			if err := wr.Commit(ctx, nil, intent("run.state_changed", dk, "run_1", fmt.Sprintf(`{"run":"run_1","state":%q,"version":%d}`, st, v+1))); err != nil {
				t.Fatal(err)
			}
		}
		if err := wr.Commit(ctx, nil, intent("run.created", "trg_1", "run_1", `{"trigger":"trg_1"}`)); err != nil {
			t.Fatal(err)
		}
		p := newPublisher(t, store, broker, c)
		if res, err := p.Drain(ctx); err != nil || res.Published != 4 {
			t.Fatalf("drain = %+v, %v", res, err)
		}
		// Newest first, then the older ones, duplicates of the newest, and the
		// creation twice.
		for _, i := range []int{2, 0, 1, 2, 3, 3, 0} {
			if err := broker.deliver(name, i); err != nil {
				t.Fatal(err)
			}
		}
		if w.view["run_1"] != "succeeded" || w.proj.Version("run_1") != 3 {
			t.Fatalf("projection regressed: %v at version %d", w.view, w.proj.Version("run_1"))
		}
		if w.runs["trg_1"] != 1 {
			t.Fatalf("run created %d times", w.runs["trg_1"])
		}
		// Full replay (a new durable consumer, or a restored one with a lost
		// ledger) converges to the same state.
		replay := "kodama_shell_projection-replay"
		w2 := newWorld()
		_, _ = broker.Subscribe(ctx, replay, "kodama.v1.>", outbox.Consumer{Name: replay, Ledger: &outbox.MemoryLedger{}, Apply: handler(w2)}.Handle)
		for _, i := range []int{3, 2, 1, 0, 2} {
			_ = broker.deliver(replay, i)
		}
		if w2.view["run_1"] != "succeeded" || w2.runs["trg_1"] != 1 {
			t.Fatalf("replay diverged: %v %v", w2.view, w2.runs)
		}
	})

	t.Run("a handler error leaves the delivery for redelivery and is not recorded", func(t *testing.T) {
		broker, ledger := newBroker(), &outbox.MemoryLedger{}
		fails := 2
		applied := 0
		name := "kodama_shell_flaky"
		_, _ = broker.Subscribe(ctx, name, "kodama.v1.>", outbox.Consumer{Name: name, Ledger: ledger, Apply: func(context.Context, bus.Message) error {
			if fails > 0 {
				fails--
				return errors.New("store unavailable")
			}
			applied++
			return nil
		}}.Handle)
		_ = broker.Publish(ctx, "kodama.v1.acme.erp.run.created", bus.Message{ID: "evt_x", TenantID: tenant, Type: "run.created"})
		if f := broker.drive(name); f != 1 {
			t.Fatalf("first drive failed %d", f)
		}
		if f := broker.drive(name); f != 1 {
			t.Fatalf("second drive failed %d", f)
		}
		if f := broker.drive(name); f != 0 || applied != 1 {
			t.Fatalf("third drive failed %d, applied %d", f, applied)
		}
		_ = broker.deliver(name, 0) // duplicate after the ack
		if applied != 1 {
			t.Fatalf("applied %d times", applied)
		}
	})

	t.Run("two consumers each apply an event once", func(t *testing.T) {
		ledger := &outbox.MemoryLedger{}
		var a, b int
		ca := outbox.Consumer{Name: "kodama_shell_a", Ledger: ledger, Apply: func(context.Context, bus.Message) error { a++; return nil }}
		cb := outbox.Consumer{Name: "kodama_shell_b", Ledger: ledger, Apply: func(context.Context, bus.Message) error { b++; return nil }}
		m := bus.Message{ID: "evt_1", TenantID: tenant, Type: "x.y"}
		for range 2 {
			_ = ca.Handle(ctx, m)
			_ = cb.Handle(ctx, m)
		}
		if a != 1 || b != 1 {
			t.Fatalf("a=%d b=%d", a, b)
		}
		if err := (outbox.Consumer{}).Handle(ctx, m); err == nil {
			t.Fatal("a consumer without a name, ledger and apply was accepted")
		}
	})

	t.Run("reconciliation republishes committed but unpublished intent", func(t *testing.T) {
		c := newClock()
		store, broker := outbox.NewMemoryStore(), newBroker()
		wr := outbox.Writer{Store: store, Now: c.now}
		p := newPublisher(t, store, broker, c)
		for i := range 5 {
			if err := wr.Commit(ctx, nil, intent("run.created", fmt.Sprintf("trg_%d", i), "run", "")); err != nil {
				t.Fatal(err)
			}
		}
		// The publisher was down: nothing was published and no drain ran.
		if res, _ := p.Reconcile(ctx); res.Published != 0 {
			t.Fatalf("reconciliation touched rows younger than its grace: %+v", res)
		}
		c.advance(2 * time.Minute)
		// A long outage also deferred every row beyond the drain's reach.
		for _, r := range store.Rows() {
			_ = store.MarkFailed(ctx, tenant, r.ID, c.now().Add(24*time.Hour))
		}
		if res, _ := p.Drain(ctx); res.Published != 0 {
			t.Fatalf("the normal drain published deferred rows: %+v", res)
		}
		res, err := p.Reconcile(ctx)
		if err != nil || res.Published != 5 {
			t.Fatalf("reconcile = %+v, %v", res, err)
		}
		for _, r := range store.Rows() {
			if !store.Published(r.ID) {
				t.Fatalf("%s still unpublished", r.ID)
			}
		}
		if res, _ := p.Reconcile(ctx); res.Published != 0 {
			t.Fatalf("a second reconciliation republished %+v", res)
		}
	})

	t.Run("reconciliation during an outage keeps every row", func(t *testing.T) {
		c := newClock()
		store, broker := outbox.NewMemoryStore(), newBroker()
		p := newPublisher(t, store, broker, c)
		for i := range 3 {
			_ = outbox.Writer{Store: store, Now: c.now}.Commit(ctx, nil, intent("run.created", fmt.Sprintf("trg_%d", i), "run", ""))
		}
		c.advance(2 * time.Minute)
		broker.setDown(errors.New("down"))
		res, err := p.Reconcile(ctx)
		if err != nil || res.Failed != 3 || res.Published != 0 {
			t.Fatalf("reconcile during outage = %+v, %v", res, err)
		}
		broker.setDown(nil)
		c.advance(time.Hour)
		if res, _ := p.Drain(ctx); res.Published != 3 {
			t.Fatalf("recovery = %+v", res)
		}
	})
}

func TestEventID(t *testing.T) {
	a, err := outbox.EventID(tenant, "wait.resolved", "wait_1")
	if err != nil || !strings.HasPrefix(a, "evt_") {
		t.Fatalf("id = %q, %v", a, err)
	}
	if b, _ := outbox.EventID(tenant, "wait.resolved", "wait_1"); a != b {
		t.Fatal("the id is not stable")
	}
	for _, other := range [][3]string{{"t_acme", "wait.expired", "wait_1"}, {"t_acme", "wait.resolved", "wait_2"}, {"t_other", "wait.resolved", "wait_1"}, {"t_acme", "wait.resolve", "dwait_1"}} {
		if b, _ := outbox.EventID(domain.TenantID(other[0]), other[1], other[2]); a == b {
			t.Fatalf("%v collides", other)
		}
	}
	if _, err := outbox.EventID(tenant, "wait.resolved", ""); err == nil {
		t.Fatal("an empty deduplication key was accepted")
	}
	if _, err := outbox.EventID(tenant, "", "k"); err == nil {
		t.Fatal("an empty type was accepted")
	}
	if _, err := outbox.EventID(tenant, "wait.resolved", strings.Repeat("k", outbox.MaxDedupKeyLen+1)); err == nil {
		t.Fatal("an oversized key was accepted")
	}
	if _, err := outbox.EventID(tenant, "a", "b\x00c"); err == nil {
		t.Fatal("a NUL in the key was accepted")
	}
}

func TestProjectionNeverRegresses(t *testing.T) {
	var p outbox.Projection
	if !p.Advance("run_1", 2) || p.Advance("run_1", 1) || p.Advance("run_1", 2) || !p.Advance("run_1", 3) || p.Version("run_1") != 3 {
		t.Fatal("projection version rules broken")
	}
	if !p.Advance("run_2", 1) {
		t.Fatal("versions are per key")
	}
}

func TestPublisherRunDrainsOnKick(t *testing.T) {
	store, broker := outbox.NewMemoryStore(), newBroker()
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: store, Bus: broker, Interval: time.Hour, ReconcileEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	if err := (outbox.Writer{Store: store, Wake: p.Kick}).Commit(ctx, nil, intent("run.created", "trg_k", "run", "")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for broker.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if broker.len() != 1 {
		t.Fatalf("a kicked publisher did not publish: %d", broker.len())
	}
	if _, err := outbox.NewPublisher(outbox.PublisherConfig{}); err == nil {
		t.Fatal("a publisher without a store and a bus was accepted")
	}
}

// noCursor is a source whose Unpublished ignores the cursor and leaves Cursor empty,
// as an adapter over a store without a sequence does.
type noCursor struct{ *outbox.MemoryStore }

func (n noCursor) Unpublished(ctx context.Context, tenant domain.TenantID, cutoff time.Time, _ string, limit int) ([]outbox.Row, error) {
	rows, err := n.MemoryStore.Unpublished(ctx, tenant, cutoff, "", limit)
	for i := range rows {
		rows[i].Cursor = ""
	}
	return rows, err
}

func TestReconcileTerminatesWithoutACursor(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	mem, broker := outbox.NewMemoryStore(), newBroker()
	for i := range 5 {
		_ = outbox.Writer{Store: mem, Now: c.now}.Commit(ctx, nil, intent("run.created", fmt.Sprintf("trg_%d", i), "run", ""))
	}
	c.advance(2 * time.Minute)
	p := newPublisher(t, noCursor{mem}, broker, c) // batch of 2
	broker.setDown(errors.New("down"))
	if res, err := p.Reconcile(ctx); err != nil || res.Published != 0 || res.Failed != 2 {
		t.Fatalf("reconcile during outage = %+v, %v (a page of failed rows is attempted once)", res, err)
	}
	broker.setDown(nil)
	if res, err := p.Reconcile(ctx); err != nil || res.Published != 5 {
		t.Fatalf("reconcile after recovery = %+v, %v", res, err)
	}
}

func TestPublisherPagesEveryTenantAndDefersUnpublishableRows(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, broker := outbox.NewMemoryStore(), newBroker()
	w := outbox.Writer{Store: store, Now: c.now}
	for i := range 7 {
		if err := w.Commit(ctx, nil, intent("x.y", fmt.Sprintf("a%02d", i), "r", "")); err != nil {
			t.Fatal(err)
		}
	}
	other := intent("x.y", "b00", "r", "")
	other.TenantID, other.TenantKey, other.ProjectID, other.ProjectKey = "t_b", "b", "", ""
	if err := w.Commit(ctx, nil, other); err != nil {
		t.Fatal(err)
	}
	p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: store, Bus: broker, Batch: 3, Now: c.now, BaseBackoff: time.Second, MaxBackoff: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := p.Drain(ctx); err != nil || res.Published != 8 || res.Failed != 0 || broker.len() != 8 {
		t.Fatalf("drain = %+v, %v (%d on the broker)", res, err, broker.len())
	}
	if got := broker.stream[7].subject; got != "kodama.v1.b._.x.y" {
		t.Fatalf("tenant-wide subject = %s", got)
	}

	// A row whose subject cannot be formed never reaches the bus, is deferred
	// with a growing delay and does not hold back the others. (The writer refuses
	// such an intent; this row stands for one written by another path.)
	bad, _ := intent("x.y", "z-bad", "r", "").Row(c.now())
	bad.TenantKey = "Bad Key"
	good, _ := intent("x.y", "z-good", "r", "").Row(c.now())
	if err := store.Transact(ctx, func(ctx context.Context, tx outbox.Tx) error {
		for _, r := range []outbox.Row{bad, good} {
			if _, err := tx.Enqueue(ctx, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := p.Drain(ctx)
	if err != nil || res.Published != 1 || res.Failed != 1 || !store.Published(good.ID) || store.Published(bad.ID) {
		t.Fatalf("drain with a bad subject = %+v, %v", res, err)
	}
	for attempts, want := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 8 * time.Second, 40: 8 * time.Second} {
		if got := p.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempts, got, want)
		}
	}
}

// timeOrderSource lists unpublished rows in (created_at, id) order, the order of
// the PostgreSQL adapter (audit_events_unpublished), where created_at is fixed
// when the row is inserted and not when its transaction commits. afterRead runs
// once the first page has been read and before it is returned, the moment at
// which another connection can commit a transaction that began earlier.
type timeOrderSource struct {
	*outbox.MemoryStore
	afterRead func()
}

func timeKey(r outbox.Row) string { return fmt.Sprintf("%020d/%s", r.CreatedAt.UnixNano(), r.ID) }

func (s *timeOrderSource) Unpublished(_ context.Context, tn domain.TenantID, cutoff time.Time, after string, limit int) ([]outbox.Row, error) {
	var out []outbox.Row
	for _, r := range s.MemoryStore.Rows() {
		if r.TenantID == tn && !s.MemoryStore.Published(r.ID) && !r.CreatedAt.After(cutoff) && timeKey(r) > after {
			r.Cursor = timeKey(r)
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cursor < out[j].Cursor })
	if len(out) > limit {
		out = out[:limit]
	}
	if fn := s.afterRead; fn != nil {
		s.afterRead = nil
		fn()
	}
	return out, nil
}

// Test_unit_outbox_cursor_covers_concurrent_commits: a transaction that inserted
// its row before a page was read but commits after it carries an earlier
// timestamp than the page's cursor, so that pass cannot see it. The guarantee
// (the package documentation's "Cursor and commit visibility") is that the
// late row is delivered exactly once by the next pass, or by the next drain,
// whichever comes first, and never again after that.
func Test_unit_outbox_cursor_covers_concurrent_commits(t *testing.T) {
	ctx := context.Background()
	c := newClock() // 10:00
	early := func(h, m int) time.Time { return time.Date(2026, 10, 31, h, m, 0, 0, time.UTC) }
	commitAt := func(s outbox.Store, at time.Time, key string) string {
		t.Helper()
		in := intent("run.created", key, "run", "")
		if err := (outbox.Writer{Store: s, Now: func() time.Time { return at }}).Commit(ctx, nil, in); err != nil {
			t.Fatal(err)
		}
		id, _ := outbox.EventID(tenant, in.Type, in.DedupKey)
		return id
	}
	// The late transaction inserted its row at 08:00, long before the others.
	lateRow := func(t *testing.T) outbox.Row {
		t.Helper()
		r, err := intent("run.created", "late", "run", "").Row(early(8, 0))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	commitLate := func(s outbox.Store, r outbox.Row) func() {
		return func() {
			if err := s.Transact(ctx, func(ctx context.Context, tx outbox.Tx) error { _, err := tx.Enqueue(ctx, r); return err }); err != nil {
				t.Error(err)
			}
		}
	}
	count := func(b *fakeBroker, id string) (n int) {
		for _, got := range b.ids() {
			if got == id {
				n++
			}
		}
		return n
	}
	setup := func(t *testing.T) (*timeOrderSource, *fakeBroker, *outbox.Publisher, outbox.Row, string, string) {
		t.Helper()
		mem, broker := outbox.NewMemoryStore(), newBroker()
		a, b := commitAt(mem, early(9, 0), "a"), commitAt(mem, early(9, 30), "b")
		src := &timeOrderSource{MemoryStore: mem}
		late := lateRow(t)
		src.afterRead = commitLate(mem, late)
		p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: src, Bus: broker, Now: c.now, Batch: 1, Grace: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return src, broker, p, late, a, b
	}

	t.Run("the next reconciliation delivers it once", func(t *testing.T) {
		src, broker, p, late, a, b := setup(t)
		// Pass 1 reads a page of one row (a), then the late transaction commits:
		// the row lies before the cursor, so this pass does not see it.
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 2 || count(broker, late.ID) != 0 {
			t.Fatalf("first pass = %+v, %v; late delivered %d times", res, err, count(broker, late.ID))
		}
		if src.Published(late.ID) {
			t.Fatal("the late row was marked published without a publication")
		}
		// Pass 2 starts from the beginning with a fresh cutoff: it finds only the late row.
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 1 || count(broker, late.ID) != 1 {
			t.Fatalf("second pass = %+v, %v", res, err)
		}
		// Nothing repeats.
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 0 {
			t.Fatalf("third pass = %+v, %v", res, err)
		}
		if res, err := p.Drain(ctx); err != nil || res.Published != 0 {
			t.Fatalf("drain = %+v, %v", res, err)
		}
		for _, id := range []string{a, b, late.ID} {
			if n := count(broker, id); n != 1 {
				t.Fatalf("event %s delivered %d times", id, n)
			}
		}
	})

	t.Run("the next drain delivers it once, whatever the cursor", func(t *testing.T) {
		src, broker, p, late, _, _ := setup(t)
		if _, err := p.Reconcile(ctx); err != nil || count(broker, late.ID) != 0 {
			t.Fatalf("first pass: %v (late delivered %d times)", err, count(broker, late.ID))
		}
		if res, err := p.Drain(ctx); err != nil || res.Published != 1 || !src.Published(late.ID) {
			t.Fatalf("drain = %+v, %v", res, err)
		}
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 0 || count(broker, late.ID) != 1 {
			t.Fatalf("reconcile after the drain = %+v, %v (late delivered %d times)", res, err, count(broker, late.ID))
		}
	})

	t.Run("a late row that fails stays covered", func(t *testing.T) {
		_, broker, p, late, _, _ := setup(t)
		if _, err := p.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		broker.setDown(errors.New("down"))
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 0 || res.Failed != 1 {
			t.Fatalf("pass during an outage = %+v, %v", res, err)
		}
		broker.setDown(nil)
		if res, err := p.Reconcile(ctx); err != nil || res.Published != 1 || count(broker, late.ID) != 1 {
			t.Fatalf("pass after the outage = %+v, %v (%d stream entries)", res, err, count(broker, late.ID))
		}
	})
}
