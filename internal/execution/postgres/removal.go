package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Removable implements execution.Store. It reads settled attempts whose
// workload removal is not recorded and whose run is pinned to backend; runs
// pinned to another backend, or to none, are not listed.
func (s *Store) Removable(ctx context.Context, tenant domain.TenantID, backend string, after execution.RemovalCursor, limit int) ([]execution.Removal, error) {
	if limit < 1 || limit > 1000 {
		return nil, execution.Invalidf("limit must be between 1 and 1000")
	}
	var out []execution.Removal
	err := s.inTenantRead(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.project_id, c.attempt_id, a.job_name, a.observed_job_uid, c.settled_at,
				r.inputs->'runtime'->>'backend'
			FROM attempt_controls c
			JOIN step_attempts a ON a.tenant_id = c.tenant_id AND a.project_id = c.project_id AND a.id = c.attempt_id
			JOIN runs r ON r.tenant_id = c.tenant_id AND r.project_id = c.project_id AND r.id = c.run_id
			WHERE c.tenant_id = $1 AND c.settled_at IS NOT NULL AND c.workload_removed_at IS NULL
				AND r.inputs->'runtime'->>'backend' = $6
				AND ($2 = '' OR (c.settled_at, c.project_id, c.attempt_id) > ($3, $4, $2))
			ORDER BY c.settled_at, c.project_id, c.attempt_id LIMIT $5`,
			string(tenant), string(after.AttemptID), micros(after.SettledAt), string(after.ProjectID), limit, backend)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				project, attempt, name string
				uid                    *string
				r                      execution.Removal
			)
			if err := rows.Scan(&project, &attempt, &name, &uid, &r.SettledAt, &r.Backend); err != nil {
				return err
			}
			r.SettledAt = r.SettledAt.UTC()
			r.Workload = execution.Workload{
				Scope:     domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)},
				AttemptID: domain.AttemptID(attempt), Name: name, UID: str(uid),
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// RecordRemoval implements execution.Store. Recording an already recorded
// removal changes nothing; an unsettled attempt has no removable workload. It
// locks only the bookkeeping row and takes no other lock while holding it, so
// it cannot invert the run lock order.
func (s *Store) RecordRemoval(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	if scope.Validate() != nil || attempt.Validate() != nil {
		return execution.ErrNotFound
	}
	return s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var settled, removed *time.Time
		err := tx.QueryRow(ctx, `SELECT settled_at, workload_removed_at FROM attempt_controls
			WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt)).Scan(&settled, &removed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return execution.ErrNotFound
		case err != nil:
			return err
		case removed != nil:
			return nil
		case settled == nil:
			return fmt.Errorf("%w: the attempt is not settled", execution.ErrInvalidTransition)
		}
		now := s.now()
		_, err = tx.Exec(ctx, `UPDATE attempt_controls SET workload_removed_at = $4, updated_at = $4
			WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt), now)
		return err
	})
}
