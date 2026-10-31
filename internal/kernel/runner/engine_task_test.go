package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// admitFor admits run id for task (nil: a task-free run), as the shell does when
// a task starts or reruns.
func (e *env) admitFor(id domain.RunID, task *runtime.TaskRef) (runtime.RunRecord, error) {
	e.t.Helper()
	p := plan("task-"+string(id), ctr("a"), ctr("b", "a"))
	run, err := runtime.NewRun(scope, id, "pipe1", p, 3)
	if err != nil {
		e.t.Fatalf("build the run: %v", err)
	}
	return e.state.Admit(context.Background(), runtime.Admission{Run: run, Plan: p, Task: task,
		Events: []runtime.Event{{ID: "run.created:" + string(id), Scope: scope, RunID: id, Kind: runtime.EventRunCreated, Version: 1, At: e.clock.Now()}}})
}

// Test_unit_kernel_admission_carries_task proves the owning task is part of an
// admission: it must be in the run's project, it is reported by the record the
// engine drives and survives every commit and a takeover, and a task-free run
// stays valid and reports none.
func Test_unit_kernel_admission_carries_task(t *testing.T) {
	own := &runtime.TaskRef{Scope: scope, ID: "task1"}

	t.Run("admission validates the task against the run's project", func(t *testing.T) {
		e := newEnv(t)
		p := plan("v", ctr("a"))
		run, err := runtime.NewRun(scope, "run1", "pipe1", p, 3)
		if err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			task *runtime.TaskRef
			ok   bool
		}{
			"no task":                 {nil, true},
			"a task of the project":   {own, true},
			"a task of the project 2": {&runtime.TaskRef{Scope: scope, ID: "task2"}, true},
			"another project":         {&runtime.TaskRef{Scope: domain.Scope{TenantID: scope.TenantID, ProjectID: "other"}, ID: "task1"}, false},
			"another tenant":          {&runtime.TaskRef{Scope: domain.Scope{TenantID: "other", ProjectID: scope.ProjectID}, ID: "task1"}, false},
			"no scope":                {&runtime.TaskRef{ID: "task1"}, false},
			"an invalid id":           {&runtime.TaskRef{Scope: scope, ID: "bad id"}, false},
			"an empty id":             {&runtime.TaskRef{Scope: scope}, false},
		} {
			err := runtime.Admission{Run: run, Plan: p, Task: tc.task}.Validate()
			if tc.ok && err != nil {
				e.expect(false, "%s must be admitted: %v", name, err)
			}
			if !tc.ok && !errors.Is(err, domain.ErrInvalidValue) {
				e.expect(false, "%s must be refused as invalid: %v", name, err)
			}
		}
		// A foreign-project task reaches no store: nothing is recorded.
		_, err = e.admitFor("run1", &runtime.TaskRef{Scope: domain.Scope{TenantID: scope.TenantID, ProjectID: "other"}, ID: "task1"})
		e.expect(errors.Is(err, domain.ErrInvalidValue), "the store refuses a foreign task: %v", err)
		if _, err := e.state.Load(context.Background(), scope, "run1"); err == nil {
			e.expect(false, "a refused admission left a run")
		}
	})

	t.Run("a record is coherent only with a task of its own project", func(t *testing.T) {
		e := newEnv(t)
		rec, err := e.admitFor("run1", own)
		e.expect(err == nil, "admit: %v", err)
		e.expect(rec.Validate() == nil, "the admitted record is coherent: %v", rec.Validate())
		rec.Task = &runtime.TaskRef{Scope: domain.Scope{TenantID: scope.TenantID, ProjectID: "other"}, ID: "task1"}
		e.expect(errors.Is(rec.Validate(), domain.ErrInvalidValue), "a record naming a foreign task is incoherent")
		rec.Task = nil
		e.expect(rec.Validate() == nil, "a task-free record is coherent")
	})

	t.Run("the engine keeps the task through a run to its end and a takeover", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.admitFor("run1", own)
		e.expect(err == nil, "admit for a task: %v", err)
		_, err = e.admitFor("run2", nil)
		e.expect(err == nil, "admit a task-free run: %v", err)

		// A first engine stops mid-run, a second takes over after the lease expired.
		e.rt.scripts["a"] = script{polls: 3}
		crashed := false
		e.onSleep = func() error {
			if !crashed {
				crashed = true
				return errCrash
			}
			return nil
		}
		_, _ = e.engine("w1").Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
		e.onSleep = nil
		e.expect(crashed, "the first engine was interrupted")
		mid := e.load("run1")
		e.expect(mid.Task != nil && *mid.Task == *own, "the task survives an interrupted run: %+v", mid.Task)
		e.expire()
		e.drive(e.engine("w2"), "run1")
		e.drive(e.engine("w1"), "run2")

		done := e.load("run1")
		e.expect(done.Run.Outcome == runtime.OutcomeSucceeded, "the task's run succeeded: %+v", done.Run)
		e.expect(done.Task != nil && *done.Task == *own, "a settled run reports its task: %+v", done.Task)
		view, err := e.state.View(context.Background(), scope, "run1")
		e.expect(err == nil && view.Task != nil && *view.Task == *own, "the view reports the task: %+v %v", view.Task, err)

		free := e.load("run2")
		e.expect(free.Run.Outcome == runtime.OutcomeSucceeded && free.Task == nil, "a task-free run completes and reports no task: %+v", free.Task)
	})
}
