package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// Record writes the audit rows and typed events of a change inside the open
// transaction. It is the last operation of a mutation: it takes the event
// store's own locks, and nothing may be locked or written after it.
func (t *txImpl) Record(ctx context.Context, c identity.Change) error {
	if t.recorded {
		return errOrder("Record twice")
	}
	t.recorded = true
	if c.Tenant != t.tenant {
		return fmt.Errorf("%w: the change belongs to another tenant", identity.ErrInvalid)
	}
	return recordChange(ctx, t.tx, t.events, c)
}

func auditActorType(a identity.ActorType) (string, error) {
	switch a {
	case identity.ActorUser:
		return "user", nil
	case identity.ActorOperator:
		return "operator", nil
	case identity.ActorService:
		return "system", nil
	case identity.ActorWorkload:
		return "workload", nil
	}
	return "", fmt.Errorf("identity/postgres: unknown actor type %q", a)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("identity/postgres: marshal %T: %v", v, err)) // plain structs; cannot fail
	}
	return b
}

// recordChange inserts one audit row per binding operation and per project
// effect, then appends the typed events. Audit metadata carries identifiers,
// roles and versions only: never a token, email or display name.
func recordChange(ctx context.Context, tx pgx.Tx, appender EventAppender, c identity.Change) error {
	actorType, err := auditActorType(c.Actor.Type)
	if err != nil {
		return err
	}
	if err := c.Actor.Validate(); err != nil {
		return err
	}
	n := 0
	audit := func(project domain.ProjectID, action, resourceType, resourceID string, version int64, meta map[string]any) error {
		n++
		_, err := tx.Exec(ctx, `INSERT INTO audit
			(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			string(c.Tenant), fmt.Sprintf("aud_%s_%d", c.ID, n), nilIfEmpty(string(project)), micros(c.OccurredAt), actorType, c.Actor.ID,
			action, resourceType, resourceID, version, nilIfEmpty(c.RequestID), mustJSON(meta))
		return err
	}
	correlation := c.RequestID
	if correlation == "" {
		correlation = c.ID
	}
	actor := eventsv1.Actor{Type: eventsv1.ActorType(c.Actor.Type), ID: c.Actor.ID}

	for _, bc := range c.Bindings {
		ids := make([]string, 0, len(bc.Ops))
		for _, op := range bc.Ops {
			meta := map[string]any{"userId": string(bc.User), "role": string(op.Role), "effectiveVersion": bc.EffectiveVersion}
			if op.PreviousRole != "" {
				meta["previousRole"] = string(op.PreviousRole)
			}
			if err := audit(bc.Project, string(op.Kind), "membership", string(op.ID), int64(op.Version), meta); err != nil {
				return err
			}
			ids = append(ids, string(op.ID))
		}
		data, err := json.Marshal(eventsv1.MembershipChanged{UserID: string(bc.User), BindingIDs: ids, EffectiveVersion: bc.EffectiveVersion})
		if err != nil {
			return err
		}
		if _, err := appender.Append(ctx, tx, eventsv1.Envelope{
			ID: fmt.Sprintf("evt_%s_m%d", c.ID, n), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeMembershipChanged, OccurredAt: c.OccurredAt.UTC(),
			TenantID: c.Tenant, ProjectID: bc.Project,
			Resource: eventsv1.Resource{Type: "membership", ID: string(bc.User), Version: bc.EffectiveVersion},
			Actor:    actor, CorrelationID: correlation, Data: data,
		}); err != nil {
			return err
		}
	}
	if pc := c.Project; pc != nil {
		action := string(identity.ChangeProjectUpdated)
		if c.Kind == identity.ChangeProjectCreated {
			action = string(identity.ChangeProjectCreated)
		}
		if err := audit(pc.Scope.ProjectID, action, "project", string(pc.Scope.ProjectID), int64(pc.Version), map[string]any{"changed": pc.Changed}); err != nil {
			return err
		}
		data, err := json.Marshal(eventsv1.ProjectSettingsChanged{ChangedSettings: pc.Changed, Version: int64(pc.Version)})
		if err != nil {
			return err
		}
		if _, err := appender.Append(ctx, tx, eventsv1.Envelope{
			ID: fmt.Sprintf("evt_%s_p", c.ID), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeProjectSettingsChanged, OccurredAt: c.OccurredAt.UTC(),
			TenantID: c.Tenant, ProjectID: pc.Scope.ProjectID,
			Resource: eventsv1.Resource{Type: "project", ID: string(pc.Scope.ProjectID), Version: int64(pc.Version)},
			Actor:    actor, CorrelationID: correlation, Data: data,
		}); err != nil {
			return err
		}
	}
	return nil
}
