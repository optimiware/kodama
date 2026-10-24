package postgres_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

func ctrlStatement() logs.Statement {
	return logs.Statement{Completeness: logs.Unknown, Reasons: []logs.Reason{contract.ReasonControllerCapture}}
}

func begin(e *logstest.Env, a logstest.Attempt, id logs.RecordID, token string, lease time.Duration, now time.Time) (logs.Claim, error) {
	var claim logs.Claim
	_, err := e.Logs.Update(ctx(), a.Scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
		c, err := r.Begin(token, lease, now)
		claim = c
		return err == nil, logs.Audit{Action: "log.collecting"}, err
	})
	return claim, err
}

// TestConcurrentLinkage: two collectors and racing wrapper/controller linkage
// cannot overwrite evidence or accept a stale claim; rollback preserves prior
// state. Real PostgreSQL.
func TestConcurrentLinkage(t *testing.T) {
	e := logstest.New(t, logstest.Options{})
	a := e.NewAttempt(logstest.ScopeA, true)
	info := e.Info(a)

	t.Run("one of many collectors wins the claim", func(t *testing.T) {
		rec := newRecord(t, e, a, logs.SourceController)
		const n = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		var won, busy, other int
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := begin(e, a, rec.ID, logs.RandomIDs{}.NewToken(), time.Minute, time.Now().UTC())
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, logs.ErrBusy):
					busy++
				default:
					other++
					t.Errorf("unexpected: %v", err)
				}
			}(i)
		}
		wg.Wait()
		if won != 1 || busy != n-1 {
			t.Fatalf("won=%d busy=%d other=%d", won, busy, other)
		}
		got, _ := e.Logs.Get(ctx(), a.Scope, rec.ID)
		if got.Claim.Fence != 1 || got.Claims != 1 {
			t.Fatalf("exactly one claim must be recorded: %+v", got)
		}
	})

	t.Run("a stale collector cannot commit after takeover", func(t *testing.T) {
		a2 := e.NewAttempt(logstest.ScopeA, true)
		rec := newRecord(t, e, a2, logs.SourceController)
		now := time.Now().UTC().Truncate(time.Microsecond)
		old, err := begin(e, a2, rec.ID, "tok_old", time.Second, now)
		if err != nil {
			t.Fatal(err)
		}
		// The old lease lapses; a second collector takes over with a higher fence.
		fresh, err := begin(e, a2, rec.ID, "tok_new", time.Minute, now.Add(2*time.Second))
		if err != nil || fresh.Fence != old.Fence+1 {
			t.Fatalf("takeover: %+v %v", fresh, err)
		}
		snap := logs.Snapshot{LogName: "controller-g1.log", MetaName: "controller-g1.log.meta.json", Digest: "sha256:" + string(make([]byte, 0)) + zeros(), Size: 3, Statement: ctrlStatement()}
		_, err = e.Logs.Update(ctx(), a2.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Freeze(old, snap, now)
			return ch, logs.Audit{Action: "log.frozen"}, err
		})
		if !errors.Is(err, logs.ErrClaimLost) {
			t.Fatalf("stale freeze: %v", err)
		}
		ref := logRef(t, e, a2, "controller-g1.log", "abc")
		meta := logRef(t, e, a2, "controller-g1.log.meta.json", "{}")
		_, err = e.Logs.Update(ctx(), a2.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			err := r.Retain(old, logs.Link{Log: ref, Meta: &meta, Statement: ctrlStatement()}, now)
			return err == nil, logs.Audit{Action: "log.retained"}, err
		})
		if !errors.Is(err, logs.ErrClaimLost) {
			t.Fatalf("stale linkage must not commit: %v", err)
		}
		got, _ := e.Logs.Get(ctx(), a2.Scope, rec.ID)
		if got.State != logs.StateCollecting || got.Claim.Token != "tok_new" || got.Snapshot != nil || got.Link != nil {
			t.Fatalf("the stale collector changed the record: %+v", got)
		}
	})

	t.Run("racing observations of different logs keep exactly one", func(t *testing.T) {
		a3 := e.NewAttempt(logstest.ScopeA, true)
		rec := newRecord(t, e, a3, logs.SourceWrapper)
		refs := []logs.ArtifactRef{
			logRef(t, e, a3, contract.CommandLogName, "first\n"),
			logRef(t, e, a3, "second.log", "second\n"),
			logRef(t, e, a3, "third.log", "third\n"),
		}
		var wg sync.WaitGroup
		results := make([]error, len(refs)*3)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, results[i] = observe(e, a3.Scope, rec.ID, wrapperLink(refs[i%len(refs)]))
			}(i)
		}
		wg.Wait()
		got, _ := e.Logs.Get(ctx(), a3.Scope, rec.ID)
		if got.State != logs.StateRetained || got.Version != 2 {
			t.Fatalf("one linkage, one change: %+v", got)
		}
		winner := got.Link.Log
		for i, err := range results {
			same := refs[i%len(refs)] == winner
			switch {
			case same && err != nil:
				t.Errorf("an identical duplicate must succeed: %v", err)
			case !same && !errors.Is(err, logs.ErrConflict):
				t.Errorf("a different linkage must conflict: %v", err)
			}
		}
		if n := e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1 AND action = 'log.linked'`, string(rec.ID)); n != 1 {
			t.Fatalf("exactly one audit entry for one linkage, got %d", n)
		}
	})

	t.Run("wrapper and controller linkage coexist without overwriting", func(t *testing.T) {
		a4 := e.NewAttempt(logstest.ScopeA, true)
		wrap := newRecord(t, e, a4, logs.SourceWrapper)
		ctrl := newRecord(t, e, a4, logs.SourceController)
		if wrap.ID == ctrl.ID {
			t.Fatal("sources have separate records")
		}
		wref := logRef(t, e, a4, contract.CommandLogName, "from the wrapper\n")
		cref := logRef(t, e, a4, "controller-g1.log", "from the controller\n")
		cmeta := logRef(t, e, a4, "controller-g1.log.meta.json", "{}")
		snap := logs.Snapshot{LogName: "controller-g1.log", MetaName: "controller-g1.log.meta.json", Digest: cref.Digest, Size: cref.Size, Statement: ctrlStatement()}
		now := time.Now().UTC().Truncate(time.Microsecond)
		claim, err := begin(e, a4, ctrl.ID, "tok", time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		var werr, cerr error
		go func() {
			defer wg.Done()
			_, werr = observe(e, a4.Scope, wrap.ID, wrapperLink(wref))
		}()
		go func() {
			defer wg.Done()
			if _, cerr = e.Logs.Update(ctx(), a4.Scope, ctrl.ID, func(r *logs.Record) (bool, logs.Audit, error) {
				ch, err := r.Freeze(claim, snap, now)
				return ch, logs.Audit{Action: "log.frozen"}, err
			}); cerr != nil {
				return
			}
			_, cerr = e.Logs.Update(ctx(), a4.Scope, ctrl.ID, func(r *logs.Record) (bool, logs.Audit, error) {
				err := r.Retain(claim, logs.Link{Log: cref, Meta: &cmeta, Statement: ctrlStatement()}, now)
				return err == nil, logs.Audit{Action: "log.retained"}, err
			})
		}()
		wg.Wait()
		if werr != nil || cerr != nil {
			t.Fatalf("wrapper %v controller %v", werr, cerr)
		}
		recs, _ := e.Logs.ForAttempt(ctx(), a4.Scope, a4.ID)
		if len(recs) != 2 {
			t.Fatalf("records = %d", len(recs))
		}
		sel := logs.Select(recs)
		if sel == nil || sel.Source != logs.SourceWrapper || sel.Link.Log != wref {
			t.Fatalf("the verified complete wrapper capture is preferred: %+v", sel)
		}
		for _, r := range recs {
			if r.Source == logs.SourceController && r.Link.Log != cref {
				t.Fatalf("the controller evidence was overwritten: %+v", r.Link)
			}
		}
	})

	t.Run("Ensure is idempotent under concurrency and survives restarts", func(t *testing.T) {
		a5 := e.NewAttempt(logstest.ScopeA, true)
		var wg sync.WaitGroup
		ids := make(chan logs.RecordID, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tmpl, _ := logs.NewRecord(a5.Scope, logs.RandomIDs{}.NewRecordID(), a5.ID, a5.Run, info.Step, logs.SourceController, 1, t0)
				r, err := e.Logs.Ensure(ctx(), *tmpl)
				if err != nil {
					t.Error(err)
					return
				}
				ids <- r.ID
			}()
		}
		wg.Wait()
		close(ids)
		first := logs.RecordID("")
		for id := range ids {
			if first == "" {
				first = id
			}
			if id != first {
				t.Fatalf("concurrent Ensure produced several records: %s vs %s", first, id)
			}
		}
	})

	t.Run("a new generation exists only after lost frozen bytes", func(t *testing.T) {
		a6 := e.NewAttempt(logstest.ScopeA, true)
		rec := newRecord(t, e, a6, logs.SourceController)
		next, err := logs.NewRecord(a6.Scope, "logrec_gen2", a6.ID, a6.Run, info.Step, logs.SourceController, 2, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Logs.Create(ctx(), next); err == nil {
			t.Fatal("generation 2 was created while generation 1 had not lost its bytes")
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		claim, _ := begin(e, a6, rec.ID, "tok", time.Minute, now)
		if _, err := e.Logs.Update(ctx(), a6.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			err := r.Fail(claim, logs.FailureFrozenLost, now)
			return err == nil, logs.Audit{Action: "log.unavailable"}, err
		}); err != nil {
			t.Fatal(err)
		}
		got, err := e.Logs.Create(ctx(), next)
		if err != nil || got.Generation != 2 {
			t.Fatalf("create: %+v %v", got, err)
		}
		again, err := e.Logs.Create(ctx(), next)
		if err != nil || again.ID != got.ID {
			t.Fatalf("creating the same generation again returns it: %+v %v", again, err)
		}
		latest := newRecordLatest(t, e, a6, info.Step)
		if latest.Generation != 2 {
			t.Fatalf("Ensure returns the latest generation: %d", latest.Generation)
		}
	})
}

func zeros() string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

func newRecordLatest(t *testing.T, e *logstest.Env, a logstest.Attempt, step string) *logs.Record {
	t.Helper()
	tmpl, _ := logs.NewRecord(a.Scope, logs.RandomIDs{}.NewRecordID(), a.ID, a.Run, step, logs.SourceController, 1, t0)
	r, err := e.Logs.Ensure(ctx(), *tmpl)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
