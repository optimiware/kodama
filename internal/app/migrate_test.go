package app

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/test/support/postgres"
)

func TestMigrationBaselineIsTwoFiles(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	cs, ms := Contributions(), m.Migrations()
	if len(cs) != 1 || cs[0].Module != "storage" || len(ms) != 2 || ms[0].ID != "001_schema.sql" || ms[1].ID != "002_grants.sql" {
		t.Fatalf("unexpected baseline: %+v", cs)
	}
}

func TestSimplifiedManifestMigration(t *testing.T) {
	db := postgres.New(t)
	ctx := t.Context()
	result, err := Migrate(ctx, MigrateConfig{MigrationURL: db.MigrationURL, RuntimeRole: db.RuntimeRole})
	if err != nil || len(result.Applied) != 2 {
		t.Fatalf("migrate: %+v, %v", result, err)
	}
	pool := db.MigrationPool(ctx, 1)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations'`).Scan(&count); err != nil || count != 15 {
		t.Fatalf("tables=%d, err=%v; want 15", count, err)
	}
	result, err = Migrate(ctx, MigrateConfig{MigrationURL: db.MigrationURL, RuntimeRole: db.RuntimeRole})
	if err != nil || result.Skipped != 2 || len(result.Applied) != 0 {
		t.Fatalf("reapply: %+v, %v", result, err)
	}
	if err := requireLegacySchema(ctx, db.RuntimePool(ctx, 1)); err == nil {
		t.Fatal("old server adapters accepted the Phase 2 schema")
	}
}

func TestMigrateRefusesUnsafeIdentities(t *testing.T) {
	for name, tc := range map[string]MigrateConfig{
		"no owner":           {RuntimeURL: "postgres://rt:p@h/db"},
		"no runtime":         {MigrationURL: "postgres://own:p@h/db"},
		"same role":          {MigrationURL: "postgres://same:p@h/db", RuntimeURL: "postgres://same:q@h/db"},
		"anonymous owner":    {MigrationURL: "postgres://h/db", RuntimeURL: "postgres://rt:p@h/db"},
		"same role by name":  {MigrationURL: "postgres://own:p@h/db", RuntimeRole: "own"},
		"both runtime forms": {MigrationURL: "postgres://own:p@h/db", RuntimeURL: "postgres://rt:p@h/db", RuntimeRole: "rt"},
	} {
		if _, err := Migrate(context.Background(), tc); err == nil {
			t.Errorf("%s: migration attempted", name)
		}
	}
}
