// Package postgres is the PostgreSQL adapter of the pipeline-definition module:
// the immutable definitions table, atomic registration with its audit record,
// and scoped reads.
//
// Every statement runs inside storage.WithTenant (READ COMMITTED) under the
// non-owner runtime role, carries explicit tenant and project predicates, and
// is further constrained by forced row level security. The runtime role can
// only SELECT and INSERT: no path updates or deletes a version.
//
// Lock order, which every module touching these rows must follow: the project
// row (FOR NO KEY UPDATE) is taken first and only to allocate a version; no
// other lock is taken while it is held, and no network call is ever made inside
// a transaction (authority, capability, profile and reference lookups all
// happen before it).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/storage"
)

// Options tune the store. The zero value selects the defaults.
type Options struct {
	// LockTimeout bounds how long registration waits for the project row lock
	// before reporting the registration unavailable. Default 5 seconds.
	LockTimeout time.Duration
}

// DefaultLockTimeout is the default project-lock wait.
const DefaultLockTimeout = 5 * time.Second

// Store implements pipeline.Repository over PostgreSQL.
type Store struct {
	db          storage.TxBeginner
	lockTimeout time.Duration
}

var _ pipeline.Repository = (*Store)(nil)

// NewStore requires a database.
func NewStore(db storage.TxBeginner, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("pipeline/postgres: a database is required")
	}
	if opts.LockTimeout < 0 {
		return nil, errors.New("pipeline/postgres: LockTimeout must not be negative")
	}
	if opts.LockTimeout == 0 {
		opts.LockTimeout = DefaultLockTimeout
	}
	return &Store{db: db, lockTimeout: opts.LockTimeout}, nil
}

// inTenant runs fn in a tenant transaction and classifies its failure.
func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

// classify maps infrastructure failures to the module's classes. Business
// errors pass through unchanged; anything unknown stays unclassified (a 500).
func classify(err error) error {
	if err == nil || pipeline.IsBusinessError(err) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return pipeline.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503": // foreign key: the project is not in scope
			return fmt.Errorf("%w: the project was not found", pipeline.ErrNotFound)
		case pgErr.Code == "23505": // a concurrent registration won; a retry converges on its result
			return pipeline.Unavailablef(err, "a concurrent registration changed the project; retry")
		case pgErr.Code == "55P03": // lock_not_available: lock_timeout expired
			return pipeline.Unavailablef(err, "the project is busy; retry")
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "40" || pgErr.Code[:2] == "53" || pgErr.Code[:2] == "57" || pgErr.Code[:2] == "08"):
			return pipeline.Unavailablef(err, "the database could not complete the request")
		}
	}
	var connErr interface{ Timeout() bool }
	if errors.As(err, &connErr) {
		return pipeline.Unavailablef(err, "the database could not be reached")
	}
	return err
}

func notFound() error { return fmt.Errorf("%w: the definition was not found", pipeline.ErrNotFound) }
