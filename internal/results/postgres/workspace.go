package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// WorkspaceStore decorates the workspace store for request-originated
// publication mutations. An Update that loads an attempt's receipt (the
// mutations that record or settle that attempt's publication) under a context
// carrying a Session first repeats runtimeauth.CheckSession in the same
// transaction, which takes the execution run lock and the grant FOR SHARE, and
// only then enters the workspace row lock: execution, grant, workspace.
//
// Consequences, by design:
//   - a callback that lost the race against cancellation or revocation cannot
//     record a new publication intent and cannot initiate Git;
//   - a publication authorized before the cancellation may still complete on
//     the remote; this request then cannot record it, and the owner's
//     reconciliation (an undecorated path) settles the receipt from the remote.
//     Reconciliation never changes the cancelled attempt into success.
//
// Updates without a session or without a receipt (writer claims, reconciliation,
// human operations) pass through untouched.
type WorkspaceStore struct {
	workspace.Store
	pg    *workspacepg.Store
	db    storage.TxBeginner
	guard *Guard
}

var _ workspace.Store = (*WorkspaceStore)(nil)

// NewWorkspaceStore requires the store, its database and the guard.
func NewWorkspaceStore(pg *workspacepg.Store, db storage.TxBeginner, guard *Guard) (*WorkspaceStore, error) {
	if pg == nil || db == nil || guard == nil {
		return nil, errors.New("results/postgres: a workspace store, a database and a guard are required")
	}
	return &WorkspaceStore{Store: pg, pg: pg, db: db, guard: guard}, nil
}

// Update implements workspace.Store.
func (s *WorkspaceStore) Update(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	sess, guarded := SessionFrom(ctx)
	if !guarded || load.Receipt == "" {
		return s.pg.Update(ctx, scope, id, load, fn)
	}
	var out *workspace.Aggregate
	err := storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if load.Receipt != sess.Route {
			return workspace.ErrAttemptRevoked // another attempt's receipt is never this session's to change
		}
		if _, err := s.guard.Check(ctx, tx, scope, sess); err != nil {
			return asWorkspaceError(err)
		}
		var err error
		out, err = s.pg.UpdateInTx(ctx, tx, scope, id, load, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// asWorkspaceError maps an authority denial to the workspace module's
// attempt_revoked class, which its HTTP mapping already understands. An outage
// stays unclassified (retryable).
func asWorkspaceError(err error) error {
	for _, k := range []error{runtimeauth.ErrAttemptRevoked, runtimeauth.ErrInvalidCredential, runtimeauth.ErrForbidden, runtimeauth.ErrNotFound, runtimeauth.ErrInvalidRequest} {
		if errors.Is(err, k) {
			return errors.Join(workspace.ErrAttemptRevoked, err)
		}
	}
	return err
}
