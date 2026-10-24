package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

const (
	// AuditAction is recorded once per created version.
	AuditAction = "pipeline_definition.registered"
	// AuditResourceType is the audit resource type of a definition.
	AuditResourceType = "pipeline_definition"
)

// maxIDBytes leaves room for the "aud_" prefix within the 128-byte ID syntax.
const maxIDBytes = 120

// RegisterOrGet implements pipeline.Repository.
//
// Inside one tenant transaction: a lock-free lookup by scoped digest returns an
// existing identical version without writing anything. Otherwise the project
// row is locked (bounded by lock_timeout), the digest is rechecked, the next
// version of the name is allocated, and the definition and its audit row are
// inserted. Duplicates therefore create no version and no audit row; an audit
// failure or cancellation rolls the definition back. No network call happens
// while the lock is held.
func (s *Store) RegisterOrGet(ctx context.Context, c pipeline.Candidate) (pipeline.Definition, bool, error) {
	if err := c.Validate(); err != nil {
		return pipeline.Definition{}, false, fmt.Errorf("%w: %w", pipeline.ErrInvalid, err)
	}
	if len(c.ID) > maxIDBytes {
		return pipeline.Definition{}, false, fmt.Errorf("%w: definition identifier is too long", pipeline.ErrInvalid)
	}
	var (
		out     pipeline.Definition
		created bool
		wrote   bool
	)
	err := s.inTenant(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if existing, ok, err := byDigest(ctx, tx, c.Scope, c.Content.Digest()); err != nil {
			return err
		} else if ok {
			if !sameContent(existing, c) {
				return fmt.Errorf("%w: stored content differs from the content with the same digest", pipeline.ErrConflict)
			}
			out = existing
			return nil
		}

		// Bound the wait for the project lock; transaction-local.
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`,
			fmt.Sprintf("%dms", s.lockTimeout.Milliseconds())); err != nil {
			return err
		}
		if err := lockProject(ctx, tx, c.Scope); err != nil {
			return err
		}
		// A concurrent registration of the same content may have committed while
		// this transaction waited for the lock.
		if existing, ok, err := byDigest(ctx, tx, c.Scope, c.Content.Digest()); err != nil {
			return err
		} else if ok {
			if !sameContent(existing, c) {
				return fmt.Errorf("%w: stored content differs from the content with the same digest", pipeline.ErrConflict)
			}
			out = existing
			return nil
		}

		var next int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM pipeline_definitions
			WHERE tenant_id = $1 AND project_id = $2 AND name = $3`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), c.Content.Name()).Scan(&next); err != nil {
			return err
		}
		def, err := pipeline.NewDefinition(c, domain.Version(next))
		if err != nil {
			return fmt.Errorf("%w: %w", pipeline.ErrInvalid, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pipeline_definitions
			(tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			string(def.Scope.TenantID), string(def.Scope.ProjectID), string(def.ID), def.Name, int64(def.Version),
			string(def.Digest), string(def.Content.Canonical()), string(def.CreatedBy.Type), def.CreatedBy.ID, def.CreatedAt); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, def, c.RequestID); err != nil {
			return err
		}
		out, created, wrote = def, true, true
		return nil
	})
	if err != nil && wrote {
		// Every statement succeeded, so the failure is the commit's: it may or
		// may not have been applied. Registration is content-addressed, so the
		// caller's retry converges on one definition either way.
		return pipeline.Definition{}, false, fmt.Errorf("%w: %w: the outcome of the registration is unknown; retry the same request",
			pipeline.ErrUnavailable, pipeline.ErrOutcomeUnknown)
	}
	if err != nil {
		return pipeline.Definition{}, false, err
	}
	return out, created, nil
}

// lockProject serializes version allocation within one project. FOR NO KEY
// UPDATE does not block the foreign-key checks of concurrent inserts.
func lockProject(ctx context.Context, tx pgx.Tx, scope domain.Scope) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE`,
		string(scope.TenantID), string(scope.ProjectID)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the project was not found", pipeline.ErrNotFound)
	}
	return err
}

// insertAudit appends the creation audit row. The metadata holds only values
// safe for every reader of the audit trail: the name, version and digest.
func insertAudit(ctx context.Context, tx pgx.Tx, d pipeline.Definition, requestID string) error {
	actorType := string(d.CreatedBy.Type)
	if d.CreatedBy.Type == pipeline.ActorService {
		actorType = "system" // the audit table's name for the internal service actor
	}
	meta, err := json.Marshal(map[string]any{"name": d.Name, "version": int64(d.Version), "digest": string(d.Digest)})
	if err != nil {
		return err
	}
	var req *string
	if requestID != "" {
		req = &requestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit
		(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(d.Scope.TenantID), "aud_"+string(d.ID), string(d.Scope.ProjectID), d.CreatedAt, actorType, d.CreatedBy.ID,
		AuditAction, AuditResourceType, string(d.ID), int64(d.Version), req, meta)
	return err
}
