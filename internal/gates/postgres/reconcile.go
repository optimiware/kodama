package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

// ReadyGateSteps implements gates.Store: pending gate steps of nonterminal runs
// that have no gate yet, in bounded pages ordered by (project, run, step). The
// scan only lists candidates; readiness is decided by the use case from the
// loaded run, so a step whose dependencies are unfinished is skipped there.
func (s *Store) ReadyGateSteps(ctx context.Context, tenant domain.TenantID, after gates.StepRef, limit int) ([]gates.StepRef, error) {
	if err := tenant.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	var out []gates.StepRef
	err := s.inTenantRead(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s.project_id, s.run_id, s.step_id FROM step_runs s
			JOIN runs r ON r.tenant_id = s.tenant_id AND r.project_id = s.project_id AND r.id = s.run_id
			WHERE s.tenant_id = $1 AND s.status = 'pending' AND s.gate_ref IS NULL AND s.spec->>'kind' = 'gate'
			  AND r.status IN ('queued', 'running') AND r.cancel_requested_at IS NULL AND r.failure_intent_at IS NULL
			  AND (s.project_id, s.run_id, s.step_id) > ($2, $3, $4)
			ORDER BY s.project_id, s.run_id, s.step_id LIMIT $5`,
			string(tenant), string(after.Scope.ProjectID), string(after.RunID), after.StepID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var project, run, step string
			if err := rows.Scan(&project, &run, &step); err != nil {
				return err
			}
			out = append(out, gates.StepRef{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, RunID: domain.RunID(run), StepID: step})
		}
		return rows.Err()
	})
	return out, err
}

// OpenGates implements gates.Store: gates of nonterminal runs that were not
// invalidated, ordered by (project, id).
func (s *Store) OpenGates(ctx context.Context, tenant domain.TenantID, after gates.GateRef, limit int) ([]gates.GateRef, error) {
	if err := tenant.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	var out []gates.GateRef
	err := s.inTenantRead(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT g.project_id, g.id FROM gates g
			JOIN runs r ON r.tenant_id = g.tenant_id AND r.project_id = g.project_id AND r.id = g.run_id
			WHERE g.tenant_id = $1 AND g.invalidated_at IS NULL AND r.status IN ('queued', 'running')
			  AND (g.project_id, g.id) > ($2, $3)
			ORDER BY g.project_id, g.id LIMIT $4`,
			string(tenant), string(after.Scope.ProjectID), string(after.ID), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var project, id string
			if err := rows.Scan(&project, &id); err != nil {
				return err
			}
			out = append(out, gates.GateRef{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, ID: gates.ID(id)})
		}
		return rows.Err()
	})
	return out, err
}
