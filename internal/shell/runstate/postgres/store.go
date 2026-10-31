package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is what the adapter needs of a connection pool: tenant transactions and,
// for Runnable, the narrow tenant enumeration. *storage.Pool and
// *pgxpool.Pool satisfy it.
type DB interface {
	storage.TxBeginner
	storage.Querier
}

// Store is a runtime.StateStore over PostgreSQL. It is safe for concurrent use.
//
// Every operation is one tenant transaction (storage.WithTenant), so row level
// security applies, and every statement carries explicit tenant and project
// predicates. The durable columns and their guard triggers are the one source of
// truth: the store keeps no state of its own, reads the record from the rows
// under a row lock, applies the kernel's pure transition rules to it, writes the
// difference and reads it back. A state change and the outbox events that
// describe it are written by the same transaction: if it does not commit, neither
// exists.
type Store struct {
	db    DB
	clock runtime.Clock

	mu    sync.Mutex
	fault error
}

var _ runtime.StateStore = (*Store)(nil)

// New returns a store. clock is the store's one clock: it times a cancellation,
// which carries no time of its own.
func New(db DB, clock runtime.Clock) (*Store, error) {
	if db == nil {
		return nil, errors.New("runstate/postgres: a database is required")
	}
	if clock == nil {
		return nil, errors.New("runstate/postgres: a clock is required")
	}
	return &Store{db: db, clock: clock}, nil
}

// FailNextCommit makes the next write that reaches its commit point fail with
// err (wrapped) and persist nothing: the transaction rolls back after its last
// statement. A nil err clears a pending fault; a write that fails earlier does
// not consume it. It exists for failure-recovery tests and the shared contract
// suite (test/support/kernelcontracts), never for production wiring.
func (s *Store) FailNextCommit(err error) {
	s.mu.Lock()
	s.fault = err
	s.mu.Unlock()
}

func (s *Store) takeFault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.fault
	s.fault = nil
	return err
}

// maxAttempts bounds how often a transaction is retried when PostgreSQL aborts
// it as a deadlock victim or a serialization failure. Every transaction starts
// from a fresh read under its row lock, so a retry is safe.
const maxAttempts = 4

// update runs fn as one read-write tenant transaction.
func (s *Store) update(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.run(ctx, tenant, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if fault := s.takeFault(); fault != nil {
			return fmt.Errorf("runstate/postgres: commit: %w", fault)
		}
		return nil
	})
}

// view runs fn as one read-only tenant transaction on a single snapshot, so a
// record is read as of one instant across its tables.
func (s *Store) view(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.run(ctx, tenant, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (s *Store) run(ctx context.Context, tenant domain.TenantID, opts pgx.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = storage.WithTenantOptions(ctx, s.db, tenant, opts, fn)
		if !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return mapErr(err)
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

// mapErr turns a constraint the database refused into the kernel's vocabulary,
// keeping the database error in the chain. The kernel's own checks run first, so
// this is the second fence: a guard trigger or check the Go rules did not see.
func mapErr(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case "23514": // check_violation, also raised by the guard triggers
		return fmt.Errorf("%w: %w", runtime.ErrIllegalTransition, err)
	case "23503": // foreign_key_violation
		return fmt.Errorf("%w: %w", domain.ErrInvalidValue, err)
	case "23505": // unique_violation
		return fmt.Errorf("%w: %w", domain.ErrConflict, err)
	}
	return err
}

func notFound(what string) error {
	return &domain.Error{Code: domain.CodeNotFound, Message: what + " not found"}
}

func versionConflict(format string, args ...any) error {
	return &domain.Error{Code: domain.CodeVersionConflict, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidValue, fmt.Sprintf(format, args...))
}

func checkRef(scope domain.Scope, id domain.RunID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return id.Validate()
}
