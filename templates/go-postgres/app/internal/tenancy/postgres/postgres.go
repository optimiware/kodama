// Package postgres adapts the tenancy ports to PostgreSQL.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.invalid/inventory/internal/dbtx"
	"example.invalid/inventory/internal/tenancy"
)

// Memberships reads current membership with the runtime role. The tenant
// context makes row-level security expose only the requested tenant.
type Memberships struct{ pool *pgxpool.Pool }

// NewMemberships requires a runtime pool.
func NewMemberships(pool *pgxpool.Pool) (*Memberships, error) {
	if pool == nil {
		return nil, errors.New("tenancy/postgres: a pool is required")
	}
	return &Memberships{pool: pool}, nil
}

// Role implements tenancy.Memberships.
func (m *Memberships) Role(ctx context.Context, tenant tenancy.TenantID, subject string) (tenancy.Role, bool, error) {
	var role string
	found := false
	err := dbtx.InTenant(ctx, m.pool, string(tenant), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "SELECT role FROM memberships WHERE tenant_id = $1 AND subject = $2", string(tenant), subject).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return "", false, err
	}
	return tenancy.Role(role), found, nil
}

// Member is one provisioned membership.
type Member struct {
	Subject string
	Role    tenancy.Role
}

// Provisioner writes tenants and memberships with the owner identity. It is
// used for explicit provisioning and test/preview seeding only; the API and
// worker processes never hold this authority.
type Provisioner struct{ pool *pgxpool.Pool }

// NewProvisioner requires an owner pool.
func NewProvisioner(pool *pgxpool.Pool) (*Provisioner, error) {
	if pool == nil {
		return nil, errors.New("tenancy/postgres: a pool is required")
	}
	return &Provisioner{pool: pool}, nil
}

// Seed creates the tenant if needed and sets exactly the listed members' roles.
// It does not remove other members.
func (p *Provisioner) Seed(ctx context.Context, tenant tenancy.TenantID, name string, members []Member) error {
	if _, err := tenancy.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	return dbtx.InTenant(ctx, p.pool, string(tenant), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name", string(tenant), name); err != nil {
			return err
		}
		for _, m := range members {
			if !m.Role.Valid() {
				return fmt.Errorf("%w: role %q", tenancy.ErrInvalid, m.Role)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, subject, role) VALUES ($1, $2, $3)
				ON CONFLICT (tenant_id, subject) DO UPDATE SET role = EXCLUDED.role`, string(tenant), m.Subject, string(m.Role)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Remove deletes one membership; the member loses access with their next request.
func (p *Provisioner) Remove(ctx context.Context, tenant tenancy.TenantID, subject string) error {
	return dbtx.InTenant(ctx, p.pool, string(tenant), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM memberships WHERE tenant_id = $1 AND subject = $2", string(tenant), subject)
		return err
	})
}
