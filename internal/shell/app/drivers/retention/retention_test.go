package retention

// Retained-commit discovery delegates every decision to the sweeper's
// evidence rule. These cases mirror the sweeper's boundary behavior, so
// the keep-ref source can never grow a second retention window.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
)

func TestRetainedCommitUsesTheEvidenceRule(t *testing.T) {
	now := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := installation.RetentionPolicy{}
	candidate := "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name      string
		createdAt time.Time
		active    bool
		want      bool
	}{
		{name: "inside the 90-day window", createdAt: now.Add(-89 * 24 * time.Hour), want: true},
		{name: "exactly at the boundary", createdAt: now.Add(-90 * 24 * time.Hour), want: true},
		{name: "older and unreferenced", createdAt: now.Add(-91 * 24 * time.Hour), want: false},
		{name: "older but actively referenced", createdAt: now.Add(-400 * 24 * time.Hour), active: true, want: true},
		{name: "created after now", createdAt: now.Add(time.Hour), want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := retainedCommit(policy, candidate, testCase.createdAt, now, testCase.active)
			if err != nil {
				t.Fatalf("retainedCommit: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("retained = %v; want %v", got, testCase.want)
			}
		})
	}

	if retained, err := retainedCommit(policy, "", now, now, false); err != nil || retained {
		t.Fatalf("empty candidate retained=%v err=%v; want false/nil", retained, err)
	}
}

type fakeRetentionDB struct {
	conns []*fakeRetentionConn
	rows  []installation.RetentionCandidate
	aff   int64
}

func (db *fakeRetentionDB) Begin(context.Context) (postgres.Conn, error) {
	conn := &fakeRetentionConn{rows: db.rows, aff: db.aff}
	db.conns = append(db.conns, conn)
	return conn, nil
}

type fakeRetentionConn struct {
	execSQL   []string
	execArgs  [][]any
	querySQL  []string
	queryArgs [][]any
	committed bool
	rows      []installation.RetentionCandidate
	aff       int64
}

func (conn *fakeRetentionConn) Exec(_ context.Context, sql string, args ...any) (store.Result, error) {
	conn.execSQL = append(conn.execSQL, sql)
	conn.execArgs = append(conn.execArgs, append([]any(nil), args...))
	if strings.Contains(sql, "DELETE FROM") {
		return store.Result{RowsAffected: conn.aff}, nil
	}
	return store.Result{}, nil
}

func (conn *fakeRetentionConn) Query(_ context.Context, sql string, args ...any) (store.Rows, error) {
	conn.querySQL = append(conn.querySQL, sql)
	conn.queryArgs = append(conn.queryArgs, append([]any(nil), args...))
	return &fakeRetentionRows{items: conn.rows, index: -1}, nil
}

func (conn *fakeRetentionConn) Commit(context.Context) error {
	conn.committed = true
	return nil
}

func (conn *fakeRetentionConn) Rollback(context.Context) error {
	return nil
}

type fakeRetentionRows struct {
	items []installation.RetentionCandidate
	index int
}

func (rows *fakeRetentionRows) Next() bool {
	rows.index++
	return rows.index < len(rows.items)
}

func (rows *fakeRetentionRows) Scan(dest ...any) error {
	if len(dest) != 3 {
		return fmt.Errorf("unexpected scan arity %d", len(dest))
	}
	item := rows.items[rows.index]
	*(dest[0].(*string)) = item.ID
	*(dest[1].(*time.Time)) = item.CreatedAt
	*(dest[2].(*bool)) = item.ActiveReferences
	return nil
}

func (rows *fakeRetentionRows) Err() error   { return nil }
func (rows *fakeRetentionRows) Close() error { return nil }

func TestRawContentCandidatesAndDeleteAreProjectScopedAndGuarded(t *testing.T) {
	now := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	before := now.Add(-7 * 24 * time.Hour)
	recorded := now.Add(-10 * 24 * time.Hour)

	db := &fakeRetentionDB{
		rows: []installation.RetentionCandidate{
			{ID: "content-1", CreatedAt: recorded, ActiveReferences: false},
			{ID: "content-2", CreatedAt: recorded, ActiveReferences: true},
		},
		aff: 1,
	}
	adapter := NewRetentionStore(db)

	// Tenant-wide scopes never touch content_records.
	tenantScope := installation.RetentionScope{TenantID: "tenant-1"}
	if got, err := adapter.Candidates(context.Background(), tenantScope, installation.RetentionRawContent, before); err != nil || len(got) != 0 {
		t.Fatalf("tenant-wide Candidates = %+v, %v; want empty", got, err)
	}
	if deleted, err := adapter.Delete(context.Background(), tenantScope, installation.RetentionRawContent, installation.RetentionCandidate{ID: "content-1", CreatedAt: recorded}, before); err != nil || deleted {
		t.Fatalf("tenant-wide Delete = %v, %v; want false, nil", deleted, err)
	}
	if len(db.conns) != 0 {
		t.Fatalf("tenant-wide raw_content opened %d DB transactions; want 0", len(db.conns))
	}

	projectScope := installation.RetentionScope{TenantID: "tenant-1", ProjectID: "project-1"}
	candidates, err := adapter.Candidates(context.Background(), projectScope, installation.RetentionRawContent, before)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0].ID != "content-1" || candidates[0].ActiveReferences || !candidates[1].ActiveReferences {
		t.Fatalf("Candidates = %+v; want content-1 inactive and content-2 active", candidates)
	}
	scanConn := db.conns[0]
	if len(scanConn.querySQL) != 1 || !strings.Contains(scanConn.querySQL[0], "FROM content_records") || !strings.Contains(scanConn.querySQL[0], "retain_until > $4") {
		t.Fatalf("unexpected candidate SQL: %v", scanConn.querySQL)
	}
	if got := scanConn.queryArgs[0]; len(got) != 4 || got[0] != "tenant-1" || got[1] != "project-1" || !got[2].(time.Time).Equal(before) || !got[3].(time.Time).Equal(now) {
		t.Fatalf("candidate args = %+v; want tenant-1, project-1, %v, %v", got, before, now)
	}

	deleted, err := adapter.Delete(context.Background(), projectScope, installation.RetentionRawContent, candidates[0], before)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !deleted {
		t.Fatal("Delete reported false when 1 row was affected")
	}
	deleteConn := db.conns[1]
	if !deleteConn.committed || len(deleteConn.execSQL) != 2 {
		t.Fatalf("delete transaction committed=%v execCount=%d; want committed with scope+delete", deleteConn.committed, len(deleteConn.execSQL))
	}
	deleteSQL := deleteConn.execSQL[1]
	if !strings.Contains(deleteSQL, "DELETE FROM content_records") || !strings.Contains(deleteSQL, "retain_until <= $6") || !strings.Contains(deleteSQL, "NOT EXISTS") {
		t.Fatalf("unexpected delete SQL: %s", deleteSQL)
	}
	if got := deleteConn.execArgs[1]; len(got) != 6 || got[0] != "content-1" || got[1] != "tenant-1" || got[2] != "project-1" || !got[3].(time.Time).Equal(recorded) || !got[4].(time.Time).Equal(before) || !got[5].(time.Time).Equal(now) {
		t.Fatalf("delete args = %+v", got)
	}
}
