package logs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/adapters"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

type protection struct {
	mu        sync.Mutex
	protected map[domain.ArtifactID]bool
	err       error
}

func (p *protection) Protected(_ context.Context, _ domain.Scope, id domain.ArtifactID) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.protected[id], p.err
}

func (p *protection) protect(id domain.ArtifactID) {
	p.mu.Lock()
	p.protected[id] = true
	p.mu.Unlock()
}

func (p *protection) fail(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

func newReconciler(t *testing.T, e *logstest.Env, ret artifacts.Retention) *artifacts.Reconciler {
	t.Helper()
	r, err := artifacts.NewReconciler(artifacts.ReconcilerConfig{Store: e.ArtStore, Objects: e.Objects, Retention: ret,
		Policy: artifacts.CleanupPolicy{StagingGrace: time.Second, AbandonAfter: time.Minute, DeletionLease: 5 * time.Second, Batch: 50}, Clock: e.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sweep(t *testing.T, r *artifacts.Reconciler) artifacts.SweepReport {
	t.Helper()
	var total artifacts.SweepReport
	for i := 0; i < 6; i++ {
		rep, err := r.Sweep(bg())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		total.Removed += rep.Removed
		total.Deleted += rep.Deleted
		if rep.Removed == 0 && rep.Deleted == 0 && rep.Retryable == 0 {
			break
		}
	}
	return total
}

func disposition(t *testing.T, e *logstest.Env, id domain.ArtifactID) string {
	t.Helper()
	var d string
	e.Scalar(&d, `SELECT disposition FROM artifacts WHERE id = $1`, string(id))
	return d
}

// TestLogRetention: the 30-day window applies to logs and their sidecars, an
// active pin or protection extends retention, concurrent pins never lose to a
// deletion, and a missing or failing protection policy deletes nothing. Real
// PostgreSQL and S3.
func TestLogRetention(t *testing.T) {
	e := logstest.New(t)
	prot := &protection{protected: map[domain.ArtifactID]bool{}}
	ret, err := adapters.NewRetention(0, prot)
	if err != nil {
		t.Fatal(err)
	}
	rec := newReconciler(t, e, ret)

	// A finished attempt with a wrapper log and sidecar, a controller capture
	// and an unrelated output artifact.
	a := e.NewAttempt(logstest.ScopeA, true)
	wrapperID := e.WrapperLog(a, "wrapper log\n", contract.Complete)
	var metaID domain.ArtifactID
	e.Scalar(&metaID, `SELECT id FROM artifacts WHERE attempt_id = $1 AND name = 'command.log.meta.json'`, string(a.ID))
	ctrl := e.Controller(&logstest.FakeLive{Body: "controller log\n"}, logstest.ControllerOptions{})
	crec, err := ctrl.Collect(bg(), a.Scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []domain.ArtifactID{wrapperID, metaID, crec.Link.Log.ID, crec.Link.Meta.ID}
	obs, err := ctrl.Observe(bg(), a.Scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nothing is eligible before the window", func(t *testing.T) {
		e.Clock.Advance(29 * 24 * time.Hour)
		if rep := sweep(t, rec); rep.Removed != 0 {
			t.Fatalf("removed %d artifacts before 30 days", rep.Removed)
		}
		for _, id := range ids {
			if d := disposition(t, e, id); d != "active" {
				t.Fatalf("%s is %s", id, d)
			}
		}
	})

	t.Run("a protection extends retention past the window", func(t *testing.T) {
		prot.protect(wrapperID)
		e.Clock.Advance(2 * 24 * time.Hour) // 31 days
		sweep(t, rec)
		if d := disposition(t, e, wrapperID); d != "active" {
			t.Fatalf("a protected log was removed: %s", d)
		}
		// Its sidecar and the controller capture are not protected and age out.
		for _, id := range ids[1:] {
			if d := disposition(t, e, id); d != "removed" {
				t.Fatalf("%s is %s after 31 days", id, d)
			}
		}
	})

	t.Run("removal changes availability but not recorded completeness", func(t *testing.T) {
		if err := ctrl.Reconcile(bg(), a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := e.Logs.Get(bg(), a.Scope, crec.ID)
		if got.Available() || got.Link.Completeness != logs.Unknown {
			t.Fatalf("%+v", got)
		}
		wrapperRec, _ := e.Logs.Get(bg(), a.Scope, obs.ID)
		if !wrapperRec.Available() || wrapperRec.Link.Completeness != logs.Complete {
			t.Fatalf("the protected wrapper log stays available: %+v", wrapperRec)
		}
		if logs.Select(mustRecords(t, e, a)) == nil {
			t.Fatal("the protected capture is still served")
		}
	})

	t.Run("an output artifact is never a log's to delete", func(t *testing.T) {
		b := e.NewAttempt(logstest.ScopeA, true)
		// An output role artifact of the same age is not eligible under the log policy.
		eligible, err := ret.Eligible(bg(), artifacts.RetentionSubject{ID: "art_out", Scope: b.Scope, Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
			VerifiedAt: e.Clock.Now().Add(-90 * 24 * time.Hour)}, e.Clock.Now())
		if err != nil || eligible {
			t.Fatalf("%v %v", eligible, err)
		}
		eligible, err = ret.Eligible(bg(), artifacts.RetentionSubject{ID: "art_oci", Scope: b.Scope, Kind: artifacts.KindOCI, Role: artifacts.RoleOutput}, e.Clock.Now())
		if err != nil || eligible {
			t.Fatalf("%v %v", eligible, err)
		}
	})

	t.Run("an active pin holds a log, and releasing it lets it age out", func(t *testing.T) {
		b := e.NewAttempt(logstest.ScopeA, true)
		id := e.WrapperLog(b, "pinned\n", contract.Complete)
		e.Clock.Advance(31 * 24 * time.Hour)
		pin := artifacts.PinOwner{Kind: "gate", ID: "gate_t023"}
		if err := e.ArtSvc.Pin(bg(), b.Scope, id, pin); err != nil {
			t.Fatal(err)
		}
		sweep(t, rec)
		if d := disposition(t, e, id); d != "active" {
			t.Fatalf("a pinned log was removed: %s", d)
		}
		if err := e.ArtSvc.Unpin(bg(), b.Scope, id, pin); err != nil {
			t.Fatal(err)
		}
		sweep(t, rec)
		if d := disposition(t, e, id); d != "removed" {
			t.Fatalf("an unpinned, unprotected old log is %s", d)
		}
	})

	t.Run("a pin racing a deletion never leaves a pinned artifact removed", func(t *testing.T) {
		b := e.NewAttempt(logstest.ScopeA, true)
		var logsIDs []domain.ArtifactID
		for i := 0; i < 6; i++ {
			logsIDs = append(logsIDs, e.WrapperUpload(b, "race-"+string(rune('a'+i))+".log", contract.LogMediaType, []byte("x\n")))
		}
		e.Clock.Advance(31 * 24 * time.Hour)
		pin := artifacts.PinOwner{Kind: "gate", ID: "gate_race"}
		results := make([]error, len(logsIDs))
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sweep(t, rec)
		}()
		for i, id := range logsIDs {
			wg.Add(1)
			go func(i int, id domain.ArtifactID) {
				defer wg.Done()
				results[i] = e.ArtSvc.Pin(bg(), b.Scope, id, pin)
			}(i, id)
		}
		wg.Wait()
		sweep(t, rec)
		for i, id := range logsIDs {
			d := disposition(t, e, id)
			switch {
			case results[i] == nil && d != "active":
				t.Errorf("%s was pinned successfully and removed (%s)", id, d)
			case results[i] != nil && !errors.Is(results[i], artifacts.ErrNotFound):
				t.Errorf("a losing pin must report not found, got %v", results[i])
			case results[i] != nil && d != "removed":
				t.Errorf("%s: the pin failed but the artifact is %s", id, d)
			}
		}
	})

	t.Run("a failing or missing protection policy deletes nothing", func(t *testing.T) {
		b := e.NewAttempt(logstest.ScopeA, true)
		id := e.WrapperLog(b, "kept on outage\n", contract.Complete)
		e.Clock.Advance(40 * 24 * time.Hour)
		prot.fail(errors.New("protection service down"))
		if _, err := rec.Sweep(bg()); err == nil {
			t.Fatal("an unestablishable protection state must be reported, not swallowed")
		}
		if d := disposition(t, e, id); d != "active" {
			t.Fatalf("an outage of the protection policy allowed a deletion: %s", d)
		}
		prot.fail(nil)
		// Missing wiring fails closed at construction and as a zero value.
		if _, err := adapters.NewRetention(0, nil); err == nil {
			t.Fatal("a retention policy without a protection policy must not be constructible")
		}
		if _, err := (adapters.Retention{}).Eligible(bg(), artifacts.RetentionSubject{Kind: artifacts.KindFile, Role: artifacts.RoleDiagnostic,
			VerifiedAt: e.Clock.Now().Add(-90 * 24 * time.Hour)}, e.Clock.Now()); !errors.Is(err, logs.ErrProtectionUnknown) {
			t.Fatalf("the zero policy must fail closed: %v", err)
		}
		unwired := newReconciler(t, e, adapters.Retention{})
		if _, err := unwired.Sweep(bg()); err == nil {
			t.Fatal("an unwired policy must be reported")
		}
		if d := disposition(t, e, id); d != "active" {
			t.Fatalf("an unwired policy allowed a deletion: %s", d)
		}
		sweep(t, rec)
		if d := disposition(t, e, id); d != "removed" {
			t.Fatalf("once protection is known the old log ages out: %s", d)
		}
	})
}

func mustRecords(t *testing.T, e *logstest.Env, a logstest.Attempt) []*logs.Record {
	t.Helper()
	recs, err := e.Logs.ForAttempt(bg(), a.Scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}
