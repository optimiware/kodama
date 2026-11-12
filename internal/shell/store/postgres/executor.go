package postgres

import (
	"context"

	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var _ store.Executor = (*Store)(nil)

// Execute applies one complete commit unit through the store's own connector.
// The neutral store.Unit carries no SQL; this adapter renders and orders the
// locks, writes, receipt, audit rows and event publication intent inside one
// scoped transaction.
func (s *Store) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	return ExecuteUnit(ctx, s, scope, unit)
}

// Read reads one row by identity inside a scoped transaction. The transaction
// is read-only in effect and is completed with a commit so the transaction-local
// scope resets with the transaction; a failed read rolls the transaction back
// and returns the original cause.
func (s *Store) Read(ctx context.Context, scope tenancy.Scope, table, id string) (store.Row, error) {
	if err := ctx.Err(); err != nil {
		return store.Row{}, err
	}
	tx, err := s.Begin(ctx, scope)
	if err != nil {
		return store.Row{}, err
	}
	row, err := ReadRow(ctx, tx, scope, table, id)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return store.Row{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Row{}, err
	}
	return row, nil
}
