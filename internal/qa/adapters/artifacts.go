package adapters

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
)

// PinOwnerKind is the owner kind of a QA defect's artifact pins.
const PinOwnerKind = "qa_defect"

// Artifacts implements qa.Artifacts over the artifact module's repository and
// object store: scoped metadata, sealed verified bytes and retention pins. No
// storage key leaves it.
type Artifacts struct {
	repo    artifacts.Repository
	objects artifacts.ObjectStore
	now     func() time.Time
}

var _ qa.Artifacts = (*Artifacts)(nil)

// NewArtifacts requires both collaborators; a nil clock selects UTC wall time.
func NewArtifacts(r artifacts.Repository, o artifacts.ObjectStore, clock func() time.Time) (*Artifacts, error) {
	if r == nil || o == nil {
		return nil, errors.New("qa/adapters: an artifact repository and an object store are required")
	}
	if clock == nil {
		clock = nowUTC
	}
	return &Artifacts{repo: r, objects: o, now: clock}, nil
}

func mapArtifact(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifacts.ErrNotFound):
		return qa.Failf(qa.ErrNotFound, "artifact not found")
	case errors.Is(err, artifacts.ErrIncomplete), errors.Is(err, artifacts.ErrObjectNotFound), errors.Is(err, artifacts.ErrDigestMismatch),
		errors.Is(err, artifacts.ErrInconsistent):
		return qa.Failf(qa.ErrEvidence, "an artifact is not verified content")
	}
	return qa.Unavailablef(err, "the artifact store is unavailable")
}

// Describe implements qa.Artifacts. A foreign or absent artifact is
// qa.ErrNotFound.
func (a *Artifacts) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (qa.EvidenceMeta, error) {
	art, err := a.repo.Get(ctx, scope, id)
	if err != nil {
		return qa.EvidenceMeta{}, mapArtifact(err)
	}
	res := art.Reservation()
	meta := qa.EvidenceMeta{
		ID: art.ID(), Scope: art.Scope(), Run: res.Run, Attempt: art.Attempt(), Step: res.Step,
		Digest: runtimev1.Digest(art.Digest()), MediaType: art.MediaType(), File: art.Kind() == artifacts.KindFile, Usable: art.Usable(),
	}
	if size, ok := art.Size(); ok {
		meta.Size = size
	}
	return meta, nil
}

// Read implements qa.Artifacts: bounded bytes of a usable, sealed file whose
// length and digest were re-verified against the metadata.
func (a *Artifacts) Read(ctx context.Context, scope domain.Scope, meta qa.EvidenceMeta, max int64) ([]byte, error) {
	art, err := a.repo.Get(ctx, scope, meta.ID)
	if err != nil {
		return nil, mapArtifact(err)
	}
	if art.Kind() != artifacts.KindFile || !art.Usable() || art.SealedKey() == "" || runtimev1.Digest(art.Digest()) != meta.Digest {
		return nil, qa.Failf(qa.ErrEvidence, "the artifact has no verified file content")
	}
	size, _ := art.Size()
	if size < 0 || size > max {
		return nil, qa.Failf(qa.ErrEvidence, "the artifact exceeds the read bound")
	}
	obj, err := a.objects.Open(ctx, art.SealedKey())
	if err != nil {
		if errors.Is(err, artifacts.ErrObjectNotFound) {
			return nil, qa.Failf(qa.ErrEvidence, "the artifact content is absent")
		}
		return nil, qa.Unavailablef(err, "the object store is unavailable")
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(io.LimitReader(obj.Body, max+1))
	if err != nil {
		return nil, qa.Unavailablef(err, "the object store is unavailable")
	}
	if int64(len(data)) != size || artifacts.DigestOf(data) != art.Digest() || !bytes.Equal([]byte(art.Digest()), []byte(meta.Digest)) {
		return nil, qa.Failf(qa.ErrEvidence, "the artifact bytes do not match their length and digest")
	}
	return data, nil
}

// Pin implements qa.Artifacts. Pinning is idempotent and serialized against
// retention deletion in the repository; a removed artifact is not found.
func (a *Artifacts) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner string) error {
	return mapArtifact(a.repo.Pin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: owner}, a.now()))
}
