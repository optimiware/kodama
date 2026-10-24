package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// txEnv is the work.Tx bound to one mutating transaction. The cross-module
// guards come from the injected GuardBinder, which reads their tables through
// the same transaction; the checks that concern work-owned rows (WIP, children,
// parents, memberships) are answered here.
type txEnv struct {
	tx            pgx.Tx
	guards        work.TxGuards
	scope         domain.Scope
	projectLocked bool
}

var _ work.Tx = (*txEnv)(nil)

func (s *Store) newTxEnv(tx pgx.Tx, scope domain.Scope, projectLocked bool) *txEnv {
	return &txEnv{tx: tx, guards: s.bind(tx), scope: scope, projectLocked: projectLocked}
}

func (e *txEnv) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	return e.guards.Authorize(ctx, actor, scope, perm)
}

func (e *txEnv) WriterSettled(ctx context.Context, item work.Item) (bool, error) {
	return e.guards.WriterSettled(ctx, item)
}

func (e *txEnv) DependenciesSatisfied(ctx context.Context, item work.Item) error {
	return e.guards.DependenciesSatisfied(ctx, item)
}

func (e *txEnv) VerifySubject(ctx context.Context, item work.Item, claim work.ReviewClaim) (work.VerifiedSubject, error) {
	return e.guards.VerifySubject(ctx, item, claim)
}

func (e *txEnv) VerifyEvidence(ctx context.Context, scope domain.Scope, refs []work.EvidenceRef) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	return e.guards.VerifyEvidence(ctx, scope, refs)
}

// AdmitActive checks and reserves nothing itself: the caller writes the
// transition in this transaction while the project lock taken before the item
// lock is still held, so the count below cannot change underneath it. The count
// is a separate statement issued after the lock was acquired, so under READ
// COMMITTED it sees every earlier admission's commit. Blocked items count: a
// blocked_reason does not release the slot.
func (e *txEnv) AdmitActive(ctx context.Context, item work.Item) error {
	if !e.projectLocked || item.Scope != e.scope {
		return work.Unavailablef(nil, "active admission requires the project lock")
	}
	var limit int
	if err := e.tx.QueryRow(ctx, `SELECT active_wip_limit FROM projects WHERE tenant_id = $1 AND id = $2`,
		string(e.scope.TenantID), string(e.scope.ProjectID)).Scan(&limit); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the project was not found", work.ErrNotFound)
		}
		return err
	}
	var active int
	if err := e.tx.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND state = 'active'`,
		string(e.scope.TenantID), string(e.scope.ProjectID)).Scan(&active); err != nil {
		return err
	}
	if active >= limit {
		return fmt.Errorf("%w: %d of %d active slots are in use", work.ErrWIPLimit, active, limit)
	}
	return nil
}

// ChildSummary counts an epic's children by terminal state. Every state change
// holds the project lock, which the transition holding this call also holds, so
// the summary is consistent with the transition it supports.
func (e *txEnv) ChildSummary(ctx context.Context, item work.Item) (work.ChildSummary, error) {
	if item.Scope != e.scope || item.Kind != work.KindEpic {
		return work.ChildSummary{}, work.Invalidf("only an epic has a child summary")
	}
	var sum work.ChildSummary
	err := e.tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 'done'), count(*) FILTER (WHERE state = 'cancelled')
		FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND parent_id = $3`,
		string(e.scope.TenantID), string(e.scope.ProjectID), string(item.ID)).Scan(&sum.Total, &sum.Done, &sum.Cancelled)
	return sum, err
}

// AssigneeEligible requires a current tenant-wide or project membership of the
// user in this tenant; a user row alone is not project membership.
func (e *txEnv) AssigneeEligible(ctx context.Context, scope domain.Scope, userID string) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	var one int
	err := e.tx.QueryRow(ctx, `SELECT 1 FROM memberships
		WHERE tenant_id = $1 AND user_id = $2 AND (project_id IS NULL OR project_id = $3) LIMIT 1`,
		string(scope.TenantID), userID, string(scope.ProjectID)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the assignee was not found", work.ErrNotFound)
	}
	return err
}

// ParentEligible requires an epic of the same project and no cycle. The
// project lock (held for parent changes) serializes concurrent reparenting.
func (e *txEnv) ParentEligible(ctx context.Context, scope domain.Scope, id, parent domain.WorkItemID) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	if id == parent {
		return work.Invalidf("a work item cannot be its own parent")
	}
	var kind string
	err := e.tx.QueryRow(ctx, `SELECT kind FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(parent)).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the parent was not found", work.ErrNotFound)
	} else if err != nil {
		return err
	}
	if work.Kind(kind) != work.KindEpic {
		return work.Invalidf("the parent must be an epic")
	}
	// An existing item must not become its own ancestor. A new item (not yet
	// stored) cannot be anyone's ancestor.
	var cycle bool
	if err := e.tx.QueryRow(ctx, `WITH RECURSIVE ancestors(id, parent_id) AS (
			SELECT id, parent_id FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3
			UNION
			SELECT w.id, w.parent_id FROM work_items w JOIN ancestors a
			  ON w.tenant_id = $1 AND w.project_id = $2 AND w.id = a.parent_id
		) SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = $4)`,
		string(scope.TenantID), string(scope.ProjectID), string(parent), string(id)).Scan(&cycle); err != nil {
		return err
	}
	if cycle {
		return work.Invalidf("the parent would create a cycle")
	}
	return nil
}
