package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
)

var (
	gScope = domain.Scope{TenantID: "t_guard", ProjectID: "p_guard"}
	gNow   = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
)

func guardStore(t *testing.T) *Store {
	t.Helper()
	w := NewWorld(memdb.New())
	s, err := NewStore(w.db, w.Peers(NewAuthority()), Options{Clock: func() time.Time { return gNow }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func guardRun(id domain.RunID, status execution.RunStatus) *execution.Run {
	return &execution.Run{Scope: gScope, ID: id, Purpose: execution.PurposeCleanup, DefinitionID: "pd_g", Status: status, Version: 1, CreatedAt: gNow, UpdatedAt: gNow}
}

func in(t *testing.T, s *Store, fn func(tx memdb.Tx) error) error {
	t.Helper()
	return s.db.Update(context.Background(), gScope.TenantID, fn)
}

// TestUnitStoreGuards: the Go counterparts of the PostgreSQL triggers: terminal
// rows never change, versions move one step at a time, an attempt is created
// only after every earlier attempt of its step settled, and bookkeeping is
// recorded once.
func TestUnitStoreGuards(t *testing.T) {
	t.Run("a terminal run and a skipped version are refused", func(t *testing.T) {
		s := guardStore(t)
		if err := in(t, s, func(tx memdb.Tx) error {
			if err := s.insertRun(tx, guardRun("run_done", execution.RunSucceeded)); err != nil {
				return err
			}
			return s.insertRun(tx, guardRun("run_live", execution.RunRunning))
		}); err != nil {
			t.Fatal(err)
		}
		done := guardRun("run_done", execution.RunSucceeded)
		done.Version = 2
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateRun(tx, done) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("terminal run: %v", err)
		}
		skip := guardRun("run_live", execution.RunRunning)
		skip.Version = 3
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateRun(tx, skip) }); !errors.Is(err, execution.ErrStale) {
			t.Fatalf("skipped version: %v", err)
		}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertRun(tx, guardRun("run_live", execution.RunRunning)) }); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("duplicate run: %v", err)
		}
		next := guardRun("run_live", execution.RunRunning)
		next.Version = 2
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateRun(tx, next) }); err != nil {
			t.Fatalf("the next version: %v", err)
		}
	})

	t.Run("a step is created only after its earlier attempts settled", func(t *testing.T) {
		s := guardStore(t)
		attempt := func(id domain.AttemptID, ordinal int) *execution.Attempt {
			return &execution.Attempt{Scope: gScope, ID: id, RunID: "run_g", StepID: "s", Ordinal: ordinal, Access: execution.AccessNone, Status: execution.AttemptRunning, Version: 1, CreatedAt: gNow, UpdatedAt: gNow}
		}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, attempt("att_1", 1)) }); err != nil {
			t.Fatal(err)
		}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, attempt("att_1", 1)) }); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("duplicate attempt: %v", err)
		}
		// No bookkeeping row yet, and then an unsettled one: both block.
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, attempt("att_2", 2)) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("an attempt created beside an unbookkept one: %v", err)
		}
		c := execution.NewControl("att_1")
		if err := in(t, s, func(tx memdb.Tx) error { return s.putControl(tx, gScope, "run_g", c, true) }); err != nil {
			t.Fatal(err)
		}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, attempt("att_2", 2)) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("an attempt created beside an unsettled one: %v", err)
		}
		settled := gNow
		c.SettledAt = &settled
		if err := in(t, s, func(tx memdb.Tx) error { return s.putControl(tx, gScope, "run_g", c, false) }); err != nil {
			t.Fatal(err)
		}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, attempt("att_2", 2)) }); err != nil {
			t.Fatalf("an attempt after settlement: %v", err)
		}
		// Settlement is recorded once.
		later := gNow.Add(time.Second)
		c.SettledAt = &later
		if err := in(t, s, func(tx memdb.Tx) error { return s.putControl(tx, gScope, "run_g", c, false) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("a rewritten settlement: %v", err)
		}
		if err := in(t, s, func(tx memdb.Tx) error {
			return s.putControl(tx, gScope, "run_g", execution.NewControl("att_9"), false)
		}); !errors.Is(err, execution.ErrStale) {
			t.Fatalf("an update of missing bookkeeping: %v", err)
		}
	})

	t.Run("a terminal attempt and a rebound workload are immutable", func(t *testing.T) {
		s := guardStore(t)
		a := &execution.Attempt{Scope: gScope, ID: "att_t", RunID: "run_g", StepID: "s", Ordinal: 1, Access: execution.AccessNone, Status: execution.AttemptRunning, WorkloadUID: "u1", Version: 1, CreatedAt: gNow, UpdatedAt: gNow}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, a) }); err != nil {
			t.Fatal(err)
		}
		rebound := *a
		rebound.Version, rebound.WorkloadUID = 2, "u2"
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateAttempt(tx, &rebound) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("rebound workload: %v", err)
		}
		done := *a
		done.Version, done.Status = 2, execution.AttemptFailed
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateAttempt(tx, &done) }); err != nil {
			t.Fatal(err)
		}
		again := done
		again.Version = 3
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateAttempt(tx, &again) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("terminal attempt: %v", err)
		}
	})

	t.Run("a terminal step and a miscounted attempt are refused", func(t *testing.T) {
		s := guardStore(t)
		step := &execution.StepRun{RunID: "run_g", ID: "s", Status: execution.StepRunning, Spec: pipeline.Step{ID: "s"}, Version: 1, CreatedAt: gNow, UpdatedAt: gNow}
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertStep(tx, gScope, step) }); err != nil {
			t.Fatal(err)
		}
		jump := *step
		jump.Version, jump.AttemptsStarted = 2, 2
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateStep(tx, gScope, &jump) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("two attempts at once: %v", err)
		}
		done := *step
		done.Version, done.Status = 2, execution.StepSucceeded
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateStep(tx, gScope, &done) }); err != nil {
			t.Fatal(err)
		}
		again := done
		again.Version = 3
		if err := in(t, s, func(tx memdb.Tx) error { return s.updateStep(tx, gScope, &again) }); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("terminal step: %v", err)
		}
	})

	t.Run("a transaction of another tenant reaches nothing", func(t *testing.T) {
		s := guardStore(t)
		if err := in(t, s, func(tx memdb.Tx) error { return s.insertRun(tx, guardRun("run_x", execution.RunQueued)) }); err != nil {
			t.Fatal(err)
		}
		other := domain.Scope{TenantID: "t_other", ProjectID: gScope.ProjectID}
		err := s.db.Update(context.Background(), other.TenantID, func(tx memdb.Tx) error {
			_, _, err := s.UpdateInTx(context.Background(), tx, gScope, "run_x", execution.UpdateOptions{}, execution.CancelMutation(execution.ReasonCancelled, nil))
			return err
		})
		if !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a foreign transaction updated the run: %v", err)
		}
		if _, err := s.Get(context.Background(), other, "run_x"); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a foreign get: %v", err)
		}
	})

	t.Run("a failed commit reports an unknown outcome and publishes nothing", func(t *testing.T) {
		fault := errors.New("disk")
		fail := false
		w := NewWorld(memdb.New(memdb.WithCommitFault(func() error {
			if fail {
				return fault
			}
			return nil
		})))
		s, err := NewStore(w.db, w.Peers(NewAuthority()), Options{Clock: func() time.Time { return gNow }})
		if err != nil {
			t.Fatal(err)
		}
		fail = true
		plan := &execution.AdmissionPlan{Aggregate: &execution.Aggregate{Run: guardRun("run_c", execution.RunQueued)}}
		if _, _, err := s.Admit(context.Background(), plan); !errors.Is(err, execution.ErrUnavailable) || !errors.Is(err, fault) {
			t.Fatalf("admit with a failing commit: %v", err)
		}
		fail = false
		if _, err := s.Get(context.Background(), gScope, "run_c"); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a failed commit published the run: %v", err)
		}
	})
}
