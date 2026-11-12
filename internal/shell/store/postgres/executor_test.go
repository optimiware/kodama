// The neutral store.Executor seam: the PostgreSQL adapter implements the same
// structured commit unit as the package-level SQL helpers, and Read drives one
// scoped read-only transaction through its own connector.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestStoreImplementsExecutorSeam(t *testing.T) {
	// Compile-time conformance: a memory adapter can implement store.Executor
	// without importing this package, and this adapter keeps the same seam.
	var _ store.Executor = New(newFakeDB())
}

func TestExecutorExecuteCommitsWholeUnit(t *testing.T) {
	db := newFakeDB()
	executor := New(db)
	scope := testProjectScope()
	record := testReceipt(t)
	unit := store.Unit{
		Locks: []store.Lock{
			{Table: "cleanup", IDs: []string{testExecutionID}},
			{Table: "executions", IDs: []string{testExecutionID}},
		},
		Writes: []store.Write{
			{Table: "actions", ID: "action-1", Insert: true, Values: unitValues("actions")},
			{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")},
		},
		Receipt: &record,
		Audit:   []event.Audit{testAudit(scope)},
		Events:  []event.Envelope{testEvent(scope)},
	}

	result, err := executor.Execute(context.Background(), scope, unit)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Replayed {
		t.Fatal("a fresh commit unit was reported as a replay")
	}
	if result.Receipt.ExecutionID != record.ExecutionID {
		t.Errorf("committed receipt execution = %q; want %q", result.Receipt.ExecutionID, record.ExecutionID)
	}

	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	executionsLock := indexOfTable(calls, "executions", "lock")
	cleanupLock := indexOfTable(calls, "cleanup", "lock")
	executionsWrite := indexOfTable(calls, "executions", "insert")
	actionsWrite := indexOfTable(calls, "actions", "insert")
	receiptInsert := indexOfTable(calls, "receipts", "insert")
	auditInsert := indexOfTable(calls, "audit", "insert")
	eventInsert := indexOfTable(calls, "events", "insert")
	if executionsLock < 0 || cleanupLock < 0 || executionsWrite < 0 || actionsWrite < 0 ||
		receiptInsert < 0 || auditInsert < 0 || eventInsert < 0 {
		t.Fatalf("missing commit-unit calls: locks=%d/%d writes=%d/%d receipt=%d audit=%d event=%d",
			executionsLock, cleanupLock, executionsWrite, actionsWrite, receiptInsert, auditInsert, eventInsert)
	}
	if !(executionsLock < cleanupLock && cleanupLock < executionsWrite && executionsWrite < actionsWrite) {
		t.Error("the executor did not apply locks and writes in the declared order")
	}
	if !(actionsWrite < receiptInsert && receiptInsert < auditInsert && auditInsert < eventInsert) {
		t.Error("the executor did not commit receipt/audit/event intent after the business writes")
	}
}

func TestExecutorExecuteRollsBackOnInjectedFailure(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("event write failed")
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if indexOfTable([]recordedCall{{kind: "exec", sql: sql}}, "events", "insert") == 0 {
			return store.Result{}, injected
		}
		return store.Result{RowsAffected: 1}, nil
	})
	executor := New(db)
	scope := testProjectScope()
	record := testReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &record,
		Audit:   []event.Audit{testAudit(scope)},
		Events:  []event.Envelope{testEvent(scope)},
	}

	if _, err := executor.Execute(context.Background(), scope, unit); !errors.Is(err, injected) {
		t.Fatalf("Execute error = %v; want the injected cause", err)
	}
	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("failed executor unit terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
}

func TestExecutorExecuteReplaysEqualReceipt(t *testing.T) {
	db := newFakeDB()
	stored := testReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))
	executor := New(db)
	candidate := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &candidate,
		Events:  []event.Envelope{testEvent(scope)},
	}

	result, err := executor.Execute(context.Background(), scope, unit)
	if err != nil {
		t.Fatalf("Execute replay: %v", err)
	}
	if !result.Replayed {
		t.Fatal("equal receipt content did not replay the committed result")
	}
	if result.Receipt.ExecutionID != stored.ExecutionID {
		t.Errorf("replayed execution = %q; want %q", result.Receipt.ExecutionID, stored.ExecutionID)
	}
	if string(result.Receipt.Result) != string(stored.Result) {
		t.Errorf("replayed result = %q; want %q", result.Receipt.Result, stored.Result)
	}
	conn := db.conn(t)
	if conn.terminalCalls() != 1 {
		t.Fatalf("replay issued %d terminal calls; want exactly 1", conn.terminalCalls())
	}
	calls := conn.callsCopy()
	if indexOfTable(calls, "executions", "insert") >= 0 {
		t.Error("a replay re-applied the business write")
	}
	if indexOfTable(calls, "receipts", "insert") >= 0 {
		t.Error("a replay inserted a second receipt")
	}
	if indexOfTable(calls, "events", "insert") >= 0 {
		t.Error("a replay re-published the event")
	}
}

func TestExecutorExecuteRefusesChangedReceiptContent(t *testing.T) {
	db := newFakeDB()
	stored := testReceipt(t)
	stored.ContentHash = testFingerprint(t, []byte(`{"a":2}`))
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))
	executor := New(db)
	candidate := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &candidate,
		Events:  []event.Envelope{testEvent(scope)},
	}

	_, err := executor.Execute(context.Background(), scope, unit)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)
	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("changed-content terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	if indexOfTable(conn.callsCopy(), "executions", "insert") >= 0 {
		t.Error("changed receipt content re-applied the business write")
	}
}

func TestExecutorExecuteEnforcesLockGuardsAndTenantWide(t *testing.T) {
	t.Run("stale guard refuses the unit", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
			return &fakeRows{index: -1}, nil
		})
		executor := New(db)
		unit := store.Unit{
			Locks: []store.Lock{{Table: "services", IDs: []string{testServiceID}, ExpectVersion: 3}},
			Writes: []store.Write{{
				Table:         "services",
				ID:            testServiceID,
				ExpectVersion: 3,
				Values:        map[string]any{"state": "disabled"},
			}},
		}
		_, err := executor.Execute(context.Background(), testTenantScope(), unit)
		assertFaultClass(t, err, fault.VersionConflict)
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("stale guard terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
		if indexOfTable(conn.callsCopy(), "services", "update") >= 0 {
			t.Error("a stale authority guard still applied its business write")
		}
	})

	t.Run("missing guard row refuses the unit", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
			return &fakeRows{index: -1}, nil
		})
		executor := New(db)
		unit := store.Unit{
			Locks: []store.Lock{{
				Table:  "memberships",
				IDs:    []string{"membership-1"},
				Expect: map[string]any{"principal_id": testPrincipalID},
			}},
		}
		_, err := executor.Execute(context.Background(), testProjectScope(), unit)
		assertFaultClass(t, err, fault.VersionConflict)
		if conn := db.conn(t); conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("missing guard terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})

	t.Run("tenant-wide guard from project scope", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
			return singleRow("services", sql, map[string]any{"id": testServiceID}), nil
		})
		executor := New(db)
		unit := store.Unit{
			Locks: []store.Lock{{
				Table:         "services",
				IDs:           []string{testServiceID},
				ExpectVersion: 2,
				TenantWide:    true,
			}},
		}
		if _, err := executor.Execute(context.Background(), testProjectScope(), unit); err != nil {
			t.Fatalf("Execute tenant-wide guard: %v", err)
		}
		conn := db.conn(t)
		if conn.commits != 1 || conn.rollbacks != 0 {
			t.Fatalf("tenant-wide guard terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
		calls := conn.callsCopy()
		locks := dataExecCalls(calls)
		if len(locks) != 1 {
			t.Fatalf("tenant-wide guard issued %d lock statements; want 1", len(locks))
		}
		if text := strings.ToLower(locks[0].sql); !strings.Contains(text, "project_id is null") {
			t.Errorf("the lock did not target the tenant-wide row: %s", locks[0].sql)
		}
		guards := queryCalls(calls)
		if len(guards) != 1 {
			t.Fatalf("tenant-wide guard issued %d guard queries; want 1", len(guards))
		}
		text := strings.ToLower(guards[0].sql)
		if !strings.Contains(text, "project_id is null") || strings.Contains(text, "project_id = $") {
			t.Errorf("the guard did not target the tenant-wide row: %s", guards[0].sql)
		}
		if argContains(guards[0].args, testProjectID) {
			t.Errorf("tenant-wide guard args %v carry the project identity", guards[0].args)
		}
	})
}

func TestExecutorExecuteRefusesCrossScopeBeforeTransaction(t *testing.T) {
	db := newFakeDB()
	executor := New(db)
	record := testReceipt(t)
	foreign := testEvent(tenancy.Scope{TenantID: "tenant-2", ProjectID: testProjectID})
	unit := store.Unit{
		Receipt: &record,
		Events:  []event.Envelope{foreign},
	}
	_, err := executor.Execute(context.Background(), testProjectScope(), unit)
	assertFaultClass(t, err, fault.Forbidden)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("a cross-scope event opened %d transactions", got)
	}
}

func TestExecutorReadCommitsScopedReadOnlyTransaction(t *testing.T) {
	yaml := []byte("name: build\non:\n  push:\n    branches: [main]\n")
	hash := testDigest(yaml)
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"pipelines": pipelineFixture(yaml, hash)}))
	executor := New(db)

	row, err := executor.Read(context.Background(), testProjectScope(), "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if row.Table != "pipelines" {
		t.Errorf("row table = %q; want pipelines", row.Table)
	}
	stored, ok := row.Values["yaml"].([]byte)
	if !ok || !bytes.Equal(stored, yaml) {
		t.Errorf("retained YAML = %v; want exact %q", row.Values["yaml"], yaml)
	}

	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("read terminal calls: commits=%d rollbacks=%d; want one read-only commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	if len(calls) < 3 {
		t.Fatalf("read issued %d calls; want scope, query and commit", len(calls))
	}
	if indexOfSQL(calls, "set_config") < 0 {
		t.Error("Read did not establish transaction-local scope")
	}
	queries := queryCalls(calls)
	if len(queries) != 1 {
		t.Fatalf("Read issued %d queries; want 1", len(queries))
	}
	text := strings.ToLower(queries[0].sql)
	if !strings.Contains(text, "tenant_id") || !strings.Contains(text, "project_id") {
		t.Errorf("scoped read does not filter on ownership: %s", queries[0].sql)
	}
	for _, value := range []any{testTenantID, testProjectID, "pipeline-1"} {
		if !argContains(queries[0].args, value) {
			t.Errorf("scoped read args %v do not carry %v", queries[0].args, value)
		}
	}
	if strings.Contains(text, "pipeline-1") {
		t.Errorf("scoped read interpolated the row identity: %s", queries[0].sql)
	}
	if last := calls[len(calls)-1]; last.kind != "commit" {
		t.Errorf("Read ended with %q; want a read-only commit", last.kind)
	}
}

func TestExecutorReadRollsBackOnFailure(t *testing.T) {
	t.Run("missing row", func(t *testing.T) {
		db := newFakeDB()
		executor := New(db)
		_, err := executor.Read(context.Background(), testProjectScope(), "pipelines", "pipeline-1")
		assertFaultClass(t, err, fault.NotFound)
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("missing-row read terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})

	t.Run("cross-scope row", func(t *testing.T) {
		yaml := []byte("name: build\n")
		fixture := pipelineFixture(yaml, testDigest(yaml))
		fixture["project_id"] = "project-2"
		db := newFakeDB()
		db.script.setQuery(queryByTable(map[string]map[string]any{"pipelines": fixture}))
		executor := New(db)
		_, err := executor.Read(context.Background(), testProjectScope(), "pipelines", "pipeline-1")
		assertFaultClass(t, err, fault.Forbidden)
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("cross-scope read terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})
}
