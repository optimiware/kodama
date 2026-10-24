package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// Admission is the current eligibility of a work item for workspace
// provisioning or run dispatch, as read under the item lock.
type Admission struct {
	Scope           domain.Scope
	ID              domain.WorkItemID
	Kind            work.Kind
	State           work.State
	Version         domain.Version
	ContentRevision domain.ContentRevision
}

// LockActionable is the transaction-local admission seam for the workspace
// (T-013) and execution (T-020) modules. Inside the caller's own tenant
// transaction tx it locks the work item row FOR SHARE and returns its current
// kind, state, version and content revision. It refuses
//
//   - an item outside scope (work.ErrNotFound, indistinguishable from a missing
//     one),
//   - an epic (work.ErrNotActionable): epics have no workspace, and
//   - a cancelled item (work.ErrCancelled): cancellation prevents new dispatch.
//
// The share lock is held until tx ends. The caller must keep tx open through its
// durable reservation or claim (the unique workspace row, the dispatched
// attempt and writer reservation) and commit them together: a concurrent
// cancellation takes the same row FOR UPDATE, so it either commits first, in
// which case this call refuses, or waits for the reservation's commit and then
// finds the new work to cancel. An earlier advisory read of the item is not a
// substitute. Concurrent admissions share the lock and cooperate through the
// caller's unique constraint.
//
// Lock order: no project lock is needed or taken here. After this call the
// caller may lock its own rows (workspace, attempt); work transitions take
// project, then item, then read those rows without locking them, so the orders
// cannot cycle. Never call LockActionable while holding a lock a work mutation
// might need earlier in the order (the project row).
func LockActionable(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.WorkItemID) (Admission, error) {
	if err := scope.Validate(); err != nil {
		return Admission{}, work.Invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return Admission{}, work.Invalidf("%v", err)
	}
	var kind, state string
	var version, revision int64
	err := tx.QueryRow(ctx, `SELECT kind, state, version, content_revision FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&kind, &state, &version, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, notFound()
	} else if err != nil {
		return Admission{}, err
	}
	a := Admission{Scope: scope, ID: id, Kind: work.Kind(kind), State: work.State(state),
		Version: domain.Version(version), ContentRevision: domain.ContentRevision(revision)}
	if !a.Kind.Actionable() {
		return a, fmt.Errorf("%w: an epic has no workspace", work.ErrNotActionable)
	}
	if a.State == work.StateCancelled {
		return a, fmt.Errorf("%w: a cancelled work item admits no new work", work.ErrCancelled)
	}
	return a, nil
}

// CancelledItems implements work.CancellationSource: a bounded, tenant-scoped
// scan of cancelled items ordered by (project, id), starting after the cursor.
// The cancelled state is permanent and committed together with the cancellation
// event, so a scan recovers every cancellation whose event was lost or delayed.
func (s *Store) CancelledItems(ctx context.Context, tenant domain.TenantID, after work.CancelledCursor, limit int) ([]work.CancelledItem, error) {
	if limit < 1 || limit > 1000 {
		return nil, work.Invalidf("limit must be between 1 and 1000")
	}
	var out []work.CancelledItem
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id, id, version FROM work_items
			WHERE tenant_id = $1 AND state = 'cancelled' AND (project_id, id) > ($2, $3)
			ORDER BY project_id, id LIMIT $4`, string(tenant), string(after.ProjectID), string(after.ID), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var project, id string
			var version int64
			if err := rows.Scan(&project, &id, &version); err != nil {
				return err
			}
			out = append(out, work.CancelledItem{CancelRequest: work.CancelRequest{
				Scope:      domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)},
				WorkItemID: domain.WorkItemID(id), Version: domain.Version(version),
			}})
		}
		return rows.Err()
	})
	return out, err
}

var _ work.CancellationSource = (*Store)(nil)
