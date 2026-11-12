//go:build component

package postgrescomponent

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestControllerReadProductionShapeOnPostgreSQL proves the product
// Controller.Read adapter in its real unscoped production shape: it derives no
// transaction-local scope, reads a cross-scope execution row through the
// controller SELECT policy, and returns exactly the requested row's identity,
// scope, state and version rather than another tenant's row. A missing id is the
// not_found fault class on the real server. The read closes the rows iterator
// before commit (an open iterator makes pgx report "conn busy"); this test is
// the regression proof.
func TestControllerReadProductionShapeOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedExecution(ctx, scope1, "execution-1", "queued", 1, "", "")
	// The target row is in another tenant/project and carries a distinct
	// state/version so a row-filter, scope-predicate or column-mapping defect
	// cannot pass by returning the wrong row with a nil error.
	shard.SeedExecution(ctx, scope2, "execution-2", "running", 7, "", "")
	controllerDB := shard.Role(store.ControllerRole, 2)
	defer controllerDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controllerDB, store.ControllerRole)
	controller := storepg.NewController(controllerDB)

	row, err := controller.Read(ctx, "executions", "execution-2")
	if err != nil {
		t.Fatalf("Controller.Read on real PostgreSQL: %v%s", err, pgtest.CauseText(err))
	}
	if row.Table != "executions" {
		t.Fatalf("Controller.Read table = %q; want executions", row.Table)
	}
	want := map[string]any{
		"id":         "execution-2",
		"tenant_id":  "tenant-2",
		"project_id": "project-2",
		"state":      "running",
		"version":    int64(7),
	}
	for column, value := range want {
		if got := row.Values[column]; got != value {
			t.Fatalf("Controller.Read %s = %#v; want %#v (row %v)", column, got, value, row.Values)
		}
	}

	missing, err := controller.Read(ctx, "executions", "execution-missing")
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Controller.Read missing id = %v; want not_found", err)
	}
	if missing.Table != "" || len(missing.Values) != 0 {
		t.Fatalf("Controller.Read missing id returned row %+v; want the zero row", missing)
	}
}

// TestControllerScanProductionShapeOnPostgreSQL proves the product
// Controller.Scan adapter in its real unscoped production shape: it returns a
// bounded deterministic page across tenants with decoded row values, a cursor
// exactly when more rows remain, and a second page that neither repeats nor
// skips a row.
func TestControllerScanProductionShapeOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedExecution(ctx, scope1, "execution-1", "queued", 1, "", "")
	shard.SeedExecution(ctx, scope2, "execution-2", "running", 7, "", "")
	controllerDB := shard.Role(store.ControllerRole, 2)
	defer controllerDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controllerDB, store.ControllerRole)
	controller := storepg.NewController(controllerDB)

	scan := store.ControllerScan{
		Table:   "executions",
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1}
	first, err := controller.Scan(ctx, scan)
	if err != nil {
		t.Fatalf("Controller.Scan page 1: %v%s", err, pgtest.CauseText(err))
	}
	if len(first.Rows) != 1 || !first.HasMore || first.Next == "" {
		t.Fatalf("Controller.Scan page 1 rows=%d hasMore=%v next=%q; want 1 row and a cursor", len(first.Rows), first.HasMore, first.Next)
	}
	if first.Rows[0].Table != "executions" {
		t.Fatalf("Controller.Scan page 1 table = %q; want executions", first.Rows[0].Table)
	}
	if first.Rows[0].Values["id"] != "execution-1" || first.Rows[0].Values["tenant_id"] != "tenant-1" ||
		first.Rows[0].Values["state"] != "queued" || first.Rows[0].Values["version"] != int64(1) {
		t.Fatalf("Controller.Scan page 1 row = %v; want execution-1 in tenant-1 at queued/1", first.Rows[0].Values)
	}

	scan.After = first.Next
	second, err := controller.Scan(ctx, scan)
	if err != nil {
		t.Fatalf("Controller.Scan page 2: %v%s", err, pgtest.CauseText(err))
	}
	if len(second.Rows) != 1 || second.HasMore || second.Next != "" {
		t.Fatalf("Controller.Scan page 2 rows=%d hasMore=%v next=%q; want exactly the final row", len(second.Rows), second.HasMore, second.Next)
	}
	if second.Rows[0].Values["id"] != "execution-2" || second.Rows[0].Values["tenant_id"] != "tenant-2" ||
		second.Rows[0].Values["project_id"] != "project-2" ||
		second.Rows[0].Values["state"] != "running" || second.Rows[0].Values["version"] != int64(7) {
		t.Fatalf("Controller.Scan page 2 row = %v; want execution-2 in tenant-2/project-2 at running/7", second.Rows[0].Values)
	}
}
