package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func fullManifest(t *testing.T) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(storage.BaseContribution(), execpg.RunResourceMigrations())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func constraint(t *testing.T, pool *pgxpool.Pool, table, name string) (deferrable, deferred, validated bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT c.condeferrable, c.condeferred, c.convalidated FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = $1 AND c.conname = $2`, table, name).Scan(&deferrable, &deferred, &validated)
	if err != nil {
		t.Fatalf("constraint %s.%s: %v", table, name, err)
	}
	return
}

// Test_execution_migrations: the baseline applies and reapplies, the execution
// constraints it creates are validated with the intended deferral, immutable
// checksums are enforced and the runtime grants are column scoped.
func Test_execution_migrations(t *testing.T) {
	ctx := context.Background()
	opts := func(db *pgtest.Database) storage.MigrateOptions {
		return storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	}

	t.Run("fresh application, reapply and dependent constraints", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		m := fullManifest(t)
		first, err := storage.Migrate(ctx, pool, m, opts(db))
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Applied) != 3 {
			t.Fatalf("the manifest is the two-file baseline plus run_shared, applied %v", first.Applied)
		}
		second, err := storage.Migrate(ctx, pool, m, opts(db))
		if err != nil || len(second.Applied) != 0 || second.Skipped != len(m.Migrations()) {
			t.Fatalf("reapply: %+v %v", second, err)
		}
		// Every dependent constraint exists and is validated; the writer and
		// publication links are deferred to commit.
		for _, c := range []struct {
			table, name string
			deferred    bool
		}{
			{"workspaces", "workspaces_work_item_fk", false}, {"workspaces", "workspaces_writer_attempt_fk", true},
			{"workspaces", "workspaces_publish_attempt_fk", true}, {"workspaces", "workspaces_publish_artifact_fk", false},
			{"workspace_publications", "workspace_publications_attempt_fk", false}, {"workspace_publications", "workspace_publications_artifact_fk", false},
			{"workspace_retained_refs", "workspace_retained_refs_run_fk", false}, {"workspace_retained_refs", "workspace_retained_refs_attempt_fk", false},
			{"artifacts", "artifacts_producer_attempt_fk", false}, {"secret_pins", "secret_pins_run_fk", false},
		} {
			deferrable, deferred, validated := constraint(t, pool, c.table, c.name)
			if !validated || deferrable != c.deferred || deferred != c.deferred {
				t.Errorf("%s: deferrable=%v deferred=%v validated=%v", c.name, deferrable, deferred, validated)
			}
		}
		// The deferred writer constraint is enforced at commit.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ('t020_mig', 't020_mig')`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t020_mig', 'p020_mig', 'p020_mig', 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_items (tenant_id, project_id, id, kind, title, state, created_by_type, created_by_id, created_at, updated_at)
			VALUES ('t020_mig', 'p020_mig', 'wi_mig', 'story', 't', 'ready', 'user', 'u', now(), now())`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch,
			initial_commit, accepted_commit, state, writer_attempt_id, writer_input_commit, writer_expected_tip, writer_reserved_at)
			VALUES ('t020_mig', 'p020_mig', 'ws_mig', 'wi_mig', 'r', 'https://x.invalid/r.git', 'secret://a/b', 'secret://a/c', 'refs/heads/w',
			$1, $1, 'available', 'att_ghost', $1, $1, now())`, strings.Repeat("a", 40)); err != nil {
			t.Fatalf("an immediate check fired before commit: %v", err)
		}
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("a writer reservation naming no attempt committed")
		}
	})

	t.Run("checksums are immutable and grants need the runtime role", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		if _, err := storage.Migrate(ctx, pool, fullManifest(t), opts(db)); err != nil {
			t.Fatal(err)
		}
		base := storage.BaseContribution()
		for i := range base.Migrations {
			e := storage.BaseContribution()
			e.Migrations[i].SQL += "\n-- edited after merge\n"
			m, err := storage.NewManifest(append([]storage.Contribution{e}, execpg.RunResourceMigrations())...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.Migrate(ctx, pool, m, opts(db)); !errors.Is(err, storage.ErrChecksumMismatch) {
				t.Fatalf("migration %s: %v", base.Migrations[i].ID, err)
			}
		}
		fresh := pgtest.New(t)
		if _, err := storage.Migrate(ctx, fresh.MigrationPool(ctx, 2), fullManifest(t), storage.MigrateOptions{}); !errors.Is(err, storage.ErrRuntimeRoleRequired) {
			t.Fatalf("grants need the runtime role placeholder: %v", err)
		}
	})

	t.Run("runtime grants are column scoped", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		if _, err := storage.Migrate(ctx, pool, fullManifest(t), opts(db)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			table, col string
			want       bool
		}{{"runs", "status", true}, {"runs", "inputs", false}, {"runs", "definition_id", false}, {"runs", "specification", false},
			{"step_attempts", "status", true}, {"step_attempts", "context", false}, {"step_attempts", "job_name", false},
			{"step_runs", "spec", false}, {"execution_reservations", "released_at", true}, {"execution_reservations", "held_at", false},
			{"attempt_controls", "workload_removed_at", true},
			// run_shared (003_run_resources.sql): only the mutable evidence columns are updatable.
			{"runs", "deadline_at", true}, {"runs", "run_shared", false}, {"run_resources", "uid", true}, {"run_resources", "spec", false},
			{"run_resources", "fingerprint", false}, {"run_resources", "name", false}, {"run_holders", "released_at", true},
			{"run_holders", "held_at", false}, {"attempt_begins", "body_digest", false}, {"execution_run_reservations", "released_at", true},
			{"execution_run_reservations", "cpu_millis", false}} {
			var got bool
			if err := pool.QueryRow(ctx, `SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, c.table, c.col).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("UPDATE(%s.%s) = %v, want %v", c.table, c.col, got, c.want)
			}
		}
		for _, table := range []string{"runs", "step_runs", "step_attempts", "attempt_controls", "attempt_evidence", "run_history", "execution_reservations",
			"run_resources", "run_holders", "attempt_begins", "execution_run_reservations"} {
			var del bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'DELETE')`, db.RuntimeRole, table).Scan(&del); err != nil || del {
				t.Errorf("the runtime role can DELETE from %s", table)
			}
		}
	})
}
