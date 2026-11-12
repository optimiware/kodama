// Guarded deletion: a delete is an explicit, version/column-guarded row
// removal inside the same atomic commit unit as its receipt, audit and event
// publication intent. An unguarded, value-carrying or stale delete is refused
// before it can remove a row.
package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestDeleteRowRendersScopedVersionGuard(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:         "executions",
		ID:            testExecutionID,
		Delete:        true,
		ExpectVersion: 3,
	}); err != nil {
		t.Fatalf("WriteRow delete: %v", err)
	}

	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("delete issued %d statements; want 1", len(calls))
	}
	text := strings.ToLower(calls[0].sql)
	if !strings.HasPrefix(text, "delete from executions") {
		t.Fatalf("delete statement = %s", calls[0].sql)
	}
	for _, predicate := range []string{"id = $", "tenant_id = $", "project_id = $", "version = $"} {
		if !strings.Contains(text, predicate) {
			t.Errorf("delete does not carry %q: %s", predicate, calls[0].sql)
		}
	}
	for _, value := range []any{testExecutionID, testTenantID, testProjectID} {
		if !argContains(calls[0].args, value) {
			t.Errorf("delete args %v do not carry %v", calls[0].args, value)
		}
	}
	if !argContainsNumber(calls[0].args, 3) {
		t.Errorf("delete args %v do not carry the expected version", calls[0].args)
	}
	if strings.Contains(text, testExecutionID) {
		t.Errorf("delete interpolated the row identity: %s", calls[0].sql)
	}
}

func TestDeleteRowRendersExplicitColumnGuardForVersionlessTable(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:  "tasks",
		ID:     "task-1",
		Delete: true,
		Expect: map[string]any{"lifecycle_version": uint64(2)},
	}); err != nil {
		t.Fatalf("WriteRow versionless delete: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("versionless delete issued %d statements; want 1", len(calls))
	}
	text := strings.ToLower(calls[0].sql)
	if !strings.HasPrefix(text, "delete from tasks") || !strings.Contains(text, "lifecycle_version = $") {
		t.Fatalf("versionless delete statement = %s", calls[0].sql)
	}
	if !argContainsNumber(calls[0].args, 2) {
		t.Errorf("versionless delete args %v do not carry the expected lifecycle version", calls[0].args)
	}
}

func TestDeleteRowStaleMissingOrOutOfScopeRowIsConflict(t *testing.T) {
	cases := []struct {
		name          string
		scope         func() tenancy.Scope
		table         string
		id            string
		expectVersion uint64
	}{
		{"stale version", testProjectScope, "executions", testExecutionID, 7},
		{"missing row", testProjectScope, "pipelines", "pipeline-1", 3},
		{"out-of-scope tenant-wide row", testProjectScope, "services", testServiceID, 5},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := newFakeDB()
			// A zero-row delete means the row is absent, outside the verified
			// scope, or no longer at the expected version. All three are a
			// conflict and never a silent success.
			db.script.setExec(func(sql string, args []any) (store.Result, error) {
				return store.Result{RowsAffected: 0}, nil
			})
			connector := New(db)
			tx, err := connector.Begin(context.Background(), testCase.scope())
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()

			err = WriteRow(context.Background(), tx, testCase.scope(), store.Write{
				Table:         testCase.table,
				ID:            testCase.id,
				Delete:        true,
				ExpectVersion: testCase.expectVersion,
			})
			assertFaultClass(t, err, fault.VersionConflict)
			calls := dataExecCalls(db.conn(t).callsCopy())
			if len(calls) != 1 {
				t.Fatalf("zero-row delete issued %d statements; want 1", len(calls))
			}
			text := strings.ToLower(calls[0].sql)
			for _, predicate := range []string{"id = $", "tenant_id = $", "project_id = $", "version = $"} {
				if !strings.Contains(text, predicate) {
					t.Errorf("delete does not carry %q: %s", predicate, calls[0].sql)
				}
			}
			if !argContainsNumber(calls[0].args, int64(testCase.expectVersion)) {
				t.Errorf("delete args %v do not carry the expected version", calls[0].args)
			}
		})
	}
}

func TestDeleteRowRefusesUnguardedOrAmbiguousWritesBeforeAnyStatement(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	cases := []struct {
		name  string
		scope func() tenancy.Scope
		write store.Write
		want  fault.Class
	}{
		{
			name:  "insert and delete",
			scope: testProjectScope,
			write: store.Write{Table: "executions", ID: testExecutionID, Insert: true, Delete: true, ExpectVersion: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "delete with values",
			scope: testProjectScope,
			write: store.Write{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
			want:  fault.InvalidInput,
		},
		{
			name:  "versioned delete without expected version",
			scope: testProjectScope,
			write: store.Write{Table: "executions", ID: testExecutionID, Delete: true},
			want:  fault.InvalidInput,
		},
		{
			name:  "versionless delete without column guard",
			scope: testProjectScope,
			write: store.Write{Table: "tasks", ID: "task-1", Delete: true},
			want:  fault.InvalidInput,
		},
		{
			name:  "versionless delete with expected version",
			scope: testProjectScope,
			write: store.Write{Table: "tasks", ID: "task-1", Delete: true, ExpectVersion: 2},
			want:  fault.InvalidInput,
		},
		{
			name:  "unknown table",
			scope: testProjectScope,
			write: store.Write{Table: "not_a_table", ID: "id-1", Delete: true, ExpectVersion: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "project table in tenant scope",
			scope: testTenantScope,
			write: store.Write{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "cross-project root delete",
			scope: testProjectScope,
			write: store.Write{Table: "projects", ID: "project-2", Delete: true, ExpectVersion: 1},
			want:  fault.Forbidden,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := WriteRow(context.Background(), tx, testCase.scope(), testCase.write)
			assertFaultClass(t, err, testCase.want)
		})
	}
	if got := len(dataExecCalls(db.conn(t).callsCopy())); got != 0 {
		t.Fatalf("invalid deletes issued %d statements", got)
	}
}

func TestDeleteWithReceiptCommitsAtomically(t *testing.T) {
	db := newFakeDB()
	executor := New(db)
	record := testReceipt(t)
	unit := store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            testExecutionID,
			Delete:        true,
			ExpectVersion: 1,
		}},
		Receipt: &record,
	}
	if _, err := executor.Execute(context.Background(), testProjectScope(), unit); err != nil {
		t.Fatalf("Execute delete unit: %v", err)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("delete unit terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	deleteIndex := deleteCallIndex(calls, "executions")
	receiptIndex := indexOfTable(calls, "receipts", "insert")
	if deleteIndex < 0 || receiptIndex < 0 {
		t.Fatalf("missing delete unit calls: delete=%d receipt=%d", deleteIndex, receiptIndex)
	}
	if deleteIndex > receiptIndex {
		t.Error("the receipt was inserted before the business delete")
	}
}

func TestDeleteWithReceiptRollsBackWhenReceiptFails(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("receipt insert failed")
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "insert into receipts") {
			return store.Result{}, injected
		}
		return store.Result{RowsAffected: 1}, nil
	})
	executor := New(db)
	record := testReceipt(t)
	unit := store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            testExecutionID,
			Delete:        true,
			ExpectVersion: 1,
		}},
		Receipt: &record,
	}
	if _, err := executor.Execute(context.Background(), testProjectScope(), unit); !errors.Is(err, injected) {
		t.Fatalf("Execute error = %v; want the injected receipt failure", err)
	}
	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("failed delete unit terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	if deleteCallIndex(calls, "executions") < 0 {
		t.Error("the business delete was not attempted before the failed receipt")
	}
}

// deleteCallIndex finds the DELETE statement for one table in the recorded
// call log.
func deleteCallIndex(calls []recordedCall, table string) int {
	for index, call := range calls {
		if call.kind != "exec" {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(call.sql))
		if strings.HasPrefix(text, "delete from "+table) {
			return index
		}
	}
	return -1
}
