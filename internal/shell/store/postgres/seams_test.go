// Adapted shared seam vectors (test/contracts/adapters/testdata/seams.json).
//
// The contract package's runners are test-only and not importable, so this file
// adapts the same JSON vectors and the same assertions to the PostgreSQL
// adapter over the in-process SQL emulator. It is a semantic check of the
// controller scope derivation, credential lookup, receipt completion and
// transaction-time deadline guard; it is not PostgreSQL isolation or RLS proof.
package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Range         []scanRangeJSON  `json:"range"`
	OrderBy       []scanOrderJSON  `json:"order_by"`
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

// TestSeamConformanceVectors runs every adapted seam vector against the
// PostgreSQL adapter over the emulator.
func TestSeamConformanceVectors(t *testing.T) {
	document := loadSeamVectors(t)
	validateSeamVectors(t, document)
	for _, vector := range document.Controller {
		t.Run("controller/"+vector.Name, func(t *testing.T) {
			harness := newSeamHarness(t, vector, "kodama_controller")
			if err := runControllerVector(vector, harness); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Credentials {
		t.Run("credentials/"+vector.Name, func(t *testing.T) {
			harness := newSeamHarness(t, vector, "kodama_auth")
			if err := runCredentialVector(vector, harness); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Receipts {
		t.Run("receipts/"+vector.Name, func(t *testing.T) {
			harness := newSeamHarness(t, vector, "kodama_runtime")
			if err := runReceiptVector(vector, harness); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.ReceiptInProgress {
		t.Run("receipt_in_progress/"+vector.Name, func(t *testing.T) {
			harness := newSeamHarness(t, vector, "kodama_runtime")
			if err := runReceiptVector(vector, harness); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Deadlines {
		t.Run("deadlines/"+vector.Name, func(t *testing.T) {
			harness := newSeamHarness(t, vector, "kodama_runtime")
			if err := runDeadlineVector(vector, harness); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
}

func loadSeamVectors(t *testing.T) seamDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "seams.json")
	data, err := os.ReadFile(path)
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
}

// seamHarness owns the emulator and the adapter seams under test.
type seamHarness struct {
	db          *emuDB
	controller  *Controller
	credentials *Credentials
	executor    *Store
	now         time.Time
}

func newSeamHarness(t *testing.T, vector seamVector, role string) *seamHarness {
	t.Helper()
	now := seamClock(vector.Clock)
	db := newEmuDB(role, now)
	seedEmulator(t, db, vector.Seed, now)
	return &seamHarness{
		db:          db,
		controller:  NewController(db),
		credentials: NewCredentialLookup(db),
		executor:    New(db),
		now:         now,
	}
}

func seedEmulator(t *testing.T, db *emuDB, seed seamSeed, now time.Time) {
	t.Helper()
	for _, row := range seed.Rows {
		values := make(map[string]any, len(row.Values)+4)
		for column, value := range row.Values {
			values[column] = normalizeSeamValue(row.Table, column, value)
		}
		values["tenant_id"] = row.TenantID
		values["project_id"] = seamProjectValue(row.Table, row.ProjectID)
		values["id"] = row.ID
		values["version"] = int64(row.Version)
		fillSeamColumns(row.Table, values, now)
		db.seed(row.Table, values)
	}
	for index, record := range seed.Receipts {
		values := map[string]any{
			"id":             fmt.Sprintf("receipt-%d", index+1),
			"tenant_id":      record.TenantID,
			"project_id":     seamProjectValue("receipts", record.ProjectID),
			"principal_id":   record.PrincipalID,
			"operation":      record.Operation,
			"caller_key":     record.CallerKey,
			"content_sha256": seamHashBytes(t, record.ContentHash),
			"created_at":     now,
		}
		if record.Result != "" {
			values["result"] = []byte(record.Result)
		}
		if record.ExecutionID != "" {
			values["execution_id"] = record.ExecutionID
		}
		if record.Completed {
			values["completed_at"] = now
		}
		db.seed("receipts", values)
	}
}

func seamProjectValue(table, project string) any {
	if project != "" {
		return project
	}
	meta, ok := lookupTable(table)
	if !ok {
		return nil
	}
	if column, ok := meta.columns["project_id"]; ok && !column.Nullable {
		return ""
	}
	return nil
}

func seamHashBytes(t *testing.T, fingerprint string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(strings.TrimPrefix(fingerprint, "sha256:"))
	if err != nil || len(decoded) != 32 {
		t.Fatalf("invalid vector content hash %q", fingerprint)
	}
	return decoded
}

func normalizeSeamValue(table, column string, value any) any {
	meta, ok := lookupTable(table)
	if !ok {
		return value
	}
	definition, ok := meta.columns[column]
	if !ok {
		return value
	}
	switch definition.Type {
	case "timestamptz":
		if text, ok := value.(string); ok {
			if parsed, err := time.Parse(time.RFC3339, text); err == nil {
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

// fillSeamColumns gives every missing non-nullable column its zero value so the
// adapter's typed scan sees a complete row. A vector only carries the columns it
// asserts.
func fillSeamColumns(table string, values map[string]any, now time.Time) {
	meta, ok := lookupTable(table)
	if !ok {
		return
	}
	for _, column := range meta.table.Columns {
		if _, present := values[column.Name]; present {
			continue
		}
		if column.Nullable {
			continue
		}
		switch column.Type {
		case "text":
			values[column.Name] = ""
		case "bigint", "integer":
			values[column.Name] = int64(0)
		case "boolean":
			values[column.Name] = false
		case "jsonb":
			values[column.Name] = []byte("{}")
		case "bytea":
			values[column.Name] = []byte{}
		case "timestamptz":
			values[column.Name] = now
		}
	}
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

func runControllerVector(vector seamVector, harness *seamHarness) error {
	for index, step := range vector.Steps {
		switch step.Op {
		case "read":
			row, err := harness.controller.Read(context.Background(), step.Table, step.ID)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" {
				if err := checkSeamRow(step, row, true); err != nil {
					return fmt.Errorf("step %d: %w", index, err)
				}
			}
		case "scan":
			scan := store.ControllerScan{
				Table: step.Table, Equals: step.Equals, Range: scannerRangeFilters(step.Range),
				OrderBy: scannerOrderTerms(step.OrderBy), Limit: step.Limit}
			page, err := harness.controller.Scan(context.Background(), scan)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError == "" {
				got := make([]string, 0, len(page.Rows))
				for _, row := range page.Rows {
					got = append(got, emuString(row.Values["id"]))
				}
				if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
					return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
				}
			}
		case "execute":
			unit, err := seamUnitFromStep(step, harness.now)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			result, err := harness.controller.Execute(context.Background(), step.ExecutionID, unit)
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
			row := harness.inspectRow(*step.Scope, step.Table, step.ID)
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

func runCredentialVector(vector seamVector, harness *seamHarness) error {
	for index, step := range vector.Steps {
		if step.Op != "lookup" {
			return fmt.Errorf("step %d has unknown credential op %q", index, step.Op)
		}
		candidate, err := harness.credentials.LookupCredential(context.Background(), store.CredentialKind(step.Kind), step.ID)
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

func runReceiptVector(vector seamVector, harness *seamHarness) error {
	for index, step := range vector.Steps {
		switch step.Op {
		case "apply":
			if step.Scope == nil {
				return fmt.Errorf("step %d applies without a scope", index)
			}
			unit, err := seamUnitFromStep(step, harness.now)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			result, err := harness.executor.Execute(context.Background(), seamScopeOf(*step.Scope), unit)
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
			if step.Receipt == nil {
				return fmt.Errorf("step %d inspects no receipt", index)
			}
			record := harness.inspectReceipt(*step.Receipt)
			if record == nil {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d receipt is absent; want present", index)
			}
			if step.WantResult != "" && emuString(record.values["result"]) != step.WantResult {
				return fmt.Errorf("step %d receipt result = %q; want %q", index, emuString(record.values["result"]), step.WantResult)
			}
			completed := record.values["completed_at"] != nil
			if step.WantCompleted != nil && completed != *step.WantCompleted {
				return fmt.Errorf("step %d receipt completed = %v; want %v", index, completed, *step.WantCompleted)
			}
		case "inspect":
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			row := harness.inspectRow(*step.Scope, step.Table, step.ID)
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

func runDeadlineVector(vector seamVector, harness *seamHarness) error {
	return runReceiptVector(vector, harness)
}

func (harness *seamHarness) inspectRow(scope seamScope, table, rowID string) *emuRow {
	harness.db.mu.Lock()
	defer harness.db.mu.Unlock()
	for _, row := range harness.db.rows[table] {
		if emuString(row.values["id"]) != rowID {
			continue
		}
		project, _ := row.values["project_id"].(string)
		if emuString(row.values["tenant_id"]) != scope.TenantID || project != scope.ProjectID {
			continue
		}
		return row
	}
	return nil
}

func (harness *seamHarness) inspectReceipt(record seamSeedReceipt) *emuRow {
	harness.db.mu.Lock()
	defer harness.db.mu.Unlock()
	for _, row := range harness.db.rows["receipts"] {
		if emuString(row.values["tenant_id"]) != record.TenantID ||
			emuString(row.values["project_id"]) != record.ProjectID ||
			emuString(row.values["principal_id"]) != record.PrincipalID ||
			emuString(row.values["operation"]) != record.Operation ||
			emuString(row.values["caller_key"]) != record.CallerKey {
			continue
		}
		return row
	}
	return nil
}

// seamLocks converts the vector locks of one step into store locks. The seam
// and controller vector runners share it.
func seamLocks(vector []seamLock) []store.Lock {
	var locks []store.Lock
	for _, lock := range vector {
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
		locks = append(locks, converted)
	}
	return locks
}

// seamWrites converts the vector writes of one step into store writes. The
// seam and controller vector runners share it.
func seamWrites(vector []seamWrite) []store.Write {
	var writes []store.Write
	for _, write := range vector {
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
		writes = append(writes, converted)
	}
	return writes
}

func seamUnitFromStep(step seamStep, now time.Time) (store.Unit, error) {
	unit := store.Unit{}
	if step.Unit == nil {
		return unit, nil
	}
	unit.ReceiptMode = store.ReceiptMode(step.Unit.ReceiptMode)
	unit.Locks = seamLocks(step.Unit.Locks)
	unit.Writes = seamWrites(step.Unit.Writes)
	if step.Unit.Receipt != nil {
		seed := *step.Unit.Receipt
		record := receipt.Record{
			ID: "receipt-vector",
			Key: receipt.Key{
				TenantID:    id.TenantID(seed.TenantID),
				ProjectID:   id.ProjectID(seed.ProjectID),
				PrincipalID: id.PrincipalID(seed.PrincipalID),
				Operation:   seed.Operation,
				CallerKey:   seed.CallerKey,
			},
			ContentHash: seed.ContentHash,
			ExecutionID: id.ExecutionID(seed.ExecutionID),
			At:          now,
		}
		if seed.Result != "" {
			record.Result = []byte(seed.Result)
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
		if version := emuInt(row.Values["version"]); version != int64(*step.WantVersion) {
			return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, version, *step.WantVersion)
		}
	}
	for column, want := range step.WantValues {
		if emuCompare(row.Values[column], want) != 0 {
			return fmt.Errorf("row %s/%s column %q = %v; want %v", step.Table, step.ID, column, row.Values[column], want)
		}
	}
	return nil
}

// scannerRangeFilters and scannerOrderTerms are the tiny adapters the vector
// runner needs; they mirror the contract test's conversion.
func scannerRangeFilters(ranges []scanRangeJSON) []store.RangeFilter {
	filters := make([]store.RangeFilter, 0, len(ranges))
	for _, filter := range ranges {
		filters = append(filters, store.RangeFilter{Column: filter.Column, After: filter.After, Before: filter.Before})
	}
	return filters
}

func scannerOrderTerms(orders []scanOrderJSON) []store.Order {
	terms := make([]store.Order, 0, len(orders))
	for _, order := range orders {
		terms = append(terms, store.Order{Column: order.Column, Desc: order.Desc})
	}
	return terms
}
