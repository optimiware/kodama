package memory

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var _ store.Controller = controller{}

// controllerEventCommit is the adapter-local gate for the execution-bound
// event/audit write path. Production follows the store declaration, which is
// true; this package's tests override it (and restore it) to prove the
// forced-off refusal.
var controllerEventCommit = store.ControllerEventCommitImplemented

// Controller returns the cross-scope Kernel-state and trigger-event seam backed
// by this store. The wrapper exists because the scoped Executor.Read and the
// controller's unscoped Read cannot share one method set; the wrapper exposes
// only the three controller methods and no generic repository.
func (s *Store) Controller() store.Controller {
	return controller{store: s}
}

// controller implements store.Controller over one Store. It is not a general
// unscoped repository: reads are limited to the controller table set and writes
// are limited to the state tables under the owning scope derived from the
// execution row.
type controller struct {
	store *Store
}

// Read returns one row by identity from the controller read set. The read is
// deliberately cross-scope, so the table authority is the only predicate.
func (c controller) Read(ctx context.Context, table, id string) (store.Row, error) {
	if err := ctx.Err(); err != nil {
		return store.Row{}, err
	}
	if !controllerReadTable(table) {
		return store.Row{}, faultInvalid(fmt.Sprintf("controller read table %q is not in the controller authority", table))
	}
	if !validIdentity(id) {
		return store.Row{}, faultInvalid(fmt.Sprintf("row id %q is not a valid identity", id))
	}
	meta, ok := lookupTable(table)
	if !ok {
		return store.Row{}, faultInvalid(fmt.Sprintf("unknown table %q", table))
	}

	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if table == "events" {
		for _, committed := range c.store.state.events {
			if string(committed.Envelope.ID) == id {
				return store.Row{Table: table, Values: eventRowValues(committed)}, nil
			}
		}
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found", table, id))
	}
	values, ok := c.store.state.row(table, id)
	if !ok {
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found", table, id))
	}
	return store.Row{Table: table, Values: materializeValues(meta, values)}, nil
}

// Scan returns one bounded deterministic page from the controller read set
// across every scope. The scan shape, table authority, columns, ordering and
// cursor are validated by ControllerScan before any row is inspected.
func (c controller) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scan.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	meta, ok := lookupTable(scan.Table)
	if !ok {
		return store.QueryPage{}, faultInvalid(fmt.Sprintf("unknown table %q", scan.Table))
	}
	prepared, err := prepareScanQuery(meta, tenancy.Scope{}, scan.ScanQuery)
	if err != nil {
		return store.QueryPage{}, err
	}
	candidates := c.store.snapshotScanRowsAll(meta)
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	return buildScanPage(scan.ScanQuery, prepared, tenancy.Scope{}, candidates), nil
}

// Execute resolves the owning scope from the execution row inside the same
// atomic section and then applies the unit under that scope exactly like
// Executor.Execute. A row outside the state tables or the owning scope is
// refused, and a receipt is always refused.
//
// When the event/audit write path is enabled, the unit may also carry
// execution-bound lifecycle event rows and audit rows committed atomically with
// its state writes. The unit must itself lock the execution row; every event and
// audit row must carry exactly the derived scope or the whole unit is refused
// and nothing is staged; events and audit are INSERT-only and audit stays
// write-only for this seam.
func (c controller) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if !validIdentity(executionID) {
		return store.UnitResult{}, faultInvalid(fmt.Sprintf("execution id %q is not a valid identity", executionID))
	}

	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	execution, ok := c.store.state.row("executions", executionID)
	if !ok {
		return store.UnitResult{}, faultNotFound(fmt.Sprintf("execution %q was not found", executionID))
	}
	tenant, _ := execution["tenant_id"].(string)
	project, _ := execution["project_id"].(string)
	scope := tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
	if err := scope.Validate(); err != nil {
		return store.UnitResult{}, faultInternal("the execution row has no usable owning scope")
	}
	for _, lock := range unit.Locks {
		// The controller authority never performs a membership authority check,
		// so an absence guard is outside it even on a controller-writable table.
		if lock.ExpectAbsent {
			return store.UnitResult{}, faultForbidden("the controller cannot use an absence guard")
		}
		if !controllerWriteTable(lock.Table) {
			return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller lock table %q is outside the controller authority", lock.Table))
		}
	}
	for _, write := range unit.Writes {
		if !controllerWriteTable(write.Table) {
			return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller write table %q is outside the controller authority", write.Table))
		}
		if controllerInsertOnlyTable(write.Table) {
			// The authority is INSERT-only. While the declaration-derived
			// gate is off the adapter refuses every event/audit write.
			if !controllerEventCommit {
				return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller table %q is not writable until the controller event write path is implemented", write.Table))
			}
			if !write.Insert {
				return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller table %q is insert-only", write.Table))
			}
		}
	}
	// An execution-bound event/audit unit must itself lock the execution row.
	// The lock proves the caller named the execution whose row the atomic
	// section derives the owning scope from; without it the unit cannot be
	// tied to a guarded execution and is refused before anything is staged.
	if controllerEventCommit && (len(unit.Events) > 0 || len(unit.Audit) > 0) && !controllerExecutionLocked(unit, executionID) {
		return store.UnitResult{}, faultForbidden("an event or audit unit requires a locked execution row")
	}
	return c.store.executeLocked(scope, unit, true)
}

// controllerExecutionLocked reports whether the unit itself locks the execution
// row whose scope the controller derived. A guard is permitted but not required:
// the lock serializes the row and fails when it is absent or out of scope.
func controllerExecutionLocked(unit store.Unit, executionID string) bool {
	for _, lock := range unit.Locks {
		if lock.Table != "executions" {
			continue
		}
		if slices.Contains(lock.IDs, executionID) {
			return true
		}
	}
	return false
}

func controllerReadTable(table string) bool {
	return slices.Contains(store.ControllerReadTables(), table)
}

func controllerWriteTable(table string) bool {
	return slices.Contains(store.ControllerWriteTables(), table)
}

func controllerInsertOnlyTable(table string) bool {
	return slices.Contains(store.ControllerInsertOnlyTables(), table)
}
