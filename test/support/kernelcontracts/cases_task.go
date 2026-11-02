package kernelcontracts

import (
	"encoding/json"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Tasks the suite admits runs for. An adapter whose store keeps tasks as rows
// (a database) must have both in the project of SeededTasks' scope before each
// case; one that keeps no tasks needs nothing.
const (
	Task1 = domain.TaskID("task1")
	Task2 = domain.TaskID("task2")
)

// SeededTasks lists the tasks every case may name, all in the suite's first
// project (tenant t1, project p1).
func SeededTasks() []runtime.TaskRef {
	return []runtime.TaskRef{{Scope: scopeA, ID: Task1}, {Scope: scopeA, ID: Task2}}
}

func (e *Env) admitFor(scope domain.Scope, id domain.RunID, task *runtime.TaskRef) (runtime.RunRecord, error) {
	e.T.Helper()
	a := e.admission(scope, id, reviewPlan(), 2)
	a.Task = task
	return e.S.Admit(e.Ctx, a)
}

func taskCases() []Case {
	own := func(id domain.TaskID) *runtime.TaskRef { return &runtime.TaskRef{Scope: scopeA, ID: id} }
	return []Case{
		{"admit/task_is_recorded_and_reported", GroupAdmission, func(t TB, e *Env) {
			rec, err := e.admitFor(scopeA, run1, own(Task1))
			e.ok(err, "admit a run for a task")
			e.expect(rec.Task != nil && *rec.Task == *own(Task1), "the admitted record reports its task: %+v", rec.Task)
			e.expect(rec.Validate() == nil, "a record with its task is coherent")
			loaded := e.load(scopeA, run1)
			e.expect(loaded.Task != nil && *loaded.Task == *own(Task1), "Load reports the task: %+v", loaded.Task)
			view, err := e.S.View(e.Ctx, scopeA, run1)
			e.ok(err, "view the run")
			e.expect(view.Task != nil && *view.Task == *own(Task1), "View reports the task: %+v", view.Task)

			// The task is fixed at admission: claiming and committing keep it.
			claimed, err := e.S.Claim(e.Ctx, e.claimReq(scopeA, run1, "w1", runtime.ClaimExecute, ttl), e.Clock.Now())
			e.ok(err, "claim the run")
			e.expect(claimed.Task != nil && *claimed.Task == *own(Task1), "a claim keeps the task")

			// A task owns several runs: each run is its own record.
			second, err := e.admitFor(scopeA, run2, own(Task1))
			e.ok(err, "admit a second run for the same task")
			e.expect(second.Task != nil && *second.Task == *own(Task1), "the second run reports the same task")
		}},
		{"admit/task_of_another_project_is_refused", GroupAdmission, func(t TB, e *Env) {
			for name, foreign := range map[string]*runtime.TaskRef{
				"another project": {Scope: scopeC, ID: Task1},
				"another tenant":  {Scope: scopeB, ID: Task1},
			} {
				_, err := e.admitFor(scopeA, run1, foreign)
				e.isErr(err, domain.ErrInvalidValue, "a run admitted for a task of "+name)
				_, err = e.S.Load(e.Ctx, scopeA, run1)
				e.notFound(err, "the refused run of a task of "+name)
			}
			_, err := e.admitFor(scopeA, run1, &runtime.TaskRef{Scope: scopeA, ID: "bad id"})
			e.isErr(err, domain.ErrInvalidValue, "a run admitted for an invalid task id")
			_, err = e.S.Load(e.Ctx, scopeA, run1)
			e.notFound(err, "the run of an invalid task id")
		}},
		{"admit/task_free_run_is_unchanged", GroupAdmission, func(t TB, e *Env) {
			rec := e.admit(scopeA, run1, reviewPlan())
			e.expect(rec.Task == nil, "a task-free run reports no task: %+v", rec.Task)
			e.expect(e.load(scopeA, run1).Task == nil, "Load reports no task")
			view, err := e.S.View(e.Ctx, scopeA, run1)
			e.ok(err, "view the task-free run")
			e.expect(view.Task == nil, "View reports no task")
			raw, err := json.Marshal(rec)
			e.ok(err, "encode the record")
			e.expect(!strings.Contains(string(raw), `"task"`), "a task-free record has no task field: %s", raw)
		}},
		{"admit/repeat_must_name_the_same_task", GroupAdmission, func(t TB, e *Env) {
			first, err := e.admitFor(scopeA, run1, own(Task1))
			e.ok(err, "admit for task 1")
			again, err := e.admitFor(scopeA, run1, own(Task1))
			e.ok(err, "repeat for the same task")
			e.expect(again.Task != nil && *again.Task == *own(Task1), "the repeat returns the stored record")
			_, err = e.admitFor(scopeA, run1, own(Task2))
			e.isErr(err, domain.ErrConflict, "a repeat naming another task")
			_, err = e.admitFor(scopeA, run1, nil)
			e.isErr(err, domain.ErrConflict, "a repeat naming no task")
			e.unchanged(first, 1, "the conflicting repeats")
			free := e.admit(scopeA, run2, reviewPlan())
			_, err = e.admitFor(scopeA, run2, own(Task1))
			e.isErr(err, domain.ErrConflict, "a repeat that adds a task")
			e.unchanged(free, 1, "the repeat that adds a task")
		}},
		{"admit/task_reference_is_copied", GroupAdmission, func(t TB, e *Env) {
			task := own(Task1)
			rec, err := e.admitFor(scopeA, run1, task)
			e.ok(err, "admit for a task")
			task.ID = Task2
			rec.Task.ID = Task2
			e.expect(e.load(scopeA, run1).Task.ID == Task1, "neither the caller's reference nor the returned record changes the store")
			loaded := e.load(scopeA, run1)
			loaded.Task.ID = Task2
			e.expect(e.load(scopeA, run1).Task.ID == Task1, "a loaded record shares no task reference with the store")
		}},
	}
}
