package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaLockKey is the PostgreSQL advisory lock serializing schema changes in
// one database ("KODAMA" in ASCII).
const schemaLockKey int64 = 0x4b4f44414d41

// Acquirer is satisfied by *Pool and *pgxpool.Pool.
type Acquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// MigrateOptions configures Migrate.
type MigrateOptions struct {
	// RuntimeRole is the non-owner role that receives the grants written
	// against RuntimeRolePlaceholder. Required if any migration uses the
	// placeholder; the role must already exist (created by the environment).
	RuntimeRole string
}

// MigrateResult reports what one Migrate call did.
type MigrateResult struct {
	Applied []string // IDs applied by this call, in order
	Skipped int      // already-applied migrations verified by checksum
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    seq        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id         text NOT NULL UNIQUE,
    checksum   text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migrate applies the manifest's pending migrations in order using a
// migration-role (table owner) connection.
//
// A session-level advisory lock held for the whole run serializes concurrent
// runners. Under the lock Migrate verifies all applied migrations (checksums
// match, all known to the manifest, no pending migration ordered before an
// applied one) before applying anything. Each migration and its
// schema_migrations row commit in one transaction, so a failing migration
// leaves neither partial DDL nor a record; earlier migrations stay applied.
// Running again with the same manifest is a no-op. Migrations must not use
// statements that cannot run in a transaction block.
func Migrate(ctx context.Context, db Acquirer, m Manifest, opts MigrateOptions) (MigrateResult, error) {
	var res MigrateResult
	expanded, err := expandAll(m.migrations, opts.RuntimeRole)
	if err != nil {
		return res, err
	}

	conn, err := db.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("storage: acquire migration connection: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			var ok bool
			if err := conn.QueryRow(uctx, "SELECT pg_advisory_unlock($1)", schemaLockKey).Scan(&ok); err != nil || !ok {
				// Never return a connection that may still hold the lock.
				_ = conn.Hijack().Close(uctx)
				return
			}
		}
		conn.Release()
	}()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		return res, fmt.Errorf("storage: schema lock: %w", err)
	}
	locked = true

	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return res, fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return res, err
	}
	pending, err := plan(m.migrations, applied)
	if err != nil {
		return res, err
	}
	res.Skipped = len(m.migrations) - len(pending)

	for _, i := range pending {
		mig := m.migrations[i]
		if err := applyOne(ctx, conn, mig, expanded[i]); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, mig.ID)
	}
	return res, nil
}

func expandAll(ms []Migration, role string) ([]string, error) {
	out := make([]string, len(ms))
	for i, m := range ms {
		if !strings.Contains(m.SQL, RuntimeRolePlaceholder) {
			out[i] = m.SQL
			continue
		}
		if role == "" {
			return nil, fmt.Errorf("storage: %w: %s", ErrRuntimeRoleRequired, m.ID)
		}
		out[i] = strings.ReplaceAll(m.SQL, RuntimeRolePlaceholder, pgx.Identifier{role}.Sanitize())
	}
	return out, nil
}

func loadApplied(ctx context.Context, conn *pgxpool.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, "SELECT id, checksum FROM schema_migrations ORDER BY seq")
	if err != nil {
		return nil, fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, fmt.Errorf("storage: scan schema_migrations: %w", err)
		}
		applied[id] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	return applied, nil
}

// plan returns the manifest indexes still to apply, or an error if the applied
// history conflicts with the manifest.
func plan(ms []Migration, applied map[string]string) ([]int, error) {
	inManifest := make(map[string]bool, len(ms))
	for _, m := range ms {
		inManifest[m.ID] = true
	}
	for id := range applied {
		if !inManifest[id] {
			return nil, fmt.Errorf("storage: %w: %s", ErrUnknownMigration, id)
		}
	}
	var pending []int
	for i, m := range ms {
		sum, ok := applied[m.ID]
		switch {
		case !ok:
			pending = append(pending, i)
		case sum != m.Checksum():
			return nil, fmt.Errorf("storage: %w: %s", ErrChecksumMismatch, m.ID)
		case len(pending) > 0:
			return nil, fmt.Errorf("storage: %w: %s is applied but %s, ordered before it, is not", ErrMigrationOrder, m.ID, ms[pending[0]].ID)
		}
	}
	return pending, nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, m Migration, sql string) (err error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("storage: begin migration %s: %w", m.ID, err)
	}
	defer func() {
		if err != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if rerr := tx.Rollback(rctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	// No arguments: pgx uses the simple protocol, so a file may hold many
	// statements.
	if _, err = tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("storage: apply migration %s: %w", m.ID, err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations (id, checksum) VALUES ($1, $2)", m.ID, m.Checksum()); err != nil {
		return fmt.Errorf("storage: record migration %s: %w", m.ID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit migration %s: %w", m.ID, err)
	}
	return nil
}
