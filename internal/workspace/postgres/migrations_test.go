package postgres_test

import (
	"context"
	"errors"
	"testing"

	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Test_migration_reapply: fresh application, repeated application (a no-op
// verified by checksum), schema constraints and manifest contribution rules.
func Test_migration_reapply(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	migration := db.MigrationPool(ctx, 2)
	manifest, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workspacepg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.Migrate(ctx, migration, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole})
	if err != nil {
		t.Fatal(err)
	}
	var applied bool
	for _, id := range first.Applied {
		if id == "T013_001_workspaces.sql" {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("T013_001 not applied: %v", first.Applied)
	}
	second, err := storage.Migrate(ctx, migration, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole})
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(manifest.Migrations()) {
		t.Fatalf("reapply: %+v %v", second, err)
	}

	t.Run("every table has forced row level security and an explicit tenant policy", func(t *testing.T) {
		rows, err := migration.Query(ctx, `SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
			(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid AND pg_get_expr(p.polqual, p.polrelid) LIKE '%kodama_tenant_id()%')
			FROM pg_class c WHERE c.relname IN ('workspaces', 'workspace_publications', 'workspace_retained_refs') ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var name string
			var rls, force bool
			var policies int
			if err := rows.Scan(&name, &rls, &force, &policies); err != nil {
				t.Fatal(err)
			}
			if !rls || !force || policies < 1 {
				t.Errorf("%s: rls=%v force=%v tenant policies=%d", name, rls, force, policies)
			}
			n++
		}
		if n != 3 {
			t.Fatalf("%d tables", n)
		}
	})

	t.Run("a checksum change is refused", func(t *testing.T) {
		altered := workspacepg.Migrations()
		altered.Migrations[0].SQL += "\n-- edited after merge\n"
		m2, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), altered)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, migration, m2, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the contribution is a valid, ordered, non-conflicting manifest part", func(t *testing.T) {
		c := workspacepg.Migrations()
		if c.Module != "workspace" || len(c.Migrations) == 0 {
			t.Fatalf("%+v", c)
		}
		if _, err := storage.NewManifest(storage.BaseContribution(), c, c); !errors.Is(err, storage.ErrDuplicateMigration) {
			t.Fatalf("a duplicate contribution must be rejected: %v", err)
		}
	})

	t.Run("the grants need the runtime role placeholder", func(t *testing.T) {
		fresh := pgtest.New(t)
		pool := fresh.MigrationPool(ctx, 2)
		if _, err := storage.Migrate(ctx, pool, manifest, storage.MigrateOptions{}); !errors.Is(err, storage.ErrRuntimeRoleRequired) {
			t.Fatalf("got %v", err)
		}
	})
}
