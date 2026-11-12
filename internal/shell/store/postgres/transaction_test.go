// Transaction seam behavior: one scoped transaction per Begin, transaction-local
// scope, terminal cleanup and no use after commit or rollback.
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

func TestStoreImplementsTheScopedConnection(t *testing.T) {
	var _ store.ScopedConnection = New(newFakeDB())
}

func scopeCalls(calls []recordedCall) []recordedCall {
	scoped := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind == "exec" && strings.Contains(strings.ToLower(call.sql), "set_config") {
			scoped = append(scoped, call)
		}
	}
	return scoped
}

func TestBeginEstablishesTransactionLocalScope(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	conn := db.conn(t)
	calls := conn.callsCopy()
	scoped := scopeCalls(calls)
	if len(scoped) == 0 {
		t.Fatal("Begin did not establish a transaction-local scope")
	}
	var scopeSQL strings.Builder
	var scopeArgs []any
	for _, call := range scoped {
		scopeSQL.WriteString(call.sql)
		scopeSQL.WriteString(" ")
		scopeArgs = append(scopeArgs, call.args...)
	}
	text := strings.ToLower(scopeSQL.String())
	for _, setting := range []string{"kodama.tenant_id", "kodama.project_id"} {
		if !strings.Contains(text, setting) {
			t.Errorf("scope statement does not set %q: %s", setting, scopeSQL.String())
		}
	}
	if !strings.Contains(text, "true") {
		t.Errorf("scope is not transaction-local (set_config is_local must be true): %s", scopeSQL.String())
	}
	if strings.Contains(text, "false") {
		t.Errorf("scope is established at session level: %s", scopeSQL.String())
	}
	for _, literal := range []string{testTenantID, testProjectID} {
		if strings.Contains(text, literal) {
			t.Errorf("scope value %q is interpolated into SQL instead of travelling as an argument: %s", literal, scopeSQL.String())
		}
	}
	if !argContains(scopeArgs, testTenantID) {
		t.Errorf("scope arguments %v do not carry the tenant", scopeArgs)
	}
	if !argContains(scopeArgs, testProjectID) {
		t.Errorf("scope arguments %v do not carry the project", scopeArgs)
	}

	if _, err := tx.Exec(context.Background(), store.Statement{SQL: "SELECT 1", Args: []any{"value-1"}}); err != nil {
		t.Fatalf("Exec after Begin: %v", err)
	}
	after := conn.callsCopy()
	firstStatement := indexOfSQL(after, "SELECT 1")
	if firstStatement < 0 {
		t.Fatal("the caller statement was not executed")
	}
	for index, call := range after {
		if call.kind == "exec" && strings.Contains(strings.ToLower(call.sql), "set_config") && index > firstStatement {
			t.Error("scope was established after the caller statement")
		}
	}
}

func TestBeginTenantScopeSetsAnEmptyProject(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testTenantScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	calls := db.conn(t).callsCopy()
	var args []any
	var text strings.Builder
	for _, call := range scopeCalls(calls) {
		text.WriteString(strings.ToLower(call.sql))
		text.WriteString(" ")
		args = append(args, call.args...)
	}
	if !strings.Contains(text.String(), "kodama.project_id") {
		t.Fatalf("tenant scope does not reset the project setting: %s", text.String())
	}
	if !argContains(args, "") && !argContains(args, nil) {
		t.Errorf("tenant-scoped transaction must pass an empty project to the project setting; args=%v", args)
	}
	if !argContains(args, testTenantID) {
		t.Errorf("tenant scope args %v do not carry the tenant", args)
	}
}

func TestBeginRefusesInvalidScopeBeforeDatabaseCall(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), tenancy.Scope{})
	if tx != nil {
		t.Fatal("Begin returned a transaction for an invalid scope")
	}
	assertFaultClass(t, err, fault.InvalidInput)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("Begin opened %d database transactions for an invalid scope", got)
	}
}

func TestBeginRefusesCanceledContextBeforeDatabaseCall(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := connector.Begin(ctx, testProjectScope())
	if tx != nil {
		t.Fatal("Begin returned a transaction for a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Begin error = %v; want context.Canceled", err)
	}
	if got := db.beginCount(); got != 0 {
		t.Fatalf("Begin opened %d database transactions after cancellation", got)
	}
}

func TestBeginFailureReturnsTheCause(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("dial failed")
	db.beginErr = injected
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if tx != nil {
		t.Fatal("Begin returned a transaction after the database failed")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("Begin error = %v; want the injected cause", err)
	}
}

func TestBeginScopeFailureRollsBackTheConnection(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("scope statement failed")
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if strings.Contains(strings.ToLower(sql), "set_config") {
			return store.Result{}, injected
		}
		return store.Result{RowsAffected: 1}, nil
	})
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if tx != nil {
		t.Fatal("Begin returned a transaction after scope setup failed")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("Begin error = %v; want the injected cause", err)
	}
	conn := db.conn(t)
	if conn.rollbacks != 1 {
		t.Fatalf("scope failure rolled back %d times; want 1", conn.rollbacks)
	}
	if conn.commits != 0 {
		t.Fatalf("scope failure committed %d times", conn.commits)
	}
}

func TestTransactionCommitClosesTheTransaction(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls after Commit: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
	}
	assertNoTransaction(t, "Exec", func() error {
		_, err := tx.Exec(context.Background(), store.Statement{SQL: "SELECT 1"})
		return err
	})
	assertNoTransaction(t, "Query", func() error {
		_, err := tx.Query(context.Background(), store.Query{SQL: "SELECT 1"})
		return err
	})
	assertNoTransaction(t, "Commit", func() error { return tx.Commit(context.Background()) })
	assertNoTransaction(t, "Rollback", func() error { return tx.Rollback(context.Background()) })
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("a closed transaction issued another terminal call: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
	}
}

func TestTransactionRollbackClosesTheTransaction(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	conn := db.conn(t)
	if conn.rollbacks != 1 || conn.commits != 0 {
		t.Fatalf("terminal calls after Rollback: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
	}
	assertNoTransaction(t, "Exec", func() error {
		_, err := tx.Exec(context.Background(), store.Statement{SQL: "SELECT 1"})
		return err
	})
	assertNoTransaction(t, "Commit", func() error { return tx.Commit(context.Background()) })
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("a closed transaction issued another terminal call: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
	}
}

func assertNoTransaction(t *testing.T, operation string, call func() error) {
	t.Helper()
	err := call()
	if !errors.Is(err, ErrNoTransaction) {
		t.Fatalf("%s after transaction completion = %v; want ErrNoTransaction", operation, err)
	}
}

func TestCommitFailureAttemptsRollbackAndCloses(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("commit outcome unknown")
	db.script.commitErr = injected
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Commit(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Commit error = %v; want the injected cause", err)
	}
	conn := db.conn(t)
	if conn.commits != 1 {
		t.Fatalf("Commit attempted %d commits; want 1", conn.commits)
	}
	if conn.rollbacks != 1 {
		t.Fatalf("a failed commit attempted %d rollbacks; want 1 cleanup rollback", conn.rollbacks)
	}
	assertNoTransaction(t, "second Commit", func() error { return tx.Commit(context.Background()) })
	assertNoTransaction(t, "Exec", func() error {
		_, err := tx.Exec(context.Background(), store.Statement{SQL: "SELECT 1"})
		return err
	})
	if conn.commits != 1 {
		t.Fatalf("a failed commit was retried: commits=%d", conn.commits)
	}
}

func TestRollbackFailureStillClosesTheTransaction(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("rollback failed")
	db.script.rollbackErr = injected
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Rollback error = %v; want the injected cause", err)
	}
	conn := db.conn(t)
	if conn.rollbacks != 1 {
		t.Fatalf("rollback attempted %d times; want 1", conn.rollbacks)
	}
	assertNoTransaction(t, "Exec", func() error {
		_, err := tx.Exec(context.Background(), store.Statement{SQL: "SELECT 1"})
		return err
	})
}

func TestExecAndQueryPassParametersThrough(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	statement := store.Statement{SQL: "UPDATE tasks SET state = $1 WHERE id = $2", Args: []any{"waiting", "task-1"}}
	if _, err := tx.Exec(context.Background(), statement); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	calls := execCalls(db.conn(t).callsCopy())
	last := calls[len(calls)-1]
	if last.sql != statement.SQL {
		t.Fatalf("Exec SQL = %q; want %q", last.sql, statement.SQL)
	}
	if !equalAny(last.args, statement.Args) {
		t.Fatalf("Exec args = %v; want %v", last.args, statement.Args)
	}

	query := store.Query{SQL: "SELECT id FROM tasks WHERE id = $1", Args: []any{"task-1"}}
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if rows == nil {
		t.Fatal("Query returned nil rows")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close rows: %v", err)
	}
	queries := queryCalls(db.conn(t).callsCopy())
	lastQuery := queries[len(queries)-1]
	if lastQuery.sql != query.SQL || !equalAny(lastQuery.args, query.Args) {
		t.Fatalf("Query = %q %v; want %q %v", lastQuery.sql, lastQuery.args, query.SQL, query.Args)
	}
}

func equalAny(left, right []any) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !argContains([]any{left[index]}, right[index]) {
			return false
		}
	}
	return true
}

func TestEachBeginGetsAFreshTransactionLocalScope(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	first, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if err := first.Commit(context.Background()); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	second, err := connector.Begin(context.Background(), tenancy.Scope{TenantID: testTenantID, ProjectID: "project-2"})
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	defer func() { _ = second.Rollback(context.Background()) }()

	conns := db.connsCopy()
	if len(conns) != 2 {
		t.Fatalf("Begin opened %d connections; want 2", len(conns))
	}
	var args []any
	for _, call := range scopeCalls(conns[1].callsCopy()) {
		args = append(args, call.args...)
	}
	if !argContains(args, "project-2") {
		t.Errorf("second transaction scope args %v do not carry project-2", args)
	}
	if argContains(args, testProjectID) {
		t.Errorf("second transaction reused the first transaction's project scope: %v", args)
	}
}
