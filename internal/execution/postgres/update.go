package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Update implements execution.Store: lock, load, mutate, persist, run peer
// effects and write history, audit and events in one transaction.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	var agg *execution.Aggregate
	var res any
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agg, res, err = s.UpdateInTx(ctx, tx, scope, id, opts, fn)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return agg, res, nil
}

// UpdateInTx is Update inside a caller-owned tenant transaction (from
// storage.WithTenant for scope.TenantID), as used by the event consumer that
// records cancellation. It never commits, rolls back or calls out.
func (s *Store) UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	p, err := s.mutateInTx(ctx, tx, scope, id, opts, fn)
	if err != nil {
		return nil, nil, err
	}
	if err := p.settleWriters(ctx); err != nil {
		return nil, nil, err
	}
	if err := s.recordInTx(ctx, tx, p); err != nil {
		return nil, nil, err
	}
	return p.agg, p.res, nil
}

// pendingUpdate is a run mutation applied and persisted in the caller's
// transaction up to lock level 4: its grants are revoked and its secrets
// released, but its workspace settlement and its events are still to come.
// UpdateInTx completes it at once; a batch completes every run's levels 3-4
// first so no run's level 5-6 effect precedes another run's lower-level lock.
type pendingUpdate struct {
	agg *execution.Aggregate
	res any
	env *txEnv
}

// mutateInTx takes the locks (quota, work, run), applies the mutation, saves
// the execution rows, revokes the grants of attempts whose authority ended
// without an accepted result, and runs the queued peer effects (capacity,
// secrets, writer reservation) with writer settlement deferred.
func (s *Store) mutateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*pendingUpdate, error) {
	if err := scope.Validate(); err != nil {
		return nil, execution.ErrNotFound
	}
	if err := id.Validate(); err != nil {
		return nil, execution.ErrNotFound
	}
	env := s.newEnv(tx, scope)
	env.deferWriter = true
	if opts.Quota {
		if err := env.lockQuota(ctx); err != nil {
			return nil, err
		}
	}
	if opts.LockWork {
		var item *string
		err := tx.QueryRow(ctx, `SELECT work_item_id FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&item)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, execution.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if item != nil {
			if err := env.takeWork(ctx, domain.WorkItemID(*item)); err != nil {
				return nil, err
			}
		}
	}
	mode := lockUpdate
	if opts.SkipLocked {
		mode = lockSkip
	}
	agg, err := s.loadAggregate(ctx, tx, scope, id, mode)
	if err != nil {
		return nil, err
	}
	fx, err := fn(ctx, env, agg)
	if err != nil {
		return nil, err
	}
	if err := s.persist(ctx, tx, agg); err != nil {
		return nil, err
	}
	if err := s.revokeGrants(ctx, tx, scope, agg, env.Now()); err != nil {
		return nil, err
	}
	if err := s.revokeGitGrants(ctx, tx, scope, agg, env.Now()); err != nil {
		return nil, err
	}
	for _, after := range fx.After {
		if err := after(ctx, env); err != nil {
			return nil, err
		}
	}
	return &pendingUpdate{agg: agg, res: fx.Result, env: env}, nil
}

// settleWriters performs the deferred workspace writer settlements (level 5).
func (p *pendingUpdate) settleWriters(ctx context.Context) error { return p.env.runSettles(ctx) }

// recordInTx writes history, audit and events (level 6) and marks the
// aggregate clean.
func (s *Store) recordInTx(ctx context.Context, tx pgx.Tx, p *pendingUpdate) error {
	if err := s.writeTransitions(ctx, tx, p.agg); err != nil {
		return err
	}
	s.markClean(p.agg)
	return nil
}

// revokeGrants ends the runtime grants of every attempt whose authority ended
// in this mutation without an accepted result: cancellation, timeout, startup
// failure and failure without a result. An attempt whose result was accepted
// (successful, failed, needs-input or validation failure) is not revoked here:
// its control row is revoked, but its grant keeps the receipt exception of
// runtimeauth.CheckSession so a retry of the accepted hash reaches the stored
// acknowledgment; only an explicit grant revocation, expiry or a signing
// rotation ends that. Attempts are revoked in ID order so concurrent
// transactions touching the same attempts cannot lock them crosswise.
func (s *Store) revokeGrants(ctx context.Context, tx pgx.Tx, scope domain.Scope, agg *execution.Aggregate, now time.Time) error {
	if s.grants == nil {
		return nil
	}
	seen := map[domain.AttemptID]bool{}
	var ids []domain.AttemptID
	for _, at := range agg.Revoked {
		if at.ResultHash != "" || seen[at.ID] {
			continue
		}
		if at.Scope != scope {
			return fmt.Errorf("%w: an attempt of another scope was revoked", execution.ErrNotFound)
		}
		seen[at.ID] = true
		ids = append(ids, at.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if _, err := s.grants(ctx, tx, scope, id, now); err != nil {
			return execution.Unavailablef(err, "the runtime grants of attempt %s could not be revoked", id)
		}
	}
	return nil
}

// revokeGitGrants ends the Git grants of every attempt of a source-set run whose
// authority this transaction ended, in attempt ID order, in the transaction
// itself (lock level 4, with the runtime grants): the run row is locked, the
// attempt's authority recorded, and only then are its grants revoked, so a push
// that races the cancellation either already obtained commit intent (and
// completes) or is refused. It is idempotent.
func (s *Store) revokeGitGrants(ctx context.Context, tx pgx.Tx, scope domain.Scope, agg *execution.Aggregate, now time.Time) error {
	if !agg.Run.SourceRun() {
		return nil
	}
	var ids []domain.AttemptID
	for _, at := range agg.Attempts {
		if c, ok := agg.Controls[at.ID]; ok && c.AuthorityEnded() {
			if at.Scope != scope {
				return fmt.Errorf("%w: an attempt of another scope was revoked", execution.ErrNotFound)
			}
			ids = append(ids, at.ID)
		}
	}
	if len(ids) > 0 && s.peers.GitGrants == nil {
		return fmt.Errorf("%w: the Git grant revoker is required to end the authority of a source-set attempt", execution.ErrNotConfigured)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if _, err := s.peers.GitGrants(ctx, tx, scope, id, now); err != nil {
			return execution.Unavailablef(err, "the Git grants of attempt %s could not be revoked", id)
		}
	}
	return nil
}
