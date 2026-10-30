package artifactstest

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

type releaseRec struct {
	R artifacts.Release
}

type publicationRec struct {
	P artifactsv1.PublicationReceipt
}

func rkey(project domain.ProjectID, id domain.ReleaseID) string {
	return string(project) + "/" + string(id)
}

func pubkey(project domain.ProjectID, id domain.PublicationID) string {
	return string(project) + "/" + string(id)
}

var (
	releasesT = memdb.NewTable("artifact_releases",
		func(r releaseRec) string { return rkey(r.R.Scope().ProjectID, r.R.ID()) },
		func(r releaseRec) string {
			return string(r.R.Scope().ProjectID) + "/" + r.R.Name() + "/" + r.R.VersionLabel()
		})
	publicationsT = memdb.NewTable("artifact_publications",
		func(r publicationRec) string { return pubkey(r.P.Scope.ProjectID, r.P.ID) },
		func(r publicationRec) string {
			return fmt.Sprintf("%s/%s/%s/%s/%s",
				r.P.Scope.ProjectID, r.P.ArtifactID, r.P.Destination.Kind, r.P.Destination.Reference, r.P.Destination.Digest)
		})
)

var _ artifacts.ReleaseStore = (*MemStore)(nil)

// ListArtifacts implements artifacts.ReleaseStore.
func (s *MemStore) ListArtifacts(ctx context.Context, scope domain.Scope, after domain.ArtifactID, limit int) ([]artifacts.Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, artifacts.ErrInvalid
	}
	var all []artifacts.Artifact
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		artifactsT.Scan(tx, func(r artifactRec) bool {
			if r.A.Scope() == scope && (after == "" || r.A.ID() > after) {
				all = append(all, r.A)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID() < all[j].ID() })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// CreateRelease implements artifacts.ReleaseStore.
func (s *MemStore) CreateRelease(ctx context.Context, rel artifacts.Release, _ string) (artifacts.Release, bool, error) {
	scope := rel.Scope()
	if err := scope.Validate(); err != nil {
		return artifacts.Release{}, false, artifacts.ErrInvalid
	}
	s.know(scope.TenantID)
	var out artifacts.Release
	var created bool
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		// Check if (scope, name, versionLabel) already exists.
		var existing *artifacts.Release
		releasesT.Scan(tx, func(r releaseRec) bool {
			if r.R.Scope() == scope && r.R.Name() == rel.Name() && r.R.VersionLabel() == rel.VersionLabel() {
				cp := r.R
				existing = &cp
				return false
			}
			return true
		})
		if existing != nil {
			if existing.Manifest().Digest() == rel.Manifest().Digest() && bytes.Equal(existing.Manifest().Canonical(), rel.Manifest().Canonical()) {
				out, created = *existing, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}

		// Validate every referenced artifact in deterministic ID order and pin it.
		entries := rel.Manifest().Entries()
		producersByArt := make(map[domain.ArtifactID]artifactsv1.ProducerRef, len(rel.Manifest().Producers()))
		for _, p := range rel.Manifest().Producers() {
			producersByArt[p.ArtifactID] = p
		}
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		sort.Strings(names)
		owner := artifacts.ReleasePinOwner(rel.ID())
		for _, n := range names {
			entry := entries[n]
			a, err := load(tx, scope, entry.ArtifactID)
			if err != nil {
				return err
			}
			if err := artifacts.ValidateReleaseEntryProvenance(a, entry, producersByArt[entry.ArtifactID], rel.Manifest().Sources()); err != nil {
				return err
			}
			if err := pinsT.Put(tx, pinRec{Project: scope.ProjectID, Artifact: a.ID(), Owner: owner}); err != nil {
				return err
			}
		}

		if err := releasesT.Put(tx, releaseRec{R: rel}); err != nil {
			return artifacts.ErrIdempotencyConflict
		}
		out, created = rel, true
		return nil
	})
	return out, created, err
}

// GetRelease implements artifacts.ReleaseStore.
func (s *MemStore) GetRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := releasesT.Get(tx, rkey(scope.ProjectID, id))
		if !ok || r.R.Scope() != scope {
			return artifacts.ErrNotFound
		}
		out = r.R
		return nil
	})
	return out, err
}

// FindRelease implements artifacts.ReleaseStore.
func (s *MemStore) FindRelease(ctx context.Context, scope domain.Scope, name, versionLabel string) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		found := false
		releasesT.Scan(tx, func(r releaseRec) bool {
			if r.R.Scope() == scope && r.R.Name() == name && r.R.VersionLabel() == versionLabel {
				out, found = r.R, true
				return false
			}
			return true
		})
		if !found {
			return artifacts.ErrNotFound
		}
		return nil
	})
	return out, err
}

// ListReleases implements artifacts.ReleaseStore.
func (s *MemStore) ListReleases(ctx context.Context, scope domain.Scope, nameFilter string, after domain.ReleaseID, limit int) ([]artifacts.Release, error) {
	var all []artifacts.Release
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		releasesT.Scan(tx, func(r releaseRec) bool {
			if r.R.Scope() == scope && (nameFilter == "" || r.R.Name() == nameFilter) && (after == "" || r.R.ID() > after) {
				all = append(all, r.R)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID() < all[j].ID() })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// RetireRelease implements artifacts.ReleaseStore.
func (s *MemStore) RetireRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID, expected domain.Version, _ artifacts.Actor, _ string, now time.Time) (artifacts.Release, error) {
	var out artifacts.Release
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := releasesT.Get(tx, rkey(scope.ProjectID, id))
		if !ok || r.R.Scope() != scope {
			return artifacts.ErrNotFound
		}
		retired, err := r.R.Retire(expected, now)
		if err != nil {
			return err
		}
		if err := releasesT.Put(tx, releaseRec{R: retired}); err != nil {
			return err
		}
		// Unpin only this release's pins; other owners' pins remain untouched.
		owner := artifacts.ReleasePinOwner(id)
		for _, entry := range retired.Manifest().Entries() {
			pinsT.Delete(tx, pkey(scope.ProjectID, entry.ArtifactID, owner))
		}
		out = retired
		return nil
	})
	return out, err
}

// SavePublication implements artifacts.ReleaseStore.
func (s *MemStore) SavePublication(ctx context.Context, pub artifactsv1.PublicationReceipt, _ artifacts.Actor, _ string) (artifactsv1.PublicationReceipt, bool, error) {
	if err := pub.Validate(); err != nil {
		return artifactsv1.PublicationReceipt{}, false, artifacts.ErrInvalid
	}
	s.know(pub.Scope.TenantID)
	var out artifactsv1.PublicationReceipt
	var created bool
	err := s.store.Update(ctx, pub.Scope.TenantID, func(tx memdb.Tx) error {
		var existing *artifactsv1.PublicationReceipt
		publicationsT.Scan(tx, func(r publicationRec) bool {
			if r.P.Scope == pub.Scope && r.P.ArtifactID == pub.ArtifactID &&
				r.P.Destination.Kind == pub.Destination.Kind &&
				r.P.Destination.Reference == pub.Destination.Reference {
				cp := r.P
				existing = &cp
				return false
			}
			return true
		})
		if existing != nil {
			if existing.Destination.Digest == pub.Destination.Digest && existing.ArtifactDigest == pub.ArtifactDigest {
				out, created = *existing, false
				return nil
			}
			return artifacts.ErrIdempotencyConflict
		}
		a, err := load(tx, pub.Scope, pub.ArtifactID)
		if err != nil {
			return err
		}
		if !a.Usable() || string(a.Digest()) != string(pub.ArtifactDigest) {
			return artifacts.ErrIncomplete
		}
		if err := publicationsT.Put(tx, publicationRec{P: pub}); err != nil {
			return artifacts.ErrIdempotencyConflict
		}
		out, created = pub, true
		return nil
	})
	return out, created, err
}

// ListPublications implements artifacts.ReleaseStore.
func (s *MemStore) ListPublications(ctx context.Context, scope domain.Scope, artifactID domain.ArtifactID, after domain.PublicationID, limit int) ([]artifactsv1.PublicationReceipt, error) {
	var all []artifactsv1.PublicationReceipt
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		publicationsT.Scan(tx, func(r publicationRec) bool {
			if r.P.Scope == scope && r.P.ArtifactID == artifactID && (after == "" || r.P.ID > after) {
				all = append(all, r.P)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
