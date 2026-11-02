package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

var _ artifacts.ReleaseStore = (*Store)(nil)

// ListArtifacts implements artifacts.ReleaseStore.
func (s *Store) ListArtifacts(ctx context.Context, scope domain.Scope, after domain.ArtifactID, limit int) ([]artifacts.Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, artifacts.ErrInvalid
	}
	var out []artifacts.Artifact
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+s.columns()+`
FROM artifacts
WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR id > $3)
ORDER BY id ASC
LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanArtifact(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// CreateRelease implements artifacts.ReleaseStore.
func (s *Store) CreateRelease(ctx context.Context, rel artifacts.Release, _ string) (artifacts.Release, bool, error) {
	scope := rel.Scope()
	var out artifacts.Release
	var created bool
	err := s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := findReleaseTx(ctx, tx, scope, rel.Name(), rel.VersionLabel(), "FOR UPDATE")
		if err == nil {
			if existing.Manifest().Digest() == rel.Manifest().Digest() && bytes.Equal(existing.Manifest().Canonical(), rel.Manifest().Canonical()) {
				out, created = existing, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}
		if !errors.Is(err, artifacts.ErrNotFound) {
			return err
		}

		entries := rel.Manifest().Entries()
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		sort.Strings(names)

		// Lock referenced artifacts in deterministic ID order and verify usability.
		artifactIDs := make([]domain.ArtifactID, 0, len(entries))
		seenID := map[domain.ArtifactID]bool{}
		for _, n := range names {
			id := entries[n].ArtifactID
			if !seenID[id] {
				seenID[id] = true
				artifactIDs = append(artifactIDs, id)
			}
		}
		sort.Slice(artifactIDs, func(i, j int) bool { return artifactIDs[i] < artifactIDs[j] })
		loaded := make(map[domain.ArtifactID]artifacts.Artifact, len(artifactIDs))
		for _, id := range artifactIDs {
			a, err := s.getArtifact(ctx, tx, scope, id, "FOR SHARE")
			if err != nil {
				return err
			}
			if !a.Usable() || a.Role() != artifacts.RoleOutput {
				return fmt.Errorf("%w: artifact %s is not a usable output", artifacts.ErrOutputInvalid, id)
			}
			loaded[id] = a
		}

		producersByID := make(map[string]int, len(rel.Manifest().Producers()))
		prods := rel.Manifest().Producers()
		for i, p := range prods {
			producersByID[string(p.ArtifactID)] = i
		}
		hasSource := false
		for _, n := range names {
			entry := entries[n]
			a := loaded[entry.ArtifactID]
			prod := prods[producersByID[string(entry.ArtifactID)]]
			if err := artifacts.ValidateReleaseEntryProvenance(a, entry, prod, rel.Manifest().Sources()); err != nil {
				return err
			}
			if !a.Provenance().IsZero() {
				hasSource = true
			}
		}
		if !hasSource && len(rel.Manifest().Sources()) > 0 {
			return fmt.Errorf("%w: source-free artifacts cannot declare fabricated release sources", artifacts.ErrOutputInvalid)
		}

		tag, err := tx.Exec(ctx, `INSERT INTO artifact_releases (
tenant_id, project_id, id, name, version_label, manifest_digest, manifest,
created_by_type, created_by_id, created_at, updated_at, retired_at, version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (tenant_id, project_id, name, version_label) DO NOTHING`,
			string(scope.TenantID), string(scope.ProjectID), string(rel.ID()),
			rel.Name(), rel.VersionLabel(), string(rel.ManifestDigest()), rel.Manifest().Canonical(),
			rel.CreatedBy().Type, rel.CreatedBy().ID, rel.CreatedAt(), rel.UpdatedAt(), rel.RetiredAt(), int64(rel.Version()),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			winner, werr := findReleaseTx(ctx, tx, scope, rel.Name(), rel.VersionLabel(), "FOR SHARE")
			if werr != nil {
				return werr
			}
			if winner.Manifest().Digest() == rel.Manifest().Digest() && bytes.Equal(winner.Manifest().Canonical(), rel.Manifest().Canonical()) {
				out, created = winner, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}

		owner := artifacts.ReleasePinOwner(rel.ID())
		for _, n := range names {
			entry := entries[n]
			_, err := tx.Exec(ctx, `INSERT INTO artifact_release_entries (
tenant_id, project_id, release_id, entry_name, artifact_id, digest, kind, media_type
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				string(scope.TenantID), string(scope.ProjectID), string(rel.ID()),
				n, string(entry.ArtifactID), string(entry.Digest), string(entry.Kind), entry.MediaType,
			)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO artifact_pins (tenant_id, project_id, artifact_id, owner_kind, owner_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
				string(scope.TenantID), string(scope.ProjectID), string(entry.ArtifactID), owner.Kind, owner.ID, rel.CreatedAt(),
			)
			if err != nil {
				return err
			}
		}
		out, created = rel, true
		return nil
	})
	return out, created, err
}

func scanRelease(r rowScanner) (artifacts.Release, error) {
	var (
		tenant, project, id, name, verLabel, digest, actorType, actorID string
		manifestBytes                                                   []byte
		createdAt, updatedAt                                            time.Time
		retiredAt                                                       *time.Time
		version                                                         int64
	)
	if err := r.Scan(&tenant, &project, &id, &name, &verLabel, &digest, &manifestBytes,
		&actorType, &actorID, &createdAt, &updatedAt, &retiredAt, &version); err != nil {
		return artifacts.Release{}, err
	}
	m, err := artifacts.ParseReleaseManifest(manifestBytes)
	if err != nil {
		return artifacts.Release{}, fmt.Errorf("%w: %v", artifacts.ErrInconsistent, err)
	}
	if string(m.Digest()) != digest || m.Scope().TenantID != domain.TenantID(tenant) || m.Scope().ProjectID != domain.ProjectID(project) || m.Name() != name || m.VersionLabel() != verLabel {
		return artifacts.Release{}, fmt.Errorf("%w: release row disagrees with canonical manifest", artifacts.ErrInconsistent)
	}
	return artifacts.RestoreRelease(artifacts.ReleaseRecord{
		ID:        domain.ReleaseID(id),
		Manifest:  m,
		Version:   domain.Version(version),
		CreatedBy: artifacts.Actor{Type: actorType, ID: actorID},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		RetiredAt: retiredAt,
	})
}

const releaseColumns = `tenant_id, project_id, id, name, version_label, manifest_digest, manifest,
created_by_type, created_by_id, created_at, updated_at, retired_at, version`

func getReleaseTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.ReleaseID, lock string) (artifacts.Release, error) {
	q := `SELECT ` + releaseColumns + ` FROM artifact_releases WHERE tenant_id = $1 AND project_id = $2 AND id = $3`
	if lock != "" {
		q += ` ` + lock
	}
	rel, err := scanRelease(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifacts.Release{}, artifacts.ErrNotFound
	}
	return rel, err
}

func findReleaseTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, name, versionLabel, lock string) (artifacts.Release, error) {
	q := `SELECT ` + releaseColumns + ` FROM artifact_releases WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND version_label = $4`
	if lock != "" {
		q += ` ` + lock
	}
	rel, err := scanRelease(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), name, versionLabel))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifacts.Release{}, artifacts.ErrNotFound
	}
	return rel, err
}

// GetRelease implements artifacts.ReleaseStore.
func (s *Store) GetRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = getReleaseTx(ctx, tx, scope, id, "")
		return err
	})
	return out, err
}

// FindRelease implements artifacts.ReleaseStore.
func (s *Store) FindRelease(ctx context.Context, scope domain.Scope, name, versionLabel string) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = findReleaseTx(ctx, tx, scope, name, versionLabel, "")
		return err
	})
	return out, err
}

// ListReleases implements artifacts.ReleaseStore.
func (s *Store) ListReleases(ctx context.Context, scope domain.Scope, nameFilter string, after domain.ReleaseID, limit int) ([]artifacts.Release, error) {
	var out []artifacts.Release
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+releaseColumns+`
FROM artifact_releases
WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR name = $3) AND ($4 = '' OR id > $4)
ORDER BY id ASC
LIMIT $5`, string(scope.TenantID), string(scope.ProjectID), nameFilter, string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRelease(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// RetireRelease implements artifacts.ReleaseStore.
func (s *Store) RetireRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID, expected domain.Version, _ artifacts.Actor, _ string, now time.Time) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := getReleaseTx(ctx, tx, scope, id, "FOR UPDATE")
		if err != nil {
			return err
		}
		retired, err := cur.Retire(expected, now)
		if err != nil {
			return err
		}
		if retired.Version() != cur.Version() {
			_, err = tx.Exec(ctx, `UPDATE artifact_releases
SET retired_at = $1, updated_at = $2, version = $3
WHERE tenant_id = $4 AND project_id = $5 AND id = $6 AND version = $7`,
				retired.RetiredAt(), retired.UpdatedAt(), int64(retired.Version()),
				string(scope.TenantID), string(scope.ProjectID), string(id), int64(expected),
			)
			if err != nil {
				return err
			}
		}
		owner := artifacts.ReleasePinOwner(id)
		_, err = tx.Exec(ctx, `DELETE FROM artifact_pins
WHERE tenant_id = $1 AND project_id = $2 AND owner_kind = $3 AND owner_id = $4`,
			string(scope.TenantID), string(scope.ProjectID), owner.Kind, owner.ID,
		)
		if err != nil {
			return err
		}
		out = retired
		return nil
	})
	return out, err
}

// SavePublication implements artifacts.ReleaseStore.
func (s *Store) SavePublication(ctx context.Context, pub artifacts.PublicationReceipt, _ artifacts.Actor, _ string) (artifacts.PublicationReceipt, bool, error) {
	if err := pub.Validate(); err != nil {
		return artifacts.PublicationReceipt{}, false, artifacts.ErrInvalid
	}
	var out artifacts.PublicationReceipt
	var created bool
	err := s.write(ctx, pub.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var existingID, artDigest, destDigest, runID, attID, stepID string
		var complete bool
		var verifiedAt time.Time
		err := tx.QueryRow(ctx, `SELECT id, artifact_digest, destination_digest, run_id, attempt_id, step_id, complete_graph, verified_at
FROM artifact_publications
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND destination_kind = $4 AND destination_reference = $5
FOR UPDATE`,
			string(pub.Scope.TenantID), string(pub.Scope.ProjectID), string(pub.ArtifactID),
			string(pub.Destination.Kind), pub.Destination.Reference,
		).Scan(&existingID, &artDigest, &destDigest, &runID, &attID, &stepID, &complete, &verifiedAt)
		if err == nil {
			if artDigest == string(pub.ArtifactDigest) && destDigest == string(pub.Destination.Digest) {
				rec, rerr := artifacts.NewPublicationReceipt(
					domain.PublicationID(existingID), pub.Scope, pub.ArtifactID, artDigest,
					string(pub.Destination.Kind), pub.Destination.Reference, destDigest,
					domain.RunID(runID), domain.AttemptID(attID), stepID, complete, verifiedAt,
				)
				if rerr != nil {
					return rerr
				}
				out, created = rec, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		a, err := s.getArtifact(ctx, tx, pub.Scope, pub.ArtifactID, "FOR SHARE")
		if err != nil {
			return err
		}
		if !a.Usable() || string(a.Digest()) != string(pub.ArtifactDigest) {
			return artifacts.ErrIncomplete
		}
		ts, err := time.Parse(time.RFC3339Nano, string(pub.VerifiedAt))
		if err != nil {
			return artifacts.ErrInvalid
		}
		tag, err := tx.Exec(ctx, `INSERT INTO artifact_publications (
tenant_id, project_id, id, artifact_id, artifact_digest, destination_kind, destination_reference, destination_digest,
run_id, attempt_id, step_id, complete_graph, verified_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (tenant_id, project_id, artifact_id, destination_kind, destination_reference) DO NOTHING`,
			string(pub.Scope.TenantID), string(pub.Scope.ProjectID), string(pub.ID),
			string(pub.ArtifactID), string(pub.ArtifactDigest),
			string(pub.Destination.Kind), pub.Destination.Reference, string(pub.Destination.Digest),
			string(pub.RunID), string(pub.AttemptID), pub.StepID, pub.CompleteGraph, ts.UTC(),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			err := tx.QueryRow(ctx, `SELECT id, artifact_digest, destination_digest, run_id, attempt_id, step_id, complete_graph, verified_at
FROM artifact_publications
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND destination_kind = $4 AND destination_reference = $5
FOR SHARE`,
				string(pub.Scope.TenantID), string(pub.Scope.ProjectID), string(pub.ArtifactID),
				string(pub.Destination.Kind), pub.Destination.Reference,
			).Scan(&existingID, &artDigest, &destDigest, &runID, &attID, &stepID, &complete, &verifiedAt)
			if err != nil {
				return err
			}
			if artDigest == string(pub.ArtifactDigest) && destDigest == string(pub.Destination.Digest) {
				rec, rerr := artifacts.NewPublicationReceipt(
					domain.PublicationID(existingID), pub.Scope, pub.ArtifactID, artDigest,
					string(pub.Destination.Kind), pub.Destination.Reference, destDigest,
					domain.RunID(runID), domain.AttemptID(attID), stepID, complete, verifiedAt,
				)
				if rerr != nil {
					return rerr
				}
				out, created = rec, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}
		out, created = pub, true
		return nil
	})
	return out, created, err
}

// ListPublications implements artifacts.ReleaseStore.
func (s *Store) ListPublications(ctx context.Context, scope domain.Scope, artifactID domain.ArtifactID, after domain.PublicationID, limit int) ([]artifacts.PublicationReceipt, error) {
	var out []artifacts.PublicationReceipt
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, artifact_digest, destination_kind, destination_reference, destination_digest, run_id, attempt_id, step_id, complete_graph, verified_at
FROM artifact_publications
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND ($4 = '' OR id > $4)
ORDER BY id ASC
LIMIT $5`, string(scope.TenantID), string(scope.ProjectID), string(artifactID), string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, artDigest, destKind, destRef, destDigest, runID, attID, stepID string
			var complete bool
			var verifiedAt time.Time
			if err := rows.Scan(&id, &artDigest, &destKind, &destRef, &destDigest, &runID, &attID, &stepID, &complete, &verifiedAt); err != nil {
				return err
			}
			rec, err := artifacts.NewPublicationReceipt(
				domain.PublicationID(id), scope, artifactID, artDigest,
				destKind, destRef, destDigest,
				domain.RunID(runID), domain.AttemptID(attID), stepID, complete, verifiedAt,
			)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	return out, err
}
