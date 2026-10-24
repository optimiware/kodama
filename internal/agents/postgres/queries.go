package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
)

const profileColumns = `id, name, version, digest, body, created_by_type, created_by_id, created_at`

// scanProfile reads profileColumns into a verified ProfileVersion. Stored
// content that does not match its digest, name or canonical form is reported as
// an internal failure, never returned.
func scanProfile(row pgx.Row, scope domain.Scope) (agents.ProfileVersion, error) {
	var (
		id, name, dig, body, byType, byID string
		version                           int64
		createdAt                         time.Time
	)
	if err := row.Scan(&id, &name, &version, &dig, &body, &byType, &byID, &createdAt); err != nil {
		return agents.ProfileVersion{}, err
	}
	content, err := agents.ContentFromCanonical([]byte(body), agents.Digest(dig))
	if err != nil {
		return agents.ProfileVersion{}, fmt.Errorf("agents/postgres: stored profile %q is corrupt: %v", id, err)
	}
	p := agents.ProfileVersion{
		Scope: scope, ID: agents.ProfileID(id), Name: name, Version: domain.Version(version), Digest: agents.Digest(dig),
		Content: content, CreatedAt: createdAt.UTC(), CreatedBy: agents.Actor{ID: byID, Type: agents.ActorType(byType)},
	}
	if err := p.Validate(); err != nil {
		return agents.ProfileVersion{}, fmt.Errorf("agents/postgres: stored profile %q is inconsistent: %v", id, err)
	}
	return p, nil
}

// Get implements agents.Repository: one exact (name, version) of the scope.
func (s *Store) Get(ctx context.Context, scope domain.Scope, name string, version domain.Version) (agents.ProfileVersion, error) {
	var out agents.ProfileVersion
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM agent_profiles
			WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND version = $4`,
			string(scope.TenantID), string(scope.ProjectID), name, int64(version)), scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		out = p
		return err
	})
	return out, err
}

// byDigest reads the version of identical content, if any.
func byDigest(ctx context.Context, tx pgx.Tx, scope domain.Scope, digest agents.Digest) (agents.ProfileVersion, bool, error) {
	p, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM agent_profiles
		WHERE tenant_id = $1 AND project_id = $2 AND digest = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(digest)), scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return agents.ProfileVersion{}, false, nil
	}
	return p, err == nil, err
}

// List implements agents.Repository. Ordering is (created_at, id); the ID is the
// unique tie-breaker, so a position identifies exactly one row.
func (s *Store) List(ctx context.Context, scope domain.Scope, q agents.ListQuery) ([]agents.ProfileVersion, error) {
	if q.Limit < 1 || q.Limit > agents.MaxPageRows {
		return nil, fmt.Errorf("%w: invalid page size", agents.ErrInvalid)
	}
	var out []agents.ProfileVersion
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var after *time.Time
		var afterID *string
		if q.After != nil {
			t, id := q.After.CreatedAt.UTC(), string(q.After.ID)
			after, afterID = &t, &id
		}
		var name *string
		if q.Name != "" {
			name = &q.Name
		}
		rows, err := tx.Query(ctx, `SELECT `+profileColumns+` FROM agent_profiles
			WHERE tenant_id = $1 AND project_id = $2
			  AND ($3::text IS NULL OR name = $3)
			  AND ($4::timestamptz IS NULL OR (created_at, id) > ($4, $5))
			ORDER BY created_at, id LIMIT $6`,
			string(scope.TenantID), string(scope.ProjectID), name, after, afterID, q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// sameContent compares a stored version with a candidate's canonical bytes.
func sameContent(p agents.ProfileVersion, c agents.Candidate) bool {
	return p.Digest == c.Content.Digest() && bytes.Equal(p.Content.Canonical(), c.Content.Canonical())
}
