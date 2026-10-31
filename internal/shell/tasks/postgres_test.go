package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/pipelines"
	runstatepg "github.com/optimiware/kodama/internal/shell/runstate/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// The PostgreSQL checks run outside the unit layer (go test without -tags unit,
// with KODAMA_REQUIRE_POSTGRES=1 and KODAMA_TEST_POSTGRES_ADMIN_URL set). A
// skipped database is not evidence: newDatabase skips only when neither is set
// and fails when KODAMA_REQUIRE_POSTGRES=1.

// database is one migrated database. owner seeds and inspects as the migration
// role; rt is the pool of the non-owner runtime role every adapter runs on, so
// row-level security, forced RLS and the column grants apply as in the product.
type database struct {
	owner *pgxpool.Pool
	rt    *pgxpool.Pool
}

func newDatabase(t *testing.T) *database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgtest.New(t)
	d := &database{owner: db.MigrationPool(ctx, 4), rt: db.RuntimePool(ctx, 20)}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, d.owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d.exec(t, `INSERT INTO tenants (id, key) VALUES ('t1', 't1'), ('t2', 't2')`)
	for _, s := range []domain.Scope{scopeA, scopeB, scopeC} {
		d.exec(t, `INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ($1, $2::text, $2::text, '{}')`, string(s.TenantID), string(s.ProjectID))
	}
	return d
}

func (d *database) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := d.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

func (d *database) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// postgresRig builds a rig over a fresh database: the pipeline registry, the
// kernel run-state store and the task store, all unmodified adapters on the
// runtime role. Runs are admitted by the kernel store with their task, as in the
// product; nothing here writes a run or links one to its task.
func postgresRig(t *testing.T) *rig {
	t.Helper()
	d := newDatabase(t)
	clock := kernelcontracts.NewClock()
	runs, err := runstatepg.New(d.rt, clock)
	if err != nil {
		t.Fatal(err)
	}
	return assemble(t, clock, NewPostgres(d.rt), nil, runs, pipelines.New(pipelines.NewPostgres(d.rt)), d.owner)
}

// Test_postgres_tasks_scenarios runs every service-level scenario of the unit
// suite (selection, projection versioning, start and rerun, inbox, resolution)
// over real PostgreSQL adapters. Scenarios that need the memory adapter's fault
// injection log it and return.
func Test_postgres_tasks_scenarios(t *testing.T) {
	if layer.Unit {
		t.Log("the PostgreSQL checks run outside the unit layer: go test without -tags unit, with KODAMA_REQUIRE_POSTGRES=1")
		return
	}
	pgtest.New(t) // require or skip once, before any scenario starts
	prev := buildRig
	buildRig = postgresRig
	defer func() { buildRig = prev }()
	for _, s := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"pipeline_selection", Test_unit_tasks_pipeline_selection},
		{"projection_versioning", Test_unit_tasks_projection_versioning},
		{"start_rerun", Test_unit_tasks_start_rerun},
		{"list", Test_unit_tasks_list},
		{"inbox_visibility", Test_unit_tasks_inbox_visibility},
		{"resolution", Test_unit_tasks_resolution},
	} {
		t.Run(s.name, s.run)
	}
}

// seedPipelines gives every scope the pipeline "pipe1" and returns its IDs.
func (d *database) seedPipelines(t *testing.T) map[domain.Scope]domain.PipelineID {
	t.Helper()
	out := map[domain.Scope]domain.PipelineID{}
	for _, s := range []domain.Scope{scopeA, scopeB, scopeC} {
		id := domain.PipelineID("pipe_" + string(s.TenantID) + "_" + string(s.ProjectID))
		d.exec(t, `INSERT INTO pipelines (tenant_id, project_id, id, name, definition, source) VALUES ($1, $2, $3, 'pipe1', '{}', 'api')`,
			string(s.TenantID), string(s.ProjectID), string(id))
		out[s] = id
	}
	return out
}

// admitRun admits a run for the task through the kernel's PostgreSQL store, the
// production path that records the run as the task's.
func (d *database) admitRun(t *testing.T, scope domain.Scope, pipeline domain.PipelineID, task domain.TaskID, run domain.RunID) {
	t.Helper()
	runs, err := runstatepg.New(d.rt, kernelcontracts.NewClock())
	if err != nil {
		t.Fatal(err)
	}
	plan := solePlan()
	r, err := runtime.NewRun(scope, run, pipeline, plan, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Admit(context.Background(), runtime.Admission{Run: r, Plan: plan, Task: &runtime.TaskRef{Scope: scope, ID: task}}); err != nil {
		t.Fatalf("admit run %s: %v", run, err)
	}
}

// Test_postgres_tasks_store holds the PostgreSQL task store to the Store port's
// contract and to the guards of the schema: scope isolation under row-level
// security, the content version, the one current run, the projection guard with
// concurrent writers, the inbox candidate query and the composite ownership keys.
func Test_postgres_tasks_store(t *testing.T) {
	if layer.Unit {
		t.Log("the PostgreSQL checks run outside the unit layer: go test without -tags unit, with KODAMA_REQUIRE_POSTGRES=1")
		return
	}
	d := newDatabase(t)
	pipes := d.seedPipelines(t)
	ctx := t.Context()
	store := NewPostgres(d.rt)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	newTask := func(t *testing.T, scope domain.Scope, id string, c Content) Task {
		t.Helper()
		c, err := c.Normalized()
		if err != nil {
			t.Fatal(err)
		}
		task := Task{Scope: scope, ID: domain.TaskID(id), Content: c, PipelineID: pipes[scope], Version: 1,
			Progress: Progress{Status: StatusPending}, Created: now, Updated: now}
		if err := store.Create(ctx, task, "eng"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		return task
	}
	selectRun := func(t *testing.T, task Task, expected, run domain.RunID, p Progress) (Task, error) {
		t.Helper()
		d.admitRun(t, task.Scope, task.PipelineID, task.ID, run)
		return store.Select(ctx, RunSelection{Scope: task.Scope, TaskID: task.ID, Expected: expected, Run: run, Progress: p})
	}

	t.Run("a task round-trips with its content, selected pipeline and creation event", func(t *testing.T) {
		parent := newTask(t, scopeA, "st_parent", Content{Title: "parent"})
		task := newTask(t, scopeA, "st_child", Content{Title: "child", Body: "body", ParentID: parent.ID, AssignedRole: RoleReviewer,
			Branch: "main", Context: []byte(`{"b":1,"a":[1,2]}`)})
		got, err := store.Get(ctx, scopeA, task.ID)
		if err != nil || got.Title != "child" || got.ParentID != parent.ID || got.PipelineID != pipes[scopeA] || got.Version != 1 ||
			got.Progress.Status != StatusPending || got.Progress.Version != 0 || !got.Created.Equal(now) || !sameContent(got.Content, task.Content) {
			t.Fatalf("get = %+v, %v", got, err)
		}
		if err := store.Create(ctx, task, "eng"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a second create of the id: %v, want a conflict", err)
		}
		if n := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_type = 'task' AND resource_id = $1 AND event_type = 'task.created'`, string(task.ID)); n != 1 {
			t.Fatalf("%d task.created events, want one", n)
		}
		// The legacy workflow columns are not used: the row carries none of their state.
		if n := d.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND parked_at IS NULL AND blocked_by_id IS NULL AND resume_state IS NULL AND resolution IS NULL AND resolved_at IS NULL`, string(task.ID)); n != 1 {
			t.Fatal("the legacy workflow columns were written")
		}
	})

	t.Run("the pipeline, parent and repository must be in the task's project", func(t *testing.T) {
		bad := Task{Scope: scopeA, ID: "st_bad_pipe", Content: Content{Title: "x", Context: []byte(`{}`)}, PipelineID: pipes[scopeC], Version: 1,
			Progress: Progress{Status: StatusPending}, Created: now, Updated: now}
		if err := store.Create(ctx, bad, "eng"); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("another project's pipeline: %v, want a refusal", err)
		}
		other := newTask(t, scopeC, "st_in_c", Content{Title: "c"})
		bad.ID, bad.PipelineID, bad.ParentID = "st_bad_parent", pipes[scopeA], other.ID
		if err := store.Create(ctx, bad, "eng"); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("another project's parent: %v, want a refusal", err)
		}
		bad.ID, bad.ParentID, bad.RepositoryID = "st_bad_repo", "", "no_such_repo"
		if err := store.Create(ctx, bad, "eng"); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("an unknown repository: %v, want a refusal", err)
		}
		bad.ID, bad.RepositoryID, bad.Scope = "st_bad_project", "", domain.Scope{TenantID: "t1", ProjectID: "nope"}
		if err := store.Create(ctx, bad, "eng"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unknown project: %v, want not found", err)
		}
		if n := d.count(t, `SELECT count(*) FROM tasks WHERE id LIKE 'st_bad_%'`); n != 0 {
			t.Fatalf("%d refused tasks were written", n)
		}
		if n := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_id LIKE 'st_bad_%'`); n != 0 {
			t.Fatalf("%d events of refused tasks", n)
		}
	})

	t.Run("a task is invisible to another project and another tenant", func(t *testing.T) {
		task := newTask(t, scopeA, "st_private", Content{Title: "private"})
		for _, s := range []domain.Scope{scopeB, scopeC} {
			if _, err := store.Get(ctx, s, task.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("get from %s: %v", s, err)
			}
			if _, err := store.UpdateContent(ctx, s, task.ID, 1, Content{Title: "x", Context: []byte(`{}`)}, now, "eng"); !errors.Is(err, ErrNotFound) {
				t.Errorf("update from %s: %v", s, err)
			}
			ts, err := store.List(ctx, ListQuery{Scope: s, Limit: 100})
			if err != nil {
				t.Errorf("list from %s: %v", s, err)
			}
			for _, x := range ts {
				if x.ID == task.ID {
					t.Errorf("list from %s shows %s", s, x.ID)
				}
			}
			if _, err := store.TaskOfRun(ctx, s, "anything"); !errors.Is(err, ErrNotFound) {
				t.Errorf("run lookup from %s: %v", s, err)
			}
		}
		// A row-level-security probe: with the tenant set to t2 the table shows nothing of t1.
		var n int
		err := storage.WithTenant(ctx, d.rt, "t2", func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant_id = 't1'`).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Fatalf("t2 sees %d of t1's tasks: %v", n, err)
		}
	})

	t.Run("content edits use their own version and never touch execution", func(t *testing.T) {
		task := newTask(t, scopeA, "st_edit", Content{Title: "v1"})
		c := Content{Title: "v2", Body: "more", Branch: "dev", AssignedRole: RoleEngineer, Context: []byte(`{"k":"v"}`)}
		got, err := store.UpdateContent(ctx, scopeA, task.ID, 1, c, now.Add(time.Minute), "editor")
		if err != nil || got.Version != 2 || got.Title != "v2" || got.Branch != "dev" || got.Progress != task.Progress || got.PipelineID != task.PipelineID {
			t.Fatalf("update = %+v, %v", got, err)
		}
		if _, err := store.UpdateContent(ctx, scopeA, task.ID, 1, c, now, "editor"); !errors.Is(err, ErrContentConflict) {
			t.Fatalf("a stale update: %v", err)
		}
		if _, err := store.UpdateContent(ctx, scopeA, "st_missing", 1, c, now, "editor"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a missing task: %v", err)
		}
		// The edit committed one event, with its actor, in the same transaction.
		if n := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_id = $1 AND event_type = 'task.content_changed'
  AND actor_type = 'user' AND actor_id = 'editor' AND payload->>'contentVersion' = '2' AND NOT payload ? 'title'`, string(task.ID)); n != 1 {
			t.Fatalf("%d content events with the editor and version, want one", n)
		}
		// Refusals wrote none.
		if n := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_id = $1 AND event_type = 'task.content_changed'`, string(task.ID)); n != 1 {
			t.Fatalf("%d content events after the refusals, want one", n)
		}
	})

	t.Run("an edit whose event cannot be persisted rolls back the content and the version", func(t *testing.T) {
		task := newTask(t, scopeA, "st_rollback", Content{Title: "before"})
		d.exec(t, `CREATE FUNCTION t184_refuse_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'event store down' USING ERRCODE = '23514'; END $$`)
		d.exec(t, `CREATE TRIGGER t184_refuse BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.event_type = 'task.content_changed') EXECUTE FUNCTION t184_refuse_event()`)
		_, err := store.UpdateContent(ctx, scopeA, task.ID, 1, Content{Title: "after", Context: []byte(`{}`)}, now, "editor")
		d.exec(t, `DROP TRIGGER t184_refuse ON audit_events`)
		d.exec(t, `DROP FUNCTION t184_refuse_event()`)
		if err == nil {
			t.Fatal("the edit succeeded without its event")
		}
		got, gerr := store.Get(ctx, scopeA, task.ID)
		if gerr != nil || got.Title != "before" || got.Version != 1 {
			t.Fatalf("the failed edit persisted: %+v, %v", got, gerr)
		}
		if n := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_id = $1 AND event_type = 'task.content_changed'`, string(task.ID)); n != 0 {
			t.Fatalf("%d events of a rolled-back edit", n)
		}
		// With the event store back, the same edit commits at the same version.
		if got, err := store.UpdateContent(ctx, scopeA, task.ID, 1, Content{Title: "after", Context: []byte(`{}`)}, now, "editor"); err != nil || got.Version != 2 {
			t.Fatalf("the retry: %+v, %v", got, err)
		}
	})

	t.Run("exactly one current run, bound to its own task and project", func(t *testing.T) {
		a := newTask(t, scopeA, "st_run_a", Content{Title: "a"})
		b := newTask(t, scopeA, "st_run_b", Content{Title: "b"})
		first := Progress{RunID: "st_run_1", Status: StatusQueued, RunVersion: 3}
		got, err := selectRun(t, a, "", "st_run_1", first)
		if err != nil || got.Progress.RunID != "st_run_1" || got.Progress.Version != 1 || got.Progress.RunVersion != 3 || got.Progress.Status != StatusQueued {
			t.Fatalf("select = %+v, %v", got.Progress, err)
		}
		again, err := store.Select(ctx, RunSelection{Scope: scopeA, TaskID: a.ID, Run: "st_run_1", Progress: first})
		if err != nil || again.Progress != got.Progress {
			t.Fatalf("the replay of a selection: %+v, %v", again.Progress, err)
		}
		if _, err := selectRun(t, a, "", "st_run_other", Progress{RunID: "st_run_other", Status: StatusQueued, RunVersion: 1}); !errors.Is(err, ErrStaleRun) {
			t.Fatalf("a second first-run selection: %v, want stale", err)
		}
		// A run admitted for another task cannot become this task's current run.
		d.admitRun(t, scopeA, pipes[scopeA], b.ID, "st_run_of_b")
		if _, err := store.Select(ctx, RunSelection{Scope: scopeA, TaskID: a.ID, Expected: "st_run_1", Run: "st_run_of_b",
			Progress: Progress{RunID: "st_run_of_b", Status: StatusQueued, RunVersion: 1}}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("another task's run: %v, want a refusal", err)
		}
		// A run that was never admitted at all is refused the same way.
		if _, err := store.Select(ctx, RunSelection{Scope: scopeA, TaskID: a.ID, Expected: "st_run_1", Run: "st_run_nowhere",
			Progress: Progress{RunID: "st_run_nowhere", Status: StatusQueued, RunVersion: 1}}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("an unrecorded run: %v, want a refusal", err)
		}
		if got, _ := store.Get(ctx, scopeA, a.ID); got.Progress.RunID != "st_run_1" {
			t.Fatalf("a refused selection changed the current run: %+v", got.Progress)
		}
		// The next run: the version restarts for the new run, the progress version does not.
		second, err := selectRun(t, a, "st_run_1", "st_run_2", Progress{RunID: "st_run_2", Status: StatusQueued, RunVersion: 1})
		if err != nil || second.Progress.Version != 2 || second.Progress.RunVersion != 1 {
			t.Fatalf("the next run = %+v, %v", second.Progress, err)
		}
		if owner, err := store.TaskOfRun(ctx, scopeA, "st_run_1"); err != nil || owner != a.ID {
			t.Fatalf("the earlier run left its task's history: %v %v", owner, err)
		}
		if n := d.count(t, `SELECT count(*) FROM runs WHERE task_id = $1 AND id IN ('st_run_1', 'st_run_2')`, string(a.ID)); n != 2 {
			t.Fatalf("%d of the task's two runs kept, want both", n)
		}
	})

	t.Run("the projection guard holds with concurrent writers", func(t *testing.T) {
		task := newTask(t, scopeA, "st_guard", Content{Title: "g"})
		if _, err := selectRun(t, task, "", "st_guard_run", Progress{RunID: "st_guard_run", Status: StatusQueued, RunVersion: 1}); err != nil {
			t.Fatal(err)
		}
		obs := func(v domain.Version) Progress {
			return Progress{RunID: "st_guard_run", Status: StatusRunning, Step: domain.StepID(fmt.Sprintf("s%d", v)), RunVersion: v}
		}
		const n = 24
		var wg sync.WaitGroup
		verdicts := make([]Verdict, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, v, err := store.Advance(ctx, scopeA, task.ID, obs(domain.Version(i+2)), false)
				if err != nil {
					t.Errorf("advance %d: %v", i, err)
				}
				verdicts[i] = v
			}()
		}
		wg.Wait()
		got, _ := store.Get(ctx, scopeA, task.ID)
		if got.Progress.RunVersion != n+1 || got.Progress.Step != domain.StepID(fmt.Sprintf("s%d", n+1)) {
			t.Fatalf("the newest observation did not win: %+v", got.Progress)
		}
		// Every applied observation is a distinct, increasing version, one event each.
		applied := 0
		for _, v := range verdicts {
			if v == VerdictApplied {
				applied++
			}
		}
		if applied < 1 || got.Progress.Version != domain.Version(applied)+1 {
			t.Fatalf("%d applied, progress version %d", applied, got.Progress.Version)
		}
		if ev := d.count(t, `SELECT count(*) FROM audit_events WHERE resource_id = $1 AND event_type = 'task.progress_changed'`, string(task.ID)); ev != applied+1 {
			t.Fatalf("%d progress events for %d applied observations and the selection", ev, applied)
		}
		// Older and replayed observations change nothing.
		for _, v := range []domain.Version{1, 5, n + 1} {
			after, verdict, err := store.Advance(ctx, scopeA, task.ID, obs(v), true)
			if err != nil || verdict == VerdictApplied && v != n+1 || after.Progress != got.Progress {
				t.Fatalf("observation %d: %v %+v, %v", v, verdict, after.Progress, err)
			}
		}
		// Another run's observation, an unrecorded task and another scope are refused.
		if _, v, _ := store.Advance(ctx, scopeA, task.ID, Progress{RunID: "other", RunVersion: 999, Status: StatusFailed}, true); v != VerdictStale {
			t.Fatalf("another run: %v", v)
		}
		if _, _, err := store.Advance(ctx, scopeB, task.ID, obs(500), false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another tenant: %v", err)
		}
	})

	t.Run("the database refuses a projection that goes backwards", func(t *testing.T) {
		task := newTask(t, scopeA, "st_trigger", Content{Title: "t"})
		if _, err := selectRun(t, task, "", "st_trigger_run", Progress{RunID: "st_trigger_run", Status: StatusRunning, RunVersion: 9}); err != nil {
			t.Fatal(err)
		}
		for name, sql := range map[string]string{
			"progress version decreases": `UPDATE tasks SET progress_version = 0 WHERE id = 'st_trigger'`,
			"progress version cleared":   `UPDATE tasks SET progress_version = NULL, progress_run_version = NULL WHERE id = 'st_trigger'`,
			"run version decreases":      `UPDATE tasks SET progress_run_version = 1 WHERE id = 'st_trigger'`,
			"pipeline changes":           `UPDATE tasks SET pipeline_id = '` + string(pipes[scopeA]) + `x' WHERE id = 'st_trigger'`,
		} {
			err := storage.WithTenant(ctx, d.rt, "t1", func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql)
				return err
			})
			if err == nil {
				t.Errorf("%s: the database accepted it", name)
			}
		}
		got, _ := store.Get(ctx, scopeA, task.ID)
		if got.Progress.Version != 1 || got.Progress.RunVersion != 9 {
			t.Fatalf("the progress changed: %+v", got.Progress)
		}
	})

	t.Run("the inbox candidate query lists tasks waiting for the caller's roles, by project, in pages", func(t *testing.T) {
		mk := func(scope domain.Scope, id, role string) {
			task := newTask(t, scope, id, Content{Title: id})
			run := domain.RunID(id + "_run")
			p := Progress{RunID: run, Status: StatusWaiting, Step: "review", WaitingFor: role, RunVersion: 1}
			if role == "" {
				p = Progress{RunID: run, Status: StatusRunning, Step: "build", RunVersion: 1}
			}
			if _, err := selectRun(t, task, "", run, p); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
		mk(scopeA, "ib_1", "reviewer")
		mk(scopeA, "ib_2", "reviewer")
		mk(scopeA, "ib_3", "operator")
		mk(scopeA, "ib_4", "")
		mk(scopeA, "ib_5", "reviewer")
		mk(scopeC, "ib_c", "reviewer")
		ids := func(ts []Task) string {
			var s []string
			for _, x := range ts {
				s = append(s, string(x.ID))
			}
			return strings.Join(s, ",")
		}
		got, err := store.Waiting(ctx, scopeA, []string{"reviewer"}, "", 10)
		if err != nil || ids(got) != "ib_1,ib_2,ib_5" {
			t.Fatalf("reviewer = %s, %v", ids(got), err)
		}
		got, err = store.Waiting(ctx, scopeA, []string{"reviewer", "operator"}, "", 10)
		if err != nil || ids(got) != "ib_1,ib_2,ib_3,ib_5" {
			t.Fatalf("reviewer+operator = %s, %v", ids(got), err)
		}
		got, err = store.Waiting(ctx, scopeA, []string{"reviewer"}, "ib_1", 1)
		if err != nil || ids(got) != "ib_2" {
			t.Fatalf("page = %s, %v", ids(got), err)
		}
		for _, roles := range [][]string{{"viewer"}, {}, {"admin", "engineer"}} {
			if got, err := store.Waiting(ctx, scopeA, roles, "", 10); err != nil || len(got) != 0 {
				t.Fatalf("roles %v: %s, %v", roles, ids(got), err)
			}
		}
		if got, err := store.Waiting(ctx, scopeB, []string{"reviewer"}, "", 10); err != nil || len(got) != 0 {
			t.Fatalf("another tenant: %s, %v", ids(got), err)
		}
		if got, err := store.Waiting(ctx, scopeC, []string{"reviewer"}, "", 10); err != nil || ids(got) != "ib_c" {
			t.Fatalf("another project: %s, %v", ids(got), err)
		}
	})

	t.Run("status and parent filters list by id", func(t *testing.T) {
		got, err := store.List(ctx, ListQuery{Scope: scopeA, Status: StatusWaiting, Limit: 100})
		if err != nil || len(got) != 4 {
			t.Fatalf("waiting = %d, %v", len(got), err)
		}
		pending, err := store.List(ctx, ListQuery{Scope: scopeA, Status: StatusPending, ParentID: "st_parent", Limit: 100})
		if err != nil || len(pending) != 1 || pending[0].ID != "st_child" {
			t.Fatalf("pending children = %+v, %v", pending, err)
		}
		page, err := store.List(ctx, ListQuery{Scope: scopeA, After: "st_child", Limit: 2})
		if err != nil || len(page) != 2 || page[0].ID <= "st_child" || page[0].ID >= page[1].ID {
			t.Fatalf("keyset page = %+v, %v", page, err)
		}
	})
}
