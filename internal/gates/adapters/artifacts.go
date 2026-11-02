package adapters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

// Evidence implements gates.Evidence over the artifact module's repository and
// object store. It reads scoped metadata and sealed object bytes; no storage
// key leaves it.
type Evidence struct {
	repo    artifacts.Repository
	objects artifacts.ObjectStore
}

var _ gates.Evidence = (*Evidence)(nil)

// NewEvidence requires both collaborators.
func NewEvidence(r artifacts.Repository, o artifacts.ObjectStore) (*Evidence, error) {
	if r == nil || o == nil {
		return nil, errors.New("gates/adapters: an artifact repository and an object store are required")
	}
	return &Evidence{repo: r, objects: o}, nil
}

func mapArtifact(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifacts.ErrNotFound):
		return gates.ErrNotFound
	case errors.Is(err, artifacts.ErrIncomplete), errors.Is(err, artifacts.ErrObjectNotFound), errors.Is(err, artifacts.ErrDigestMismatch),
		errors.Is(err, artifacts.ErrInconsistent):
		return fmt.Errorf("%w: %s is not verified content", gates.ErrEvidence, what)
	}
	return gates.Unavailablef(err, "the artifact store is unavailable")
}

// Describe implements gates.Evidence. A foreign or absent artifact is
// gates.ErrNotFound; the metadata carries the producer and provenance the
// artifact module verified when it accepted the output.
func (e *Evidence) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (gates.EvidenceMeta, error) {
	a, err := e.repo.Get(ctx, scope, id)
	if err != nil {
		return gates.EvidenceMeta{}, mapArtifact(err, "the artifact")
	}
	res := a.Reservation()
	meta := gates.EvidenceMeta{
		ID: a.ID(), Scope: a.Scope(), RunID: res.Run, StepID: res.Step, Attempt: a.Attempt(), Digest: string(a.Digest()), Usable: a.Usable(),
		Repository: a.Provenance().RepositoryURL, Commit: a.Provenance().Commit,
	}
	if a.Provenance().Modified() {
		// The producer worked in a scratch workspace that was not the commit it
		// started from (the state the server recorded at its command begin): the
		// artifact does not stand for that commit and so connects to no candidate
		// (DQ-16). It stays a usable artifact; it is just not evidence of an
		// approved commit.
		meta.Repository, meta.Commit = "", ""
	}
	if set := a.Provenance().SourceSet; !set.IsZero() {
		commits := make([]gates.SourceCommit, 0, len(set.Commits()))
		for _, c := range set.Commits() {
			commits = append(commits, gates.SourceCommit{RepositoryID: c.RepositoryID, Commit: c.Commit})
		}
		ref, err := gates.NewSourceSetRef(set.Digest, commits)
		if err != nil {
			return gates.EvidenceMeta{}, fmt.Errorf("%w: the artifact's source set is invalid", gates.ErrEvidence)
		}
		meta.SourceSet = ref
	}
	if size, ok := a.Size(); ok {
		meta.Size = size
	}
	return meta, nil
}

// Read implements gates.Evidence: bounded, verified bytes of a usable file.
func (e *Evidence) Read(ctx context.Context, scope domain.Scope, meta gates.EvidenceMeta, max int64) ([]byte, error) {
	a, err := e.repo.Get(ctx, scope, meta.ID)
	if err != nil {
		return nil, mapArtifact(err, "the artifact")
	}
	if a.Kind() != artifacts.KindFile || !a.Usable() || a.SealedKey() == "" || string(a.Digest()) != meta.Digest {
		return nil, fmt.Errorf("%w: the artifact has no verified file content", gates.ErrEvidence)
	}
	size, _ := a.Size()
	if size < 0 || size > max {
		return nil, fmt.Errorf("%w: the artifact exceeds the read bound", gates.ErrEvidence)
	}
	obj, err := e.objects.Open(ctx, a.SealedKey())
	if err != nil {
		if errors.Is(err, artifacts.ErrObjectNotFound) {
			return nil, fmt.Errorf("%w: the artifact content is absent", gates.ErrEvidence)
		}
		return nil, gates.Unavailablef(err, "the object store is unavailable")
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(io.LimitReader(obj.Body, max+1))
	if err != nil {
		return nil, gates.Unavailablef(err, "the object store is unavailable")
	}
	if int64(len(data)) != size || artifacts.DigestOf(data) != a.Digest() || !bytes.Equal([]byte(a.Digest()), []byte(meta.Digest)) {
		return nil, fmt.Errorf("%w: the artifact bytes do not match their length and digest", gates.ErrEvidence)
	}
	return data, nil
}

// Pins implements gates.Pins over the artifact service. Pinning is idempotent
// and serialized against retention deletion in the repository; a removed
// artifact is not found.
type Pins struct{ repo artifacts.Repository }

var _ gates.Pins = (*Pins)(nil)

// PinOwnerKind is the owner kind of a gate's pins.
const PinOwnerKind = "gate"

// NewPins requires the repository.
func NewPins(r artifacts.Repository) (*Pins, error) {
	if r == nil {
		return nil, errors.New("gates/adapters: an artifact repository is required")
	}
	return &Pins{repo: r}, nil
}

// Pin implements gates.Pins.
func (p *Pins) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, gate gates.ID) error {
	err := p.repo.Pin(ctx, scope, id, artifacts.PinOwner{Kind: PinOwnerKind, ID: string(gate)}, nowUTC())
	return mapArtifact(err, "the artifact")
}
