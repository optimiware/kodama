package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

// ExecutionStore decorates execution's store. A mutation whose context carries a
// Session runs in one tenant transaction in which, after execution has locked
// the run and loaded the aggregate and before the mutation or any peer effect,
// runtimeauth.CheckSession is repeated. A revocation or cancellation that
// committed first is therefore seen; one that did not waits for this
// transaction's commit. Lock order: quota/work, run rows, grant, then peer rows
// (workspace, event backlog) exactly as execution documents.
//
// A context without a session is trusted background work (dispatch,
// observation, reconciliation, cancellation) and takes execution's own path.
type ExecutionStore struct {
	execution.Store
	pg    *execpg.Store
	db    storage.TxBeginner
	guard *Guard
}

var _ execution.Store = (*ExecutionStore)(nil)

// NewExecutionStore requires the store, its database and the guard.
func NewExecutionStore(pg *execpg.Store, db storage.TxBeginner, guard *Guard) (*ExecutionStore, error) {
	if pg == nil || db == nil || guard == nil {
		return nil, errors.New("results/postgres: an execution store, a database and a guard are required")
	}
	return &ExecutionStore{Store: pg, pg: pg, db: db, guard: guard}, nil
}

// Update implements execution.Store.
func (s *ExecutionStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	sess, guarded := SessionFrom(ctx)
	if !guarded {
		return s.pg.Update(ctx, scope, id, opts, fn)
	}
	var agg *execution.Aggregate
	var res any
	err := storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agg, res, err = s.pg.UpdateInTx(ctx, tx, scope, id, opts, func(ctx context.Context, env execution.Env, a *execution.Aggregate) (execution.Effects, error) {
			grant, err := s.guard.Check(ctx, tx, scope, sess)
			if err != nil {
				return execution.Effects{}, err
			}
			if grant.RunID != id {
				return execution.Effects{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another run"))
			}
			return fn(ctx, env, a)
		})
		return err
	})
	if err != nil {
		return nil, nil, classifyTx(err)
	}
	return agg, res, nil
}

// classifyTx keeps classified business and authority errors and maps
// infrastructure failures, including an unknown commit outcome, to the same
// classes execution's own adapter uses.
func classifyTx(err error) error {
	if err == nil || execution.IsBusinessError(err) || isAuthorityError(err) {
		return err
	}
	switch {
	case errors.Is(err, events.ErrBacklogFull):
		return execution.Unavailablef(err, "the event backlog is full")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return execution.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503":
			return fmt.Errorf("%w: a referenced resource was not found", execution.ErrNotFound)
		case pgErr.Code == "23514":
			return fmt.Errorf("%w: the database refused the change (%s)", execution.ErrInvalidTransition, pgErr.ConstraintName+pgErr.Hint)
		case pgErr.Code == "55P03":
			return execution.ErrBusy
		}
	}
	// Anything else is a failed or uncertain transaction: nothing may be
	// acknowledged, and a retry observes whatever committed.
	return execution.Unavailablef(err, "the database could not complete the request")
}

func isAuthorityError(err error) bool {
	for _, k := range []error{runtimeauth.ErrInvalidRequest, runtimeauth.ErrInvalidCredential, runtimeauth.ErrForbidden, runtimeauth.ErrAttemptRevoked,
		runtimeauth.ErrAdmissionConsumed, runtimeauth.ErrProfileUnsupported, runtimeauth.ErrConflict, runtimeauth.ErrNotFound, runtimeauth.ErrUnavailable} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}
