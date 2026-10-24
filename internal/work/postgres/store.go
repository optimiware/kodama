// Package postgres is the PostgreSQL adapter of the work-management module: the
// tables, queries, transactional mutations, guard bindings and the admission and
// cancellation seams for the workspace and execution modules.
//
// Every statement runs inside storage.WithTenant (READ COMMITTED, as event
// append requires) under the non-owner runtime role, carries explicit tenant and
// project predicates, and is further constrained by forced row level security.
//
// Lock order, which every module touching these rows must follow:
//
//  1. the project row (FOR NO KEY UPDATE; it does not block foreign-key checks),
//     only for transitions and parent changes;
//  2. the work item row (FOR UPDATE for mutations, FOR SHARE for admission);
//  3. scoped prerequisite rows (children, memberships, workspace and evidence
//     rows) in a consistent order, read-only unless their owner documents more;
//  4. the event backlog and scope rows inside events/postgres.Store.Append,
//     always last: no work or project lock is taken after Append.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
)

// EventAppender is the transaction-local event append of the event bus
// (events/postgres.Store.Append).
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// GuardBinder binds the cross-module guards (authority, writer, dependency and
// review-subject checks) to one mutating transaction. Implementations read
// their own tables through tx, so each answer reflects state after the work
// item lock was acquired. A zero TxGuards member fails closed.
type GuardBinder func(tx pgx.Tx) work.TxGuards

// Store implements work.Repository over PostgreSQL.
type Store struct {
	db     storage.TxBeginner
	events EventAppender
	bind   GuardBinder
}

var _ work.Repository = (*Store)(nil)

// NewStore requires every collaborator: a store without an event appender or
// guard binder could not honor atomic outbox writes or fail-closed guards.
func NewStore(db storage.TxBeginner, appender EventAppender, bind GuardBinder) (*Store, error) {
	if db == nil || appender == nil || bind == nil {
		return nil, errors.New("work/postgres: a database, an event appender and a guard binder are required")
	}
	return &Store{db: db, events: appender, bind: bind}, nil
}

// inTenant runs fn in a tenant transaction and classifies its failure.
func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

// classify maps infrastructure failures to the module's classes. Business
// errors pass through unchanged; anything unknown stays unclassified (a 500).
func classify(err error) error {
	if err == nil || work.IsBusinessError(err) {
		return err
	}
	if errors.Is(err, events.ErrBacklogFull) {
		return work.Unavailablef(err, "the event backlog is full")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return work.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503": // foreign key: a referenced row is not in scope
			return fmt.Errorf("%w: a referenced resource was not found", work.ErrNotFound)
		case pgErr.Code == "23505" && pgErr.ConstraintName == "work_item_history_version_key":
			return fmt.Errorf("%w: the resource changed concurrently", work.ErrVersionConflict)
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "40" || pgErr.Code[:2] == "53" || pgErr.Code[:2] == "57" || pgErr.Code[:2] == "08"):
			return work.Unavailablef(err, "the database could not complete the request")
		}
	}
	var connErr interface{ Timeout() bool }
	if errors.As(err, &connErr) {
		return work.Unavailablef(err, "the database could not be reached")
	}
	return err
}

func tenantOf(scope domain.Scope) domain.TenantID { return scope.TenantID }

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
