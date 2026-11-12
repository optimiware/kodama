package memory

import (
	"bytes"
	"maps"
	"time"

	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// state is the one committed state a Store owns. Business rows, durable
// receipts, committed event rows and audit rows all live here behind the
// Store's single lock, so no capability can observe or publish part of a unit.
type state struct {
	rows     map[string]map[string]map[string]any
	receipts map[receipt.Key]receipt.Record
	// receiptCompleted is the durable completion marker (the receipts.completed_at
	// column) for each receipt. An incomplete receipt has no marker until a
	// completion-mode unit compare-and-sets it.
	receiptCompleted map[receipt.Key]bool
	events           []CommittedEvent
	audits           []event.Audit
	sequence         uint64
}

func newState() *state {
	return &state{
		rows:             make(map[string]map[string]map[string]any),
		receipts:         make(map[receipt.Key]receipt.Record),
		receiptCompleted: make(map[receipt.Key]bool),
	}
}

// clone returns a deep copy of the committed state. A unit is staged against the
// clone and the clone is published only when the whole unit succeeds, so a
// refused or failed unit can never leave a partial effect behind.
func (committed *state) clone() *state {
	next := newState()
	next.sequence = committed.sequence
	for table, rows := range committed.rows {
		copied := make(map[string]map[string]any, len(rows))
		for identity, values := range rows {
			copied[identity] = copyValues(values)
		}
		next.rows[table] = copied
	}
	for key, record := range committed.receipts {
		next.receipts[key] = copyReceipt(record)
	}
	maps.Copy(next.receiptCompleted, committed.receiptCompleted)
	next.events = make([]CommittedEvent, len(committed.events))
	for position, entry := range committed.events {
		next.events[position] = CommittedEvent{
			Sequence:  entry.Sequence,
			Envelope:  copyEnvelope(entry.Envelope),
			Published: entry.Published,
		}
	}
	next.audits = append([]event.Audit(nil), committed.audits...)
	return next
}

// row returns the stored values for one table identity.
func (committed *state) row(table, identity string) (map[string]any, bool) {
	rows, ok := committed.rows[table]
	if !ok {
		return nil, false
	}
	values, ok := rows[identity]
	return values, ok
}

// hasRow reports whether one table identity exists in any scope.
func (committed *state) hasRow(table, identity string) bool {
	_, ok := committed.row(table, identity)
	return ok
}

// visible returns the stored row when it exists and belongs to the caller's
// ownership scope. tenantWide selects a tenant-wide (project_id IS NULL) row
// from a project-scoped caller, matching the declared lock and guard semantics.
func (committed *state) visible(meta *tableMeta, scope tenancy.Scope, table, identity string, tenantWide bool) (map[string]any, bool) {
	values, ok := committed.row(table, identity)
	if !ok {
		return nil, false
	}
	if !rowVisible(meta, scope, identity, values, tenantWide) {
		return nil, false
	}
	return values, true
}

// rowVisible applies the adapter's scope predicates to one stored row. The
// tenant and project root rows carry their own identity, so their row id must
// stay inside the caller's scope as well.
func rowVisible(meta *tableMeta, scope tenancy.Scope, identity string, values map[string]any, tenantWide bool) bool {
	if meta.hasTenant {
		tenant, _ := values["tenant_id"].(string)
		if tenant != string(scope.TenantID) {
			return false
		}
	}
	if meta.hasProject {
		project, _ := values["project_id"].(string)
		if tenantWide || scope.TenantScoped() {
			return project == ""
		}
		return project == string(scope.ProjectID)
	}
	switch meta.table.Name {
	case "tenants":
		return identity == string(scope.TenantID)
	case "projects":
		if scope.ProjectScoped() {
			return identity == string(scope.ProjectID)
		}
	}
	return true
}

// scopeVisible reports whether a committed event or audit row is visible in the
// caller's scope: a tenant scope sees only tenant-wide (project_id IS NULL) rows
// and a project scope sees only its own project.
func scopeVisible(scope, row tenancy.Scope) bool {
	if row.TenantID != scope.TenantID {
		return false
	}
	if scope.TenantScoped() {
		return row.ProjectID == ""
	}
	return row.ProjectID == scope.ProjectID
}

// materializeValues returns a defensive copy carrying every schema column in
// column order. A column with no stored value reads as SQL NULL (nil), matching
// the PostgreSQL adapter's full SELECT list and the store.Row contract.
func materializeValues(meta *tableMeta, values map[string]any) map[string]any {
	copied := make(map[string]any, len(meta.columnOrder))
	for _, name := range meta.columnOrder {
		copied[name] = copyValue(values[name])
	}
	return copied
}

// copyValues returns a deep copy of a column value map.
func copyValues(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for name, value := range values {
		copied[name] = copyValue(value)
	}
	return copied
}

// copyValue copies the mutable values a row can retain: byte slices, nested
// maps and slices, and the receipt snapshot digest pointer.
func copyValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return copyBytes(typed)
	case map[string]any:
		return copyValues(typed)
	case []any:
		copied := make([]any, len(typed))
		for position, item := range typed {
			copied[position] = copyValue(item)
		}
		return copied
	case *[32]byte:
		if typed == nil {
			return (*[32]byte)(nil)
		}
		digest := *typed
		return &digest
	default:
		return value
	}
}

// copyBytes copies a retained byte slice, preserving nil.
func copyBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

// copyReceipt copies a durable receipt, including its result body and snapshot
// digest, so neither caller input nor inspection results alias stored state.
func copyReceipt(record receipt.Record) receipt.Record {
	copied := record
	copied.Result = copyBytes(record.Result)
	if record.SnapshotHash != nil {
		digest := *record.SnapshotHash
		copied.SnapshotHash = &digest
	}
	return copied
}

// copyEnvelope copies an event envelope and its retained payload bytes.
func copyEnvelope(envelope event.Envelope) event.Envelope {
	copied := envelope
	copied.Data = copyBytes(envelope.Data)
	return copied
}

// asUint64 converts a stored optimistic version to uint64. The adapter accepts
// the integer shapes a caller or a vector can supply.
func asUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case uint64:
		return typed, true
	case uint32:
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int32:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	default:
		return 0, false
	}
}

// valuesEqual compares one stored column with an expected guard value. Numeric
// version columns are compared across integer widths; retained bytes compare
// byte-exactly.
func valuesEqual(stored, expected any) bool {
	if stored == nil || expected == nil {
		return stored == nil && expected == nil
	}
	if left, ok := asUint64(stored); ok {
		if right, ok := asUint64(expected); ok {
			return left == right
		}
	}
	switch typed := stored.(type) {
	case []byte:
		other, ok := expected.([]byte)
		return ok && bytes.Equal(typed, other)
	case time.Time:
		other, ok := expected.(time.Time)
		return ok && typed.Equal(other)
	case string:
		other, ok := expected.(string)
		return ok && typed == other
	case bool:
		other, ok := expected.(bool)
		return ok && typed == other
	default:
		return false
	}
}
