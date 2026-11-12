// Package pgdriver implements the postgres.Conn and postgres.DB seams
// over the pgx v5 pool.
//
// The store owns every SQL statement, scope setting and commit unit; this
// wrapper owns only transaction acquisition, parameterized execution, bounded
// row iteration and error classification. Scope reset is the store's
// transaction-local set_config and the wrapper's completion guard: after a
// commit, rollback or cancellation no statement can run on the same Conn.
//
// The package opens no socket in its unit suite. The pool is created lazily on
// the first Begin and tests drive the consumer-owned minimal driver surface.
// Real PostgreSQL conformance belongs to the component suite.
package pgdriver

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
)

// Config configures one pooled database wrapper. Every field is explicit; an
// empty or malformed connection string is refused.
type Config struct {
	// ConnString is the pgx connection string. It may carry a password and is
	// never rendered in an error or log.
	ConnString string
	// MaxConns bounds the pool. Zero uses the pgx default.
	MaxConns int32
	// MinConns is the pool's minimum idle connection count. Zero uses the pgx
	// default.
	MinConns int32
	// MaxConnLifetime, MaxConnIdleTime and HealthCheckPeriod optionally bound
	// connection age. Zero uses the pgx default.
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds one physical connection attempt. Zero uses the pgx
	// default.
	ConnectTimeout time.Duration
}

// commandTag is the minimal statement result this wrapper consumes.
type commandTag interface {
	RowsAffected() int64
}

// rows is the minimal row iterator this wrapper consumes.
type rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

// transaction is the minimal pgx transaction surface this wrapper consumes.
type transaction interface {
	Exec(context.Context, string, ...any) (commandTag, error)
	Query(context.Context, string, ...any) (rows, error)
	Commit(context.Context) error
	Rollback(context.Context) error
}

// beginner opens one transaction. The production implementation is the pgx
// pool; tests supply a deterministic fake.
type beginner interface {
	Begin(context.Context) (transaction, error)
}

// DB implements postgres.DB over a lazily created pool.
type DB struct {
	mu     sync.Mutex
	driver beginner
	open   func(context.Context) (beginner, func(), error)
	closer func()
	closed bool
}

var _ postgres.DB = (*DB)(nil)

// New validates the configuration and builds one wrapper. It dials nothing: the
// pool is created on the first Begin.
func New(config Config) (*DB, error) {
	if strings.TrimSpace(config.ConnString) == "" {
		return nil, fault.New(fault.InvalidInput, "database connection string is empty")
	}
	if config.MaxConns < 0 || config.MinConns < 0 {
		return nil, fault.New(fault.InvalidInput, "database pool bounds are negative")
	}
	if config.MaxConns > 0 && config.MinConns > config.MaxConns {
		return nil, fault.New(fault.InvalidInput, "database minimum connections exceed the maximum")
	}
	if config.MaxConnLifetime < 0 || config.MaxConnIdleTime < 0 || config.HealthCheckPeriod < 0 || config.ConnectTimeout < 0 {
		return nil, fault.New(fault.InvalidInput, "database connection durations are negative")
	}
	poolConfig, err := parsePoolConfig(config.ConnString)
	if err != nil {
		return nil, err
	}
	if config.MaxConns > 0 {
		poolConfig.MaxConns = config.MaxConns
	}
	if config.MinConns > 0 {
		poolConfig.MinConns = config.MinConns
	}
	if config.MaxConnLifetime > 0 {
		poolConfig.MaxConnLifetime = config.MaxConnLifetime
	}
	if config.MaxConnIdleTime > 0 {
		poolConfig.MaxConnIdleTime = config.MaxConnIdleTime
	}
	if config.HealthCheckPeriod > 0 {
		poolConfig.HealthCheckPeriod = config.HealthCheckPeriod
	}
	if config.ConnectTimeout > 0 {
		poolConfig.ConnConfig.ConnectTimeout = config.ConnectTimeout
	}
	return newWithOpener(func(context.Context) (beginner, func(), error) {
		pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err != nil {
			return nil, nil, err
		}
		return &poolDriver{pool: pool}, pool.Close, nil
	}), nil
}

// parsePoolConfig parses one pgx pool configuration. The TLS query options
// (sslmode, sslrootcert) are interpreted by pgx exactly here, so the wrapper
// never rewrites, trims or downgrades what the operator supplied: the migrate
// and bootstrap-admin commands connect with exactly the DSN their Secret
// carries. The cause is private and never rendered, so a credential in the
// connection string cannot reach an error message.
func parsePoolConfig(connString string) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "database connection string is invalid", err)
	}
	return poolConfig, nil
}

// newWithOpener builds a wrapper that creates its driver on first use.
func newWithOpener(open func(context.Context) (beginner, func(), error)) *DB {
	return &DB{open: open}
}

// Begin opens one transaction. A canceled context performs no database call.
func (db *DB) Begin(ctx context.Context) (postgres.Conn, error) {
	if db == nil {
		return nil, fault.New(fault.InvalidInput, "database wrapper is nil")
	}
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, "database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(err)
	}
	driver, err := db.current(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := driver.Begin(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if tx == nil {
		return nil, fault.New(fault.DependencyUnavailable, "database driver returned no transaction")
	}
	return &conn{tx: tx}, nil
}

// LockSession acquires one session-level advisory lock on a dedicated pooled
// connection and holds it until release is called. The lock is what serializes
// concurrent one-shot setup runs: it is visible cluster-wide (for this
// database) even though the holder is one pooled session. Release unlocks and
// returns the connection to the pool; it is safe to call once, and a canceled
// caller context does not abandon the unlock.
func (db *DB) LockSession(ctx context.Context, key int64) (func(context.Context) error, error) {
	if db == nil {
		return nil, fault.New(fault.InvalidInput, "database wrapper is nil")
	}
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, "database context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(err)
	}
	driver, err := db.current(ctx)
	if err != nil {
		return nil, err
	}
	locker, ok := driver.(interface {
		LockSession(context.Context, int64) (func(context.Context) error, error)
	})
	if !ok {
		return nil, fault.New(fault.DependencyUnavailable, "database driver cannot hold a session lock")
	}
	return locker.LockSession(ctx, key)
}

// current returns the pool driver, creating it once.
func (db *DB) current(ctx context.Context) (beginner, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, fault.New(fault.DependencyUnavailable, "database wrapper is closed")
	}
	if db.driver != nil {
		return db.driver, nil
	}
	if db.open == nil {
		return nil, fault.New(fault.InvalidInput, "database wrapper has no driver")
	}
	driver, closer, err := db.open(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if driver == nil {
		return nil, fault.New(fault.DependencyUnavailable, "database pool returned no driver")
	}
	db.driver = driver
	db.closer = closer
	return driver, nil
}

// Close closes the pool. It is idempotent.
func (db *DB) Close() {
	if db == nil {
		return
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return
	}
	db.closed = true
	closer := db.closer
	db.closer = nil
	db.driver = nil
	db.mu.Unlock()
	if closer != nil {
		closer()
	}
}

// poolDriver adapts the pgx pool to the minimal driver surface.
type poolDriver struct {
	pool *pgxpool.Pool
}

func (driver *poolDriver) Begin(ctx context.Context) (transaction, error) {
	tx, err := driver.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &poolTransaction{tx: tx}, nil
}

// LockSession holds one session-level advisory lock on a dedicated connection.
func (driver *poolDriver) LockSession(ctx context.Context, key int64) (func(context.Context) error, error) {
	connection, err := driver.pool.Acquire(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		connection.Release()
		return nil, mapError(err)
	}
	return func(releaseCtx context.Context) error {
		if releaseCtx == nil {
			releaseCtx = context.Background()
		}
		_, unlockErr := connection.Exec(releaseCtx, `SELECT pg_advisory_unlock($1)`, key)
		connection.Release()
		return mapError(unlockErr)
	}, nil
}

// poolTransaction adapts one pgx transaction.
type poolTransaction struct {
	tx pgx.Tx
}

func (tx *poolTransaction) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	return tx.tx.Exec(ctx, sql, args...)
}

func (tx *poolTransaction) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	result, err := tx.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &poolRows{rows: result}, nil
}

// poolRows adapts pgx.Rows to the seam's Close() error shape.
type poolRows struct {
	rows pgx.Rows
}

func (result *poolRows) Next() bool             { return result.rows.Next() }
func (result *poolRows) Scan(dest ...any) error { return result.rows.Scan(dest...) }
func (result *poolRows) Err() error             { return result.rows.Err() }
func (result *poolRows) Close() error           { result.rows.Close(); return nil }

func (tx *poolTransaction) Commit(ctx context.Context) error {
	return tx.tx.Commit(ctx)
}

func (tx *poolTransaction) Rollback(ctx context.Context) error {
	return tx.tx.Rollback(ctx)
}

// conn implements postgres.Conn over one transaction. Completion is a
// statement-level guard: after commit, rollback or cancellation every further
// call reports ErrNoTransaction and issues no statement.
type conn struct {
	mu     sync.Mutex
	tx     transaction
	closed bool
}

var _ postgres.Conn = (*conn)(nil)

// Exec runs one parameterized statement.
func (c *conn) Exec(ctx context.Context, sql string, args ...any) (store.Result, error) {
	if err := c.active(ctx); err != nil {
		return store.Result{}, err
	}
	tag, err := c.tx.Exec(ctx, sql, args...)
	if err != nil {
		c.abortIfUnusable(ctx, err)
		return store.Result{}, mapError(err)
	}
	return store.Result{RowsAffected: tag.RowsAffected()}, nil
}

// Query runs one parameterized read.
func (c *conn) Query(ctx context.Context, sql string, args ...any) (store.Rows, error) {
	if err := c.active(ctx); err != nil {
		return nil, err
	}
	result, err := c.tx.Query(ctx, sql, args...)
	if err != nil {
		c.abortIfUnusable(ctx, err)
		return nil, mapError(err)
	}
	if result == nil {
		return nil, fault.New(fault.InternalFailure, "database query returned no rows")
	}
	return result, nil
}

// Commit applies the transaction. A failed commit is reported and one cleanup
// rollback is attempted so the connection does not stay in an aborted
// transaction.
func (c *conn) Commit(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "database context is nil")
	}
	if err := ctx.Err(); err != nil {
		c.abort(ctx)
		return mapError(err)
	}
	if err := c.finish(); err != nil {
		return err
	}
	if err := c.tx.Commit(ctx); err != nil {
		_ = c.tx.Rollback(context.WithoutCancel(ctx))
		return mapError(err)
	}
	return nil
}

// Rollback discards the transaction. A failed rollback still closes the
// connection.
func (c *conn) Rollback(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "database context is nil")
	}
	if err := c.finish(); err != nil {
		return err
	}
	if err := c.tx.Rollback(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// active refuses a completed transaction or a canceled context.
func (c *conn) active(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "database context is nil")
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return postgres.ErrNoTransaction
	}
	if err := ctx.Err(); err != nil {
		c.abort(ctx)
		return mapError(err)
	}
	return nil
}

// finish marks the connection complete and reports a repeated completion.
func (c *conn) finish() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return postgres.ErrNoTransaction
	}
	c.closed = true
	return nil
}

// abortOnCancellation closes the transaction when the context ended during a
// statement.
func (c *conn) abortOnCancellation(ctx context.Context) {
	if ctx == nil {
		return
	}
	if ctx.Err() != nil {
		c.abort(ctx)
	}
}

// abortIfUnusable closes the transaction when the driver reported that the
// context ended or the transaction is already gone. A constraint error leaves
// the decision to the caller, which still owns the commit unit.
func (c *conn) abortIfUnusable(ctx context.Context, err error) {
	switch {
	case isCancellation(err):
		c.abort(ctx)
	case errors.Is(err, pgx.ErrTxClosed):
		_ = c.finish()
	default:
		c.abortOnCancellation(ctx)
	}
}

// isCancellation reports whether the driver error means the context ended.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return true
	}
	return false
}

// abort marks the connection closed and rolls back with a detached context.
func (c *conn) abort(ctx context.Context) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.tx.Rollback(context.WithoutCancel(ctx))
}

// mapError maps a pgx/pgconn error to the fault class the seam expects. The
// public message never renders the cause, so a connection string or statement
// argument cannot leak.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, postgres.ErrNoTransaction) || errors.Is(err, pgx.ErrTxClosed) {
		return postgres.ErrNoTransaction
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fault.Wrap(fault.DeadlineExpired, "database operation was canceled", err)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "23505", "40001", "40P01":
			return fault.Wrap(fault.VersionConflict, "database constraint or serialization conflict", err)
		case "42501":
			return fault.Wrap(fault.Forbidden, "database permission was denied", err)
		case "57014":
			return fault.Wrap(fault.DeadlineExpired, "database statement was canceled", err)
		case "23502", "23503", "23514", "22P02", "22001", "22003", "22007", "22008", "22P05":
			return fault.Wrap(fault.InvalidInput, "database rejected the statement value", err)
		case "42P01", "42703", "42883", "42P10", "3D000":
			return fault.Wrap(fault.InternalFailure, "database schema does not match the adapter", err)
		}
		if strings.HasPrefix(pgErr.Code, "08") {
			return fault.Wrap(fault.DependencyUnavailable, "database connection is unavailable", err)
		}
		return fault.Wrap(fault.InternalFailure, "database statement failed", err)
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return fault.Wrap(fault.DependencyUnavailable, "database connection is unavailable", err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return fault.Wrap(fault.DependencyUnavailable, "database connection is unavailable", err)
	}
	return fault.Wrap(fault.DependencyUnavailable, "database dependency failed", err)
}

// EnsureDatabase creates the named database when it does not exist, over one
// plain connection to connString (a maintenance database). CREATE DATABASE
// cannot run inside a transaction, so this is the one statement this package
// runs outside the transactional seam. The new database is owned by the
// connecting role; the migration step hands it to the owner role afterwards.
// The connection string may carry a password and is never rendered in an error.
func EnsureDatabase(ctx context.Context, connString, name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\"") {
		return fault.New(fault.InvalidInput, "database name is invalid")
	}
	connection, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fault.New(fault.DependencyUnavailable, "maintenance database is unreachable")
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()
	var exists bool
	if err := connection.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fault.New(fault.DependencyUnavailable, "database existence check failed")
	}
	if exists {
		return nil
	}
	if _, err := connection.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return fault.New(fault.DependencyUnavailable, "database creation failed")
	}
	return nil
}
