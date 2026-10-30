package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Test_migration_reapply: fresh application, repeated application (a no-op
// verified by checksum), a refused edit, the runtime-role placeholder and the
// constraints the module relies on.
func Test_migration_reapply(t *testing.T) {
	ctx := context.Background()
	manifest, err := storage.NewManifest(storage.BaseContribution(), postgres.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if got := postgres.Migrations().Migrations; len(got) != 1 || got[0].ID != postgres.MigrationID {
		t.Fatalf("the contribution is one migration, 007_git_repositories.sql: %+v", got)
	}

	if postgres.MigrationID != "007_git_repositories.sql" {
		t.Fatalf("the reserved number is 007: %s", postgres.MigrationID)
	}

	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(ctx, owner, manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Applied) != 3 {
		t.Fatalf("the baseline is two migrations and the contribution one, applied %v", first.Applied)
	}
	second, err := storage.Migrate(ctx, owner, manifest, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}

	changed := postgres.Migrations()
	changed.Migrations[0].SQL += "\n-- edited after merge"
	bad, err := storage.NewManifest(storage.BaseContribution(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, bad, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}

	t.Run("the schema carries the invariants", func(t *testing.T) {
		for _, name := range []string{
			"repositories_pkey", "repositories_project_id_unique", "repositories_name_unique", "repositories_tenant_id_project_id_fkey",
			"repositories_name_syntax", "repositories_branch_syntax", "repositories_branch_not_reserved", "repositories_status_known",
			"repositories_hosting_known", "repositories_reason_matches_status", "repositories_hosting_url", "repositories_external_shape",
			"repositories_provisioning_not_empty", "repositories_version_range", "repositories_external_url_syntax",
		} {
			var n int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'repositories'::regclass`, name).Scan(&n); err != nil || n != 1 {
				t.Errorf("constraint %s missing (%d, %v)", name, n, err)
			}
		}
		var fk string
		if err := owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'repositories_tenant_id_project_id_fkey'`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != "FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id)" {
			t.Errorf("foreign key = %s", fk)
		}
		var rls, force bool
		var policies, triggers int
		if err := owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity,
			(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid AND pg_get_expr(p.polqual, p.polrelid) LIKE '%kodama_tenant_id()%'),
			(SELECT count(*) FROM pg_trigger WHERE tgrelid = c.oid AND tgname = 'repositories_guard' AND NOT tgisinternal)
			FROM pg_class c WHERE c.relname = 'repositories'`).Scan(&rls, &force, &policies, &triggers); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || policies < 1 || triggers != 1 {
			t.Fatalf("rls=%v force=%v tenant policies=%d guard triggers=%d", rls, force, policies, triggers)
		}
	})
}

// Test_unit_migration_least_privilege: every grant names the runtime role through
// the placeholder, none deletes or grants everything, and the migration never
// touches the baseline.
func Test_unit_migration_least_privilege(t *testing.T) {
	ms := postgres.Migrations().Migrations
	if len(ms) != 1 {
		t.Fatalf("%d migrations", len(ms))
	}
	sql := ms[0].SQL
	if !strings.Contains(sql, storage.RuntimeRolePlaceholder) {
		t.Fatal("the migration grants nothing to the runtime role")
	}
	for _, m := range regexp.MustCompile(`(?is)\bGRANT\b[^;]*;`).FindAllString(sql, -1) {
		if !strings.Contains(m, storage.RuntimeRolePlaceholder) {
			t.Errorf("a grant that does not name the runtime role: %s", m)
		}
		for _, bad := range []string{"DELETE", "ALL", "TRUNCATE", "PUBLIC", "WITH GRANT OPTION"} {
			if regexp.MustCompile(`(?i)\b` + bad + `\b`).MatchString(m) {
				t.Errorf("grant mentions %s: %s", bad, m)
			}
		}
	}
	for _, forbidden := range []string{"ALTER TABLE projects", "DROP ", "INSERT INTO projects", "001_schema"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("the migration touches the baseline: %s", forbidden)
		}
	}
}
