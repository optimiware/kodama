package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

const definitionColumns = `id, name, version, digest, body, created_by_type, created_by_id, created_at`

// scanDefinition reads definitionColumns into a verified Definition. Stored
// content that does not match its digest, name or canonical form is reported as
// an internal failure, never returned.
func scanDefinition(row pgx.Row, scope domain.Scope) (pipeline.Definition, error) {
	var (
		id, name, dig, body, byType, byID string
		version                           int64
		createdAt                         time.Time
	)
	if err := row.Scan(&id, &name, &version, &dig, &body, &byType, &byID, &createdAt); err != nil {
		return pipeline.Definition{}, err
	}
	content, err := pipeline.NewValidatedFromCanonical([]byte(body), pipeline.Digest(dig))
	if err != nil {
		return pipeline.Definition{}, fmt.Errorf("pipeline/postgres: stored definition %q is corrupt: %v", id, err)
	}
	def := pipeline.Definition{
		Scope: scope, ID: domain.DefinitionID(id), Name: name, Version: domain.Version(version), Digest: pipeline.Digest(dig),
		Content: content, CreatedAt: createdAt.UTC(), CreatedBy: pipeline.Actor{ID: byID, Type: pipeline.ActorType(byType)},
	}
	if err := def.Validate(); err != nil {
		return pipeline.Definition{}, fmt.Errorf("pipeline/postgres: stored definition %q is inconsistent: %v", id, err)
	}
	return def, nil
}

// Get implements pipeline.Repository.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	var out pipeline.Definition
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+` FROM pipeline_definitions
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)), scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		out = d
		return err
	})
	return out, err
}

// byDigest reads the definition of identical content, if any.
func byDigest(ctx context.Context, tx pgx.Tx, scope domain.Scope, digest pipeline.Digest) (pipeline.Definition, bool, error) {
	d, err := scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+` FROM pipeline_definitions
		WHERE tenant_id = $1 AND project_id = $2 AND digest = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(digest)), scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.Definition{}, false, nil
	}
	return d, err == nil, err
}

// List implements pipeline.Repository. Ordering is (created_at, id); the ID is
// the unique tie-breaker, so a position identifies exactly one row.
func (s *Store) List(ctx context.Context, scope domain.Scope, q pipeline.ListQuery) ([]pipeline.Definition, error) {
	if q.Limit < 1 || q.Limit > pipeline.MaxPageRows {
		return nil, fmt.Errorf("%w: invalid page size", pipeline.ErrInvalid)
	}
	var out []pipeline.Definition
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
		rows, err := tx.Query(ctx, `SELECT `+definitionColumns+` FROM pipeline_definitions
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
			d, err := scanDefinition(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// sameContent compares a stored definition with a candidate's canonical bytes.
func sameContent(d pipeline.Definition, c pipeline.Candidate) bool {
	return d.Digest == c.Content.Digest() && bytes.Equal(d.Content.Canonical(), c.Content.Canonical())
}
