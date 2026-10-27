package postgres_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Test_migration_reapply: the baseline applies, reapplies as a no-op, rejects a
// modified applied migration, and creates the constraints the module relies on.
func Test_migration_reapply(t *testing.T) {
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}

	db := pgtest.New(t)
	owner := db.MigrationPool(bg, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Applied) != 2 {
		t.Fatalf("the baseline is two migrations, applied %v", first.Applied)
	}
	second, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}

	changed := storage.BaseContribution()
	changed.Migrations[0].SQL += "\n-- edited"
	bad, _ := storage.NewManifest(changed)
	if _, err := storage.Migrate(bg, owner, bad, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}
	if _, err := storage.Migrate(bg, owner, manifest, storage.MigrateOptions{}); err == nil {
		t.Fatal("grants without a runtime role must be refused")
	}

	for _, name := range []string{
		"runtime_grants_credential_unique", "runtime_grants_session_unique", "runtime_grants_consumption_complete",
		"runtime_grants_actions_known", "runtime_grants_actions_bounded", "runtime_grants_expiry_order",
		"runtime_grants_lifetime_order", "runtime_grants_credential_digest_length", "runtime_grants_generation_range",
		"runtime_grants_pkey", "runtime_grants_tenant_id_project_id_fkey",
	} {
		var n int
		if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'runtime_grants'::regclass`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("constraint %s missing (%d, %v)", name, n, err)
		}
	}
	var fk string
	if err := owner.QueryRow(bg, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'runtime_grants_tenant_id_project_id_fkey'`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != "FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id)" {
		t.Errorf("foreign key = %s", fk)
	}
	var triggers int
	if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'runtime_grants'::regclass AND tgname = 'runtime_grants_guard' AND NOT tgisinternal`).Scan(&triggers); err != nil || triggers != 1 {
		t.Fatalf("guard trigger: %d %v", triggers, err)
	}
}
