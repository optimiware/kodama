package adapters

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// BundleRepository is the artifact module's scoped metadata and pin surface.
// artifacts.Repository (the PostgreSQL store) implements it.
type BundleRepository interface {
	Get(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (artifacts.Artifact, error)
	Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error
}

// SealedObjects opens sealed, verified content by its server-generated logical
// key. artifacts.ObjectStore implements it.
type SealedObjects interface {
	Open(ctx context.Context, key string) (artifacts.Object, error)
}

// Bundles implements workspace.Bundles: scoped access to a completed file
// bundle's sealed bytes. The workspace module checks the producing attempt and
// the digest; the Git preparer re-verifies the digest and size while reading,
// so a corrupted or swapped object is never published.
type Bundles struct {
	repo    BundleRepository
	objects SealedObjects
}

var _ workspace.Bundles = (*Bundles)(nil)

// NewBundles requires both collaborators.
func NewBundles(repo BundleRepository, objects SealedObjects) (*Bundles, error) {
	if repo == nil || objects == nil {
		return nil, errors.New("results/adapters: an artifact repository and sealed object access are required")
	}
	return &Bundles{repo: repo, objects: objects}, nil
}

// Open implements workspace.Bundles. A missing or foreign artifact is
// workspace.ErrNotFound (one answer for both). An artifact that is not a usable
// file bundle is returned with Complete false and no bytes, so the workspace
// reports it incomplete without this adapter reading anything.
func (b *Bundles) Open(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (workspace.Bundle, error) {
	a, err := b.repo.Get(ctx, scope, id)
	switch {
	case errors.Is(err, artifacts.ErrNotFound):
		return workspace.Bundle{}, workspace.ErrNotFound
	case err != nil:
		return workspace.Bundle{}, err
	}
	meta := workspace.Bundle{ID: a.ID(), Scope: a.Scope(), AttemptID: a.Attempt(), Role: string(a.Role()), Body: io.NopCloser(bytes.NewReader(nil))}
	size, hasSize := a.Size()
	if a.Kind() != artifacts.KindFile || !a.Usable() || a.SealedKey() == "" || !hasSize {
		return meta, nil
	}
	obj, err := b.objects.Open(ctx, a.SealedKey())
	if err != nil {
		return workspace.Bundle{}, err
	}
	meta.Complete, meta.Digest, meta.Size, meta.Body = true, string(a.Digest()), size, obj.Body
	return meta, nil
}

// Settled implements workspace.Bundles: the publication reached a final state,
// so the finalization pin of the bundle is released. Deleting the bytes is the
// artifact module's retention mechanism and needs its owner-supplied policy;
// nothing is deleted here, and the call is best effort.
func (b *Bundles) Settled(ctx context.Context, scope domain.Scope, id domain.ArtifactID) error {
	a, err := b.repo.Get(ctx, scope, id)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return nil
		}
		return err
	}
	return b.repo.Unpin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: string(a.Attempt())})
}
