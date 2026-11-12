// PostgreSQL absence guard.
//
// READ COMMITTED does not lock a gap: SELECT ... FOR UPDATE on zero rows
// protects nothing, so two concurrent first grants could both observe an absent
// membership and both insert. The plan therefore serializes on a
// transaction-scoped advisory lock over a deterministic hash of (table,
// unique-key values). The absence guard and every INSERT into a table with
// registered absence-guardable unique keys take the same lock; the guard then
// verifies that no row matches, and a unique-key violation maps to
// version_conflict. The lock is released by commit or rollback, and the
// declared lock order places it with the table's write rank.
package postgres

import (
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// advisoryLockSQL takes one transaction-scoped PostgreSQL advisory lock on a
// deterministic 64-bit hash of the canonical unique-key identity. Both the
// absence guard and every insert into an absence-guardable table issue exactly
// this parameterized statement, so either path blocks the other until the
// transaction completes.
const advisoryLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

// absenceKeyText renders the canonical identity of one registered unique key:
// the table plus each declared column value in declared order. Every segment is
// length-prefixed, so two different keys can never render the same text, and a
// SQL NULL is a distinct segment from any string. The encoding sorts a NULL
// before every string value, which matches the declared same-table guard order.
func absenceKeyText(table string, values map[string]any) (string, error) {
	columns, registered := store.AbsenceKeyColumns(table)
	if !registered {
		return "", faultInvalid(fmt.Sprintf("table %q registers no absence-guardable unique key", table))
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "t%d:%s;", len(table), table)
	for _, column := range columns {
		value, present := values[column]
		if !present {
			return "", faultInvalid(fmt.Sprintf("absence key for table %q is missing column %q", table, column))
		}
		switch typed := value.(type) {
		case nil:
			builder.WriteString("n;")
		case string:
			fmt.Fprintf(&builder, "s%d:%s;", len(typed), typed)
		default:
			return "", faultInvalid(fmt.Sprintf("absence key column %q of table %q must be a string or null", column, table))
		}
	}
	return builder.String(), nil
}

// absenceInsertKeyText renders the unique-key identity one INSERT creates. The
// adapter supplies the tenant and project columns from the verified transaction
// scope, so an insert and a guard on the same identity take the identical
// advisory lock. A key column the insert omits is rendered as the empty string:
// the registered non-project key columns are NOT NULL, so such an insert can
// never create a unique row, and sharing a lock with an empty-string value only
// over-serializes two statements that cannot both match the guard.
func absenceInsertKeyText(scope tenancy.Scope, table string, values map[string]any) (string, error) {
	columns, registered := store.AbsenceKeyColumns(table)
	if !registered {
		return "", faultInvalid(fmt.Sprintf("table %q registers no absence-guardable unique key", table))
	}
	key := make(map[string]any, len(columns))
	for _, column := range columns {
		switch column {
		case "tenant_id":
			key[column] = string(scope.TenantID)
		case "project_id":
			key[column] = scopeColumnValue(scope)
		default:
			value, present := values[column]
			if !present {
				key[column] = ""
				continue
			}
			key[column] = value
		}
	}
	return absenceKeyText(table, key)
}

// absenceInsertAdvisory returns the advisory-lock statement one INSERT must
// issue before its row, or nil for a table with no registered absence key.
func absenceInsertAdvisory(scope tenancy.Scope, write store.Write) (*store.Statement, error) {
	if _, registered := store.AbsenceKeyColumns(write.Table); !registered {
		return nil, nil
	}
	key, err := absenceInsertKeyText(scope, write.Table, write.Values)
	if err != nil {
		return nil, err
	}
	return &store.Statement{SQL: advisoryLockSQL, Args: []any{key}}, nil
}

// planAbsenceLock validates one absence guard and renders its advisory lock and
// NULL-safe existence check. It performs no I/O and refuses every malformed
// guard Unit.Validate refuses, so LockRows cannot silently ignore a guard that
// ExecuteUnit would validate.
func planAbsenceLock(lock store.Lock, position int) (plannedLock, error) {
	meta, ok := lookupTable(lock.Table)
	if !ok {
		return plannedLock{}, faultInvalid(fmt.Sprintf("unknown table %q", lock.Table))
	}
	if len(lock.IDs) > 0 {
		return plannedLock{}, faultInvalid("an absence guard cannot carry row ids")
	}
	if lock.ExpectVersion != 0 {
		return plannedLock{}, faultInvalid("an absence guard cannot carry an expected version")
	}
	if len(lock.Expect) > 0 {
		return plannedLock{}, faultInvalid("an absence guard cannot carry expected column values")
	}
	if lock.TenantWide {
		return plannedLock{}, faultInvalid("an absence guard cannot carry the tenant-wide flag; its key names project_id explicitly")
	}
	if lock.Deadline != nil {
		return plannedLock{}, faultInvalid("an absence guard cannot carry a deadline guard")
	}
	columns, registered := store.AbsenceKeyColumns(lock.Table)
	if !registered {
		return plannedLock{}, faultInvalid(fmt.Sprintf("table %q registers no absence-guardable unique key", lock.Table))
	}
	if len(lock.Key) != len(columns) {
		return plannedLock{}, faultInvalid(fmt.Sprintf("an absence key for table %q must name exactly the registered unique-key columns %s", lock.Table, strings.Join(columns, ",")))
	}
	key := make(map[string]any, len(columns))
	for _, name := range columns {
		value, present := lock.Key[name]
		if !present {
			return plannedLock{}, faultInvalid(fmt.Sprintf("an absence key for table %q is missing column %q", lock.Table, name))
		}
		column, ok := meta.columns[name]
		if !ok {
			return plannedLock{}, faultInvalid(fmt.Sprintf("absence key column %q is not declared by table %q", name, lock.Table))
		}
		if value == nil {
			if !column.Nullable {
				return plannedLock{}, faultInvalid(fmt.Sprintf("absence key column %q of table %q is not nullable", name, lock.Table))
			}
			key[name] = nil
			continue
		}
		text, ok := value.(string)
		if !ok {
			return plannedLock{}, faultInvalid(fmt.Sprintf("absence key column %q of table %q must be a string or null", name, lock.Table))
		}
		if text == "" {
			return plannedLock{}, faultInvalid(fmt.Sprintf("absence key column %q of table %q is empty", name, lock.Table))
		}
		if column.Type != "text" {
			return plannedLock{}, faultInvalid(fmt.Sprintf("absence key column %q of table %q has unsupported type %q", name, lock.Table, column.Type))
		}
		key[name] = text
	}
	canonical, err := absenceKeyText(lock.Table, key)
	if err != nil {
		return plannedLock{}, err
	}
	args := make([]any, len(columns))
	predicates := make([]string, len(columns))
	for index, name := range columns {
		args[index] = key[name]
		predicates[index] = fmt.Sprintf("%s IS NOT DISTINCT FROM $%d", name, index+1)
	}
	guard := store.Query{
		SQL:  fmt.Sprintf("SELECT 1 FROM %s WHERE %s", lock.Table, strings.Join(predicates, " AND ")),
		Args: args,
	}
	return plannedLock{
		rank:         writeRank(lock.Table),
		position:     position,
		table:        lock.Table,
		absence:      true,
		absenceKey:   canonical,
		advisory:     store.Statement{SQL: advisoryLockSQL, Args: []any{canonical}},
		absenceGuard: &guard,
	}, nil
}

// sqlStateError is the narrow surface a PostgreSQL driver error exposes. The
// adapter keeps no driver dependency: a *pgconn.PgError satisfies this, and a
// seam fake can too.
type sqlStateError interface {
	SQLState() string
}

// mapAbsenceInsertError maps a PostgreSQL unique-key violation (SQLSTATE 23505)
// to the version_conflict class. Every other cause travels unchanged.
func mapAbsenceInsertError(err error) error {
	if err == nil {
		return nil
	}
	var state sqlStateError
	if errors.As(err, &state) && state.SQLState() == "23505" {
		return faultConflict("the registered unique key already exists")
	}
	return err
}
