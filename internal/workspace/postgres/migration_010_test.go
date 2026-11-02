package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Test_migration_workspace_repositories: the 010 contribution applies after the
// baseline, repeats as a checksum-verified no-op, refuses an edit, keeps every
// existing workspace external, and gives the new tables forced row level
// security, the constraints the module relies on and least-privilege grants.
func Test_migration_workspace_repositories(t *testing.T) {
	ctx := context.Background()
	if got := postgres.Migrations().Migrations; len(got) != 1 || got[0].ID != postgres.MigrationID || postgres.MigrationID != "010_workspace_repositories.sql" {
		t.Fatalf("the contribution is one migration, 010_workspace_repositories.sql: %+v", got)
	}
	baseline, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	full, err := storage.NewManifest(storage.BaseContribution(), postgres.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}

	// A workspace that exists before 010 stays external.
	if _, err := storage.Migrate(ctx, owner, baseline, opts); err != nil {
		t.Fatal(err)
	}
	pgtest.DropExecutionLinks(t, owner, "workspaces", "workspaces_work_item_fk", "workspaces_writer_attempt_fk", "workspaces_publish_attempt_fk", "workspaces_publish_artifact_fk")
	for _, q := range []string{
		`INSERT INTO tenants (id, key) VALUES ('t147_a', 't147_a')`,
		`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t147_a', 'p147_a', 'p147_a', 5)`,
		`INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch, state, version, fence_epoch, publication_fenced, created_at, updated_at)
		 VALUES ('t147_a', 'p147_a', 'ws_old', 'wi_old', 'repo_old', 'https://git.example.test/o.git', 'secret://c', 'secret://p', 'refs/heads/kodama/work/wi_old', 'provisioning', 1, 1, false, now(), now())`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	first, err := storage.Migrate(ctx, owner, full, opts)
	if err != nil || len(first.Applied) != 1 || first.Applied[0] != postgres.MigrationID {
		t.Fatalf("apply 010 over the baseline: %+v %v", first, err)
	}
	second, err := storage.Migrate(ctx, owner, full, opts)
	if err != nil || len(second.Applied) != 0 {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := postgres.Migrations()
	changed.Migrations[0].SQL += "\n-- edited after merge"
	edited, err := storage.NewManifest(storage.BaseContribution(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, edited, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}

	var hosting string
	if err := owner.QueryRow(ctx, `SELECT hosting FROM workspaces WHERE id = 'ws_old'`).Scan(&hosting); err != nil || hosting != "external" {
		t.Fatalf("an existing workspace is %q (%v), want external", hosting, err)
	}

	for table, names := range map[string][]string{
		"workspaces": {"workspaces_hosting_known"},
		"workspace_repositories": {"workspace_repositories_pkey", "workspace_repositories_one_branch", "workspace_repositories_accepted_when_created",
			"workspace_repositories_branch_syntax", "workspace_repositories_state_known", "workspace_repositories_tenant_id_project_id_workspace_id_fkey"},
		"workspace_checkpoints": {"workspace_checkpoints_pkey", "workspace_checkpoints_pin_unique", "workspace_checkpoints_pinned_consistent",
			"workspace_checkpoints_pin_syntax", "workspace_checkpoints_pin_status_known", "workspace_checkpoints_tenant_id_project_id_workspace_id_fkey"},
	} {
		for _, name := range names {
			var n int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`, name, table).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: constraint %s missing (%d, %v)", table, name, n, err)
			}
		}
	}
	for _, table := range []string{"workspace_repositories", "workspace_checkpoints"} {
		var rls, force bool
		var policies int
		if err := owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity,
			(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid AND pg_get_expr(p.polqual, p.polrelid) LIKE '%kodama_tenant_id()%')
			FROM pg_class c WHERE c.relname = $1`, table).Scan(&rls, &force, &policies); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || policies < 1 {
			t.Errorf("%s: rls=%v force=%v tenant policies=%d", table, rls, force, policies)
		}
	}

	t.Run("least privilege", func(t *testing.T) {
		can := func(query string, args ...any) bool {
			var ok bool
			if err := owner.QueryRow(ctx, query, args...).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			return ok
		}
		role := db.RuntimeRole
		if can(`SELECT has_column_privilege($1, 'workspaces', 'hosting', 'UPDATE')`, role) {
			t.Error("the runtime role can change a workspace's hosting")
		}
		if !can(`SELECT has_column_privilege($1, 'workspaces', 'hosting', 'SELECT')`, role) {
			t.Error("the runtime role cannot read a workspace's hosting")
		}
		for _, table := range []string{"workspace_repositories", "workspace_checkpoints"} {
			if can(`SELECT has_table_privilege($1, $2, 'DELETE')`, role, table) {
				t.Errorf("the runtime role can delete from %s", table)
			}
			if !can(`SELECT has_table_privilege($1, $2, 'INSERT')`, role, table) {
				t.Errorf("the runtime role cannot insert into %s", table)
			}
		}
		for table, immutable := range map[string][]string{
			"workspace_repositories": {"task_branch", "base_commit", "repository_id", "created_at"},
			"workspace_checkpoints":  {"commit_oid", "previous_commit", "attempt_id", "run_id", "push_id", "ref"},
		} {
			for _, col := range immutable {
				if can(`SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, role, table, col) {
					t.Errorf("the runtime role can update %s.%s", table, col)
				}
			}
		}
	})

	t.Run("the database refuses an inconsistent record", func(t *testing.T) {
		const (
			a40 = "'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'"
			b40 = "'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'"
		)
		cols := `(tenant_id, project_id, workspace_id, repository_id, task_branch, base_commit, accepted_commit, state, created_at, updated_at)`
		for name, values := range map[string]string{
			"a created branch without an accepted commit": `('t147_a','p147_a','ws_old','r1','refs/heads/kodama/work/wi_old',` + a40 + `,NULL,'created',now(),now())`,
			"an intent with an accepted commit":           `('t147_a','p147_a','ws_old','r1','refs/heads/kodama/work/wi_old',` + a40 + `,` + b40 + `,'intent',now(),now())`,
			"a branch outside the task namespace":         `('t147_a','p147_a','ws_old','r1','refs/heads/main',` + a40 + `,NULL,'intent',now(),now())`,
			"a workspace of another project":              `('t147_a','p147_other','ws_old','r1','refs/heads/kodama/work/wi_old',` + a40 + `,NULL,'intent',now(),now())`,
		} {
			if _, err := owner.Exec(ctx, `INSERT INTO workspace_repositories `+cols+` VALUES `+values); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		if _, err := owner.Exec(ctx, `INSERT INTO workspace_repositories `+cols+` VALUES ('t147_a','p147_a','ws_old','r1','refs/heads/kodama/work/wi_old',`+a40+`,NULL,'intent',now(),now())`); err != nil {
			t.Fatalf("a valid association: %v", err)
		}
		if _, err := owner.Exec(ctx, `INSERT INTO workspace_repositories `+cols+` VALUES ('t147_a','p147_a','ws_old','r2','refs/heads/kodama/work/wi_old',`+a40+`,NULL,'intent',now(),now())`); err != nil {
			t.Fatalf("the same branch name in another repository: %v", err)
		}
		if _, err := owner.Exec(ctx, `INSERT INTO workspace_repositories `+cols+` VALUES ('t147_a','p147_a','ws_old','r1','refs/heads/kodama/work/wi_old',`+a40+`,NULL,'intent',now(),now())`); err == nil {
			t.Error("a second association of one repository was accepted")
		}
		cpCols := `(tenant_id, project_id, workspace_id, repository_id, push_id, ref, commit_oid, previous_commit, advanced, attempt_id, run_id, pin_ref, pin_status, created_at, pinned_at)`
		for name, values := range map[string]string{
			"a pin outside the retained namespace": `('t147_a','p147_a','ws_old','r1','push_1','refs/heads/kodama/work/wi_old',` + b40 + `,` + a40 + `,true,'att_1','run_1','refs/heads/x/1','intent',now(),NULL)`,
			"a confirmed pin without its time":     `('t147_a','p147_a','ws_old','r1','push_1','refs/heads/kodama/work/wi_old',` + b40 + `,` + a40 + `,true,'att_1','run_1','refs/kodama/runs/run_1/1','confirmed',now(),NULL)`,
			"an advance that does not move":        `('t147_a','p147_a','ws_old','r1','push_1','refs/heads/kodama/work/wi_old',` + a40 + `,` + a40 + `,true,'att_1','run_1','refs/kodama/runs/run_1/1','intent',now(),NULL)`,
		} {
			if _, err := owner.Exec(ctx, `INSERT INTO workspace_checkpoints `+cpCols+` VALUES `+values); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		ok := `('t147_a','p147_a','ws_old','r1','push_1','refs/heads/kodama/work/wi_old',` + b40 + `,` + a40 + `,true,'att_1','run_1','refs/kodama/runs/run_1/1','intent',now(),NULL)`
		if _, err := owner.Exec(ctx, `INSERT INTO workspace_checkpoints `+cpCols+` VALUES `+ok); err != nil {
			t.Fatalf("a valid checkpoint: %v", err)
		}
		dup := `('t147_a','p147_a','ws_old','r1','push_2','refs/heads/kodama/work/wi_old',` + b40 + `,` + a40 + `,true,'att_1','run_1','refs/kodama/runs/run_1/1','intent',now(),NULL)`
		if _, err := owner.Exec(ctx, `INSERT INTO workspace_checkpoints `+cpCols+` VALUES `+dup); err == nil {
			t.Error("two checkpoints took one retained ref")
		}
	})
}
