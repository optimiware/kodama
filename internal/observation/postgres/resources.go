package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/observation"
)

// WorkResources is the merged work-management binding: it reads the current id,
// kind, state and version of the project's work items from work_items, through
// the snapshot transaction and with explicit tenant and project predicates. It
// reconstructs nothing from events.
type WorkResources struct{}

var _ ResourceBinding = WorkResources{}

// ReadResources implements ResourceBinding. A run-scoped query has no work
// items. Rows are ordered by ID so a bounded read is deterministic.
func (WorkResources) ReadResources(ctx context.Context, tx pgx.Tx, q ResourceQuery) ([]observation.Resource, error) {
	if q.Scope.Run != "" || !contains(q.Families, observation.FamilyWork) {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, state, version FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 ORDER BY id LIMIT $3`,
		string(q.Scope.Tenant), string(q.Scope.Project), q.Limit)
	if err != nil {
		return nil, fmt.Errorf("observation/postgres: read work items: %w", err)
	}
	defer rows.Close()
	var out []observation.Resource
	for rows.Next() {
		r := observation.Resource{Type: observation.FamilyWork}
		if err := rows.Scan(&r.ID, &r.Kind, &r.State, &r.Version); err != nil {
			return nil, fmt.Errorf("observation/postgres: scan work item: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("observation/postgres: read work items: %w", err)
	}
	return out, nil
}
