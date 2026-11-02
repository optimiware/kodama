// Package postgres implements project-scoped task transactions over pgx.
package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/tasks"
)

type Store struct{ DB storage.TxBeginner }

var _ tasks.Store = Store{}

func (s Store) Update(ctx context.Context, scope domain.Scope, fn func(tasks.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return storage.WithTenant(ctx, s.DB, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var raw []byte
		// One small project lock serializes workflow edits and related task
		// mutations, including park/resolve, without a workflow lock framework.
		err := tx.QueryRow(ctx, `SELECT task_workflow::jsonb FROM projects WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, scope.TenantID, scope.ProjectID).Scan(&raw)
		if err != nil {
			return translate(err)
		}
		var workflow tasks.Workflow
		if err := json.Unmarshal(raw, &workflow); err != nil {
			return err
		}
		if err := workflow.Validate(); err != nil {
			return err
		}
		return fn(transaction{ctx, tx, scope, workflow})
	})
}

type transaction struct {
	ctx      context.Context
	tx       pgx.Tx
	scope    domain.Scope
	workflow tasks.Workflow
}

func (t transaction) Workflow() tasks.Workflow { return t.workflow.Clone() }
func (t transaction) Get(id domain.TaskID) (tasks.Task, error) {
	row := tasks.Task{Scope: t.scope, ID: id}
	var resolution []byte
	err := t.tx.QueryRow(t.ctx, `SELECT coalesce(parent_id,''), coalesce(blocked_by_id,''), kind, title, body, state,
        parked_at, coalesce(resume_state,''), coalesce(assigned_role,''), coalesce(assigned_user_id,''),
        coalesce(repo_id,''), coalesce(branch,''), context::jsonb, resolution::jsonb, resolved_at, version, created_at, updated_at
        FROM tasks WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, t.scope.TenantID, t.scope.ProjectID, id).
		Scan(&row.ParentID, &row.BlockedByID, &row.Kind, &row.Title, &row.Body, &row.State, &row.ParkedAt,
			&row.ResumeState, &row.AssignedRole, &row.AssignedUserID, &row.RepositoryID, &row.Branch, &row.Context,
			&resolution, &row.ResolvedAt, &row.Version, &row.CreatedAt, &row.UpdatedAt)
	if err == nil && resolution != nil {
		err = json.Unmarshal(resolution, &row.Resolution)
	}
	return row, translate(err)
}
func (t transaction) GetByBlocker(request domain.TaskID) (tasks.Task, error) {
	row := tasks.Task{Scope: t.scope}
	var resolution []byte
	err := t.tx.QueryRow(t.ctx, `SELECT id,coalesce(parent_id,''),coalesce(blocked_by_id,''),kind,title,body,state,
        parked_at,coalesce(resume_state,''),coalesce(assigned_role,''),coalesce(assigned_user_id,''),
        coalesce(repo_id,''),coalesce(branch,''),context::jsonb,resolution::jsonb,resolved_at,version,created_at,updated_at
        FROM tasks WHERE tenant_id=$1 AND project_id=$2 AND blocked_by_id=$3`,
		t.scope.TenantID, t.scope.ProjectID, request).
		Scan(&row.ID, &row.ParentID, &row.BlockedByID, &row.Kind, &row.Title, &row.Body, &row.State, &row.ParkedAt,
			&row.ResumeState, &row.AssignedRole, &row.AssignedUserID, &row.RepositoryID, &row.Branch, &row.Context,
			&resolution, &row.ResolvedAt, &row.Version, &row.CreatedAt, &row.UpdatedAt)
	if err == nil && resolution != nil {
		err = json.Unmarshal(resolution, &row.Resolution)
	}
	return row, translate(err)
}

func (t transaction) Save(next tasks.Task, expected domain.Version) error {
	var current *tasks.Task
	row, err := t.Get(next.ID)
	if err == nil {
		current = &row
	} else if !errors.Is(err, tasks.ErrNotFound) {
		return err
	}
	if err := tasks.CheckSave(next, current, expected, t.scope, t.workflow); err != nil {
		return err
	}
	if next.AssignedUserID != "" {
		var member bool
		err := t.tx.QueryRow(t.ctx, `SELECT EXISTS (SELECT 1 FROM memberships
            WHERE tenant_id=$1 AND user_id=$2 AND (project_id IS NULL OR project_id=$3))`,
			t.scope.TenantID, next.AssignedUserID, t.scope.ProjectID).Scan(&member)
		if err != nil {
			return err
		}
		if !member {
			return tasks.ErrNotFound
		}
	}
	args := []any{t.scope.TenantID, t.scope.ProjectID, next.ID, next.ParentID, next.BlockedByID, next.Kind,
		next.Title, next.Body, next.State, next.ParkedAt, next.ResumeState, next.AssignedRole, next.AssignedUserID,
		next.RepositoryID, next.Branch, next.Context, next.Resolution, next.ResolvedAt, next.Version, next.CreatedAt, next.UpdatedAt}
	if expected == 0 {
		_, err = t.tx.Exec(t.ctx, `INSERT INTO tasks
            (tenant_id,project_id,id,parent_id,blocked_by_id,kind,title,body,state,parked_at,resume_state,
             assigned_role,assigned_user_id,repo_id,branch,context,resolution,resolved_at,version,created_at,updated_at)
            VALUES ($1,$2,$3,nullif($4,''),nullif($5,''),$6,$7,$8,$9,$10,nullif($11,''),nullif($12,''),
                    nullif($13,''),nullif($14,''),nullif($15,''),$16::jsonb,$17::jsonb,$18,$19,$20,$21)`, args...)
	} else {
		// Kind and creation time are immutable but stay in the argument list.
		var result pgconn.CommandTag
		result, err = t.tx.Exec(t.ctx, `UPDATE tasks SET parent_id=nullif($4,''),blocked_by_id=nullif($5,''),
            title=$7,body=$8,state=$9,parked_at=$10,resume_state=nullif($11,''),assigned_role=nullif($12,''),
            assigned_user_id=nullif($13,''),repo_id=nullif($14,''),branch=nullif($15,''),context=$16::jsonb,resolution=$17::jsonb,
            resolved_at=$18,version=$19,updated_at=$21
            WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND kind=$6 AND created_at=$20 AND version=$22`, append(args, expected)...)
		if err == nil && result.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	return translate(err)
}
func (t transaction) Append(event tasks.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	_, err := t.tx.Exec(t.ctx, `INSERT INTO audit_events
        (tenant_id,project_id,id,actor_type,actor_id,event_type,resource_type,resource_id,payload,created_at)
        VALUES ($1,$2,$3,$4,$5,$6,'task',$7,$8::jsonb,$9)`, t.scope.TenantID, t.scope.ProjectID, event.ID,
		event.ActorType, event.ActorID, event.Type, event.TaskID, event.Payload, event.CreatedAt)
	return translate(err)
}

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return tasks.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			return tasks.ErrNotFound
		case "23514", "23502":
			return domain.ErrInvalidValue
		}
	}
	return err
}
