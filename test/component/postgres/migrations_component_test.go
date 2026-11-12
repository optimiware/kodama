//go:build component

package postgrescomponent

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestMigrationsApplyOnEmptyDatabase applies the migration set to an empty
// database and proves the order, the ledger and the core schema facts.
func TestMigrationsApplyOnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)

	applied, err := runner.Apply(ctx, 0)
	if err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	frozen := storepg.Migrations()
	if len(applied) != len(frozen) {
		t.Fatalf("applied %d migrations; want %d", len(applied), len(frozen))
	}
	for index, migration := range applied {
		if migration.Name != frozen[index].Name {
			t.Fatalf("applied[%d] = %q; want %q", index, migration.Name, frozen[index].Name)
		}
	}
	ledger, err := runner.Applied(ctx)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(ledger) != len(frozen) {
		t.Fatalf("ledger has %d entries; want %d", len(ledger), len(frozen))
	}
	for _, table := range store.SchemaDesign().Tables {
		if !tableExists(ctx, t, shard, table.Name) {
			t.Fatalf("table %q was not created", table.Name)
		}
		enabled, forced := rlsFacts(ctx, t, shard, table.Name)
		if table.RowLevelSecurity && (!enabled || !forced) {
			t.Fatalf("table %q RLS enabled=%v forced=%v; want both true", table.Name, enabled, forced)
		}
	}
	sequence, err := pgtest.ScalarInt(ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_class WHERE relkind = 'S' AND relname = 'events_publish_sequence_seq'`)
	if err != nil || sequence != 1 {
		t.Fatalf("event publication sequence count = %d, err=%v; want 1", sequence, err)
	}
	writeSummary(t, shard, "migrations-empty", map[string]any{"migrations": len(applied), "tables": len(store.SchemaDesign().Tables)})
}

// TestMigrationReRunIsNoOpAndRewriteRefused proves the ledger contract: an
// identical re-run applies nothing, while a recorded migration whose body
// changed is refused.
func TestMigrationReRunIsNoOpAndRewriteRefused(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)
	if _, err := runner.Apply(ctx, 0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	again, err := runner.Apply(ctx, 0)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-run applied %d migrations; want 0", len(again))
	}
	ledger, err := runner.Applied(ctx)
	if err != nil || len(ledger) != storepgMigrations() {
		t.Fatalf("ledger entries = %d, err=%v; want %d", len(ledger), err, storepgMigrations())
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE kodama_test_meta.kodama_schema_migrations SET checksum = 'sha256:deadbeef' WHERE name = '0002_tables.sql'`); err != nil {
		t.Fatalf("tamper ledger: %v", err)
	}
	if _, err := runner.Apply(ctx, 0); !errors.Is(err, pgtest.ErrAppliedMigrationRewritten) {
		t.Fatalf("rewritten migration error = %v; want ErrAppliedMigrationRewritten", err)
	}
}
