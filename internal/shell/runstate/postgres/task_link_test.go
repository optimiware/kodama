package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

// Test_unit_runstate_postgres_task_link proves a run's owning task is written by
// the insert that creates the run and read back from the row, and that the
// database, not a test, then enforces the task's current run: the non-owner
// runtime role admits through the adapter's own path and sets
// tasks.current_run_id with no owner intervention, a run is current only for its
// own task, and the role cannot rewrite or widen the link afterwards.
//
// The database subtests follow the contract test's rule: not registered in the
// unit layer, required (never skipped) with KODAMA_REQUIRE_POSTGRES=1.
func Test_unit_runstate_postgres_task_link(t *testing.T) {
	t.Run("the run insert writes the task in the same statement and the read returns it", func(t *testing.T) {
		if !strings.Contains(stmtInsertRun, "task_id") || !strings.Contains(stmtInsertRun, "INSERT INTO runs") {
			t.Errorf("the run insert does not write task_id: %s", stmtInsertRun)
		}
		if !strings.Contains(stmtGetRun, "task_id") {
			t.Errorf("the run read does not return task_id: %s", stmtGetRun)
		}
		if strings.Contains(stmtUpdateRun, "task_id") {
			t.Errorf("a run update must never rewrite the task: %s", stmtUpdateRun)
		}
	})

	withDatabase(t, func(t *testing.T) {
		e := newEnv(t)
		task1 := &runtime.TaskRef{Scope: scope1, ID: kernelcontracts.Task1}
		task2 := &runtime.TaskRef{Scope: scope1, ID: kernelcontracts.Task2}
		admitFor := func(t *testing.T, s *Store, clock *kernelcontracts.Clock, id domain.RunID, task *runtime.TaskRef) (runtime.RunRecord, error) {
			a := admission(t, scope1, id, builderPlan(), clock.Now())
			a.Task = task
			return s.Admit(context.Background(), a)
		}
		taskColumn := func(id domain.RunID) (task *string) {
			must(t, e.owner.QueryRow(context.Background(), `SELECT task_id FROM runs WHERE tenant_id = 't1' AND project_id = 'p1' AND id = $1`, id).Scan(&task), "read runs.task_id")
			return task
		}
		code := func(err error) string {
			var pe *pgconn.PgError
			if err == nil || !asPg(err, &pe) {
				return ""
			}
			return pe.Code
		}

		t.Run("the runtime role holds exactly the grants the link needs", func(t *testing.T) {
			priv := func(table, column, privilege string) bool {
				var ok bool
				must(t, e.owner.QueryRow(context.Background(), `SELECT has_column_privilege($1, $2, $3, $4)`, e.db.RuntimeRole, table, column, privilege).Scan(&ok),
					"read the column privilege")
				return ok
			}
			for _, c := range []struct {
				table, column, privilege string
				want                     bool
			}{
				{"runs", "task_id", "INSERT", true},
				{"runs", "task_id", "UPDATE", false},
				{"runs", "pipeline_id", "UPDATE", false},
				{"runs", "definition_snapshot", "UPDATE", false},
				{"tasks", "current_run_id", "UPDATE", true},
				{"tasks", "id", "UPDATE", false},
			} {
				if got := priv(c.table, c.column, c.privilege); got != c.want {
					t.Errorf("%s %s.%s: %v, want %v", c.privilege, c.table, c.column, got, c.want)
				}
			}
		})

		t.Run("a run admitted for a task persists it and every read reports it", func(t *testing.T) {
			e.reset()
			clock := kernelcontracts.NewClock()
			s := e.store(clock)
			rec, err := admitFor(t, s, clock, "run1", task1)
			must(t, err, "admit for a task")
			if rec.Task == nil || *rec.Task != *task1 {
				t.Fatalf("the admitted record reports %+v", rec.Task)
			}
			if got := taskColumn("run1"); got == nil || *got != string(task1.ID) {
				t.Fatalf("runs.task_id = %v", got)
			}
			// A replacement store reads it from the row alone, through Load and View.
			fresh := e.store(clock)
			loaded, err := fresh.Load(context.Background(), scope1, "run1")
			must(t, err, "load")
			view, err := fresh.View(context.Background(), scope1, "run1")
			must(t, err, "view")
			if loaded.Task == nil || *loaded.Task != *task1 || view.Task == nil || *view.Task != *task1 {
				t.Fatalf("load %+v, view %+v", loaded.Task, view.Task)
			}
			// A claim and the writes after it keep the link.
			claimed, err := fresh.Claim(context.Background(), runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "w1",
				Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
			must(t, err, "claim")
			if claimed.Task == nil || *claimed.Task != *task1 || taskColumn("run1") == nil {
				t.Fatalf("the claim lost the task: %+v", claimed.Task)
			}
		})

		t.Run("a task-free run stores null and reports none", func(t *testing.T) {
			e.reset()
			clock := kernelcontracts.NewClock()
			s := e.store(clock)
			rec, err := admitFor(t, s, clock, "run1", nil)
			must(t, err, "admit without a task")
			if rec.Task != nil || taskColumn("run1") != nil {
				t.Fatalf("a task-free run has a task: %+v / %v", rec.Task, taskColumn("run1"))
			}
		})

		t.Run("a task that is not in the project refuses the admission and records nothing", func(t *testing.T) {
			e.reset()
			e.ownerExec(`INSERT INTO tasks (tenant_id, project_id, id, title, state) VALUES ('t1', 'p2', 'otherp', 'other', 'open')`)
			clock := kernelcontracts.NewClock()
			s := e.store(clock)
			for name, task := range map[string]*runtime.TaskRef{
				"absent":                       {Scope: scope1, ID: "absent"},
				"a task of project p2":         {Scope: domain.Scope{TenantID: "t1", ProjectID: "p2"}, ID: "otherp"},
				"an id that exists only in p2": {Scope: scope1, ID: "otherp"},
			} {
				_, err := admitFor(t, s, clock, "run1", task)
				isErr(t, err, domain.ErrInvalidValue, name)
			}
			if n := e.count(`SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM run_steps) + (SELECT count(*) FROM audit_events)`); n != 0 {
				t.Fatalf("the refused admissions left %d rows", n)
			}
		})

		t.Run("a repeat must name the same task", func(t *testing.T) {
			e.reset()
			clock := kernelcontracts.NewClock()
			s := e.store(clock)
			_, err := admitFor(t, s, clock, "run1", task1)
			must(t, err, "admit")
			again, err := admitFor(t, s, clock, "run1", task1)
			must(t, err, "repeat")
			if again.Task == nil || *again.Task != *task1 {
				t.Fatalf("the repeat reports %+v", again.Task)
			}
			for name, task := range map[string]*runtime.TaskRef{"another task": task2, "no task": nil} {
				_, err = admitFor(t, s, clock, "run1", task)
				isErr(t, err, domain.ErrConflict, name)
			}
			if got := taskColumn("run1"); got == nil || *got != string(task1.ID) {
				t.Fatalf("a conflicting repeat changed runs.task_id to %v", got)
			}
		})

		t.Run("the runtime role sets and validates tasks.current_run_id with no owner intervention", func(t *testing.T) {
			e.reset()
			clock := kernelcontracts.NewClock()
			s := e.store(clock)
			_, err := admitFor(t, s, clock, "run1", task1)
			must(t, err, "admit the task's first run")
			_, err = admitFor(t, s, clock, "run2", task2)
			must(t, err, "admit another task's run")
			_, err = admitFor(t, s, clock, "run3", task1)
			must(t, err, "admit the task's rerun")
			_, err = admitFor(t, s, clock, "free", nil)
			must(t, err, "admit a task-free run")

			setCurrent := func(task, run, status string) error {
				return e.asRuntime("t1", func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE tasks SET pipeline_id = COALESCE(pipeline_id, 'pipe1'), current_run_id = $2, status = $3
						WHERE tenant_id = 't1' AND project_id = 'p1' AND id = $1`, task, run, status)
					return err
				})
			}
			must(t, setCurrent("task1", "run1", "running"), "start: the runtime role sets the task's current run")
			must(t, setCurrent("task1", "run3", "running"), "rerun: the current run moves to the task's new run")
			var current *string
			must(t, e.owner.QueryRow(context.Background(), `SELECT current_run_id FROM tasks WHERE tenant_id = 't1' AND project_id = 'p1' AND id = 'task1'`).Scan(&current), "read the task")
			if current == nil || *current != "run3" {
				t.Fatalf("current_run_id = %v", current)
			}

			// The key is the task and the run together.
			if c := code(setCurrent("task1", "run2", "running")); c != "23503" {
				t.Errorf("another task's run as current: code %q", c)
			}
			if c := code(setCurrent("task2", "free", "running")); c != "23503" {
				t.Errorf("a task-free run as current: code %q", c)
			}
			if c := code(setCurrent("task2", "absent", "running")); c != "23503" {
				t.Errorf("a missing run as current: code %q", c)
			}
			// The runtime role cannot rewrite the link: no column grant beyond the insert.
			for name, sql := range map[string]string{
				"retarget the run":     `UPDATE runs SET task_id = 'task2' WHERE tenant_id = 't1' AND project_id = 'p1' AND id = 'run1'`,
				"detach the run":       `UPDATE runs SET task_id = NULL WHERE tenant_id = 't1' AND project_id = 'p1' AND id = 'run1'`,
				"rewrite the tenant":   `UPDATE runs SET tenant_id = 't1' WHERE id = 'run1'`,
				"rewrite the pipeline": `UPDATE runs SET pipeline_id = 'pipe1' WHERE id = 'run1'`,
			} {
				err := e.asRuntime("t1", func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
				if c := code(err); c != "42501" {
					t.Errorf("%s: code %q, want 42501 (%v)", name, c, err)
				}
			}
			if got := taskColumn("run1"); got == nil || *got != "task1" {
				t.Errorf("runs.task_id of run1 is %v", got)
			}
			// A task of another project cannot own the run even for a direct insert.
			e.ownerExec(`INSERT INTO tasks (tenant_id, project_id, id, title, state) VALUES ('t1', 'p2', 'otherp', 'other', 'open') ON CONFLICT DO NOTHING`)
			err = e.asRuntime("t1", func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot, task_id) VALUES ('t1', 'p1', 'x', 'pipe1', '{}', 'otherp')`)
				return err
			})
			if c := code(err); c != "23503" {
				t.Errorf("a direct insert naming a task of another project: code %q (%v)", c, err)
			}
		})
	})
}
