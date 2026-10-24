package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

// EventAppender is the transaction-local event append of the event bus
// (events/postgres.Store.Append).
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// PermissionBinder binds the identity authority's permission check to one
// mutating transaction, so a revocation committed while the mutation waited for
// the project or item lock is seen before anything is written. A binder that
// returns nil fails closed with work.ErrUnavailable.
type PermissionBinder func(tx pgx.Tx) work.PermissionChecker

// Store implements dependencies.Repository over PostgreSQL.
type Store struct {
	db     storage.TxBeginner
	events EventAppender
	bind   PermissionBinder
}

var _ dependencies.Repository = (*Store)(nil)

// NewStore requires every collaborator: a store without an event appender or
// permission binder could not honor atomic outbox writes or fail-closed
// authorization.
func NewStore(db storage.TxBeginner, appender EventAppender, bind PermissionBinder) (*Store, error) {
	if db == nil || appender == nil || bind == nil {
		return nil, errors.New("work/dependencies/postgres: a database, an event appender and a permission binder are required")
	}
	return &Store{db: db, events: appender, bind: bind}, nil
}

func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

// classify maps infrastructure failures to the module's classes. Business
// errors pass through unchanged; anything unknown stays unclassified (a 500).
func classify(err error) error {
	if err == nil || work.IsBusinessError(err) || errors.Is(err, dependencies.ErrGraph) {
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
		case pgErr.Code == "23503": // foreign key: an endpoint is not a work item of this project
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
