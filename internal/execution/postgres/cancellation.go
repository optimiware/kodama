package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// RequestCancellation implements execution.Store in its own transaction.
func (s *Store) RequestCancellation(ctx context.Context, c execution.WorkCancellation) error {
	return s.inTenant(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.RequestCancellationInTx(ctx, tx, c)
	})
}

// RequestCancellationInTx is the transaction-bound requester consumed by the
// work module's cancellation binding (workpg.CancellationBinding). In the
// consumer's inbox transaction it records the item-scoped intent idempotently
// and cancels the item's nonterminal runs: dispatch is refused from the commit
// on, live attempts lose authority and the runs settle before completing. A
// duplicate or reordered delivery repeats an idempotent request; a run created
// later is refused by the admission lock, and the periodic scan covers a lost
// delivery. The runs are locked together in ID order, their runtime grants are
// revoked and secrets released, and only then are queued workspace writer
// settlements run and events appended, so the batch follows the package lock
// order across runs. The request is intent and revocation only: a writer slot
// or capacity reservation of an attempt whose workload is not yet confirmed
// gone is released by the later settlement, never by this transaction.
func (s *Store) RequestCancellationInTx(ctx context.Context, tx pgx.Tx, c execution.WorkCancellation) error {
	if err := c.Scope.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	if err := c.WorkItemID.Validate(); err != nil {
		return execution.Invalidf("%v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_cancellation_requests (tenant_id, project_id, work_item_id, version, requested_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, project_id, work_item_id) DO UPDATE SET version = EXCLUDED.version
		WHERE work_cancellation_requests.version < EXCLUDED.version`,
		string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.WorkItemID), int64(c.Version), s.now()); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM runs WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3 AND status IN ('queued', 'running')
		ORDER BY id FOR UPDATE`, string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.WorkItemID))
	if err != nil {
		return err
	}
	var ids []domain.RunID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, domain.RunID(id))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Every run row is locked, in ID order, before any run's grants, secrets,
	// workspace or events are touched; then each lock level is completed for
	// the whole batch before the next (see the package lock order).
	pending := make([]*pendingUpdate, 0, len(ids))
	for _, id := range ids {
		p, err := s.mutateInTx(ctx, tx, c.Scope, id, execution.UpdateOptions{}, execution.CancelMutation(execution.ReasonWorkCancelled, nil))
		if err != nil {
			return err
		}
		pending = append(pending, p)
	}
	for _, p := range pending {
		if err := p.settleWriters(ctx); err != nil {
			return err
		}
	}
	for _, p := range pending {
		if err := s.recordInTx(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}
