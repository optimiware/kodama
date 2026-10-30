package serve

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// SeedRoles validates the tenant-wide roles a seed binds. None is the
// administrator alone: the role gives no push authority, so a person who also
// pushes (and requests the seed credential of a repository) names engineer as
// well, explicitly.
func SeedRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		return []string{"admin"}, nil
	}
	known := map[string]bool{"admin": true, "engineer": true, "viewer": true, "reviewer": true, "analyst": true, "operator": true}
	var out []string
	seen := map[string]bool{}
	for _, role := range roles {
		if !known[role] {
			return nil, fmt.Errorf("kernel seed: %q is not a role (admin, engineer, viewer, reviewer, analyst, operator)", role)
		}
		if !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	return out, nil
}

// Seed creates (idempotently) one tenant, one principal and its tenant-wide
// role bindings (the administrator alone when roles is empty). It runs with the
// owner database identity and is an operator command, never a request path.
func Seed(ctx context.Context, migrationURL, tenantKey, userID, issuer, subject, displayName string, roles ...string) (tenant domain.TenantID, err error) {
	if migrationURL == "" || tenantKey == "" || userID == "" || issuer == "" || subject == "" {
		return "", fmt.Errorf("kernel seed: migration URL, tenant key, user id, issuer and subject are required")
	}
	roles, err = SeedRoles(roles)
	if err != nil {
		return "", err
	}
	pool, err := storage.Open(ctx, storage.Config{URL: migrationURL, ApplicationName: "kodama-kernel-seed"})
	if err != nil {
		return "", err
	}
	defer pool.Close()

	var id string
	err = pool.QueryRow(ctx, `SELECT kodama_tenant_lookup($1)`, tenantKey).Scan(&id)
	if err != nil {
		id = "t_" + tenantKey
		tenant = domain.TenantID(id)
	} else {
		tenant = domain.TenantID(id)
	}
	err = storage.WithTenant(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id,key,display_name) VALUES ($1,$2,$3)
			ON CONFLICT (id) DO NOTHING`, tenant, tenantKey, tenantKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id,issuer,subject,display_name) VALUES ($1,$2,$3,$4)
			ON CONFLICT (issuer,subject) DO UPDATE SET display_name=excluded.display_name`,
			userID, issuer, subject, displayName); err != nil {
			return err
		}
		for _, role := range roles {
			id := "m_" + userID
			if role != "admin" {
				id += "_" + role
			}
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id,id,user_id,role)
				VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id,project_id,user_id,role) DO NOTHING`,
				tenant, id, userID, role); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return tenant, nil
}
