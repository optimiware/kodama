package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func peerContributions() []storage.Contribution {
	return []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
}

func fullManifest(t *testing.T) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(append(peerContributions(), execpg.Migrations()...)...)
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

// Test_execution_migrations: ordered upgrade/reapply, FK validation, immutable
// migration checksums and workspace SQL/generated-copy equality.
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
		pos := map[string]int{}
		for i, id := range first.Applied {
			pos[id] = i
		}
		for _, id := range []string{"T020_002_runs.sql", "T020_001_attempt_fk.sql", "T020_003_artifact_fk.sql", "T020_004_secret_pin_fk.sql", execpg.CleanupMigrationID} {
			if _, ok := pos[id]; !ok {
				t.Fatalf("%s was not applied: %v", id, first.Applied)
			}
		}
		// Execution tables first, then the dependent constraints in listed
		// order, then cleanup bookkeeping.
		if !(pos["T020_002_runs.sql"] < pos["T020_001_attempt_fk.sql"] && pos["T020_001_attempt_fk.sql"] < pos["T020_003_artifact_fk.sql"] &&
			pos["T020_003_artifact_fk.sql"] < pos["T020_004_secret_pin_fk.sql"] && pos["T020_004_secret_pin_fk.sql"] < pos[execpg.CleanupMigrationID]) {
			t.Fatalf("application order: %v", first.Applied)
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

	t.Run("upgrade of a database that already holds work, workspaces and secrets", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		before, err := storage.NewManifest(peerContributions()...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, before, opts(db)); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`INSERT INTO tenants (id, key) VALUES ('t020_up', 't020_up')`,
			`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t020_up', 'p020_up', 'p020_up', 1)`,
			`INSERT INTO work_items (tenant_id, project_id, id, kind, title, state, created_by_type, created_by_id, created_at, updated_at)
				VALUES ('t020_up', 'p020_up', 'wi_up', 'story', 't', 'ready', 'user', 'u', now(), now())`,
			`INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch)
				VALUES ('t020_up', 'p020_up', 'ws_up', 'wi_up', 'r', 'https://x.invalid/r.git', 'secret://a/b', 'secret://a/c', 'refs/heads/w')`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("seed: %v\n%s", err, q)
			}
		}
		res, err := storage.Migrate(ctx, pool, fullManifest(t), opts(db))
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if len(res.Applied) != 5 {
			t.Fatalf("applied %v, want only the five execution migrations", res.Applied)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("workspace row lost: %d %v", n, err)
		}
	})

	t.Run("a migration out of order is refused", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		c := execpg.Migrations()
		wrong, err := storage.NewManifest(append(peerContributions(), c[1], c[0], c[2], c[3])...) // links before the tables
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, wrong, opts(db)); err == nil {
			t.Fatal("the workspace links applied before the tables they reference")
		}
	})

	t.Run("cleanup bookkeeping applies last onto a deployed execution schema", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		c := execpg.Migrations()
		deployed, err := storage.NewManifest(append(peerContributions(), c[:3]...)...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, deployed, opts(db)); err != nil {
			t.Fatal(err)
		}
		res, err := storage.Migrate(ctx, pool, fullManifest(t), opts(db))
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != execpg.CleanupMigrationID {
			t.Fatalf("upgrade: %+v %v", res, err)
		}
	})

	t.Run("cleanup bookkeeping applies after the runtime authority schema of a deployed composition", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		deployed, err := storage.NewManifest(append(append(peerContributions(), execpg.Schema()...), rtpg.Migrations())...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, deployed, opts(db)); err != nil {
			t.Fatal(err)
		}
		upgraded, err := storage.NewManifest(append(append(peerContributions(), execpg.Schema()...), rtpg.Migrations(), execpg.CleanupMigrations())...)
		if err != nil {
			t.Fatal(err)
		}
		res, err := storage.Migrate(ctx, pool, upgraded, opts(db))
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != execpg.CleanupMigrationID {
			t.Fatalf("upgrade: %+v %v", res, err)
		}
	})

	t.Run("checksums are immutable", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		if _, err := storage.Migrate(ctx, pool, fullManifest(t), opts(db)); err != nil {
			t.Fatal(err)
		}
		for i := range execpg.Migrations() {
			c := execpg.Migrations()
			c[i].Migrations[0].SQL += "\n-- edited after merge\n"
			m, err := storage.NewManifest(append(peerContributions(), c...)...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.Migrate(ctx, pool, m, opts(db)); !errors.Is(err, storage.ErrChecksumMismatch) {
				t.Fatalf("contribution %s: %v", c[i].Module, err)
			}
		}
	})

	t.Run("the contributions are valid, non-conflicting manifest parts", func(t *testing.T) {
		c := execpg.Migrations()
		if len(c) != 4 || c[0].Module != "execution" || c[1].Module != "execution-workspace-links" || c[2].Module != "execution-links" ||
			c[3].Module != "execution-cleanup" || len(c[3].Migrations) != 1 || c[3].Migrations[0].ID != execpg.CleanupMigrationID {
			t.Fatalf("contributions: %+v", c)
		}
		if _, err := storage.NewManifest(append(c, c...)...); !errors.Is(err, storage.ErrDuplicateMigration) {
			t.Fatalf("a duplicate contribution must be rejected: %v", err)
		}
		fresh := pgtest.New(t)
		if _, err := storage.Migrate(ctx, fresh.MigrationPool(ctx, 2), fullManifest(t), storage.MigrateOptions{}); !errors.Is(err, storage.ErrRuntimeRoleRequired) {
			t.Fatalf("grants need the runtime role placeholder: %v", err)
		}
	})

	t.Run("the generated workspace SQL equals its authoritative file", func(t *testing.T) {
		src, err := os.ReadFile("../../workspace/migrations/T020_001_attempt_fk.sql")
		if err != nil {
			t.Fatal(err)
		}
		if string(src) != execpg.WorkspaceFKSQL() {
			t.Fatal("internal/workspace/migrations/T020_001_attempt_fk.sql and the generated copy differ; run go generate ./internal/execution/...")
		}
		c := execpg.Migrations()
		if c[1].Migrations[0].ID != execpg.WorkspaceFKMigrationID || c[1].Migrations[0].SQL != string(src) {
			t.Fatal("the registered contribution is not the authoritative file")
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
			{"attempt_controls", "workload_removed_at", true}} {
			var got bool
			if err := pool.QueryRow(ctx, `SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, c.table, c.col).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("UPDATE(%s.%s) = %v, want %v", c.table, c.col, got, c.want)
			}
		}
		for _, table := range []string{"runs", "step_runs", "step_attempts", "attempt_controls", "attempt_evidence", "run_history", "execution_reservations"} {
			var del bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'DELETE')`, db.RuntimeRole, table).Scan(&del); err != nil || del {
				t.Errorf("the runtime role can DELETE from %s", table)
			}
		}
	})
}
