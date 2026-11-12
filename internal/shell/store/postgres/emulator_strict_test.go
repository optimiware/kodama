// The emulator must reject the malformed predicate shapes PostgreSQL rejects,
// so a rendered statement with an empty WHERE, a trailing AND/OR or a missing
// predicate cannot pass a seam vector.
package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEmulatorRejectsMalformedPredicates(t *testing.T) {
	db := newEmuDB("kodama_controller", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin emulator transaction: %v", err)
	}
	defer func() { _ = conn.Rollback(ctx) }()

	malformedQueries := []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM "events" WHERE  ORDER BY "id" ASC LIMIT $1`, []any{10}},
		{`SELECT id FROM "events" WHERE ORDER BY "id" ASC LIMIT $1`, []any{10}},
		{`SELECT id FROM "events" WHERE id = $1 AND ORDER BY "id" ASC LIMIT $2`, []any{"event-1", 10}},
		{`SELECT id FROM "events" WHERE id = $1 AND`, []any{"event-1"}},
		{`SELECT id FROM "events" WHERE id = $1 OR`, []any{"event-1"}},
		{`SELECT id FROM "events" WHERE`, nil},
	}
	for _, test := range malformedQueries {
		rows, err := conn.Query(ctx, test.sql, test.args...)
		if rows != nil {
			_ = rows.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "malformed predicate") {
			t.Errorf("query %q error = %v; want malformed predicate rejection", test.sql, err)
		}
	}

	malformedWrites := []struct {
		sql  string
		args []any
	}{
		{`UPDATE "events" SET "type" = $1 WHERE `, []any{"type"}},
		{`UPDATE "events" SET "type" = $1 WHERE id = $2 AND`, []any{"type", "event-1"}},
		{`DELETE FROM "events" WHERE `, nil},
		{`DELETE FROM "events" WHERE id = $1 OR`, []any{"event-1"}},
	}
	for _, test := range malformedWrites {
		_, err := conn.Exec(ctx, test.sql, test.args...)
		if err == nil || !strings.Contains(err.Error(), "malformed predicate") {
			t.Errorf("exec %q error = %v; want malformed predicate rejection", test.sql, err)
		}
	}

	// A valid predicate and a genuinely unfiltered select still work.
	rows, err := conn.Query(ctx, `SELECT id FROM "events" WHERE id = $1`, "event-1")
	if err != nil {
		t.Fatalf("valid predicate was rejected: %v", err)
	}
	_ = rows.Close()
	rows, err = conn.Query(ctx, `SELECT id FROM "events"`)
	if err != nil {
		t.Fatalf("unfiltered select was rejected: %v", err)
	}
	_ = rows.Close()
}
