// Package memory is the in-process development persistence adapter.
//
// The tests in this directory pin the behavior of these exported symbols:
//
//	func New() *Store
//
//	func (s *Store) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error)
//	func (s *Store) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error)
//
//	func (s *Store) FailNextCommit(err error)
//
//	func (s *Store) Snapshot(scope tenancy.Scope, table, id string) (store.Row, bool)
//	func (s *Store) Receipt(key receipt.Key) (receipt.Record, bool)
//	func (s *Store) Events(scope tenancy.Scope) []CommittedEvent
//	func (s *Store) Audit(scope tenancy.Scope) []event.Audit
//
//	type CommittedEvent struct {
//		Sequence uint64
//		Envelope event.Envelope
//	}
//
// Pinned semantics:
//
//   - One store owns ONE shared committed state. There are no per-capability
//     maps with independent locks.
//   - Execute applies the whole unit (locks, business writes and guarded
//     deletes, receipt, audit rows and event publication intent) against a
//     copy-on-write/transactional snapshot and publishes it all-or-nothing. A
//     refused or failed unit leaves no row, receipt, audit row or event
//     visible, including a receipt that was never committed.
//   - Execute validates the whole unit before any effect: scope, table, id,
//     reserved/unknown columns, write/delete shape, root-row ownership, audit
//     and event scope, and receipt identity. A cross-scope row, lock or record
//     is refused (Forbidden for root identities and record scope; a scoped
//     lock or write that cannot see the row is VersionConflict).
//   - An existing receipt with equal content replays the ORIGINAL durable
//     result (UnitResult.Replayed == true) and applies nothing. Changed content
//     is refused with fault.ChangedIdempotencyContent before any write.
//   - Locks are validated in the declared schema lock order. A plain lock fails
//     with VersionConflict when the row is absent or out of scope. ExpectVersion
//     and Expect are read-only guards inside the same unit; a missing row or a
//     mismatched version/column is VersionConflict. TenantWide targets a
//     tenant-wide (project_id IS NULL) row from a project-scoped unit.
//   - A versioned write compares ExpectVersion and advances the single version
//     column by one. A table without a version column (tasks) is updated with
//     an explicit Expect column guard instead. A zero-row update or delete is
//     VersionConflict, never a silent success.
//   - Write.Delete is the only row-removal shape. It forbids Insert and Values
//     and requires ExpectVersion on a versioned table or a non-empty Expect on a
//     versionless table. An insert of an already-present row (including a
//     duplicate insert inside one unit) is refused with VersionConflict and
//     changes nothing.
//   - Read returns a defensive deep copy of one committed row inside the scope
//     (fault.NotFound otherwise). A cross-scope row never leaks, and mutating a
//     returned Row, Values map or byte slice cannot alias stored state.
//   - FailNextCommit arms exactly one deterministic failure. The armed error is
//     returned unchanged by the next Execute call after the unit has been fully
//     staged against the snapshot and before anything is published, so no
//     staged effect is visible. It is a one-shot hook for tests.
//   - Snapshot returns a committed row by table/id inside the scope and reports
//     false when the row is absent or out of scope. Events returns committed
//     event rows visible in the scope in ascending publication sequence order
//     (tenant scope: project_id IS NULL; project scope: the exact project).
//     Audit returns committed audit rows visible in the scope in commit order.
//     All three return defensive copies.
//   - New() returns an independent, ephemeral store. Dropping the *Store drops
//     its state; two stores never share rows.
//
// The production package doc must state that this is an ephemeral development
// adapter and a conformance fixture, not evidence of PostgreSQL RLS, isolation
// or constraint behavior.
package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenantID    = "tenant-1"
	otherTenantID   = "tenant-2"
	testProjectID   = "project-1"
	otherProjectID  = "project-2"
	testPrincipalID = "principal-1"
	testServiceID   = "service-1"
	testExecutionID = "execution-1"
	testActionID    = "action-1"
	testTaskID      = "task-1"
)

// errAborted is a stable injected failure for tests that do not care which
// error the adapter returns, only that the unit did not commit.
var errAborted = errors.New("aborted by test")

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID, ProjectID: testProjectID}
}

func otherProjectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID, ProjectID: otherProjectID}
}

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID}
}

func scopeFor(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func fixedTime() time.Time {
	return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return out
}

func fingerprintOfContent(content string) (string, error) {
	return receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(content)})
}

func fingerprintOf(t *testing.T, content string) string {
	t.Helper()
	fingerprint, err := fingerprintOfContent(content)
	if err != nil {
		t.Fatalf("fingerprint %q: %v", content, err)
	}
	return fingerprint
}

func keyFor(tenant, project, principal, operation, callerKey string) receipt.Key {
	return receipt.Key{
		TenantID:    id.TenantID(tenant),
		ProjectID:   id.ProjectID(project),
		PrincipalID: id.PrincipalID(principal),
		Operation:   operation,
		CallerKey:   callerKey,
	}
}

// receiptRecord builds one receipt with an explicit identity. content is
// normalized as JSON, matching the vector and domain content fingerprint.
func receiptRecord(t *testing.T, receiptID string, key receipt.Key, content, result string) receipt.Record {
	t.Helper()
	return receipt.Record{
		ID:          id.ReceiptID(receiptID),
		Key:         key,
		ContentHash: fingerprintOf(t, content),
		Result:      []byte(result),
		At:          fixedTime(),
	}
}

func defaultReceipt(t *testing.T) receipt.Record {
	t.Helper()
	return receiptRecord(t,
		"receipt-1",
		keyFor(testTenantID, testProjectID, testPrincipalID, "task.create", "caller-1"),
		`{"task":"task-1"}`,
		`{"execution_id":"execution-1"}`,
	)
}

func defaultEvent(scope tenancy.Scope, eventID string, eventType event.Type, subjectID string) event.Envelope {
	return event.Envelope{
		ID:        id.EventID(eventID),
		Type:      eventType,
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID(testPrincipalID)},
		At:        fixedTime(),
		Scope:     scope,
		SubjectID: subjectID,
	}
}

func defaultAudit(scope tenancy.Scope, auditID, operation string) event.Audit {
	return event.Audit{
		ID:        id.EventID(auditID),
		Scope:     scope,
		Actor:     id.PrincipalID(testPrincipalID),
		Operation: operation,
		At:        fixedTime(),
		Detail:    operation,
	}
}

func cloneValues(values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	maps.Copy(copied, values)
	return copied
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func executionValues(version uint64, state string) map[string]any {
	values := insertValues("executions")
	values["version"] = version
	values["state"] = state
	return values
}

func actionValues(version uint64, state string) map[string]any {
	values := insertValues("actions")
	values["version"] = version
	values["state"] = state
	return values
}

func taskValues(lifecycleVersion uint64, state string) map[string]any {
	values := insertValues("tasks")
	values["lifecycle_version"] = lifecycleVersion
	values["state"] = state
	return values
}

// insertValues returns a complete-enough row for one schema table. The adapter
// does not interpret business columns; these values only exercise the write
// path with realistic names and a caller-supplied initial version.
func insertValues(table string) map[string]any {
	switch table {
	case "tenants":
		return map[string]any{"name": "acme", "version": uint64(1), "derivation_key": []byte(`{"version":1}`)}
	case "projects":
		return map[string]any{"name": "app", "version": uint64(1)}
	case "memberships":
		return map[string]any{"principal_id": testPrincipalID, "roles": []byte(`["admin"]`), "version": uint64(1)}
	case "services":
		return map[string]any{"name": "service", "state": "enabled", "roles": []byte(`["engineer"]`), "version": uint64(1)}
	case "pipelines":
		yaml := []byte("name: build\non:\n  push:\n    branches: [main]\n")
		return map[string]any{"name": "build", "yaml": yaml, "yaml_sha256": sha256Sum(yaml), "version": uint64(1)}
	case "tasks":
		return map[string]any{
			"content_version":   uint64(1),
			"lifecycle_version": uint64(1),
			"state":             "open",
			"initiator_id":      testPrincipalID,
			"title":             "task",
		}
	case "task_messages":
		return map[string]any{"task_id": testTaskID, "author_id": testPrincipalID, "at": fixedTime(), "body": "hello"}
	case "executions":
		return map[string]any{
			"version":      uint64(1),
			"state":        "queued",
			"principal_id": testPrincipalID,
			"yaml_sha256":  sha256Sum([]byte("name: build\non: push\n")),
			"snapshot":     []byte(`{"yaml_sha256":"00"}`),
		}
	case "actions":
		return map[string]any{
			"execution_id":       testExecutionID,
			"index":              int64(0),
			"name":               "action-1",
			"kind":               "container",
			"state":              "pending",
			"version":            uint64(1),
			"timeout_ms":         int64(1000),
			"staged_outputs_set": false,
		}
	default:
		return map[string]any{}
	}
}

func mustExecute(t *testing.T, s *Store, scope tenancy.Scope, unit store.Unit) store.UnitResult {
	t.Helper()
	result, err := s.Execute(context.Background(), scope, unit)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

func seedRow(t *testing.T, s *Store, scope tenancy.Scope, table, id string, values map[string]any) {
	t.Helper()
	mustExecute(t, s, scope, store.Unit{
		Writes: []store.Write{{Table: table, ID: id, Insert: true, Values: cloneValues(values)}},
	})
}

func mustSnapshot(t *testing.T, s *Store, scope tenancy.Scope, table, id string) store.Row {
	t.Helper()
	row, found := s.Snapshot(scope, table, id)
	if !found {
		t.Fatalf("%s %q is absent from scope %+v", table, id, scope)
	}
	return row
}

func assertAbsent(t *testing.T, s *Store, scope tenancy.Scope, table, id string) {
	t.Helper()
	if row, found := s.Snapshot(scope, table, id); found {
		t.Fatalf("%s %q is present in scope %+v: %v", table, id, scope, row.Values)
	}
}

func rowString(t *testing.T, row store.Row, column string) string {
	t.Helper()
	raw, ok := row.Values[column]
	if !ok {
		t.Fatalf("%s row has no %q column: %v", row.Table, column, row.Values)
	}
	text, ok := raw.(string)
	if !ok {
		t.Fatalf("%s row %q is %T; want a string", row.Table, column, raw)
	}
	return text
}

// rowVersion reads a bigint column and requires the exact contract type: the
// adapter stores versions as int64, matching PostgreSQL. Accepting uint64 here
// would hide the defect this helper guards against.
func rowVersion(t *testing.T, row store.Row, column string) uint64 {
	t.Helper()
	raw, ok := row.Values[column]
	if !ok {
		t.Fatalf("%s row has no %q column: %v", row.Table, column, row.Values)
	}
	value, ok := raw.(int64)
	if !ok {
		t.Fatalf("%s row %q is %T; want int64", row.Table, column, raw)
	}
	if value < 0 {
		t.Fatalf("%s row %q = %d; want a non-negative version", row.Table, column, value)
	}
	return uint64(value)
}

func assertFault(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s fault, got nil", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("expected a %s fault, got %T: %v", want, err, err)
	}
	if got != want {
		t.Fatalf("fault class = %s; want %s (%v)", got, want, err)
	}
}

func eventIDs(events []CommittedEvent) []string {
	identities := make([]string, len(events))
	for index, committed := range events {
		identities[index] = string(committed.Envelope.ID)
	}
	return identities
}

func auditOperations(audits []event.Audit) []string {
	operations := make([]string, len(audits))
	for index, audit := range audits {
		operations[index] = audit.Operation
	}
	return operations
}

// runConcurrently starts n goroutines behind one explicit barrier. There is no
// sleep: every goroutine signals readiness, the barrier opens, and the results
// are collected in index order.
func runConcurrently(t *testing.T, n int, fn func(index int) error) []error {
	t.Helper()
	ready := make(chan struct{}, n)
	start := make(chan struct{})
	out := make([]error, n)
	var group sync.WaitGroup
	group.Add(n)
	for index := range n {
		go func(index int) {
			defer group.Done()
			ready <- struct{}{}
			<-start
			out[index] = fn(index)
		}(index)
	}
	for range n {
		<-ready
	}
	close(start)
	group.Wait()
	return out
}

func countNil(errs []error) int {
	count := 0
	for _, err := range errs {
		if err == nil {
			count++
		}
	}
	return count
}

func countFault(t *testing.T, errs []error, class fault.Class) int {
	t.Helper()
	count := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		got, ok := fault.ClassOf(err)
		if !ok {
			t.Fatalf("unexpected non-fault error: %v", err)
		}
		if got == class {
			count++
		}
	}
	return count
}

func describeErrors(errs []error) string {
	return fmt.Sprint(errs)
}
