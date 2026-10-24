package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/workspace"
)

// AttemptAuthority implements workspace.Attempts over the execution tables: the
// current authority of an attempt over a workspace. An attempt is live only
// while it is running and its authority was not revoked; cancellation, failure,
// timeout, supersession and completion all end it. A foreign or unknown attempt
// is workspace.ErrNotFound.
type AttemptAuthority struct{ s *Store }

var _ workspace.Attempts = (*AttemptAuthority)(nil)

// NewAttemptAuthority adapts the store to the workspace module's port.
func NewAttemptAuthority(s *Store) (*AttemptAuthority, error) {
	if s == nil {
		return nil, errors.New("execution/postgres: a store is required")
	}
	return &AttemptAuthority{s: s}, nil
}

// Authority implements workspace.Attempts.
func (a *AttemptAuthority) Authority(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (workspace.WriterAuthority, error) {
	var out workspace.WriterAuthority
	err := a.s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ws *string
		var access, status string
		var revoked bool
		e := tx.QueryRow(ctx, `SELECT a.workspace_id, a.access, a.status, c.revoked_at IS NOT NULL
			FROM step_attempts a JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
			WHERE a.tenant_id = $1 AND a.project_id = $2 AND a.id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&ws, &access, &status, &revoked)
		if errors.Is(e, pgx.ErrNoRows) {
			return workspace.ErrNotFound
		}
		if e != nil {
			return e
		}
		out = workspace.WriterAuthority{WorkspaceID: workspace.ID(str(ws)), Access: workspace.Access(access), Live: status == string(execution.AttemptRunning) && !revoked}
		return nil
	})
	return out, err
}

// WriterGuard implements work.WriterGuard over one open transaction: the item's
// writer attempts must all be settled (terminal, workload gone, effects
// accounted for) before the item's workspace may be released by a business
// transition. A pending, unknown-outcome or unterminated writer reports false.
type WriterGuard struct{ tx pgx.Tx }

var _ work.WriterGuard = WriterGuard{}

// NewWriterGuard binds the guard to the work mutation's transaction.
func NewWriterGuard(tx pgx.Tx) WriterGuard { return WriterGuard{tx: tx} }

// WriterSettled implements work.WriterGuard.
func (g WriterGuard) WriterSettled(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (bool, error) {
	var open bool
	err := g.tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM runs r
		JOIN step_attempts a ON a.tenant_id = r.tenant_id AND a.project_id = r.project_id AND a.run_id = r.id
		LEFT JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
		WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.work_item_id = $3 AND a.access = 'write' AND (c.settled_at IS NULL))`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&open)
	return !open, err
}

// AttemptSnapshot is the authoritative view of one attempt that the runtime
// admission module (T-019) maps into its own state: it never trusts a request.
type AttemptSnapshot struct {
	Scope         domain.Scope
	RunID         domain.RunID
	AttemptID     domain.AttemptID
	Status        execution.AttemptStatus
	Live          bool
	Revision      int64
	WorkloadName  string
	ContextDigest string
	Access        execution.Access
	WorkspaceID   string
}

// LockAttempt reads an attempt inside the caller's tenant transaction and locks
// its run row FOR SHARE, so a concurrent cancellation (which locks the run FOR
// UPDATE) either committed first, and is seen, or waits for the caller's
// transaction. Lock order: callers that also lock grants take this first.
func LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (AttemptSnapshot, error) {
	var runID string
	err := tx.QueryRow(ctx, `SELECT run_id FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptSnapshot{}, execution.ErrNotFound
	}
	if err != nil {
		return AttemptSnapshot{}, err
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
		string(scope.TenantID), string(scope.ProjectID), runID).Scan(&one); err != nil {
		return AttemptSnapshot{}, err
	}
	var snap AttemptSnapshot
	var ws *string
	var status, access string
	var revoked bool
	err = tx.QueryRow(ctx, `SELECT a.status, a.authority_revision, a.job_name, a.context_digest, a.access, a.workspace_id, c.revoked_at IS NOT NULL
		FROM step_attempts a JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
		WHERE a.tenant_id = $1 AND a.project_id = $2 AND a.id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&status, &snap.Revision, &snap.WorkloadName, &snap.ContextDigest, &access, &ws, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptSnapshot{}, execution.ErrNotFound
	}
	if err != nil {
		return AttemptSnapshot{}, err
	}
	snap.Scope, snap.RunID, snap.AttemptID = scope, domain.RunID(runID), attempt
	snap.Status, snap.Access, snap.WorkspaceID = execution.AttemptStatus(status), execution.Access(access), str(ws)
	snap.Live = snap.Status == execution.AttemptRunning && !revoked
	return snap, nil
}

// ObserveAttempt reads an attempt in its own read-only transaction without
// locks; the answer is advisory.
func (s *Store) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptSnapshot, error) {
	var out AttemptSnapshot
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = LockAttemptNoLock(ctx, tx, scope, attempt)
		return err
	})
	return out, classify(err)
}

// LockAttemptNoLock is LockAttempt without the run lock (read-only
// transactions cannot take it).
func LockAttemptNoLock(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (AttemptSnapshot, error) {
	var runID string
	var snap AttemptSnapshot
	var ws *string
	var status, access string
	var revoked bool
	err := tx.QueryRow(ctx, `SELECT a.run_id, a.status, a.authority_revision, a.job_name, a.context_digest, a.access, a.workspace_id, c.revoked_at IS NOT NULL
		FROM step_attempts a JOIN attempt_controls c ON c.tenant_id = a.tenant_id AND c.project_id = a.project_id AND c.attempt_id = a.id
		WHERE a.tenant_id = $1 AND a.project_id = $2 AND a.id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&runID, &status, &snap.Revision, &snap.WorkloadName, &snap.ContextDigest, &access, &ws, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptSnapshot{}, execution.ErrNotFound
	}
	if err != nil {
		return AttemptSnapshot{}, err
	}
	snap.Scope, snap.RunID, snap.AttemptID = scope, domain.RunID(runID), attempt
	snap.Status, snap.Access, snap.WorkspaceID = execution.AttemptStatus(status), execution.Access(access), str(ws)
	snap.Live = snap.Status == execution.AttemptRunning && !revoked
	return snap, nil
}
