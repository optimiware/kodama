package app

import (
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodama/test/support/postgres"
)

// TestMigrationBaselineIsThreeFiles: the application manifest is the ADR 0013
// baseline (schema and grants) plus the appended Git hosting contribution (O-36),
// the pipeline-runtime expand contribution (T-179) and the durable run-state
// contribution (T-185), in that order and nothing else: five files today. The
// name is kept because the Phase 2 handoff selects the test by it.
func TestMigrationBaselineIsThreeFiles(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	cs := Contributions()
	if len(cs) != 1 || cs[0].Module != "storage" {
		t.Fatalf("unexpected contributions: %+v", cs)
	}
	var ids []string
	for _, mig := range m.Migrations() {
		ids = append(ids, mig.ID)
	}
	if !slices.Equal(ids, []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", "004_pipeline_runtime_expand.sql", "005_durable_run_state.sql", "006_wait_decision_and_materializations.sql", "007_signal_decision_reason.sql"}) {
		t.Fatalf("unexpected baseline: %v", ids)
	}
}

func TestSimplifiedManifestMigration(t *testing.T) {
	db := postgres.New(t)
	ctx := t.Context()
	result, err := Migrate(ctx, MigrateConfig{MigrationURL: db.MigrationURL, RuntimeRole: db.RuntimeRole})
	if err != nil || len(result.Applied) != 7 {
		t.Fatalf("migrate: %+v, %v", result, err)
	}
	pool := db.MigrationPool(ctx, 1)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations'`).Scan(&count); err != nil || count != 21 {
		t.Fatalf("tables=%d, err=%v; want 21 (15 baseline tables plus git_credentials, signal_records, run_commands, run_workload_observations, run_resource_observations and run_materializations)", count, err)
	}
	result, err = Migrate(ctx, MigrateConfig{MigrationURL: db.MigrationURL, RuntimeRole: db.RuntimeRole})
	if err != nil || result.Skipped != 7 || len(result.Applied) != 0 {
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
