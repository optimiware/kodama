package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
)

// Querier is satisfied by *Pool, *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Membership is one fixed-role binding of a user. ProjectID is empty for a
// tenant-wide binding.
type Membership struct {
	UserID    string
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	Role      string
}

// DiscoverMemberships returns the bindings of the one user mapped to
// (issuer, subject). It is the only path that reads memberships outside a
// tenant transaction and is filtered by the authenticated principal; callers
// must pass the verified issuer and subject, never request-supplied tenant
// identifiers. An unknown principal yields no rows.
func DiscoverMemberships(ctx context.Context, q Querier, issuer, subject string) ([]Membership, error) {
	if issuer == "" || subject == "" {
		return nil, fmt.Errorf("storage: %w: issuer and subject are required", domain.ErrInvalidValue)
	}
	rows, err := q.Query(ctx, "SELECT user_id, tenant_id, project_id, role FROM kodama_discover_memberships($1, $2)", issuer, subject)
	if err != nil {
		return nil, fmt.Errorf("storage: discover memberships: %w", err)
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		var tenant string
		var project *string
		if err := rows.Scan(&m.UserID, &tenant, &project, &m.Role); err != nil {
			return nil, fmt.Errorf("storage: scan membership: %w", err)
		}
		m.TenantID = domain.TenantID(tenant)
		if project != nil {
			m.ProjectID = domain.ProjectID(*project)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: discover memberships: %w", err)
	}
	return out, nil
}

// ListTenantIDs enumerates tenant identifiers for workers, which then enter
// ordinary WithTenant transactions. It exposes identifiers only.
func ListTenantIDs(ctx context.Context, q Querier) ([]domain.TenantID, error) {
	rows, err := q.Query(ctx, "SELECT kodama_list_tenant_ids()")
	if err != nil {
		return nil, fmt.Errorf("storage: list tenant ids: %w", err)
	}
	defer rows.Close()
	var out []domain.TenantID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("storage: scan tenant id: %w", err)
		}
		out = append(out, domain.TenantID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list tenant ids: %w", err)
	}
	return out, nil
}
