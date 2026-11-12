package memory

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// reservedWriteColumns are supplied by the adapter from the unit scope and the
// row identity. A caller that passes one is trying to write outside its own
// scope or replace the row identity, so it is refused before any effect.
var reservedWriteColumns = [...]string{"id", "tenant_id", "project_id"}

// plannedLock is one validated row lock. A guarded lock also asserts an expected
// version and/or explicit column facts inside the same unit. An absence lock
// names no row id: it pins the absence of its registered unique-key identity
// and is evaluated against the same staged state as every other lock.
type plannedLock struct {
	meta          *tableMeta
	table         string
	identity      string
	tenantWide    bool
	guarded       bool
	expectVersion uint64
	expect        map[string]any
	deadline      *store.DeadlineGuard
	absence       bool
	keyColumns    []string
	key           map[string]any
	keyText       string
	rank          float64
	position      int
}

// plannedWrite is one validated row write or guarded delete.
type plannedWrite struct {
	meta          *tableMeta
	table         string
	identity      string
	insert        bool
	delete        bool
	expectVersion uint64
	expect        map[string]any
	values        map[string]any
	deadline      *store.DeadlineGuard
	rank          float64
}

// planUnitWrites validates and orders every write by the deterministic write
// rank so a unit applies its tables in the declared order. Equal ranks keep the
// caller's relative order.
func planUnitWrites(scope tenancy.Scope, writes []store.Write) ([]plannedWrite, error) {
	planned := make([]plannedWrite, 0, len(writes))
	for _, write := range writes {
		rendered, err := planWrite(scope, write)
		if err != nil {
			return nil, err
		}
		planned = append(planned, rendered)
	}
	sort.SliceStable(planned, func(left, right int) bool {
		return planned[left].rank < planned[right].rank
	})
	return planned, nil
}

// planWrite validates one write before any effect. It refuses a reserved,
// unknown, null-violating or version-replacing column, an unguarded update or
// delete, and a root row outside the caller's scope.
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
	if err := checkRowIdentityScope(meta, scope, write.ID); err != nil {
		return plannedWrite{}, err
	}
	if meta.projectRequired && scope.TenantScoped() {
		return plannedWrite{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope write is not allowed", write.Table))
	}
	// The receipt/audit publication rule, the events acknowledgement rule and
	// the version/guard shape live in the neutral seam and run at the same
	// point in both adapters, after the scope and identity checks, so a write
	// that breaks two rules is refused with the same class by either.
	if err := store.ValidateWrite(write); err != nil {
		return plannedWrite{}, err
	}

	values := make(map[string]any, len(write.Values))
	for name, value := range write.Values {
		normalized, err := validateWriteColumn(meta, write, name, value)
		if err != nil {
			return plannedWrite{}, err
		}
		values[name] = normalized
	}

	if write.Insert && len(write.Expect) > 0 {
		return plannedWrite{}, faultInvalid("an insert cannot carry expected column values")
	}
	expect := make(map[string]any, len(write.Expect))
	for name, value := range write.Expect {
		normalized, err := validateGuardColumn(meta, name, value)
		if err != nil {
			return plannedWrite{}, err
		}
		expect[name] = normalized
	}
	if !write.Insert && !write.Delete && len(write.Values) == 0 {
		return plannedWrite{}, faultInvalid("an update must set at least one column")
	}
	var deadline *store.DeadlineGuard
	if write.Deadline != nil {
		if write.Insert {
			return plannedWrite{}, faultInvalid("an insert cannot carry a deadline guard")
		}
		if err := validateDeadlineColumn(meta, *write.Deadline); err != nil {
			return plannedWrite{}, err
		}
		guard := *write.Deadline
		deadline = &guard
	}

	return plannedWrite{
		meta:          meta,
		table:         write.Table,
		identity:      write.ID,
		insert:        write.Insert,
		delete:        write.Delete,
		expectVersion: write.ExpectVersion,
		expect:        expect,
		values:        values,
		deadline:      deadline,
		rank:          writeRank(write.Table),
	}, nil
}

// validateDeadlineColumn proves a deadline guard names a declared timestamp
// column of the target table before any row is inspected.
func validateDeadlineColumn(meta *tableMeta, guard store.DeadlineGuard) error {
	if err := guard.Validate(); err != nil {
		return err
	}
	column, ok := meta.columns[guard.Column]
	if !ok || column.Type != "timestamptz" {
		return faultInvalid(fmt.Sprintf("deadline guard column %q is not a declared timestamp column of %q", guard.Column, meta.table.Name))
	}
	return nil
}

// validateWriteColumn refuses a reserved, unknown, null-violating or
// version-replacing value column and returns the value normalized to the shape
// store.Row promises for that column type. A caller cannot store an int in a
// text column or a string in a bigint column and get it back unchanged.
func validateWriteColumn(meta *tableMeta, write store.Write, name string, value any) (any, error) {
	for _, reserved := range reservedWriteColumns {
		if name == reserved {
			return nil, faultInvalid(fmt.Sprintf("column %q is supplied by the adapter, not the caller", name))
		}
	}
	column, ok := meta.columns[name]
	if !ok {
		return nil, faultInvalid(fmt.Sprintf("table %q has no column %q", meta.table.Name, name))
	}
	if !write.Insert && name == "version" {
		return nil, faultInvalid("the adapter advances the version column; an update cannot replace it")
	}
	return normalizeColumnValue(column, value)
}

// validateGuardColumn refuses an unknown, reserved or version-duplicating
// optimistic guard and normalizes the expected value to the column's contract
// type. Write expectations and read-only lock guards share it so both assert the
// same row shape and compare like with like.
func validateGuardColumn(meta *tableMeta, name string, value any) (any, error) {
	for _, reserved := range reservedWriteColumns {
		if name == reserved {
			return nil, faultInvalid(fmt.Sprintf("column %q is supplied by the adapter, not the caller", name))
		}
	}
	column, ok := meta.columns[name]
	if !ok {
		return nil, faultInvalid(fmt.Sprintf("table %q has no column %q", meta.table.Name, name))
	}
	if meta.hasVersion && name == "version" {
		return nil, faultInvalid("use ExpectVersion for the version column")
	}
	return normalizeColumnValue(column, value)
}

// normalizeColumnValue validates one column value against the declared column
// type and returns the adapter-decoded shape store.Row promises: bigint and
// integer as int64, text as string, bytea/jsonb as []byte, boolean as bool,
// timestamptz as time.Time and NULL as nil. A mismatch is invalid_input before
// any effect, matching the PostgreSQL reference's type enforcement.
func normalizeColumnValue(column store.Column, value any) (any, error) {
	if value == nil {
		if !column.Nullable {
			return nil, faultInvalid(fmt.Sprintf("column %q is not nullable", column.Name))
		}
		return nil, nil
	}
	switch column.Type {
	case "text":
		text, ok := value.(string)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects text; got %T", column.Name, value))
		}
		return text, nil
	case "bigint", "integer":
		switch typed := value.(type) {
		case int:
			return int64(typed), nil
		case int8:
			return int64(typed), nil
		case int16:
			return int64(typed), nil
		case int32:
			return int64(typed), nil
		case int64:
			return typed, nil
		case uint:
			if uint64(typed) > math.MaxInt64 {
				return nil, faultInvalid(fmt.Sprintf("column %q value overflows int64", column.Name))
			}
			return int64(typed), nil
		case uint8:
			return int64(typed), nil
		case uint16:
			return int64(typed), nil
		case uint32:
			return int64(typed), nil
		case uint64:
			if typed > math.MaxInt64 {
				return nil, faultInvalid(fmt.Sprintf("column %q value overflows int64", column.Name))
			}
			return int64(typed), nil
		default:
			return nil, faultInvalid(fmt.Sprintf("column %q expects an integer; got %T", column.Name, value))
		}
	case "bytea", "jsonb":
		bytes, ok := value.([]byte)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects []byte; got %T", column.Name, value))
		}
		return bytes, nil
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects bool; got %T", column.Name, value))
		}
		return boolean, nil
	case "timestamptz":
		moment, ok := value.(time.Time)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects time.Time; got %T", column.Name, value))
		}
		return moment, nil
	default:
		return nil, faultInternal(fmt.Sprintf("column %q has unsupported type %q", column.Name, column.Type))
	}
}

// checkRowIdentityScope ties the tenant and project root rows to the unit scope.
// Their row identity is the tenant or project identity, so a row for another
// tenant (or, for a project-scoped unit, another project) is out of scope even
// when the tenant matches.
func checkRowIdentityScope(meta *tableMeta, scope tenancy.Scope, identity string) error {
	switch meta.table.Name {
	case "tenants":
		if identity != string(scope.TenantID) {
			return faultForbidden("tenant row belongs to another tenant")
		}
	case "projects":
		if scope.ProjectScoped() && identity != string(scope.ProjectID) {
			return faultForbidden("project row belongs to another project")
		}
	}
	return nil
}

// planLocks validates and orders every row lock in the declared lock order. A
// guarded lock names exactly one row and runs its guard after every lock
// statement, before any write. An absence guard names its registered unique key
// and shares the table's write rank; same-table guards sort by canonical key.
func planLocks(scope tenancy.Scope, locks []store.Lock) ([]plannedLock, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	plans := make([]plannedLock, 0, len(locks))
	for position, lock := range locks {
		meta, ok := lookupTable(lock.Table)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("unknown table %q", lock.Table))
		}
		if meta.hasProject && meta.projectRequired && scope.TenantScoped() {
			return nil, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope lock is not allowed", lock.Table))
		}
		// An absence guard names no row id: it plans one keyed entry that is
		// checked against the committed state inside the same atomic section.
		// Unit.Validate has already refused every malformed guard.
		if lock.ExpectAbsent {
			columns, registered := store.AbsenceKeyColumns(lock.Table)
			if !registered {
				return nil, faultInvalid(fmt.Sprintf("table %q registers no absence-guardable unique key", lock.Table))
			}
			key := make(map[string]any, len(columns))
			for _, name := range columns {
				value, present := lock.Key[name]
				if !present {
					return nil, faultInvalid(fmt.Sprintf("an absence key for table %q is missing column %q", lock.Table, name))
				}
				key[name] = value
			}
			plans = append(plans, plannedLock{
				meta:       meta,
				table:      lock.Table,
				absence:    true,
				keyColumns: columns,
				key:        key,
				keyText:    absenceKeyText(lock.Table, columns, key),
				rank:       writeRank(lock.Table),
				position:   position,
			})
			continue
		}
		if lock.TenantWide {
			switch {
			case !meta.hasProject:
				return nil, faultInvalid(fmt.Sprintf("table %q has no project column; a tenant-wide lock is not possible", lock.Table))
			case meta.projectRequired:
				return nil, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-wide lock is not allowed", lock.Table))
			}
		}
		identities := append([]string(nil), lock.IDs...)
		for _, identity := range identities {
			if !validIdentity(identity) {
				return nil, faultInvalid(fmt.Sprintf("lock id %q is not a valid identity", identity))
			}
		}
		guarded := lock.ExpectVersion != 0 || len(lock.Expect) > 0
		expect := make(map[string]any, len(lock.Expect))
		if guarded {
			if len(identities) != 1 {
				return nil, faultInvalid("a guarded lock must name exactly one row")
			}
			if lock.ExpectVersion != 0 && !meta.hasVersion {
				return nil, faultInvalid(fmt.Sprintf("table %q has no version column", lock.Table))
			}
			for name, value := range lock.Expect {
				normalized, err := validateGuardColumn(meta, name, value)
				if err != nil {
					return nil, err
				}
				expect[name] = normalized
			}
		}
		var deadline *store.DeadlineGuard
		if lock.Deadline != nil {
			if err := validateDeadlineColumn(meta, *lock.Deadline); err != nil {
				return nil, err
			}
			guard := *lock.Deadline
			deadline = &guard
		}
		sort.Strings(identities)
		for _, identity := range identities {
			if err := checkRowIdentityScope(meta, scope, identity); err != nil {
				return nil, err
			}
			plans = append(plans, plannedLock{
				meta:          meta,
				table:         lock.Table,
				identity:      identity,
				tenantWide:    lock.TenantWide,
				guarded:       guarded,
				expectVersion: lock.ExpectVersion,
				expect:        expect,
				deadline:      deadline,
				rank:          writeRank(lock.Table),
				position:      position,
			})
		}
	}
	sort.SliceStable(plans, func(left, right int) bool {
		if plans[left].rank != plans[right].rank {
			return plans[left].rank < plans[right].rank
		}
		// Two absence guards on the same table sort by their canonical unique
		// key; a guard shares its table's write rank with a present-row lock.
		if plans[left].absence && plans[right].absence && plans[left].keyText != plans[right].keyText {
			return plans[left].keyText < plans[right].keyText
		}
		return plans[left].position < plans[right].position
	})
	return plans, nil
}

// absenceKeyText renders the canonical unique-key identity of one absence guard:
// the table name and its declared key columns joined by a NUL byte. The NUL
// separator sorts below every value byte and a NULL project renders as the empty
// part, so a tenant-wide key sorts before any project and two same-table guards
// order deterministically.
func absenceKeyText(table string, columns []string, key map[string]any) string {
	parts := make([]string, 0, len(columns)+1)
	parts = append(parts, table)
	for _, name := range columns {
		if key[name] == nil {
			parts = append(parts, "")
			continue
		}
		parts = append(parts, fmt.Sprintf("%v", key[name]))
	}
	return strings.Join(parts, "\x00")
}

// validateAudit refuses an audit row outside the unit scope or with an unusable
// identity, actor, operation or time.
func validateAudit(scope tenancy.Scope, audit event.Audit) error {
	if audit.Scope != scope {
		return faultForbidden("audit record scope does not match the commit unit scope")
	}
	if _, err := id.ParseEventID(string(audit.ID)); err != nil {
		return faultInvalid("audit record id is invalid")
	}
	if audit.Actor == "" {
		return faultInvalid("audit record actor is empty")
	}
	if audit.Operation == "" {
		return faultInvalid("audit record operation is empty")
	}
	if audit.At.IsZero() {
		return faultInvalid("audit record time is zero")
	}
	return nil
}

// validateEvent refuses an event envelope outside the unit scope or with an
// unusable type, source, time or payload. The envelope rules themselves are the
// event contract's.
func validateEvent(scope tenancy.Scope, envelope event.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if envelope.Scope != scope {
		return faultForbidden("event scope does not match the commit unit scope")
	}
	return nil
}

// validateReceipt refuses a receipt whose identity, content hash or scope cannot
// protect the unit. Receipt identity is (tenant, project, principal, operation,
// caller key), and the project must be exactly the unit scope's project.
func validateReceipt(scope tenancy.Scope, record receipt.Record) error {
	if err := record.Key.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseReceiptID(string(record.ID)); err != nil {
		return faultInvalid("receipt id is invalid")
	}
	if _, err := contentHashBytes(record.ContentHash); err != nil {
		return faultInvalid("receipt content hash is not a sha256 fingerprint")
	}
	if record.At.IsZero() {
		return faultInvalid("receipt time is zero")
	}
	if record.Key.TenantID != scope.TenantID || record.Key.ProjectID != scope.ProjectID {
		return faultForbidden("receipt identity does not match the commit unit scope")
	}
	return nil
}

// contentHashBytes decodes a receipt content fingerprint into its byte form.
func contentHashBytes(value string) ([]byte, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return nil, fmt.Errorf("receipt content hash has no sha256 prefix")
	}
	decoded, err := hex.DecodeString(value[len(prefix):])
	if err != nil {
		return nil, err
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("receipt content hash is %d bytes; want 32", len(decoded))
	}
	return decoded, nil
}

// sameContentHash compares two receipt fingerprints byte-exactly, falling back
// to text equality only if a stored value is not a well-formed fingerprint.
func sameContentHash(left, right string) bool {
	leftBytes, leftErr := contentHashBytes(left)
	rightBytes, rightErr := contentHashBytes(right)
	if leftErr != nil || rightErr != nil {
		return left == right
	}
	return bytes.Equal(leftBytes, rightBytes)
}
