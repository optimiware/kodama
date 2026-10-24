package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

const itemColumns = `kind, title, spec, acceptance_criteria, evidence_links, assignee_id, parent_id, state,
	blocked_reason, version, content_revision, review_subject, completion,
	created_by_type, created_by_id, created_at, updated_at`

// scanItem reads itemColumns, with id and scope supplied by the caller.
func scanItem(row pgx.Row, scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	var (
		it                                  = work.Item{Scope: scope, ID: id}
		kind, state, createdByType          string
		criteria, links, review, completion []byte
		assignee, parent, blocked           *string
		version, revision                   int64
	)
	if err := row.Scan(&kind, &it.Title, &it.Spec, &criteria, &links, &assignee, &parent, &state, &blocked,
		&version, &revision, &review, &completion, &createdByType, &it.CreatedBy.ID, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return work.Item{}, err
	}
	it.Kind, it.State = work.Kind(kind), work.State(state)
	it.CreatedBy.Type = work.ActorType(createdByType)
	it.Version, it.ContentRevision = domain.Version(version), domain.ContentRevision(revision)
	if assignee != nil {
		it.AssigneeID = *assignee
	}
	if parent != nil {
		it.ParentID = domain.WorkItemID(*parent)
	}
	if blocked != nil {
		it.BlockedReason = *blocked
	}
	it.CreatedAt, it.UpdatedAt = it.CreatedAt.UTC(), it.UpdatedAt.UTC()
	if err := json.Unmarshal(criteria, &it.Criteria); err != nil {
		return work.Item{}, fmt.Errorf("work/postgres: stored acceptance criteria: %w", err)
	}
	if err := json.Unmarshal(links, &it.EvidenceLinks); err != nil {
		return work.Item{}, fmt.Errorf("work/postgres: stored evidence links: %w", err)
	}
	if it.Criteria == nil {
		it.Criteria = []work.Criterion{}
	}
	if it.EvidenceLinks == nil {
		it.EvidenceLinks = []work.EvidenceRef{}
	}
	if review != nil {
		it.ReviewSubject = new(work.ReviewSubject)
		if err := json.Unmarshal(review, it.ReviewSubject); err != nil {
			return work.Item{}, fmt.Errorf("work/postgres: stored review subject: %w", err)
		}
	}
	if completion != nil {
		it.Completion = new(work.CompletionRecord)
		if err := json.Unmarshal(completion, it.Completion); err != nil {
			return work.Item{}, fmt.Errorf("work/postgres: stored completion: %w", err)
		}
	}
	return it, nil
}

func notFound() error { return fmt.Errorf("%w: the work item was not found", work.ErrNotFound) }

// Get implements work.Repository.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	var out work.Item
	err := s.inTenant(ctx, tenantOf(scope), func(ctx context.Context, tx pgx.Tx) error {
		it, err := selectItem(ctx, tx, scope, id, "")
		if err != nil {
			return err
		}
		out = it
		return nil
	})
	return out, err
}

// selectItem reads one item in scope; lock is "" or a row-lock clause.
func selectItem(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.WorkItemID, lock string) (work.Item, error) {
	row := tx.QueryRow(ctx, `SELECT `+itemColumns+` FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3 `+lock,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	it, err := scanItem(row, scope, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return work.Item{}, notFound()
	}
	return it, err
}

// List implements work.Repository. Ordering is (created_at, id); the ID is the
// unique tie-breaker, so a position identifies exactly one row.
func (s *Store) List(ctx context.Context, scope domain.Scope, q work.ListQuery) ([]work.Item, error) {
	var out []work.Item
	err := s.inTenant(ctx, tenantOf(scope), func(ctx context.Context, tx pgx.Tx) error {
		args := []any{string(scope.TenantID), string(scope.ProjectID)}
		var where []string
		add := func(cond string, v any) {
			args = append(args, v)
			where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
		}
		if len(q.States) > 0 {
			ss := make([]string, len(q.States))
			for i, st := range q.States {
				ss[i] = string(st)
			}
			add("state = ANY(?)", ss)
		}
		if len(q.Kinds) > 0 {
			ks := make([]string, len(q.Kinds))
			for i, k := range q.Kinds {
				ks[i] = string(k)
			}
			add("kind = ANY(?)", ks)
		}
		if q.AssigneeID != "" {
			add("assignee_id = ?", q.AssigneeID)
		}
		if q.ParentID != "" {
			add("parent_id = ?", string(q.ParentID))
		}
		if q.Blocked != nil {
			add("(blocked_reason IS NOT NULL) = ?", *q.Blocked)
		}
		if q.After != nil {
			args = append(args, micros(q.After.CreatedAt), string(q.After.ID))
			where = append(where, fmt.Sprintf("(created_at, id) > ($%d, $%d)", len(args)-1, len(args)))
		}
		args = append(args, q.Limit)
		sql := `SELECT id, ` + itemColumns + ` FROM work_items WHERE tenant_id = $1 AND project_id = $2`
		for _, w := range where {
			sql += " AND " + w
		}
		sql += fmt.Sprintf(" ORDER BY created_at, id LIMIT $%d", len(args))
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			// listRow reads the leading id column, then scanItem the rest.
			it, err := scanItem(listRow{rows: rows, id: &id}, scope, "")
			if err != nil {
				return err
			}
			it.ID = domain.WorkItemID(id)
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// listRow lets scanItem read the id column first and then fix up the item.
type listRow struct {
	rows pgx.Rows
	id   *string
}

func (l listRow) Scan(dest ...any) error {
	return l.rows.Scan(append([]any{l.id}, dest...)...)
}

// History implements work.Repository.
func (s *Store) History(ctx context.Context, scope domain.Scope, id domain.WorkItemID, q work.HistoryQuery) ([]work.HistoryEntry, error) {
	var out []work.HistoryEntry
	err := s.inTenant(ctx, tenantOf(scope), func(ctx context.Context, tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		} else if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, reason,
				changed_fields, from_state, to_state, before_values, after_values, review_subject, completion, request_id
			FROM work_item_history
			WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3 AND version > $4
			ORDER BY version LIMIT $5`,
			string(scope.TenantID), string(scope.ProjectID), string(id), int64(q.AfterVersion), q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			h, err := scanHistory(rows, scope, id)
			if err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

func scanHistory(row pgx.Row, scope domain.Scope, id domain.WorkItemID) (work.HistoryEntry, error) {
	var (
		h                                 = work.HistoryEntry{Scope: scope, WorkItemID: id}
		version, revision                 int64
		kind, actorType                   string
		reason, from, to, requestID       *string
		before, after, review, completion []byte
		occurred                          time.Time
	)
	if err := row.Scan(&h.ID, &version, &revision, &kind, &actorType, &h.Actor.ID, &occurred, &reason,
		&h.ChangedFields, &from, &to, &before, &after, &review, &completion, &requestID); err != nil {
		return work.HistoryEntry{}, err
	}
	h.Version, h.ContentRevision = domain.Version(version), domain.ContentRevision(revision)
	h.Kind, h.Actor.Type, h.OccurredAt = work.ChangeKind(kind), work.ActorType(actorType), occurred.UTC()
	if reason != nil {
		h.Reason = *reason
	}
	if from != nil {
		h.From = work.State(*from)
	}
	if to != nil {
		h.To = work.State(*to)
	}
	if requestID != nil {
		h.RequestID = *requestID
	}
	if h.ChangedFields == nil {
		h.ChangedFields = []string{}
	}
	if err := json.Unmarshal(before, &h.Before); err != nil {
		return work.HistoryEntry{}, fmt.Errorf("work/postgres: stored history values: %w", err)
	}
	if err := json.Unmarshal(after, &h.After); err != nil {
		return work.HistoryEntry{}, fmt.Errorf("work/postgres: stored history values: %w", err)
	}
	if review != nil {
		h.Review = new(work.ReviewSubject)
		if err := json.Unmarshal(review, h.Review); err != nil {
			return work.HistoryEntry{}, err
		}
	}
	if completion != nil {
		h.Completion = new(work.CompletionRecord)
		if err := json.Unmarshal(completion, h.Completion); err != nil {
			return work.HistoryEntry{}, err
		}
	}
	return h, nil
}
