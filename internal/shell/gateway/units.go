package gateway

// Schema-conforming commit units for the two optional-model-gateway rows.
//
// UsageRecord.Unit is the model_usage builder the gateway commits: it writes an
// absent ExecutionID as SQL NULL into the nullable execution_id column.
// ContentRecord.Unit is the matching content_records builder;
// StoreContentRecorder commits it through the neutral store.Executor. Both
// units are single-row inserts with no receipt, lock, audit row or event:
// model.invoke has receipt policy none, and raw capture is a retention-only
// side record.

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
)

const (
	// ContentTable is the opt-in raw-content capture table.
	ContentTable = "content_records"
	// adapterIdentityColumns are supplied by the store adapter from the unit
	// scope and the row identity, never by a caller.
	adapterIdentityColumn = "id"
	adapterTenantColumn   = "tenant_id"
	adapterProjectColumn  = "project_id"
)

// Unit builds the one-row content_records commit unit for an authorized raw
// capture. The recorder owns row-identity allocation, so id is the allocated
// content row identity. It is written only when the configured model opts in;
// the writer applies the seven-day default retention when the caller supplies
// none, and the retention expiry is always strictly after the recorded time.
// The row carries request/response bytes only, never a provider credential.
func (record ContentRecord) Unit(rowID string) (store.Unit, error) {
	if err := record.validate(); err != nil {
		return store.Unit{}, err
	}
	if rowID == "" {
		return store.Unit{}, fault.New(fault.InvalidInput, "content record id is empty")
	}
	if len(rowID) > id.MaxIDLength {
		return store.Unit{}, fault.New(fault.InvalidInput, "content record id is too long")
	}
	retainUntil := record.RetainUntil
	if retainUntil.IsZero() {
		retainUntil = record.At.Add(DefaultContentRetention)
	}
	if !retainUntil.After(record.At) {
		return store.Unit{}, fault.New(fault.InvalidInput, "content record retention must be strictly after the recorded time")
	}
	var executionID any
	if record.ExecutionID != "" {
		executionID = string(record.ExecutionID)
	}
	unit := store.Unit{
		Writes: []store.Write{{
			Table:  ContentTable,
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id":  string(record.PrincipalID),
				"execution_id":  executionID,
				"model":         record.Model,
				"request_body":  append([]byte{}, record.Request...),
				"response_body": append([]byte{}, record.Response...),
				"recorded_at":   record.At,
				"retain_until":  retainUntil,
				"version":       uint64(1),
			},
		}},
	}
	if err := conformUnit(unit, ContentTable); err != nil {
		return store.Unit{}, fault.Wrap(fault.InternalFailure, "content record unit does not conform to the frozen schema", err)
	}
	return unit, nil
}

// validate checks that a content capture can become an attributable, bounded
// retention row: a project scope, a principal, an optional valid execution id,
// a configured model, a recorded time and a retention window.
func (record ContentRecord) validate() error {
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if !record.Scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "content record scope must name a project")
	}
	if _, err := id.ParsePrincipalID(string(record.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "content record principal id is invalid", err)
	}
	if record.ExecutionID != "" {
		if _, err := id.ParseExecutionID(string(record.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "content record execution id is invalid", err)
		}
	}
	if record.Model == "" {
		return fault.New(fault.InvalidInput, "content record model is empty")
	}
	if record.At.IsZero() {
		return fault.New(fault.InvalidInput, "content record time is zero")
	}
	if !record.RetainUntil.IsZero() && !record.RetainUntil.After(record.At) {
		return fault.New(fault.InvalidInput, "content record retention must be strictly after the recorded time")
	}
	return nil
}

// StoreContentRecorder is the neutral store-backed ContentRecorder. It commits
// exactly one content_records insert through store.Executor under the record's
// project scope; the adapter supplies id/tenant_id/project_id and enforces the
// declared constraints. NewID allocates the content row identity, because a
// content capture has no identity of its own in the declared value.
type StoreContentRecorder struct {
	Executor store.Executor
	NewID    func() (string, error)
}

// Record persists one authorized raw-content capture.
func (recorder StoreContentRecorder) Record(ctx context.Context, record ContentRecord) error {
	if recorder.Executor == nil {
		return fault.New(fault.InternalFailure, "content recorder requires a store executor")
	}
	if recorder.NewID == nil {
		return fault.New(fault.InternalFailure, "content recorder requires an identity source")
	}
	rowID, err := recorder.NewID()
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "content record identity could not be allocated", err)
	}
	unit, err := record.Unit(rowID)
	if err != nil {
		return err
	}
	if _, err := recorder.Executor.Execute(ctx, record.Scope, unit); err != nil {
		return fault.Wrap(fault.InternalFailure, "content record could not be persisted", err)
	}
	return nil
}

// conformUnit proves one gateway-built unit is a single plain insert into a
// declared table: the table exists, the row identity is present, every written
// column is declared, every required non-adapter column is present and every
// value has a plausible Go type for the declared column. It catches schema
// drift before an adapter sees the unit.
func conformUnit(unit store.Unit, tableName string) error {
	if len(unit.Locks) != 0 {
		return fault.New(fault.InternalFailure, fmt.Sprintf("unit locks %d rows; want none", len(unit.Locks)))
	}
	if unit.Receipt != nil || unit.ReceiptMode != store.ReceiptInsert || unit.ReceiptExpect != nil {
		return fault.New(fault.InternalFailure, "unit carries receipt intent; want none")
	}
	if len(unit.Audit) != 0 || len(unit.Events) != 0 {
		return fault.New(fault.InternalFailure, "unit carries audit or event intent; want none")
	}
	if len(unit.Writes) != 1 {
		return fault.New(fault.InternalFailure, fmt.Sprintf("unit writes %d rows; want exactly one", len(unit.Writes)))
	}
	write := unit.Writes[0]
	if !write.Insert || write.Delete {
		return fault.New(fault.InternalFailure, "unit write is not a plain insert")
	}
	if write.ExpectVersion != 0 || len(write.Expect) != 0 || write.Deadline != nil {
		return fault.New(fault.InternalFailure, "unit insert carries a write guard")
	}
	if write.Table != tableName {
		return fault.New(fault.InternalFailure, fmt.Sprintf("unit writes table %q; want %q", write.Table, tableName))
	}
	if write.ID == "" {
		return fault.New(fault.InternalFailure, "unit row identity is empty")
	}
	table, ok := schemaTable(tableName)
	if !ok {
		return fault.New(fault.InternalFailure, fmt.Sprintf("table %q is not in the frozen schema", tableName))
	}
	for name, value := range write.Values {
		column, ok := tableColumn(table, name)
		if !ok {
			return fault.New(fault.InternalFailure, fmt.Sprintf("table %q has no column %q", tableName, name))
		}
		switch name {
		case adapterIdentityColumn, adapterTenantColumn, adapterProjectColumn:
			return fault.New(fault.InternalFailure, fmt.Sprintf("unit writes adapter-owned column %q", name))
		}
		if err := plausibleColumnValue(column, value); err != nil {
			return err
		}
	}
	for _, column := range table.Columns {
		if column.Nullable || column.Name == adapterIdentityColumn ||
			column.Name == adapterTenantColumn || column.Name == adapterProjectColumn {
			continue
		}
		if _, present := write.Values[column.Name]; !present {
			return fault.New(fault.InternalFailure, fmt.Sprintf("unit omits required column %q of table %q", column.Name, tableName))
		}
	}
	return nil
}

// schemaTable looks one table up in the declared schema design.
func schemaTable(name string) (store.Table, bool) {
	for _, table := range store.SchemaDesign().Tables {
		if table.Name == name {
			return table, true
		}
	}
	return store.Table{}, false
}

// tableColumn looks one column up inside a declared table.
func tableColumn(table store.Table, name string) (store.Column, bool) {
	for _, column := range table.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return store.Column{}, false
}

// plausibleColumnValue mirrors the adapter type check: text is string, integers
// are signed or unsigned integers, boolean is bool, timestamptz is time.Time and
// bytea/jsonb are []byte. Only a nullable column may be SQL NULL.
func plausibleColumnValue(column store.Column, value any) error {
	if value == nil {
		if !column.Nullable {
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not nullable but the unit writes NULL", column.Name))
		}
		return nil
	}
	switch column.Type {
	case "text":
		if _, ok := value.(string); !ok {
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q expects text; got %T", column.Name, value))
		}
	case "bigint", "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		default:
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q expects an integer; got %T", column.Name, value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q expects bool; got %T", column.Name, value))
		}
	case "timestamptz":
		if _, ok := value.(time.Time); !ok {
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q expects time.Time; got %T", column.Name, value))
		}
	case "bytea", "jsonb":
		if _, ok := value.([]byte); !ok {
			return fault.New(fault.InternalFailure, fmt.Sprintf("column %q expects []byte; got %T", column.Name, value))
		}
	default:
		return fault.New(fault.InternalFailure, fmt.Sprintf("column %q has unsupported frozen type %q", column.Name, column.Type))
	}
	return nil
}
