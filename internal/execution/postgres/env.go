package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/workspace"
)

// txEnv is the execution.Env bound to one open tenant transaction.
type txEnv struct {
	s     *Store
	tx    pgx.Tx
	scope domain.Scope

	quotaLocked bool
	work        execution.WorkAdmission
	workErr     error
	workTaken   bool
	authority   execution.TxAuthority

	// deferWriter queues SettleWriter calls (workspace row, level 5) so the
	// store runs them after the queued secret release (level 4), whatever
	// order the entities queued their effects in.
	deferWriter bool
	settles     []func(ctx context.Context) error
}

var _ execution.Env = (*txEnv)(nil)

func (s *Store) newEnv(tx pgx.Tx, scope domain.Scope) *txEnv {
	return &txEnv{s: s, tx: tx, scope: scope}
}

func (e *txEnv) Now() time.Time             { return e.s.now() }
func (e *txEnv) NewID(prefix string) string { return e.s.opts.NewID(prefix) }
func (e *txEnv) Work() (execution.WorkAdmission, error) {
	if !e.workTaken {
		return execution.WorkAdmission{}, execution.Unavailablef(nil, "the work item admission lock was not taken")
	}
	return e.work, e.workErr
}

// Authorize checks the actor's permission through the transaction. A missing
// binding fails closed.
func (e *txEnv) Authorize(ctx context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	if scope != e.scope {
		return execution.ErrDenied
	}
	if e.authority == nil {
		e.authority = e.s.peers.Authority(e.tx)
	}
	if e.authority == nil {
		return execution.Unavailablef(nil, "authority is not bound")
	}
	return e.authority.Authorize(ctx, actor, scope, perm)
}

// lockQuota takes the tenant scope row, then the project scope row, FOR UPDATE.
// Concurrent claims of one tenant therefore serialize in a fixed order.
func (e *txEnv) lockQuota(ctx context.Context) error {
	for _, key := range []string{"tenant", "project:" + string(e.scope.ProjectID)} {
		if _, err := e.tx.Exec(ctx, `INSERT INTO execution_quota_scopes (tenant_id, scope_key) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			string(e.scope.TenantID), key); err != nil {
			return err
		}
		var one int
		if err := e.tx.QueryRow(ctx, `SELECT 1 FROM execution_quota_scopes WHERE tenant_id = $1 AND scope_key = $2 FOR UPDATE`,
			string(e.scope.TenantID), key).Scan(&one); err != nil {
			return err
		}
	}
	e.quotaLocked = true
	return nil
}

// CapacityAvailable counts the held reservations of the tenant and of the
// project under the scope locks and refuses when either limit is reached.
func (e *txEnv) CapacityAvailable(ctx context.Context, scope domain.Scope, lim execution.Limit) error {
	if !e.quotaLocked || scope != e.scope {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	if err := lim.Validate(); err != nil {
		return execution.Unavailablef(err, "invalid limits")
	}
	var tenantHeld, projectHeld int
	if err := e.tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE project_id = $2) FROM execution_reservations
		WHERE tenant_id = $1 AND released_at IS NULL`, string(scope.TenantID), string(scope.ProjectID)).Scan(&tenantHeld, &projectHeld); err != nil {
		return err
	}
	// A run_shared run's active-attempt slot is reserved for its whole life.
	var sharedTenant, sharedProject int
	if err := e.tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE project_id = $2) FROM execution_run_reservations
		WHERE tenant_id = $1 AND released_at IS NULL`, string(scope.TenantID), string(scope.ProjectID)).Scan(&sharedTenant, &sharedProject); err != nil {
		return err
	}
	tenantHeld, projectHeld = tenantHeld+sharedTenant, projectHeld+sharedProject
	if tenantHeld >= lim.TenantConcurrency || projectHeld >= lim.ProjectConcurrency {
		return fmt.Errorf("%w: %d of %d tenant and %d of %d project slots are in use", execution.ErrQuotaExceeded,
			tenantHeld, lim.TenantConcurrency, projectHeld, lim.ProjectConcurrency)
	}
	return nil
}

// ReserveCapacity records the reservation exactly once.
func (e *txEnv) ReserveCapacity(ctx context.Context, at *execution.Attempt) error {
	if !e.quotaLocked {
		return execution.Unavailablef(nil, "capacity is not locked")
	}
	_, err := e.tx.Exec(ctx, `INSERT INTO execution_reservations (tenant_id, project_id, attempt_id, run_id, held_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		string(at.Scope.TenantID), string(at.Scope.ProjectID), string(at.ID), string(at.RunID), e.s.now())
	return err
}

// ReleaseCapacity releases the reservation once; a repeat is a no-op.
func (e *txEnv) ReleaseCapacity(ctx context.Context, at *execution.Attempt) error {
	_, err := e.tx.Exec(ctx, `UPDATE execution_reservations SET released_at = $4
		WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND released_at IS NULL`,
		string(at.Scope.TenantID), string(at.Scope.ProjectID), string(at.ID), e.s.now())
	return err
}

var system = workspace.Actor{Type: workspace.ActorService, ID: "execution"}

// ReserveWriter joins the workspace's writer reservation to this transaction,
// so the attempt, the capacity reservation and the writer slot commit or roll
// back together.
func (e *txEnv) ReserveWriter(ctx context.Context, at *execution.Attempt) error {
	in := workspace.ReserveWriterInput{
		Scope: at.Scope, WorkspaceID: workspace.ID(at.WorkspaceID), AttemptID: at.ID,
		InputCommit: workspace.Commit(at.InputCommit), ExpectedTip: workspace.Commit(at.ExpectedTip), Actor: system,
	}
	_, err := e.s.peers.Workspaces.UpdateInTx(ctx, e.tx, at.Scope, in.WorkspaceID, workspace.Load{}, workspace.ReserveWriterMutation(in, e.s.now))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrWriterBusy), errors.Is(err, workspace.ErrFenced), errors.Is(err, workspace.ErrAdmissionClosed):
		return fmt.Errorf("%w: the workspace writer slot is not available", execution.ErrBusy)
	case errors.Is(err, workspace.ErrStale):
		return fmt.Errorf("%w: the workspace tip moved", execution.ErrStale)
	case errors.Is(err, workspace.ErrNotFound):
		return execution.ErrNotFound
	}
	return err
}

var _ execution.InternalWriterEnv = (*txEnv)(nil)

// ReserveInternalWriter joins the reservation of a work item's internally hosted
// workspace writer to this transaction: the same single slot, reserved only while
// the workspace still has the version the writer gate was checked at, so the
// reservation never commits against a gate answer that predates another writer's
// unknown push (ErrStale: the claim is repeated from the gate).
func (e *txEnv) ReserveInternalWriter(ctx context.Context, at *execution.Attempt, observed domain.Version) error {
	in := workspace.ReserveWriterInput{Scope: at.Scope, WorkspaceID: workspace.ID(at.WorkspaceID), AttemptID: at.ID, Actor: system}
	_, err := e.s.peers.Workspaces.UpdateInTx(ctx, e.tx, at.Scope, in.WorkspaceID, workspace.Load{Associations: true},
		workspace.ReserveInternalWriterMutation(in, observed, e.s.now))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrWriterBusy), errors.Is(err, workspace.ErrFenced), errors.Is(err, workspace.ErrAdmissionClosed):
		return fmt.Errorf("%w: the workspace writer slot is not available", execution.ErrBusy)
	case errors.Is(err, workspace.ErrStale):
		return fmt.Errorf("%w: the workspace changed since the writer gate was checked", execution.ErrStale)
	case errors.Is(err, workspace.ErrNotFound):
		return execution.ErrNotFound
	}
	return err
}

// SettleWriter ends the writer reservation with the attempt's outcome. An
// unsettled publication blocks it; a slot already released is success.
func (e *txEnv) SettleWriter(ctx context.Context, at *execution.Attempt, outcome execution.WriterOutcome, source string) error {
	if e.deferWriter {
		e.settles = append(e.settles, func(ctx context.Context) error { return e.settleWriter(ctx, at, outcome, source) })
		return nil
	}
	return e.settleWriter(ctx, at, outcome, source)
}

// runSettles performs the queued writer settlements in the order they were
// requested and stops deferring.
func (e *txEnv) runSettles(ctx context.Context) error {
	e.deferWriter = false
	settles := e.settles
	e.settles = nil
	for _, settle := range settles {
		if err := settle(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (e *txEnv) settleWriter(ctx context.Context, at *execution.Attempt, outcome execution.WriterOutcome, source string) error {
	in := workspace.SettleWriterInput{
		Scope: at.Scope, WorkspaceID: workspace.ID(at.WorkspaceID), AttemptID: at.ID,
		Outcome: workspace.WriterOutcome(outcome), SourceCommit: workspace.Commit(source), Actor: system,
	}
	_, err := e.s.peers.Workspaces.UpdateInTx(ctx, e.tx, at.Scope, in.WorkspaceID, workspace.Load{Receipt: at.ID}, workspace.SettleWriterMutation(in, e.s.now))
	switch {
	case err == nil, errors.Is(err, workspace.ErrNotWriter):
		return nil
	case errors.Is(err, workspace.ErrPublicationPending):
		return fmt.Errorf("%w: the writer's publication is unsettled", execution.ErrBusy)
	case errors.Is(err, workspace.ErrOutputNotSelectable):
		return fmt.Errorf("%w: the writer's output is not published", execution.ErrStale)
	}
	return err
}

// ReleaseSecrets releases the run's secret-consumer pins through the secrets
// module's transaction seam; historical reference metadata is retained.
func (e *txEnv) ReleaseSecrets(ctx context.Context, run *execution.Run) error {
	_, err := e.s.peers.Secrets.ReleasePins(ctx, e.tx, run.Scope, secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(run.ID)})
	return err
}

// takeWork locks the work item for admission, FOR SHARE, until the transaction
// ends, so a concurrent cancellation either commits first (and is seen) or waits
// for this transaction's durable claim and then finds the new work to cancel.
func (e *txEnv) takeWork(ctx context.Context, id domain.WorkItemID) error {
	adm, err := e.s.peers.Work(ctx, e.tx, e.scope, id)
	e.workTaken = true
	switch {
	case err == nil:
		e.work = execution.WorkAdmission{ContentRevision: adm.ContentRevision}
	case errors.Is(err, work.ErrCancelled):
		e.workErr = execution.ErrWorkCancelled
	case errors.Is(err, work.ErrNotActionable):
		e.workErr = execution.ErrNotActionable
	case errors.Is(err, work.ErrNotFound):
		e.workErr = execution.ErrNotFound
	default:
		return err
	}
	return nil
}
