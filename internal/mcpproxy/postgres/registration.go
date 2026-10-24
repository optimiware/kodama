package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

const serverColumns = `tenant_id, project_id, id, endpoint, secret_name, secret_version, tools, version, created_by, updated_by, created_at, updated_at, deleted_at`

func scanServer(row pgx.Row) (mcpproxy.Registration, error) {
	var (
		r                    mcpproxy.Registration
		tenant, project, id  string
		createdAt, updatedAt time.Time
		deletedAt            *time.Time
		version              int64
		createdBy, updatedBy string
	)
	if err := row.Scan(&tenant, &project, &id, &r.Endpoint, &r.Secret.Name, &r.Secret.Version, &r.Tools, &version, &createdBy, &updatedBy, &createdAt, &updatedAt, &deletedAt); err != nil {
		return mcpproxy.Registration{}, err
	}
	r.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	r.ID, r.Version = mcpproxy.RegistrationID(id), domain.Version(version)
	r.CreatedBy, r.UpdatedBy = mcpproxy.Actor{ID: createdBy}, mcpproxy.Actor{ID: updatedBy}
	r.CreatedAt, r.UpdatedAt = createdAt.UTC(), updatedAt.UTC()
	if deletedAt != nil {
		t := deletedAt.UTC()
		r.DeletedAt = &t
	}
	if r.Tools == nil {
		r.Tools = []string{}
	}
	return r, nil
}

func (s *Store) actorAudit(r mcpproxy.Registration, m mcpproxy.Mutation, action string) auditEntry {
	return auditEntry{Scope: r.Scope, ActorType: "user", ActorID: m.Actor.ID, Action: action, Resource: string(r.ID), Version: int64(r.Version),
		RequestID: m.RequestID, At: m.Now,
		Metadata: map[string]any{"endpoint": r.Endpoint, "secretName": r.Secret.Name, "secretVersion": r.Secret.Version, "tools": r.Tools}}
}

// CreateRegistration inserts version 1 and its audit entry in one transaction.
func (s *Store) CreateRegistration(ctx context.Context, m mcpproxy.Mutation, r mcpproxy.Registration) error {
	if err := r.Validate(); err != nil || r.Version != 1 || r.Deleted() {
		return fmt.Errorf("%w: the registration is not a new version 1", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, r.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_servers
			(tenant_id, project_id, id, endpoint, secret_name, secret_version, tools, version, created_by, updated_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, $8, $9, $9)`,
			string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), r.Endpoint, r.Secret.Name, r.Secret.Version, r.Tools, r.CreatedBy.ID, r.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, s.actorAudit(r, m, "mcp_server.created"))
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the registration commit outcome is unknown; read before retrying")
	}
	return classify(err)
}

// ReplaceRegistration applies an edit or tombstone with the expected version in
// the write itself and invalidates the sessions of the previous revision in the
// same transaction. Run bindings stay frozen at their old revision and are
// therefore unusable.
func (s *Store) ReplaceRegistration(ctx context.Context, m mcpproxy.Mutation, next mcpproxy.Registration, expected domain.Version) error {
	if err := next.Validate(); err != nil || next.Version != expected+1 {
		return fmt.Errorf("%w: the registration is not the successor of the expected version", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, next.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Lock first so a concurrent admission (FOR SHARE) and this write are
		// totally ordered.
		var cur int64
		var deleted *time.Time
		err := tx.QueryRow(ctx, `SELECT version, deleted_at FROM mcp_servers WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(next.Scope.TenantID), string(next.Scope.ProjectID), string(next.ID)).Scan(&cur, &deleted)
		if err != nil {
			return noRows(err, mcpproxy.ErrNotFound)
		}
		if deleted != nil {
			return mcpproxy.ErrNotFound
		}
		if domain.Version(cur) != expected {
			return mcpproxy.ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE mcp_servers SET endpoint = $4, secret_name = $5, secret_version = $6, tools = $7, version = $8,
			updated_by = $9, updated_at = $10, deleted_at = $11 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(next.Scope.TenantID), string(next.Scope.ProjectID), string(next.ID), next.Endpoint, next.Secret.Name, next.Secret.Version,
			next.Tools, int64(next.Version), next.UpdatedBy.ID, next.UpdatedAt, next.DeletedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE mcp_sessions SET state = 'invalid', updated_at = $4
			WHERE tenant_id = $1 AND project_id = $2 AND server_id = $3 AND state IN ('initializing', 'active')`,
			string(next.Scope.TenantID), string(next.Scope.ProjectID), string(next.ID), next.UpdatedAt); err != nil {
			return err
		}
		action := "mcp_server.updated"
		if next.Deleted() {
			action = "mcp_server.deleted"
		}
		return s.audit(ctx, tx, s.actorAudit(next, m, action))
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the registration commit outcome is unknown; read before retrying")
	}
	return classify(err)
}

// GetRegistration reads one registration, including a tombstone.
func (s *Store) GetRegistration(ctx context.Context, scope domain.Scope, id mcpproxy.RegistrationID) (mcpproxy.Registration, error) {
	var out mcpproxy.Registration
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` FROM mcp_servers WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if err != nil {
			return noRows(err, mcpproxy.ErrNotFound)
		}
		out = r
		return nil
	})
	return out, classify(err)
}

// ListRegistrations returns live registrations ordered by ID.
func (s *Store) ListRegistrations(ctx context.Context, scope domain.Scope, after mcpproxy.RegistrationID, limit int) ([]mcpproxy.Registration, error) {
	var out []mcpproxy.Registration
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+serverColumns+` FROM mcp_servers
			WHERE tenant_id = $1 AND project_id = $2 AND deleted_at IS NULL AND id > $3 ORDER BY id LIMIT $4`,
			string(scope.TenantID), string(scope.ProjectID), string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanServer(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, classify(err)
}

// CheckSecret verifies that the referenced secret is an MCP-kind, live secret
// of the same project with an exact, non-revoked version. It reads metadata
// only. It satisfies mcpproxy.SecretChecker.
func (s *Store) CheckSecret(ctx context.Context, scope domain.Scope, ref mcpproxy.SecretRef) error {
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		var deleted bool
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT s.kind, s.deleted_at IS NOT NULL, v.revoked_at FROM secrets s
			JOIN secret_versions v ON v.tenant_id = s.tenant_id AND v.project_id = s.project_id AND v.secret_id = s.id
			WHERE s.tenant_id = $1 AND s.project_id = $2 AND s.name = $3 AND v.version = $4`,
			string(scope.TenantID), string(scope.ProjectID), ref.Name, ref.Version).Scan(&kind, &deleted, &revoked)
		if err != nil {
			return noRows(err, mcpproxy.ErrCredential)
		}
		if kind != "mcp" || deleted || revoked != nil {
			return mcpproxy.ErrCredential
		}
		return nil
	})
	return classify(err)
}

// Check adapts CheckSecret to the port's method name.
func (s *Store) Check(ctx context.Context, scope domain.Scope, ref mcpproxy.SecretRef) error {
	return s.CheckSecret(ctx, scope, ref)
}
