package serve

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// Seed creates (idempotently) one tenant, one principal and one tenant-wide
// admin binding. It runs with the owner database identity and is an operator
// command, never a request path.
func Seed(ctx context.Context, migrationURL, tenantKey, userID, issuer, subject, displayName string) (tenant domain.TenantID, err error) {
	if migrationURL == "" || tenantKey == "" || userID == "" || issuer == "" || subject == "" {
		return "", fmt.Errorf("kernel seed: migration URL, tenant key, user id, issuer and subject are required")
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
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id,id,user_id,role)
			VALUES ($1,$2,$3,'admin') ON CONFLICT (tenant_id,project_id,user_id,role) DO NOTHING`,
			tenant, "m_"+userID, userID)
		return err
	})
	if err != nil {
		return "", err
	}
	return tenant, nil
}
