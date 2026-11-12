package pgtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
)

// PgError returns the PostgreSQL server error carried by err, if any. The
// product wrapper keeps its private cause reachable through errors.As, so a
// conformance test can assert the exact SQLSTATE and constraint name instead of
// a rendered message substring.
func PgError(err error) (*pgconn.PgError, bool) {
	if err == nil {
		return nil, false
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr, true
	}
	return nil, false
}

// SQLState returns the PostgreSQL SQLSTATE carried by err. The primary path is
// errors.As over the wrapped cause; the bounded fallback reads the "(SQLSTATE
// xxxxx)" marker pgx renders when a driver error is not a *pgconn.PgError.
func SQLState(err error) (string, bool) {
	if pgErr, ok := PgError(err); ok && pgErr.Code != "" {
		return pgErr.Code, true
	}
	text := CauseText(err)
	const marker = "SQLSTATE "
	_, after, ok := strings.Cut(text, marker)
	if !ok {
		return "", false
	}
	rest := after
	code := rest
	if end := strings.IndexAny(rest, ") \n\t"); end >= 0 {
		code = rest[:end]
	}
	if len(code) != 5 {
		return "", false
	}
	return code, true
}

// RequireSQLState fails unless err is a PostgreSQL error with exactly code.
func RequireSQLState(t testing.TB, err error, code, label string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: statement succeeded; want SQLSTATE %s", label, code)
	}
	got, ok := SQLState(err)
	if !ok {
		t.Fatalf("%s: error %v carries no SQLSTATE; want %s", label, err, code)
	}
	if got != code {
		t.Fatalf("%s: SQLSTATE = %s; want %s (%v)", label, got, code, err)
	}
	pgErr, _ := PgError(err)
	return pgErr
}

// RequirePermissionDenied fails unless err is the server's privilege denial
// (SQLSTATE 42501 with a "permission denied" message). The message check is
// deliberate: a row-level-security refusal also reports 42501, and a grant test
// must prove the grant, not the policy, is what refused the statement.
func RequirePermissionDenied(t testing.TB, err error, label string) {
	t.Helper()
	pgErr := RequireSQLState(t, err, "42501", label)
	text := CauseText(err)
	if pgErr != nil {
		text = pgErr.Message
	}
	if !strings.Contains(strings.ToLower(text), "permission denied") {
		t.Fatalf("%s: SQLSTATE 42501 but message = %q; want a privilege denial", label, text)
	}
}

// RequireRLSViolation fails unless err is a row-level-security refusal
// (SQLSTATE 42501 with an RLS message), never a plain privilege denial.
func RequireRLSViolation(t testing.TB, err error, label string) {
	t.Helper()
	pgErr := RequireSQLState(t, err, "42501", label)
	text := CauseText(err)
	if pgErr != nil {
		text = pgErr.Message
	}
	if !strings.Contains(strings.ToLower(text), "row-level security") {
		t.Fatalf("%s: SQLSTATE 42501 but message = %q; want a row-level-security refusal", label, text)
	}
}

// RequireForeignKeyViolation fails unless err is SQLSTATE 23503 raised by the
// named constraint. Asserting the constraint name is what distinguishes the
// composite ownership foreign key from an unrelated single-column reference.
func RequireForeignKeyViolation(t testing.TB, err error, constraint, label string) {
	t.Helper()
	pgErr := RequireSQLState(t, err, "23503", label)
	if pgErr != nil {
		if pgErr.ConstraintName != constraint {
			t.Fatalf("%s: violated constraint = %q; want %q", label, pgErr.ConstraintName, constraint)
		}
		return
	}
	if !strings.Contains(CauseText(err), `constraint "`+constraint+`"`) {
		t.Fatalf("%s: error %v does not name constraint %q", label, err, constraint)
	}
}

// RequireNonOwnerSession proves the connection is the expected non-owner,
// non-superuser, NOBYPASSRLS role with row security on. Every security test
// starts with this so a superuser or a bypass role cannot make a refusal pass
// for the wrong reason.
func RequireNonOwnerSession(t testing.TB, ctx context.Context, db *pgdriver.DB, wantRole string) {
	t.Helper()
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("open %s session: %v", wantRole, err)
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := QueryRows(ctx, conn, 4,
		`SELECT current_user, current_setting('row_security'),
		        r.rolbypassrls::text, r.rolsuper::text
		 FROM pg_roles r WHERE r.rolname = current_user`)
	if err != nil {
		t.Fatalf("read %s identity: %v", wantRole, err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %s identity: %d rows; want 1", wantRole, len(rows))
	}
	row := rows[0]
	if row[0] != wantRole {
		t.Fatalf("session role = %q; want %q", row[0], wantRole)
	}
	if row[1] != "on" {
		t.Fatalf("row_security = %q; want on", row[1])
	}
	if row[2] != "false" || row[3] != "false" {
		t.Fatalf("%s bypassrls=%s super=%s; want both false", wantRole, row[2], row[3])
	}
}

// RequireRowCount fails unless a count query returns want. It is a small helper
// so a security test can assert an unchanged durable count in one line.
func RequireRowCount(t testing.TB, ctx context.Context, db *pgdriver.DB, query string, want int64, label string, args ...any) {
	t.Helper()
	got, err := ScalarInt(ctx, db, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s = %d; want %d", label, got, want)
	}
}
