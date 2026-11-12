package adaptercontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

// The store conformance vectors are executable: each one carries seed state,
// ordered steps with explicit versions/keys/scopes, injected failure or
// conflict points, and exact expected post-state or error class. The reference
// interpreter below is deliberately trivial; it proves the vectors describe a
// real atomic unit, not labels. A deliberately non-atomic fake must fail at
// least one vector.

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

var storeVectorKinds = map[string]bool{
	"rollback":         true,
	"conflict":         true,
	"receipt_identity": true,
	"atomicity":        true,
	"scope":            true,
	"ownership":        true,
}

var storeVectorOps = map[string]bool{
	"begin":           true,
	"write":           true,
	"receipt":         true,
	"commit":          true,
	"rollback":        true,
	"fail":            true,
	"read":            true,
	"inspect":         true,
	"inspect_receipt": true,
}

var storeVectorErrors = map[string]bool{
	"injected_failure":            true,
	"version_conflict":            true,
	"changed_idempotency_content": true,
	"cross_scope":                 true,
	"no_transaction":              true,
	"not_found":                   true,
	"invalid_scope":               true,
	"invalid_table":               true,
}

type vectorError struct {
	class string
}

func (err vectorError) Error() string {
	return err.class
}

func vectorErrorClass(err error) string {
	var classed vectorError
	if errors.As(err, &classed) {
		return classed.class
	}
	return "unclassified"
}

// TestStoreConformanceVectors loads, validates and runs the executable store
// vectors against an atomic reference interpreter. It also proves the vectors
// are not vacuous: the same steps must fail against a non-atomic fake.
func TestStoreConformanceVectors(t *testing.T) {
	document := loadStoreVectors(t)
	validateStoreVectors(t, document)

	schema := store.SchemaDesign()
	tables := make(map[string]bool, len(schema.Tables))
	for _, table := range schema.Tables {
		tables[table.Name] = true
	}

	nonAtomicFailures := 0
	for _, vector := range document.Vectors {
		if err := executeStoreVector(vector, newReferenceVectorStore(tables, vector.Seed)); err != nil {
			t.Errorf("atomic reference interpreter failed vector %q: %v", vector.Name, err)
		}
		if err := executeStoreVector(vector, newNonAtomicVectorStore(tables, vector.Seed)); err != nil {
			nonAtomicFailures++
		}
	}
	if nonAtomicFailures == 0 {
		t.Fatal("no conformance vector failed against the deliberately non-atomic fake")
	}
}

func loadStoreVectors(t *testing.T) storeVectorDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "store.json"))
	if err != nil {
		t.Fatalf("read store conformance vectors: %v", err)
	}
	var document storeVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode store conformance vectors: %v", err)
	}
	return document
}

func validateStoreVectors(t *testing.T, document storeVectorDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("store vector document version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("store conformance vector set is empty")
	}
	names := make(map[string]bool, len(document.Vectors))
	kinds := make(map[string]int, len(storeVectorKinds))
	for _, vector := range document.Vectors {
		if vector.Name == "" || vector.Kind == "" {
			t.Errorf("incomplete conformance vector %+v", vector)
			continue
		}
		if names[vector.Name] {
			t.Errorf("duplicate conformance vector name %q", vector.Name)
		}
		names[vector.Name] = true
		if !storeVectorKinds[vector.Kind] {
			t.Errorf("vector %q has unknown kind %q", vector.Name, vector.Kind)
		}
		kinds[vector.Kind]++
		if len(vector.Steps) == 0 {
			t.Errorf("vector %q has no steps", vector.Name)
			continue
		}
		if vector.Steps[0].Op != "begin" {
			t.Errorf("vector %q does not begin with a scoped transaction", vector.Name)
		}
		terminal := false
		inspected := false
		writes := 0
		aborted := false
		for index, step := range vector.Steps {
			if !storeVectorOps[step.Op] {
				t.Errorf("vector %q step %d has unknown op %q", vector.Name, index, step.Op)
				continue
			}
			switch step.Op {
			case "begin":
				if step.Scope == nil {
					t.Errorf("vector %q step %d begins without a scope", vector.Name, index)
				}
			case "write":
				writes++
				if step.Table == "" || step.ID == "" {
					t.Errorf("vector %q step %d writes without a table/id", vector.Name, index)
				}
			case "receipt":
				if step.Key == nil {
					t.Errorf("vector %q step %d has no receipt key", vector.Name, index)
				}
			case "read", "inspect":
				if step.Table == "" || step.ID == "" {
					t.Errorf("vector %q step %d reads without a table/id", vector.Name, index)
				}
				inspected = true
			case "inspect_receipt":
				if step.Key == nil {
					t.Errorf("vector %q step %d has no receipt key", vector.Name, index)
				}
				inspected = true
			case "commit", "rollback", "fail":
				terminal = true
			}
			if step.WantError != "" {
				aborted = true
				if !storeVectorErrors[step.WantError] {
					t.Errorf("vector %q step %d expects unknown error class %q", vector.Name, index, step.WantError)
				}
			}
		}
		if !terminal && !aborted {
			t.Errorf("vector %q has no commit, rollback or injected failure", vector.Name)
		}
		if !inspected {
			t.Errorf("vector %q never inspects post-state", vector.Name)
		}
		if vector.Kind == "rollback" && writes < 2 {
			t.Errorf("rollback vector %q does not write a multi-record unit", vector.Name)
		}
	}

	required := []string{"rollback", "conflict", "receipt_identity", "atomicity", "scope", "ownership"}
	for _, kind := range required {
		if kinds[kind] == 0 {
			t.Errorf("conformance vectors are missing kind %q", kind)
		}
	}
	if !hasStoreStep(document, func(step storeVectorStep) bool { return step.WantError == "version_conflict" }) {
		t.Error("conformance vectors do not inject a stale-version conflict")
	}
	if !hasStoreStep(document, func(step storeVectorStep) bool { return step.WantResult != "" }) {
		t.Error("receipt vectors do not require the original result on equal content")
	}
	if !hasStoreStep(document, func(step storeVectorStep) bool { return step.WantError == "changed_idempotency_content" }) {
		t.Error("receipt vectors do not require a changed-content conflict")
	}
	if !hasStoreStep(document, func(step storeVectorStep) bool { return step.WantError == "no_transaction" }) {
		t.Error("scope vectors do not prove the transaction-local scope resets")
	}
	if !hasStoreStep(document, func(step storeVectorStep) bool { return step.WantError == "cross_scope" }) {
		t.Error("ownership vectors do not refuse a cross-scope reference")
	}
}

func hasStoreStep(document storeVectorDocument, match func(storeVectorStep) bool) bool {
	for _, vector := range document.Vectors {
		for _, step := range vector.Steps {
			if match(step) {
				return true
			}
		}
	}
	return false
}

func executeStoreVector(vector storeVector, adapter vectorStore) error {
	for _, step := range vector.Steps {
		var err error
		switch step.Op {
		case "begin":
			err = adapter.Begin(*step.Scope)
		case "write":
			err = adapter.Write(step)
		case "receipt":
			var result string
			result, err = adapter.Receipt(*step.Key, step.Content, step.Result)
			if err == nil && step.WantResult != "" && result != step.WantResult {
				return fmt.Errorf("step %s: receipt result = %q; want %q", step.Op, result, step.WantResult)
			}
		case "commit":
			err = adapter.Commit()
		case "rollback":
			err = adapter.Rollback()
		case "fail":
			err = adapter.Fail()
		case "read":
			var row storeVectorRow
			var found bool
			row, found, err = adapter.Read(step.Table, step.ID)
			if err == nil {
				err = checkVectorRow(step, row, found)
			}
		case "inspect":
			var row storeVectorRow
			var found bool
			row, found, err = adapter.Inspect(step.Scope, step.Table, step.ID)
			if err == nil {
				err = checkVectorRow(step, row, found)
			}
		case "inspect_receipt":
			var receipt storeVectorReceipt
			var found bool
			receipt, found, err = adapter.InspectReceipt(*step.Key)
			if err == nil {
				err = checkVectorReceipt(step, receipt, found)
			}
		default:
			return fmt.Errorf("unknown op %q", step.Op)
		}
		if step.WantError != "" {
			if err == nil {
				return fmt.Errorf("step %s succeeded; want error class %q", step.Op, step.WantError)
			}
			if class := vectorErrorClass(err); class != step.WantError {
				return fmt.Errorf("step %s error = %q; want %q", step.Op, class, step.WantError)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("step %s: %w", step.Op, err)
		}
	}
	return nil
}

func checkVectorRow(step storeVectorStep, row storeVectorRow, found bool) error {
	if step.WantAbsent {
		if found {
			return fmt.Errorf("row %s/%s is present; want absent", step.Table, step.ID)
		}
		return nil
	}
	if !found {
		return fmt.Errorf("row %s/%s is absent; want present", step.Table, step.ID)
	}
	if step.WantVersion != nil && row.Version != *step.WantVersion {
		return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, row.Version, *step.WantVersion)
	}
	for key, want := range step.Want {
		if got := row.Values[key]; got != want {
			return fmt.Errorf("row %s/%s value %q = %q; want %q", step.Table, step.ID, key, got, want)
		}
	}
	return nil
}

func checkVectorReceipt(step storeVectorStep, receipt storeVectorReceipt, found bool) error {
	if step.WantAbsent {
		if found {
			return fmt.Errorf("receipt is present; want absent")
		}
		return nil
	}
	if !found {
		return fmt.Errorf("receipt is absent; want present")
	}
	if step.WantContent != "" && receipt.Content != step.WantContent {
		return fmt.Errorf("receipt content = %q; want %q", receipt.Content, step.WantContent)
	}
	if step.WantResult != "" && receipt.Result != step.WantResult {
		return fmt.Errorf("receipt result = %q; want %q", receipt.Result, step.WantResult)
	}
	return nil
}

type vectorStore interface {
	Begin(storeVectorScope) error
	Write(storeVectorStep) error
	Receipt(storeVectorKey, string, string) (string, error)
	Commit() error
	Rollback() error
	Fail() error
	Read(string, string) (storeVectorRow, bool, error)
	Inspect(*storeVectorScope, string, string) (storeVectorRow, bool, error)
	InspectReceipt(storeVectorKey) (storeVectorReceipt, bool, error)
}

func rowKey(table string, scope storeVectorScope, id string) string {
	return table + "|" + scope.TenantID + "|" + scope.ProjectID + "|" + id
}

func receiptKey(key storeVectorKey) string {
	return key.TenantID + "|" + key.ProjectID + "|" + key.PrincipalID + "|" + key.Operation + "|" + key.CallerKey
}

func copyValues(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

type vectorTransaction struct {
	scope    storeVectorScope
	writes   map[string]storeVectorRow
	receipts map[string]storeVectorReceipt
}

type referenceVectorStore struct {
	tables   map[string]bool
	rows     map[string]storeVectorRow
	receipts map[string]storeVectorReceipt
	txn      *vectorTransaction
}

func newReferenceVectorStore(tables map[string]bool, seed storeVectorSeed) *referenceVectorStore {
	adapter := &referenceVectorStore{
		tables:   tables,
		rows:     make(map[string]storeVectorRow, len(seed.Rows)),
		receipts: make(map[string]storeVectorReceipt, len(seed.Receipts)),
	}
	for _, row := range seed.Rows {
		adapter.rows[rowKey(row.Table, storeVectorScope{row.TenantID, row.ProjectID}, row.ID)] = row
	}
	for _, receipt := range seed.Receipts {
		adapter.receipts[receiptKey(storeVectorKey{receipt.TenantID, receipt.ProjectID, receipt.PrincipalID, receipt.Operation, receipt.CallerKey})] = receipt
	}
	return adapter
}

func (adapter *referenceVectorStore) Begin(scope storeVectorScope) error {
	if scope.TenantID == "" {
		return vectorError{"invalid_scope"}
	}
	adapter.txn = &vectorTransaction{
		scope:    scope,
		writes:   make(map[string]storeVectorRow),
		receipts: make(map[string]storeVectorReceipt),
	}
	return nil
}

func (adapter *referenceVectorStore) Write(step storeVectorStep) error {
	if adapter.txn == nil {
		return vectorError{"no_transaction"}
	}
	if !adapter.tables[step.Table] {
		adapter.txn = nil
		return vectorError{"invalid_table"}
	}
	if err := adapter.checkReference(step); err != nil {
		adapter.txn = nil
		return err
	}
	key := rowKey(step.Table, adapter.txn.scope, step.ID)
	existing, found := adapter.txn.writes[key]
	if !found {
		existing, found = adapter.rows[key]
	}
	if step.ExpectVersion != nil && (!found || existing.Version != *step.ExpectVersion) {
		adapter.txn = nil
		return vectorError{"version_conflict"}
	}
	version := uint64(1)
	if found {
		version = existing.Version + 1
	}
	adapter.txn.writes[key] = storeVectorRow{
		Table:     step.Table,
		TenantID:  adapter.txn.scope.TenantID,
		ProjectID: adapter.txn.scope.ProjectID,
		ID:        step.ID,
		Version:   version,
		Values:    copyValues(step.Values),
	}
	return nil
}

func (adapter *referenceVectorStore) checkReference(step storeVectorStep) error {
	if step.Reference == nil {
		return nil
	}
	scope := storeVectorScope{step.Reference.TenantID, step.Reference.ProjectID}
	key := rowKey(step.Reference.Table, scope, step.Reference.ID)
	row, found := adapter.txn.writes[key]
	if !found {
		row, found = adapter.rows[key]
	}
	if !found {
		return vectorError{"not_found"}
	}
	if row.TenantID != adapter.txn.scope.TenantID || row.ProjectID != adapter.txn.scope.ProjectID {
		return vectorError{"cross_scope"}
	}
	return nil
}

func (adapter *referenceVectorStore) Receipt(key storeVectorKey, content, result string) (string, error) {
	if adapter.txn == nil {
		return "", vectorError{"no_transaction"}
	}
	identity := receiptKey(key)
	existing, found := adapter.txn.receipts[identity]
	if !found {
		existing, found = adapter.receipts[identity]
	}
	if found {
		if existing.Content != content {
			adapter.txn = nil
			return "", vectorError{"changed_idempotency_content"}
		}
		return existing.Result, nil
	}
	adapter.txn.receipts[identity] = storeVectorReceipt{
		TenantID:    key.TenantID,
		ProjectID:   key.ProjectID,
		PrincipalID: key.PrincipalID,
		Operation:   key.Operation,
		CallerKey:   key.CallerKey,
		Content:     content,
		Result:      result,
	}
	return result, nil
}

func (adapter *referenceVectorStore) Commit() error {
	if adapter.txn == nil {
		return vectorError{"no_transaction"}
	}
	for key, row := range adapter.txn.writes {
		adapter.rows[key] = row
	}
	for key, receipt := range adapter.txn.receipts {
		adapter.receipts[key] = receipt
	}
	adapter.txn = nil
	return nil
}

func (adapter *referenceVectorStore) Rollback() error {
	if adapter.txn == nil {
		return vectorError{"no_transaction"}
	}
	adapter.txn = nil
	return nil
}

func (adapter *referenceVectorStore) Fail() error {
	if adapter.txn == nil {
		return vectorError{"no_transaction"}
	}
	adapter.txn = nil
	return vectorError{"injected_failure"}
}

func (adapter *referenceVectorStore) Read(table, id string) (storeVectorRow, bool, error) {
	if adapter.txn == nil {
		return storeVectorRow{}, false, vectorError{"no_transaction"}
	}
	key := rowKey(table, adapter.txn.scope, id)
	if row, found := adapter.txn.writes[key]; found {
		return row, true, nil
	}
	row, found := adapter.rows[key]
	return row, found, nil
}

func (adapter *referenceVectorStore) Inspect(scope *storeVectorScope, table, id string) (storeVectorRow, bool, error) {
	if scope != nil {
		row, found := adapter.rows[rowKey(table, *scope, id)]
		return row, found, nil
	}
	for _, row := range adapter.rows {
		if row.Table == table && row.ID == id {
			return row, true, nil
		}
	}
	return storeVectorRow{}, false, nil
}

func (adapter *referenceVectorStore) InspectReceipt(key storeVectorKey) (storeVectorReceipt, bool, error) {
	receipt, found := adapter.receipts[receiptKey(key)]
	return receipt, found, nil
}

// nonAtomicVectorStore applies every write and receipt immediately and ignores
// rollback/failure. It is deliberately wrong: the vectors must detect it.
type nonAtomicVectorStore struct {
	tables   map[string]bool
	rows     map[string]storeVectorRow
	receipts map[string]storeVectorReceipt
	scope    *storeVectorScope
}

func newNonAtomicVectorStore(tables map[string]bool, seed storeVectorSeed) *nonAtomicVectorStore {
	adapter := &nonAtomicVectorStore{
		tables:   tables,
		rows:     make(map[string]storeVectorRow, len(seed.Rows)),
		receipts: make(map[string]storeVectorReceipt, len(seed.Receipts)),
	}
	for _, row := range seed.Rows {
		adapter.rows[rowKey(row.Table, storeVectorScope{row.TenantID, row.ProjectID}, row.ID)] = row
	}
	for _, receipt := range seed.Receipts {
		adapter.receipts[receiptKey(storeVectorKey{receipt.TenantID, receipt.ProjectID, receipt.PrincipalID, receipt.Operation, receipt.CallerKey})] = receipt
	}
	return adapter
}

func (adapter *nonAtomicVectorStore) Begin(scope storeVectorScope) error {
	if scope.TenantID == "" {
		return vectorError{"invalid_scope"}
	}
	adapter.scope = &scope
	return nil
}

func (adapter *nonAtomicVectorStore) Write(step storeVectorStep) error {
	if adapter.scope == nil {
		return vectorError{"no_transaction"}
	}
	if !adapter.tables[step.Table] {
		adapter.scope = nil
		return vectorError{"invalid_table"}
	}
	if step.Reference != nil {
		refScope := storeVectorScope{step.Reference.TenantID, step.Reference.ProjectID}
		if _, found := adapter.rows[rowKey(step.Reference.Table, refScope, step.Reference.ID)]; !found {
			adapter.scope = nil
			return vectorError{"not_found"}
		}
		if step.Reference.TenantID != adapter.scope.TenantID || step.Reference.ProjectID != adapter.scope.ProjectID {
			adapter.scope = nil
			return vectorError{"cross_scope"}
		}
	}
	key := rowKey(step.Table, *adapter.scope, step.ID)
	existing, found := adapter.rows[key]
	if step.ExpectVersion != nil && (!found || existing.Version != *step.ExpectVersion) {
		adapter.scope = nil
		return vectorError{"version_conflict"}
	}
	version := uint64(1)
	if found {
		version = existing.Version + 1
	}
	adapter.rows[key] = storeVectorRow{
		Table:     step.Table,
		TenantID:  adapter.scope.TenantID,
		ProjectID: adapter.scope.ProjectID,
		ID:        step.ID,
		Version:   version,
		Values:    copyValues(step.Values),
	}
	return nil
}

func (adapter *nonAtomicVectorStore) Receipt(key storeVectorKey, content, result string) (string, error) {
	if adapter.scope == nil {
		return "", vectorError{"no_transaction"}
	}
	identity := receiptKey(key)
	existing, found := adapter.receipts[identity]
	if found {
		if existing.Content != content {
			adapter.scope = nil
			return "", vectorError{"changed_idempotency_content"}
		}
		return existing.Result, nil
	}
	adapter.receipts[identity] = storeVectorReceipt{
		TenantID:    key.TenantID,
		ProjectID:   key.ProjectID,
		PrincipalID: key.PrincipalID,
		Operation:   key.Operation,
		CallerKey:   key.CallerKey,
		Content:     content,
		Result:      result,
	}
	return result, nil
}

func (adapter *nonAtomicVectorStore) Commit() error {
	if adapter.scope == nil {
		return vectorError{"no_transaction"}
	}
	adapter.scope = nil
	return nil
}

func (adapter *nonAtomicVectorStore) Rollback() error {
	if adapter.scope == nil {
		return vectorError{"no_transaction"}
	}
	adapter.scope = nil
	return nil
}

func (adapter *nonAtomicVectorStore) Fail() error {
	if adapter.scope == nil {
		return vectorError{"no_transaction"}
	}
	adapter.scope = nil
	return vectorError{"injected_failure"}
}

func (adapter *nonAtomicVectorStore) Read(table, id string) (storeVectorRow, bool, error) {
	if adapter.scope == nil {
		return storeVectorRow{}, false, vectorError{"no_transaction"}
	}
	row, found := adapter.rows[rowKey(table, *adapter.scope, id)]
	return row, found, nil
}

func (adapter *nonAtomicVectorStore) Inspect(scope *storeVectorScope, table, id string) (storeVectorRow, bool, error) {
	if scope != nil {
		row, found := adapter.rows[rowKey(table, *scope, id)]
		return row, found, nil
	}
	for _, row := range adapter.rows {
		if row.Table == table && row.ID == id {
			return row, true, nil
		}
	}
	return storeVectorRow{}, false, nil
}

func (adapter *nonAtomicVectorStore) InspectReceipt(key storeVectorKey) (storeVectorReceipt, bool, error) {
	receipt, found := adapter.receipts[receiptKey(key)]
	return receipt, found, nil
}
