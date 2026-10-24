package adapters

import (
	"context"
	"errors"
	"io"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// Artifacts is the read side of logs.Retained over the artifact module's
// repository and object store. It validates every linked artifact as a scoped,
// same-attempt, usable diagnostic file and binds a completeness sidecar to its
// log by ID, digest and size. It never exposes object keys or transfer URLs:
// bytes are opened server-side after the caller's current authorization.
type Artifacts struct {
	repo    artifacts.Repository
	objects artifacts.ObjectStore
}

// NewArtifacts requires both collaborators.
func NewArtifacts(repo artifacts.Repository, objects artifacts.ObjectStore) (*Artifacts, error) {
	if repo == nil || objects == nil {
		return nil, errors.New("logs/adapters: a repository and an object store are required")
	}
	return &Artifacts{repo: repo, objects: objects}, nil
}

func repoErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifacts.ErrNotFound):
		return logs.ErrNotFound
	}
	return logs.Unavailablef(err, "artifact lookup")
}

// usableLog loads the artifact and requires it to be a usable, active,
// diagnostic file of the attempt.
func (a *Artifacts) usableLog(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (artifacts.Artifact, error) {
	art, err := a.repo.GetForAttempt(ctx, scope, attempt, id)
	if err != nil {
		return artifacts.Artifact{}, repoErr(err)
	}
	return checkLog(art, scope, attempt)
}

func checkLog(art artifacts.Artifact, scope domain.Scope, attempt domain.AttemptID) (artifacts.Artifact, error) {
	if art.Scope() != scope || art.Attempt() != attempt || art.Kind() != artifacts.KindFile || art.Role() != artifacts.RoleDiagnostic {
		return artifacts.Artifact{}, logs.ErrNotFound
	}
	if !art.Usable() || art.State() != artifacts.StateUsable || art.SealedKey() == "" {
		return artifacts.Artifact{}, logs.ErrNoLog // reserved, abandoned or removed: no bytes
	}
	return art, nil
}

func refOf(art artifacts.Artifact) logs.ArtifactRef {
	size, _ := art.Size()
	return logs.ArtifactRef{ID: art.ID(), Digest: string(art.Digest()), Size: size}
}

// Open implements logs.Retained.
func (a *Artifacts) Open(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ref logs.ArtifactRef) (io.ReadCloser, error) {
	art, err := a.usableLog(ctx, scope, attempt, ref.ID)
	if err != nil {
		return nil, err
	}
	if got := refOf(art); got != ref {
		return nil, logs.ErrNotFound // the record names different content than the artifact holds
	}
	obj, err := a.objects.Open(ctx, art.SealedKey())
	if err != nil {
		if errors.Is(err, artifacts.ErrObjectNotFound) {
			return nil, logs.ErrNoLog
		}
		return nil, logs.Unavailablef(err, "read retained log")
	}
	if obj.Size != ref.Size {
		_ = obj.Body.Close()
		return nil, logs.Unavailablef(nil, "retained log size")
	}
	return &limited{Reader: io.LimitReader(obj.Body, ref.Size), c: obj.Body}, nil
}

type limited struct {
	io.Reader
	c io.Closer
}

func (l *limited) Close() error { return l.c.Close() }

// Usable implements logs.Retained.
func (a *Artifacts) Usable(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ref logs.ArtifactRef) (bool, error) {
	art, err := a.repo.GetForAttempt(ctx, scope, attempt, ref.ID)
	if err != nil {
		return false, repoErr(err)
	}
	return art.Scope() == scope && art.Usable(), nil
}

// WrapperLog implements logs.Retained: the wrapper's `command.log` and, when it
// is valid and bound to that exact artifact, its sidecar. Legacy logs and logs
// with an unusable, unreadable or mismatched sidecar have unknown completeness;
// a sidecar is never trusted for an artifact it does not describe.
func (a *Artifacts) WrapperLog(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (logs.Link, error) {
	logArt, err := a.repo.FindByName(ctx, scope, attempt, contract.CommandLogName)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return logs.Link{}, logs.ErrNoLog
		}
		return logs.Link{}, repoErr(err)
	}
	logArt, err = checkLog(logArt, scope, attempt)
	if err != nil {
		return logs.Link{}, err
	}
	link := logs.Link{Log: refOf(logArt)}
	link.Completeness, link.Reasons = contract.Unverified()

	metaName, _ := contract.MetadataName(contract.CommandLogName)
	metaArt, err := a.repo.FindByName(ctx, scope, attempt, metaName)
	switch {
	case err == nil:
	case errors.Is(err, artifacts.ErrNotFound):
		return link, nil
	default:
		return logs.Link{}, repoErr(err)
	}
	metaArt, err = checkLog(metaArt, scope, attempt)
	if err != nil {
		if errors.Is(err, logs.ErrNoLog) || errors.Is(err, logs.ErrNotFound) {
			return link, nil // an unusable sidecar states nothing
		}
		return logs.Link{}, err
	}
	doc, err := a.readSidecar(ctx, metaArt)
	if err != nil {
		if errors.Is(err, logs.ErrUnavailable) {
			return logs.Link{}, err
		}
		return link, nil
	}
	if doc.Source != contract.SourceWrapper || doc.LogName != contract.CommandLogName ||
		doc.Bind(logArt.ID(), string(logArt.Digest()), link.Log.Size) != nil {
		return link, nil
	}
	meta := refOf(metaArt)
	link.Meta, link.Completeness, link.Reasons = &meta, doc.Completeness, append([]logs.Reason(nil), doc.Reasons...)
	return link, nil
}

var errBadSidecar = errors.New("logs/adapters: sidecar is not valid")

func (a *Artifacts) readSidecar(ctx context.Context, art artifacts.Artifact) (contract.Metadata, error) {
	size, _ := art.Size()
	if size > contract.MaxMetadataBytes || art.MediaType() != contract.MediaType {
		return contract.Metadata{}, errBadSidecar
	}
	obj, err := a.objects.Open(ctx, art.SealedKey())
	if err != nil {
		if errors.Is(err, artifacts.ErrObjectNotFound) {
			return contract.Metadata{}, errBadSidecar
		}
		return contract.Metadata{}, logs.Unavailablef(err, "read sidecar")
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(io.LimitReader(obj.Body, contract.MaxMetadataBytes+1))
	if err != nil {
		return contract.Metadata{}, logs.Unavailablef(err, "read sidecar")
	}
	if artifacts.DigestOf(data) != art.Digest() {
		return contract.Metadata{}, errBadSidecar
	}
	doc, err := contract.Decode(data)
	if err != nil {
		return contract.Metadata{}, errBadSidecar
	}
	return doc, nil
}
