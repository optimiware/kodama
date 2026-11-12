package pgtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// RawConn is the narrow query surface of the pgx wrapper connection.
type RawConn interface {
	Query(context.Context, string, ...any) (store.Rows, error)
}

// QueryRows scans a known number of columns from a raw wrapper connection.
func QueryRows(ctx context.Context, conn RawConn, columns int, query string, args ...any) ([][]string, error) {
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return ScanRows(rows, columns)
}

// QueryRowsCount returns the single integer from a one-column count query.
func QueryRowsCount(ctx context.Context, conn RawConn, query string, args ...any) (int64, error) {
	rows, err := QueryRows(ctx, conn, 1, query, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("count query returned %d rows; want 1", len(rows))
	}
	var value int64
	if _, err := fmt.Sscan(rows[0][0], &value); err != nil {
		return 0, err
	}
	return value, nil
}

// ScanRows reads and closes one rows iterator.
func ScanRows(rows store.Rows, columns int) ([][]string, error) {
	defer func() { _ = rows.Close() }()
	var result [][]string
	for rows.Next() {
		values := make([]any, columns)
		pointers := make([]any, columns)
		rendered := make([]string, columns)
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for index, value := range values {
			rendered[index] = renderValue(value)
		}
		result = append(result, rendered)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryStrings runs one single-column query in its own transaction and returns
// every value rendered as text.
func QueryStrings(ctx context.Context, db *pgdriver.DB, query string, args ...any) ([][]string, error) {
	return QueryStringsN(ctx, db, 1, query, args...)
}

// QueryStringsN runs one query with a known column count.
func QueryStringsN(ctx context.Context, db *pgdriver.DB, columns int, query string, args ...any) ([][]string, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result [][]string
	for rows.Next() {
		values := make([]any, columns)
		pointers := make([]any, columns)
		stringsOut := make([]string, columns)
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for index, value := range values {
			stringsOut[index] = renderValue(value)
		}
		result = append(result, stringsOut)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := conn.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func renderValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

// QueryOne runs one query and returns the single row's rendered values.
func QueryOne(ctx context.Context, db *pgdriver.DB, columns int, query string, args ...any) ([]string, error) {
	rows, err := QueryStringsN(ctx, db, columns, query, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("query returned %d rows; want 1", len(rows))
	}
	return rows[0], nil
}

// ScalarString runs one single-column query and returns its value.
func ScalarString(ctx context.Context, db *pgdriver.DB, query string, args ...any) (string, error) {
	rows, err := QueryStringsN(ctx, db, 1, query, args...)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("query returned %d rows; want 1", len(rows))
	}
	return rows[0][0], nil
}

// ScalarInt runs one single-column query and parses the integer result.
func ScalarInt(ctx context.Context, db *pgdriver.DB, query string, args ...any) (int64, error) {
	text, err := ScalarString(ctx, db, query, args...)
	if err != nil {
		return 0, err
	}
	var value int64
	if _, err := fmt.Sscan(text, &value); err != nil {
		return 0, fmt.Errorf("parse integer %q: %w", text, err)
	}
	return value, nil
}

// ExecSQL runs one statement in its own transaction and returns the affected
// row count.
func ExecSQL(ctx context.Context, db *pgdriver.DB, statement string, args ...any) (int64, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	result, err := conn.Exec(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	if err := conn.Commit(ctx); err != nil {
		return 0, err
	}
	return result.RowsAffected, nil
}

// ScopedQuery runs one query inside a transaction whose scope is established
// with the same transaction-local set_config the product adapter uses.
func ScopedQuery(ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, columns int, query string, args ...any) ([][]string, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx,
		`SELECT set_config('kodama.tenant_id', $1, true), set_config('kodama.project_id', $2, true)`,
		string(scope.TenantID), string(scope.ProjectID)); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result [][]string
	for rows.Next() {
		values := make([]any, columns)
		pointers := make([]any, columns)
		rendered := make([]string, columns)
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for index, value := range values {
			rendered[index] = renderValue(value)
		}
		result = append(result, rendered)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := conn.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// ScopedExec runs one statement inside a scoped transaction.
func ScopedExec(ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, statement string, args ...any) (int64, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx,
		`SELECT set_config('kodama.tenant_id', $1, true), set_config('kodama.project_id', $2, true)`,
		string(scope.TenantID), string(scope.ProjectID)); err != nil {
		return 0, err
	}
	result, err := conn.Exec(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	if err := conn.Commit(ctx); err != nil {
		return 0, err
	}
	return result.RowsAffected, nil
}

// CauseText renders the full wrapped error chain. The fault type hides
// its private cause from Error(), but tests need the server SQLSTATE text to
// distinguish an RLS refusal from a missing grant. It never renders a
// connection string or credential.
func CauseText(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		parts = append(parts, cause.Error())
	}
	return strings.Join(parts, " | ")
}

// NullString renders a Go string as SQL NULL when empty.
func NullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
