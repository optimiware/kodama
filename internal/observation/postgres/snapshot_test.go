package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
	obspg "github.com/optimiware/kodama/internal/observation/postgres"
)

// recordingRuns wraps the test-only run tables and records the transaction and
// isolation each call ran under.
type recordingRuns struct {
	inner obspg.RunBinding
	mu    sync.Mutex
	txs   []pgx.Tx
	iso   []string
	ro    []string
}

func (r *recordingRuns) note(ctx context.Context, tx pgx.Tx) {
	var iso, ro string
	_ = tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&iso, &ro)
	r.mu.Lock()
	r.txs, r.iso, r.ro = append(r.txs, tx), append(r.iso, iso), append(r.ro, ro)
	r.mu.Unlock()
}

func (r *recordingRuns) RunExists(ctx context.Context, tx pgx.Tx, s domain.Scope, run domain.RunID) (bool, error) {
	r.note(ctx, tx)
	return r.inner.RunExists(ctx, tx, s, run)
}

func (r *recordingRuns) ReadResources(ctx context.Context, tx pgx.Tx, q obspg.ResourceQuery) ([]observation.Resource, error) {
	r.note(ctx, tx)
	return r.inner.ReadResources(ctx, tx, q)
}

type recordingWork struct {
	recordingRuns
}

func (w *recordingWork) ReadResources(ctx context.Context, tx pgx.Tx, q obspg.ResourceQuery) ([]observation.Resource, error) {
	w.note(ctx, tx)
	return obspg.WorkResources{}.ReadResources(ctx, tx, q)
}

// fakeBinding returns canned resources.
type fakeBinding struct {
	resources []observation.Resource
	err       error
	limits    []int
}

func (f *fakeBinding) ReadResources(_ context.Context, _ pgx.Tx, q obspg.ResourceQuery) ([]observation.Resource, error) {
	f.limits = append(f.limits, q.Limit)
	return f.resources, f.err
}

func (f *fakeBinding) RunExists(context.Context, pgx.Tx, domain.Scope, domain.RunID) (bool, error) {
	return true, nil
}

// Test_snapshot_binding_contract: bindings use one supplied transaction;
// unavailable bindings and oversized or incomplete snapshots cannot issue
// successful cursors. Execution seams use explicitly test-only persisted
// fixtures, not invented production tables.
func Test_snapshot_binding_contract(t *testing.T) {
	e := ot.New(t, ot.Options{})
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		e.Commit(ot.TenantA, ot.InsertWork(ot.ProjectA, fmt.Sprintf("wi_%d", i), "backlog"), ot.WorkCreated(ot.TenantA, ot.ProjectA, fmt.Sprintf("t024_e%d", i), fmt.Sprintf("wi_%d", i)))
	}
	e.SeedRun(ot.TenantA, ot.ProjectA, "run_1", "running", 4)
	e.SeedRun(ot.TenantA, ot.ProjectA, "run_2", "pending", 1)
	e.SeedAttempt(ot.TenantA, ot.ProjectA, "att_1", "run_1", "build", "running", 2)
	e.SeedAttempt(ot.TenantA, ot.ProjectA, "att_2", "run_2", "build", "pending", 1)

	t.Run("construction requires every binding", func(t *testing.T) {
		for name, args := range map[string]struct {
			db   bool
			b    obspg.Bindings
			want bool
		}{
			"no work binding": {true, obspg.Bindings{Runs: e.Runs}, false},
			"no run binding":  {true, obspg.Bindings{Work: obspg.WorkResources{}}, false},
			"no bindings":     {true, obspg.Bindings{}, false},
			"complete":        {true, obspg.Bindings{Work: obspg.WorkResources{}, Runs: e.Runs}, true},
		} {
			_, err := obspg.NewStore(e.Runtime, e.Events, args.b)
			if (err == nil) != args.want || (err != nil && !errors.Is(err, observation.ErrInvalidConfiguration)) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		if _, err := obspg.NewStore(nil, e.Events, obspg.Bindings{Work: obspg.WorkResources{}, Runs: e.Runs}); !errors.Is(err, observation.ErrInvalidConfiguration) {
			t.Errorf("nil db: %v", err)
		}
		if _, err := obspg.NewStore(e.Runtime, nil, obspg.Bindings{Work: obspg.WorkResources{}, Runs: e.Runs}); !errors.Is(err, observation.ErrInvalidConfiguration) {
			t.Errorf("nil log: %v", err)
		}
	})

	t.Run("one repeatable-read read-only transaction serves every binding", func(t *testing.T) {
		runs := &recordingRuns{inner: e.Runs}
		work := &recordingWork{recordingRuns{inner: e.Runs}}
		st, err := obspg.NewStore(e.Runtime, e.Events, obspg.Bindings{Work: work, Runs: runs})
		if err != nil {
			t.Fatal(err)
		}
		snap, err := st.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Resources) != 7 || snap.Position != 3 {
			t.Fatalf("snapshot = %d resources at %d", len(snap.Resources), snap.Position)
		}
		if len(work.txs) != 1 || len(runs.txs) != 1 || work.txs[0] != runs.txs[0] {
			t.Fatalf("the bindings did not share one transaction: %d/%d", len(work.txs), len(runs.txs))
		}
		for _, iso := range append(work.iso, runs.iso...) {
			if iso != "repeatable read" {
				t.Errorf("isolation = %q", iso)
			}
		}
		for _, ro := range append(work.ro, runs.ro...) {
			if ro != "on" {
				t.Errorf("transaction_read_only = %q", ro)
			}
		}
		// A run stream also verifies the run in that same transaction family.
		runs.txs = nil
		if _, err := st.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, "run_1"), 100); err != nil {
			t.Fatal(err)
		}
		if len(runs.txs) != 2 || runs.txs[0] != runs.txs[1] {
			t.Fatalf("run lookup and run state must share the snapshot transaction (%d calls)", len(runs.txs))
		}
	})

	t.Run("run snapshots contain only the run and its attempts", func(t *testing.T) {
		snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, "run_1"), 100)
		if err != nil || len(snap.Resources) != 2 {
			t.Fatalf("snapshot = %+v, %v", snap, err)
		}
		for _, r := range snap.Resources {
			if r.ID != "run_1" && r.ID != "att_1" {
				t.Errorf("foreign resource %+v", r)
			}
		}
	})

	t.Run("a type filter narrows the families read", func(t *testing.T) {
		snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, "", "run.state_changed"), 100)
		if err != nil || len(snap.Resources) != 2 {
			t.Fatalf("snapshot = %+v, %v", snap, err)
		}
		for _, r := range snap.Resources {
			if r.Type != observation.FamilyRun {
				t.Errorf("unexpected %+v", r)
			}
		}
	})

	t.Run("an unavailable binding never yields an empty success", func(t *testing.T) {
		down := errors.New("execution binding down")
		e.Runs.Down.Store(&down)
		defer e.Runs.Down.Store(nil)
		snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 100)
		if !errors.Is(err, observation.ErrUnavailable) || len(snap.Resources) != 0 || snap.Position != 0 {
			t.Fatalf("snapshot = %+v, err = %v", snap, err)
		}
		if err := e.Obs.Verify(ctx, scope(t, ot.TenantA, ot.ProjectA, "run_1")); !errors.Is(err, observation.ErrUnavailable) {
			t.Fatalf("run lookup with the binding down: %v", err)
		}
		// Work-only snapshots still read the work binding but must not hide the
		// missing execution state: the project projection includes runs.
		if _, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, "", "work_item.created"), 100); err != nil {
			t.Fatalf("a work-only projection does not need the execution binding: %v", err)
		}
	})

	t.Run("oversized snapshots fail instead of truncating", func(t *testing.T) {
		for max, want := range map[int]bool{7: true, 6: false, 3: false, 1: false} {
			snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), max)
			if want {
				if err != nil || len(snap.Resources) != 7 {
					t.Errorf("bound %d: %+v, %v", max, snap, err)
				}
				continue
			}
			if !errors.Is(err, observation.ErrSnapshotTooLarge) || len(snap.Resources) != 0 || snap.Position != 0 {
				t.Errorf("bound %d: snapshot = %+v, err = %v", max, snap, err)
			}
		}
		if _, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 0); !errors.Is(err, observation.ErrInvalidConfiguration) {
			t.Errorf("a zero bound: %v", err)
		}
	})

	t.Run("bindings are asked for one more than the budget left", func(t *testing.T) {
		work := &fakeBinding{resources: []observation.Resource{{Type: observation.FamilyWork, ID: "w1", Version: 1, State: "backlog"}}}
		runs := &fakeBinding{}
		st, _ := obspg.NewStore(e.Runtime, e.Events, obspg.Bindings{Work: work, Runs: runs})
		if _, err := st.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 10); err != nil {
			t.Fatal(err)
		}
		if len(work.limits) != 1 || work.limits[0] != 11 || len(runs.limits) != 1 || runs.limits[0] != 10 {
			t.Fatalf("limits work=%v runs=%v", work.limits, runs.limits)
		}
	})

	t.Run("a binding returning an unrequested family is a failure", func(t *testing.T) {
		work := &fakeBinding{resources: []observation.Resource{{Type: observation.FamilyRun, ID: "x", Version: 1, State: "s"}}}
		st, _ := obspg.NewStore(e.Runtime, e.Events, obspg.Bindings{Work: work, Runs: &fakeBinding{}})
		if _, err := st.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 10); !errors.Is(err, observation.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a failing binding aborts the whole snapshot", func(t *testing.T) {
		st, _ := obspg.NewStore(e.Runtime, e.Events, obspg.Bindings{Work: &fakeBinding{err: errors.New("boom")}, Runs: &fakeBinding{}})
		snap, err := st.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 10)
		if !errors.Is(err, observation.ErrUnavailable) || snap.Position != 0 {
			t.Fatalf("snapshot = %+v, err = %v", snap, err)
		}
	})
}

// isolationLog records the transaction a durable read runs in.
type isolationLog struct {
	obspg.EventLog
	iso, ro string
}

func (l *isolationLog) ReadObservations(ctx context.Context, tx pgx.Tx, s events.Scope, after int64, limit int) ([]eventspg.Observation, eventspg.Watermark, error) {
	_ = tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&l.iso, &l.ro)
	return l.EventLog.ReadObservations(ctx, tx, s, after, limit)
}

// Test_durable_read_snapshot: a page and the watermark it reports come from one
// snapshot. The event store re-reads the watermark after the rows, so under
// READ COMMITTED a commit in between would report a high-water mark above a page
// that does not contain it and an idle stream could mistake that for a corrupt
// log; the store therefore reads in one read-only REPEATABLE READ transaction.
func Test_durable_read_snapshot(t *testing.T) {
	e := ot.New(t, ot.Options{})
	for i := 1; i <= 3; i++ {
		e.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, fmt.Sprintf("t024_dr%d", i), "wi_dr", int64(i)))
	}
	il := &isolationLog{EventLog: e.Events}
	st, err := obspg.NewStore(e.Runtime, il, obspg.Bindings{Work: obspg.WorkResources{}, Runs: e.Runs})
	if err != nil {
		t.Fatal(err)
	}
	recs, wm, err := st.Read(context.Background(), scope(t, ot.TenantA, ot.ProjectA, ""), 1, 10)
	if err != nil || len(recs) != 2 || wm.HighWater != 3 {
		t.Fatalf("read = %d records, %+v, %v", len(recs), wm, err)
	}
	if il.iso != "repeatable read" || il.ro != "on" {
		t.Fatalf("durable reads ran in %q (read-only %q)", il.iso, il.ro)
	}
	// An expired position is classified without a partial page.
	if _, _, err := st.Read(context.Background(), scope(t, ot.TenantA, ot.ProjectA, ""), -1, 10); err == nil {
		t.Fatal("a negative position must fail")
	}
}
