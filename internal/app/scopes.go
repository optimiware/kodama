package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/storage"
)

// projectLister is how the cleanup scheduler enumerates the projects of the
// installation: the tenants first, then each tenant's projects in identity order
// inside that tenant's own transaction (so row level security applies and one
// tenant's listing can never name another's project).
type projectLister struct {
	tenants  execution.Tenants
	projects func(ctx context.Context, tenant domain.TenantID, after domain.ProjectID, limit int) ([]domain.ProjectID, error)
}

var _ deliverycleanup.Scopes = projectLister{}

// Scopes implements deliverycleanup.Scopes: up to limit project scopes ordered by
// (tenant, project) strictly after the cursor. A tenant that cannot be listed
// fails the call; the scheduler reports it and repeats on the next sweep, so a
// failure never skips a project silently.
func (p projectLister) Scopes(ctx context.Context, after *domain.Scope, limit int) ([]domain.Scope, error) {
	if limit < 1 {
		return nil, fmt.Errorf("app: a scope page needs a positive limit")
	}
	ids, err := p.tenants.TenantIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant enumeration: %w", err)
	}
	sorted := append([]domain.TenantID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var out []domain.Scope
	for _, t := range sorted {
		if after != nil && t < after.TenantID {
			continue
		}
		var cursor domain.ProjectID
		if after != nil && t == after.TenantID {
			cursor = after.ProjectID
		}
		projects, err := p.projects(ctx, t, cursor, limit-len(out))
		if err != nil {
			return nil, fmt.Errorf("projects of tenant %s: %w", t, err)
		}
		for _, id := range projects {
			s := domain.Scope{TenantID: t, ProjectID: id}
			if s.Validate() != nil {
				continue // a malformed identity is never swept
			}
			out = append(out, s)
		}
		if len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

// sqlProjects lists a tenant's project identities after a cursor.
func sqlProjects(db storage.TxBeginner) func(context.Context, domain.TenantID, domain.ProjectID, int) ([]domain.ProjectID, error) {
	return func(ctx context.Context, tenant domain.TenantID, after domain.ProjectID, limit int) ([]domain.ProjectID, error) {
		var out []domain.ProjectID
		err := storage.WithTenant(ctx, db, tenant, func(ctx context.Context, tx pgx.Tx) error {
			out = out[:0]
			rows, err := tx.Query(ctx, `SELECT id FROM projects WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT $3`, string(tenant), string(after), limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				out = append(out, domain.ProjectID(id))
			}
			return rows.Err()
		})
		return out, err
	}
}
