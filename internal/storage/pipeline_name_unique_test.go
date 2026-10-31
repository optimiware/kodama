package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

const gitCommit = "0123456789abcdef0123456789abcdef01234567"

// priorSchema migrates a fresh database to the state before the expand
// migration (001 to 003) and seeds tenants, projects and repositories as the
// migration owner, as a deployed database at that checksum would hold them.
func priorSchema(t *testing.T) (*postgres.Database, *pgxpool.Pool, storage.MigrateOptions) {
	t.Helper()
	ctx := testCtx(t)
	db := postgres.New(t)
	pool := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	prior, err := storage.NewManifest(storage.Contribution{Module: "storage", Migrations: storage.BaseContribution().Migrations[:3]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, pool, prior, opts); err != nil {
		t.Fatalf("migrate prior schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO tenants (id, key) VALUES ('tenant-a', 'a');
INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ('tenant-a', 'pa1', 'pa1', '{}'), ('tenant-a', 'pa2', 'pa2', '{}');
INSERT INTO repositories (tenant_id, id, project_id, name) VALUES
  ('tenant-a', 'repo1', 'pa1', 'app'), ('tenant-a', 'repo2', 'pa1', 'lib');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, pool, opts
}

func fullManifest(t *testing.T) storage.Manifest { return baseManifest(t) }

func applied(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(testCtx(t), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(testCtx(t), `SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(testCtx(t), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// Test_pipeline_name_unique_postgres verifies stage 2 (validate and map) against
// real PostgreSQL (it skips in the unit layer): duplicate project-scoped
// pipeline names and ambiguous task-to-pipeline mappings refuse the migration
// whole, change nothing, and invent no pipeline; once resolved the migration
// applies and the constraint holds.
func Test_pipeline_name_unique_postgres(t *testing.T) {
	t.Run("constraint_after_migration", func(t *testing.T) {
		e := newEnv(t)
		e.exec(t, `INSERT INTO repositories (tenant_id, id, project_id, name) VALUES ('tenant-a', 'repo1', 'pa1', 'app'), ('tenant-a', 'repo2', 'pa1', 'lib')`)
		e.exec(t, `INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-a', 'pa1', 'api-sdlc', 'sdlc', 'api', '{}')`)
		// The baseline key includes the repository, so a repository definition of
		// the same name was admitted; the project-scoped key now refuses it.
		requireCode(t, tryExec(t, e, `INSERT INTO pipelines (tenant_id, project_id, id, repo_id, name, source, source_commit, definition)
VALUES ('tenant-a', 'pa1', 'git-sdlc', 'repo1', 'sdlc', 'git', '`+gitCommit+`', '{}')`), "23505", "repository definition of an API name")
		e.exec(t, `INSERT INTO pipelines (tenant_id, project_id, id, repo_id, name, source, source_commit, definition)
VALUES ('tenant-a', 'pa1', 'git-ci', 'repo1', 'ci', 'git', '`+gitCommit+`', '{}')`)
		requireCode(t, tryExec(t, e, `INSERT INTO pipelines (tenant_id, project_id, id, repo_id, name, source, source_commit, definition)
VALUES ('tenant-a', 'pa1', 'git-ci2', 'repo2', 'ci', 'git', '`+gitCommit+`', '{}')`), "23505", "same name from two repositories")
		requireCode(t, tryExec(t, e, `INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition)
VALUES ('tenant-a', 'pa1', 'api-sdlc2', 'sdlc', 'api', '{}')`), "23505", "second API definition")
		// The same name in another project or tenant is unrelated.
		for _, sql := range []string{
			`INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-a', 'pa2', 'other-project', 'sdlc', 'api', '{}')`,
			`INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-b', 'pb1', 'other-tenant', 'sdlc', 'api', '{}')`,
		} {
			if err := tryExec(t, e, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		// The constraint also holds for the runtime role.
		pool := e.runtime(t, 1)
		requireCode(t, runtimeExec(t, pool, tenantA, `INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition)
VALUES ('tenant-a', 'pa1', 'late', 'sdlc', 'api', '{}')`), "23505", "runtime duplicate")
	})

	t.Run("duplicate_names_refuse_migration", func(t *testing.T) {
		_, pool, opts := priorSchema(t)
		ctx := testCtx(t)
		// A conflicting definition source: the API row and a Git row share a name,
		// and a second name is defined by two repositories.
		if _, err := pool.Exec(ctx, `
INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-a', 'pa1', 'api-sdlc', 'sdlc', 'api', '{"v":1}');
INSERT INTO pipelines (tenant_id, project_id, id, repo_id, name, source, source_commit, definition) VALUES
  ('tenant-a', 'pa1', 'git-sdlc', 'repo1', 'sdlc', 'git', '`+gitCommit+`', '{"v":2}'),
  ('tenant-a', 'pa1', 'git-ci1', 'repo1', 'ci', 'git', '`+gitCommit+`', '{}'),
  ('tenant-a', 'pa1', 'git-ci2', 'repo2', 'ci', 'git', '`+gitCommit+`', '{}');`); err != nil {
			t.Fatal(err)
		}
		before := count(t, pool, `SELECT count(*) FROM pipelines`)

		_, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		if err == nil {
			t.Fatal("migration succeeded over duplicate pipeline names")
		}
		requireCode(t, err, "23505", "duplicate pipeline names")
		if !strings.Contains(err.Error(), "2 project-scoped name(s)") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("error does not name the conflict: %v", err)
		}
		// Whole refusal: no ledger row, no new object, no data change, nothing chosen.
		if n := applied(t, pool); n != 3 {
			t.Errorf("ledger has %d rows, want 3", n)
		}
		if columnExists(t, pool, "tasks", "pipeline_id") || tableExists(t, pool, "signal_records") {
			t.Error("the expand migration left objects behind")
		}
		if got := count(t, pool, `SELECT count(*) FROM pipelines`); got != before {
			t.Errorf("pipelines = %d, want the original %d (no row merged or removed)", got, before)
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT definition::text FROM pipelines WHERE id = 'api-sdlc'`).Scan(&def); err != nil || !strings.Contains(def, `"v": 1`) {
			t.Errorf("definition changed: %s (%v)", def, err)
		}
		// Repeating the migration is refused again, the same way.
		_, err = storage.Migrate(ctx, pool, fullManifest(t), opts)
		requireCode(t, err, "23505", "repeat over duplicates")

		// The operator resolves the conflicts; only then does it apply.
		if _, err := pool.Exec(ctx, `DELETE FROM pipelines WHERE id IN ('git-sdlc', 'git-ci2')`); err != nil {
			t.Fatal(err)
		}
		res, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		if err != nil || len(res.Applied) != 4 || res.Applied[0] != expandMigration || res.Applied[1] != durableMigration || res.Applied[2] != waitMigration || res.Applied[3] != reasonMigration {
			t.Fatalf("migrate after resolution: %+v, %v", res, err)
		}
		if n := count(t, pool, `SELECT count(*) FROM pg_constraint WHERE conname = 'pipelines_name_unique'`); n != 1 {
			t.Errorf("pipelines_name_unique missing after migration")
		}
	})

	t.Run("ambiguous_mapping_refuses_migration", func(t *testing.T) {
		_, pool, opts := priorSchema(t)
		ctx := testCtx(t)
		if _, err := pool.Exec(ctx, `
INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES
  ('tenant-a', 'pa1', 'plA', 'alpha', 'api', '{}'), ('tenant-a', 'pa1', 'plB', 'beta', 'api', '{}'),
  ('tenant-a', 'pa2', 'plC', 'gamma', 'api', '{}');
INSERT INTO tasks (tenant_id, project_id, id, title, state) VALUES
  ('tenant-a', 'pa1', 'ambiguous', 'on two pipelines', 'open'),
  ('tenant-a', 'pa1', 'single', 'reruns of one', 'open'),
  ('tenant-a', 'pa1', 'unrun', 'never started', 'open'),
  ('tenant-a', 'pa2', 'other', 'another project', 'open');
INSERT INTO runs (tenant_id, project_id, id, pipeline_id, task_id, definition_snapshot) VALUES
  ('tenant-a', 'pa1', 'r1', 'plA', 'ambiguous', '{}'),
  ('tenant-a', 'pa1', 'r2', 'plB', 'ambiguous', '{}'),
  ('tenant-a', 'pa1', 'r3', 'plA', 'single', '{}'),
  ('tenant-a', 'pa1', 'r4', 'plA', 'single', '{}'),
  ('tenant-a', 'pa2', 'r5', 'plC', 'other', '{}'),
  ('tenant-a', 'pa1', 'r6', 'plB', NULL, '{}');`); err != nil {
			t.Fatal(err)
		}
		pipelines := count(t, pool, `SELECT count(*) FROM pipelines`)

		_, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		requireCode(t, err, "23514", "ambiguous task mapping")
		if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("error does not name the task: %v", err)
		}
		if n := applied(t, pool); n != 3 || columnExists(t, pool, "tasks", "pipeline_id") || tableExists(t, pool, "signal_records") {
			t.Errorf("refused migration left state: %d ledger rows", n)
		}

		// Explicit resolution: the operator removes the stray run's ambiguity.
		if _, err := pool.Exec(ctx, `UPDATE runs SET task_id = NULL WHERE id = 'r2'`); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, fullManifest(t), opts); err != nil {
			t.Fatalf("migrate after resolution: %v", err)
		}
		// Only what the task's own runs prove is mapped: a pipeline is never chosen
		// or created, a task without a run stays unmapped, no run is made current.
		want := map[string]string{"ambiguous": "plA", "single": "plA", "other": "plC", "unrun": ""}
		for task, pl := range want {
			var got *string
			if err := pool.QueryRow(testCtx(t), `SELECT pipeline_id FROM tasks WHERE id = $1`, task).Scan(&got); err != nil {
				t.Fatal(err)
			}
			switch {
			case pl == "" && got != nil:
				t.Errorf("task %s mapped to %s without evidence", task, *got)
			case pl != "" && (got == nil || *got != pl):
				t.Errorf("task %s pipeline = %v, want %s", task, got, pl)
			}
		}
		if got := count(t, pool, `SELECT count(*) FROM pipelines`); got != pipelines {
			t.Errorf("pipelines = %d, want %d: the migration must not create or remove one", got, pipelines)
		}
		if n := count(t, pool, `SELECT count(*) FROM tasks WHERE current_run_id IS NOT NULL OR status IS NOT NULL`); n != 0 {
			t.Errorf("%d tasks have a projection after the expand stage", n)
		}
		// The legacy columns survive with their data.
		if n := count(t, pool, `SELECT count(*) FROM tasks WHERE state = 'open'`); n != 4 {
			t.Errorf("legacy state readable for %d tasks, want 4", n)
		}
		if n := count(t, pool, `SELECT count(*) FROM projects WHERE task_workflow = '{}'`); n != 2 {
			t.Errorf("legacy task_workflow readable for %d projects, want 2", n)
		}
	})
	// The migration's validation and mapping must not race application writers.
	// The runner's advisory lock serializes runners only; the migration locks the
	// tables it reads and maps, so a concurrent admission either commits before the
	// validation (and is refused or mapped with it) or waits until the migration ends.
	t.Run("concurrent_admission_cannot_bypass_validation", func(t *testing.T) {
		seed := `
INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES
  ('tenant-a', 'pa1', 'plA', 'alpha', 'api', '{}'), ('tenant-a', 'pa1', 'plB', 'beta', 'api', '{}');
INSERT INTO tasks (tenant_id, project_id, id, title, state) VALUES ('tenant-a', 'pa1', 't1', 'one', 'open');
INSERT INTO runs (tenant_id, project_id, id, pipeline_id, task_id, definition_snapshot) VALUES
  ('tenant-a', 'pa1', 'r1', 'plA', 't1', '{}');`
		const secondRun = `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, task_id, definition_snapshot)
VALUES ('tenant-a', 'pa1', 'r2', 'plB', 't1', '{}')`

		// waiters counts backends waiting on a heavyweight lock in this database.
		waiters := func(ctx context.Context, pool *pgxpool.Pool) int {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		waitFor := func(ctx context.Context, pool *pgxpool.Pool, want int, what string) {
			t.Helper()
			deadline := time.Now().Add(20 * time.Second)
			for waiters(ctx, pool) < want {
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s", what)
				}
				time.Sleep(25 * time.Millisecond)
			}
		}

		t.Run("admission_in_flight_is_validated", func(t *testing.T) {
			db, pool, opts := priorSchema(t)
			ctx := testCtx(t)
			if _, err := pool.Exec(ctx, seed); err != nil {
				t.Fatal(err)
			}
			app := db.MigrationPool(ctx, 2) // an application connection, apart from the runner's
			tx, err := app.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			// The second run of the task is written but not committed: the checks of a
			// migration that did not wait for it would not see it.
			if _, err := tx.Exec(ctx, secondRun); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
				done <- err
			}()
			waitFor(ctx, app, 1, "the migration to wait for the in-flight admission")
			select {
			case err := <-done:
				t.Fatalf("the migration did not wait for the in-flight admission: %v", err)
			default:
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			requireCode(t, err, "23514", "task with an admission that committed during the migration")
			if n := applied(t, pool); n != 3 || columnExists(t, pool, "tasks", "pipeline_id") {
				t.Errorf("refused migration left state: %d ledger rows", n)
			}
			if got := count(t, pool, `SELECT count(*) FROM runs WHERE task_id = 't1'`); got != 2 {
				t.Errorf("runs of t1 = %d, want both", got)
			}
		})

		t.Run("writers_wait_for_the_whole_migration", func(t *testing.T) {
			db, pool, opts := priorSchema(t)
			ctx := testCtx(t)
			if _, err := pool.Exec(ctx, seed); err != nil {
				t.Fatal(err)
			}
			holder, writer := db.MigrationPool(ctx, 2), db.MigrationPool(ctx, 2)
			// A holds a row lock on tasks, so the migration acquires its locks on
			// pipelines and runs and then waits for tasks: the locks are held while it
			// waits, as they are through mapping.
			hold, err := holder.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(ctx) //nolint:errcheck
			if _, err := hold.Exec(ctx, `UPDATE tasks SET title = 'held' WHERE id = 't1'`); err != nil {
				t.Fatal(err)
			}
			migrated := make(chan error, 1)
			go func() {
				_, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
				migrated <- err
			}()
			waitFor(ctx, holder, 1, "the migration to wait for tasks")
			// A concurrent admission of a run on another pipeline must wait for it.
			admitted := make(chan error, 1)
			go func() { _, err := writer.Exec(ctx, secondRun); admitted <- err }()
			waitFor(ctx, holder, 2, "the admission to wait for the migration")
			select {
			case err := <-admitted:
				t.Fatalf("an admission committed while the migration held its locks: %v", err)
			default:
			}
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-migrated; err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if err := <-admitted; err != nil {
				t.Fatalf("the waiting admission failed after the migration: %v", err)
			}
			// The mapping saw exactly what was validated: one run, one pipeline.
			var pl *string
			if err := pool.QueryRow(ctx, `SELECT pipeline_id FROM tasks WHERE id = 't1'`).Scan(&pl); err != nil || pl == nil || *pl != "plA" {
				t.Fatalf("task t1 pipeline = %v (%v), want plA from the validated run", pl, err)
			}
		})
	})
}
