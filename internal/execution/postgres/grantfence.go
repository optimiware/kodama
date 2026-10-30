package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// GrantFencer checks a runtime grant inside the caller's open tenant transaction
// (execution.GrantFence's peer). It must not open another transaction and must
// take its row lock after the caller's run lock.
type GrantFencer interface {
	AttemptGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error
	ResourceGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error
}

// SQLGrantFence reads runtime_grants and runtime_resource_grants under a share
// row lock. The runtime authority revokes with a single UPDATE of the grant row
// and takes no other lock, so the two orders are the only ones: the revocation
// commits first (the fence sees revoked_at and refuses), or the record commits
// first (the revocation waits for the share lock and lands after it). Because the
// fence runs after the run row is locked and takes one grant row, it cannot
// deadlock with a revocation, which locks the grant row alone. The module-wide
// lock order is quota/work -> run/attempt -> grants -> secrets -> workspace -> events.
type SQLGrantFence struct{}

var _ GrantFencer = SQLGrantFence{}

// AttemptGrantLive implements GrantFencer: the attempt's grant exists, is not
// revoked and is the claim's session at the claim's signing generation.
func (SQLGrantFence) AttemptGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error {
	var session *string
	var generation int64
	var live bool
	err := tx.QueryRow(ctx, `SELECT session_id, signing_generation, revoked_at IS NULL FROM runtime_grants
		WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 FOR SHARE`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&session, &generation, &live)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: the attempt has no runtime grant", execution.ErrAttemptRevoked)
	case err != nil:
		return execution.Unavailablef(err, "the runtime grant of attempt %s could not be read", attempt)
	case !live || session == nil || *session != claim.Session || generation != claim.Generation:
		return fmt.Errorf("%w: the runtime grant is no longer the session's", execution.ErrAttemptRevoked)
	}
	return nil
}

// ResourceGrantLive implements GrantFencer for the grant of one run resource.
func (SQLGrantFence) ResourceGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error {
	var session *string
	var generation int64
	var live bool
	err := tx.QueryRow(ctx, `SELECT session_id, signing_generation, revoked_at IS NULL FROM runtime_resource_grants
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND resource_key = $4 FOR SHARE`,
		string(scope.TenantID), string(scope.ProjectID), string(run), key).Scan(&session, &generation, &live)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: the resource has no runtime grant", execution.ErrResourceRevoked)
	case err != nil:
		return execution.Unavailablef(err, "the runtime grant of resource %s could not be read", key)
	case !live || session == nil || *session != claim.Session || generation != claim.Generation:
		return fmt.Errorf("%w: the runtime grant is no longer the session's", execution.ErrResourceRevoked)
	}
	return nil
}

var _ execution.GrantFence = (*txEnv)(nil)

// AttemptGrantLive implements execution.GrantFence over the store's grant peer,
// on this transaction. A store without the peer (the component store) refuses.
func (e *txEnv) AttemptGrantLive(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error {
	if e.s.peers.GrantFence == nil || scope != e.scope {
		return fmt.Errorf("%w: the store has no runtime grant fence", execution.ErrNotConfigured)
	}
	return e.s.peers.GrantFence.AttemptGrantLive(ctx, e.tx, scope, attempt, claim)
}

// ResourceGrantLive implements execution.GrantFence.
func (e *txEnv) ResourceGrantLive(ctx context.Context, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error {
	if e.s.peers.GrantFence == nil || scope != e.scope {
		return fmt.Errorf("%w: the store has no runtime grant fence", execution.ErrNotConfigured)
	}
	return e.s.peers.GrantFence.ResourceGrantLive(ctx, e.tx, scope, run, key, claim)
}
