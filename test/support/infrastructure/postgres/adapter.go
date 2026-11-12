package pgtest

import (
	"context"
	"fmt"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// AdapterScopedExec runs one statement inside a transaction begun by the
// product store adapter. It exists so a conformance test exercises the
// adapter's own establishScopeSQL instead of re-implementing set_config in the
// test helper; a scope-reset or pooled-connection defect is then observable.
func AdapterScopedExec(ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, statement string, args ...any) (int64, error) {
	tx, err := storepg.New(db).Begin(ctx, scope)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := tx.Exec(ctx, store.Statement{SQL: statement, Args: args})
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return result.RowsAffected, nil
}

// AdapterScopedCount runs a count through the product store adapter's scoped
// transaction. table is a trusted test constant, never caller input.
func AdapterScopedCount(ctx context.Context, db *pgdriver.DB, scope tenancy.Scope, table string) (int64, error) {
	tx, err := storepg.New(db).Begin(ctx, scope)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, store.Query{SQL: "SELECT count(*) FROM " + table})
	if err != nil {
		return 0, err
	}
	values, err := ScanRows(rows, 1)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("count query returned %d rows; want 1", len(values))
	}
	var count int64
	if _, err := fmt.Sscan(values[0][0], &count); err != nil {
		return 0, fmt.Errorf("parse count %q: %w", values[0][0], err)
	}
	return count, nil
}
