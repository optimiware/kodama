// Cross-scope controller seam for the scope-free Kernel state port and the
// trigger EventReader.
//
// The controller is not a general unscoped repository. Read and Scan are limited
// to ControllerReadTables and Execute writes only ControllerWriteTables under
// the owning scope it derives from the execution row inside the same
// transaction. The role is a separate non-owner, NOBYPASSRLS pool; the table
// grants and the declared policies, not a role-level bypass, are what limit it.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Controller implements store.Controller over the controller-role connection
// pool.
type Controller struct {
	db DB
}

// NewController builds the cross-scope controller seam over the
// kodama_controller pool. The pool is bound to that role by configuration; the
// adapter never selects a role with SQL.
func NewController(db DB) *Controller {
	return &Controller{db: db}
}

var _ store.Controller = (*Controller)(nil)

// controllerEventCommit is the overridable write-path switch. It derives its
// default from the store declaration, which is true, so the PostgreSQL
// controller event/audit write path is active together with the memory store's.
// Tests override it to prove the forced-off refusal; the production default is
// never overridden.
var controllerEventCommit = store.ControllerEventCommitImplemented

// controllerReadTable reports whether table is in the controller read authority.
func controllerReadTable(table string) bool {
	return slices.Contains(store.ControllerReadTables(), table)
}

// controllerWriteTable reports whether table is in the controller write
// authority.
func controllerWriteTable(table string) bool {
	return slices.Contains(store.ControllerWriteTables(), table)
}

// controllerInsertOnlyTable reports whether the controller may only INSERT into
// table. The write set admits events/audit as insert-only
// publication-intent/audit rows; an update or delete is outside that authority.
func controllerInsertOnlyTable(table string) bool {
	return slices.Contains(store.ControllerInsertOnlyTables(), table)
}

// Read returns one row by identity from the controller read set. It is
// cross-scope by design, so it applies no transaction-local scope; the declared
// SELECT policy admits every row of the controller tables and the role's table
// grant is what limits the read.
func (controller *Controller) Read(ctx context.Context, table, id string) (store.Row, error) {
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
	tx, err := beginUnscoped(ctx, controller.db, true)
	if err != nil {
		return store.Row{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, store.Query{
		SQL:  fmt.Sprintf("SELECT %s FROM %s WHERE id = $1", strings.Join(meta.columnOrder, ", "), meta.table.Name),
		Args: []any{id},
	})
	if err != nil {
		return store.Row{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.Row{}, err
		}
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found", table, id))
	}
	values, err := scanColumns(meta, meta.columnOrder, rows)
	if err != nil {
		return store.Row{}, err
	}
	if err := rows.Err(); err != nil {
		return store.Row{}, err
	}
	// pgx refuses to complete a transaction while a Rows iterator is still
	// open ("conn busy"), and every engine read goes through this path, so the
	// single row must be released before the commit. The deferred Close still
	// covers every early-return path.
	if err := rows.Close(); err != nil {
		return store.Row{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Row{}, err
	}
	return store.Row{Table: table, Values: values}, nil
}

// Scan returns one bounded deterministic page from the controller read set.
func (controller *Controller) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
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
	statement, err := buildControllerScanStatement(meta, scan.ScanQuery)
	if err != nil {
		return store.QueryPage{}, err
	}
	tx, err := beginUnscoped(ctx, controller.db, true)
	if err != nil {
		return store.QueryPage{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	page, err := readControllerScanPage(ctx, tx, meta, statement, scan.Limit)
	if err != nil {
		return store.QueryPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.QueryPage{}, err
	}
	return page, nil
}

// Execute resolves the owning scope from the execution row inside one
// transaction and then applies the unit exactly like Executor.Execute under that
// derived scope. A lock or write whose existing row belongs to another scope is
// forbidden; a missing row is version_conflict. The controller unit may not
// carry a receipt. An engine unit may carry execution-bound event and audit
// rows: the owning scope is derived from the execution row with a plain SELECT,
// the transaction-local scope is set from it, and only then is the execution row
// locked FOR UPDATE and its scope/version re-verified. Every event/audit row
// must match the verified scope or the whole unit is refused.
func (controller *Controller) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if !validIdentity(executionID) {
		return store.UnitResult{}, faultInvalid(fmt.Sprintf("execution id %q is not a valid identity", executionID))
	}
	if unit.Receipt != nil || unit.ReceiptExpect != nil || unit.ReceiptMode != store.ReceiptInsert {
		return store.UnitResult{}, faultForbidden("the controller cannot commit a receipt")
	}
	// The controller holds execution-bound event/audit authority. The write
	// path is gated by the single declaration-derived switch so both the
	// memory and PostgreSQL controllers fail closed while it is off.
	if !controllerEventCommit && len(unit.Audit) > 0 {
		return store.UnitResult{}, faultForbidden("the controller cannot commit an audit row until the controller event write path is implemented")
	}
	if !controllerEventCommit && len(unit.Events) > 0 {
		return store.UnitResult{}, faultForbidden("the controller cannot commit an event until the controller event write path is implemented")
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
		if write.Delete {
			return store.UnitResult{}, faultForbidden("the controller has no delete authority")
		}
		if controllerInsertOnlyTable(write.Table) {
			// The authority is INSERT-only: a committed event or audit row
			// is immutable to this role, and every such write is refused while
			// the declaration-derived switch is off.
			if !controllerEventCommit {
				return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller table %q is not writable until the controller event write path is implemented", write.Table))
			}
			if !write.Insert {
				return store.UnitResult{}, faultForbidden(fmt.Sprintf("controller table %q is insert-only", write.Table))
			}
		}
	}
	if controllerEventWrite(unit) {
		return controller.executeEventUnit(ctx, executionID, unit)
	}

	tx, err := beginUnscoped(ctx, controller.db, false)
	if err != nil {
		return store.UnitResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	scope, err := controllerExecutionScope(ctx, tx, executionID)
	if err != nil {
		return store.UnitResult{}, err
	}
	owning, ok := tx.(*transaction)
	if !ok {
		return store.UnitResult{}, faultInternal("the controller transaction has no scope handle")
	}
	if err := owning.setScope(ctx, scope); err != nil {
		return store.UnitResult{}, err
	}

	locks, err := planLocks(scope, unit.Locks)
	if err != nil {
		return store.UnitResult{}, err
	}
	writes, err := planUnitWrites(scope, unit.Writes)
	if err != nil {
		return store.UnitResult{}, err
	}
	reclassify := func(ctx context.Context, tx store.Transaction, table, id string) error {
		return controllerReclassify(ctx, tx, scope, table, id)
	}
	if err := applyLocksWith(ctx, tx, locks, reclassify); err != nil {
		return store.UnitResult{}, err
	}
	for _, write := range writes {
		if err := applyWriteWith(ctx, tx, write, reclassify); err != nil {
			return store.UnitResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.UnitResult{}, err
	}
	return store.UnitResult{Replayed: false}, nil
}

// controllerEventWrite reports whether the unit carries an execution-bound
// event/audit write: the lifecycle event or audit slices, or a raw insert-only
// write to one of those tables. Every such unit must carry a guarded execution
// row so the owning scope is derived from a verified execution state.
func controllerEventWrite(unit store.Unit) bool {
	if len(unit.Events) > 0 || len(unit.Audit) > 0 {
		return true
	}
	for _, write := range unit.Writes {
		if controllerInsertOnlyTable(write.Table) {
			return true
		}
	}
	return false
}

// executeEventUnit applies an engine commit that carries execution-bound
// event and audit rows. The owning scope is derived from the execution row with
// a plain SELECT before the transaction-local scope is set: PostgreSQL applies
// the UPDATE policy's USING expression to a locking read, so a FOR UPDATE issued
// before the scope is set would be filtered to zero rows. The scope is then set,
// the execution row is locked FOR UPDATE under it, and its scope and version are
// re-verified against the derivation and the guard. Only then are every event
// and audit row validated and inserted. The inserts and the Kernel state writes
// commit or roll back together.
func (controller *Controller) executeEventUnit(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	guard, ok := controllerExecutionGuard(unit, executionID)
	if !ok {
		return store.UnitResult{}, faultForbidden("an event or audit unit requires a guarded execution row")
	}

	tx, err := beginUnscoped(ctx, controller.db, false)
	if err != nil {
		return store.UnitResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Derive the owning scope with a plain SELECT (the cross-scope SELECT policy
	// admits it) before the transaction-local scope exists. Never derive it from
	// caller input.
	scope, _, err := controllerExecutionScopeVersion(ctx, tx, executionID)
	if err != nil {
		return store.UnitResult{}, err
	}
	owning, ok := tx.(*transaction)
	if !ok {
		return store.UnitResult{}, faultInternal("the controller transaction has no scope handle")
	}
	if err := owning.setScope(ctx, scope); err != nil {
		return store.UnitResult{}, err
	}

	// Now that the scope is set, lock the execution row and re-read its scope
	// and version from the locked row. A row that vanished or changed scope
	// between derivation and lock is refused before any event/audit row is
	// staged.
	lockedScope, version, err := controllerLockedExecutionScope(ctx, tx, executionID)
	if err != nil {
		return store.UnitResult{}, err
	}
	if lockedScope != scope {
		return store.UnitResult{}, faultForbidden("the execution row changed scope between scope derivation and lock")
	}
	if guard.ExpectVersion != version {
		return store.UnitResult{}, faultConflict("the execution row is not at the expected version")
	}

	locks, err := planLocks(scope, unit.Locks)
	if err != nil {
		return store.UnitResult{}, err
	}
	writes, err := planUnitWrites(scope, unit.Writes)
	if err != nil {
		return store.UnitResult{}, err
	}
	// Validate every event and audit row against the derived scope before any
	// row is staged, so a single foreign row refuses the whole unit.
	for _, envelope := range unit.Events {
		if err := validateEvent(scope, envelope); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, audit := range unit.Audit {
		if err := validateAudit(scope, audit); err != nil {
			return store.UnitResult{}, err
		}
	}
	// The row's tenant/project comes from the locked execution row, not from
	// the caller-supplied envelope/audit scope, even though they were just
	// verified equal.
	for _, envelope := range unit.Events {
		envelope.Scope = scope
		if _, err := tx.Exec(ctx, eventInsertStatement(envelope)); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, audit := range unit.Audit {
		audit.Scope = scope
		if _, err := tx.Exec(ctx, auditInsertStatement(audit)); err != nil {
			return store.UnitResult{}, err
		}
	}

	reclassify := func(ctx context.Context, tx store.Transaction, table, id string) error {
		return controllerReclassify(ctx, tx, scope, table, id)
	}
	if err := applyLocksWith(ctx, tx, locks, reclassify); err != nil {
		return store.UnitResult{}, err
	}
	for _, write := range writes {
		if err := applyWriteWith(ctx, tx, write, reclassify); err != nil {
			return store.UnitResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.UnitResult{}, err
	}
	return store.UnitResult{Replayed: false}, nil
}

// controllerExecutionGuard returns the unit's guard for the target execution
// row. An event/audit unit must carry an execution lock on that exact row with
// an expected version; without it the commit would not be coupled to a verified
// execution state and is refused.
func controllerExecutionGuard(unit store.Unit, executionID string) (store.Lock, bool) {
	for _, lock := range unit.Locks {
		if lock.Table != "executions" || lock.ExpectVersion == 0 {
			continue
		}
		if slices.Contains(lock.IDs, executionID) {
			return lock, true
		}
	}
	return store.Lock{}, false
}

// controllerExecutionScopeVersion reads the owning (tenant, project) and version
// from the execution row with a plain SELECT. It runs before the
// transaction-local scope is set, so only the controller's cross-scope SELECT
// policy (the `any` predicate) filters it. It must not lock the row: PostgreSQL
// also applies the UPDATE policy's USING expression to a locking read, and
// before the scope is set that policy admits no row. A missing execution is
// not_found.
func controllerExecutionScopeVersion(ctx context.Context, tx store.Transaction, executionID string) (tenancy.Scope, uint64, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  "SELECT tenant_id, project_id, version FROM executions WHERE id = $1",
		Args: []any{executionID},
	})
	if err != nil {
		return tenancy.Scope{}, 0, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return tenancy.Scope{}, 0, err
		}
		return tenancy.Scope{}, 0, faultNotFound(fmt.Sprintf("execution %q was not found", executionID))
	}
	var tenant string
	var project sql.NullString
	var version int64
	if err := rows.Scan(&tenant, &project, &version); err != nil {
		return tenancy.Scope{}, 0, fault.Wrap(fault.InternalFailure, "read the execution owning scope", err)
	}
	if err := rows.Err(); err != nil {
		return tenancy.Scope{}, 0, err
	}
	if version < 0 {
		return tenancy.Scope{}, 0, faultInternal("the execution row has a negative version")
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenant)}
	if project.Valid {
		scope.ProjectID = id.ProjectID(project.String)
	}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, 0, fault.Wrap(fault.InternalFailure, "the execution owning scope is invalid", err)
	}
	return scope, uint64(version), nil
}

// controllerLockedExecutionScope locks the execution row FOR UPDATE under the
// already-established transaction-local scope and re-reads the owning
// (tenant, project) and version from that locked row. PostgreSQL applies the
// UPDATE policy's USING expression to a locking read, so this statement only
// sees a row that still belongs to the derived scope. A row that vanished is
// not_found; a row that exists but changed scope is forbidden, because the
// locking read was filtered by the scope it no longer matches.
func controllerLockedExecutionScope(ctx context.Context, tx store.Transaction, executionID string) (tenancy.Scope, uint64, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  "SELECT tenant_id, project_id, version FROM executions WHERE id = $1 FOR UPDATE",
		Args: []any{executionID},
	})
	if err != nil {
		return tenancy.Scope{}, 0, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Close(); err != nil {
			return tenancy.Scope{}, 0, err
		}
		if err := rows.Err(); err != nil {
			return tenancy.Scope{}, 0, err
		}
		return tenancy.Scope{}, 0, controllerLockedExecutionMiss(ctx, tx, executionID)
	}
	var tenant string
	var project sql.NullString
	var version int64
	if err := rows.Scan(&tenant, &project, &version); err != nil {
		return tenancy.Scope{}, 0, fault.Wrap(fault.InternalFailure, "read the locked execution owning scope", err)
	}
	if err := rows.Err(); err != nil {
		return tenancy.Scope{}, 0, err
	}
	if version < 0 {
		return tenancy.Scope{}, 0, faultInternal("the locked execution row has a negative version")
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenant)}
	if project.Valid {
		scope.ProjectID = id.ProjectID(project.String)
	}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, 0, fault.Wrap(fault.InternalFailure, "the locked execution owning scope is invalid", err)
	}
	return scope, uint64(version), nil
}

// controllerLockedExecutionMiss classifies a zero-row scoped execution lock. A
// plain SELECT (the cross-scope SELECT policy still admits it) distinguishes a
// row that vanished between derivation and lock from one that moved out of the
// derived scope. Both refuse the unit before anything is staged.
func controllerLockedExecutionMiss(ctx context.Context, tx store.Transaction, executionID string) error {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  "SELECT tenant_id, project_id FROM executions WHERE id = $1",
		Args: []any{executionID},
	})
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return faultForbidden("the execution row changed scope between scope derivation and lock")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return faultNotFound(fmt.Sprintf("execution %q was not found", executionID))
}

// controllerExecutionScope reads the owning (tenant, project) from the execution
// row inside the controller transaction. A missing execution is not_found.
func controllerExecutionScope(ctx context.Context, tx store.Transaction, executionID string) (tenancy.Scope, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  "SELECT tenant_id, project_id FROM executions WHERE id = $1",
		Args: []any{executionID},
	})
	if err != nil {
		return tenancy.Scope{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return tenancy.Scope{}, err
		}
		return tenancy.Scope{}, faultNotFound(fmt.Sprintf("execution %q was not found", executionID))
	}
	var tenant, project string
	if err := rows.Scan(&tenant, &project); err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.InternalFailure, "read the execution owning scope", err)
	}
	if err := rows.Err(); err != nil {
		return tenancy.Scope{}, err
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.InternalFailure, "the execution owning scope is invalid", err)
	}
	return scope, nil
}

// controllerReclassify distinguishes a row that exists in another scope from a
// same-scope row that was missing or not at the expected version. A lock or
// write that matched zero rows is forbidden only when the row id exists in a
// different (tenant, project) scope; otherwise it is a version conflict, which
// is what a stale version or guard on an in-scope row must report. The
// cross-scope existence query runs under the controller role's declared SELECT
// policy.
func controllerReclassify(ctx context.Context, tx store.Transaction, scope tenancy.Scope, table, rowID string) error {
	meta, ok := lookupTable(table)
	if !ok {
		return faultInternal(fmt.Sprintf("unknown table %q", table))
	}
	args := []any{rowID}
	scopePredicates := make([]string, 0, 2)
	if meta.hasTenant {
		args = append(args, string(scope.TenantID))
		scopePredicates = append(scopePredicates, fmt.Sprintf("tenant_id = $%d", len(args)))
	}
	if meta.hasProject {
		args = append(args, scopeColumnValue(scope))
		scopePredicates = append(scopePredicates, fmt.Sprintf("project_id IS NOT DISTINCT FROM $%d", len(args)))
	}
	statement := fmt.Sprintf("SELECT id FROM %s WHERE id = $1", meta.table.Name)
	if len(scopePredicates) > 0 {
		// NOT (...) is true only for an id whose row belongs to another scope.
		// A tenant-wide row has project_id NULL, so IS NOT DISTINCT FROM keeps
		// the tenant-scoped comparison NULL-safe instead of silently matching
		// no row.
		statement += " AND NOT (" + strings.Join(scopePredicates, " AND ") + ")"
	}
	rows, err := tx.Query(ctx, store.Query{
		SQL:  statement,
		Args: args,
	})
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return faultForbidden("a locked or written row is outside the owning scope")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return faultConflict("a locked or written row is missing or was not at the expected version")
}

// buildControllerScanStatement renders the same bounded keyset SELECT as the
// scoped scanner without any transaction-scope predicate.
func buildControllerScanStatement(meta *tableMeta, query store.ScanQuery) (scanStatement, error) {
	order := canonicalScanOrder(query.OrderBy)
	var cursor *queryCursor
	if query.After != "" {
		decoded, err := decodeQueryCursor(query.After, meta, tenancy.Scope{}, order, query.Table)
		if err != nil {
			return scanStatement{}, err
		}
		cursor = decoded
	}
	args := make([]any, 0, 8)
	predicates := make([]string, 0, 8)
	addArgument := func(value any) int {
		args = append(args, value)
		return len(args)
	}
	addComparison := func(column, operator string, value any) {
		index := addArgument(value)
		predicates = append(predicates, fmt.Sprintf("%s %s $%d", quoteIdentifier(column), operator, index))
	}
	for _, column := range sortedScanColumns(query.Equals) {
		addComparison(column, "=", query.Equals[column])
	}
	for _, filter := range query.Range {
		if filter.After != nil {
			addComparison(filter.Column, ">", filter.After)
		}
		if filter.Before != nil {
			addComparison(filter.Column, "<", filter.Before)
		}
	}
	if cursor != nil {
		predicates = append(predicates, keysetPredicate(order, cursor.Key, addArgument))
	}
	limitIndex := addArgument(query.Limit + 1)
	selectList := make([]string, 0, len(meta.columnOrder))
	for _, column := range meta.columnOrder {
		selectList = append(selectList, quoteIdentifier(column))
	}
	orderList := make([]string, 0, len(order))
	for _, term := range order {
		direction := "ASC"
		if term.Desc {
			direction = "DESC"
		}
		orderList = append(orderList, fmt.Sprintf("%s %s", quoteIdentifier(term.Column), direction))
	}
	statement := fmt.Sprintf(
		"SELECT %s FROM %s",
		strings.Join(selectList, ", "),
		quoteIdentifier(meta.table.Name),
	)
	if len(predicates) > 0 {
		statement += " WHERE " + strings.Join(predicates, " AND ")
	}
	statement += fmt.Sprintf(
		" ORDER BY %s LIMIT $%d",
		strings.Join(orderList, ", "),
		limitIndex,
	)
	return scanStatement{query: store.Query{SQL: statement, Args: args}, order: order}, nil
}

// readControllerScanPage reads at most limit+1 rows, computes HasMore and builds
// the opaque next cursor. There is no row-scope recheck because the controller
// is cross-scope by contract.
func readControllerScanPage(ctx context.Context, tx store.Transaction, meta *tableMeta, statement scanStatement, limit int) (store.QueryPage, error) {
	rows, err := tx.Query(ctx, statement.query)
	if err != nil {
		return store.QueryPage{}, err
	}
	defer func() { _ = rows.Close() }()
	decoded := make([]map[string]any, 0, limit+1)
	for rows.Next() {
		values, err := scanColumns(meta, meta.columnOrder, rows)
		if err != nil {
			return store.QueryPage{}, err
		}
		decoded = append(decoded, values)
	}
	if err := rows.Err(); err != nil {
		return store.QueryPage{}, err
	}
	page := store.QueryPage{}
	if len(decoded) > limit {
		page.HasMore = true
		decoded = decoded[:limit]
	}
	page.Rows = make([]store.Row, len(decoded))
	for index, values := range decoded {
		page.Rows[index] = store.Row{Table: meta.table.Name, Values: values}
	}
	if page.HasMore {
		cursor := encodeQueryCursor(tenancy.Scope{}, meta.table.Name, statement.order, decoded[len(decoded)-1])
		if cursor == "" {
			return store.QueryPage{}, faultInternal("controller scanner could not encode the next cursor")
		}
		page.Next = cursor
	}
	return page, nil
}
