package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

// IssuedGrants implements runtimeauth.IssuedGrants over table runtime_git_grants
// (migration 014_attempt_git.sql): the append-only record of the runtime Git
// grants (seed, integration) issued to attempts. It holds claims only, never a
// credential.
type IssuedGrants struct {
	db    DB
	clock func() time.Time
}

var _ runtimeauth.IssuedGrants = (*IssuedGrants)(nil)

// NewIssuedGrants requires the database; the clock defaults to time.Now.
func NewIssuedGrants(db DB, clock func() time.Time) (*IssuedGrants, error) {
	if db == nil {
		return nil, errors.New("runtimeauth/postgres: a database is required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &IssuedGrants{db: db, clock: clock}, nil
}

// RecordIssued implements runtimeauth.IssuedGrants: idempotent by grant ID.
func (s *IssuedGrants) RecordIssued(ctx context.Context, g runtimeauth.IssuedGrant) error {
	if err := g.Validate(); err != nil {
		return err
	}
	doc, err := json.Marshal(g.Claims)
	if err != nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, err)
	}
	return storage.WithTenant(ctx, s.db, g.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO runtime_git_grants
			(tenant_id, grant_id, project_id, run_id, attempt_id, kind, repository_id, claims, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, grant_id) DO NOTHING`,
			string(g.Scope.TenantID), g.Claims.GrantID, string(g.Scope.ProjectID), string(g.RunID), string(g.AttemptID),
			string(g.Claims.Kind), g.Claims.Repositories[0].ID, doc, s.clock().UTC())
		switch {
		case isPg(err, "23503"):
			return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("unknown project"))
		case isPg(err, "23514"):
			return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("the record violates its constraints"))
		case err != nil:
			return fmt.Errorf("runtimeauth/postgres: record issued grant: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var same bool
		if err := tx.QueryRow(ctx, `SELECT project_id = $3 AND run_id = $4 AND attempt_id = $5 AND claims = $6::jsonb
			FROM runtime_git_grants WHERE tenant_id = $1 AND grant_id = $2`,
			string(g.Scope.TenantID), g.Claims.GrantID, string(g.Scope.ProjectID), string(g.RunID), string(g.AttemptID), doc).Scan(&same); err != nil {
			return fmt.Errorf("runtimeauth/postgres: read issued grant: %w", err)
		}
		if !same {
			return errors.Join(runtimeauth.ErrConflict, errors.New("a different grant is recorded under this ID"))
		}
		return nil
	})
}

// IssuedFor implements runtimeauth.IssuedGrants.
func (s *IssuedGrants) IssuedFor(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error) {
	if err := scope.Validate(); err != nil {
		return nil, errors.Join(runtimeauth.ErrInvalidRequest, err)
	}
	if err := attempt.Validate(); err != nil {
		return nil, errors.Join(runtimeauth.ErrInvalidRequest, err)
	}
	out := []gitv1.GrantClaims{}
	err := storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, `SELECT claims FROM runtime_git_grants
			WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 ORDER BY grant_id`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt))
		if err != nil {
			return fmt.Errorf("runtimeauth/postgres: read issued grants: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var doc []byte
			if err := rows.Scan(&doc); err != nil {
				return fmt.Errorf("runtimeauth/postgres: read issued grants: %w", err)
			}
			c, err := gitv1.DecodeResponse[gitv1.GrantClaims](doc)
			if err != nil {
				return errors.Join(runtimeauth.ErrUnavailable, fmt.Errorf("a recorded grant is unreadable: %w", err))
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
