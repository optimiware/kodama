// Controller.Read must release its rows iterator before it commits the
// read-only transaction. pgx refuses to complete or reuse a connection while a
// Rows iterator is still open ("conn busy"), and every engine read goes through
// Controller.Read, so an open iterator at commit breaks every real read.
//
// The emulator and the fake model that refusal for every transaction, so the
// same defect class is caught on every path, not only on this one.
package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// TestControllerReadCommitsAfterClosingRows guards against scanning the single
// row and then committing the transaction while the iterator is still open,
// which real PostgreSQL via pgx reports as "conn busy".
func TestControllerReadCommitsAfterClosingRows(t *testing.T) {
	now := controllerEventClock()
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: controllerEventSeedRows()}, now)
	controller := NewController(db)

	row, err := controller.Read(context.Background(), "executions", "execution-1")
	if err != nil {
		t.Fatalf("Controller.Read: %v", err)
	}
	if row.Table != "executions" {
		t.Fatalf("Controller.Read table = %q; want executions", row.Table)
	}
	if identity := emuString(row.Values["id"]); identity != "execution-1" {
		t.Fatalf("Controller.Read id = %q; want execution-1", identity)
	}

	conn := db.conns[len(db.conns)-1]
	if !conn.closed {
		t.Error("Controller.Read did not complete its transaction")
	}
	if conn.openRows.busy() {
		t.Error("Controller.Read completed its transaction with a rows iterator still open")
	}
}

// TestControllerReadMissingRowIsNotFound pins the not-found semantics across
// the rows-close change: a missing id still returns a not_found fault and
// completes no transaction with an open iterator.
func TestControllerReadMissingRowIsNotFound(t *testing.T) {
	now := controllerEventClock()
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: controllerEventSeedRows()}, now)
	controller := NewController(db)

	_, err := controller.Read(context.Background(), "executions", "execution-missing")
	if err == nil {
		t.Fatal("Controller.Read of a missing row returned no error")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.NotFound {
		t.Fatalf("Controller.Read missing row = %v; want a not_found fault", err)
	}
	conn := db.conns[len(db.conns)-1]
	if conn.openRows.busy() {
		t.Error("Controller.Read left a rows iterator open on the not-found path")
	}
}

// TestEmulatorRefusesCommitWithOpenRows proves the emulator models pgx's
// refusal instead of silently accepting a transaction completed mid-iteration.
func TestEmulatorRefusesCommitWithOpenRows(t *testing.T) {
	ctx := context.Background()
	db := newEmuDB("kodama_controller", controllerEventClock())
	db.seed("executions", map[string]any{"id": "execution-1", "tenant_id": "tenant-1", "project_id": "project-1", "version": int64(1)})
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin emulator transaction: %v", err)
	}

	rows, err := conn.Query(ctx, `SELECT id FROM "executions" WHERE id = $1`, "execution-1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !rows.Next() {
		t.Fatal("query returned no row")
	}
	if err := conn.Commit(ctx); err == nil || !strings.Contains(err.Error(), "conn busy") {
		t.Fatalf("commit with an open iterator = %v; want a conn busy refusal", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close rows: %v", err)
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("commit after closing rows: %v", err)
	}
}

// TestEmulatorRefusesRollbackWithOpenRows mirrors the commit rule for rollback:
// pgx refuses to reuse the connection while an iterator is open.
func TestEmulatorRefusesRollbackWithOpenRows(t *testing.T) {
	ctx := context.Background()
	db := newEmuDB("kodama_controller", controllerEventClock())
	db.seed("executions", map[string]any{"id": "execution-1", "tenant_id": "tenant-1", "project_id": "project-1", "version": int64(1)})
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin emulator transaction: %v", err)
	}

	rows, err := conn.Query(ctx, `SELECT id FROM "executions" WHERE id = $1`, "execution-1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !rows.Next() {
		t.Fatal("query returned no row")
	}
	if err := conn.Rollback(ctx); err == nil || !strings.Contains(err.Error(), "conn busy") {
		t.Fatalf("rollback with an open iterator = %v; want a conn busy refusal", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close rows: %v", err)
	}
	if err := conn.Rollback(ctx); err != nil {
		t.Fatalf("rollback after closing rows: %v", err)
	}
}

// TestEmulatorExhaustedRowsAllowCommit proves the emulator matches pgx's
// automatic close on exhaustion: an adapter that drains its iterator (the
// outbox and scanner paths) may complete the transaction without an explicit
// close first.
func TestEmulatorExhaustedRowsAllowCommit(t *testing.T) {
	ctx := context.Background()
	db := newEmuDB("kodama_controller", controllerEventClock())
	db.seed("executions", map[string]any{"id": "execution-1", "tenant_id": "tenant-1", "project_id": "project-1", "version": int64(1)})
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin emulator transaction: %v", err)
	}

	rows, err := conn.Query(ctx, `SELECT id FROM "executions" WHERE id = $1`, "execution-1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain rows: %v", err)
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("commit after draining rows: %v", err)
	}
}

// TestFakeConnRefusesCommitWithOpenRows applies the same rule to the scripted
// fake so adapter paths driven by the fake are covered too.
func TestFakeConnRefusesCommitWithOpenRows(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{
		"executions": {"id": "execution-1"},
	}))
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fake transaction: %v", err)
	}

	rows, err := conn.Query(ctx, `SELECT id FROM "executions" WHERE id = $1`, "execution-1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !rows.Next() {
		t.Fatal("query returned no row")
	}
	if err := conn.Commit(ctx); err == nil || !strings.Contains(err.Error(), "conn busy") {
		t.Fatalf("commit with an open iterator = %v; want a conn busy refusal", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close rows: %v", err)
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("commit after closing rows: %v", err)
	}
}

// TestControllerLockedExecutionScopeClosesRowsBeforeMissQuery proves that when
// the scoped FOR UPDATE re-read matches zero rows, its iterator is explicitly
// closed and checked before controllerLockedExecutionMiss issues a follow-up
// query on the same transaction.
func TestControllerLockedExecutionScopeClosesRowsBeforeMissQuery(t *testing.T) {
	withControllerEventCommit(t)
	ctx := context.Background()
	unit := controllerEventUnit(testProjectScope())

	t.Run("closes FOR UPDATE rows before miss classification query", func(t *testing.T) {
		queries := 0
		db := &strictCloseDB{
			queryFn: func(sql string, _ []any) (*strictCloseRows, error) {
				queries++
				lower := strings.ToLower(sql)
				switch {
				case strings.Contains(lower, "for update"):
					// Simulate the row moving out of scope before the locking read:
					// Next() returns false without auto-closing the iterator.
					return &strictCloseRows{inner: &fakeRows{index: -1}}, nil
				case queries == 1:
					inner := &fakeRows{rows: [][]any{{testTenantID, testProjectID, int64(2)}}, index: -1}
					return &strictCloseRows{inner: inner}, nil
				default:
					// controllerLockedExecutionMiss sees the row in another scope.
					inner := &fakeRows{rows: [][]any{{testTenantID, "project-2"}}, index: -1}
					return &strictCloseRows{inner: inner}, nil
				}
			},
		}
		_, err := NewController(db).Execute(ctx, "execution-1", unit)
		assertFaultClass(t, err, fault.Forbidden)
		if queries != 3 {
			t.Fatalf("queries issued = %d; want derive + FOR UPDATE + miss classification (3)", queries)
		}
	})

	t.Run("propagates Close error from zero-row FOR UPDATE before miss query", func(t *testing.T) {
		closeBoom := errors.New("locked scope close failed")
		queries := 0
		db := &strictCloseDB{
			queryFn: func(sql string, _ []any) (*strictCloseRows, error) {
				queries++
				if strings.Contains(strings.ToLower(sql), "for update") {
					return &strictCloseRows{inner: &fakeRows{index: -1}, closeErr: closeBoom}, nil
				}
				inner := &fakeRows{rows: [][]any{{testTenantID, testProjectID, int64(2)}}, index: -1}
				return &strictCloseRows{inner: inner}, nil
			},
		}
		_, err := NewController(db).Execute(ctx, "execution-1", unit)
		if !errors.Is(err, closeBoom) {
			t.Fatalf("Execute error = %v; want %v", err, closeBoom)
		}
		if queries != 2 {
			t.Fatalf("queries issued = %d; want miss query skipped on Close error (2)", queries)
		}
	})
}
