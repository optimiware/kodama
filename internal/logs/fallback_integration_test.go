package logs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	logskube "github.com/optimiware/kodama/internal/logs/kubernetes"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/internal/logs/logstest/kubefake"
)

func bg() context.Context { return context.Background() }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// attemptState is what execution recorded about the attempt; log collection
// must never change it.
func attemptState(t *testing.T, e *logstest.Env, a logstest.Attempt) (execution.AttemptStatus, domain.Version) {
	t.Helper()
	agg, err := e.Store.Get(bg(), a.Scope, a.Run)
	if err != nil {
		t.Fatal(err)
	}
	at := agg.Attempt(a.ID)
	return at.Status, at.Version
}

func controllerArtifacts(t *testing.T, e *logstest.Env, a logstest.Attempt) int {
	t.Helper()
	return e.Count(`SELECT count(*) FROM artifacts WHERE attempt_id = $1 AND name LIKE 'controller-g%'`, string(a.ID))
}

func readArtifact(t *testing.T, e *logstest.Env, a logstest.Attempt, ref logs.ArtifactRef) string {
	t.Helper()
	rc, err := e.Reader.Open(bg(), a.Scope, a.ID, ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref.ID, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sidecarOf(t *testing.T, e *logstest.Env, a logstest.Attempt, ref logs.ArtifactRef) contract.Metadata {
	t.Helper()
	m, err := contract.Decode([]byte(readArtifact(t, e, a, ref)))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	return m
}

// TestFallbackRecovery: duplicate collection, upload and commit response loss,
// restart, stale lease fencing and lost frozen bytes converge or stay explicitly
// incomplete, without changing attempt outcomes. Real PostgreSQL and S3; the
// Kubernetes side is a controlled fake (the real reader is covered against the
// Kubernetes API fake in TestControllerWithRealReader).
func TestFallbackRecovery(t *testing.T) {
	e := logstest.New(t)

	t.Run("collects, links and leaves the attempt untouched", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		statusBefore, versionBefore := attemptState(t, e, a)
		live := &logstest.FakeLive{Body: "controller line 1\ncontroller line 2\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State != logs.StateRetained || rec.Source != logs.SourceController || rec.Generation != 1 || rec.Link == nil || rec.Link.Meta == nil {
			t.Fatalf("record = %+v", rec)
		}
		if got := readArtifact(t, e, a, rec.Link.Log); got != live.Body {
			t.Fatalf("retained bytes = %q", got)
		}
		doc := sidecarOf(t, e, a, *rec.Link.Meta)
		if doc.Source != contract.SourceController || doc.Completeness == contract.Complete || doc.Bind(rec.Link.Log.ID, rec.Link.Log.Digest, rec.Link.Log.Size) != nil {
			t.Fatalf("controller EOF alone must not establish complete capture: %+v", doc)
		}
		if rec.Link.Completeness != logs.Unknown {
			t.Fatalf("completeness = %s", rec.Link.Completeness)
		}
		tgt := live.Targets()[0]
		if tgt.Namespace != logstest.Namespace || tgt.JobName != a.Job || tgt.JobUID != logstest.JobUID || tgt.Container != logstest.Container {
			t.Fatalf("target = %+v", tgt)
		}
		status, version := attemptState(t, e, a)
		if status != statusBefore || version != versionBefore {
			t.Fatalf("log collection changed the attempt: %s/%d -> %s/%d", statusBefore, versionBefore, status, version)
		}
		// The record's audit entries carry no content.
		if n := e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1`, string(rec.ID)); n < 4 {
			t.Fatalf("audit entries = %d", n)
		}
	})

	t.Run("duplicate and concurrent collection converge on one capture", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "once\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		var wg sync.WaitGroup
		results := make([]*logs.Record, 8)
		errs := make([]error, 8)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = c.Collect(bg(), a.Scope, a.ID)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil && !errors.Is(err, logs.ErrBusy) {
				t.Errorf("collector %d: %v", i, err)
			}
		}
		again, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil || again.State != logs.StateRetained {
			t.Fatalf("a later collection returns the retained capture: %+v %v", again, err)
		}
		if n := controllerArtifacts(t, e, a); n != 2 {
			t.Fatalf("one log and one sidecar, got %d artifacts", n)
		}
		if live.Opened() != 1 {
			t.Fatalf("the workload log was read %d times", live.Opened())
		}
	})

	t.Run("a lost upload response is reconciled with the same frozen snapshot", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "uploaded but unacknowledged\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		var failOnce sync.Once
		e.FaultObjects.Set(func(f *artifactstestFaultObjects) {
			f.OnPut = func(spec artifacts.PutSpec, next func() error) error {
				if !contains(spec.Key, "/staging/") {
					return next()
				}
				var err error
				failOnce.Do(func() {
					if perr := next(); perr != nil { // the object lands, the response is lost
						t.Errorf("put: %v", perr)
					}
					err = errors.New("connection reset by peer")
				})
				if err != nil {
					return err
				}
				return next()
			}
		})
		t.Cleanup(func() { e.FaultObjects.Set(func(f *artifactstestFaultObjects) { f.OnPut = nil }) })

		rec, err := c.Collect(bg(), a.Scope, a.ID)
		// The in-call recovery finds the staged object and completes.
		if err != nil || rec.State != logs.StateRetained {
			t.Fatalf("collect after a lost upload response: %+v %v", rec, err)
		}
		if live.Opened() != 1 {
			t.Fatalf("the live log must not be read again: %d", live.Opened())
		}
		if n := controllerArtifacts(t, e, a); n != 2 {
			t.Fatalf("artifacts = %d", n)
		}
		if got := readArtifact(t, e, a, rec.Link.Log); got != live.Body {
			t.Fatalf("bytes = %q", got)
		}
	})

	t.Run("a lost commit response never reports failure for durable success", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "committed but unacknowledged\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		var once sync.Once
		e.FaultRepo.Set(func(f *artifactstestFaultRepo) {
			f.OnCommit = func(cmd artifacts.CommitCommand, next func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
				var lost bool
				once.Do(func() { lost = true })
				art, err := next()
				if lost && err == nil {
					return artifacts.Artifact{}, errors.New("response lost after commit")
				}
				return art, err
			}
		})
		t.Cleanup(func() { e.FaultRepo.Set(func(f *artifactstestFaultRepo) { f.OnCommit = nil }) })
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.State != logs.StateRetained {
			t.Fatalf("%+v %v", rec, err)
		}
		if n := controllerArtifacts(t, e, a); n != 2 {
			t.Fatalf("artifacts = %d", n)
		}
	})

	t.Run("a lost reservation response is recovered by name", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "reserved but unacknowledged\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		var once sync.Once
		e.FaultRepo.Set(func(f *artifactstestFaultRepo) {
			f.OnReserve = func(cmd artifacts.ReserveCommand, next func() (artifacts.ReserveResult, error)) (artifacts.ReserveResult, error) {
				res, err := next()
				var lost bool
				once.Do(func() { lost = true })
				if lost && err == nil {
					return artifacts.ReserveResult{}, errors.New("response lost after reservation")
				}
				return res, err
			}
		})
		t.Cleanup(func() { e.FaultRepo.Set(func(f *artifactstestFaultRepo) { f.OnReserve = nil }) })
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if err == nil {
			t.Fatalf("an unknown reservation outcome is never success: %+v", rec)
		}
		if rec.State != logs.StateUnavailable || rec.Failure != logs.FailureStore || rec.Snapshot == nil {
			t.Fatalf("the failure is explicit and retryable: %+v", rec)
		}
		rec, err = c.Collect(bg(), a.Scope, a.ID)
		// The retry finds the reservation but not the bytes (they only lived in
		// the failed call's spool): that capture is fenced and a new generation
		// recaptures.
		if err != nil || rec.Generation != 2 || rec.State != logs.StateRetained {
			t.Fatalf("expected generation 2 retained: %+v %v", rec, err)
		}
		recs, _ := e.Logs.ForAttempt(bg(), a.Scope, a.ID)
		if len(recs) != 2 || recs[0].Failure != logs.FailureFrozenLost || recs[0].State != logs.StateUnavailable || recs[0].Snapshot == nil {
			t.Fatalf("the earlier failure and snapshot stay recorded: %+v", recs)
		}
	})

	t.Run("restart after the upload completes from the staged bytes", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "survives a restart\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		// The first process dies after the bytes reached staging: its sealing
		// hangs forever (never returns).
		hang := make(chan struct{})
		var begun sync.Once
		started := make(chan struct{})
		e.FaultRepo.Set(func(f *artifactstestFaultRepo) {
			f.OnBegin = func(cmd artifacts.BeginCompletion, next func() (artifacts.ClaimResult, error)) (artifacts.ClaimResult, error) {
				begun.Do(func() { close(started) })
				<-hang
				return artifacts.ClaimResult{}, errors.New("process died")
			}
		})
		t.Cleanup(func() { e.FaultRepo.Set(func(f *artifactstestFaultRepo) { f.OnBegin = nil }) })
		done := make(chan error, 1)
		go func() {
			_, err := c.Collect(bg(), a.Scope, a.ID)
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("the first collector never reached sealing")
		}
		e.FaultRepo.Set(func(f *artifactstestFaultRepo) { f.OnBegin = nil })
		// Its lease lapses; a restarted controller (fresh process, no spool) takes over.
		e.Clock.Advance(time.Minute)
		c2 := e.Controller(live, logstest.ControllerOptions{})
		rec, err := c2.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.State != logs.StateRetained || rec.Generation != 1 {
			t.Fatalf("the restarted collector must finish generation 1 from the staged bytes: %+v %v", rec, err)
		}
		if live.Opened() != 1 {
			t.Fatalf("no recapture was needed: %d", live.Opened())
		}
		if rec.Claims != 2 {
			t.Fatalf("the takeover is a second claim: %d", rec.Claims)
		}
		// The old collector wakes up: it must not change the committed record.
		close(hang)
		if err := <-done; err == nil {
			t.Fatal("a fenced-out collector must not report success")
		}
		got, _ := e.Logs.Get(bg(), a.Scope, rec.ID)
		if got.Link == nil || got.Link.Log != rec.Link.Log || got.Version != rec.Version {
			t.Fatalf("the stale collector changed the capture: %+v vs %+v", got, rec)
		}
		if n := controllerArtifacts(t, e, a); n != 2 {
			t.Fatalf("artifacts = %d", n)
		}
	})

	t.Run("lost frozen bytes fence the capture and a bounded new generation is explicit", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: "never staged\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		// Every staging write fails before landing, so the bytes only ever live in
		// the (lost) spool.
		e.FaultObjects.Set(func(f *artifactstestFaultObjects) {
			f.OnPut = func(spec artifacts.PutSpec, next func() error) error {
				if contains(spec.Key, "/staging/") {
					return errors.New("object store unreachable")
				}
				return next()
			}
		})
		t.Cleanup(func() { e.FaultObjects.Set(func(f *artifactstestFaultObjects) { f.OnPut = nil }) })
		var last *logs.Record
		var lastErr error
		for i := 0; i < 6; i++ {
			last, lastErr = c.Collect(bg(), a.Scope, a.ID)
			if errors.Is(lastErr, logs.ErrGenerationsExhausted) {
				break
			}
		}
		if !errors.Is(lastErr, logs.ErrGenerationsExhausted) {
			t.Fatalf("generations are bounded: %+v %v", last, lastErr)
		}
		recs, _ := e.Logs.ForAttempt(bg(), a.Scope, a.ID)
		if len(recs) != logs.MaxGenerations {
			t.Fatalf("records = %d, want %d", len(recs), logs.MaxGenerations)
		}
		for _, r := range recs {
			if r.State != logs.StateUnavailable || r.Failure != logs.FailureFrozenLost {
				t.Fatalf("every generation keeps its failure: %+v", r)
			}
		}
		if logs.Select(recs) != nil {
			t.Fatal("nothing is served: no evidence was fabricated")
		}
		// The storage recovers: nothing more is attempted automatically.
		e.FaultObjects.Set(func(f *artifactstestFaultObjects) { f.OnPut = nil })
		if r, err := c.Collect(bg(), a.Scope, a.ID); !errors.Is(err, logs.ErrGenerationsExhausted) || r.State == logs.StateRetained {
			t.Fatalf("an exhausted capture stays explicitly unavailable: %+v %v", r, err)
		}
	})

	t.Run("a missing workload is explicit unavailable evidence", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		statusBefore, versionBefore := attemptState(t, e, a)
		live := &logstest.FakeLive{Err: logs.ErrWorkloadMissing}
		c := e.Controller(live, logstest.ControllerOptions{})
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureWorkloadMissing || rec.Link != nil {
			t.Fatalf("%+v %v", rec, err)
		}
		if controllerArtifacts(t, e, a) != 0 {
			t.Fatal("no artifact may be fabricated for a missing workload")
		}
		again, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil || again.Failure != logs.FailureWorkloadMissing || again.Claims != 1 || len(live.Queries()) != 1 {
			t.Fatalf("a missing workload is not retried: %+v %v", again, err)
		}
		status, version := attemptState(t, e, a)
		if status != statusBefore || version != versionBefore {
			t.Fatal("a missing workload must not change the attempt's result")
		}
	})

	t.Run("an attempt whose Job was never observed has no workload to read", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, false)
		live := &logstest.FakeLive{Body: "must not be read\n"}
		c := e.Controller(live, logstest.ControllerOptions{})
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if !errors.Is(err, logs.ErrWorkloadNotObserved) || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureBackend {
			t.Fatalf("a running attempt may still start: retryable, %+v %v", rec, err)
		}
		if len(live.Queries()) != 0 {
			t.Fatal("no similarly named workload may be selected")
		}
		e.Finish(a)
		rec, err = c.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.Failure != logs.FailureWorkloadMissing {
			t.Fatalf("a finished attempt that never started has no log: %+v %v", rec, err)
		}
	})

	t.Run("a Kubernetes outage is retryable", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Err: logs.Unavailablef(nil, "api")}
		c := e.Controller(live, logstest.ControllerOptions{})
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if !errors.Is(err, logs.ErrUnavailable) || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureBackend {
			t.Fatalf("%+v %v", rec, err)
		}
		live.Err, live.Body = nil, "recovered\n"
		rec, err = c.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.State != logs.StateRetained || rec.Claims != 2 {
			t.Fatalf("%+v %v", rec, err)
		}
	})

	t.Run("a truncated or interrupted capture is partial, never complete", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		live := &logstest.FakeLive{Body: strings.Repeat("0123456789", 100)}
		c := e.Controller(live, logstest.ControllerOptions{CaptureBytes: 100})
		rec, err := c.Collect(bg(), a.Scope, a.ID)
		if err != nil || rec.Link.Completeness != logs.Partial || rec.Link.Log.Size != 100 {
			t.Fatalf("truncated: %+v %v", rec, err)
		}
		doc := sidecarOf(t, e, a, *rec.Link.Meta)
		if !hasReason(doc.Reasons, contract.ReasonTruncated) || !hasReason(doc.Reasons, contract.ReasonControllerCapture) {
			t.Fatalf("reasons = %v", doc.Reasons)
		}

		b := e.NewAttempt(logstest.ScopeA, true)
		live2 := &logstest.FakeLive{Body: strings.Repeat("abcdefghij", 50), FailAt: 200, FailErr: io.ErrUnexpectedEOF}
		rec, err = e.Controller(live2, logstest.ControllerOptions{}).Collect(bg(), b.Scope, b.ID)
		if err != nil || rec.Link.Completeness != logs.Partial || rec.Link.Log.Size != 200 {
			t.Fatalf("interrupted: %+v %v", rec, err)
		}
		if !hasReason(sidecarOf(t, e, b, *rec.Link.Meta).Reasons, contract.ReasonInterrupted) {
			t.Fatal("an interruption must be recorded")
		}
	})

	t.Run("known values are redacted before anything is stored", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		e.Redact.Set("hunter2-token")
		t.Cleanup(func() { e.Redact.Set() })
		live := &logstest.FakeLive{Body: "login ok with hunter2-token and again hunter2-token\n"}
		rec, err := e.Controller(live, logstest.ControllerOptions{}).Collect(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := readArtifact(t, e, a, rec.Link.Log)
		if contains(got, "hunter2") || got != "login ok with [REDACTED] and again [REDACTED]\n" {
			t.Fatalf("stored %q", got)
		}
		// When the values cannot be established nothing is captured.
		b := e.NewAttempt(logstest.ScopeA, true)
		e.Redact.Fail(errors.New("secret store down"))
		rec, err = e.Controller(live, logstest.ControllerOptions{}).Collect(bg(), b.Scope, b.ID)
		if err == nil || rec.State != logs.StateUnavailable || controllerArtifacts(t, e, b) != 0 {
			t.Fatalf("an unredacted capture must never be stored: %+v %v", rec, err)
		}
		e.Redact.Fail(nil)
	})

	t.Run("wrapper observation is idempotent and refuses changed evidence", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		c := e.Controller(&logstest.FakeLive{}, logstest.ControllerOptions{})
		if _, err := c.Observe(bg(), a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("no wrapper log yet: %v", err)
		}
		e.WrapperLog(a, "wrapper output\n", contract.Complete)
		first, err := c.Observe(bg(), a.Scope, a.ID)
		if err != nil || first.State != logs.StateRetained || first.Link.Completeness != logs.Complete {
			t.Fatalf("%+v %v", first, err)
		}
		second, err := c.Observe(bg(), a.Scope, a.ID)
		if err != nil || second.Version != first.Version {
			t.Fatalf("an exact duplicate observation changes nothing: %+v %v", second, err)
		}
		// A sidecar appearing later with a different statement cannot upgrade or
		// replace the recorded evidence: the record stays as first observed.
		legacy := e.NewAttempt(logstest.ScopeA, true)
		e.LegacyWrapperLog(legacy, "legacy\n")
		l1, err := c.Observe(bg(), legacy.Scope, legacy.ID)
		if err != nil || l1.Link.Completeness != logs.Unknown {
			t.Fatalf("%+v %v", l1, err)
		}
		metaName, _ := contract.MetadataName(contract.CommandLogName)
		art, _ := e.FaultRepo.FindByName(bg(), legacy.Scope, legacy.ID, contract.CommandLogName)
		size := art.Reservation().Size
		doc, _ := contract.Encode(contract.Metadata{Version: 1, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
			Log:          contract.Ref{ArtifactID: art.ID(), Digest: string(art.Digest()), SizeBytes: *size},
			Completeness: contract.Complete})
		e.WrapperUpload(legacy, metaName, contract.MediaType, doc)
		if _, err := c.Observe(bg(), legacy.Scope, legacy.ID); !errors.Is(err, logs.ErrConflict) {
			t.Fatalf("recorded evidence is immutable: %v", err)
		}
		got, _ := e.Logs.Get(bg(), legacy.Scope, l1.ID)
		if got.Link.Completeness != logs.Unknown {
			t.Fatal("completeness was upgraded after the fact")
		}
	})

	t.Run("removal changes availability, not recorded completeness", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		id := e.WrapperLog(a, "to be removed\n", contract.Complete)
		c := e.Controller(&logstest.FakeLive{}, logstest.ControllerOptions{})
		rec, err := c.Observe(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Reconcile(bg(), a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.Logs.Get(bg(), a.Scope, rec.ID); !got.Available() {
			t.Fatal("usable content stays available")
		}
		e.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(id))
		if err := c.Reconcile(bg(), a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := e.Logs.Get(bg(), a.Scope, rec.ID)
		if got.Available() || got.RemovedAt == nil || got.Link.Completeness != logs.Complete {
			t.Fatalf("%+v", got)
		}
		if logs.Select([]*logs.Record{got}) != nil {
			t.Fatal("removed content is not served")
		}
	})
}

func hasReason(rs []contract.Reason, want contract.Reason) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// TestControllerWithRealReader exercises the fallback through the real
// Kubernetes log reader against the API fake: the trusted workload is verified
// before reading, and each failure of the workload or the stream is recorded as
// explicit evidence.
func TestControllerWithRealReader(t *testing.T) {
	e := logstest.New(t)
	collect := func(t *testing.T, mut func(k *kubefake.API), opts logstest.ControllerOptions) (logstest.Attempt, *kubefake.API, *logs.Record, error) {
		t.Helper()
		a := e.NewAttempt(logstest.ScopeA, true)
		k := kubefake.New(t, logstest.Namespace, a.Job, logstest.JobUID, logstest.Container)
		if mut != nil {
			k.Set(mut)
		}
		reader, err := logskube.NewReader(logskube.Config{Endpoint: k.URL(), Client: k.Client(), AllowInsecure: true})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := e.Controller(reader, opts).Collect(bg(), a.Scope, a.ID)
		return a, k, rec, err
	}

	t.Run("captures the verified workload's log within bounds", func(t *testing.T) {
		a, k, rec, err := collect(t, func(k *kubefake.API) { k.LogBody = "real line 1\nreal line 2\n" }, logstest.ControllerOptions{CaptureBytes: 4096})
		if err != nil || rec.State != logs.StateRetained {
			t.Fatalf("%+v %v", rec, err)
		}
		if got := readArtifact(t, e, a, rec.Link.Log); got != "real line 1\nreal line 2\n" {
			t.Fatalf("bytes = %q", got)
		}
		reqs := k.Requests()
		if len(reqs) != 3 || !contains(reqs[2], "limitBytes=4097") || !contains(reqs[2], "container=step") || contains(reqs[2], "follow") || contains(reqs[2], "tailLines") {
			t.Fatalf("requests = %v", reqs)
		}
	})

	t.Run("output beyond the capture bound is partial, not complete", func(t *testing.T) {
		_, _, rec, err := collect(t, func(k *kubefake.API) { k.LogBody = strings.Repeat("0123456789", 100) }, logstest.ControllerOptions{CaptureBytes: 100})
		if err != nil || rec.Link.Completeness != logs.Partial || rec.Link.Log.Size != 100 {
			t.Fatalf("%+v %v", rec, err)
		}
	})

	t.Run("a Job that is gone or was recreated is recorded, never read", func(t *testing.T) {
		for name, mut := range map[string]func(k *kubefake.API){
			"job gone": func(k *kubefake.API) { k.Job = nil },
			"job recreated (new uid)": func(k *kubefake.API) {
				k.Job["metadata"].(map[string]any)["uid"] = "99999999-2222-3333-4444-555555555555"
			},
			"no pod": func(k *kubefake.API) { k.Pods = nil },
		} {
			a, k, rec, err := collect(t, mut, logstest.ControllerOptions{})
			if err != nil || rec.State != logs.StateUnavailable || rec.Failure != logs.FailureWorkloadMissing {
				t.Fatalf("%s: %+v %v", name, rec, err)
			}
			if controllerArtifacts(t, e, a) != 0 || len(k.LogRequests()) != 0 {
				t.Fatalf("%s: nothing may be captured from an unverified workload", name)
			}
		}
	})

	t.Run("a kubelet failure is retryable and a mid-stream cut is partial evidence", func(t *testing.T) {
		_, _, rec, err := collect(t, func(k *kubefake.API) { k.LogCode = http.StatusInternalServerError }, logstest.ControllerOptions{})
		if !errors.Is(err, logs.ErrUnavailable) || rec.Failure != logs.FailureBackend || contains(err.Error(), "credentials") {
			t.Fatalf("%+v %v", rec, err)
		}
		a, _, rec, err := collect(t, func(k *kubefake.API) {
			k.LogHandler = func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "before the cut\n")
				w.(http.Flusher).Flush()
				conn, _, herr := w.(http.Hijacker).Hijack()
				if herr == nil {
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
					conn.Close()
				}
			}
		}, logstest.ControllerOptions{})
		if err != nil || rec.Link == nil || rec.Link.Completeness != logs.Partial {
			t.Fatalf("%+v %v", rec, err)
		}
		if got := readArtifact(t, e, a, rec.Link.Log); got != "before the cut\n" {
			t.Fatalf("bytes = %q", got)
		}
		if !hasReason(sidecarOf(t, e, a, *rec.Link.Meta).Reasons, contract.ReasonInterrupted) {
			t.Fatal("the interruption must be recorded")
		}
	})
}
