package postgres_test

import (
	"context"
	"testing"
	"time"

	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var flowViews = []string{"work_flow_steps", "work_flow_segments", "work_flow_episodes", "work_flow_blocked_runs"}

// Test_metrics_migration_reapply: the migrations apply in dependency order and
// reapply without effect; the projections are plain security-invoker views the
// runtime role may only read; T-011's append-only history is untouched; no table,
// trigger or materialized state is introduced.
func Test_metrics_migration_reapply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	it := e.toActive(e.create(scopeA, at(0), "survives reapplication"), at(mins(1)), at(mins(2)))
	w := window(at(0), at(mins(10)))
	before := e.summarize(it, w)

	manifest, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workpg.Migrations(), metricspg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	res, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: e.stack.DB.RuntimeRole})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || res.Skipped < 1 {
		t.Fatalf("reapply applied %v skipped %d", res.Applied, res.Skipped)
	}
	if after := e.summarize(it, w); after != before {
		t.Fatalf("summary changed across reapplication: %+v vs %+v", after, before)
	}
	if c := metricspg.Migrations(); c.Module != "metrics" || len(c.Migrations) != 1 || c.Migrations[0].ID != "T042_001_flow_intervals.sql" {
		t.Fatalf("contribution = %+v", c)
	}

	t.Run("views_are_plain_security_invoker_views", func(t *testing.T) {
		for _, v := range flowViews {
			var kind string
			var invoker bool
			if err := e.owner.QueryRow(ctx, `SELECT c.relkind::text, coalesce('security_invoker=true' = ANY(c.reloptions), false)
				FROM pg_class c WHERE c.relname = $1`, v).Scan(&kind, &invoker); err != nil {
				t.Fatalf("%s: %v", v, err)
			}
			if kind != "v" || !invoker {
				t.Errorf("%s: relkind %q security_invoker %v, want an ordinary security-invoker view", v, kind, invoker)
			}
		}
		// Nothing is stored: the module owns no table, materialized view, trigger or function.
		var n int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname LIKE 'work\_flow\_%' AND relkind <> 'v'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("stored flow objects: %d %v", n, err)
		}
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE c.relname = 'work_item_history' AND NOT t.tgisinternal`).Scan(&n); err != nil || n != 0 {
			t.Errorf("triggers on work_item_history: %d %v", n, err)
		}
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname LIKE '%flow%'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("flow functions: %d %v", n, err)
		}
	})

	t.Run("runtime_privileges_are_read_only", func(t *testing.T) {
		for _, v := range flowViews {
			for priv, want := range map[string]bool{"SELECT": true, "INSERT": false, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
				var has bool
				if err := e.owner.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, e.stack.DB.RuntimeRole, v, priv).Scan(&has); err != nil {
					t.Fatal(err)
				}
				if has != want {
					t.Errorf("%s %s for the runtime role = %v, want %v", v, priv, has, want)
				}
			}
		}
		// T-011's grants are unchanged: history is append-only for the runtime role.
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
			var has bool
			if err := e.owner.QueryRow(ctx, `SELECT has_table_privilege($1, 'work_item_history', $2)`, e.stack.DB.RuntimeRole, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("work_item_history %s for the runtime role = %v, want %v", priv, has, want)
			}
		}
	})

	t.Run("history_stays_append_only", func(t *testing.T) {
		// Not even the owner has an update or delete policy: the views read an
		// immutable record.
		tag, err := e.owner.Exec(ctx, `UPDATE work_item_history SET reason = 'rewritten'`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("owner update: %v rows, %v", tag.RowsAffected(), err)
		}
		tag, err = e.owner.Exec(ctx, `DELETE FROM work_item_history`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("owner delete: %v rows, %v", tag.RowsAffected(), err)
		}
		if after := e.summarize(it, w); after != before {
			t.Errorf("summary changed: %+v", after)
		}
	})

	t.Run("dependency_order_is_enforced", func(t *testing.T) {
		// Without the work contribution the views cannot be created, and the
		// failed file leaves nothing behind.
		db := pgtest.New(t)
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		owner := db.MigrationPool(cctx, 2)
		bad, err := storage.NewManifest(storage.BaseContribution(), metricspg.Migrations())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(cctx, owner, bad, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err == nil {
			t.Fatal("the flow views were created without work_item_history")
		}
		var n int
		if err := owner.QueryRow(cctx, `SELECT count(*) FROM pg_class WHERE relname LIKE 'work\_flow\_%'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a failed migration left %d objects: %v", n, err)
		}
	})
}
