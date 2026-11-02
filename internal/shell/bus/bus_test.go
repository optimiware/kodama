package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/bus"
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
