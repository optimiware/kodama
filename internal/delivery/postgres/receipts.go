package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

// history appends one row of the deployment's append-only history inside the
// mutation's transaction. Together with the deployment row it is the durable
// receipt: a retry, a replay and a restart read what was committed, and settled
// history is never rewritten (a trigger refuses UPDATE and DELETE).
func (s *Store) history(ctx context.Context, tx pgx.Tx, m delivery.Mutation, scope domain.Scope, id delivery.DeploymentID, tr delivery.Transition, version domain.Version, fromStatus string) error {
	detail := tr.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	dj, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO deployment_history
(tenant_id, project_id, deployment_id, kind, version, from_status, to_status, reason, actor_type, actor_id, attempt_id, detail, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13)`,
		string(scope.TenantID), string(scope.ProjectID), string(id), tr.Kind, int64(version), fromStatus, string(tr.Change.ToStatus), tr.Change.ToReason,
		string(m.Actor.Type), m.Actor.ID, string(tr.Attempt), string(dj), m.Now.UTC())
	return err
}

// History implements delivery.Store.
func (s *Store) History(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (out []delivery.HistoryEntry, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT seq, kind, version, from_status, to_status, reason, actor_type, actor_id, attempt_id, detail, occurred_at
FROM deployment_history WHERE tenant_id = $1 AND project_id = $2 AND deployment_id = $3 ORDER BY seq`,
			string(scope.TenantID), string(scope.ProjectID), string(id))
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var h delivery.HistoryEntry
			var from, to, actor, attempt string
			var version int64
			var detail []byte
			if e := rows.Scan(&h.Seq, &h.Kind, &version, &from, &to, &h.Reason, &actor, &h.Actor.ID, &attempt, &detail, &h.At); e != nil {
				return e
			}
			h.Version, h.FromStatus, h.ToStatus, h.Actor.Type, h.Attempt, h.At = domain.Version(version), delivery.Status(from), delivery.Status(to),
				delivery.ActorType(actor), domain.AttemptID(attempt), utc(h.At)
			if e := json.Unmarshal(detail, &h.Detail); e != nil {
				return e
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}
