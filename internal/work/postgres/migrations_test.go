package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// Test_migration_reapply: the base, event and work migrations apply and reapply
// without effect, preserve existing rows and install the intended constraints,
// row level security and runtime grants.
func Test_migration_reapply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	it := e.create(scopeA, work.KindStory, "survives reapplication")

	manifest, err := storage.NewManifest(storage.BaseContribution(), postgres.Migrations(), workpg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	res, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || res.Skipped < 1 {
		t.Fatalf("reapply applied %v skipped %d", res.Applied, res.Skipped)
	}
	if got := e.get(it); got.Title != it.Title {
		t.Fatalf("row after reapply: %+v", got)
	}
	if ms := workpg.Migrations().Migrations; len(ms) != 1 || ms[0].ID != "T011_001_work.sql" {
		t.Fatalf("contribution = %+v", ms)
	}

	var rls, forced int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE relrowsecurity), count(*) FILTER (WHERE relforcerowsecurity)
		FROM pg_class WHERE relname IN ('work_items', 'work_item_history')`).Scan(&rls, &forced); err != nil || rls != 2 || forced != 2 {
		t.Fatalf("row level security: %d %d %v", rls, forced, err)
	}
	for table, wants := range map[string]map[string]bool{
		"work_items":        {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false, "TRUNCATE": false},
		"work_item_history": {"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false},
	} {
		for priv, want := range wants {
			var has bool
			if err := e.owner.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, e.db.RuntimeRole, table, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s %s for the runtime role = %v, want %v", table, priv, has, want)
			}
		}
	}
	for _, name := range []string{"work_items_kind_known", "work_items_state_known", "work_items_version_range", "work_items_content_revision_range",
		"work_items_not_own_parent", "work_item_history_version_key", "work_items_scope_key"} {
		var n int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("constraint %s: %d %v", name, n, err)
		}
	}
	// Ownership is relational: parent and project references are composite.
	var composite int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'work_items' AND c.contype = 'f' AND array_length(c.conkey, 1) >= 2`).Scan(&composite); err != nil || composite < 2 {
		t.Fatalf("composite foreign keys on work_items: %d %v", composite, err)
	}
	for _, idx := range []string{"work_items_list_idx", "work_items_parent_idx", "work_items_active_idx", "work_items_cancelled_idx"} {
		var n int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", idx, n, err)
		}
	}
	// Database checks reject what the entity would never produce.
	for _, sql := range []string{
		`UPDATE work_items SET version = 0`,
		`UPDATE work_items SET state = 'archived'`,
		`UPDATE work_items SET state = 'review'`,    // review requires a review subject
		`UPDATE work_items SET state = 'done'`,      // done requires a completion
		`UPDATE work_items SET parent_id = id`,      // no self-parent
		`UPDATE work_items SET blocked_reason = ''`, // cleared means NULL
	} {
		if _, err := e.owner.Exec(ctx, sql); err == nil {
			t.Errorf("accepted: %s", sql)
		}
	}
}
