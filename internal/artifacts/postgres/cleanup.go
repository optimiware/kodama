package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

func seconds(d time.Duration) float64 { return d.Seconds() }

// Tenants implements artifacts.CleanupStore through the SECURITY DEFINER
// enumeration function; it returns identifiers only.
func (s *Store) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT kodama_list_tenant_ids()`)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var out []domain.TenantID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.TenantID(id))
	}
	return out, classify(rows.Err())
}

// AbandonExpired implements artifacts.CleanupStore. Only idle reservations
// whose latest issued transfer (and latest completion lease) expired more than
// AbandonAfter ago qualify; the row lock orders this against a late completion.
func (s *Store) AbandonExpired(ctx context.Context, tenant domain.TenantID, now time.Time, p artifacts.CleanupPolicy) (int, error) {
	n := 0
	err := s.write(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+s.columns()+` FROM artifacts
WHERE tenant_id = $1 AND state = 'reserved' AND disposition = 'active' AND kind = 'file'
  AND transfer_expires_at + make_interval(secs => $2) <= $3
  AND (claim_until IS NULL OR claim_until + make_interval(secs => $2) <= $3)
ORDER BY transfer_expires_at, id LIMIT $4 FOR UPDATE SKIP LOCKED`, string(tenant), seconds(p.AbandonAfter), now, p.Batch)
		if err != nil {
			return err
		}
		var due []artifacts.Artifact
		for rows.Next() {
			a, err := scanArtifact(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range due {
			ab, err := a.Abandon(now)
			if err != nil {
				return err
			}
			sc := a.Scope()
			if _, err := tx.Exec(ctx, `UPDATE artifacts SET disposition = 'abandoned', version = $4, updated_at = $5
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(sc.TenantID), string(sc.ProjectID), string(a.ID()), int64(ab.Version()), now); err != nil {
				return err
			}
			if err := auditEntry(ctx, tx, ab, artifacts.Actor{Type: "system", ID: "artifact-reconciler"}, "artifact.abandoned", "", now); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ClaimDeletions implements artifacts.CleanupStore.
//
// Step one promotes eligible live objects to durable deletion intent. An object
// is eligible only when it is not the winner, no holder of a credential or
// lease can still write it (writable_until plus the in-flight grace has passed)
// and it is unreferenced: a staging object of a usable or closed artifact, or a
// sealed candidate that is not the live claim's destination. Step two leases a
// batch of pending deletions under a fresh token.
func (s *Store) ClaimDeletions(ctx context.Context, tenant domain.TenantID, now time.Time, p artifacts.CleanupPolicy, token string) ([]artifacts.DeletionClaim, error) {
	var out []artifacts.DeletionClaim
	err := s.write(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `WITH due AS (
  SELECT o.project_id, o.artifact_id, o.key
  FROM artifact_objects o
  JOIN artifacts a ON a.tenant_id = o.tenant_id AND a.project_id = o.project_id AND a.id = o.artifact_id
  WHERE o.tenant_id = $1 AND o.state = 'live' AND NOT o.winner
    AND o.writable_until + make_interval(secs => $2) <= $3
    AND (a.state = 'usable' OR a.disposition <> 'active'
         OR (o.purpose = 'candidate'
             AND NOT (a.claim_generation = o.generation AND a.claim_until IS NOT NULL AND a.claim_until >= $3)))
  ORDER BY o.writable_until, o.key LIMIT $4
  FOR UPDATE OF o SKIP LOCKED)
UPDATE artifact_objects o SET state = 'delete_pending', delete_requested_at = $3
FROM due WHERE o.tenant_id = $1 AND o.project_id = due.project_id AND o.artifact_id = due.artifact_id AND o.key = due.key`,
			string(tenant), seconds(p.StagingGrace), now, p.Batch); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH due AS (
  SELECT project_id, artifact_id, key FROM artifact_objects
  WHERE tenant_id = $1 AND state = 'delete_pending' AND (delete_until IS NULL OR delete_until <= $2)
  ORDER BY delete_requested_at, key LIMIT $3 FOR UPDATE SKIP LOCKED)
UPDATE artifact_objects o SET delete_token = $4, delete_until = $2 + make_interval(secs => $5), delete_attempts = o.delete_attempts + 1
FROM due WHERE o.tenant_id = $1 AND o.project_id = due.project_id AND o.artifact_id = due.artifact_id AND o.key = due.key
RETURNING o.project_id, o.artifact_id, o.key`, string(tenant), now, p.Batch, token, seconds(p.DeletionLease))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var project, artifact, key string
			if err := rows.Scan(&project, &artifact, &key); err != nil {
				return err
			}
			out = append(out, artifacts.DeletionClaim{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)},
				ArtifactID: domain.ArtifactID(artifact), Key: key, Token: token})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AckDeletion implements artifacts.CleanupStore. It records deletion only while
// the lease is held and no renewed transfer reopened the object's write horizon.
func (s *Store) AckDeletion(ctx context.Context, c artifacts.DeletionClaim, now time.Time, p artifacts.CleanupPolicy) error {
	return s.write(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE artifact_objects SET state = 'deleted', deleted_at = $6, delete_token = NULL, delete_until = NULL
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND key = $4 AND state = 'delete_pending' AND delete_token = $5
  AND (NOT (purpose = 'staging') OR writable_until + make_interval(secs => $7) <= $6)`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.ArtifactID), c.Key, c.Token, now, seconds(p.StagingGrace))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return artifacts.ErrClaimLost
		}
		return nil
	})
}

// RetentionCandidates implements artifacts.CleanupStore.
func (s *Store) RetentionCandidates(ctx context.Context, tenant domain.TenantID, after domain.ArtifactID, limit int) ([]artifacts.RetentionSubject, error) {
	var out []artifacts.RetentionSubject
	err := s.read(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+s.columns()+` FROM artifacts a
WHERE tenant_id = $1 AND state IN ('usable', 'registered') AND disposition = 'active' AND id > $2
  AND NOT EXISTS (SELECT 1 FROM artifact_pins p WHERE p.tenant_id = a.tenant_id AND p.project_id = a.project_id AND p.artifact_id = a.id)
ORDER BY id LIMIT $3`, string(tenant), string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanArtifact(rows)
			if err != nil {
				return err
			}
			var verified time.Time
			if a.VerifiedAt() != nil {
				verified = *a.VerifiedAt()
			}
			out = append(out, artifacts.RetentionSubject{ID: a.ID(), Scope: a.Scope(), Attempt: a.Attempt(), Kind: a.Kind(), Role: a.Role(),
				CreatedAt: a.CreatedAt(), VerifiedAt: verified})
		}
		return rows.Err()
	})
	return out, err
}

// BeginRetentionDeletion implements artifacts.CleanupStore. The artifact row
// lock orders it against Pin: a pin committed first is seen here, and a pin
// arriving later waits and then finds the artifact removed.
func (s *Store) BeginRetentionDeletion(ctx context.Context, scope domain.Scope, id domain.ArtifactID, now time.Time, actor artifacts.Actor) error {
	return s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := s.getArtifact(ctx, tx, scope, id, "FOR UPDATE")
		if err != nil {
			return err
		}
		if !a.Usable() {
			return artifacts.ErrNotFound
		}
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artifact_pins WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3)`,
			string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&pinned); err != nil {
			return err
		}
		if pinned {
			return artifacts.ErrPinned
		}
		n, err := a.Remove(now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE artifacts SET disposition = 'removed', version = $4, updated_at = $5
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(scope.TenantID), string(scope.ProjectID), string(id), int64(n.Version()), now); err != nil {
			return err
		}
		// Deletion intent is durable before the object store is touched. A local
		// OCI registration has no object; registry content is never deleted here.
		if _, err := tx.Exec(ctx, `UPDATE artifact_objects SET state = 'delete_pending', delete_requested_at = $4
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND winner AND state = 'live'`,
			string(scope.TenantID), string(scope.ProjectID), string(id), now); err != nil {
			return err
		}
		return auditEntry(ctx, tx, n, actor, "artifact.removed", "", now)
	})
}
