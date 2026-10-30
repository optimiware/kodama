package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/bus"
)

func TestSubject(t *testing.T) {
	for _, c := range []struct {
		tenant, project, typ, want string
	}{
		{"acme", "erp", "git.refs_updated", "kodama.v1.acme.erp.git.refs_updated"},
		{"acme", "", "repository.created", "kodama.v1.acme._.repository.created"},
		{"a_b-c", "p-1", "run.state_changed", "kodama.v1.a_b-c.p-1.run.state_changed"},
	} {
		got, err := bus.Subject(c.tenant, c.project, c.typ)
		if err != nil || got != c.want {
			t.Errorf("Subject(%q,%q,%q) = %q, %v; want %q", c.tenant, c.project, c.typ, got, err, c.want)
		}
	}
	// A key or type that could name another tenant's subject or a wildcard is refused.
	for _, c := range [][3]string{
		{"acme.other", "erp", "git.refs_updated"}, {"acme", "erp.x", "git.refs_updated"}, {"*", "erp", "x.y"}, {"acme", ">", "x.y"},
		{"acme", "erp", "git refs"}, {"acme", "erp", "git.*"}, {"Acme", "erp", "x.y"}, {"", "erp", "x.y"}, {"acme", "erp", ""},
		{"acme", "erp", "a.b.c.d.e"},
	} {
		if s, err := bus.Subject(c[0], c[1], c[2]); err == nil {
			t.Errorf("Subject(%q,%q,%q) = %q, want an error", c[0], c[1], c[2], s)
		}
	}
}

func TestMatchSubject(t *testing.T) {
	for _, c := range []struct {
		filter, subject string
		want            bool
	}{
		{"kodama.v1.>", "kodama.v1.acme.erp.git.refs_updated", true},
		{"kodama.v1.*.*.git.refs_updated", "kodama.v1.acme.erp.git.refs_updated", true},
		{"kodama.v1.*.*.git.refs_updated", "kodama.v1.acme._.git.refs_updated", true},
		{"kodama.v1.*.*.git.refs_updated", "kodama.v1.acme.erp.repository.created", false},
		{"kodama.v1.*.*.git.refs_updated", "kodama.v1.acme.git.refs_updated", false},
		{"kodama.v1.acme.>", "kodama.v1.other.erp.x.y", false},
		{"kodama.v1.>", "kodama.v1", false},
		{"a.b", "a.b.c", false},
		{"a.b.c", "a.b", false},
	} {
		if got := bus.MatchSubject(c.filter, c.subject); got != c.want {
			t.Errorf("MatchSubject(%q,%q) = %v", c.filter, c.subject, got)
		}
	}
}

func TestMessageWireForm(t *testing.T) {
	m := bus.Message{ID: "evt_1", TenantID: "t_acme", ProjectID: "p_erp", Type: "git.refs_updated", ResourceType: "git_push", ResourceID: "rcv_1",
		Actor: bus.Actor{Type: "user", ID: "u_1"}, OccurredAt: time.Date(2026, 10, 30, 10, 0, 0, 123000, time.UTC), Data: json.RawMessage(`{"a":1}`)}
	raw, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := bus.DecodeMessage(raw)
	if err != nil || got.ID != m.ID || got.TenantID != m.TenantID || !got.OccurredAt.Equal(m.OccurredAt) || string(got.Data) != `{"a":1}` {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for _, bad := range []string{``, `[]`, `{}`, `{"id":"e","type":"t"}`, `{"id":"","type":"t","tenantId":"t_a"}`} {
		if _, err := bus.DecodeMessage([]byte(bad)); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestMemoryBusDeliversToOneConsumerPerName(t *testing.T) {
	b := bus.NewMemory()
	var a1, a2, other int
	if _, err := b.Subscribe(t.Context(), "a", "kodama.v1.>", func(context.Context, bus.Message) error { a1++; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(t.Context(), "a", "kodama.v1.>", func(context.Context, bus.Message) error { a2++; return nil }); err != nil {
		t.Fatal(err)
	}
	stop, _ := b.Subscribe(t.Context(), "b", "kodama.v1.x.>", func(context.Context, bus.Message) error { other++; return nil })
	m := bus.Message{ID: "e", TenantID: "t_a", Type: "x.y"}
	if err := b.Publish(t.Context(), "kodama.v1.x.p.x.y", m); err != nil {
		t.Fatal(err)
	}
	if a1+a2 != 1 || other != 1 {
		t.Fatalf("deliveries a=%d+%d b=%d", a1, a2, other)
	}
	stop()
	if err := b.Publish(t.Context(), "kodama.v1.x.p.x.y", m); err != nil || other != 1 {
		t.Fatalf("a stopped consumer was called: %d %v", other, err)
	}
	// A consumer's error is the publish's error, and nothing is recorded.
	boom := errors.New("handler failed")
	if _, err := b.Subscribe(t.Context(), "c", "kodama.v1.>", func(context.Context, bus.Message) error { return boom }); err != nil {
		t.Fatal(err)
	}
	n := len(b.Published())
	if err := b.Publish(t.Context(), "kodama.v1.y.p.x.y", m); !errors.Is(err, boom) || len(b.Published()) != n {
		t.Fatalf("publish with a failing consumer = %v (%d recorded, was %d)", err, len(b.Published()), n)
	}
	if _, err := b.Subscribe(t.Context(), "", "x", func(context.Context, bus.Message) error { return nil }); err == nil {
		t.Fatal("an unnamed consumer was accepted")
	}
}

// fakeOutbox is an in-memory outbox with a controllable failure.
type fakeOutbox struct {
	mu       sync.Mutex
	rows     map[domain.TenantID][]*fakeRow
	failMark error
	failNote error
}

type fakeRow struct {
	bus.Row
	published *time.Time
	next      time.Time
}

func (o *fakeOutbox) add(tenant domain.TenantID, tenantKey, projectKey, id, typ string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rows == nil {
		o.rows = map[domain.TenantID][]*fakeRow{}
	}
	o.rows[tenant] = append(o.rows[tenant], &fakeRow{next: at, Row: bus.Row{Message: bus.Message{ID: id, TenantID: tenant, Type: typ}, TenantKey: tenantKey, ProjectKey: projectKey}})
}

func (o *fakeOutbox) Tenants(context.Context) ([]domain.TenantID, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []domain.TenantID
	for t := range o.rows {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (o *fakeOutbox) Pending(_ context.Context, tenant domain.TenantID, now time.Time, limit int) ([]bus.Row, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var due []*fakeRow
	for _, r := range o.rows[tenant] {
		if r.published == nil && !r.next.After(now) {
			due = append(due, r)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].next.Equal(due[j].next) {
			return due[i].next.Before(due[j].next)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]bus.Row, len(due))
	for i, r := range due {
		out[i] = r.Row
	}
	return out, nil
}

func (o *fakeOutbox) find(tenant domain.TenantID, id string) *fakeRow {
	for _, r := range o.rows[tenant] {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (o *fakeOutbox) Published(_ context.Context, tenant domain.TenantID, id string, at time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failMark != nil {
		return o.failMark
	}
	if r := o.find(tenant, id); r != nil && r.published == nil {
		r.published = &at
	}
	return nil
}

func (o *fakeOutbox) Failed(_ context.Context, tenant domain.TenantID, id string, next time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failNote != nil {
		return o.failNote
	}
	if r := o.find(tenant, id); r != nil && r.published == nil {
		r.Attempts++
		r.next = next
	}
	return nil
}

func TestPublisherDrainsEveryTenantInPagesAndRetriesWithBackoff(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	o := &fakeOutbox{}
	for i := 0; i < 7; i++ {
		o.add("t_a", "a", "p", fmt.Sprintf("a%02d", i), "x.y", now)
	}
	o.add("t_b", "b", "", "b00", "x.y", now)
	b := bus.NewMemory()
	p, err := bus.NewPublisher(bus.PublisherConfig{Outbox: o, Bus: b, Batch: 3, Now: func() time.Time { return now }, BaseBackoff: time.Second, MaxBackoff: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Drain(t.Context())
	if err != nil || res.Published != 8 || res.Failed != 0 {
		t.Fatalf("drain = %+v, %v", res, err)
	}
	var ids []string
	for _, pub := range b.Published() {
		ids = append(ids, pub.Message.ID)
	}
	if fmt.Sprint(ids) != "[a00 a01 a02 a03 a04 a05 a06 b00]" {
		t.Fatalf("published in order %v", ids)
	}
	if b.Published()[7].Subject != "kodama.v1.b._.x.y" {
		t.Fatalf("tenant-wide subject = %s", b.Published()[7].Subject)
	}

	// An event whose subject cannot be formed never reaches the bus, is retried
	// with a growing delay and does not hold back the others.
	o.add("t_a", "Bad Key", "p", "z-bad", "x.y", now)
	o.add("t_a", "a", "p", "z-good", "x.y", now)
	res, err = p.Drain(t.Context())
	if err != nil || res.Published != 1 || res.Failed != 1 {
		t.Fatalf("drain with a bad subject = %+v, %v", res, err)
	}
	bad := o.find("t_a", "z-bad")
	if bad.Attempts != 1 || !bad.next.Equal(now.Add(time.Second)) {
		t.Fatalf("bad row = %+v", bad)
	}
	for attempts, want := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 8 * time.Second, 40: 8 * time.Second} {
		if got := p.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempts, got, want)
		}
	}

	// A mark that fails after a successful publication repeats the publication
	// (at least once) under the same ID and reports the failure.
	o.add("t_a", "a", "p", "m1", "x.y", now)
	o.failMark = errors.New("db down")
	before := len(b.Published())
	if _, err := p.Drain(t.Context()); err == nil {
		t.Fatal("a failed mark was not reported")
	}
	o.failMark = nil
	if _, err := p.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, pub := range b.Published()[before:] {
		if pub.Message.ID == "m1" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("m1 was published %d times, want the at-least-once repeat", count)
	}
	if res, _ := p.Drain(t.Context()); res.Published != 0 {
		t.Fatalf("published rows come back: %+v", res)
	}

	// A failure to record the failure is reported and ends the round; the row
	// stays due, so it is attempted again.
	o.add("t_b", "b", "", "n1", "x.y", now)
	b.FailNext(1, errors.New("broker down"))
	o.failNote = errors.New("db down")
	if _, err := p.Drain(t.Context()); err == nil {
		t.Fatal("a failed failure record was not reported")
	}
	o.failNote = nil
	if res, err := p.Drain(t.Context()); err != nil || res.Published != 1 {
		t.Fatalf("recovery drain = %+v, %v", res, err)
	}
}

func TestPublisherRunStopsOnCancelAndKickWakes(t *testing.T) {
	now := time.Now()
	o := &fakeOutbox{}
	b := bus.NewMemory()
	p, err := bus.NewPublisher(bus.PublisherConfig{Outbox: o, Bus: b, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	o.add("t_a", "a", "p", "k1", "x.y", now.Add(-time.Second))
	p.Kick()
	deadline := time.Now().Add(5 * time.Second)
	for len(b.Published()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(b.Published()) != 1 {
		t.Fatal("a kick did not drain the outbox before the interval")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if _, err := bus.NewPublisher(bus.PublisherConfig{}); err == nil {
		t.Fatal("a publisher without an outbox and a bus")
	}
}
