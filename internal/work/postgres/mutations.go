package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// Insert implements work.Repository. The new item, its history entry, the audit
// row and the event commit in one transaction.
func (s *Store) Insert(ctx context.Context, scope domain.Scope, fn work.InsertFunc) (work.Item, error) {
	var out work.Item
	committedFn := false
	err := s.inTenant(ctx, tenantOf(scope), func(ctx context.Context, tx pgx.Tx) error {
		env := s.newTxEnv(tx, scope, false)
		it, h, err := fn(ctx, env)
		if err != nil {
			return err
		}
		if it.Scope != scope || h.Scope != scope {
			return fmt.Errorf("%w: the mutation does not belong to the transaction's scope", work.ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_items
			(tenant_id, project_id, id, kind, title, spec, acceptance_criteria, evidence_links, assignee_id, parent_id, state,
			 blocked_reason, version, content_revision, review_subject, completion, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
			itemArgs(it)...); err != nil {
			return err
		}
		if err := s.recordMutation(ctx, tx, it, h); err != nil {
			return err
		}
		out = it
		committedFn = true
		return nil
	})
	return out, uncertain(err, committedFn)
}

// Update implements work.Repository.
//
// Order inside the transaction: project lock (when requested), item row lock,
// expected-version comparison, the update function (which re-evaluates every
// prerequisite through guards bound to this transaction), the conditional
// UPDATE, history, audit and finally the event append, which takes the event
// store's own locks last.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id domain.WorkItemID, expected domain.Version, opts work.UpdateOptions, fn work.UpdateFunc) (work.Item, error) {
	var out work.Item
	committedFn := false
	err := s.inTenant(ctx, tenantOf(scope), func(ctx context.Context, tx pgx.Tx) error {
		if opts.LockProject {
			if err := lockProject(ctx, tx, scope); err != nil {
				return err
			}
		}
		cur, err := selectItem(ctx, tx, scope, id, "FOR UPDATE")
		if err != nil {
			return err
		}
		if cur.Version != expected {
			return fmt.Errorf("%w: the work item is at version %d", work.ErrVersionConflict, cur.Version)
		}
		next, h, err := fn(ctx, s.newTxEnv(tx, scope, opts.LockProject), cur)
		if errors.Is(err, work.ErrNoChange) {
			out = cur
			return nil
		}
		if err != nil {
			return err
		}
		if next.Scope != scope || next.ID != id || next.Kind != cur.Kind || next.Version != cur.Version+1 || h.Version != next.Version {
			return fmt.Errorf("%w: the mutation does not continue the locked item", work.ErrInvalid)
		}
		tag, err := tx.Exec(ctx, `UPDATE work_items SET title = $4, spec = $5, acceptance_criteria = $6, evidence_links = $7,
				assignee_id = $8, parent_id = $9, state = $10, blocked_reason = $11, version = $12, content_revision = $13,
				review_subject = $14, completion = $15, updated_at = $16
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $17`,
			updateArgs(next, expected)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			// Unreachable while the row lock is held; the predicate is the guard
			// against any path that bypassed it.
			return fmt.Errorf("%w: the work item changed concurrently", work.ErrVersionConflict)
		}
		if err := s.recordMutation(ctx, tx, next, h); err != nil {
			return err
		}
		out = next
		committedFn = true
		return nil
	})
	return out, uncertain(err, committedFn)
}

// uncertain converts a failure after the mutation function completed into an
// unknown-outcome error: the commit may or may not have happened, so the caller
// rereads and nothing is replayed automatically. A failure before that point
// rolled back and is reported as classified.
func uncertain(err error, ranToCommit bool) error {
	if err != nil && ranToCommit {
		// The update function and every statement succeeded, so the failure is
		// the commit's: it may or may not have been applied.
		return work.Unavailablef(err, "the outcome of the write is unknown; reread the work item before retrying")
	}
	return err
}

// lockProject takes the project row lock. FOR NO KEY UPDATE serializes
// transitions and parent changes of one project without blocking the foreign-key
// checks of concurrent inserts.
func lockProject(ctx context.Context, tx pgx.Tx, scope domain.Scope) error {
	var limit int
	err := tx.QueryRow(ctx, `SELECT active_wip_limit FROM projects WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE`,
		string(scope.TenantID), string(scope.ProjectID)).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the project was not found", work.ErrNotFound)
	}
	return err
}

func jsonb(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("work/postgres: marshal %T: %v", v, err)) // plain structs; cannot fail
	}
	return b
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonbOrNil(isNil bool, v any) any {
	if isNil {
		return nil
	}
	return jsonb(v)
}

func itemArgs(it work.Item) []any {
	return []any{
		string(it.Scope.TenantID), string(it.Scope.ProjectID), string(it.ID), string(it.Kind), it.Title, it.Spec,
		jsonb(it.Criteria), jsonb(it.EvidenceLinks), nilIfEmpty(it.AssigneeID), nilIfEmpty(string(it.ParentID)), string(it.State),
		nilIfEmpty(it.BlockedReason), int64(it.Version), int64(it.ContentRevision),
		jsonbOrNil(it.ReviewSubject == nil, it.ReviewSubject), jsonbOrNil(it.Completion == nil, it.Completion),
		string(it.CreatedBy.Type), it.CreatedBy.ID, it.CreatedAt, it.UpdatedAt,
	}
}

func updateArgs(it work.Item, expected domain.Version) []any {
	return []any{
		string(it.Scope.TenantID), string(it.Scope.ProjectID), string(it.ID),
		it.Title, it.Spec, jsonb(it.Criteria), jsonb(it.EvidenceLinks), nilIfEmpty(it.AssigneeID), nilIfEmpty(string(it.ParentID)),
		string(it.State), nilIfEmpty(it.BlockedReason), int64(it.Version), int64(it.ContentRevision),
		jsonbOrNil(it.ReviewSubject == nil, it.ReviewSubject), jsonbOrNil(it.Completion == nil, it.Completion),
		it.UpdatedAt, int64(expected),
	}
}

// recordMutation writes the history entry, the audit row and the outbox event.
// The event append is last: it takes the event store's backlog and scope locks,
// and no work or project lock may follow it.
func (s *Store) recordMutation(ctx context.Context, tx pgx.Tx, it work.Item, h work.HistoryEntry) error {
	if h.ID == "" || len(h.ID) > 120 {
		return fmt.Errorf("%w: history identifier is unusable", work.ErrInvalid)
	}
	var from, to *string
	if h.From != "" {
		from = nilIfEmpty(string(h.From))
	}
	if h.To != "" {
		to = nilIfEmpty(string(h.To))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_item_history
		(tenant_id, project_id, id, work_item_id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, reason,
		 changed_fields, from_state, to_state, before_values, after_values, review_subject, completion, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		string(h.Scope.TenantID), string(h.Scope.ProjectID), h.ID, string(h.WorkItemID), int64(h.Version), int64(h.ContentRevision),
		string(h.Kind), string(h.Actor.Type), h.Actor.ID, h.OccurredAt, nilIfEmpty(h.Reason),
		nonNilStrings(h.ChangedFields), from, to, jsonb(nonNilMap(h.Before)), jsonb(nonNilMap(h.After)),
		jsonbOrNil(h.Review == nil, h.Review), jsonbOrNil(h.Completion == nil, h.Completion), nilIfEmpty(h.RequestID)); err != nil {
		return err
	}

	action, evType, data, err := eventFor(h)
	if err != nil {
		return err
	}
	auditActor := string(h.Actor.Type)
	if h.Actor.Type == work.ActorService {
		auditActor = "system" // the audit table's name for the internal service actor
	}
	meta := map[string]any{"historyId": h.ID, "contentRevision": int64(h.ContentRevision)}
	if len(h.ChangedFields) > 0 {
		meta["changedFields"] = h.ChangedFields
	}
	if h.Kind == work.ChangeTransitioned {
		meta["from"], meta["to"] = string(h.From), string(h.To)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit
		(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'work_item', $8, $9, $10, $11, $12)`,
		string(h.Scope.TenantID), "aud_"+h.ID, string(h.Scope.ProjectID), h.OccurredAt, auditActor, h.Actor.ID, action,
		string(h.WorkItemID), int64(h.Version), nilIfEmpty(h.Reason), nilIfEmpty(h.RequestID), jsonb(meta)); err != nil {
		return err
	}

	correlation := h.RequestID
	if correlation == "" {
		correlation = h.ID
	}
	_, err = s.events.Append(ctx, tx, eventsv1.Envelope{
		ID: "evt_" + h.ID, SchemaVersion: eventsv1.SchemaVersion, Type: evType, OccurredAt: h.OccurredAt,
		TenantID: h.Scope.TenantID, ProjectID: h.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "work_item", ID: string(h.WorkItemID), Version: int64(h.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(h.Actor.Type), ID: h.Actor.ID},
		CorrelationID: correlation, Data: data,
	})
	return err
}

// eventFor builds the catalog event of a history entry. Payloads carry
// references and field names only, never specification content.
func eventFor(h work.HistoryEntry) (action, eventType string, data json.RawMessage, err error) {
	switch h.Kind {
	case work.ChangeCreated:
		data, err = json.Marshal(eventsv1.WorkItemCreated{Kind: kindFromAfter(h), State: string(h.To)})
		return "work_item.created", eventsv1.TypeWorkItemCreated, data, err
	case work.ChangeUpdated:
		p := eventsv1.WorkItemUpdated{ChangedFields: h.ChangedFields}
		if h.ContentChanged() {
			rev := int64(h.ContentRevision)
			p.ContentRevision = &rev
		}
		data, err = json.Marshal(p)
		return "work_item.updated", eventsv1.TypeWorkItemUpdated, data, err
	case work.ChangeTransitioned:
		data, err = json.Marshal(eventsv1.WorkItemTransitioned{From: string(h.From), To: string(h.To), TransitionID: h.ID})
		return "work_item.transitioned", eventsv1.TypeWorkItemTransitioned, data, err
	}
	return "", "", nil, fmt.Errorf("%w: unknown change kind %q", work.ErrInvalid, h.Kind)
}

func kindFromAfter(h work.HistoryEntry) string {
	if k, ok := h.After[work.FieldKind].(string); ok {
		return k
	}
	return ""
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
