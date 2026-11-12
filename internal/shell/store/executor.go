// Structured executor seam: the adapter-independent commit unit every
// persistence adapter (PostgreSQL and the memory adapter) executes.
//
// A Unit is one complete atomic commit unit. The seam deliberately carries no
// SQL: an adapter decides how to lock, read and write, while the caller still
// cannot commit a business change apart from its receipt, audit rows and event
// publication intent. SQL text stays inside a concrete adapter.
package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Executor is the neutral persistence seam. Execute applies one complete commit
// unit inside exactly one adapter transaction and Read returns one scoped row.
//
// Execute validates the whole unit before any statement, replays an equal
// receipt instead of re-applying it, refuses changed receipt content, acquires
// locks in the declared lock order, and commits business rows, receipt, audit
// and event publication intent together. Read establishes one transaction-local
// scope, reads by identity inside that scope and completes the read-only
// transaction so a pooled connection cannot leak the scope.
//
// There is intentionally no generic Save/Update/SetState/Persist/Store/
// Repository method.
type Executor interface {
	Execute(ctx context.Context, scope tenancy.Scope, unit Unit) (UnitResult, error)
	Read(ctx context.Context, scope tenancy.Scope, table, id string) (Row, error)
}

// Unit is one complete atomic commit unit: the rows it locks, the business rows
// it writes or deletes, its durable receipt, and the audit rows and event
// publication intent committed with it. A unit never contains an external
// effect; effects follow the commit.
type Unit struct {
	Locks   []Lock
	Writes  []Write
	Receipt *receipt.Record
	// ReceiptMode selects insert-or-replay (the zero value), insertion of an
	// incomplete receipt, or completion of a previously inserted receipt. A
	// completion-mode unit is how a trigger Start advances the incomplete
	// receipt its Select already inserted; a plain insert-mode receipt is
	// completed at insert.
	ReceiptMode ReceiptMode
	// ReceiptExpect is the required prior receipt state for ReceiptComplete. It
	// is refused on an insert-mode unit and required on a completion-mode unit.
	ReceiptExpect *ReceiptExpect
	Audit         []event.Audit
	Events        []event.Envelope
}

// UnitResult is the committed outcome. Replayed reports that an equal receipt
// already existed, in which case the original durable result is returned and no
// business write, audit row or event row was applied again.
type UnitResult struct {
	Replayed bool
	Receipt  receipt.Record
}

// Validate checks the receipt-mode shape of one commit unit before an adapter
// starts a transaction: the mode is declared, an insert-mode unit carries no
// completion expectation, and a completion unit carries both a receipt and a
// valid expectation. A receipt completed at insert must carry a result payload;
// an incomplete insert MAY carry an in-progress result (the trigger Select's
// persisted selection) because it is not a completion.
func (unit Unit) Validate() error {
	if !unit.ReceiptMode.Valid() {
		return fault.New(fault.InvalidInput, "receipt mode is not frozen")
	}
	switch unit.ReceiptMode {
	case ReceiptInsert:
		if unit.ReceiptExpect != nil {
			return fault.New(fault.InvalidInput, "an insert-mode unit cannot carry a receipt expectation")
		}
		if unit.Receipt != nil && unit.Receipt.Result == nil {
			return fault.New(fault.InvalidInput, "a completed receipt insert requires a result")
		}
	case ReceiptInsertIncomplete:
		if unit.ReceiptExpect != nil {
			return fault.New(fault.InvalidInput, "an insert-mode unit cannot carry a receipt expectation")
		}
		// An incomplete insert is still an insert: the row it starts must exist.
		// Without a receipt the mode names state the unit never supplies, so the
		// adapter would commit nothing while reporting an insert.
		if unit.Receipt == nil {
			return fault.New(fault.InvalidInput, "an incomplete receipt insert requires a receipt")
		}
	case ReceiptComplete:
		if unit.Receipt == nil || unit.ReceiptExpect == nil {
			return fault.New(fault.InvalidInput, "a completion-mode unit requires a receipt and its expectation")
		}
		if err := unit.ReceiptExpect.Validate(); err != nil {
			return err
		}
	}
	for _, lock := range unit.Locks {
		if err := lock.validateAbsence(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWrite enforces the write-shape rules every store adapter shares
// before it plans a write, so a rule can never live in one adapter and be
// missing from the other:
//
//   - the receipt and audit rows are commit-unit publication records, never
//     business rows, so a Write to one is refused before any effect;
//   - the events table carries committed publication intent: only the
//     publisher's acknowledgement moves publish_state, and only under an
//     explicit expected-column guard;
//   - a delete or update of a versioned row carries its expected version, a
//     table without a version column carries an explicit column guard instead,
//     and an insert carries no expected version;
//   - the tasks table has no single version column (content, lifecycle and
//     question carry their own), so an update must carry an explicit guard.
//
// Column types, reserved columns and scope identity stay adapter-local because
// they depend on each adapter's own column metadata.
func ValidateWrite(write Write) error {
	if IsPublicationTable(write.Table) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("table %q is written through its publication record, not a business write", write.Table))
	}
	hasVersion, ok := tableVersioned()[write.Table]
	if !ok {
		// The adapter that owns the table metadata reports the unknown table.
		return nil
	}
	if write.Insert && write.Delete {
		return fault.New(fault.InvalidInput, "a write cannot be both an insert and a delete")
	}
	if write.Delete {
		if len(write.Values) > 0 {
			return fault.New(fault.InvalidInput, "a delete cannot carry value columns")
		}
		switch {
		case hasVersion && write.ExpectVersion == 0:
			return fault.New(fault.InvalidInput, fmt.Sprintf("a delete from %q must carry its expected version", write.Table))
		case !hasVersion && write.ExpectVersion != 0:
			return fault.New(fault.InvalidInput, fmt.Sprintf("table %q has no version column", write.Table))
		case !hasVersion && len(write.Expect) == 0:
			return fault.New(fault.InvalidInput, fmt.Sprintf("a delete from %q must carry an explicit expected-column guard", write.Table))
		}
	}
	if write.Insert && write.ExpectVersion != 0 {
		return fault.New(fault.InvalidInput, "an insert cannot carry an expected version")
	}
	if !write.Insert && !write.Delete {
		switch {
		case hasVersion && write.ExpectVersion == 0:
			return fault.New(fault.InvalidInput, fmt.Sprintf("an update to %q must carry its expected version", write.Table))
		case !hasVersion && write.ExpectVersion != 0:
			return fault.New(fault.InvalidInput, fmt.Sprintf("table %q has no version column", write.Table))
		}
		if write.Table == "tasks" && len(write.Expect) == 0 {
			return fault.New(fault.InvalidInput, "a task update must carry an explicit expected-column guard")
		}
	}
	if write.Table == "events" {
		return validateEventPublishWrite(write)
	}
	return nil
}

// tableVersioned indexes every declared table by whether it carries the single
// optimistic version column. ValidateWrite runs once per write, so the schema
// is walked once instead of being rebuilt for each one.
var tableVersioned = sync.OnceValue(func() map[string]bool {
	tables := SchemaDesign().Tables
	versioned := make(map[string]bool, len(tables))
	for _, table := range tables {
		versioned[table.Name] = tableHasColumn(table, "version")
	}
	return versioned
})

// publicationTables are written through their publication records (Unit.Audit
// and Unit.Receipt), never as generic business rows. A business Write to one
// would land in a shadow map the receipt replay index and the committed audit
// rows never consult, so it is refused before any effect. The events table is
// deliberately absent: a publisher acknowledgement is the explicit
// publish_state write validateEventPublishWrite allows.
var publicationTables = [...]string{"receipts", "audit"}

// IsPublicationTable reports whether a table is published only through its
// commit-unit record rather than a business write.
func IsPublicationTable(name string) bool {
	return slices.Contains(publicationTables[:], name)
}

// validateEventPublishWrite restricts a generic events write to the publisher's
// acknowledgement. The event row itself is committed publication intent and may
// not be inserted, deleted or rewritten; only publish_state moves, and only to
// one of the two declared values.
func validateEventPublishWrite(write Write) error {
	if write.Insert || write.Delete {
		return fault.New(fault.InvalidInput, "events rows are committed as publication intent, not inserted or deleted as business rows")
	}
	if write.Deadline != nil {
		return fault.New(fault.InvalidInput, "an events publication write cannot carry a deadline guard")
	}
	if len(write.Values) != 1 || len(write.Expect) == 0 {
		return fault.New(fault.InvalidInput, "an events write may only update publish_state under an explicit expected-column guard")
	}
	published, ok := write.Values["publish_state"].(string)
	if !ok || (published != "pending" && published != "published") {
		return fault.New(fault.InvalidInput, fmt.Sprintf("publish_state %v is not a frozen value", write.Values["publish_state"]))
	}
	return nil
}

// validateAbsence checks the shared shape of an absence guard before an adapter
// starts a transaction. A malformed guard is invalid_input, never a present-row
// wildcard or a silently ignored field.
func (lock Lock) validateAbsence() error {
	if !lock.ExpectAbsent {
		if lock.Key != nil {
			return fault.New(fault.InvalidInput, "a lock key is only meaningful with an absence guard")
		}
		return nil
	}
	if len(lock.IDs) > 0 {
		return fault.New(fault.InvalidInput, "an absence guard cannot carry row ids")
	}
	if lock.ExpectVersion != 0 {
		return fault.New(fault.InvalidInput, "an absence guard cannot carry an expected version")
	}
	if len(lock.Expect) > 0 {
		return fault.New(fault.InvalidInput, "an absence guard cannot carry expected column values")
	}
	if lock.TenantWide {
		return fault.New(fault.InvalidInput, "an absence guard cannot carry the tenant-wide flag; its key names project_id explicitly")
	}
	if lock.Deadline != nil {
		return fault.New(fault.InvalidInput, "an absence guard cannot carry a deadline guard")
	}
	if len(lock.Key) == 0 {
		return fault.New(fault.InvalidInput, "an absence guard requires a unique-key identity")
	}
	columns, registered := AbsenceKeyColumns(lock.Table)
	if !registered {
		return fault.New(fault.InvalidInput, fmt.Sprintf("table %q registers no absence-guardable unique key", lock.Table))
	}
	if len(lock.Key) != len(columns) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("an absence key for table %q must name exactly the registered unique-key columns %s", lock.Table, strings.Join(columns, ",")))
	}
	table, ok := schemaTable(lock.Table)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("absence guard table %q is not in the frozen schema", lock.Table))
	}
	for _, name := range columns {
		value, present := lock.Key[name]
		if !present {
			return fault.New(fault.InvalidInput, fmt.Sprintf("an absence key for table %q is missing column %q", lock.Table, name))
		}
		column, ok := tableColumn(table, name)
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("absence key column %q is not declared by table %q", name, lock.Table))
		}
		if value == nil {
			if !column.Nullable {
				return fault.New(fault.InvalidInput, fmt.Sprintf("absence key column %q of table %q is not nullable", name, lock.Table))
			}
			continue
		}
		if column.Type != "text" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("absence key column %q of table %q has unsupported type %q", name, lock.Table, column.Type))
		}
		text, ok := value.(string)
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("absence key column %q of table %q must be a string or null", name, lock.Table))
		}
		if text == "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("absence key column %q of table %q is empty", name, lock.Table))
		}
	}
	return nil
}

// Lock names the rows one commit unit must serialize before it writes. Locks are
// acquired in the schema lock order regardless of the order the caller
// lists them, and the row ids inside one lock are sorted so two units that touch
// the same rows cannot deadlock by taking them in opposite order.
//
// A lock may also be a read-only guard. ExpectVersion and Expect assert facts
// about the locked row inside the same transaction, so a membership, service,
// binding or dependency row that was revoked or changed since the caller read it
// cannot authorize the commit. A guarded lock names exactly one row; its guard
// query runs after every lock statement and before any write, and a missing row
// or a mismatched version/column is a version_conflict. A lock with neither
// field only serializes the row and still fails when the row is absent.
//
// TenantWide targets a tenant-wide row (project_id IS NULL) from a project-scoped
// unit, so a project operation can guard tenant-wide authority without widening
// its own scope.
//
// ExpectAbsent is the absence guard. It pins a row that must NOT exist by
// its registered UNIQUE-KEY identity (Key) rather than by a row id, because an
// absent row has no id. When the unit commits, a matching row is a
// version_conflict and nothing is persisted. ExpectAbsent is exclusive: it
// cannot be combined with IDs, ExpectVersion, Expect, TenantWide or Deadline,
// and Key is only meaningful with ExpectAbsent. Unit.Validate refuses every
// malformed combination with invalid_input before an adapter starts a
// transaction. The registered key columns are returned by AbsenceKeyColumns.
type Lock struct {
	Table         string
	IDs           []string
	ExpectVersion uint64
	Expect        map[string]any
	TenantWide    bool
	// Deadline asserts that the named timestamp column of the locked row is
	// strictly after the transaction clock. A nil guard performs no deadline
	// check; a failed guard is deadline_expired.
	Deadline *DeadlineGuard
	// ExpectAbsent pins the absence of the row named by Key.
	ExpectAbsent bool `json:"expect_absent,omitempty"`
	// Key is the registered unique-key identity of an absence guard, in
	// AbsenceKeyColumns order. A nullable column such as a tenant-wide
	// project_id is carried as an explicit nil, never omitted.
	Key map[string]any `json:"key,omitempty"`
}

// HasAbsenceGuard reports whether the unit carries at least one absence guard.
func (unit Unit) HasAbsenceGuard() bool {
	for _, lock := range unit.Locks {
		if lock.ExpectAbsent {
			return true
		}
	}
	return false
}

// Write is one row write or guarded delete inside a commit unit.
//
// Insert writes a fresh row: the adapter supplies the identity and the scope
// columns, and Values carries every other column including the initial version.
// An update compares the row's optimistic version (ExpectVersion) and advances
// it by one; a stale expectation is refused with version_conflict without a
// partial change. Expect adds further explicit equality guards for tables whose
// optimistic version is a named column (for example a task's lifecycle version)
// rather than the single version column; it never replaces ExpectVersion on a
// versioned table.
//
// Delete is an explicit guarded row removal. It forbids Insert and Values and
// requires a version guard on a versioned table (ExpectVersion) or an explicit
// column guard on a table with no version column (Expect), so an unguarded
// delete can never remove a row a concurrent change or a wrong scope would have
// protected. A delete that matches no row is a version_conflict, exactly like a
// stale update.
type Write struct {
	Table  string
	ID     string
	Insert bool
	// Delete removes the identified row instead of inserting or updating it.
	// It requires a version or explicit column guard; see the Write doc.
	Delete        bool
	ExpectVersion uint64
	Expect        map[string]any
	Values        map[string]any
	// Deadline asserts that the named timestamp column of the existing row is
	// strictly after the transaction clock before the write applies. A nil guard
	// performs no deadline check; a failed guard is deadline_expired.
	Deadline *DeadlineGuard
}

// Row is one scoped row read through the seam. Values holds the adapter-decoded
// column values: text as string, integers as int64, bytea/jsonb as []byte,
// booleans as bool, timestamptz as time.Time and SQL NULL as nil. Retained bytes
// (pipeline YAML, launch snapshots, outputs, stop evidence) therefore round-trip
// exactly.
type Row struct {
	Table  string
	Values map[string]any
}
