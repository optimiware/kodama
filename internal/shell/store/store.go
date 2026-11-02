// Package store is the simplified kernel's PostgreSQL persistence: one small,
// tenant-scoped store over the ADR 0013 schema. It exposes explicit operations
// instead of a generic repository; every query runs inside storage.WithTenant.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// Errors shared with the HTTP layer.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = domain.ErrConflict
)

// Store is safe for concurrent use; the pool owns connections.
type Store struct{ DB *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{DB: db} }

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ErrConflict
		case "23503", "42501":
			return ErrNotFound
		case "23514", "23502", "22P02":
			return fmt.Errorf("%w: %s", domain.ErrInvalidValue, pg.Message)
		}
	}
	return err
}

func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(context.Context, pgx.Tx) error) error {
	return storage.WithTenant(ctx, s.DB, tenant, fn)
}

// ---- tenancy and principals -------------------------------------------------

type Project struct {
	TenantID    domain.TenantID  `json:"tenantId"`
	ID          domain.ProjectID `json:"id"`
	Key         string           `json:"key"`
	DisplayName string           `json:"displayName"`
	Workflow    json.RawMessage  `json:"taskWorkflow"`
	Settings    json.RawMessage  `json:"settings"`
	Version     domain.Version   `json:"version"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	if p.ID.Validate() != nil || p.TenantID.Validate() != nil {
		return Project{}, fmt.Errorf("%w: project id", domain.ErrInvalidValue)
	}
	err := s.inTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO projects (tenant_id,id,key,display_name,task_workflow,settings)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING version,created_at,updated_at`,
			p.TenantID, p.ID, p.Key, p.DisplayName, p.Workflow, p.Settings).
			Scan(&p.Version, &p.CreatedAt, &p.UpdatedAt)
	})
	return p, translate(err)
}

func (s *Store) GetProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (Project, error) {
	p := Project{TenantID: tenant, ID: project}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT key,display_name,task_workflow::jsonb,settings::jsonb,version,created_at,updated_at
			FROM projects WHERE tenant_id=$1 AND id=$2`, tenant, project).
			Scan(&p.Key, &p.DisplayName, &p.Workflow, &p.Settings, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	})
	return p, translate(err)
}

func (s *Store) ListProjects(ctx context.Context, tenant domain.TenantID) ([]Project, error) {
	var out []Project
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,key,display_name,task_workflow::jsonb,settings::jsonb,version,created_at,updated_at
			FROM projects WHERE tenant_id=$1 ORDER BY key,id`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p := Project{TenantID: tenant}
			if err := rows.Scan(&p.ID, &p.Key, &p.DisplayName, &p.Workflow, &p.Settings, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// UpdateProject replaces settings and/or workflow under the expected version.
func (s *Store) UpdateProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, expected domain.Version, workflow, settings json.RawMessage) (Project, error) {
	p := Project{TenantID: tenant, ID: project}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE projects SET
			task_workflow = coalesce($4::jsonb, task_workflow),
			settings      = coalesce($5::jsonb, settings),
			version = version + 1, updated_at = now()
			WHERE tenant_id=$1 AND id=$2 AND version=$3
			RETURNING key,display_name,task_workflow::jsonb,settings::jsonb,version,created_at,updated_at`,
			tenant, project, expected, workflow, settings).
			Scan(&p.Key, &p.DisplayName, &p.Workflow, &p.Settings, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	})
	return p, translate(err)
}

type Membership struct {
	ID        string           `json:"id"`
	TenantID  domain.TenantID  `json:"tenantId"`
	ProjectID domain.ProjectID `json:"projectId,omitempty"`
	UserID    string           `json:"userId"`
	Role      string           `json:"role"`
}

// Roles lists a user's roles for one tenant (tenant-wide and project bindings).
func (s *Store) Roles(ctx context.Context, tenant domain.TenantID, userID, project string) ([]string, error) {
	var roles []string
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT role FROM memberships
			WHERE tenant_id=$1 AND user_id=$2 AND (project_id IS NULL OR project_id=$3)
			ORDER BY role`, tenant, userID, nullIfEmpty(project))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			roles = append(roles, r)
		}
		return rows.Err()
	})
	return roles, translate(err)
}

func (s *Store) UpsertMembership(ctx context.Context, m Membership) error {
	return translate(s.inTenant(ctx, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id,id,project_id,user_id,role)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (tenant_id,project_id,user_id,role) DO NOTHING`,
			m.TenantID, m.ID, nullIfEmpty(string(m.ProjectID)), m.UserID, m.Role)
		return err
	}))
}

// UpsertUserByPrincipal maps an external identity to a stable user ID.
func (s *Store) UpsertUserByPrincipal(ctx context.Context, id, issuer, subject, displayName string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO users (id,issuer,subject,display_name) VALUES ($1,$2,$3,$4)
		ON CONFLICT (issuer,subject) DO UPDATE SET display_name = excluded.display_name`,
		id, issuer, subject, displayName)
	return translate(err)
}

func (s *Store) UserIDForPrincipal(ctx context.Context, issuer, subject string) (string, error) {
	var id string
	err := s.DB.QueryRow(ctx, `SELECT id FROM users WHERE issuer=$1 AND subject=$2`, issuer, subject).Scan(&id)
	return id, translate(err)
}

func (s *Store) TenantByKey(ctx context.Context, key string) (domain.TenantID, error) {
	var id string
	err := s.DB.QueryRow(ctx, `SELECT kodama_tenant_lookup($1)`, key).Scan(&id)
	return domain.TenantID(id), translate(err)
}

func (s *Store) TenantIDs(ctx context.Context) ([]domain.TenantID, error) {
	rows, err := s.DB.Query(ctx, `SELECT kodama_list_tenant_ids()`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []domain.TenantID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err)
		}
		out = append(out, domain.TenantID(id))
	}
	return out, rows.Err()
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
