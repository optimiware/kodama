package logs_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// Test_missing_node_tail (PX-10, AC-06, IT-03, IT-11): when the node (or the
// workload's log transport) goes away the platform produces explicit incomplete
// evidence, never a fabricated complete log. Node loss is fault-injected at the
// selected real Pod's log transport (a connection reset after real mirrored
// bytes, or an unavailable backend) and by removing the fixture-owned Job and
// Pods; no kind node is stopped or deleted.
func Test_missing_node_tail(t *testing.T) {
	s := newStack(t)
	k := s.kit

	t.Run("interrupted_tail_is_partial_and_survives_restart", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: numbered("mnt", 400) + "sleep 240\n" + finishOK + "\n"})
		s.waitRunning(a, 5*time.Minute)
		s.waitRawLog(a, "mnt-line-0400", 2*time.Minute)
		raw := s.rawLog(a)
		before := s.snapshot(a)
		delivered := s.deliveredOf(a)

		// The real response is interrupted after real mirrored bytes.
		const cut = 2500
		k.Live.CutAfter = func(logs.Target) int { return cut }
		t.Cleanup(k.Live.Reset)

		// A live reader sees the interruption as a classified stop, not as success.
		live, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive, Tail: ptr(400)})
		if err != nil {
			t.Fatalf("live read: %v", err)
		}
		if live.Summary == nil || live.Summary.Stop != logs.StopInterrupted || len(live.Lines) == 0 || live.Meta.Completeness != logs.Unknown {
			t.Fatalf("an interrupted live read must say interrupted/unknown: meta %+v summary %+v lines %d", live.Meta, live.Summary, len(live.Lines))
		}
		res := k.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)+"&source=live&tail=400")
		if res.status != http.StatusOK || res.summary(t)["stop"] != "interrupted" {
			t.Fatalf("interrupted route read: %d %v", res.status, res.records[len(res.records)-1])
		}

		// The controller records what it really read: partial, interrupted, never complete.
		opens := k.Live.Opens()
		rec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceController)
		if k.Live.Opens() != opens+1 {
			t.Fatalf("one read of the workload log expected")
		}
		if rec.Link.Completeness != logs.Partial || !hasReason(rec.Link.Reasons, contract.ReasonInterrupted) || !hasReason(rec.Link.Reasons, contract.ReasonControllerCapture) {
			t.Fatalf("an interrupted capture is partial/interrupted: %+v", rec.Link)
		}
		doc := wantBoundSidecar(t, k, a, rec, contract.SourceController)
		if doc.Completeness != contract.Partial {
			t.Fatalf("sidecar: %+v", doc)
		}
		tail := k.readArtifact(a, rec.Link.Log)
		if len(tail) == 0 || len(tail) > cut || len(tail) >= len(raw) || !strings.HasPrefix(raw, tail) {
			t.Fatalf("the retained tail must be a strict prefix of the real mirrored bytes: %d of %d (cut %d)", len(tail), len(raw), cut)
		}
		noLeak(t, "the partial tail", tail, delivered...)
		if strings.Contains(tail, "mnt-line-0400") {
			t.Fatal("bytes after the interruption were fabricated into the capture")
		}
		if after := s.snapshot(a); after != before {
			t.Fatalf("log collection changed the attempt: %+v -> %+v", before, after)
		}

		// A restarted composition reads the same partial tail and does not read the
		// workload again or replace the evidence.
		k.Live.Reset()
		restarted := s.newLogs(logsOptions{})
		again, err := restarted.Ctrl.Collect(bg(), a.Scope, a.ID)
		if err != nil || again.ID != rec.ID || again.Version != rec.Version || again.Link.Log != rec.Link.Log || restarted.Live.Opens() != 0 {
			t.Fatalf("restart replaced or re-read the retained partial capture: %+v %v opens %d", again, err, restarted.Live.Opens())
		}
		got, err := readService(restarted.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeController, Tail: ptr(400)})
		if err != nil || got.Meta.Origin != logs.OriginController || got.Meta.Completeness != logs.Partial || !hasReason(got.Meta.Reasons, contract.ReasonInterrupted) {
			t.Fatalf("restarted read: %+v %v", got, err)
		}
		if !strings.HasPrefix(tail, strings.Join(got.Lines[:len(got.Lines)-1], "\n")) {
			t.Fatalf("the served tail is not the retained one")
		}

		// The workload itself disappears (a lost node's Job and Pods): the live
		// source is explicitly unavailable, and the partial tail stays the answer.
		s.removeWorkload(a)
		if _, err := readService(restarted.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive}); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("a live read of a removed workload must be explicit: %v", err)
		}
		auto, err := readService(restarted.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(400)})
		if err != nil || auto.Meta.Origin != logs.OriginController || auto.Meta.Completeness != logs.Partial {
			t.Fatalf("with the workload gone the retained partial tail is served as partial: %+v %v", auto.Meta, err)
		}
		if res := restarted.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)+"&source=live"); res.status == http.StatusOK {
			t.Fatalf("a removed workload produced a live success: %s", truncate(res.raw, 200))
		}
		// Execution notices the loss in its own time; the evidence does not change.
		s.until(5*time.Minute, "execution to settle the lost workload", func() (bool, string) {
			at := s.persisted(a)
			return at.Status.Terminal(), string(at.Status)
		})
		if at := s.persisted(a); at.Status == execution.AttemptSucceeded {
			t.Fatalf("a removed workload cannot have succeeded: %s", at.Status)
		}
		final, err := restarted.Ctrl.Collect(bg(), a.Scope, a.ID)
		if err != nil || final.ID != rec.ID || final.Version != rec.Version || final.Link.Completeness != logs.Partial {
			t.Fatalf("the partial tail is immutable evidence: %+v %v", final, err)
		}
	})

	t.Run("backend_unavailable_before_any_byte", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: chatter("mnt-b")}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
		s.waitTerminal(a, 5*time.Minute)
		before := s.snapshot(a)

		k.Live.FailOpen = func(logs.Target) error { return logs.ErrUnavailable }
		t.Cleanup(k.Live.Reset)
		rec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		if !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("collection with the backend down: %v", err)
		}
		if rec == nil || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureBackend || !rec.Failure.Retryable() || rec.Link != nil {
			t.Fatalf("unavailable evidence is explicit and retryable: %+v", rec)
		}
		if names := s.artifactNames(a, "controller-%"); len(names) != 0 {
			t.Fatalf("an unavailable backend must not produce artifacts: %v", names)
		}
		// Neither the service nor the route turns an outage into an empty success.
		if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive}); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("live read with the backend down: %v", err)
		}
		if res := k.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)+"&source=live"); res.status != http.StatusServiceUnavailable {
			t.Fatalf("route with the backend down: %d %s", res.status, truncate(res.raw, 200))
		}

		// The backend recovers: the same record is retried (a bounded number of times).
		k.Live.Reset()
		rec2, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		rec2 = wantRetained(t, rec2, err, logs.SourceController)
		if rec2.ID != rec.ID || rec2.Generation != 1 || rec2.Claims < 2 || rec2.Link.Completeness != logs.Unknown {
			t.Fatalf("recovery: %+v", rec2)
		}
		if after := s.snapshot(a); after != before {
			t.Fatalf("log collection changed the attempt: %+v -> %+v", before, after)
		}
	})

	t.Run("workload_missing_is_explicit_evidence_not_an_empty_log", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: chatter("mnt-c")}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
		s.waitTerminal(a, 5*time.Minute)
		before := s.snapshot(a)
		s.removeWorkload(a)

		opens := k.Live.Opens()
		rec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatalf("a missing workload is recorded evidence, not an error: %v", err)
		}
		if rec.State != logs.StateUnavailable || rec.Failure != logs.FailureWorkloadMissing || rec.Link != nil || rec.Failure.Retryable() {
			t.Fatalf("workload_missing must be explicit, unlinked and not retryable: %+v", rec)
		}
		if k.Live.Opens() != opens+1 {
			t.Fatalf("the (missing) workload is asked once")
		}
		if names := s.artifactNames(a, "controller-%"); len(names) != 0 {
			t.Fatalf("no artifact may be fabricated for a missing workload: %v", names)
		}
		again, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		if err != nil || again.ID != rec.ID || again.Claims != rec.Claims || again.State != logs.StateUnavailable || k.Live.Opens() != opens+1 {
			t.Fatalf("a non-retryable failure is not retried: %+v %v", again, err)
		}
		// Reads: no log at all, in every mode, with a classified answer.
		for _, mode := range []logs.Mode{logs.ModeAuto, logs.ModeController, logs.ModeWrapper} {
			if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: mode}); !errors.Is(err, logs.ErrNoLog) {
				t.Fatalf("%s read of an attempt with no evidence: %v", mode, err)
			}
		}
		if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive}); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("live read: %v", err)
		}
		if res := k.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)); res.status != http.StatusNotFound {
			t.Fatalf("route: %d %s", res.status, truncate(res.raw, 200))
		}
		// The unavailable record survives a restart.
		restarted := s.newLogs(logsOptions{})
		recs := restarted.records(a)
		if len(recs) != 1 || recs[0].Failure != logs.FailureWorkloadMissing || recs[0].State != logs.StateUnavailable {
			t.Fatalf("after restart: %+v", recs)
		}
		if after := s.snapshot(a); after != before {
			t.Fatalf("log collection changed the attempt: %+v -> %+v", before, after)
		}
	})

	t.Run("job_not_yet_observed", func(t *testing.T) {
		// A command that finishes before the reconciler persisted the Job's UID
		// leaves execution without an observed workload. The log module never
		// selects a similarly named object: it records explicit unavailable
		// evidence (or, if the observation did land first, a non-complete capture).
		a := s.start(scopeA, stepSpec{Script: chatter("mnt-d"), NoHold: true}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
		eventually(t, 5*time.Minute, "the attempt to finish", func() (bool, string) { // no reconciliation ticks: nothing observes the Job
			at := s.persisted(a)
			return at.Status.Terminal(), string(at.Status)
		})
		at := s.persisted(a)
		rec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		if at.WorkloadUID == "" {
			t.Logf("B-3: the attempt reached %s before execution persisted its Job UID; the controller cannot select the (still existing) workload", at.Status)
			if err != nil || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureWorkloadMissing || rec.Link != nil {
				t.Fatalf("an unobserved workload yields explicit unavailable evidence: %+v %v", rec, err)
			}
			if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive}); !errors.Is(err, logs.ErrWorkloadNotObserved) {
				t.Fatalf("live read of an unobserved workload: %v", err)
			}
			return
		}
		rec = wantRetained(t, rec, err, logs.SourceController)
		if rec.Link.Completeness == logs.Complete {
			t.Fatalf("a controller capture is never complete: %+v", rec.Link)
		}
	})
}
