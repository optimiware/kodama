package logstest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	lt "github.com/optimiware/kodama/internal/logs/logstest"
)

var bg = context.Background()

// limits are small, explicit ceilings that tests can reach.
func limits() logs.Limits {
	return logs.Limits{DefaultTail: 5, MaxTail: 10, MaxLines: 10, MaxBytes: 50, MaxLineBytes: 20, MaxScanBytes: 2000,
		MaxDuration: 20 * time.Second, MaxStreams: 2, WriteTimeout: time.Second}
}

type collected struct {
	meta    logs.Meta
	lines   []string
	chunks  []logs.Chunk
	summary logs.Summary
}

type collector struct{ c *collected }

func (s collector) Begin(m logs.Meta) error { s.c.meta = m; return nil }
func (s collector) Chunk(c logs.Chunk) error {
	s.c.lines, s.c.chunks = append(s.c.lines, c.Text), append(s.c.chunks, c)
	return nil
}
func (s collector) End(sum logs.Summary) error { s.c.summary = sum; return nil }

func read(t *testing.T, svc *logs.Service, req logs.Request) (*collected, error) {
	t.Helper()
	sess, err := svc.Open(bg, req)
	if err != nil {
		return nil, err
	}
	out := &collected{}
	if err := sess.Run(bg, collector{out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return out, nil
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	return b.String()
}

func ptr[T any](v T) *T { return &v }

const alice = "u_alice"

// rig is a Memory world with alice reading scope A.
type rig struct {
	m   *lt.Memory
	svc *logs.Service
}

func newRig(t *testing.T, lim logs.Limits) *rig {
	t.Helper()
	m := lt.NewMemory()
	m.Authority.Grant(alice, lt.ScopeA)
	return &rig{m: m, svc: m.Service(t, lim)}
}

func (r *rig) req(a lt.MemoryAttempt, mod func(*logs.Request)) logs.Request {
	q := logs.Request{Principal: alice, Scope: a.Scope, Run: a.Run, Attempt: a.ID}
	if mod != nil {
		mod(&q)
	}
	return q
}

// running creates an observed attempt whose Job logs body.
func (r *rig) running(t *testing.T, scope domain.Scope, body string) lt.MemoryAttempt {
	t.Helper()
	a := r.m.NewAttempt(t, scope, lt.AttemptOptions{Observed: true})
	r.m.Live.Set(a.Job, lt.JobUID, body)
	return a
}

// Test_unit_log_scope proves tenant and run selectors, read bounds and the
// retained and fallback selection without a service (PX-05, PX-07, AC-06).
func Test_unit_log_scope(t *testing.T) {
	t.Run("selectors resolve only the caller's scope, run and attempt", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.running(t, lt.ScopeA, numbered(3))
		b := r.running(t, lt.ScopeB, "tenant B secret\n")
		r.m.Authority.Grant(alice, lt.ScopeA2)
		inOtherProject := r.running(t, lt.ScopeA2, "project A2 only\n")
		r.m.Authority.Revoke(alice, lt.ScopeA2)
		_, before := r.m.Live.Queries()

		// The run's only attempt may be left implicit.
		got, err := read(t, r.svc, r.req(a, func(q *logs.Request) { q.Attempt = "" }))
		if err != nil || len(got.lines) != 3 || got.meta.Origin != logs.OriginLive || got.meta.Attempt != a.ID || got.meta.Run != a.Run {
			t.Fatalf("own run: %+v %v", got, err)
		}
		targets, _ := r.m.Live.Queries()
		if want := (logs.Target{Namespace: lt.Namespace, JobName: a.Job, JobUID: lt.JobUID, Container: lt.Container}); targets[len(targets)-1] != want {
			t.Fatalf("the workload is resolved server-side: %+v want %+v", targets[len(targets)-1], want)
		}
		_, after := r.m.Live.Queries()
		before = after

		deny := map[string]logs.Request{
			"another tenant's scope":         r.req(b, nil),
			"a project the caller is not in": r.req(inOtherProject, nil),
			"another tenant's run":           r.req(a, func(q *logs.Request) { q.Run = b.Run; q.Attempt = "" }),
			"another tenant's attempt":       r.req(a, func(q *logs.Request) { q.Attempt = b.ID }),
			"another project's run in scope": r.req(a, func(q *logs.Request) { q.Run = inOtherProject.Run; q.Attempt = "" }),
			"another project's attempt":      r.req(a, func(q *logs.Request) { q.Attempt = inOtherProject.ID }),
			"an unknown run":                 r.req(a, func(q *logs.Request) { q.Run = "run_unknown"; q.Attempt = "" }),
			"an unknown principal":           r.req(a, func(q *logs.Request) { q.Principal = "u_mallory" }),
			"no principal":                   r.req(a, func(q *logs.Request) { q.Principal = "" }),
			"an incomplete scope":            r.req(a, func(q *logs.Request) { q.Scope.ProjectID = "" }),
			"an invalid run identifier":      r.req(a, func(q *logs.Request) { q.Run = "not valid!" }),
		}
		for name, q := range deny {
			if _, err := r.svc.Open(bg, q); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, after := r.m.Live.Queries(); len(after) != len(before) {
			t.Fatalf("a denied selector reached Kubernetes %d times", len(after)-len(before))
		}

		// Several attempts of one run need an explicit attempt.
		second := r.m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Run: a.Run, Observed: true})
		r.m.Live.Set(second.Job, lt.JobUID, "second\n")
		if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Attempt = "" })); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("an ambiguous run: %v", err)
		}
		if got, err := read(t, r.svc, r.req(second, nil)); err != nil || got.lines[0] != "second" {
			t.Fatalf("an explicit attempt: %+v %v", got, err)
		}
	})

	t.Run("authority is asked on every open and fails closed", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.running(t, lt.ScopeA, numbered(2))
		r.m.Authority.Grant("u_bob", lt.ScopeA)
		if _, err := read(t, r.svc, r.req(a, func(q *logs.Request) { q.Principal = "u_bob" })); err != nil {
			t.Fatal(err)
		}
		asked := r.m.Authority.Asked()
		r.m.Authority.Revoke("u_bob", lt.ScopeA)
		_, queries := r.m.Live.Queries()
		if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Principal = "u_bob" })); !errors.Is(err, logs.ErrNotFound) || r.m.Authority.Asked() != asked+1 {
			t.Fatalf("a revoked membership denies the reconnect: %v", err)
		}
		r.m.Authority.Deny(alice, lt.ScopeA)
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrDenied) {
			t.Fatalf("a refused member: %v", err)
		}
		r.m.Authority.Fail(errors.New("identity: connection refused"))
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("an authority outage is never access: %v", err)
		}
		if _, after := r.m.Live.Queries(); len(after) != len(queries) {
			t.Fatal("a denied or failed authorization reached Kubernetes")
		}
	})

	t.Run("tail, byte, line and stream bounds apply", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.running(t, lt.ScopeA, numbered(40))
		for _, tail := range []int{0, -1, 11, 1 << 20} {
			if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Tail = ptr(tail) })); !errors.Is(err, logs.ErrInvalid) {
				t.Errorf("tail %d: %v", tail, err)
			}
		}
		got, err := read(t, r.svc, r.req(a, nil))
		if err != nil || len(got.lines) != 5 || got.lines[0] != "line 36" || got.meta.Tail != 5 {
			t.Fatalf("the default tail is bounded: %v %+v %v", got.lines, got.meta, err)
		}
		_, queries := r.m.Live.Queries()
		if q := queries[len(queries)-1]; q.TailLines != 5 || q.LimitBytes != r.svc.Limits().MaxScanBytes || q.Follow {
			t.Fatalf("the server builds the query: %+v", q)
		}
		got, err = read(t, r.svc, r.req(a, func(q *logs.Request) { q.Tail = ptr(3) }))
		if err != nil || len(got.lines) != 3 || got.lines[2] != "line 40" || got.summary.Stop != logs.StopEnd || got.summary.Truncated {
			t.Fatalf("an explicit tail: %v %+v %v", got.lines, got.summary, err)
		}
		if got.meta.Limits.MaxBytes != 50 || got.meta.Limits.MaxLines != 10 || got.meta.Limits.MaxLineBytes != 20 {
			t.Fatalf("the applied bounds are echoed: %+v", got.meta.Limits)
		}

		// A byte bound ends the read and says so.
		got, err = read(t, r.svc, r.req(a, func(q *logs.Request) { q.Tail = ptr(10) }))
		if err != nil || got.summary.Stop != logs.StopByteLimit || !got.summary.Truncated || got.summary.Bytes > 50 || len(got.lines) != 7 {
			t.Fatalf("byte limit: %+v lines=%d %v", got.summary, len(got.lines), err)
		}

		// One long line is cut at the line bound, not the whole read.
		long := r.running(t, lt.ScopeA, strings.Repeat("x", 100)+"\nshort\n")
		got, err = read(t, r.svc, r.req(long, nil))
		if err != nil || len(got.lines) != 2 || len(got.lines[0]) != 20 || !got.chunks[0].LineTruncated || got.chunks[1].LineTruncated || !got.summary.Truncated {
			t.Fatalf("line bound: %v %+v %v", got.lines, got.summary, err)
		}

		// The concurrent stream ceiling is a bound, and slots come back.
		s1, err := r.svc.Open(bg, r.req(a, nil))
		if err != nil {
			t.Fatal(err)
		}
		s2, err := r.svc.Open(bg, r.req(a, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrTooManyStreams) {
			t.Fatalf("stream ceiling: %v", err)
		}
		s1.Close()
		s1.Close() // idempotent
		s3, err := r.svc.Open(bg, r.req(a, nil))
		if err != nil {
			t.Fatalf("a closed session frees its slot: %v", err)
		}
		s2.Close()
		s3.Close()
		if n := r.m.Live.OpenStreams(); n != 0 {
			t.Fatalf("%d upstream streams leaked", n)
		}
	})

	t.Run("a retained read serves the newest lines within the bounds", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Observed: true, Terminal: true})
		r.m.WrapperLog(t, a, numbered(30), contract.Complete)
		got, err := read(t, r.svc, r.req(a, func(q *logs.Request) { q.Tail = ptr(4) }))
		if err != nil || len(got.lines) != 4 || got.lines[0] != "line 27" || got.lines[3] != "line 30" || !got.summary.Truncated && got.summary.Lines != 4 {
			t.Fatalf("%v %+v %v", got.lines, got.summary, err)
		}
		// Follow is for a running attempt's live log only.
		if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Follow = true })); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("follow a finished attempt: %v", err)
		}
		run := r.running(t, lt.ScopeA, numbered(3))
		if _, err := r.svc.Open(bg, r.req(run, func(q *logs.Request) { q.Follow = true; q.Mode = logs.ModeWrapper })); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("follow a retained source: %v", err)
		}
		if _, err := r.svc.Open(bg, r.req(run, func(q *logs.Request) { q.Mode = "everything" })); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("an unknown source: %v", err)
		}
	})

	t.Run("fallback never hides an outage or serves a foreign workload", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Observed: true}) // running, its Job is gone
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("a running attempt with neither a live nor a retained log: %v", err)
		}
		r.m.WrapperLog(t, a, "from the wrapper\n", "")
		got, err := read(t, r.svc, r.req(a, nil))
		if err != nil || got.meta.Origin != logs.OriginWrapper || got.lines[0] != "from the wrapper" {
			t.Fatalf("a missing workload falls back to the retained log: %+v %v", got, err)
		}
		if got.meta.Completeness != logs.Unknown || len(got.meta.Reasons) != 1 || got.meta.Reasons[0] != contract.ReasonUnverified {
			t.Fatalf("a log without a sidecar has unknown completeness: %+v", got.meta)
		}
		if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Mode = logs.ModeLive })); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("live mode never falls back: %v", err)
		}

		r.m.Live.Set(a.Job, "99999999-0000-0000-0000-000000000000", "another Job's secret\n")
		_, err = r.svc.Open(bg, r.req(a, nil))
		if !errors.Is(err, logs.ErrUnavailable) || strings.Contains(fmt.Sprint(err), "another") {
			t.Fatalf("a Job that is not this attempt's is never served: %v", err)
		}
		r.m.Live.Set(a.Job, lt.JobUID, "live\n")
		r.m.Live.Fail(errors.New("kube: 503 at 10.0.0.1"))
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("an outage is reported, not hidden by the retained log: %v", err)
		}
		r.m.Live.Fail(nil)

		fresh := r.m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{})
		if _, err := r.svc.Open(bg, r.req(fresh, nil)); !errors.Is(err, logs.ErrWorkloadNotObserved) {
			t.Fatalf("an unobserved Job selects nothing by name: %v", err)
		}
	})

	t.Run("a finished attempt serves the best retained record and falls back past removed content", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.running(t, lt.ScopeA, "controller view 1\ncontroller view 2\n")
		r.m.Finish(t, a)
		ctrl := r.m.Controller(t, 0)

		// Nothing retained yet: the finished attempt's Job is read live.
		if got, err := read(t, r.svc, r.req(a, nil)); err != nil || got.meta.Origin != logs.OriginLive || !got.meta.Terminal {
			t.Fatalf("%+v %v", got, err)
		}
		for _, mode := range []logs.Mode{logs.ModeWrapper, logs.ModeController} {
			if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Mode = mode })); !errors.Is(err, logs.ErrNoLog) {
				t.Fatalf("%s source without a capture: %v", mode, err)
			}
		}

		captured, err := ctrl.Collect(bg, a.Scope, a.ID)
		if err != nil || captured.State != logs.StateRetained || captured.Source != logs.SourceController {
			t.Fatalf("collect: %+v %v", captured, err)
		}
		got, err := read(t, r.svc, r.req(a, nil))
		if err != nil || got.meta.Origin != logs.OriginController || got.meta.Completeness == logs.Complete || len(got.lines) != 2 {
			t.Fatalf("only the controller's capture exists: %+v %v", got, err)
		}

		wrapperID := r.m.WrapperLog(t, a, "wrapper view\n", contract.Complete)
		if _, err := ctrl.Observe(bg, a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		got, err = read(t, r.svc, r.req(a, nil))
		if err != nil || got.meta.Origin != logs.OriginWrapper || got.meta.Completeness != logs.Complete || got.lines[0] != "wrapper view" {
			t.Fatalf("a verified complete wrapper capture wins: %+v %v", got, err)
		}
		got, err = read(t, r.svc, r.req(a, func(q *logs.Request) { q.Mode = logs.ModeController }))
		if err != nil || got.meta.Origin != logs.OriginController || got.lines[0] != "controller view 1" {
			t.Fatalf("an explicit source is never merged: %+v %v", got, err)
		}

		// Retention removes the wrapper's bytes: Reconcile records availability
		// only, and the read falls back without ever merging sources.
		r.m.RemoveArtifact(t, a, wrapperID)
		if err := ctrl.Reconcile(bg, a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		recs, _ := r.m.Store().ForAttempt(bg, a.Scope, a.ID)
		for _, rec := range recs {
			if rec.Source == logs.SourceWrapper && (rec.Available() || rec.Link == nil || rec.Link.Completeness != logs.Complete) {
				t.Fatalf("removal changes availability only: %+v", rec)
			}
		}
		got, err = read(t, r.svc, r.req(a, nil))
		if err != nil || got.meta.Origin != logs.OriginController {
			t.Fatalf("after removal: %+v %v", got, err)
		}
		if _, err := r.svc.Open(bg, r.req(a, func(q *logs.Request) { q.Mode = logs.ModeWrapper })); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a removed wrapper log has no bytes: %v", err)
		}
	})

	t.Run("known secrets never reach the reader and an unknown set fails closed", func(t *testing.T) {
		r := newRig(t, limits())
		a := r.running(t, lt.ScopeA, "token=hunter2\nplain\n")
		r.m.Redact.Set("hunter2")
		got, err := read(t, r.svc, r.req(a, nil))
		if err != nil || strings.Contains(strings.Join(got.lines, "|"), "hunter2") || got.lines[0] != "token="+logs.Marker {
			t.Fatalf("live: %v %v", got.lines, err)
		}
		fin := r.m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Terminal: true})
		r.m.WrapperLog(t, fin, "password hunter2 leaked\n", contract.Complete)
		got, err = read(t, r.svc, r.req(fin, nil))
		if err != nil || strings.Contains(got.lines[0], "hunter2") {
			t.Fatalf("retained: %v %v", got.lines, err)
		}
		_, before := r.m.Live.Queries()
		r.m.Redact.Fail(errors.New("secrets: unavailable"))
		if _, err := r.svc.Open(bg, r.req(a, nil)); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("an unknown redaction set: %v", err)
		}
		if _, after := r.m.Live.Queries(); len(after) != len(before) {
			t.Fatal("a log was opened before the redaction set was known")
		}
	})
}

// Test_unit_log_fallback proves the controller's capture replays after
// interruption and keeps explicit evidence (PX-05, AC-06).
func Test_unit_log_fallback(t *testing.T) {
	collect := func(t *testing.T) (*lt.Memory, lt.MemoryAttempt, *logs.Controller) {
		m := lt.NewMemory()
		a := m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Observed: true, Terminal: true})
		m.Live.Set(a.Job, lt.JobUID, numbered(4))
		return m, a, m.Controller(t, 0)
	}

	t.Run("a repeated capture is the same retained record", func(t *testing.T) {
		m, a, c := collect(t)
		first, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || first.State != logs.StateRetained || first.Link == nil || first.Link.Meta == nil || first.Link.Completeness != logs.Unknown {
			t.Fatalf("%+v %v", first, err)
		}
		if first.Link.Reasons[0] != contract.ReasonControllerCapture {
			t.Fatalf("a controller capture states its source: %+v", first.Link)
		}
		again, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || again.ID != first.ID || again.Version != first.Version {
			t.Fatalf("a repeat changes nothing: %+v %v", again, err)
		}
		var actions []string
		for _, e := range m.Audit(t, a.Scope, first.ID) {
			actions = append(actions, e.Action)
			for k, v := range e.Metadata {
				if s := fmt.Sprint(v); strings.Contains(s, "sha256:") || strings.Contains(s, "line 0") {
					t.Errorf("audit %s carries content in %s: %s", e.Action, k, s)
				}
			}
		}
		if strings.Join(actions, ",") != "log.created,log.collecting,log.frozen,log.retained" {
			t.Fatalf("audit trail: %v", actions)
		}
	})

	t.Run("a store failure keeps the frozen snapshot and the retry finishes the same capture", func(t *testing.T) {
		m, a, c := collect(t)
		m.FailNextStoreAfterUpload(errors.New("s3: connection reset"))
		rec, err := c.Collect(bg, a.Scope, a.ID)
		if !errors.Is(err, logs.ErrUnavailable) || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureStore || rec.Snapshot == nil {
			t.Fatalf("%+v %v", rec, err)
		}
		done, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || done.State != logs.StateRetained || done.ID != rec.ID || done.Generation != 1 || done.Snapshot.Digest != rec.Snapshot.Digest {
			t.Fatalf("the retry uploads the same declaration: %+v %v", done, err)
		}
	})

	t.Run("lost frozen bytes start a bounded new generation and keep the old failure", func(t *testing.T) {
		m, a, c := collect(t)
		m.FailNextStoreAfterUpload(errors.New("s3: connection reset"))
		if _, err := c.Collect(bg, a.Scope, a.ID); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatal(err)
		}
		m.LoseStagedBytes() // the process restarted and the staged object is gone
		done, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || done.State != logs.StateRetained || done.Generation != 2 {
			t.Fatalf("%+v %v", done, err)
		}
		all, _ := m.Store().ForAttempt(bg, a.Scope, a.ID)
		if len(all) != 2 || all[0].State != logs.StateUnavailable || all[0].Failure != logs.FailureFrozenLost || all[1].Generation != 2 {
			t.Fatalf("the earlier failure stays recorded: %+v", all)
		}
		if got := logs.Select(all); got == nil || got.ID != done.ID {
			t.Fatalf("selection skips the unavailable generation: %+v", got)
		}
	})

	t.Run("a failure before anything was uploaded starts a new generation too", func(t *testing.T) {
		m, a, c := collect(t)
		m.FailNextStore(errors.New("s3: connection reset"))
		if _, err := c.Collect(bg, a.Scope, a.ID); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatal(err)
		}
		done, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || done.State != logs.StateRetained || done.Generation != 2 {
			t.Fatalf("nothing was uploaded, so the frozen bytes cannot be recovered: %+v %v", done, err)
		}
	})

	t.Run("a missing workload is explicit unavailable evidence, not an error", func(t *testing.T) {
		m, a, c := collect(t)
		m.Live.Remove(a.Job)
		rec, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureWorkloadMissing {
			t.Fatalf("%+v %v", rec, err)
		}
		if again, err := c.Collect(bg, a.Scope, a.ID); err != nil || again.Version != rec.Version {
			t.Fatalf("a non-retryable failure is not retried: %+v %v", again, err)
		}
	})

	t.Run("a capture over the bound is retained as partial", func(t *testing.T) {
		m := lt.NewMemory()
		a := m.NewAttempt(t, lt.ScopeA, lt.AttemptOptions{Observed: true, Terminal: true})
		m.Live.Set(a.Job, lt.JobUID, numbered(50))
		rec, err := m.Controller(t, 64).Collect(bg, a.Scope, a.ID)
		if err != nil || rec.Link == nil || rec.Link.Completeness != logs.Partial || rec.Link.Log.Size != 64 {
			t.Fatalf("%+v %v", rec, err)
		}
		var truncated bool
		for _, r := range rec.Link.Reasons {
			truncated = truncated || r == contract.ReasonTruncated
		}
		if !truncated {
			t.Fatalf("truncation is stated: %+v", rec.Link.Reasons)
		}
	})

	t.Run("redaction is applied before bytes are retained", func(t *testing.T) {
		m, a, c := collect(t)
		m.Live.Set(a.Job, lt.JobUID, "key=hunter2\n")
		m.Redact.Set("hunter2")
		rec, err := c.Collect(bg, a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := m.Retained().Open(bg, a.Scope, a.ID, rec.Link.Log)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 64)
		n, _ := rc.Read(b)
		rc.Close()
		if strings.Contains(string(b[:n]), "hunter2") {
			t.Fatalf("a secret was retained: %q", b[:n])
		}
		m2, a2, c2 := collect(t)
		m2.Redact.Fail(errors.New("secrets: down"))
		if _, err := c2.Collect(bg, a2.Scope, a2.ID); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("unredacted bytes must never be captured: %v", err)
		}
	})

	t.Run("observation and capture respect the tenant boundary and replay", func(t *testing.T) {
		m, a, c := collect(t)
		if _, err := c.Observe(bg, a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("no wrapper log: %v", err)
		}
		m.WrapperLog(t, a, "wrapped\n", contract.Complete)
		first, err := c.Observe(bg, a.Scope, a.ID)
		if err != nil || first.State != logs.StateRetained || first.Link.Completeness != logs.Complete {
			t.Fatalf("%+v %v", first, err)
		}
		if again, err := c.Observe(bg, a.Scope, a.ID); err != nil || again.Version != first.Version {
			t.Fatalf("%+v %v", again, err)
		}
		for name, scope := range map[string]domain.Scope{"another tenant": lt.ScopeB, "another project": lt.ScopeA2} {
			if _, err := c.Collect(bg, scope, a.ID); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("collect %s: %v", name, err)
			}
			if _, err := c.Observe(bg, scope, a.ID); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("observe %s: %v", name, err)
			}
			if err := c.Reconcile(bg, scope, "att_invalid!"); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("reconcile %s: %v", name, err)
			}
		}
	})

	t.Run("a failed store commit leaves no record behind", func(t *testing.T) {
		m, a, c := collect(t)
		m.FailNextCommits(1)
		if _, err := c.Collect(bg, a.Scope, a.ID); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if recs, _ := m.Store().ForAttempt(bg, a.Scope, a.ID); len(recs) != 0 {
			t.Fatalf("a rolled-back ensure left %d records", len(recs))
		}
		if rec, err := c.Collect(bg, a.Scope, a.ID); err != nil || rec.State != logs.StateRetained {
			t.Fatalf("%+v %v", rec, err)
		}
	})
}

type protection struct {
	protected map[domain.ArtifactID]bool
	err       error
}

func (p protection) Protected(_ context.Context, _ domain.Scope, id domain.ArtifactID) (bool, error) {
	return p.protected[id], p.err
}

// Test_unit_log_retention proves retained-log eligibility fails closed.
func Test_unit_log_retention(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if _, err := logs.NewRetentionPolicy(0, nil); !errors.Is(err, logs.ErrProtectionUnknown) {
		t.Fatalf("no protection policy, no deletion: %v", err)
	}
	policy, err := logs.NewRetentionPolicy(0, protection{protected: map[domain.ArtifactID]bool{"art_held": true}})
	if err != nil || policy.Window != logs.RetentionWindow {
		t.Fatalf("%+v %v", policy, err)
	}
	verified := now.Add(-logs.RetentionWindow)
	for name, c := range map[string]struct {
		id   domain.ArtifactID
		at   time.Time
		want bool
	}{
		"at the window":       {"art_old", verified, true},
		"before the window":   {"art_old", verified.Add(time.Second), false},
		"protected":           {"art_held", verified, false},
		"no verified instant": {"art_old", time.Time{}, false},
	} {
		got, err := policy.Eligible(bg, lt.ScopeA, c.id, c.at, now)
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	failing, _ := logs.NewRetentionPolicy(time.Hour, protection{err: errors.New("down")})
	if got, err := failing.Eligible(bg, lt.ScopeA, "art_old", now.Add(-2*time.Hour), now); got || !errors.Is(err, logs.ErrProtectionUnknown) {
		t.Fatalf("an unknown protection state is never eligible: %v %v", got, err)
	}
	if got, err := (logs.RetentionPolicy{}).Eligible(bg, lt.ScopeA, "art_old", verified, now); got || !errors.Is(err, logs.ErrProtectionUnknown) {
		t.Fatalf("a zero policy: %v %v", got, err)
	}
}
