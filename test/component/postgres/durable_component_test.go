//go:build component

package postgrescomponent

import (
	"context"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// scopedRow runs one query through the application role inside a
// transaction-scoped read and returns its single row.
func scopedRow(t *testing.T, ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, columns int, query string, args ...any) []string {
	t.Helper()
	rows, err := pgtest.ScopedQuery(ctx, db, scope, columns, query, args...)
	if err != nil {
		t.Fatalf("application-role query %q: %v%s", query, err, pgtest.CauseText(err))
	}
	if len(rows) != 1 {
		t.Fatalf("application-role query %q returned %d rows; want 1", query, len(rows))
	}
	return rows[0]
}

// scopedCount runs one count query through the application role.
func scopedCount(t *testing.T, ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, query string, args ...any) int64 {
	t.Helper()
	row := scopedRow(t, ctx, db, scope, 1, query, args...)
	value, err := strconv.ParseInt(row[0], 10, 64)
	if err != nil {
		t.Fatalf("application-role count %q = %q: %v", query, row[0], err)
	}
	return value
}

// requireScopedCount asserts a durable row count read back through the
// application role, so a commit that returned success without persisting the
// row cannot pass.
func requireScopedCount(t *testing.T, ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, query string, want int64, label string, args ...any) {
	t.Helper()
	if got := scopedCount(t, ctx, db, scope, query, args...); got != want {
		t.Fatalf("%s = %d; want %d", label, got, want)
	}
}

// TestGuardedDeleteRefusesAStaleVersionOnPostgreSQL proves the adapter's delete
// statement itself carries the optimistic version guard: a delete whose expected
// version is stale removes nothing and fails version_conflict even when the unit
// carries no separate row lock.
func TestGuardedDeleteRefusesAStaleVersionOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.TenantScope("tenant-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, scope, "membership-1", "principal-1", 2, string(tenancy.RoleEngineer))
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	_, err := executor.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{{
			Table: "memberships", ID: "membership-1", Delete: true, ExpectVersion: 1,
		}},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("stale guarded delete error = %v; want version_conflict", err)
	}
	requireScopedCount(t, ctx, runtime, scope,
		`SELECT count(*) FROM memberships WHERE id = 'membership-1'`, 1, "memberships after a stale delete")
}

// TestUnguardedLockFailsOnAMissingRowOnPostgreSQL proves a lock with no
// version/column guard still fails when the row is absent, instead of silently
// serializing nothing.
func TestUnguardedLockFailsOnAMissingRowOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.TenantScope("tenant-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	_, err := executor.Execute(ctx, scope, store.Unit{
		Locks: []store.Lock{{Table: "memberships", IDs: []string{"membership-missing"}}},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("unguarded lock on a missing row error = %v; want version_conflict", err)
	}
}
