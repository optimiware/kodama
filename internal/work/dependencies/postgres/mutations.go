package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

// Mutate implements dependencies.Repository.
//
// Order inside the transaction: project lock, dependent row lock, the
// transaction-bound permission recheck (before the version is compared, so a
// revoked caller learns nothing about the version), the expected-version
// comparison, the mutation function (graph facts read after the locks), the
// edge write, the conditional version UPDATE, history, audit and finally the
// event append, which takes the event store's own locks last.
func (s *Store) Mutate(ctx context.Context, req dependencies.MutationRequest, fn dependencies.MutateFunc) (dependencies.MutationResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return dependencies.MutationResult{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	var out dependencies.MutationResult
	ranToCommit := false
	err := s.inTenant(ctx, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockProject(ctx, tx, req.Scope); err != nil {
			return err
		}
		cur, err := lockOwner(ctx, tx, req.Scope, req.Dependent)
		if err != nil {
			return err
		}
		if err := (work.TxGuards{Permissions: s.bind(tx)}).Authorize(ctx, req.Actor, req.Scope, req.Permission); err != nil {
			return err
		}
		if cur.Version != req.Expected {
			return fmt.Errorf("%w: the work item is at version %d", work.ErrVersionConflict, cur.Version)
		}
		change, err := fn(ctx, &txEnv{tx: tx, scope: req.Scope}, cur)
		if errors.Is(err, work.ErrNoChange) {
			out = dependencies.MutationResult{Owner: cur}
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateChange(req, cur, change); err != nil {
			return err
		}
		l := change.Link
		switch change.Operation {
		case dependencies.OpAdd:
			if _, err := tx.Exec(ctx, `INSERT INTO work_item_dependencies
				(tenant_id, project_id, dependent_id, prerequisite_id, history_id, created_by_type, created_by_id, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				string(l.Scope.TenantID), string(l.Scope.ProjectID), string(l.DependentID), string(l.PrerequisiteID),
				change.History.ID, string(change.History.Actor.Type), change.History.Actor.ID, change.History.OccurredAt); err != nil {
				return err
			}
		case dependencies.OpRemove:
			tag, err := tx.Exec(ctx, `DELETE FROM work_item_dependencies
				WHERE tenant_id = $1 AND project_id = $2 AND dependent_id = $3 AND prerequisite_id = $4`,
				string(l.Scope.TenantID), string(l.Scope.ProjectID), string(l.DependentID), string(l.PrerequisiteID))
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				// Unreachable under the project lock; the check guards any path that bypassed it.
				return fmt.Errorf("%w: the dependency changed concurrently", work.ErrVersionConflict)
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE work_items SET version = $4, updated_at = $5
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $6`,
			string(req.Scope.TenantID), string(req.Scope.ProjectID), string(req.Dependent),
			int64(change.Owner.Version), change.History.OccurredAt, int64(req.Expected))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: the work item changed concurrently", work.ErrVersionConflict)
		}
		if err := s.recordMutation(ctx, tx, change); err != nil {
			return err
		}
		out = dependencies.MutationResult{Owner: change.Owner, Changed: true}
		ranToCommit = true
		return nil
	})
	return out, uncertain(err, ranToCommit)
}

// validateChange refuses a mutation function result that does not continue the
// locked owner: a defensive check that the entity and the adapter agree.
func validateChange(req dependencies.MutationRequest, cur dependencies.Owner, c dependencies.Change) error {
	l := c.Link
	if (c.Operation != dependencies.OpAdd && c.Operation != dependencies.OpRemove) ||
		l.Scope != req.Scope || l.DependentID != req.Dependent ||
		c.Owner.Scope != req.Scope || c.Owner.ID != cur.ID || c.Owner.Version != cur.Version+1 ||
		c.Owner.ContentRevision != cur.ContentRevision || c.Owner.State != cur.State ||
		c.History.Scope != req.Scope || c.History.WorkItemID != cur.ID || c.History.Version != c.Owner.Version ||
		c.History.ContentRevision != cur.ContentRevision || c.History.Kind != work.ChangeUpdated {
		return fmt.Errorf("%w: the mutation does not continue the locked work item", work.ErrInvalid)
	}
	return nil
}

// uncertain converts a failure after every statement succeeded into an
// unknown-outcome error: the commit may or may not have happened, so the caller
// rereads the dependencies and the owner version, and nothing is replayed
// automatically. A failure before that point rolled back and stays classified.
func uncertain(err error, ranToCommit bool) error {
	if err != nil && ranToCommit {
		return work.Unavailablef(err, "the outcome of the write is unknown; reread the dependencies and the work item version before retrying")
	}
	return err
}

func jsonb(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("work/dependencies/postgres: marshal %T: %v", v, err)) // plain values; cannot fail
	}
	return b
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// recordMutation writes the history entry, the audit row and the outbox event
// of a link change, using the work item history and audit conventions. The
// event append is last: it takes the event store's backlog and scope locks, and
// no work or project lock may follow it.
func (s *Store) recordMutation(ctx context.Context, tx pgx.Tx, c dependencies.Change) error {
	h := c.History
	if h.ID == "" || len(h.ID) > 120 {
		return fmt.Errorf("%w: history identifier is unusable", work.ErrInvalid)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_item_history
		(tenant_id, project_id, id, work_item_id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, reason,
		 changed_fields, from_state, to_state, before_values, after_values, review_subject, completion, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, $11, $12, $13, $14, $15, NULL, NULL, $16)`,
		string(h.Scope.TenantID), string(h.Scope.ProjectID), h.ID, string(h.WorkItemID), int64(h.Version), int64(h.ContentRevision),
		string(h.Kind), string(h.Actor.Type), h.Actor.ID, h.OccurredAt,
		h.ChangedFields, nilIfEmpty(string(h.From)), nilIfEmpty(string(h.To)), jsonb(h.Before), jsonb(h.After), nilIfEmpty(h.RequestID)); err != nil {
		return err
	}

	auditActor := string(h.Actor.Type)
	if h.Actor.Type == work.ActorService {
		auditActor = "system" // the audit table's name for the internal service actor
	}
	meta := map[string]any{
		"historyId": h.ID, "contentRevision": int64(h.ContentRevision), "changedFields": h.ChangedFields,
		"operation": string(c.Operation), "dependencyId": string(c.Link.PrerequisiteID),
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit
		(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'work_item', $8, $9, NULL, $10, $11)`,
		string(h.Scope.TenantID), "aud_"+h.ID, string(h.Scope.ProjectID), h.OccurredAt, auditActor, h.Actor.ID, eventsv1.TypeWorkItemUpdated,
		string(h.WorkItemID), int64(h.Version), nilIfEmpty(h.RequestID), jsonb(meta)); err != nil {
		return err
	}

	// The payload carries field names only, never the graph and never content.
	data, err := json.Marshal(eventsv1.WorkItemUpdated{ChangedFields: h.ChangedFields})
	if err != nil {
		return err
	}
	correlation := h.RequestID
	if correlation == "" {
		correlation = h.ID
	}
	_, err = s.events.Append(ctx, tx, eventsv1.Envelope{
		ID: "evt_" + h.ID, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeWorkItemUpdated, OccurredAt: h.OccurredAt,
		TenantID: h.Scope.TenantID, ProjectID: h.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "work_item", ID: string(h.WorkItemID), Version: int64(h.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(h.Actor.Type), ID: h.Actor.ID},
		CorrelationID: correlation, Data: data,
	})
	return err
}
