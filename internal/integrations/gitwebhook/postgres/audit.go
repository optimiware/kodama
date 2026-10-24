package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

const maxAuditReason = 2048

// writeAudit appends the record to the shared audit table inside tx, so it
// commits or rolls back with the change it describes. The caller has already
// established that tx carries the record's tenant context; the explicit tenant
// column and the table's policy enforce it again.
func (s *Store) writeAudit(ctx context.Context, tx pgx.Tx, a gw.AuditRecord) error {
	id, err := s.newID()
	if err != nil {
		return fmt.Errorf("%w: audit identifier: %w", gw.ErrUnavailable, err)
	}
	meta := map[string]string{}
	for k, v := range a.Metadata {
		meta[k] = v
	}
	if a.DeliveryID != "" {
		meta["delivery_id"] = a.DeliveryID
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("%w: audit metadata: %v", gw.ErrInvalid, err)
	}
	actorType, actorID := "system", "git_hook"
	if a.Principal != "" {
		actorType, actorID = "user", a.Principal
	}
	var at *time.Time
	if !a.At.IsZero() {
		u := a.At.UTC()
		at = &u
	}
	reason := a.Reason
	if len(reason) > maxAuditReason {
		reason = reason[:maxAuditReason]
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, reason, metadata)
VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6, $7, 'git_trigger', $8, $9, $10::jsonb)`,
		string(a.Scope.TenantID), id, string(a.Scope.ProjectID), at, actorType, actorID, a.Action, string(a.TriggerID), nullable(reason), string(raw))
	return err
}
