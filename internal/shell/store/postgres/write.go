package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// reservedWriteColumns are supplied by the adapter from the transaction scope
// and the row identity. A caller that passes one is trying to write outside its
// own scope or replace the row identity, so it is refused before any statement.
var reservedWriteColumns = [...]string{"id", "tenant_id", "project_id"}

// WriteRow applies one parameterized write inside an existing transaction.
func WriteRow(ctx context.Context, tx store.Transaction, scope tenancy.Scope, write store.Write) error {
	planned, err := planWrite(scope, write)
	if err != nil {
		return err
	}
	if err := checkActiveScope(tx, scope); err != nil {
		return err
	}
	return applyWrite(ctx, tx, planned)
}

// plannedWrite is one fully rendered write statement plus the facts needed to
// interpret its result.
type plannedWrite struct {
	table     string
	id        string
	insert    bool
	delete    bool
	rank      float64
	statement store.Statement
	// advisory is the transaction-scoped lock an insert into a table with
	// registered absence-guardable unique keys must take before its row. It is
	// nil for every other write. advisoryTaken records that the unit's lock
	// phase already acquired it in the declared lock order, so the write phase
	// must not issue it a second time.
	advisory      *store.Statement
	advisoryTaken bool
	deadline      *store.DeadlineGuard
	scope         tenancy.Scope
	expectVersion uint64
	expectNames   []string
	expect        map[string]any
}

// planWrite validates one write and renders its fixed, parameterized SQL. It
// performs no I/O: every refusal happens before a statement reaches the
// transaction.
func planWrite(scope tenancy.Scope, write store.Write) (plannedWrite, error) {
	meta, ok := lookupTable(write.Table)
	if !ok {
		return plannedWrite{}, faultInvalid(fmt.Sprintf("unknown table %q", write.Table))
	}
	if err := scope.Validate(); err != nil {
		return plannedWrite{}, err
	}
	if !validIdentity(write.ID) {
		return plannedWrite{}, faultInvalid(fmt.Sprintf("row id %q is not a valid identity", write.ID))
	}
	// The tenant and project root rows carry their own identity, so the row id
	// itself must be inside the transaction scope. This is the write-side mirror
	// of the read-side checkRowIdentityScope; it runs before any statement.
	if err := checkRowIdentityScope(meta, scope, write.ID); err != nil {
		return plannedWrite{}, err
	}
	if meta.projectRequired && scope.TenantScoped() {
		return plannedWrite{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope write is not allowed", write.Table))
	}
	// The receipt/audit publication rule, the events acknowledgement rule and
	// the version/guard shape live in the neutral seam and run at the same
	// point in both adapters, so the memory adapter refuses exactly the same
	// writes with the same class.
	if err := store.ValidateWrite(write); err != nil {
		return plannedWrite{}, err
	}

	names := make([]string, 0, len(write.Values))
	for name, value := range write.Values {
		if err := validateWriteColumn(meta, write, name, value); err != nil {
			return plannedWrite{}, err
		}
		names = append(names, name)
	}
	sort.Strings(names)

	expectNames := make([]string, 0, len(write.Expect))
	if write.Insert && len(write.Expect) > 0 {
		return plannedWrite{}, faultInvalid("an insert cannot carry expected column values")
	}
	for name, value := range write.Expect {
		if err := validateGuardColumn(meta, name, value); err != nil {
			return plannedWrite{}, err
		}
		expectNames = append(expectNames, name)
	}
	sort.Strings(expectNames)

	if !write.Insert && !write.Delete && len(names) == 0 {
		return plannedWrite{}, faultInvalid("an update must set at least one column")
	}
	if write.Deadline != nil {
		if write.Insert {
			return plannedWrite{}, faultInvalid("an insert has no existing row for a deadline guard")
		}
		if err := validateDeadlineGuard(meta, *write.Deadline); err != nil {
			return plannedWrite{}, err
		}
	}

	statement, err := renderWrite(meta, scope, write, names, expectNames)
	if err != nil {
		return plannedWrite{}, err
	}
	var advisory *store.Statement
	if write.Insert {
		advisory, err = absenceInsertAdvisory(scope, write)
		if err != nil {
			return plannedWrite{}, err
		}
	}
	return plannedWrite{
		table:         write.Table,
		id:            write.ID,
		insert:        write.Insert,
		delete:        write.Delete,
		rank:          writeRank(write.Table),
		statement:     statement,
		advisory:      advisory,
		deadline:      write.Deadline,
		scope:         scope,
		expectVersion: write.ExpectVersion,
		expectNames:   expectNames,
		expect:        write.Expect,
	}, nil
}

// validateDeadlineGuard refuses an empty, unknown or non-timestamp deadline
// column before any statement runs.
func validateDeadlineGuard(meta *tableMeta, guard store.DeadlineGuard) error {
	if err := guard.Validate(); err != nil {
		return err
	}
	column, ok := meta.columns[guard.Column]
	if !ok {
		return faultInvalid(fmt.Sprintf("table %q has no deadline column %q", meta.table.Name, guard.Column))
	}
	if column.Type != "timestamptz" {
		return faultInvalid(fmt.Sprintf("deadline column %q is not a timestamp column", guard.Column))
	}
	return nil
}

// validateWriteColumn refuses a reserved, unknown, null-violating or
// version-replacing value column.
func validateWriteColumn(meta *tableMeta, write store.Write, name string, value any) error {
	for _, reserved := range reservedWriteColumns {
		if name == reserved {
			return faultInvalid(fmt.Sprintf("column %q is supplied by the adapter, not the caller", name))
		}
	}
	column, ok := meta.columns[name]
	if !ok {
		return faultInvalid(fmt.Sprintf("table %q has no column %q", meta.table.Name, name))
	}
	if !write.Insert && name == "version" {
		return faultInvalid("the adapter advances the version column; an update cannot replace it")
	}
	if value == nil && !column.Nullable {
		return faultInvalid(fmt.Sprintf("column %q is not nullable", name))
	}
	return nil
}

// validateGuardColumn refuses an unknown, reserved or version-duplicating
// optimistic guard. It is shared by write expectations and read-only lock
// guards so both assert the same row shape.
func validateGuardColumn(meta *tableMeta, name string, value any) error {
	for _, reserved := range reservedWriteColumns {
		if name == reserved {
			return faultInvalid(fmt.Sprintf("column %q is supplied by the adapter, not the caller", name))
		}
	}
	column, ok := meta.columns[name]
	if !ok {
		return faultInvalid(fmt.Sprintf("table %q has no column %q", meta.table.Name, name))
	}
	if meta.hasVersion && name == "version" {
		return faultInvalid("use ExpectVersion for the version column")
	}
	if value == nil && !column.Nullable {
		return faultInvalid(fmt.Sprintf("column %q is not nullable", name))
	}
	return nil
}

// renderWrite renders the insert or the versioned update. Scope columns always
// travel from the verified transaction scope; values always travel as
// arguments, so no caller value can change the statement text.
func renderWrite(meta *tableMeta, scope tenancy.Scope, write store.Write, names, expectNames []string) (store.Statement, error) {
	// The tenant/project root rows are bound to the scope's own identity. The
	// planning path already refuses a foreign root row; checking again at render
	// time keeps this statement builder from ever emitting an out-of-scope root
	// write if it is reused.
	if err := checkRowIdentityScope(meta, scope, write.ID); err != nil {
		return store.Statement{}, err
	}
	if write.Delete {
		return renderDelete(meta, scope, write, expectNames), nil
	}
	args := make([]any, 0, len(names)+len(expectNames)+4)
	if write.Insert {
		columns := make([]string, 0, len(names)+3)
		placeholders := make([]string, 0, len(names)+3)
		add := func(column string, value any) {
			columns = append(columns, column)
			args = append(args, value)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		add("id", write.ID)
		if meta.hasTenant {
			add("tenant_id", string(scope.TenantID))
		}
		if meta.hasProject {
			add("project_id", scopeColumnValue(scope))
		}
		for _, name := range names {
			add(name, write.Values[name])
		}
		return store.Statement{
			SQL: fmt.Sprintf(
				"INSERT INTO %s (%s) VALUES (%s)",
				write.Table, strings.Join(columns, ", "), strings.Join(placeholders, ", "),
			),
			Args: args,
		}, nil
	}

	assignments := make([]string, 0, len(names)+1)
	for _, name := range names {
		args = append(args, write.Values[name])
		assignments = append(assignments, fmt.Sprintf("%s = $%d", name, len(args)))
	}
	if meta.hasVersion {
		assignments = append(assignments, "version = version + 1")
	}

	predicates := make([]string, 0, len(expectNames)+5)
	args = append(args, write.ID)
	predicates = append(predicates, fmt.Sprintf("id = $%d", len(args)))
	if meta.hasTenant {
		args = append(args, string(scope.TenantID))
		predicates = append(predicates, fmt.Sprintf("tenant_id = $%d", len(args)))
	}
	if meta.hasProject {
		if scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			args = append(args, string(scope.ProjectID))
			predicates = append(predicates, fmt.Sprintf("project_id = $%d", len(args)))
		}
	}
	if meta.hasVersion {
		args = append(args, write.ExpectVersion)
		predicates = append(predicates, fmt.Sprintf("version = $%d", len(args)))
	}
	for _, name := range expectNames {
		if write.Expect[name] == nil {
			predicates = append(predicates, fmt.Sprintf("%s IS NULL", name))
			continue
		}
		args = append(args, write.Expect[name])
		predicates = append(predicates, fmt.Sprintf("%s = $%d", name, len(args)))
	}
	return store.Statement{
		SQL: fmt.Sprintf(
			"UPDATE %s SET %s WHERE %s",
			write.Table, strings.Join(assignments, ", "), strings.Join(predicates, " AND "),
		),
		Args: args,
	}, nil
}

// renderDelete renders the guarded row removal. The identity, the ownership
// scope, the expected version and every explicit column guard travel as
// arguments, so a delete can never reach another scope or remove a row that
// changed after the caller read it.
func renderDelete(meta *tableMeta, scope tenancy.Scope, write store.Write, expectNames []string) store.Statement {
	args := make([]any, 0, len(expectNames)+4)
	predicates := make([]string, 0, len(expectNames)+4)
	add := func(column string, value any) {
		args = append(args, value)
		predicates = append(predicates, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	add("id", write.ID)
	if meta.hasTenant {
		add("tenant_id", string(scope.TenantID))
	}
	if meta.hasProject {
		if scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			add("project_id", string(scope.ProjectID))
		}
	}
	if meta.hasVersion {
		add("version", write.ExpectVersion)
	}
	for _, name := range expectNames {
		if write.Expect[name] == nil {
			predicates = append(predicates, fmt.Sprintf("%s IS NULL", name))
			continue
		}
		add(name, write.Expect[name])
	}
	return store.Statement{
		SQL: fmt.Sprintf(
			"DELETE FROM %s WHERE %s",
			write.Table, strings.Join(predicates, " AND "),
		),
		Args: args,
	}
}

// scopeColumnValue is the project column value for a write: the verified
// project, or SQL NULL for a tenant-wide row so NULLS NOT DISTINCT uniqueness
// and tenant-scope policies see one stable shape.
func scopeColumnValue(scope tenancy.Scope) any {
	if scope.TenantScoped() {
		return nil
	}
	return string(scope.ProjectID)
}

// reclassifyMissing turns a zero-row lock/write outcome into the precise fault.
// The controller seam passes one so a row that exists in another scope is
// forbidden while a genuinely missing row is version_conflict. The scoped
// runtime seam passes nil: its RLS policy already hides foreign rows, so a
// zero-row outcome is a version conflict.
type reclassifyMissing func(ctx context.Context, tx store.Transaction, table, id string) error

// applyWrite executes one planned write and interprets its row count: a zero
// row update or delete is a stale version/guard, never a silent success.
func applyWrite(ctx context.Context, tx store.Transaction, planned plannedWrite) error {
	return applyWriteWith(ctx, tx, planned, nil)
}

func applyWriteWith(ctx context.Context, tx store.Transaction, planned plannedWrite, reclassify reclassifyMissing) error {
	// An insert into a table with a registered absence-guardable unique key
	// takes the same transaction-scoped advisory lock the absence guard takes,
	// and it takes it before the row so the check and the insert cannot be
	// split. An unguarded insert therefore still serializes against a guarded
	// first grant. A planned unit acquires the lock in its lock phase; a
	// standalone WriteRow acquires it here.
	if planned.advisory != nil && !planned.advisoryTaken {
		if _, err := tx.Exec(ctx, *planned.advisory); err != nil {
			return err
		}
	}
	if planned.deadline != nil {
		if err := checkWriteDeadline(ctx, tx, planned, reclassify); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, planned.statement)
	if err != nil {
		if planned.advisory != nil {
			return mapAbsenceInsertError(err)
		}
		return err
	}
	if planned.insert {
		if result.RowsAffected == 0 {
			return faultInternal(fmt.Sprintf("insert into %q affected no row", planned.table))
		}
		return nil
	}
	if result.RowsAffected == 0 {
		if reclassify != nil {
			return reclassify(ctx, tx, planned.table, planned.id)
		}
		return faultConflict(fmt.Sprintf("%s %q was not at the expected version", planned.table, planned.id))
	}
	return nil
}

// checkWriteDeadline reads the targeted row's deadline column under the same
// scope/version predicates as the write, locks it, and asserts it is strictly
// after the transaction clock before the write applies.
func checkWriteDeadline(ctx context.Context, tx store.Transaction, planned plannedWrite, reclassify reclassifyMissing) error {
	meta, ok := lookupTable(planned.table)
	if !ok {
		return faultInternal(fmt.Sprintf("the frozen schema has no %q table", planned.table))
	}
	guard := *planned.deadline
	now, err := transactionNow(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, writeDeadlineQuery(meta, planned, guard))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		if reclassify != nil {
			return reclassify(ctx, tx, planned.table, planned.id)
		}
		return faultConflict(fmt.Sprintf("%s %q is missing or out of scope", planned.table, planned.id))
	}
	var stamp sql.NullTime
	if err := rows.Scan(&stamp); err != nil {
		return fault.Wrap(fault.InternalFailure, "scan deadline guard", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !stamp.Valid || !stamp.Time.After(now) {
		return fault.New(fault.DeadlineExpired, "the guarded timestamp is not strictly after the transaction clock")
	}
	return nil
}

// writeDeadlineQuery selects the deadline column with the same identity, scope,
// version and explicit guards the write uses, and locks the row so the check and
// the write cannot be split by a concurrent change.
func writeDeadlineQuery(meta *tableMeta, planned plannedWrite, guard store.DeadlineGuard) store.Query {
	args := make([]any, 0, len(planned.expectNames)+5)
	predicates := make([]string, 0, len(planned.expectNames)+5)
	add := func(column string, value any) {
		args = append(args, value)
		predicates = append(predicates, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	add("id", planned.id)
	if meta.hasTenant {
		add("tenant_id", string(planned.scope.TenantID))
	}
	if meta.hasProject {
		if planned.scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			add("project_id", string(planned.scope.ProjectID))
		}
	}
	if meta.hasVersion {
		add("version", planned.expectVersion)
	}
	for _, name := range planned.expectNames {
		if planned.expect[name] == nil {
			predicates = append(predicates, fmt.Sprintf("%s IS NULL", name))
			continue
		}
		add(name, planned.expect[name])
	}
	return store.Query{
		SQL: fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s FOR UPDATE",
			guard.Column, meta.table.Name, strings.Join(predicates, " AND "),
		),
		Args: args,
	}
}

// LockRows acquires row locks in the declared lock order inside an existing
// transaction. Every lock is validated before the first statement is issued.
func LockRows(ctx context.Context, tx store.Transaction, scope tenancy.Scope, locks []store.Lock) error {
	plans, err := planLocks(scope, locks)
	if err != nil {
		return err
	}
	if err := checkActiveScope(tx, scope); err != nil {
		return err
	}
	return applyLocks(ctx, tx, plans)
}

// plannedLocks is the validated lock phase: the row locks in declared order,
// followed by the read-only guards that assert authority, dependency facts or a
// transaction-time deadline.
type plannedLocks struct {
	entries []plannedLock
}

// plannedLock is one row lock, its optional guard query, its optional deadline
// assertion, or one absence guard with its advisory lock and NULL-safe
// existence check.
type plannedLock struct {
	rank      float64
	position  int
	table     string
	id        string
	statement store.Statement
	guard     *store.Query
	deadline  *store.DeadlineGuard
	// absence marks an absence guard. It takes advisory before the guard
	// phase and issues absenceGuard there, refusing when any row matches.
	absence      bool
	absenceKey   string
	advisory     store.Statement
	absenceGuard *store.Query
	// insertAdvisory marks the transaction-scoped lock an insert into an
	// absence-guardable table takes in the lock phase; it issues advisory and
	// has no guard query.
	insertAdvisory bool
}

// applyLocks issues every lock, then every guard. A zero-row lock or guard is a
// version_conflict: the row is missing, out of scope, or no longer at the
// expected version, so the unit must not continue. An absence guard takes its
// transaction-scoped advisory lock in the lock phase, in the declared lock
// order, and refuses when its NULL-safe existence check finds a row.
func applyLocks(ctx context.Context, tx store.Transaction, plans plannedLocks) error {
	return applyLocksWith(ctx, tx, plans, nil)
}

func applyLocksWith(ctx context.Context, tx store.Transaction, plans plannedLocks, reclassify reclassifyMissing) error {
	for _, entry := range plans.entries {
		if entry.absence || entry.insertAdvisory {
			if _, err := tx.Exec(ctx, entry.advisory); err != nil {
				return err
			}
			continue
		}
		result, err := tx.Exec(ctx, entry.statement)
		if err != nil {
			return err
		}
		if result.RowsAffected == 0 {
			if reclassify != nil {
				return reclassify(ctx, tx, entry.table, entry.id)
			}
			return faultConflict("a locked row is missing or out of scope")
		}
	}
	var now time.Time
	haveNow := false
	for _, entry := range plans.entries {
		if entry.absence {
			rows, err := tx.Query(ctx, *entry.absenceGuard)
			if err != nil {
				return err
			}
			found := rows.Next()
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			_ = rows.Close()
			if found {
				return faultConflict("an absence guard matched an existing row")
			}
			continue
		}
		if entry.guard == nil {
			continue
		}
		if entry.deadline != nil && !haveNow {
			var err error
			now, err = transactionNow(ctx, tx)
			if err != nil {
				return err
			}
			haveNow = true
		}
		rows, err := tx.Query(ctx, *entry.guard)
		if err != nil {
			return err
		}
		found := rows.Next()
		if !found {
			err := rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			if reclassify != nil {
				return reclassify(ctx, tx, entry.table, entry.id)
			}
			return faultConflict("a guarded row is missing or was not at the expected version")
		}
		if entry.deadline != nil {
			var identity string
			var stamp sql.NullTime
			if err := rows.Scan(&identity, &stamp); err != nil {
				_ = rows.Close()
				return fault.Wrap(fault.InternalFailure, "scan deadline guard", err)
			}
			if !stamp.Valid || !stamp.Time.After(now) {
				_ = rows.Close()
				return fault.New(fault.DeadlineExpired, "the guarded timestamp is not strictly after the transaction clock")
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// planLocks validates and renders every row lock and absence guard in the
// declared lock order. It performs no I/O.
func planLocks(scope tenancy.Scope, locks []store.Lock) (plannedLocks, error) {
	if err := scope.Validate(); err != nil {
		return plannedLocks{}, err
	}
	planned := make([]plannedLock, 0, len(locks))
	for position, lock := range locks {
		if lock.ExpectAbsent {
			entry, err := planAbsenceLock(lock, position)
			if err != nil {
				return plannedLocks{}, err
			}
			planned = append(planned, entry)
			continue
		}
		meta, ok := lookupTable(lock.Table)
		if !ok {
			return plannedLocks{}, faultInvalid(fmt.Sprintf("unknown table %q", lock.Table))
		}
		if meta.hasProject && meta.projectRequired && scope.TenantScoped() {
			return plannedLocks{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope lock is not allowed", lock.Table))
		}
		if lock.TenantWide {
			switch {
			case !meta.hasProject:
				return plannedLocks{}, faultInvalid(fmt.Sprintf("table %q has no project column; a tenant-wide lock is not possible", lock.Table))
			case meta.projectRequired:
				return plannedLocks{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-wide lock is not allowed", lock.Table))
			}
		}
		if lock.Deadline != nil {
			if err := validateDeadlineGuard(meta, *lock.Deadline); err != nil {
				return plannedLocks{}, err
			}
		}
		ids := append([]string(nil), lock.IDs...)
		for _, id := range ids {
			if !validIdentity(id) {
				return plannedLocks{}, faultInvalid(fmt.Sprintf("lock id %q is not a valid identity", id))
			}
		}
		guarded := lock.ExpectVersion != 0 || len(lock.Expect) > 0
		if guarded || lock.Deadline != nil {
			if len(ids) != 1 {
				return plannedLocks{}, faultInvalid("a guarded lock must name exactly one row")
			}
			if lock.ExpectVersion != 0 && !meta.hasVersion {
				return plannedLocks{}, faultInvalid(fmt.Sprintf("table %q has no version column", lock.Table))
			}
			for name, value := range lock.Expect {
				if err := validateGuardColumn(meta, name, value); err != nil {
					return plannedLocks{}, err
				}
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			statement, err := lockStatement(meta, scope, lock, id)
			if err != nil {
				return plannedLocks{}, err
			}
			var guard *store.Query
			if guarded || lock.Deadline != nil {
				rendered := guardStatement(meta, scope, lock, id, lock.Deadline)
				guard = &rendered
			}
			planned = append(planned, plannedLock{
				rank:      writeRank(lock.Table),
				position:  position,
				table:     lock.Table,
				id:        id,
				statement: statement,
				guard:     guard,
				deadline:  lock.Deadline,
			})
		}
	}
	sortPlannedLocks(planned)
	return plannedLocks{entries: planned}, nil
}

// sortPlannedLocks orders the lock phase by the declared table rank. Two
// absence guards on the same table sort by their canonical unique key
// (declared column order, NULL before any value); every other tie keeps the
// caller's relative order, so an insert advisory never jumps ahead of the guard
// it shares a key with.
func sortPlannedLocks(planned []plannedLock) {
	sort.SliceStable(planned, func(left, right int) bool {
		if planned[left].rank != planned[right].rank {
			return planned[left].rank < planned[right].rank
		}
		if planned[left].absence && planned[right].absence && planned[left].absenceKey != planned[right].absenceKey {
			return planned[left].absenceKey < planned[right].absenceKey
		}
		return planned[left].position < planned[right].position
	})
}

// addInsertAdvisories folds every insert's absence advisory lock into the lock
// phase at the table's write rank. The lock must be acquired in the declared
// lock order like any other lock: taking it only in the write phase would
// acquire a lower-ranked lock after a higher-ranked row lock and could deadlock
// two units that touch the same tables in different phases. Each write remembers
// that the lock phase took its advisory so the write phase does not repeat it.
func addInsertAdvisories(plans plannedLocks, writes []plannedWrite) plannedLocks {
	base := len(plans.entries)
	for index := range writes {
		if writes[index].advisory == nil {
			continue
		}
		plans.entries = append(plans.entries, plannedLock{
			rank:           writes[index].rank,
			position:       base + index,
			table:          writes[index].table,
			insertAdvisory: true,
			advisory:       *writes[index].advisory,
		})
		writes[index].advisoryTaken = true
	}
	sortPlannedLocks(plans.entries)
	return plans
}

// lockStatement renders one parameterized row lock. It selects the identity with
// FOR UPDATE so a concurrent unit blocks until this transaction completes, and
// it filters on the transaction scope so a lock can never reach outside it. The
// tenant and project root rows are bound to the scope's own identity, and
// TenantWide selects a tenant-wide (NULL project) row from a project scope.
func lockStatement(meta *tableMeta, scope tenancy.Scope, lock store.Lock, id string) (store.Statement, error) {
	if err := checkRowIdentityScope(meta, scope, id); err != nil {
		return store.Statement{}, err
	}
	args := make([]any, 0, 3)
	predicates := make([]string, 0, 3)
	add := func(column string, value any) {
		args = append(args, value)
		predicates = append(predicates, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	add("id", id)
	if meta.hasTenant {
		add("tenant_id", string(scope.TenantID))
	}
	if meta.hasProject {
		if lock.TenantWide || scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			add("project_id", string(scope.ProjectID))
		}
	}
	return store.Statement{
		SQL: fmt.Sprintf(
			"SELECT id FROM %s WHERE %s FOR UPDATE",
			meta.table.Name, strings.Join(predicates, " AND "),
		),
		Args: args,
	}, nil
}

// guardStatement renders the read-only guard query for one locked row. It runs
// after every lock statement, re-locks the same row with FOR UPDATE and asserts
// the expected version and column facts. A NULL expectation is rendered as IS
// NULL so a nullable guard column is never silently compared with NULL. When a
// deadline guard is present its timestamp column is selected alongside the id so
// the caller can compare it with the transaction clock.
func guardStatement(meta *tableMeta, scope tenancy.Scope, lock store.Lock, id string, deadline *store.DeadlineGuard) store.Query {
	args := make([]any, 0, len(lock.Expect)+4)
	predicates := make([]string, 0, len(lock.Expect)+4)
	add := func(column string, value any) {
		args = append(args, value)
		predicates = append(predicates, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	add("id", id)
	if meta.hasTenant {
		add("tenant_id", string(scope.TenantID))
	}
	if meta.hasProject {
		if lock.TenantWide || scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			add("project_id", string(scope.ProjectID))
		}
	}
	if lock.ExpectVersion != 0 {
		add("version", lock.ExpectVersion)
	}
	names := make([]string, 0, len(lock.Expect))
	for name := range lock.Expect {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if lock.Expect[name] == nil {
			predicates = append(predicates, fmt.Sprintf("%s IS NULL", name))
			continue
		}
		add(name, lock.Expect[name])
	}
	selectList := "id"
	if deadline != nil {
		selectList += ", " + deadline.Column
	}
	return store.Query{
		SQL: fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s FOR UPDATE",
			selectList, meta.table.Name, strings.Join(predicates, " AND "),
		),
		Args: args,
	}
}
