// Regression tests: the controller must reclassify a zero-row lock/write by
// scope, not by bare existence, so a same-scope stale version is a
// version_conflict while a row in another scope stays forbidden.
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func newControllerRegressionHarness(t *testing.T, rows []seamSeedRow) *Controller {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: rows}, now)
	return NewController(db)
}

func regressionRows(actionProject string, actionVersion uint64) []seamSeedRow {
	return []seamSeedRow{
		{
			Table: "executions", TenantID: "tenant-1", ProjectID: "project-1", ID: "execution-1", Version: 3,
			Values: map[string]any{"state": "running", "principal_id": "principal-1"},
		},
		{
			Table: "actions", TenantID: "tenant-1", ProjectID: actionProject, ID: "action-1", Version: actionVersion,
			Values: map[string]any{
				"execution_id": "execution-1",
				"state":        "running",
				"index":        0,
				"deadline":     "2026-01-01T01:00:00Z",
			},
		},
	}
}

func assertRegressionFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("operation succeeded; want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error = %v (class=%q); want %q", err, class, want)
	}
}

// A same-scope row at a stale version is a version_conflict, never forbidden.
// A bare-existence reclassify would return forbidden because the row exists.
func TestControllerWriteStaleVersionIsVersionConflict(t *testing.T) {
	controller := newControllerRegressionHarness(t, regressionRows("project-1", 5))
	unit := store.Unit{Writes: []store.Write{{
		Table: "actions", ID: "action-1", ExpectVersion: 2,
		Values: map[string]any{"state": "succeeded"},
	}}}
	_, err := controller.Execute(context.Background(), "execution-1", unit)
	assertRegressionFaultClass(t, err, fault.VersionConflict)
}

// A stale guarded lock (the lock matches scope, the version guard does not) is
// also a same-scope version_conflict.
func TestControllerLockGuardStaleVersionIsVersionConflict(t *testing.T) {
	controller := newControllerRegressionHarness(t, regressionRows("project-1", 5))
	unit := store.Unit{Locks: []store.Lock{{
		Table: "actions", IDs: []string{"action-1"}, ExpectVersion: 2,
	}}}
	_, err := controller.Execute(context.Background(), "execution-1", unit)
	assertRegressionFaultClass(t, err, fault.VersionConflict)
}

// A deadline-guarded write whose same-scope row is not at the expected version
// misses the deadline query and must report version_conflict.
func TestControllerDeadlineWriteStaleVersionIsVersionConflict(t *testing.T) {
	controller := newControllerRegressionHarness(t, regressionRows("project-1", 5))
	unit := store.Unit{Writes: []store.Write{{
		Table: "actions", ID: "action-1", ExpectVersion: 2,
		Deadline: &store.DeadlineGuard{Column: "deadline"},
		Values:   map[string]any{"state": "succeeded"},
	}}}
	_, err := controller.Execute(context.Background(), "execution-1", unit)
	assertRegressionFaultClass(t, err, fault.VersionConflict)
}

// A same-scope missing row is a version_conflict, not forbidden.
func TestControllerMissingRowIsVersionConflict(t *testing.T) {
	controller := newControllerRegressionHarness(t, regressionRows("project-1", 5))
	unit := store.Unit{Writes: []store.Write{{
		Table: "actions", ID: "action-9", ExpectVersion: 2,
		Values: map[string]any{"state": "succeeded"},
	}}}
	_, err := controller.Execute(context.Background(), "execution-1", unit)
	assertRegressionFaultClass(t, err, fault.VersionConflict)
}

// A row that exists in a different scope is still forbidden, for both a write
// and a lock.
func TestControllerOtherScopeRowIsForbidden(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		controller := newControllerRegressionHarness(t, regressionRows("project-2", 1))
		unit := store.Unit{Writes: []store.Write{{
			Table: "actions", ID: "action-1", ExpectVersion: 1,
			Values: map[string]any{"state": "succeeded"},
		}}}
		_, err := controller.Execute(context.Background(), "execution-1", unit)
		assertRegressionFaultClass(t, err, fault.Forbidden)
	})
	t.Run("lock", func(t *testing.T) {
		controller := newControllerRegressionHarness(t, regressionRows("project-2", 1))
		unit := store.Unit{Locks: []store.Lock{{
			Table: "actions", IDs: []string{"action-1"}, ExpectVersion: 1,
		}}}
		_, err := controller.Execute(context.Background(), "execution-1", unit)
		assertRegressionFaultClass(t, err, fault.Forbidden)
	})
}

// The scope predicate must treat a NULL project as a value: a tenant-scoped
// derived scope matches a tenant-wide row, while a project scope does not.
func TestControllerReclassifyTenantWideProject(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: []seamSeedRow{{
		Table: "events", TenantID: "tenant-1", ProjectID: "", ID: "event-1", Version: 1,
		Values: map[string]any{"type": "project.created", "publish_sequence": 1},
	}}}, now)
	ctx := context.Background()
	tx, err := beginUnscoped(ctx, db, false)
	if err != nil {
		t.Fatalf("begin unscoped: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tenantWide := tenancy.Scope{TenantID: "tenant-1"}
	if err := controllerReclassify(ctx, tx, tenantWide, "events", "event-1"); err == nil {
		t.Fatal("tenant-wide row in the same tenant scope reclassified as forbidden; want version_conflict")
	} else {
		assertRegressionFaultClass(t, err, fault.VersionConflict)
	}

	project := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	if err := controllerReclassify(ctx, tx, project, "events", "event-1"); err == nil {
		t.Fatal("tenant-wide row outside the project scope reclassified as a conflict; want forbidden")
	} else {
		assertRegressionFaultClass(t, err, fault.Forbidden)
	}
}
