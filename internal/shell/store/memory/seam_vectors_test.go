// Seam conformance: the adapter seams.json vectors executed against the
// memory adapter. The contract package's reference interpreter and runners
// are test-only code in another package, so this file copies and adapts them:
// controller vectors run against Store.Controller(), credential vectors against
// Store.LookupCredential, and receipt/deadline vectors against Store.Execute with
// the vector's injected clock. The JSON document is never edited.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Op            string            `json:"op"`
	Table         string            `json:"table"`
	ID            string            `json:"id"`
	ExecutionID   string            `json:"execution_id"`
	Kind          string            `json:"kind"`
	Equals        map[string]any    `json:"equals"`
	Range         []scanVectorRange `json:"range"`
	OrderBy       []scanVectorOrder `json:"order_by"`
	Limit         int               `json:"limit"`
	Scope         *seamScope        `json:"scope"`
	Unit          *seamUnit         `json:"unit"`
	Receipt       *seamSeedReceipt  `json:"receipt"`
	WantValues    map[string]any    `json:"want_values"`
	WantVersion   *uint64           `json:"want_version"`
	WantAbsent    bool              `json:"want_absent"`
	WantIDs       []string          `json:"want_ids"`
	WantResult    string            `json:"want_result"`
	WantReplayed  *bool             `json:"want_replayed"`
	WantCompleted *bool             `json:"want_completed"`
	WantCandidate *seamScope        `json:"want_candidate"`
	WantError     string            `json:"want_error"`
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

// TestSeamVectorsAgainstMemory executes every declared seam vector against the
// memory adapter.
func TestSeamVectorsAgainstMemory(t *testing.T) {
	document := loadSeamVectors(t)
	if document.Version != 1 {
		t.Fatalf("seam vector document version = %d; want 1", document.Version)
	}
	if len(document.Controller) == 0 || len(document.Credentials) == 0 ||
		len(document.Receipts) == 0 || len(document.ReceiptInProgress) == 0 ||
		len(document.Deadlines) == 0 {
		t.Fatal("seam conformance vector sections must all be non-empty")
	}
	for _, vector := range document.Controller {
		t.Run("controller/"+vector.Name, func(t *testing.T) {
			s := newSeamStore(t, vector)
			if err := runSeamControllerVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Credentials {
		t.Run("credentials/"+vector.Name, func(t *testing.T) {
			s := newSeamStore(t, vector)
			if err := runSeamCredentialVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Receipts {
		t.Run("receipts/"+vector.Name, func(t *testing.T) {
			s := newSeamStore(t, vector)
			if err := runSeamReceiptVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.ReceiptInProgress {
		t.Run("receipt_in_progress/"+vector.Name, func(t *testing.T) {
			s := newSeamStore(t, vector)
			if err := runSeamReceiptVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
	for _, vector := range document.Deadlines {
		t.Run("deadlines/"+vector.Name, func(t *testing.T) {
			s := newSeamStore(t, vector)
			if err := runSeamReceiptVector(t, s, vector); err != nil {
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

// newSeamStore seeds one vector into a fresh store with the vector's injected
// clock.
func newSeamStore(t *testing.T, vector seamVector) *Store {
	t.Helper()
	s := NewWithClock(func() time.Time { return seamClock(vector.Clock) })
	seedSeamStore(t, s, vector.Seed)
	return s
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

func seedSeamStore(t *testing.T, s *Store, seed seamSeed) {
	t.Helper()
	for _, row := range seed.Rows {
		if row.Table == "events" {
			seedSeamEvent(t, s, row)
			continue
		}
		scope := scopeFor(row.TenantID, row.ProjectID)
		switch row.Table {
		case "tenants":
			scope = scopeFor(row.ID, "")
		case "projects":
			scope = scopeFor(row.TenantID, row.ID)
		}
		values := scanSeedValues(t, row.Table, row.Values)
		if column, ok := vectorVersionColumn(row.Table); ok {
			version := row.Version
			if version == 0 {
				version = 1
			}
			values[column] = int64(version)
		}
		mustExecute(t, s, scope, store.Unit{Writes: []store.Write{{
			Table: row.Table, ID: row.ID, Insert: true, Values: values,
		}}})
	}
	for _, seeded := range seed.Receipts {
		key := seamKeyOf(seeded)
		record := receipt.Record{
			ID:          seamReceiptIDOf(key),
			Key:         key,
			ContentHash: seeded.ContentHash,
			Result:      []byte(seeded.Result),
			ExecutionID: id.ExecutionID(seeded.ExecutionID),
			At:          fixedTime(),
		}
		mode := store.ReceiptInsert
		if !seeded.Completed {
			// An incomplete receipt may already carry the in-progress result
			// (the trigger Select's persisted selection); seed it so a replay
			// and a later completion exercise the stored payload.
			mode = store.ReceiptInsertIncomplete
		}
		mustExecute(t, s, scopeFor(seeded.TenantID, seeded.ProjectID), store.Unit{
			Receipt:     &record,
			ReceiptMode: mode,
		})
	}
}

// seedSeamEvent commits one event publication record with its explicit durable
// sequence and publish state. Events are never generic business writes.
func seedSeamEvent(t *testing.T, s *Store, row seamSeedRow) {
	t.Helper()
	scope := scopeFor(row.TenantID, row.ProjectID)
	sequence := uint64(0)
	if raw, ok := row.Values["publish_sequence"]; ok {
		if value, ok := seamUint64Value(raw); ok {
			sequence = value
		}
	}
	published := false
	if raw, ok := row.Values["publish_state"].(string); ok && raw == "published" {
		published = true
	}
	eventType, _ := row.Values["type"].(string)
	envelope := event.Envelope{
		ID:        id.EventID(row.ID),
		Type:      event.Type(eventType),
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID(testPrincipalID)},
		At:        fixedTime(),
		Scope:     scope,
		SubjectID: row.ID,
	}
	s.mu.Lock()
	if sequence == 0 {
		s.state.sequence++
		sequence = s.state.sequence
	} else if sequence > s.state.sequence {
		s.state.sequence = sequence
	}
	s.state.events = append(s.state.events, CommittedEvent{
		Sequence:  sequence,
		Envelope:  envelope,
		Published: published,
	})
	s.mu.Unlock()
}

func seamUint64Value(value any) (uint64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case uint64:
		return typed, true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	default:
		return 0, false
	}
}

func seamKeyOf(seeded seamSeedReceipt) receipt.Key {
	return receipt.Key{
		TenantID:    id.TenantID(seeded.TenantID),
		ProjectID:   id.ProjectID(seeded.ProjectID),
		PrincipalID: id.PrincipalID(seeded.PrincipalID),
		Operation:   seeded.Operation,
		CallerKey:   seeded.CallerKey,
	}
}

// seamReceiptIDOf derives one stable receipt row identity from its key so a
// seeded receipt and its later completion unit address the same durable row.
func seamReceiptIDOf(key receipt.Key) id.ReceiptID {
	identity := string(key.TenantID) + "|" + string(key.ProjectID) + "|" + string(key.PrincipalID) + "|" + key.Operation + "|" + key.CallerKey
	sum := sha256.Sum256([]byte(identity))
	return id.ReceiptID("receipt-" + hex.EncodeToString(sum[:8]))
}

func seamUnitFromStep(step seamStep) (store.Unit, error) {
	unit := store.Unit{}
	if step.Unit == nil {
		return unit, nil
	}
	unit.ReceiptMode = store.ReceiptMode(step.Unit.ReceiptMode)
	locks, err := seamLocks(step.Unit.Locks)
	if err != nil {
		return store.Unit{}, err
	}
	unit.Locks = locks
	writes, err := seamWrites(step.Unit.Writes)
	if err != nil {
		return store.Unit{}, err
	}
	unit.Writes = writes
	if step.Unit.Receipt != nil {
		key := seamKeyOf(*step.Unit.Receipt)
		record := receipt.Record{
			ID:          seamReceiptIDOf(key),
			Key:         key,
			ContentHash: step.Unit.Receipt.ContentHash,
			Result:      []byte(step.Unit.Receipt.Result),
			ExecutionID: id.ExecutionID(step.Unit.Receipt.ExecutionID),
			At:          fixedTime(),
		}
		unit.Receipt = &record
	}
	if step.Unit.ReceiptExpect != nil {
		unit.ReceiptExpect = &store.ReceiptExpect{
			Completed:   step.Unit.ReceiptExpect.Completed,
			ContentHash: step.Unit.ReceiptExpect.ContentHash,
		}
	}
	for index := 0; index < step.Unit.Audit; index++ {
		unit.Audit = append(unit.Audit, event.Audit{})
	}
	for index := 0; index < step.Unit.Events; index++ {
		unit.Events = append(unit.Events, event.Envelope{})
	}
	return unit, nil
}

// seamLocks converts the vector locks of one step into store locks with typed
// guard values. The seam and controller vector runners share it.
func seamLocks(vector []seamLock) ([]store.Lock, error) {
	var locks []store.Lock
	for _, lock := range vector {
		expect, err := seamNormalizeValues(lock.Table, lock.Expect)
		if err != nil {
			return nil, err
		}
		converted := store.Lock{
			Table:         lock.Table,
			IDs:           append([]string(nil), lock.IDs...),
			ExpectVersion: lock.ExpectVersion,
			Expect:        expect,
			TenantWide:    lock.TenantWide,
		}
		if lock.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: lock.Deadline}
		}
		locks = append(locks, converted)
	}
	return locks, nil
}

// seamWrites converts the vector writes of one step into store writes with
// typed values and guards. The seam and controller vector runners share it.
func seamWrites(vector []seamWrite) ([]store.Write, error) {
	var writes []store.Write
	for _, write := range vector {
		values, err := seamNormalizeValues(write.Table, write.Values)
		if err != nil {
			return nil, err
		}
		expect, err := seamNormalizeValues(write.Table, write.Expect)
		if err != nil {
			return nil, err
		}
		converted := store.Write{
			Table:         write.Table,
			ID:            write.ID,
			Insert:        write.Insert,
			Delete:        write.Delete,
			ExpectVersion: write.ExpectVersion,
			Expect:        expect,
			Values:        values,
		}
		if write.Deadline != "" {
			converted.Deadline = &store.DeadlineGuard{Column: write.Deadline}
		}
		writes = append(writes, converted)
	}
	return writes, nil
}

// seamNormalizeValues converts the JSON-decoded vector values into the typed
// shapes the memory adapter stores (integers as int64, timestamps as time.Time,
// bytea/jsonb as []byte), so the vector exercises the same write path a caller
// does.
func seamNormalizeValues(table string, raw map[string]any) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	meta, ok := lookupTable(table)
	if !ok {
		return nil, fmt.Errorf("write names unknown table %q", table)
	}
	values := make(map[string]any, len(raw))
	for name, value := range raw {
		column, ok := meta.columns[name]
		if !ok {
			return nil, fmt.Errorf("write names undeclared column %q of %q", name, table)
		}
		normalized, err := scanSeedValue(column, value)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", table, name, err)
		}
		values[name] = normalized
	}
	return values, nil
}

func runSeamControllerVector(t *testing.T, s *Store, vector seamVector) error {
	t.Helper()
	controller := s.Controller()
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
			scan := store.ControllerScan{
				Table:   step.Table,
				Equals:  step.Equals,
				Range:   scanVectorRangeFilters(step.Range),
				OrderBy: scanVectorOrderTerms(step.OrderBy),
				Limit:   step.Limit}
			page, err := controller.Scan(context.Background(), scan)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError != "" {
				continue
			}
			got := make([]string, 0, len(page.Rows))
			for _, row := range page.Rows {
				identity, _ := row.Values["id"].(string)
				got = append(got, identity)
			}
			if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
				return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
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
			row, found := s.Snapshot(seamScopeOf(*step.Scope), step.Table, step.ID)
			if err := checkSeamRow(step, row, found); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown controller op %q", index, step.Op)
		}
	}
	return nil
}

func runSeamCredentialVector(t *testing.T, s *Store, vector seamVector) error {
	t.Helper()
	for index, step := range vector.Steps {
		if step.Op != "lookup" {
			return fmt.Errorf("step %d has unknown credential op %q", index, step.Op)
		}
		candidate, err := s.LookupCredential(context.Background(), store.CredentialKind(step.Kind), step.ID)
		if err := checkSeamError(index, step, err); err != nil {
			return err
		}
		if step.WantError != "" {
			continue
		}
		if step.WantCandidate == nil {
			return fmt.Errorf("step %d asserts no candidate", index)
		}
		got := seamScope{
			TenantID:    string(candidate.TenantID),
			ProjectID:   string(candidate.ProjectID),
			PrincipalID: string(candidate.PrincipalID),
		}
		if got != *step.WantCandidate {
			return fmt.Errorf("step %d candidate = %+v; want %+v", index, got, *step.WantCandidate)
		}
	}
	return nil
}

func runSeamReceiptVector(t *testing.T, s *Store, vector seamVector) error {
	t.Helper()
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
			result, err := s.Execute(context.Background(), seamScopeOf(*step.Scope), unit)
			if err := checkSeamError(index, step, err); err != nil {
				return err
			}
			if step.WantError != "" {
				continue
			}
			if step.WantReplayed != nil && result.Replayed != *step.WantReplayed {
				return fmt.Errorf("step %d Replayed = %v; want %v", index, result.Replayed, *step.WantReplayed)
			}
			if step.WantResult != "" && string(result.Receipt.Result) != step.WantResult {
				return fmt.Errorf("step %d result = %q; want %q", index, result.Receipt.Result, step.WantResult)
			}
		case "inspect_receipt":
			if step.Receipt == nil {
				return fmt.Errorf("step %d inspects no receipt", index)
			}
			key := seamKeyOf(*step.Receipt)
			record, found, completed := seamInspectReceipt(s, key)
			if !found {
				if step.WantAbsent {
					continue
				}
				return fmt.Errorf("step %d receipt is absent; want present", index)
			}
			if step.WantResult != "" && string(record.Result) != step.WantResult {
				return fmt.Errorf("step %d receipt result = %q; want %q", index, record.Result, step.WantResult)
			}
			if step.WantCompleted != nil && completed != *step.WantCompleted {
				return fmt.Errorf("step %d receipt completed = %v; want %v", index, completed, *step.WantCompleted)
			}
		case "inspect":
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			row, found := s.Snapshot(seamScopeOf(*step.Scope), step.Table, step.ID)
			if err := checkSeamRow(step, row, found); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown receipt op %q", index, step.Op)
		}
	}
	return nil
}

func seamInspectReceipt(s *Store, key receipt.Key) (receipt.Record, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.state.receipts[key]
	return record, found, s.state.receiptCompleted[key]
}

func seamScopeOf(scope seamScope) tenancy.Scope {
	return scopeFor(scope.TenantID, scope.ProjectID)
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
		version, ok := asUint64(row.Values["version"])
		if !ok {
			return fmt.Errorf("row %s/%s has no usable version", step.Table, step.ID)
		}
		if version != *step.WantVersion {
			return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, version, *step.WantVersion)
		}
	}
	meta, ok := lookupTable(step.Table)
	if !ok {
		return fmt.Errorf("row table %q is not in the schema", step.Table)
	}
	for column, want := range step.WantValues {
		definition, ok := meta.columns[column]
		if !ok {
			return fmt.Errorf("row %s/%s wants undeclared column %q", step.Table, step.ID, column)
		}
		if compareScanValues(definition, row.Values[column], want) != 0 {
			return fmt.Errorf("row %s/%s column %q = %v; want %v", step.Table, step.ID, column, row.Values[column], want)
		}
	}
	return nil
}

// TestControllerExecuteSerializesOneActiveAction asserts the controller seam
// derives the owning scope and applies one unit atomically under concurrent
// callers: exactly one of two conflicting action transitions wins and the loser
// observes the winner's version, never a partial row.
func TestControllerExecuteSerializesOneActiveAction(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "running"))
	seedRow(t, s, scope, "actions", testActionID, actionValues(1, "running"))
	controller := s.Controller()

	errs := runConcurrently(t, 2, func(index int) error {
		_, err := controller.Execute(context.Background(), testExecutionID, store.Unit{
			Writes: []store.Write{{
				Table: "actions", ID: testActionID, ExpectVersion: 1,
				Values: map[string]any{"state": "succeeded"},
			}},
		})
		return err
	})
	if countNil(errs) != 1 {
		t.Fatalf("successful controller units = %d; want exactly 1 (%v)", countNil(errs), describeErrors(errs))
	}
	if countFault(t, errs, fault.VersionConflict) != 1 {
		t.Fatalf("conflicting controller units = %d; want exactly 1 (%v)", countFault(t, errs, fault.VersionConflict), describeErrors(errs))
	}
	action := mustSnapshot(t, s, scope, "actions", testActionID)
	if got := rowString(t, action, "state"); got != "succeeded" {
		t.Fatalf("action state = %q; want succeeded", got)
	}
	if got := rowVersion(t, action, "version"); got != 2 {
		t.Fatalf("action version = %d; want 2", got)
	}
}

// TestControllerExecuteRefusesACrossScopeRowWithoutPartialEffect asserts a
// controller unit that names another project's row is refused and changes
// nothing.
func TestControllerExecuteRefusesACrossScopeRowWithoutPartialEffect(t *testing.T) {
	s := New()
	seedRow(t, s, projectScope(), "executions", testExecutionID, executionValues(1, "running"))
	foreignAction := actionValues(1, "running")
	foreignAction["execution_id"] = "execution-foreign"
	seedRow(t, s, otherProjectScope(), "actions", "action-2", foreignAction)

	_, err := s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
		Writes: []store.Write{
			{Table: "actions", ID: "action-1", Insert: true, Values: actionValues(1, "pending")},
			{Table: "actions", ID: "action-2", ExpectVersion: 1, Values: map[string]any{"state": "succeeded"}},
		},
	})
	assertFault(t, err, fault.Forbidden)
	assertAbsent(t, s, projectScope(), "actions", "action-1")
	foreign := mustSnapshot(t, s, otherProjectScope(), "actions", "action-2")
	if got := rowString(t, foreign, "state"); got != "running" {
		t.Fatalf("foreign action state = %q; want running", got)
	}
}

// TestReceiptCompletionIsACompareAndSetUnderRace asserts two concurrent
// completers of one incomplete receipt cannot both apply: the second replays the
// first durable result and leaves the stored completion marker intact.
func TestReceiptCompletionIsACompareAndSetUnderRace(t *testing.T) {
	s := New()
	scope := projectScope()
	key := keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-1")
	contentHash := fingerprintOf(t, `{"event":"event-1"}`)
	incomplete := receipt.Record{
		ID:          seamReceiptIDOf(key),
		Key:         key,
		ContentHash: contentHash,
		At:          fixedTime(),
	}
	mustExecute(t, s, scope, store.Unit{Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete})

	results := make([]store.UnitResult, 2)
	errs := runConcurrently(t, 2, func(index int) error {
		record := incomplete
		record.Result = []byte(fmt.Sprintf("result-%d", index))
		result, err := s.Execute(context.Background(), scope, store.Unit{
			Receipt:       &record,
			ReceiptMode:   store.ReceiptComplete,
			ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash},
		})
		results[index] = result
		return err
	})
	if countNil(errs) != 2 {
		t.Fatalf("completer errors = %v; want both to settle (one applies, one replays)", describeErrors(errs))
	}
	replayed := 0
	applied := ""
	for index, result := range results {
		if result.Replayed {
			replayed++
			continue
		}
		applied = string(result.Receipt.Result)
		_ = index
	}
	if replayed != 1 {
		t.Fatalf("replayed completions = %d; want exactly 1", replayed)
	}
	stored, found, completed := seamInspectReceipt(s, key)
	if !found || !completed {
		t.Fatalf("receipt found=%v completed=%v; want a completed row", found, completed)
	}
	if string(stored.Result) != applied {
		t.Fatalf("stored result = %q; want the first applied result %q", stored.Result, applied)
	}
}

// TestCredentialLookupExposesNoTokenMaterial asserts the lookup returns only the
// candidate scope and never token material from the credential row.
func TestCredentialLookupExposesNoTokenMaterial(t *testing.T) {
	s := New()
	scope := projectScope()
	values := executionValues(1, "running")
	values["token_nonce"] = make([]byte, 32)
	values["token_expires_at"] = fixedTime().Add(time.Hour)
	seedRow(t, s, scope, "executions", testExecutionID, values)

	candidate, err := s.LookupCredential(context.Background(), store.CredentialExecution, testExecutionID)
	if err != nil {
		t.Fatalf("LookupCredential: %v", err)
	}
	if candidate != (store.CandidateScope{
		TenantID:    id.TenantID(testTenantID),
		ProjectID:   id.ProjectID(testProjectID),
		PrincipalID: id.PrincipalID(testPrincipalID),
	}) {
		t.Fatalf("candidate = %+v; want only the scope/principal", candidate)
	}
}
