// Package dbtx runs work inside a PostgreSQL transaction bound to one
// application tenant. The tenant is set with a transaction-local setting, so it
// reverts automatically when the transaction ends and can never leak through a
// pooled connection to the next caller. Row-level security policies read it.
package dbtx

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoTenant is returned when no tenant is supplied.
var ErrNoTenant = errors.New("dbtx: a tenant is required")

// InTenant runs fn in a transaction whose RLS context is tenant. fn's error
// rolls everything back; a commit error is returned as-is (the outcome is then
// unknown to the caller and must be treated that way).
func InTenant(ctx context.Context, pool *pgxpool.Pool, tenant string, fn func(pgx.Tx) error) error {
	if tenant == "" {
		return ErrNoTenant
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("dbtx: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return fmt.Errorf("dbtx: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
