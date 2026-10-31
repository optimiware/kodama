package tasks

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Test_unit_tasks_start_rerun holds first execution and rerun to the one-current-run
// rule: a start admits one run, a repeat after a lost response admits none, a rerun
// is explicit, version-checked against the run the caller saw and refused while that
// run is still going.
func Test_unit_tasks_start_rerun(t *testing.T) {
	t.Run("a start admits exactly one run and a repeat finds it", func(t *testing.T) {
		r := newRig(t)
		task := r.task("start")
		started := r.start(task)
		run := started.Progress.RunID
		if run == "" || started.Progress.Status != StatusQueued {
			t.Fatalf("start = %+v", started.Progress)
		}
		again, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID})
		if err != nil || again.Progress != started.Progress {
			t.Fatalf("a repeated start = %+v, %v", again.Progress, err)
		}
		if owner, err := r.store.TaskOfRun(r.ctx, scopeA, run); err != nil || owner != task.ID {
			t.Fatalf("the run is not the task's: %v %v", owner, err)
		}
		if rec, err := r.runs.Load(r.ctx, scopeA, run); err != nil || rec.Run.PipelineID != task.PipelineID || len(rec.Waits) != 1 {
			t.Fatalf("the admitted run = %+v, %v", rec.Run, err)
		}
		if n := countProgress(r, StatusQueued); n != 1 {
			t.Fatalf("%d queued progress events, want one", n)
		}
	})

	t.Run("a lost response after the run was admitted is reconciled without a second run", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() {
			return
		}
		task := r.task("lost")
		boom := errors.New("lost response")
		r.failNext(boom) // the selection commit fails after the run was admitted
		if _, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID}); !errors.Is(err, boom) {
			t.Fatalf("start = %v", err)
		}
		if got := r.get(task.ID); got.Progress.RunID != "" || len(progressEvents(r)) != 0 {
			t.Fatalf("a failed selection changed the task: %+v", got.Progress)
		}
		started := r.start(task)
		if started.Progress.RunID != runIDFor(task, "") {
			t.Fatalf("the retry selected %s, want the run derived from the task", started.Progress.RunID)
		}
		// The run the first attempt admitted is the one selected: still a single run.
		if _, err := r.runs.Load(r.ctx, scopeA, started.Progress.RunID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a retry whose pipeline plan changed in the meantime still finds its run", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() {
			return
		}
		task := r.task("changed")
		boom := errors.New("lost response")
		r.failNext(boom)
		_, _ = r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID})
		other := solePlan()
		r.plan.plan = other // the pipeline was re-indexed: a different digest
		started := r.start(task)
		rec, err := r.runs.Load(r.ctx, scopeA, started.Progress.RunID)
		if err != nil || rec.Plan.Digest != "sha256:review" {
			t.Fatalf("the run was replaced instead of found: %+v, %v", rec.Plan.Digest, err)
		}
	})

	t.Run("start and rerun are authorized by role", func(t *testing.T) {
		r := newRig(t)
		task := r.task("auth")
		for _, p := range []string{"view", "alice"} {
			if _, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, p), TaskID: task.ID}); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s: %v, want forbidden", p, err)
			}
		}
		if _, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "stranger"), TaskID: task.ID}); !errors.Is(err, ErrNotFound) {
			t.Errorf("stranger: %v", err)
		}
		if _, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeC, "eng"), TaskID: task.ID}); !errors.Is(err, ErrNotFound) {
			t.Errorf("another project's engineer: %v", err)
		}
		if r.plan.calls != 0 {
			t.Fatalf("a refused request reached the planner %d times", r.plan.calls)
		}
		if _, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "ops"), TaskID: task.ID}); err != nil {
			t.Errorf("an operator starts: %v", err)
		}
	})

	t.Run("a rerun is explicit, current-run checked and refused while the run is active", func(t *testing.T) {
		r := newRig(t)
		r.plan.plan = solePlan()
		task := r.start(r.task("rerun"))
		first := task.Progress.RunID
		eng := r.caller(scopeA, "eng")
		rerun := func(expected domain.RunID) (Task, error) {
			return r.svc.Rerun(r.ctx, RerunRequest{Caller: eng, TaskID: task.ID, ExpectedRunID: expected})
		}
		if _, err := r.svc.Rerun(r.ctx, RerunRequest{Caller: eng, TaskID: task.ID, ExpectedRunID: "run_x"}); !errors.Is(err, ErrStaleRun) {
			t.Fatalf("a rerun of a run that is not current: %v, want stale", err)
		}
		if _, err := rerun(first); !errors.Is(err, ErrRunActive) {
			t.Fatalf("a rerun of a queued run: %v, want active", err)
		}
		if _, err := r.svc.Start(r.ctx, StartRequest{Caller: eng, TaskID: task.ID}); err != nil {
			t.Fatalf("the repeated start stays idempotent: %v", err)
		}

		// The stored projection says running, but the run's own records say it finished:
		// the rerun judges from the records.
		e := r.engine(first)
		e.step(func(x *runtime.Run) error { return x.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		r.apply(first)
		e.step(func(x *runtime.Run) error {
			return x.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
		})
		e.step(func(x *runtime.Run) error { return x.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
		e.step(func(x *runtime.Run) error { return x.CompleteCleanup() })
		if got := r.get(task.ID).Progress; got.Status != StatusRunning {
			t.Fatalf("setup: the projection should lag: %+v", got)
		}
		second, err := rerun(first)
		if err != nil || second.Progress.RunID == first {
			t.Fatalf("rerun = %+v, %v", second.Progress, err)
		}
		again, err := rerun(first) // the repeat after a lost response
		if err != nil || again.Progress != second.Progress {
			t.Fatalf("a repeated rerun = %+v, %v", again.Progress, err)
		}
		if _, err := rerun(second.Progress.RunID); !errors.Is(err, ErrRunActive) {
			t.Fatalf("a rerun of the new queued run: %v, want active", err)
		}
		// The first run is retained, queryable and still finished.
		if rec, err := r.runs.Load(r.ctx, scopeA, first); err != nil || rec.Run.Status() != runtime.StatusSucceeded {
			t.Fatalf("the first run = %+v, %v", rec.Run.Status(), err)
		}
		if owner, _ := r.store.TaskOfRun(r.ctx, scopeA, first); owner != task.ID {
			t.Fatalf("the first run lost its task: %q", owner)
		}
	})

	t.Run("a task that never ran cannot be rerun", func(t *testing.T) {
		r := newRig(t)
		task := r.task("never")
		if _, err := r.svc.Rerun(r.ctx, RerunRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedRunID: "run_x"}); !errors.Is(err, ErrNoRun) {
			t.Fatalf("rerun = %v, want no run", err)
		}
	})

	t.Run("two concurrent selections leave exactly one current run", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() {
			return // PostgreSQL needs the run recorded for the task first: see the contract test
		}
		task := r.task("race")
		sel := func(run domain.RunID) error {
			_, err := r.store.Select(r.ctx, RunSelection{Scope: scopeA, TaskID: task.ID, Run: run, Progress: Progress{RunID: run, Status: StatusQueued, RunVersion: 1}})
			return err
		}
		if err := sel("run_one"); err != nil {
			t.Fatal(err)
		}
		if err := sel("run_two"); !errors.Is(err, ErrStaleRun) {
			t.Fatalf("a second first-run selection: %v, want stale", err)
		}
		if got := r.get(task.ID).Progress.RunID; got != "run_one" {
			t.Fatalf("current run = %s", got)
		}
	})
}

func countProgress(r *rig, status Status) int {
	n := 0
	for _, e := range progressEvents(r) {
		var d struct {
			Status Status `json:"status"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.Status == status {
			n++
		}
	}
	return n
}

// Test_unit_tasks_list holds listing to scope, status and paging.
func Test_unit_tasks_list(t *testing.T) {
	r := newRig(t)
	a, b, c := r.task("a"), r.task("b"), r.task("c")
	r.start(b)
	got, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "view")})
	if err != nil || len(got) != 3 || got[0].ID != a.ID || got[2].ID != c.ID {
		t.Fatalf("list = %v, %v", got, err)
	}
	queued, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "view"), Status: StatusQueued})
	if err != nil || len(queued) != 1 || queued[0].ID != b.ID {
		t.Fatalf("queued = %v, %v", queued, err)
	}
	page, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "view"), Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %v, %v", page, err)
	}
	rest, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "view"), After: page[1].ID})
	if err != nil || len(rest) != 1 || rest[0].ID != c.ID {
		t.Fatalf("rest = %v, %v", rest, err)
	}
	if other, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeC, "eng")}); err != nil || len(other) != 0 {
		t.Fatalf("another project lists %v, %v", other, err)
	}
	if _, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "stranger")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stranger lists: %v", err)
	}
	if _, err := r.svc.List(r.ctx, ListRequest{Caller: r.caller(scopeA, "view"), Status: "bogus"}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Fatalf("a bad status: %v", err)
	}
}
