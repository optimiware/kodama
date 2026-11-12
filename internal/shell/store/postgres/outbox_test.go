// Outbox behavior over the publisher-role emulator: durable sequence order,
// late lower-sequence visibility, bounded pagination and exact-id acknowledgement.
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func seedOutboxEvent(t *testing.T, db *emuDB, eventID string, sequence int64, state string) {
	t.Helper()
	now := db.now
	db.seed("events", map[string]any{
		"id":               eventID,
		"tenant_id":        "tenant-1",
		"project_id":       "project-1",
		"type":             string(event.TypeExecutionSucceeded),
		"subject_id":       "execution-1",
		"source_kind":      string(event.SourceShell),
		"source_principal": "principal-1",
		"at":               now,
		"publish_state":    state,
		"publish_sequence": sequence,
		"version":          int64(1),
	})
}

func TestOutboxPendingIncludesLateLowerSequenceRows(t *testing.T) {
	db := newEmuDB("kodama_publisher", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seedOutboxEvent(t, db, "event-high", 5, "pending")
	seedOutboxEvent(t, db, "event-late", 2, "pending")
	seedOutboxEvent(t, db, "event-published", 1, "published")
	outbox := NewOutbox(db)

	pending, err := outbox.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending rows = %d; want the two unacknowledged rows", len(pending))
	}
	if pending[0].Sequence != 2 || pending[1].Sequence != 5 {
		t.Fatalf("pending sequences = %d,%d; want durable order 2,5", pending[0].Sequence, pending[1].Sequence)
	}
	if pending[0].Event.ID != "event-late" {
		t.Errorf("first pending event = %q; want the late lower-sequence row", pending[0].Event.ID)
	}
	if pending[0].Event.Scope.TenantID != "tenant-1" || pending[0].Event.Scope.ProjectID != "project-1" {
		t.Errorf("pending event scope = %+v", pending[0].Event.Scope)
	}
}

func TestOutboxPendingCursorIsBoundedAndRestartScansFromZero(t *testing.T) {
	db := newEmuDB("kodama_publisher", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seedOutboxEvent(t, db, "event-1", 1, "pending")
	seedOutboxEvent(t, db, "event-2", 2, "pending")
	seedOutboxEvent(t, db, "event-3", 3, "pending")
	outbox := NewOutbox(db)

	page, err := outbox.Pending(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("Pending after cursor: %v", err)
	}
	if len(page) != 1 || page[0].Sequence != 2 {
		t.Fatalf("bounded page = %+v; want sequence 2", page)
	}
	restart, err := outbox.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("restart Pending: %v", err)
	}
	if len(restart) != 3 {
		t.Fatalf("restart pending = %d; want all three rows from zero", len(restart))
	}
}

func TestOutboxMarkPublishedAcknowledgesExactIdentity(t *testing.T) {
	db := newEmuDB("kodama_publisher", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seedOutboxEvent(t, db, "event-1", 1, "pending")
	seedOutboxEvent(t, db, "event-2", 2, "pending")
	outbox := NewOutbox(db)
	ctx := context.Background()

	if err := outbox.MarkPublished(ctx, id.EventID("event-1"), event.Sequence(2)); err == nil {
		t.Fatal("acknowledging the wrong sequence succeeded")
	} else if class, _ := fault.ClassOf(err); class != fault.NotFound {
		t.Fatalf("wrong-sequence acknowledgement class = %s; want not_found", class)
	}
	if err := outbox.MarkPublished(ctx, id.EventID("event-1"), event.Sequence(1)); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	pending, err := outbox.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Pending after acknowledgement: %v", err)
	}
	if len(pending) != 1 || pending[0].Event.ID != "event-2" {
		t.Fatalf("pending after acknowledgement = %+v; want only event-2", pending)
	}
	// A retry after a lost acknowledgement is idempotent.
	if err := outbox.MarkPublished(ctx, id.EventID("event-1"), event.Sequence(1)); err != nil {
		t.Fatalf("idempotent MarkPublished: %v", err)
	}
}

func TestOutboxPendingRefusesNonPositiveLimit(t *testing.T) {
	outbox := NewOutbox(newEmuDB("kodama_publisher", time.Now()))
	if _, err := outbox.Pending(context.Background(), 0, 0); err == nil {
		t.Fatal("a zero limit was accepted")
	} else if class, _ := fault.ClassOf(err); class != fault.InvalidInput {
		t.Fatalf("zero-limit class = %s; want invalid_input", class)
	}
}

// strictCloseDB hands out connections whose Rows iterators do not auto-close on
// exhaustion, so an adapter that forgets an explicit Close before Commit or
// ignores a Close/post-Close error is caught directly.
type strictCloseDB struct {
	queryFn func(sql string, args []any) (*strictCloseRows, error)
	execFn  func(sql string, args []any) (store.Result, error)
	conns   []*strictCloseConn
}

func (db *strictCloseDB) Begin(_ context.Context) (Conn, error) {
	conn := &strictCloseConn{db: db}
	db.conns = append(db.conns, conn)
	return conn, nil
}

type strictCloseConn struct {
	db        *strictCloseDB
	openRows  int
	commits   int
	rollbacks int
}

func (conn *strictCloseConn) Exec(_ context.Context, sql string, args ...any) (store.Result, error) {
	if conn.openRows > 0 {
		return store.Result{}, errConnBusy
	}
	if conn.db.execFn != nil {
		return conn.db.execFn(sql, args)
	}
	return store.Result{RowsAffected: 1}, nil
}

func (conn *strictCloseConn) Query(_ context.Context, sql string, args ...any) (store.Rows, error) {
	if conn.openRows > 0 {
		return nil, errConnBusy
	}
	rows, err := conn.db.queryFn(sql, args)
	if err != nil {
		return nil, err
	}
	conn.openRows++
	rows.onClose = func() { conn.openRows-- }
	return rows, nil
}

func (conn *strictCloseConn) Commit(_ context.Context) error {
	if conn.openRows > 0 {
		return errConnBusy
	}
	conn.commits++
	return nil
}

func (conn *strictCloseConn) Rollback(_ context.Context) error {
	if conn.openRows > 0 {
		return errConnBusy
	}
	conn.rollbacks++
	return nil
}

type strictCloseRows struct {
	inner         store.Rows
	closeErr      error
	errAfterClose error
	closed        bool
	onClose       func()
}

func (rows *strictCloseRows) Next() bool {
	if rows.closed {
		return false
	}
	return rows.inner.Next()
}

func (rows *strictCloseRows) Scan(dest ...any) error {
	return rows.inner.Scan(dest...)
}

func (rows *strictCloseRows) Err() error {
	if rows.closed && rows.errAfterClose != nil {
		return rows.errAfterClose
	}
	return rows.inner.Err()
}

func (rows *strictCloseRows) Close() error {
	if !rows.closed {
		rows.closed = true
		if rows.onClose != nil {
			rows.onClose()
		}
	}
	return rows.closeErr
}

func TestOutboxExplicitlyClosesRowsBeforeCommit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("Pending and MarkPublished close non-autoclosed rows before Commit", func(t *testing.T) {
		db := &strictCloseDB{
			execFn: func(sql string, _ []any) (store.Result, error) {
				if sql == beginReadOnlySQL {
					return store.Result{RowsAffected: 1}, nil
				}
				return store.Result{RowsAffected: 0}, nil
			},
			queryFn: func(sql string, _ []any) (*strictCloseRows, error) {
				if sql == outboxPendingSQL {
					inner := singleRow("events", sql, map[string]any{
						"id":               "event-1",
						"tenant_id":        "tenant-1",
						"project_id":       "project-1",
						"type":             string(event.TypeExecutionSucceeded),
						"subject_id":       "execution-1",
						"source_kind":      string(event.SourceShell),
						"source_principal": "principal-1",
						"at":               now,
						"publish_sequence": int64(1),
					})
					return &strictCloseRows{inner: inner}, nil
				}
				inner := &fakeRows{rows: [][]any{{"published"}}, index: -1}
				return &strictCloseRows{inner: inner}, nil
			},
		}
		outbox := NewOutbox(db)
		pending, err := outbox.Pending(ctx, 0, 10)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) != 1 || pending[0].Event.ID != "event-1" {
			t.Fatalf("Pending = %+v; want [event-1]", pending)
		}
		if err := outbox.MarkPublished(ctx, id.EventID("event-1"), event.Sequence(1)); err != nil {
			t.Fatalf("MarkPublished: %v", err)
		}
		if db.conns[0].commits != 1 || db.conns[1].commits != 1 {
			t.Fatalf("commits = %d, %d; want 1, 1", db.conns[0].commits, db.conns[1].commits)
		}
	})

	t.Run("Pending closes rows before commit and checks Close error", func(t *testing.T) {
		closeBoom := errors.New("rows close failed")
		db := &strictCloseDB{
			queryFn: func(sql string, _ []any) (*strictCloseRows, error) {
				inner := singleRow("events", sql, map[string]any{
					"id":               "event-1",
					"tenant_id":        "tenant-1",
					"project_id":       "project-1",
					"type":             string(event.TypeExecutionSucceeded),
					"subject_id":       "execution-1",
					"source_kind":      string(event.SourceShell),
					"source_principal": "principal-1",
					"at":               now,
					"publish_sequence": int64(1),
				})
				return &strictCloseRows{inner: inner, closeErr: closeBoom}, nil
			},
		}
		_, err := NewOutbox(db).Pending(ctx, 0, 10)
		if !errors.Is(err, closeBoom) {
			t.Fatalf("Pending error = %v; want %v", err, closeBoom)
		}
		if db.conns[0].commits != 0 || db.conns[0].rollbacks != 1 {
			t.Fatalf("Pending terminal calls: commits=%d rollbacks=%d; want 0/1", db.conns[0].commits, db.conns[0].rollbacks)
		}
	})

	t.Run("Pending checks rows.Err surfaced on Close before commit", func(t *testing.T) {
		trailerBoom := errors.New("rows trailer failed")
		db := &strictCloseDB{
			queryFn: func(_ string, _ []any) (*strictCloseRows, error) {
				return &strictCloseRows{inner: &fakeRows{index: -1}, errAfterClose: trailerBoom}, nil
			},
		}
		_, err := NewOutbox(db).Pending(ctx, 0, 10)
		if !errors.Is(err, trailerBoom) {
			t.Fatalf("Pending error = %v; want %v", err, trailerBoom)
		}
		if db.conns[0].commits != 0 || db.conns[0].rollbacks != 1 {
			t.Fatalf("Pending terminal calls: commits=%d rollbacks=%d; want 0/1", db.conns[0].commits, db.conns[0].rollbacks)
		}
	})

	t.Run("MarkPublished eventAlreadyPublished closes rows before Err and Commit", func(t *testing.T) {
		closeBoom := errors.New("recheck close failed")
		db := &strictCloseDB{
			execFn: func(_ string, _ []any) (store.Result, error) {
				return store.Result{RowsAffected: 0}, nil
			},
			queryFn: func(_ string, _ []any) (*strictCloseRows, error) {
				inner := &fakeRows{rows: [][]any{{"published"}}, index: -1}
				return &strictCloseRows{inner: inner, closeErr: closeBoom}, nil
			},
		}
		err := NewOutbox(db).MarkPublished(ctx, id.EventID("event-1"), event.Sequence(1))
		if !errors.Is(err, closeBoom) {
			t.Fatalf("MarkPublished error = %v; want %v", err, closeBoom)
		}
		if db.conns[0].commits != 0 || db.conns[0].rollbacks != 1 {
			t.Fatalf("MarkPublished terminal calls: commits=%d rollbacks=%d; want 0/1", db.conns[0].commits, db.conns[0].rollbacks)
		}
	})
}
