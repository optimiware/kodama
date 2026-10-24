package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
)

// TenantSetting is the transaction-local PostgreSQL setting that carries the
// tenant context read by row level security policies.
const TenantSetting = "kodama.tenant_id"

const rollbackTimeout = 5 * time.Second

// TxBeginner is satisfied by *Pool and *pgxpool.Pool.
type TxBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// WithTenant runs fn in a transaction whose tenant context is tenantID.
//
// The identifier is validated before any connection is used. The context is
// installed with a transaction-local set_config, so it ends with the
// transaction on every path: fn returning nil commits; an error, a panic, or a
// cancelled context rolls back. A panic keeps propagating after the rollback.
// fn's error is returned unwrapped. fn must not commit or roll back tx, must
// not retain it after returning, and must not make network calls.
func WithTenant(ctx context.Context, db TxBeginner, tenantID domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return WithTenantOptions(ctx, db, tenantID, pgx.TxOptions{}, fn)
}

// WithTenantOptions is WithTenant with explicit transaction options, such as
// an isolation level or read-only access.
func WithTenantOptions(ctx context.Context, db TxBeginner, tenantID domain.TenantID, opts pgx.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	if db == nil || fn == nil {
		return errors.New("storage: WithTenant requires a database and a function")
	}
	if err := tenantID.Validate(); err != nil {
		return fmt.Errorf("storage: tenant context: %w", err)
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("storage: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Runs for errors, panics and Goexit. Use a context that survives
		// cancellation so the connection is returned in a clean state.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('"+TenantSetting+"', $1, true)", string(tenantID)); err != nil {
		return fmt.Errorf("storage: set tenant context: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	committed = true
	return nil
}
