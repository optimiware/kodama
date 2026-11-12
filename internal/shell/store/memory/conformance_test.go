// Conformance interpreter: the abstract adapter store vectors are executed
// against memory.Store's structured commit-unit seam. The vector file is never
// edited; every vector is loaded from test/contracts/adapters/testdata/store.json
// and every step is translated as follows:
//
//	begin      start one pending store.Unit in the step scope. A second begin
//	           abandons an uncommitted pending unit, exactly as the reference
//	           vector adapter does.
//	write      append one store.Write. A write without expect_version is an
//	           insert carrying the caller-supplied initial version 1.
//	           expect_version maps to Write.ExpectVersion on a table with the
//	           single version column and to Write.Expect{"lifecycle_version": n}
//	           on tasks, whose optimistic versions are named columns. A write
//	           whose table is events or audit appends to Unit.Events or Unit.Audit
//	           because those rows are committed as publication intent/audit
//	           rather than as business writes. A reference step becomes a scoped
//	           store.Lock on the referenced row.
//	receipt    stage one receipt.Record. want_result is asserted on the Execute
//	           result at the next commit because the seam applies a unit as a
//	           whole. A receipt step that carries want_error executes at once.
//	commit     Execute the pending unit exactly once.
//	rollback   discard the pending unit. The caller never submits it, so no
//	           partial state can exist.
//	fail       arm FailNextCommit with a sentinel and Execute. The sentinel is
//	           reported as injected_failure.
//	read       Store.Read against committed state. A read outside an open begin
//	           window is no_transaction; a scoped miss is not_found/want_absent.
//	inspect    Store.Snapshot over committed state.
//	inspect_receipt
//	           Store.Receipt. want_content is compared by content fingerprint
//	           because the durable record stores the hash, not the body.
//
// A VersionConflict from a reference-carrying write is reported as the vector's
// cross_scope class when the referenced row exists in a different scope. The
// adapter itself still refuses the whole unit and applies nothing; the class is
// only the vector vocabulary's name for that refusal.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type storeVectorDocument struct {
	Version int           `json:"version"`
	Vectors []storeVector `json:"vectors"`
}

type storeVector struct {
	Name  string            `json:"name"`
	Kind  string            `json:"kind"`
	Seed  storeVectorSeed   `json:"seed"`
	Steps []storeVectorStep `json:"steps"`
}

type storeVectorSeed struct {
	Rows     []storeVectorRow     `json:"rows"`
	Receipts []storeVectorReceipt `json:"receipts"`
}

type storeVectorRow struct {
	Table     string            `json:"table"`
	TenantID  string            `json:"tenant_id"`
	ProjectID string            `json:"project_id"`
	ID        string            `json:"id"`
	Version   uint64            `json:"version"`
	Values    map[string]string `json:"values"`
}

type storeVectorReceipt struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Operation   string `json:"operation"`
	CallerKey   string `json:"caller_key"`
	Content     string `json:"content"`
	Result      string `json:"result"`
}

type storeVectorScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type storeVectorReference struct {
	Table     string `json:"table"`
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
}

type storeVectorKey struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Operation   string `json:"operation"`
	CallerKey   string `json:"caller_key"`
}

type storeVectorStep struct {
	Op            string                `json:"op"`
	Scope         *storeVectorScope     `json:"scope,omitempty"`
	Table         string                `json:"table,omitempty"`
	ID            string                `json:"id,omitempty"`
	Values        map[string]string     `json:"values,omitempty"`
	ExpectVersion *uint64               `json:"expect_version,omitempty"`
	Reference     *storeVectorReference `json:"reference,omitempty"`
	Key           *storeVectorKey       `json:"key,omitempty"`
	Content       string                `json:"content,omitempty"`
	Result        string                `json:"result,omitempty"`
	Point         string                `json:"point,omitempty"`
	WantError     string                `json:"want_error,omitempty"`
	WantResult    string                `json:"want_result,omitempty"`
	Want          map[string]string     `json:"want,omitempty"`
	WantVersion   *uint64               `json:"want_version,omitempty"`
	WantAbsent    bool                  `json:"want_absent,omitempty"`
	WantContent   string                `json:"want_content,omitempty"`
}

type vectorError struct {
	class string
}

func (err vectorError) Error() string {
	return err.class
}

// errVectorInjected is the sentinel armed through FailNextCommit. The adapter
// must return the armed error unchanged.
var errVectorInjected = errors.New("injected_failure")

func TestStoreRunsEveryAdapterStoreVector(t *testing.T) {
	document := loadStoreVectors(t)
	if document.Version != 1 {
		t.Fatalf("store vector document version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("store conformance vector set is empty")
	}
	tables := schemaTables()
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			s := New()
			seedVector(t, s, vector.Seed)
			run := &vectorRun{t: t, store: s, tables: tables}
			if err := run.execute(vector.Steps); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
}

func loadStoreVectors(t *testing.T) storeVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "store.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store conformance vectors: %v", err)
	}
	var document storeVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode store conformance vectors: %v", err)
	}
	return document
}

func schemaTables() map[string]bool {
	tables := make(map[string]bool, len(store.SchemaDesign().Tables))
	for _, table := range store.SchemaDesign().Tables {
		tables[table.Name] = true
	}
	return tables
}

// vectorVersionColumn names the column the abstract vector's row version maps
// to. tasks has no single version column, so its abstract version is the
// lifecycle optimistic version.
func vectorVersionColumn(table string) (string, bool) {
	if table == "tasks" {
		return "lifecycle_version", true
	}
	for _, design := range store.SchemaDesign().Tables {
		if design.Name != table {
			continue
		}
		for _, column := range design.Columns {
			if column.Name == "version" {
				return "version", true
			}
		}
		return "", false
	}
	return "", false
}

func vectorScope(scope storeVectorScope) tenancy.Scope {
	return scopeFor(scope.TenantID, scope.ProjectID)
}

func vectorReceiptKey(key storeVectorKey) receipt.Key {
	return keyFor(key.TenantID, key.ProjectID, key.PrincipalID, key.Operation, key.CallerKey)
}

func vectorReceiptID(key storeVectorKey) id.ReceiptID {
	identity := key.TenantID + "|" + key.ProjectID + "|" + key.PrincipalID + "|" + key.Operation + "|" + key.CallerKey
	sum := sha256.Sum256([]byte(identity))
	return id.ReceiptID("receipt-" + hex.EncodeToString(sum[:8]))
}

func vectorReceipt(key storeVectorKey, content, result string) (receipt.Record, error) {
	receiptKey := vectorReceiptKey(key)
	if err := receiptKey.Validate(); err != nil {
		return receipt.Record{}, err
	}
	fingerprint, err := fingerprintOfContent(content)
	if err != nil {
		return receipt.Record{}, err
	}
	return receipt.Record{
		ID:          vectorReceiptID(key),
		Key:         receiptKey,
		ContentHash: fingerprint,
		Result:      []byte(result),
		At:          fixedTime(),
	}, nil
}

func seedVector(t *testing.T, s *Store, seed storeVectorSeed) {
	t.Helper()
	for _, row := range seed.Rows {
		values := make(map[string]any, len(row.Values)+1)
		for name, value := range row.Values {
			values[name] = value
		}
		if column, ok := vectorVersionColumn(row.Table); ok {
			version := row.Version
			if version == 0 {
				version = 1
			}
			values[column] = version
		}
		seedRow(t, s, scopeFor(row.TenantID, row.ProjectID), row.Table, row.ID, values)
	}
	for _, seeded := range seed.Receipts {
		record, err := vectorReceipt(storeVectorKey{
			TenantID:    seeded.TenantID,
			ProjectID:   seeded.ProjectID,
			PrincipalID: seeded.PrincipalID,
			Operation:   seeded.Operation,
			CallerKey:   seeded.CallerKey,
		}, seeded.Content, seeded.Result)
		if err != nil {
			t.Fatalf("seed receipt: %v", err)
		}
		mustExecute(t, s, scopeFor(seeded.TenantID, seeded.ProjectID), store.Unit{Receipt: &record})
	}
}

type pendingVectorUnit struct {
	scope      tenancy.Scope
	unit       store.Unit
	wantResult string
}

type vectorRun struct {
	t       *testing.T
	store   *Store
	tables  map[string]bool
	pending *pendingVectorUnit
}

func (run *vectorRun) execute(steps []storeVectorStep) error {
	for index, step := range steps {
		if err := run.step(step); err != nil {
			return fmt.Errorf("step %d %s: %w", index, step.Op, err)
		}
	}
	return nil
}

func (run *vectorRun) step(step storeVectorStep) error {
	switch step.Op {
	case "begin":
		return run.begin(step)
	case "write":
		return run.write(step)
	case "receipt":
		return run.receipt(step)
	case "commit":
		return run.commit(step)
	case "rollback":
		return run.rollback(step)
	case "fail":
		return run.fail(step)
	case "read":
		return run.read(step)
	case "inspect":
		return run.inspect(step)
	case "inspect_receipt":
		return run.inspectReceipt(step)
	default:
		return fmt.Errorf("unknown op %q", step.Op)
	}
}

func (run *vectorRun) begin(step storeVectorStep) error {
	if step.Scope == nil {
		return fmt.Errorf("begin without a scope")
	}
	run.pending = &pendingVectorUnit{scope: vectorScope(*step.Scope)}
	return nil
}

func (run *vectorRun) write(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	if !run.tables[step.Table] {
		return vectorError{"invalid_table"}
	}
	if step.Reference != nil {
		run.pending.unit.Locks = append(run.pending.unit.Locks, store.Lock{
			Table: step.Reference.Table,
			IDs:   []string{step.Reference.ID},
		})
	}
	switch step.Table {
	case "events":
		envelope, err := vectorEnvelope(run.pending.scope, step)
		if err != nil {
			return err
		}
		run.pending.unit.Events = append(run.pending.unit.Events, envelope)
	case "audit":
		audit, err := vectorAudit(run.pending.scope, step)
		if err != nil {
			return err
		}
		run.pending.unit.Audit = append(run.pending.unit.Audit, audit)
	default:
		write, err := vectorWrite(step)
		if err != nil {
			return err
		}
		run.pending.unit.Writes = append(run.pending.unit.Writes, write)
	}
	if step.WantError != "" {
		return run.applyAndCheck(step)
	}
	return nil
}

func (run *vectorRun) receipt(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	if step.Key == nil {
		return fmt.Errorf("receipt without a key")
	}
	record, err := vectorReceipt(*step.Key, step.Content, step.Result)
	if err != nil {
		return err
	}
	run.pending.unit.Receipt = &record
	if step.WantResult != "" {
		run.pending.wantResult = step.WantResult
	}
	if step.WantError != "" {
		return run.applyAndCheck(step)
	}
	return nil
}

func (run *vectorRun) commit(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	pending := run.pending
	run.pending = nil
	result, err := run.store.Execute(context.Background(), pending.scope, pending.unit)
	if step.WantError != "" {
		return run.expectError(step, err, pending.scope)
	}
	if err != nil {
		return err
	}
	if pending.wantResult != "" && string(result.Receipt.Result) != pending.wantResult {
		return fmt.Errorf("receipt result = %q; want %q", result.Receipt.Result, pending.wantResult)
	}
	return nil
}

func (run *vectorRun) rollback(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	run.pending = nil
	return nil
}

func (run *vectorRun) fail(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	if step.WantError == "" {
		return fmt.Errorf("fail step without want_error")
	}
	run.store.FailNextCommit(errVectorInjected)
	return run.applyAndCheck(step)
}

func (run *vectorRun) read(step storeVectorStep) error {
	if run.pending == nil {
		return run.noTransaction(step)
	}
	row, err := run.store.Read(context.Background(), run.pending.scope, step.Table, step.ID)
	if err != nil {
		if step.WantError != "" {
			return run.expectError(step, err, run.pending.scope)
		}
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound && step.WantAbsent {
			return nil
		}
		return err
	}
	return checkVectorRow(step, row, true)
}

func (run *vectorRun) inspect(step storeVectorStep) error {
	if step.Scope == nil {
		return fmt.Errorf("inspect without a scope")
	}
	row, found := run.store.Snapshot(vectorScope(*step.Scope), step.Table, step.ID)
	return checkVectorRow(step, row, found)
}

func (run *vectorRun) inspectReceipt(step storeVectorStep) error {
	if step.Key == nil {
		return fmt.Errorf("inspect_receipt without a key")
	}
	record, found := run.store.Receipt(vectorReceiptKey(*step.Key))
	return checkVectorReceipt(step, record, found)
}

func (run *vectorRun) noTransaction(step storeVectorStep) error {
	err := vectorError{"no_transaction"}
	if step.WantError != "" {
		return run.expectError(step, err, tenancy.Scope{})
	}
	return err
}

func (run *vectorRun) applyAndCheck(step storeVectorStep) error {
	pending := run.pending
	run.pending = nil
	_, err := run.store.Execute(context.Background(), pending.scope, pending.unit)
	return run.expectError(step, err, pending.scope)
}

func (run *vectorRun) expectError(step storeVectorStep, err error, unitScope tenancy.Scope) error {
	if err == nil {
		return fmt.Errorf("step %s succeeded; want error class %q", step.Op, step.WantError)
	}
	got := run.classify(err, step, unitScope)
	if got != step.WantError {
		return fmt.Errorf("step %s error = %q; want %q (%v)", step.Op, got, step.WantError, err)
	}
	return nil
}

func (run *vectorRun) classify(err error, step storeVectorStep, unitScope tenancy.Scope) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errVectorInjected) {
		return "injected_failure"
	}
	if classed, ok := errors.AsType[vectorError](err); ok {
		return classed.class
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		return "unclassified"
	}
	switch class {
	case fault.VersionConflict:
		if step.Reference != nil {
			return run.referenceClass(*step.Reference, unitScope)
		}
		return "version_conflict"
	case fault.ChangedIdempotencyContent:
		return "changed_idempotency_content"
	case fault.Forbidden:
		return "cross_scope"
	case fault.NotFound:
		return "not_found"
	default:
		return string(class)
	}
}

func (run *vectorRun) referenceClass(reference storeVectorReference, unitScope tenancy.Scope) string {
	referenceScope := scopeFor(reference.TenantID, reference.ProjectID)
	if _, found := run.store.Snapshot(referenceScope, reference.Table, reference.ID); !found {
		return "not_found"
	}
	if referenceScope == unitScope {
		return "not_found"
	}
	return "cross_scope"
}

func vectorWrite(step storeVectorStep) (store.Write, error) {
	values := make(map[string]any, len(step.Values))
	for name, value := range step.Values {
		values[name] = value
	}
	write := store.Write{Table: step.Table, ID: step.ID, Values: values}
	column, versioned := vectorVersionColumn(step.Table)
	if step.ExpectVersion != nil {
		if !versioned {
			return store.Write{}, fmt.Errorf("table %q has no vector version column", step.Table)
		}
		if column == "version" {
			write.ExpectVersion = *step.ExpectVersion
		} else {
			write.Expect = map[string]any{column: *step.ExpectVersion}
		}
		return write, nil
	}
	write.Insert = true
	if versioned {
		values[column] = uint64(1)
	}
	return write, nil
}

func vectorEnvelope(scope tenancy.Scope, step storeVectorStep) (event.Envelope, error) {
	envelope := event.Envelope{
		ID:        id.EventID(step.ID),
		Type:      event.Type(step.Values["type"]),
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID(testPrincipalID)},
		At:        fixedTime(),
		Scope:     scope,
		SubjectID: step.Values["subject_id"],
	}
	if envelope.Type == event.TypeVerifiedReport {
		envelope.ReportType = event.ReportType(step.Values["report_type"])
	}
	if err := envelope.Validate(); err != nil {
		return event.Envelope{}, err
	}
	return envelope, nil
}

func vectorAudit(scope tenancy.Scope, step storeVectorStep) (event.Audit, error) {
	actor := step.Values["actor"]
	if actor == "" {
		actor = testPrincipalID
	}
	operation := step.Values["operation"]
	if operation == "" {
		operation = "vector.operation"
	}
	return event.Audit{
		ID:        id.EventID(step.ID),
		Scope:     scope,
		Actor:     id.PrincipalID(actor),
		Operation: operation,
		At:        fixedTime(),
		Detail:    step.Values["detail"],
	}, nil
}

func checkVectorRow(step storeVectorStep, row store.Row, found bool) error {
	if step.WantAbsent {
		if found {
			return fmt.Errorf("row %s/%s is present; want absent", step.Table, step.ID)
		}
		return nil
	}
	if !found {
		return fmt.Errorf("row %s/%s is absent; want present", step.Table, step.ID)
	}
	if step.WantVersion != nil {
		column, ok := vectorVersionColumn(step.Table)
		if !ok {
			return fmt.Errorf("table %q has no vector version column", step.Table)
		}
		raw, present := row.Values[column]
		if !present {
			return fmt.Errorf("row %s/%s has no %q column", step.Table, step.ID, column)
		}
		got, ok := toUint64(raw)
		if !ok {
			return fmt.Errorf("row %s/%s %q is %T; want an integer", step.Table, step.ID, column, raw)
		}
		if got != *step.WantVersion {
			return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, got, *step.WantVersion)
		}
	}
	for name, want := range step.Want {
		raw, present := row.Values[name]
		if !present {
			return fmt.Errorf("row %s/%s has no %q column", step.Table, step.ID, name)
		}
		if got := fmt.Sprintf("%v", raw); got != want {
			return fmt.Errorf("row %s/%s value %q = %q; want %q", step.Table, step.ID, name, got, want)
		}
	}
	return nil
}

func checkVectorReceipt(step storeVectorStep, record receipt.Record, found bool) error {
	if step.WantAbsent {
		if found {
			return fmt.Errorf("receipt is present; want absent")
		}
		return nil
	}
	if !found {
		return fmt.Errorf("receipt is absent; want present")
	}
	if step.WantContent != "" {
		want, err := fingerprintOfContent(step.WantContent)
		if err != nil {
			return err
		}
		if record.ContentHash != want {
			return fmt.Errorf("receipt content fingerprint = %q; want %q", record.ContentHash, want)
		}
	}
	if step.WantResult != "" && string(record.Result) != step.WantResult {
		return fmt.Errorf("receipt result = %q; want %q", record.Result, step.WantResult)
	}
	return nil
}

// toUint64 reads a vector version and requires the exact contract type: the
// adapter returns bigint columns as int64. Accepting uint64 or int would hide a
// non-contract column value from the conformance vectors.
func toUint64(value any) (uint64, bool) {
	typed, ok := value.(int64)
	if !ok || typed < 0 {
		return 0, false
	}
	return uint64(typed), true
}
