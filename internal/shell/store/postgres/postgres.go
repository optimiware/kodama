// Package postgres implements the neutral store.Executor seam and the reviewed
// relational schema design as plain parameterized PostgreSQL.
//
// The structured seam is adapter-independent: Execute applies one complete
// store.Unit and Read returns one scoped store.Row. This package renders that
// unit as SQL inside exactly one transaction with its typed ownership scope,
// commits or rolls back, and keeps all SQL text local to the adapter. There is
// no generic Save/Update/SetState/Persist/Store/Repository method, because a
// business write that can commit apart from its receipt, audit, event/publication
// intent or revocation is not atomic.
//
// Scope is established transaction-locally from verified caller context with
// set_config(..., true) and is reset by transaction completion, so a pooled
// connection cannot leak a prior scope. Values travel as statement arguments;
// the adapter's SQL text is fixed.
//
// Migrations() renders the reviewed sequential migration set from the declared
// store.SchemaDesign: forced row-level security, the non-owner runtime role,
// composite ownership foreign keys, uniqueness, checks, indexes and the durable
// event publication sequence.
//
// Non-claims: this package opens no socket, starts no server and runs no
// migration in its unit suite. The suite drives an in-process seam. Actual SQL
// validity against a server version, RLS/locking/constraint enforcement and
// concurrent atomicity are outside this unit suite.
package postgres

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Conn is one physical database connection. It is the narrow seam the adapter
// needs: parameterized statements, parameterized queries and transaction
// completion. A Conn is not a scope; the adapter establishes scope on it inside
// one transaction.
type Conn interface {
	Exec(context.Context, string, ...any) (store.Result, error)
	Query(context.Context, string, ...any) (store.Rows, error)
	Commit(context.Context) error
	Rollback(context.Context) error
}

// DB opens physical connections. Production supplies a pool; tests supply a
// deterministic in-process fake.
type DB interface {
	Begin(context.Context) (Conn, error)
}

// ErrNoTransaction reports use of a transaction after it committed or rolled
// back. It is never returned for a fresh transaction.
var ErrNoTransaction = errors.New("postgres: transaction is not active")

// scopeTenantSetting is the transaction-local tenant setting the RLS policies
// read. It is set for every transaction, never at session level.
const scopeTenantSetting = "kodama.tenant_id"

// scopeProjectSetting is the transaction-local project setting. A tenant-scoped
// transaction sets it to the empty string so a policy cannot see a stale
// project from a reused connection.
const scopeProjectSetting = "kodama.project_id"

// scopeCredentialSetting is the transaction-local exact-row identity the auth
// role's exact_row policy reads. It is set only inside one credential lookup
// transaction and resets with it, so the lookup can never list or range-scan.
const scopeCredentialSetting = "kodama.credential_id"

// establishScopeSQL sets both transaction-local settings in one statement. The
// third set_config argument is true, which makes the value transaction-local:
// it is reset by commit or rollback and cannot leak through a pooled
// connection. Scope values travel as arguments, never as SQL text.
const establishScopeSQL = `SELECT set_config('` + scopeTenantSetting + `', $1, true), set_config('` + scopeProjectSetting + `', $2, true)`

// Store is the scoped SQL connection. It begins one transaction per call and
// never holds a scope of its own.
type Store struct {
	db DB
}

// New builds a scoped store over the supplied connection seam.
func New(db DB) *Store {
	return &Store{db: db}
}

var _ store.ScopedConnection = (*Store)(nil)

// Begin starts one transaction whose ownership scope is established from
// verified caller context. An invalid scope or an already-canceled context is
// refused before any database call; a scope statement failure rolls the
// connection back so no half-scoped transaction survives.
func (s *Store) Begin(ctx context.Context, scope tenancy.Scope) (store.Transaction, error) {
	return s.begin(ctx, scope, false)
}

// beginRead starts one read-only transaction with the same transaction-local
// scope as Begin. SET TRANSACTION READ ONLY is the first statement because
// PostgreSQL requires it before any query in the transaction; the scope
// setting follows and both reset when the transaction completes. A failure in
// either setup statement rolls the connection back.
func (s *Store) beginRead(ctx context.Context, scope tenancy.Scope) (store.Transaction, error) {
	return s.begin(ctx, scope, true)
}

func (s *Store) begin(ctx context.Context, scope tenancy.Scope, readOnly bool) (store.Transaction, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if readOnly {
		if _, err := conn.Exec(ctx, beginReadOnlySQL); err != nil {
			_ = conn.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	if _, err := conn.Exec(ctx, establishScopeSQL, string(scope.TenantID), string(scope.ProjectID)); err != nil {
		_ = conn.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return &transaction{conn: conn, scope: scope}, nil
}

// transaction is one scoped atomic commit unit. Completion is idempotent-safe:
// once the transaction is closed every further call reports ErrNoTransaction and
// issues no statement, so a caller cannot accidentally write after commit.
type transaction struct {
	conn  Conn
	scope tenancy.Scope

	mu     sync.Mutex
	closed bool
	now    time.Time
	nowSet bool
}

var _ store.Transaction = (*transaction)(nil)

// Exec runs one parameterized write statement inside the transaction.
func (tx *transaction) Exec(ctx context.Context, statement store.Statement) (store.Result, error) {
	if err := tx.active(); err != nil {
		return store.Result{}, err
	}
	return tx.conn.Exec(ctx, statement.SQL, statement.Args...)
}

// Query runs one parameterized read statement inside the transaction.
func (tx *transaction) Query(ctx context.Context, query store.Query) (store.Rows, error) {
	if err := tx.active(); err != nil {
		return nil, err
	}
	return tx.conn.Query(ctx, query.SQL, query.Args...)
}

// Commit applies the whole unit. A failed commit is reported, never retried and
// never reported as success; the adapter attempts one cleanup rollback so the
// connection does not stay inside an aborted transaction.
func (tx *transaction) Commit(ctx context.Context) error {
	tx.mu.Lock()
	if tx.closed {
		tx.mu.Unlock()
		return ErrNoTransaction
	}
	tx.closed = true
	tx.mu.Unlock()

	if err := tx.conn.Commit(ctx); err != nil {
		_ = tx.conn.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return nil
}

// Rollback discards the unit and leaves no partial rows. A failed rollback is
// reported but still closes the transaction: the connection is never reused
// with a live scope.
func (tx *transaction) Rollback(ctx context.Context) error {
	tx.mu.Lock()
	if tx.closed {
		tx.mu.Unlock()
		return ErrNoTransaction
	}
	tx.closed = true
	tx.mu.Unlock()

	return tx.conn.Rollback(ctx)
}

func (tx *transaction) active() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.closed {
		return ErrNoTransaction
	}
	return nil
}

// beginUnscoped starts one transaction without establishing a runtime scope. The
// controller, publisher and auth roles use it: their forced-RLS policies do not
// read the runtime scope, and the controller sets a scope only after it has
// derived the owning scope from the execution row inside the same transaction.
func beginUnscoped(ctx context.Context, db DB, readOnly bool) (store.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if readOnly {
		if _, err := conn.Exec(ctx, beginReadOnlySQL); err != nil {
			_ = conn.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return &transaction{conn: conn}, nil
}

// setScope establishes the transaction-local scope on an already-open
// transaction. The controller calls it once, after resolving the owning scope
// from the execution row inside the same transaction. A failure leaves the
// transaction for the caller to roll back.
func (tx *transaction) setScope(ctx context.Context, scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := tx.active(); err != nil {
		return err
	}
	if _, err := tx.conn.Exec(ctx, establishScopeSQL, string(scope.TenantID), string(scope.ProjectID)); err != nil {
		return err
	}
	tx.mu.Lock()
	tx.scope = scope
	tx.mu.Unlock()
	return nil
}

// transactionClockSQL reads the database transaction clock. The adapter reads it
// once per transaction and reuses it for every deadline guard in that
// transaction, so a write cannot settle an answer at or past the persisted
// deadline.
const transactionClockSQL = "SELECT statement_timestamp()"

// transactionNow returns the transaction clock. For this adapter's transaction
// the value is read once and cached; any other Transaction implementation is
// queried directly so the helper stays usable through the neutral seam.
func transactionNow(ctx context.Context, tx store.Transaction) (time.Time, error) {
	if owning, ok := tx.(*transaction); ok {
		if err := owning.active(); err != nil {
			return time.Time{}, err
		}
		owning.mu.Lock()
		cached, set := owning.now, owning.nowSet
		owning.mu.Unlock()
		if set {
			return cached, nil
		}
		rows, err := owning.conn.Query(ctx, transactionClockSQL)
		if err != nil {
			return time.Time{}, err
		}
		now, err := scanTransactionClock(rows)
		if err != nil {
			return time.Time{}, err
		}
		owning.mu.Lock()
		owning.now, owning.nowSet = now, true
		owning.mu.Unlock()
		return now, nil
	}
	rows, err := tx.Query(ctx, store.Query{SQL: transactionClockSQL})
	if err != nil {
		return time.Time{}, err
	}
	return scanTransactionClock(rows)
}

// scanTransactionClock reads the single clock row of a transactionClockSQL
// result and closes it.
func scanTransactionClock(rows store.Rows) (time.Time, error) {
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, faultInternal("the database did not return its transaction clock")
	}
	var now time.Time
	if err := rows.Scan(&now); err != nil {
		return time.Time{}, fault.Wrap(fault.InternalFailure, "read the database transaction clock", err)
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

// checkActiveScope refuses a unit whose declared scope differs from the scope
// the transaction actually established. The store owns no ambient scope, so a
// mismatch is a caller defect that must not reach the database. Transactions
// from another implementation are not second-guessed.
func checkActiveScope(tx store.Transaction, scope tenancy.Scope) error {
	owning, ok := tx.(*transaction)
	if !ok {
		return nil
	}
	if owning.scope != scope {
		return faultForbidden("scope does not match the active transaction")
	}
	return nil
}
