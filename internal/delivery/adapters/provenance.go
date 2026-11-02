package adapters

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

// PinOwnerKind is the artifact-pin owner kind of a deployment. The owner ID is
// the deployment, so one pin protects the image of that deployment until its
// owner releases it.
const PinOwnerKind = "deployment"

// ArtifactLookup is the artifact module's scoped read and pin surface.
// *artifacts.Service implements it.
type ArtifactLookup interface {
	Reference(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (artifacts.Reference, error)
	Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error
	Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error
}

// Artifacts implements delivery.Artifacts over the artifact owner.
type Artifacts struct{ svc ArtifactLookup }

var _ delivery.Artifacts = (*Artifacts)(nil)

// NewArtifacts requires the artifact service.
func NewArtifacts(svc ArtifactLookup) (*Artifacts, error) {
	if svc == nil {
		return nil, errors.New("delivery/adapters: an artifact service is required")
	}
	return &Artifacts{svc: svc}, nil
}

// Evidence implements delivery.Artifacts. The artifact owner scopes the lookup;
// absent and foreign artifacts are both ErrNotFound.
func (a *Artifacts) Evidence(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (delivery.ImageEvidence, error) {
	r, err := a.svc.Reference(ctx, scope, id)
	if err != nil {
		return delivery.ImageEvidence{}, mapArtifacts(err)
	}
	src := delivery.Source{RepositoryURL: r.Provenance.RepositoryURL, Commit: r.Provenance.Commit}
	if !r.Provenance.SourceSet.IsZero() {
		// The artifact's source set, in the same canonical form the registration
		// compares: a stored set that cannot be rebuilt is an outage, not a match.
		commits := make([]delivery.SourceCommit, 0, len(r.Provenance.SourceSet.Commits()))
		for _, c := range r.Provenance.SourceSet.Commits() {
			commits = append(commits, delivery.SourceCommit{RepositoryID: c.RepositoryID, Commit: c.Commit})
		}
		set, err := delivery.NewSourceSet(r.Provenance.SourceSet.Digest, commits)
		if err != nil {
			return delivery.ImageEvidence{}, delivery.Fail(delivery.ErrUnavailable, err, "the artifact's source set is invalid")
		}
		src = delivery.Source{SourceSet: set}
	}
	ev := delivery.ImageEvidence{
		ID: r.ID, Scope: r.Scope, Attempt: r.Attempt, Kind: runtimev1.ArtifactKind(r.Kind), Role: runtimev1.ArtifactRole(r.Role), Usable: r.Usable,
		Digest: runtimev1.Digest(r.Digest), Provenance: src,
	}
	if r.Provenance.Modified() {
		// An image built in a workspace that was not the commit it started from is
		// not an output of that commit (DQ-16): it matches no deployment source.
		ev.Provenance = delivery.Source{}
	}
	return ev, nil
}

// Protect implements delivery.Artifacts: an idempotent pin owned by the
// deployment.
func (a *Artifacts) Protect(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner delivery.DeploymentID) error {
	if err := a.svc.Pin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: string(owner)}); err != nil {
		return mapArtifacts(err)
	}
	return nil
}

// Release implements delivery.Artifacts.
func (a *Artifacts) Release(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner delivery.DeploymentID) error {
	if err := a.svc.Unpin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: string(owner)}); err != nil {
		return mapArtifacts(err)
	}
	return nil
}

func mapArtifacts(err error) error {
	switch {
	case errors.Is(err, artifacts.ErrNotFound), errors.Is(err, artifacts.ErrIncomplete):
		return delivery.Fail(delivery.ErrNotFound, err, "not found")
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "the artifact store could not answer")
}
