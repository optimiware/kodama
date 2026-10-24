package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/storage"
)

// ExecutionStore is the gate-aware execution.Store. It decorates a store whose
// mutations can join a caller-owned transaction (execution/postgres.Store) and
// reuses its UpdateInTx: the execution persistence is not duplicated.
//
// Every mutation that creates an executable attempt (a first claim, a retry)
// is checked, before anything is persisted, against the gates in front of the
// claimed step. Inside the same transaction that holds the claim's locks it
// verifies that each protecting gate is approved, still applicable (expiry,
// invalidation, conformance), that its subject still matches the current
// content, candidate, definition, evidence and deployment configuration, and
// that the original reviewer still holds the reviewer authority; and that a
// write after an approval has a new gate before the steps behind it. A refusal
// rolls the claim back: no attempt ordinal is consumed and no capacity or
// writer slot leaks. Cancellation, settlement, observation and result commits
// are never blocked, so a stale approval cannot wedge a run.
type ExecutionStore struct {
	execution.Store
	db    storage.TxBeginner
	inner ExecutionTx
	gates *Store
}

var _ execution.Store = (*ExecutionStore)(nil)

// NewExecutionStore wraps inner. The gate store supplies the gate reads,
// current-fact queries and authority binding.
func NewExecutionStore(db storage.TxBeginner, inner execution.Store, tx ExecutionTx, g *Store) (*ExecutionStore, error) {
	if db == nil || inner == nil || tx == nil || g == nil {
		return nil, fmt.Errorf("%w: database, execution store, transaction binding and gate store are required", gates.ErrNotConfigured)
	}
	return &ExecutionStore{Store: inner, db: db, inner: tx, gates: g}, nil
}

// Update implements execution.Store.
func (s *ExecutionStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	var agg *execution.Aggregate
	var res any
	err := classifyExec(storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agg, res, err = s.inner.UpdateInTx(ctx, tx, scope, id, opts, s.guard(tx, scope, opts, fn))
		return err
	}))
	if err != nil {
		return nil, nil, err
	}
	return agg, res, nil
}

// guard wraps a mutation so a newly created attempt is checked behind its
// gates before the aggregate is persisted.
//
// A dispatch transaction (UpdateOptions.Quota) has locked its quota scopes, the
// work item and the run when the mutation starts. Before the claim mutation
// runs, and therefore before it can take the authorization revision lock
// (Env.Authorize), the bound gates of the run are locked FOR SHARE in
// ascending identifier order. That keeps the documented order (run, then gates,
// then authorization) against decisions and invalidations, which lock a gate
// FOR UPDATE before they read authorization. Other mutations (cancellation,
// observation, settlement, results) do not lock gates before they run and are
// never blocked by a stale approval.
func (s *ExecutionStore) guard(tx pgx.Tx, scope domain.Scope, opts execution.UpdateOptions, fn execution.Mutation) execution.Mutation {
	return func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		if opts.Quota {
			if err := s.lockBoundGates(ctx, tx, scope, agg); err != nil {
				return execution.Effects{}, err
			}
		}
		fx, err := fn(ctx, env, agg)
		if err != nil {
			return fx, err
		}
		for _, at := range agg.Created {
			if err := s.checkProtected(ctx, tx, scope, agg, at.StepID); err != nil {
				return execution.Effects{}, err
			}
		}
		return fx, nil
	}
}

// lockBoundGates locks the gates bound to the run's gate steps FOR SHARE in a
// deterministic order. A reference to a missing gate is left for the protection
// check to refuse.
func (s *ExecutionStore) lockBoundGates(ctx context.Context, tx pgx.Tx, scope domain.Scope, agg *execution.Aggregate) error {
	var ids []string
	for _, st := range agg.Steps {
		if st.GateRef != "" {
			ids = append(ids, st.GateRef)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
			string(scope.TenantID), string(scope.ProjectID), id).Scan(&one)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (s *ExecutionStore) checkProtected(ctx context.Context, tx pgx.Tx, scope domain.Scope, agg *execution.Aggregate, stepID string) error {
	prot, err := agg.Protection(stepID)
	if err != nil {
		return fmt.Errorf("%w: %v", execution.ErrStale, err)
	}
	if len(prot.Gates) == 0 {
		return nil
	}
	if !prot.Covered() {
		return fmt.Errorf("%w: %w", execution.ErrStale, &gates.StaleError{Reason: gates.ReasonUncovered})
	}
	consumed := make([]*gates.Gate, 0, len(prot.Gates))
	for _, pg := range prot.Gates {
		if pg.Status != execution.StepSucceeded || pg.GateRef == "" {
			return fmt.Errorf("%w: gate step %s has not completed", execution.ErrStale, pg.StepID)
		}
		g, err := s.gates.checkConsumable(ctx, tx, gates.ID(pg.GateRef), scope, agg)
		if err != nil {
			return protectedDenial(err, stepID)
		}
		consumed = append(consumed, g)
	}
	// Every authority wait is over: a gate checked early must not have reached
	// its deadline while a later gate was being checked.
	if err := s.gates.recheckExpiry(consumed...); err != nil {
		return protectedDenial(err, stepID)
	}
	return nil
}

// protectedDenial maps a gate check failure to the execution error a refused
// claim reports: a definitive refusal is ErrStale, an outage is Unavailable.
func protectedDenial(err error, stepID string) error {
	switch {
	case errors.Is(err, gates.ErrStaleSubject), errors.Is(err, gates.ErrConformance), errors.Is(err, gates.ErrReviewerAuthority),
		errors.Is(err, gates.ErrInvalidTransition), errors.Is(err, gates.ErrNotFound):
		return fmt.Errorf("%w: %w", execution.ErrStale, err)
	case errors.Is(err, gates.ErrUnavailable), errors.Is(err, gates.ErrNotConfigured):
		return execution.Unavailablef(err, "the gate protecting step %s cannot be verified", stepID)
	}
	return err
}

// classifyExec keeps business errors intact and maps infrastructure failures of
// the decorated transaction the way the execution store does.
func classifyExec(err error) error {
	if err == nil || execution.IsBusinessError(err) || gates.IsBusinessError(err) {
		return err
	}
	switch c := classify(err); {
	case errors.Is(c, gates.ErrUnavailable):
		return execution.Unavailablef(err, "the database could not complete the request")
	case errors.Is(c, gates.ErrInvalidTransition):
		return fmt.Errorf("%w: %v", execution.ErrInvalidTransition, c)
	default:
		return err
	}
}
