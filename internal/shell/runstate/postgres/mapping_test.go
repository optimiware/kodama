package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func testNormalize(t *testing.T) {
	odd := time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	want := time.Date(2026, 10, 30, 11, 0, 0, 123456000, time.UTC)
	if got := micro(odd); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("micro(%s) = %s", odd, got)
	}
	if got := micro(time.Time{}); !got.IsZero() || got != (time.Time{}) {
		t.Fatalf("the zero time must stay the zero time: %v", got)
	}

	w := runtime.Wait{OpenedAt: odd, Deadline: odd.Add(time.Hour), Decision: runtime.Decision{At: odd}}
	tr := normalize(runtime.Transition{Now: odd, Waits: []runtime.Wait{w},
		Commands:  []runtime.Command{{RecordedAt: odd}},
		Workloads: []runtime.WorkloadObservation{{ObservedAt: odd}}, Resources: []runtime.ResourceObservation{{ObservedAt: odd}},
		Materializations: []runtime.MaterializationRecord{{IssuedAt: odd}}, Events: []runtime.Event{{At: odd}}})
	got := []time.Time{tr.Now, tr.Waits[0].OpenedAt, tr.Waits[0].Decision.At, tr.Commands[0].RecordedAt, tr.Workloads[0].ObservedAt,
		tr.Resources[0].ObservedAt, tr.Materializations[0].IssuedAt, tr.Events[0].At}
	for i, g := range got {
		if !g.Equal(want) {
			t.Errorf("time %d = %s, want %s", i, g, want)
		}
	}
	if d := tr.Waits[0].Deadline.Sub(tr.Waits[0].OpenedAt); d != time.Hour {
		t.Errorf("a deadline stays its open time plus the timeout: %s", d)
	}
	if !w.OpenedAt.Equal(odd) {
		t.Error("normalize must not change the caller's transition")
	}
}

func testEventRowID(t *testing.T) {
	a := eventRowID("p1", "run.created:run1")
	if a != eventRowID("p1", "run.created:run1") {
		t.Error("an event identity is deterministic")
	}
	if a == eventRowID("p2", "run.created:run1") || a == eventRowID("p1", "run.created:run2") {
		t.Error("another project or event has another identity")
	}
	// It fits the kodama_id domain: letters, digits, '_' and '-', at most 128, no dots or colons.
	if len(a) > 128 || strings.ContainsAny(a, ".:/ ") || !strings.HasPrefix(a, "ev-") {
		t.Errorf("event row id %q is not a database identifier", a)
	}
	if eventRowID("ab", "c") == eventRowID("a", "bc") {
		t.Error("the project and the event id must not run together")
	}
	e1 := runtime.Event{ID: "e", At: time.Unix(1, 0)}
	e2 := e1
	e2.At = time.Unix(1, 0).UTC()
	if !sameEvent(e1, e2) {
		t.Error("the same instant is the same event, whatever its zone")
	}
	e2.Version = 2
	if sameEvent(e1, e2) {
		t.Error("other content is another event")
	}
}

func testWaitColumns(t *testing.T) {
	scope := domain.Scope{TenantID: "t1", ProjectID: "p1"}
	spec := builderPlan().Steps[1].Wait
	pending, err := runtime.NewWait(scope, "w1", "run1", "review", *spec)
	must(t, err, "pending wait")

	if c := waitColumnsOf(nil); c != (waitColumns{}) {
		t.Errorf("a container step has no wait columns: %+v", c)
	}
	c := waitColumnsOf(&pending)
	if c.version != int64(1) || c.assignment != "reviewer" || c.state != nil || c.deadline != nil || c.correlation != nil || c.decidedAt != nil {
		t.Errorf("a pending wait records its version and assignment only: %+v", c)
	}

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	open := pending
	must(t, open.Open(now, map[string]string{"candidate": "c1"}, "please"), "open")
	c = waitColumnsOf(&open)
	if c.state != "waiting" || c.version != int64(2) || c.deadline != now.Add(time.Hour) || c.input != "approval" || c.approving != "approved" ||
		c.onExpiry != "fail" || c.correlation != `{"candidate":"c1"}` || c.request != "please" || c.outcome != nil || c.signal != nil {
		t.Errorf("an open wait records what it read: %+v", c)
	}

	cancelledPending := pending
	_, err = cancelledPending.Cancel(now, runtime.ReasonRunCancelled)
	must(t, err, "cancel a pending wait")
	c = waitColumnsOf(&cancelledPending)
	if c.state != "cancelled" || c.reason != "run_cancelled" || c.deadline != nil || c.correlation != nil || c.input != nil || c.decidedAt != now {
		t.Errorf("a wait cancelled before it opened has no opened content: %+v", c)
	}

	resolved := open
	_, err = resolved.Resolve(runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope, Caller: "alice", Key: "k"}, WaitID: "w1",
		Outcome: "approved", Answer: "yes", Actor: "user:alice"}, now.Add(time.Minute))
	must(t, err, "resolve")
	c = waitColumnsOf(&resolved)
	if c.state != "resolved" || c.outcome != "approved" || c.answer != "yes" || c.actor != "user:alice" || c.signal == nil || c.reason != nil {
		t.Errorf("a resolved wait records its decision: %+v", c)
	}

	expired := open
	_, err = expired.Expire(open.Deadline)
	must(t, err, "expire")
	c = waitColumnsOf(&expired)
	if c.state != "expired" || c.outcome != nil || c.actor != nil || c.signal != nil || c.decidedAt != open.Deadline {
		t.Errorf("an expiry that fails the step records no outcome: %+v", c)
	}
}

func testMapErr(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, Message: "m"}) }
	for _, c := range []struct {
		code   string
		target error
	}{
		{"23514", runtime.ErrIllegalTransition}, {"23503", domain.ErrInvalidValue}, {"23505", domain.ErrConflict},
	} {
		got := mapErr(pg(c.code))
		var pe *pgconn.PgError
		if !errors.Is(got, c.target) || !errors.As(got, &pe) || pe.Code != c.code {
			t.Errorf("%s maps to %v, want %v with the database error kept", c.code, got, c.target)
		}
	}
	plain := errors.New("plain")
	if mapErr(plain) != plain || mapErr(nil) != nil {
		t.Error("errors that are not constraint violations pass through")
	}
	if !retryable(pg("40P01")) || !retryable(pg("40001")) || retryable(pg("23505")) || retryable(plain) {
		t.Error("only deadlocks and serialization failures are retried")
	}
}
