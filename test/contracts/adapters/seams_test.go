package adaptercontract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// The seam vectors are executable: each vector carries seed rows/receipts, an
// injected clock and ordered steps with exact expected scope, page, receipt or
// error-class results. The reference interpreter below is deliberately trivial;
// it proves the vectors describe the controller scope derivation, the
// pre-authentication lookup, receipt completion and the transaction-time
// deadline guard. Adapters port the same testdata/seams.json vectors.

type seamDocument struct {
	Version           int          `json:"version"`
	Controller        []seamVector `json:"controller"`
	Credentials       []seamVector `json:"credentials"`
	Receipts          []seamVector `json:"receipts"`
	ReceiptInProgress []seamVector `json:"receipt_in_progress"`
	Deadlines         []seamVector `json:"deadlines"`
}

type seamVector struct {
	Name  string     `json:"name"`
	Kind  string     `json:"kind"`
	Clock string     `json:"clock"`
	Seed  seamSeed   `json:"seed"`
	Steps []seamStep `json:"steps"`
}

type seamSeed struct {
	Rows     []seamSeedRow     `json:"rows"`
	Receipts []seamSeedReceipt `json:"receipts"`
}

type seamSeedRow struct {
	Table     string         `json:"table"`
	TenantID  string         `json:"tenant_id"`
	ProjectID string         `json:"project_id"`
	ID        string         `json:"id"`
	Version   uint64         `json:"version"`
	Values    map[string]any `json:"values"`
}

type seamSeedReceipt struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Operation   string `json:"operation"`
	CallerKey   string `json:"caller_key"`
	ContentHash string `json:"content_hash"`
	Result      string `json:"result"`
	ExecutionID string `json:"execution_id"`
	Completed   bool   `json:"completed"`
}

type seamScope struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
}

type seamStep struct {
	Op            string           `json:"op"`
	Table         string           `json:"table"`
	ID            string           `json:"id"`
	ExecutionID   string           `json:"execution_id"`
	Kind          string           `json:"kind"`
	Equals        map[string]any   `json:"equals"`
	Range         []scannerRange   `json:"range"`
	OrderBy       []scannerOrder   `json:"order_by"`
	Limit         int              `json:"limit"`
	Scope         *seamScope       `json:"scope"`
	Unit          *seamUnit        `json:"unit"`
	Receipt       *seamSeedReceipt `json:"receipt"`
	WantValues    map[string]any   `json:"want_values"`
	WantVersion   *uint64          `json:"want_version"`
	WantAbsent    bool             `json:"want_absent"`
	WantIDs       []string         `json:"want_ids"`
	WantResult    string           `json:"want_result"`
	WantReplayed  *bool            `json:"want_replayed"`
	WantCompleted *bool            `json:"want_completed"`
	WantCandidate *seamScope       `json:"want_candidate"`
	WantError     string           `json:"want_error"`
}

type seamUnit struct {
	Locks         []seamLock         `json:"locks"`
	Writes        []seamWrite        `json:"writes"`
	Receipt       *seamSeedReceipt   `json:"receipt"`
	ReceiptMode   string             `json:"receipt_mode"`
	ReceiptExpect *seamReceiptExpect `json:"receipt_expect"`
	Audit         int                `json:"audit"`
	Events        int                `json:"events"`
}

type seamLock struct {
	Table         string         `json:"table"`
	IDs           []string       `json:"ids"`
	ExpectVersion uint64         `json:"expect_version"`
	Expect        map[string]any `json:"expect"`
	TenantWide    bool           `json:"tenant_wide"`
	Deadline      string         `json:"deadline"`
}

type seamWrite struct {
	Table         string         `json:"table"`
	ID            string         `json:"id"`
	Insert        bool           `json:"insert"`
	Delete        bool           `json:"delete"`
	ExpectVersion uint64         `json:"expect_version"`
	Expect        map[string]any `json:"expect"`
	Values        map[string]any `json:"values"`
	Deadline      string         `json:"deadline"`
}

type seamReceiptExpect struct {
	Completed   bool   `json:"completed"`
	ContentHash string `json:"content_hash"`
}

var seamErrorClasses = map[string]bool{
	"invalid_input":               true,
	"forbidden":                   true,
	"not_found":                   true,
	"version_conflict":            true,
	"changed_idempotency_content": true,
	"deadline_expired":            true,
	"unauthenticated":             true,
}

// TestSeamConformanceVectors runs every seam vector against the reference
// interpreter and proves the vectors are not vacuous by running deliberately
// non-conforming fakes.
func TestSeamConformanceVectors(t *testing.T) {
	document := loadSeamVectors(t)
	validateSeamVectors(t, document)
	for _, vector := range document.Controller {
		vector := vector
		t.Run("controller/"+vector.Name, func(t *testing.T) {
			reference := newReferenceSeams(vector.Seed, vector.Clock)
			if err := runControllerVector(vector, reference); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Credentials {
		vector := vector
		t.Run("credentials/"+vector.Name, func(t *testing.T) {
			reference := newReferenceSeams(vector.Seed, vector.Clock)
			if err := runCredentialVector(vector, reference); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Receipts {
		vector := vector
		t.Run("receipts/"+vector.Name, func(t *testing.T) {
			reference := newReferenceSeams(vector.Seed, vector.Clock)
			if err := runReceiptVector(vector, referenceScopedExecutor{reference}); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.ReceiptInProgress {
		vector := vector
		t.Run("receipt_in_progress/"+vector.Name, func(t *testing.T) {
			reference := newReferenceSeams(vector.Seed, vector.Clock)
			if err := runReceiptVector(vector, referenceScopedExecutor{reference}); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Deadlines {
		vector := vector
		t.Run("deadlines/"+vector.Name, func(t *testing.T) {
			reference := newReferenceSeams(vector.Seed, vector.Clock)
			if err := runDeadlineVector(vector, referenceScopedExecutor{reference}); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}

	// Non-vacuous: a controller that ignores the table authority and a receipt
	// executor that overwrites an already-completed receipt must fail.
	controllerFailures := 0
	for _, vector := range document.Controller {
		reference := newReferenceSeams(vector.Seed, vector.Clock)
		if err := runControllerVector(vector, brokenController{reference}); err != nil {
			controllerFailures++
		}
	}
	if controllerFailures == 0 {
		t.Fatal("no controller vector failed against the deliberately non-conforming fake")
	}
	receiptFailures := 0
	for _, vector := range document.Receipts {
		reference := newReferenceSeams(vector.Seed, vector.Clock)
		if err := runReceiptVector(vector, brokenReceiptExecutor{reference}); err != nil {
			receiptFailures++
		}
	}
	if receiptFailures == 0 {
		t.Fatal("no receipt vector failed against the deliberately non-conforming fake")
	}
	// Non-vacuous: an adapter that drops the incomplete receipt's in-progress
	// result (the persisted trigger selection) must fail the in-progress
	// receipt vectors.
	inProgressFailures := 0
	for _, vector := range document.ReceiptInProgress {
		reference := newReferenceSeams(vector.Seed, vector.Clock)
		if err := runReceiptVector(vector, brokenInProgressReceiptExecutor{referenceScopedExecutor{reference}}); err != nil {
			inProgressFailures++
		}
	}
	if inProgressFailures == 0 {
		t.Fatal("no in-progress receipt vector failed against a fake that drops the stored in-progress result")
	}
}

func loadSeamVectors(t *testing.T) seamDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "seams.json"))
	if err != nil {
		t.Fatalf("read seam conformance vectors: %v", err)
	}
	var document seamDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode seam conformance vectors: %v", err)
	}
	return document
}

func validateSeamVectors(t *testing.T, document seamDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("seam vector document version = %d; want 1", document.Version)
	}
	if len(document.Controller) == 0 || len(document.Credentials) == 0 || len(document.Receipts) == 0 || len(document.ReceiptInProgress) == 0 || len(document.Deadlines) == 0 {
		t.Fatal("seam conformance vector sections must all be non-empty")
	}
	seen := map[string]bool{}
	validate := func(section string, vectors []seamVector) {
		for _, vector := range vectors {
			if vector.Name == "" || vector.Kind == "" {
				t.Errorf("%s vector is incomplete: %+v", section, vector)
				continue
			}
			key := section + "/" + vector.Name
			if seen[key] {
				t.Errorf("duplicate seam vector %q", key)
			}
			seen[key] = true
			if len(vector.Steps) == 0 {
				t.Errorf("seam vector %q has no steps", key)
			}
			for index, step := range vector.Steps {
				if step.WantError != "" && !seamErrorClasses[step.WantError] {
					t.Errorf("vector %q step %d expects unknown error class %q", key, index, step.WantError)
				}
				if step.WantError == "" && step.Op != "apply" && step.Op != "lookup" && step.Op != "execute" {
					if step.WantIDs == nil && step.WantValues == nil && step.WantAbsent == false && step.WantResult == "" && step.WantCompleted == nil && step.WantCandidate == nil {
						t.Errorf("vector %q step %d asserts nothing", key, index)
					}
				}
			}
		}
	}
	validate("controller", document.Controller)
	validate("credentials", document.Credentials)
	validate("receipts", document.Receipts)
	validate("receipt_in_progress", document.ReceiptInProgress)
	validate("deadlines", document.Deadlines)

	// Required negative/positive coverage.
	required := func(section string, match func(seamStep) bool) bool {
		var vectors []seamVector
		switch section {
		case "controller":
			vectors = document.Controller
		case "credentials":
			vectors = document.Credentials
		case "receipts":
			vectors = document.Receipts
		case "receipt_in_progress":
			vectors = document.ReceiptInProgress
		case "deadlines":
			vectors = document.Deadlines
		}
		for _, vector := range vectors {
			for _, step := range vector.Steps {
				if match(step) {
					return true
				}
			}
		}
		return false
	}
	if !required("controller", func(step seamStep) bool { return step.WantError == "forbidden" }) {
		t.Error("controller vectors never refuse a cross-scope or non-state unit")
	}
	if !required("controller", func(step seamStep) bool { return step.WantError == "not_found" }) {
		t.Error("controller vectors never refuse a missing execution")
	}
	if !required("credentials", func(step seamStep) bool { return step.WantError == "unauthenticated" }) {
		t.Error("credential vectors never refuse a missing/mismatched lookup")
	}
	if !required("receipts", func(step seamStep) bool { return step.WantError == "changed_idempotency_content" }) {
		t.Error("receipt vectors never refuse changed content")
	}
	if !required("receipts", func(step seamStep) bool { return step.WantReplayed != nil && *step.WantReplayed }) {
		t.Error("receipt vectors never replay a completed receipt")
	}
	if !required("receipts", func(step seamStep) bool {
		return step.Unit != nil && step.Unit.ReceiptMode == string(store.ReceiptInsertIncomplete)
	}) {
		t.Error("receipt vectors never insert an incomplete receipt")
	}
	// An incomplete insert MAY persist an in-progress result, and an equal
	// replay of it must return that result without completing the receipt.
	if !required("receipt_in_progress", func(step seamStep) bool {
		return step.Unit != nil && step.Unit.ReceiptMode == string(store.ReceiptInsertIncomplete) &&
			step.Unit.Receipt != nil && step.Unit.Receipt.Result != ""
	}) {
		t.Error("in-progress receipt vectors never insert an incomplete receipt with a result")
	}
	if !required("receipt_in_progress", func(step seamStep) bool {
		return step.WantReplayed != nil && *step.WantReplayed && step.WantResult != ""
	}) {
		t.Error("in-progress receipt vectors never replay an incomplete receipt and return its stored result")
	}
	if !required("receipt_in_progress", func(step seamStep) bool {
		return step.Op == "inspect_receipt" && step.WantCompleted != nil && !*step.WantCompleted
	}) {
		t.Error("in-progress receipt vectors never prove the in-progress receipt is still incomplete")
	}
	if !required("deadlines", func(step seamStep) bool { return step.WantError == "deadline_expired" }) {
		t.Error("deadline vectors never refuse a late guard")
	}
}

// seamNormalizeValue converts a JSON-decoded vector value to the shape store.Row
// promises: bigint/integer as int64 and timestamptz as time.Time.
func seamNormalizeValue(table, column string, value any) any {
	schema := store.SchemaDesign()
	tableValue, ok := findTable(schema, table)
	if !ok {
		return value
	}
	definition, ok := findColumn(tableValue, column)
	if !ok {
		return value
	}
	switch definition.Type {
	case "timestamptz":
		if text, ok := value.(string); ok {
			parsed, err := time.Parse(time.RFC3339, text)
			if err == nil {
				return parsed
			}
		}
	case "bigint", "integer":
		switch typed := value.(type) {
		case float64:
			return int64(typed)
		case int:
			return int64(typed)
		}
	}
	return value
}

func seamClock(value string) time.Time {
	if value == "" {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return parsed
}

func seamScopeOf(scope seamScope) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(scope.TenantID), ProjectID: id.ProjectID(scope.ProjectID)}
}

// seamRow is the reference interpreter's committed row.
type seamRow struct {
	table   string
	tenant  string
	project string
	id      string
	version uint64
	values  map[string]any
}

func (row seamRow) scanner() scannerRow {
	return scannerRow{table: row.table, tenant: row.tenant, project: row.project, id: row.id, values: row.values}
}

type seamReceiptState struct {
	tenant      string
	project     string
	principal   string
	operation   string
	callerKey   string
	contentHash string
	result      string
	executionID string
	completed   bool
}

type referenceSeams struct {
	rows     map[string]*seamRow
	receipts map[string]*seamReceiptState
	now      time.Time
}

func newReferenceSeams(seed seamSeed, clock string) *referenceSeams {
	reference := &referenceSeams{
		rows:     make(map[string]*seamRow, len(seed.Rows)),
		receipts: make(map[string]*seamReceiptState, len(seed.Receipts)),
		now:      seamClock(clock),
	}
	for _, row := range seed.Rows {
		values := make(map[string]any, len(row.Values))
		for column, value := range row.Values {
			values[column] = seamNormalizeValue(row.Table, column, value)
		}
		values["id"] = row.ID
		values["tenant_id"] = row.TenantID
		values["project_id"] = row.ProjectID
		values["version"] = row.Version
		reference.rows[seamRowKey(row.Table, row.TenantID, row.ProjectID, row.ID)] = &seamRow{
			table: row.Table, tenant: row.TenantID, project: row.ProjectID, id: row.ID, version: row.Version, values: values,
		}
	}
	for _, record := range seed.Receipts {
		reference.receipts[seamReceiptKey(record.TenantID, record.ProjectID, record.PrincipalID, record.Operation, record.CallerKey)] = &seamReceiptState{
			tenant: record.TenantID, project: record.ProjectID, principal: record.PrincipalID,
			operation: record.Operation, callerKey: record.CallerKey, contentHash: record.ContentHash,
			result: record.Result, executionID: record.ExecutionID, completed: record.Completed,
		}
	}
	return reference
}

func seamRowKey(table, tenant, project, id string) string {
	return table + "|" + tenant + "|" + project + "|" + id
}

func seamReceiptKey(tenant, project, principal, operation, callerKey string) string {
	return tenant + "|" + project + "|" + principal + "|" + operation + "|" + callerKey
}

func (reference *referenceSeams) findRow(table, id string) *seamRow {
	for _, row := range reference.rows {
		if row.table == table && row.id == id {
			return row
		}
	}
	return nil
}

func (reference *referenceSeams) Read(ctx context.Context, table, id string) (store.Row, error) {
	if err := ctx.Err(); err != nil {
		return store.Row{}, err
	}
	if !seamControllerTable(table, false) {
		return store.Row{}, fault.New(fault.InvalidInput, fmt.Sprintf("controller read table %q is not in the controller authority", table))
	}
	row := reference.findRow(table, id)
	if row == nil {
		return store.Row{}, fault.New(fault.NotFound, "controller row is missing")
	}
	return store.Row{Table: table, Values: cloneScannerValues(row.values)}, nil
}

func (reference *referenceSeams) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scan.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	terms := scannerCanonicalOrder(scan.OrderBy)
	candidates := make([]seamRow, 0, len(reference.rows))
	for _, row := range reference.rows {
		if row.table != scan.Table {
			continue
		}
		scanner := row.scanner()
		if !scannerRowMatchesEquals(scanner, scan.Equals) || !scannerRowMatchesRange(scanner, scan.Range) {
			continue
		}
		candidates = append(candidates, *row)
	}
	sortSeamRows(candidates, terms)
	if scan.After != "" {
		cursor, err := decodeScannerCursor(scan.After)
		if err != nil {
			return store.QueryPage{}, err
		}
		if cursor.Table != scan.Table || !scannerSameOrder(cursor.Order, terms) {
			return store.QueryPage{}, fault.New(fault.InvalidInput, "controller cursor belongs to another table or order")
		}
		filtered := make([]seamRow, 0, len(candidates))
		for _, row := range candidates {
			if scannerRowAfterCursor(row.scanner(), terms, cursor.Key) {
				filtered = append(filtered, row)
			}
		}
		candidates = filtered
	}
	page := store.QueryPage{}
	if len(candidates) > scan.Limit {
		page.HasMore = true
		candidates = candidates[:scan.Limit]
	}
	page.Rows = make([]store.Row, 0, len(candidates))
	for _, row := range candidates {
		page.Rows = append(page.Rows, store.Row{Table: scan.Table, Values: cloneScannerValues(row.values)})
	}
	if page.HasMore {
		page.Next = encodeScannerCursor(tenancy.Scope{}, scan.Table, terms, candidates[len(candidates)-1].scanner())
	}
	return page, nil
}

func sortSeamRows(rows []seamRow, terms []store.Order) {
	for left := 0; left < len(rows); left++ {
		for right := left + 1; right < len(rows); right++ {
			if scannerCompareRows(rows[left].scanner(), rows[right].scanner(), terms) > 0 {
				rows[left], rows[right] = rows[right], rows[left]
			}
		}
	}
}

func (reference *referenceSeams) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	execution := reference.findRow("executions", executionID)
	if execution == nil {
		return store.UnitResult{}, fault.New(fault.NotFound, "controller execution is missing")
	}
	scope := tenancy.Scope{TenantID: id.TenantID(execution.tenant), ProjectID: id.ProjectID(execution.project)}
	return reference.apply(scope, unit, true)
}

func (reference *referenceSeams) LookupCredential(ctx context.Context, kind store.CredentialKind, credentialID string) (store.CandidateScope, error) {
	if err := ctx.Err(); err != nil {
		return store.CandidateScope{}, err
	}
	switch kind {
	case store.CredentialExecution:
		row := reference.findRow("executions", credentialID)
		if row == nil {
			return store.CandidateScope{}, seamUniformUnauthenticated()
		}
		return store.CandidateScope{
			TenantID:    id.TenantID(row.tenant),
			ProjectID:   id.ProjectID(row.project),
			PrincipalID: id.PrincipalID(scannerString(row.values["principal_id"])),
		}, nil
	case store.CredentialService:
		row := reference.findRow("services", credentialID)
		if row == nil {
			return store.CandidateScope{}, seamUniformUnauthenticated()
		}
		return store.CandidateScope{
			TenantID:    id.TenantID(row.tenant),
			ProjectID:   id.ProjectID(row.project),
			PrincipalID: id.PrincipalID(credentialID),
		}, nil
	default:
		return store.CandidateScope{}, fault.New(fault.InvalidInput, "credential kind is not declared")
	}
}

func seamUniformUnauthenticated() error {
	return fault.New(fault.Unauthenticated, "credential candidate is not available")
}

// apply applies one unit atomically. controller restricts the writable tables
// and refuses receipt/audit/event commit units.
func (reference *referenceSeams) apply(scope tenancy.Scope, unit store.Unit, controller bool) (store.UnitResult, error) {
	if err := scope.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	if err := unit.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	if controller {
		if unit.Receipt != nil || len(unit.Audit) > 0 || len(unit.Events) > 0 {
			return store.UnitResult{}, fault.New(fault.Forbidden, "the controller cannot commit a receipt, audit row or event")
		}
		for _, lock := range unit.Locks {
			if !seamControllerTable(lock.Table, true) {
				return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller lock table %q is outside the controller authority", lock.Table))
			}
		}
		for _, write := range unit.Writes {
			if !seamControllerTable(write.Table, true) {
				return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller write table %q is outside the controller authority", write.Table))
			}
			// The controller write set includes events/audit, but only as
			// INSERT-only publication-intent/audit rows; a write to those tables
			// is never an update or delete.
			if controllerInsertOnly(write.Table) && !write.Insert {
				return store.UnitResult{}, fault.New(fault.Forbidden, fmt.Sprintf("controller table %q is insert-only", write.Table))
			}
		}
	}

	stagedRows := make(map[string]*seamRow)
	deletedRows := make(map[string]bool)
	stagedReceipts := make(map[string]*seamReceiptState)
	result := store.UnitResult{}

	currentRow := func(table string, rowScope tenancy.Scope, rowID string) *seamRow {
		key := seamRowKey(table, string(rowScope.TenantID), string(rowScope.ProjectID), rowID)
		if row, ok := stagedRows[key]; ok {
			return row
		}
		if deletedRows[key] {
			return nil
		}
		return reference.rows[key]
	}

	if unit.Receipt != nil {
		record := unit.Receipt
		key := seamReceiptKey(string(record.Key.TenantID), string(record.Key.ProjectID), string(record.Key.PrincipalID), record.Key.Operation, record.Key.CallerKey)
		existing := reference.receipts[key]
		if staged, ok := stagedReceipts[key]; ok {
			existing = staged
		}
		switch unit.ReceiptMode {
		case store.ReceiptInsert, store.ReceiptInsertIncomplete:
			if existing != nil {
				if existing.contentHash != record.ContentHash {
					return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent, "a receipt with the same caller key holds different content")
				}
				result.Replayed = true
				result.Receipt = receipt.Record{ID: record.ID, Key: record.Key, ContentHash: existing.contentHash, Result: []byte(existing.result), ExecutionID: id.ExecutionID(existing.executionID)}
				return result, nil
			}
			// An incomplete insert MAY carry the in-progress result (the
			// trigger Select's persisted selection); it is stored but the
			// receipt stays incomplete, so a later completion overwrites it.
			incomplete := unit.ReceiptMode == store.ReceiptInsertIncomplete
			stagedReceipts[key] = &seamReceiptState{
				tenant: string(record.Key.TenantID), project: string(record.Key.ProjectID), principal: string(record.Key.PrincipalID),
				operation: record.Key.Operation, callerKey: record.Key.CallerKey, contentHash: record.ContentHash,
				result: string(record.Result), executionID: string(record.ExecutionID), completed: !incomplete,
			}
		case store.ReceiptComplete:
			// Compare-and-set: the guarded completion matches only an
			// incomplete receipt (completed_at IS NULL) whose stored content
			// hash equals both the expectation and the unit. A zero-row outcome
			// is reclassified as replay or a conflict, never as an overwrite.
			if existing == nil {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "the receipt to complete is missing")
			}
			if existing.contentHash != unit.ReceiptExpect.ContentHash || existing.contentHash != record.ContentHash {
				return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent, "a receipt with the same caller key holds different content")
			}
			if existing.completed {
				result.Replayed = true
				result.Receipt = receipt.Record{ID: record.ID, Key: record.Key, ContentHash: existing.contentHash, Result: []byte(existing.result), ExecutionID: id.ExecutionID(existing.executionID)}
				return result, nil
			}
			if unit.ReceiptExpect.Completed {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "the receipt is not at the expected completion state")
			}
			completed := *existing
			completed.result = string(record.Result)
			completed.executionID = string(record.ExecutionID)
			completed.completed = true
			stagedReceipts[key] = &completed
		}
	}

	for _, lock := range unit.Locks {
		for _, rowID := range lock.IDs {
			row := currentRow(lock.Table, scope, rowID)
			if row == nil && lock.TenantWide {
				row = reference.tenantWideRow(lock.Table, string(scope.TenantID), rowID)
			}
			if row == nil {
				if controller && reference.crossScopeRow(lock.Table, scope, rowID) {
					return store.UnitResult{}, fault.New(fault.Forbidden, "a locked row is outside the owning scope")
				}
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row is missing or out of scope")
			}
			if !seamRowInScope(row, scope, lock.TenantWide) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "a locked row is outside the owning scope")
			}
			if lock.ExpectVersion != 0 && row.version != lock.ExpectVersion {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row was not at the expected version")
			}
			if !seamValuesMatch(row.values, lock.Expect) {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a locked row no longer matches its expected values")
			}
			if lock.Deadline != nil {
				if err := reference.checkDeadline(lock.Table, row, *lock.Deadline); err != nil {
					return store.UnitResult{}, err
				}
			}
		}
	}

	for _, write := range unit.Writes {
		key := seamRowKey(write.Table, string(scope.TenantID), string(scope.ProjectID), write.ID)
		switch {
		case write.Insert:
			if write.ExpectVersion != 0 || len(write.Expect) > 0 {
				return store.UnitResult{}, fault.New(fault.InvalidInput, "an insert cannot carry an expected version or values")
			}
			if currentRow(write.Table, scope, write.ID) != nil {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "an inserted row already exists")
			}
			values := make(map[string]any, len(write.Values))
			version := uint64(1)
			for column, value := range write.Values {
				switch column {
				case "id", "tenant_id", "project_id":
					return store.UnitResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("column %q is supplied by the adapter", column))
				case "version":
					version = seamUint64(value)
				default:
					values[column] = seamNormalizeValue(write.Table, column, value)
				}
			}
			values["id"] = write.ID
			values["tenant_id"] = string(scope.TenantID)
			values["project_id"] = string(scope.ProjectID)
			values["version"] = version
			stagedRows[key] = &seamRow{table: write.Table, tenant: string(scope.TenantID), project: string(scope.ProjectID), id: write.ID, version: version, values: values}
		case write.Delete:
			row := currentRow(write.Table, scope, write.ID)
			if row == nil {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a deleted row is missing or out of scope")
			}
			if !seamRowInScope(row, scope, false) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "a deleted row is outside the owning scope")
			}
			if write.ExpectVersion != 0 && row.version != write.ExpectVersion {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a deleted row was not at the expected version")
			}
			if !seamValuesMatch(row.values, write.Expect) {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "a deleted row no longer matches its expected values")
			}
			deletedRows[key] = true
		default:
			row := currentRow(write.Table, scope, write.ID)
			if row == nil {
				if controller && reference.crossScopeRow(write.Table, scope, write.ID) {
					return store.UnitResult{}, fault.New(fault.Forbidden, "an updated row is outside the owning scope")
				}
				return store.UnitResult{}, fault.New(fault.VersionConflict, "an updated row is missing or out of scope")
			}
			if !seamRowInScope(row, scope, false) {
				return store.UnitResult{}, fault.New(fault.Forbidden, "an updated row is outside the owning scope")
			}
			if write.ExpectVersion != 0 && row.version != write.ExpectVersion {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "an updated row was not at the expected version")
			}
			if !seamValuesMatch(row.values, write.Expect) {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "an updated row no longer matches its expected values")
			}
			if write.Deadline != nil {
				if err := reference.checkDeadline(write.Table, row, *write.Deadline); err != nil {
					return store.UnitResult{}, err
				}
			}
			updated := *row
			updated.values = cloneScannerValues(row.values)
			for column, value := range write.Values {
				switch column {
				case "id", "tenant_id", "project_id":
					return store.UnitResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("column %q is supplied by the adapter", column))
				case "version":
					return store.UnitResult{}, fault.New(fault.InvalidInput, "the adapter advances the version column")
				default:
					updated.values[column] = seamNormalizeValue(write.Table, column, value)
				}
			}
			updated.version++
			updated.values["version"] = updated.version
			stagedRows[key] = &updated
		}
	}

	for key, row := range stagedRows {
		reference.rows[key] = row
	}
	for key := range deletedRows {
		delete(reference.rows, key)
	}
	for key, record := range stagedReceipts {
		reference.receipts[key] = record
	}
	return result, nil
}

func (reference *referenceSeams) crossScopeRow(table string, scope tenancy.Scope, id string) bool {
	row := reference.findRow(table, id)
	if row == nil {
		return false
	}
	return row.tenant != string(scope.TenantID) || row.project != string(scope.ProjectID)
}

func (reference *referenceSeams) tenantWideRow(table, tenant, rowID string) *seamRow {
	return reference.rows[seamRowKey(table, tenant, "", rowID)]
}

func (reference *referenceSeams) checkDeadline(table string, row *seamRow, guard store.DeadlineGuard) error {
	if err := guard.Validate(); err != nil {
		return err
	}
	definition, ok := findTable(store.SchemaDesign(), table)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("deadline guard table %q is not in the declared schema", table))
	}
	column, ok := findColumn(definition, guard.Column)
	if !ok || column.Type != "timestamptz" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("deadline guard column %q is not a declared timestamp column", guard.Column))
	}
	stamp, ok := row.values[guard.Column].(time.Time)
	if !ok || !stamp.After(reference.now) {
		return fault.New(fault.DeadlineExpired, "the guarded timestamp is not strictly after the transaction clock")
	}
	return nil
}

func seamRowInScope(row *seamRow, scope tenancy.Scope, tenantWide bool) bool {
	if row.tenant != string(scope.TenantID) {
		return false
	}
	if row.project == string(scope.ProjectID) {
		return true
	}
	return tenantWide && row.project == "" && scope.ProjectScoped()
}

func seamValuesMatch(values map[string]any, expect map[string]any) bool {
	for column, want := range expect {
		got := values[column]
		if scannerCompareValues(got, want) != 0 {
			return false
		}
	}
	return true
}

func seamUint64(value any) uint64 {
	switch typed := value.(type) {
	case float64:
		return uint64(typed)
	case int64:
		return uint64(typed)
	case uint64:
		return typed
	case int:
		return uint64(typed)
	default:
		return 0
	}
}

func seamControllerTable(table string, write bool) bool {
	tables := store.ControllerReadTables()
	if write {
		tables = store.ControllerWriteTables()
	}
	for _, candidate := range tables {
		if candidate == table {
			return true
		}
	}
	return false
}

func (reference *referenceSeams) inspectRow(scope seamScope, table, id string) *seamRow {
	return reference.rows[seamRowKey(table, scope.TenantID, scope.ProjectID, id)]
}

func (reference *referenceSeams) inspectReceipt(record *seamSeedReceipt) *seamReceiptState {
	return reference.receipts[seamReceiptKey(record.TenantID, record.ProjectID, record.PrincipalID, record.Operation, record.CallerKey)]
}

// brokenController ignores the controller table authority.
type brokenController struct {
	*referenceSeams
}

func (controller brokenController) Read(ctx context.Context, table, id string) (store.Row, error) {
	for _, row := range controller.rows {
		if row.table == table && row.id == id {
			return store.Row{Table: table, Values: cloneScannerValues(row.values)}, nil
		}
	}
	return store.Row{}, fault.New(fault.NotFound, "missing")
}

func (controller brokenController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	scan.ScanQuery.Table = "actions"
	return controller.referenceSeams.Scan(ctx, scan)
}

func (controller brokenController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	return controller.referenceSeams.Execute(ctx, executionID, unit)
}

type referenceScopedExecutor struct {
	*referenceSeams
}

func (executor referenceScopedExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	return executor.referenceSeams.apply(scope, unit, false)
}

func (executor referenceScopedExecutor) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error) {
	row := executor.referenceSeams.rows[seamRowKey(table, string(scope.TenantID), string(scope.ProjectID), id)]
	if row == nil {
		return store.Row{}, fault.New(fault.NotFound, "missing")
	}
	return store.Row{Table: table, Values: cloneScannerValues(row.values)}, nil
}

// brokenReceiptExecutor simulates an adapter with no completion compare-and-set:
// it clears the stored completed marker before a completion unit, so a completed
// receipt is overwritten instead of replayed.
type brokenReceiptExecutor struct {
	*referenceSeams
}

func (executor brokenReceiptExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if unit.ReceiptMode == store.ReceiptComplete && unit.Receipt != nil {
		record := unit.Receipt
		key := seamReceiptKey(string(record.Key.TenantID), string(record.Key.ProjectID), string(record.Key.PrincipalID), record.Key.Operation, record.Key.CallerKey)
		if existing, ok := executor.referenceSeams.receipts[key]; ok {
			existing.completed = false
		}
		unit.ReceiptExpect = &store.ReceiptExpect{Completed: false, ContentHash: record.ContentHash}
	}
	return executor.referenceSeams.apply(scope, unit, false)
}

func (executor brokenReceiptExecutor) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error) {
	row := executor.referenceSeams.rows[seamRowKey(table, string(scope.TenantID), string(scope.ProjectID), id)]
	if row == nil {
		return store.Row{}, fault.New(fault.NotFound, "missing")
	}
	return store.Row{Table: table, Values: cloneScannerValues(row.values)}, nil
}

// brokenInProgressReceiptExecutor simulates an adapter that drops the
// in-progress result an incomplete insert persists (the trigger Select's exact
// selection). A later replay must return that stored payload, so dropping it
// has to fail at least one vector.
type brokenInProgressReceiptExecutor struct {
	referenceScopedExecutor
}

func (executor brokenInProgressReceiptExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if unit.ReceiptMode == store.ReceiptInsertIncomplete && unit.Receipt != nil {
		dropped := *unit.Receipt
		dropped.Result = nil
		unit.Receipt = &dropped
	}
	return executor.referenceScopedExecutor.Execute(ctx, scope, unit)
}

func seamUnitFromStep(step seamStep) (store.Unit, error) {
	unit := store.Unit{}
	if step.Unit == nil {
		return unit, nil
	}
	unit.ReceiptMode = store.ReceiptMode(step.Unit.ReceiptMode)
	for _, lock := range step.Unit.Locks {
		converted := store.Lock{
			Table:         lock.Table,
			IDs:           append([]string(nil), lock.IDs...),
			ExpectVersion: lock.ExpectVersion,
			Expect:        lock.Expect,
			TenantWide:    lock.TenantWide,
		}
		if lock.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: lock.Deadline}
		}
		unit.Locks = append(unit.Locks, converted)
	}
	for _, write := range step.Unit.Writes {
		converted := store.Write{
			Table:         write.Table,
			ID:            write.ID,
			Insert:        write.Insert,
			Delete:        write.Delete,
			ExpectVersion: write.ExpectVersion,
			Expect:        write.Expect,
			Values:        write.Values,
		}
		if write.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: write.Deadline}
		}
		unit.Writes = append(unit.Writes, converted)
	}
	if step.Unit.Receipt != nil {
		record := receipt.Record{
			Key: receipt.Key{
				TenantID:    id.TenantID(step.Unit.Receipt.TenantID),
				ProjectID:   id.ProjectID(step.Unit.Receipt.ProjectID),
				PrincipalID: id.PrincipalID(step.Unit.Receipt.PrincipalID),
				Operation:   step.Unit.Receipt.Operation,
				CallerKey:   step.Unit.Receipt.CallerKey,
			},
			ContentHash: step.Unit.Receipt.ContentHash,
			Result:      []byte(step.Unit.Receipt.Result),
			ExecutionID: id.ExecutionID(step.Unit.Receipt.ExecutionID),
		}
		unit.Receipt = &record
	}
	if step.Unit.ReceiptExpect != nil {
		unit.ReceiptExpect = &store.ReceiptExpect{Completed: step.Unit.ReceiptExpect.Completed, ContentHash: step.Unit.ReceiptExpect.ContentHash}
	}
	for index := 0; index < step.Unit.Audit; index++ {
		unit.Audit = append(unit.Audit, event.Audit{})
	}
	for index := 0; index < step.Unit.Events; index++ {
		unit.Events = append(unit.Events, event.Envelope{})
	}
	return unit, nil
}

func runControllerVector(vector seamVector, controller store.Controller) error {
	for index, step := range vector.Steps {
		switch step.Op {
		case "read":
			row, err := controller.Read(context.Background(), step.Table, step.ID)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" {
				if err := checkSeamRow(step, row, true); err != nil {
					return fmt.Errorf("step %d: %w", index, err)
				}
			}
		case "scan":
			scan := store.ControllerScan{ScanQuery: store.ScanQuery{
				Table: step.Table, Equals: step.Equals, Range: scannerRangeFilters(step.Range),
				OrderBy: scannerOrderTerms(step.OrderBy), Limit: step.Limit,
			}}
			page, err := controller.Scan(context.Background(), scan)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" {
				got := make([]string, 0, len(page.Rows))
				for _, row := range page.Rows {
					got = append(got, scannerString(row.Values["id"]))
				}
				if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
					return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
				}
			}
		case "execute":
			unit, err := seamUnitFromStep(step)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			result, err := controller.Execute(context.Background(), step.ExecutionID, unit)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" && step.WantReplayed != nil && result.Replayed != *step.WantReplayed {
				return fmt.Errorf("step %d Replayed = %v; want %v", index, result.Replayed, *step.WantReplayed)
			}
		case "inspect":
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			inspector, ok := controller.(interface {
				inspectRow(seamScope, string, string) *seamRow
			})
			if !ok {
				return fmt.Errorf("step %d: controller cannot be inspected", index)
			}
			row := inspector.inspectRow(*step.Scope, step.Table, step.ID)
			if row == nil {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d row %s/%s is absent; want present", index, step.Table, step.ID)
			}
			if step.WantAbsent {
				return fmt.Errorf("step %d row %s/%s is present; want absent", index, step.Table, step.ID)
			}
			if err := checkSeamRow(step, store.Row{Table: step.Table, Values: row.values}, true); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown controller op %q", index, step.Op)
		}
	}
	return nil
}

func runCredentialVector(vector seamVector, lookup store.CredentialLookup) error {
	for index, step := range vector.Steps {
		if step.Op != "lookup" {
			return fmt.Errorf("step %d has unknown credential op %q", index, step.Op)
		}
		candidate, err := lookup.LookupCredential(context.Background(), store.CredentialKind(step.Kind), step.ID)
		if err := checkSeamError(index, step, err); err != nil {
			return err
		}
		if step.WantError != "" {
			continue
		}
		if step.WantCandidate == nil {
			return fmt.Errorf("step %d asserts no candidate", index)
		}
		got := seamScope{TenantID: string(candidate.TenantID), ProjectID: string(candidate.ProjectID), PrincipalID: string(candidate.PrincipalID)}
		if got != *step.WantCandidate {
			return fmt.Errorf("step %d candidate = %+v; want %+v", index, got, *step.WantCandidate)
		}
	}
	return nil
}

func runReceiptVector(vector seamVector, executor store.Executor) error {
	for index, step := range vector.Steps {
		switch step.Op {
		case "apply":
			if step.Scope == nil {
				return fmt.Errorf("step %d applies without a scope", index)
			}
			unit, err := seamUnitFromStep(step)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			result, err := executor.Execute(context.Background(), seamScopeOf(*step.Scope), unit)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" {
				if step.WantReplayed != nil && result.Replayed != *step.WantReplayed {
					return fmt.Errorf("step %d Replayed = %v; want %v", index, result.Replayed, *step.WantReplayed)
				}
				if step.WantResult != "" && string(result.Receipt.Result) != step.WantResult {
					return fmt.Errorf("step %d result = %q; want %q", index, result.Receipt.Result, step.WantResult)
				}
			}
		case "inspect_receipt":
			inspector, ok := executor.(interface {
				inspectReceipt(*seamSeedReceipt) *seamReceiptState
			})
			if !ok {
				return fmt.Errorf("step %d: executor cannot be inspected", index)
			}
			if step.Receipt == nil {
				return fmt.Errorf("step %d inspects no receipt", index)
			}
			record := inspector.inspectReceipt(step.Receipt)
			if record == nil {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d receipt is absent; want present", index)
			}
			if step.WantResult != "" && record.result != step.WantResult {
				return fmt.Errorf("step %d receipt result = %q; want %q", index, record.result, step.WantResult)
			}
			if step.WantCompleted != nil && record.completed != *step.WantCompleted {
				return fmt.Errorf("step %d receipt completed = %v; want %v", index, record.completed, *step.WantCompleted)
			}
		case "inspect":
			inspector, ok := executor.(interface {
				inspectRow(seamScope, string, string) *seamRow
			})
			if !ok {
				return fmt.Errorf("step %d: executor cannot be inspected", index)
			}
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			row := inspector.inspectRow(*step.Scope, step.Table, step.ID)
			if row == nil {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d row %s/%s is absent; want present", index, step.Table, step.ID)
			}
			if step.WantAbsent {
				return fmt.Errorf("step %d row %s/%s is present; want absent", index, step.Table, step.ID)
			}
			if err := checkSeamRow(step, store.Row{Table: step.Table, Values: row.values}, true); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown receipt op %q", index, step.Op)
		}
	}
	return nil
}

func runDeadlineVector(vector seamVector, executor store.Executor) error {
	return runReceiptVector(vector, executor)
}

func checkSeamError(index int, step seamStep, err error) error {
	if step.WantError != "" {
		if err == nil {
			return fmt.Errorf("step %d succeeded; want error class %q", index, step.WantError)
		}
		class, ok := fault.ClassOf(err)
		if !ok || string(class) != step.WantError {
			return fmt.Errorf("step %d error = %v (class=%q); want %q", index, err, class, step.WantError)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("step %d: %w", index, err)
	}
	return nil
}

func checkSeamRow(step seamStep, row store.Row, found bool) error {
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
		version, ok := row.Values["version"].(uint64)
		if !ok {
			version = seamUint64(row.Values["version"])
		}
		if version != *step.WantVersion {
			return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, version, *step.WantVersion)
		}
	}
	for column, want := range step.WantValues {
		got := row.Values[column]
		if scannerCompareValues(got, want) != 0 {
			return fmt.Errorf("row %s/%s column %q = %v; want %v", step.Table, step.ID, column, got, want)
		}
	}
	return nil
}

// TestCredentialLookupExposesNoListing proves the lookup seam is exactly one
// method, so it cannot be used to enumerate credentials.
func TestCredentialLookupExposesNoListing(t *testing.T) {
	lookupType := reflect.TypeOf((*store.CredentialLookup)(nil)).Elem()
	if lookupType.NumMethod() != 1 || lookupType.Method(0).Name != "LookupCredential" {
		t.Fatalf("CredentialLookup method set = %v; want only LookupCredential", lookupType)
	}
}

// TestControllerMethodSet proves the controller seam has exactly the three
// declared methods and no generic unscoped query.
func TestControllerMethodSet(t *testing.T) {
	controllerType := reflect.TypeOf((*store.Controller)(nil)).Elem()
	names := make([]string, 0, controllerType.NumMethod())
	for index := 0; index < controllerType.NumMethod(); index++ {
		names = append(names, controllerType.Method(index).Name)
	}
	sort.Strings(names)
	want := []string{"Execute", "Read", "Scan"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Controller methods = %v; want %v", names, want)
	}
}

// TestDeadlineGuardValidate pins the shared declaration-level guard shape.
func TestDeadlineGuardValidate(t *testing.T) {
	if err := (store.DeadlineGuard{}).Validate(); err == nil {
		t.Fatal("an empty deadline guard column was accepted")
	}
	if err := (store.DeadlineGuard{Column: "deadline"}).Validate(); err != nil {
		t.Fatalf("a named deadline guard column was refused: %v", err)
	}
}

// TestReceiptModeValidate pins the declared receipt modes and the completion
// expectation shape.
func TestReceiptModeValidate(t *testing.T) {
	if !store.ReceiptInsert.Valid() || !store.ReceiptInsertIncomplete.Valid() || !store.ReceiptComplete.Valid() {
		t.Fatal("a declared receipt mode is not valid")
	}
	if store.ReceiptMode("overwrite").Valid() {
		t.Fatal("an unknown receipt mode was accepted")
	}
	if err := (store.ReceiptExpect{ContentHash: "sha256:select"}).Validate(); err == nil {
		t.Fatal("a malformed receipt content hash was accepted")
	}
	valid := strings.Repeat("a", 64)
	if err := (store.ReceiptExpect{ContentHash: "sha256:" + valid}).Validate(); err != nil {
		t.Fatalf("a valid receipt content hash was refused: %v", err)
	}
}

// TestUnitValidate pins the receipt-mode shape before an adapter starts a
// transaction: an incomplete insert may carry an in-progress result, while a
// completed insert still requires one and a completion unit still requires its
// receipt and compare-and-set expectation.
func TestUnitValidate(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	record := func(result []byte) *receipt.Record {
		return &receipt.Record{
			Key: receipt.Key{TenantID: "tenant-1", ProjectID: "project-1", PrincipalID: "service-1",
				Operation: "pipeline.push", CallerKey: "event-1"},
			ContentHash: valid,
			Result:      result,
		}
	}
	if err := (store.Unit{Receipt: record([]byte("selection-v1")), ReceiptMode: store.ReceiptInsertIncomplete}).Validate(); err != nil {
		t.Fatalf("an incomplete insert carrying an in-progress result was refused: %v", err)
	}
	if err := (store.Unit{Receipt: record(nil), ReceiptMode: store.ReceiptInsert}).Validate(); err == nil {
		t.Fatal("a completed insert without a result was accepted")
	}
	if err := (store.Unit{Receipt: record([]byte("done")), ReceiptMode: store.ReceiptInsert}).Validate(); err != nil {
		t.Fatalf("a completed insert with a result was refused: %v", err)
	}
	if err := (store.Unit{Receipt: record([]byte("done")), ReceiptMode: store.ReceiptInsert, ReceiptExpect: &store.ReceiptExpect{ContentHash: valid}}).Validate(); err == nil {
		t.Fatal("an insert-mode unit carrying a completion expectation was accepted")
	}
	if err := (store.Unit{Receipt: record([]byte("done")), ReceiptMode: store.ReceiptComplete}).Validate(); err == nil {
		t.Fatal("a completion unit without an expectation was accepted")
	}
	if err := (store.Unit{Receipt: record([]byte("done")), ReceiptMode: store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: valid}}).Validate(); err != nil {
		t.Fatalf("a valid completion unit was refused: %v", err)
	}
}
