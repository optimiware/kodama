package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// WriterConfig configures the controller's internal artifact writer.
//
// The controller has no runtime grant: the artifact module's reserve/complete
// operations correctly refuse an expired or revoked attempt, and the fallback
// exists precisely for attempts whose wrapper could not finalize. This adapter
// therefore uses the artifact owner's exported primitives (NewFile, the
// repository's reservation and completion claims, recorded object destinations,
// verified object writes and CommitUsable) with a system audit actor. It is
// restricted to controller log captures of an attempt resolved by the execution
// module: fixed artifact names, the diagnostic role and fixed media types.
type WriterConfig struct {
	Repository artifacts.Repository
	Objects    artifacts.ObjectStore
	IDs        artifacts.IDs
	Clock      func() time.Time
	// Limits bound the attempt's diagnostic artifacts (bytes and file count). The
	// composition supplies the attempt's execution limit; both are required.
	Limits artifacts.Limits
	// UploadTTL is the horizon within which a staging write is recorded as
	// possible, so cleanup never deletes an in-flight object (default 10m).
	UploadTTL time.Duration
	// ClaimLease bounds one sealing attempt (default 30s).
	ClaimLease time.Duration
	// SpoolDir holds the verification copy ("" selects os.TempDir).
	SpoolDir string
}

// Actor attributes the controller's artifact transitions in the audit trail.
var systemActor = artifacts.Actor{Type: "system", ID: "logs-controller"}

var controllerLogName = regexp.MustCompile(`^controller-g[1-9]\.log$`)

// Retained implements logs.Retained: the read side (Artifacts) plus the
// controller's writer.
type Retained struct {
	*Artifacts
	w WriterConfig
}

var _ logs.Retained = (*Retained)(nil)

// NewRetained validates the writer configuration.
func NewRetained(reader *Artifacts, cfg WriterConfig) (*Retained, error) {
	if reader == nil || cfg.Repository == nil || cfg.Objects == nil {
		return nil, errors.New("logs/adapters: a reader, repository and object store are required")
	}
	if cfg.Limits.DiagnosticBytes <= 0 || cfg.Limits.DiagnosticFiles <= 0 {
		return nil, errors.New("logs/adapters: diagnostic limits are required")
	}
	if cfg.IDs == nil {
		cfg.IDs = artifacts.RandomIDs{}
	}
	if cfg.UploadTTL <= 0 {
		cfg.UploadTTL = 10 * time.Minute
	}
	if cfg.ClaimLease <= 0 {
		cfg.ClaimLease = 30 * time.Second
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = os.TempDir()
	}
	return &Retained{Artifacts: reader, w: cfg}, nil
}

func (r *Retained) now() time.Time {
	if r.w.Clock != nil {
		return r.w.Clock().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Store implements logs.Retained for one frozen controller capture.
func (r *Retained) Store(ctx context.Context, info logs.AttemptInfo, snap logs.Snapshot, body io.ReadSeeker) (logs.Link, error) {
	if info.Scope.Validate() != nil || info.ID.Validate() != nil || info.Run.Validate() != nil || info.Step == "" {
		return logs.Link{}, logs.ErrInvalid
	}
	wantMeta, err := contract.MetadataName(snap.LogName)
	if !controllerLogName.MatchString(snap.LogName) || err != nil || wantMeta != snap.MetaName {
		return logs.Link{}, fmt.Errorf("%w: not a controller capture name", logs.ErrInvalid)
	}
	logArt, err := r.ensure(ctx, info, fileSpec{name: snap.LogName, media: contract.LogMediaType, digest: artifacts.Digest(snap.Digest), size: snap.Size}, body)
	if err != nil {
		return logs.Link{}, err
	}
	doc := contract.Metadata{Version: contract.Version, Source: contract.SourceController, LogName: snap.LogName,
		Log:          contract.Ref{ArtifactID: logArt.ID(), Digest: snap.Digest, SizeBytes: snap.Size},
		Completeness: snap.Completeness, Reasons: snap.Reasons}
	data, err := contract.Encode(doc)
	if err != nil {
		return logs.Link{}, fmt.Errorf("%w: %v", logs.ErrInvalid, err)
	}
	// The sidecar's bytes are derived from the frozen snapshot and the log's
	// artifact ID, so they are reproducible after a restart.
	metaArt, err := r.ensure(ctx, info, fileSpec{name: snap.MetaName, media: contract.MediaType, digest: artifacts.DigestOf(data), size: int64(len(data))}, bytes.NewReader(data))
	if err != nil {
		return logs.Link{}, err
	}
	meta := refOf(metaArt)
	return logs.Link{Log: refOf(logArt), Meta: &meta, Statement: snap.Statement}, nil
}

type fileSpec struct {
	name   string
	media  string
	digest artifacts.Digest
	size   int64
}

func (s fileSpec) matches(a artifacts.Artifact) bool {
	size, _ := a.Size()
	return a.Kind() == artifacts.KindFile && a.Role() == artifacts.RoleDiagnostic && a.Name() == s.name &&
		a.Digest() == s.digest && size == s.size && a.MediaType() == s.media
}

// ensure reserves (or recovers), stages, seals and commits one diagnostic file.
// Every step is repeatable after an interruption with the same declaration.
func (r *Retained) ensure(ctx context.Context, info logs.AttemptInfo, spec fileSpec, body io.ReadSeeker) (artifacts.Artifact, error) {
	repo := r.w.Repository
	art, err := repo.FindByName(ctx, info.Scope, info.ID, spec.name)
	switch {
	case err == nil:
		if !spec.matches(art) {
			// Same name, different content: never overwritten, never reused.
			return artifacts.Artifact{}, logs.ErrConflict
		}
	case errors.Is(err, artifacts.ErrNotFound):
		if body == nil {
			return artifacts.Artifact{}, logs.ErrFrozenBytesLost // nothing reserved, nothing uploaded
		}
		if art, err = r.reserve(ctx, info, spec); err != nil {
			return artifacts.Artifact{}, err
		}
	default:
		return artifacts.Artifact{}, logs.Unavailablef(err, "artifact lookup")
	}
	if art.Usable() {
		return art, nil
	}
	if !art.Open() {
		// Abandoned by cleanup (its staging horizon passed unfinished): this
		// reservation can never complete and its name cannot be reused.
		return artifacts.Artifact{}, logs.ErrFrozenBytesLost
	}
	if err := r.stage(ctx, art, spec, body); err != nil {
		return artifacts.Artifact{}, err
	}
	return r.seal(ctx, info, art, spec)
}

func (r *Retained) reserve(ctx context.Context, info logs.AttemptInfo, spec fileSpec) (artifacts.Artifact, error) {
	now := r.now()
	id := r.w.IDs.NewArtifactID()
	size := spec.size
	res := artifacts.Reservation{Scope: info.Scope, Attempt: info.ID, Run: info.Run, Step: info.Step, Name: spec.name,
		Kind: artifacts.KindFile, Role: artifacts.RoleDiagnostic, Digest: spec.digest, MediaType: spec.media, Size: &size}
	if err := res.Validate(); err != nil {
		return artifacts.Artifact{}, fmt.Errorf("%w: %v", logs.ErrInvalid, err)
	}
	key := artifacts.ObjectKey(info.Scope, id, artifacts.SegmentStaging, r.w.IDs.NewToken())
	a, err := artifacts.NewFile(id, res, key, now.Add(r.w.UploadTTL), now)
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("%w: %v", logs.ErrInvalid, err)
	}
	out, err := r.w.Repository.Reserve(ctx, artifacts.ReserveCommand{Artifact: a, Limits: r.w.Limits, Actor: systemActor, Now: now})
	if err != nil {
		if errors.Is(err, artifacts.ErrLimitExceeded) {
			return artifacts.Artifact{}, fmt.Errorf("%w: diagnostic artifact limit", logs.ErrLimit)
		}
		return artifacts.Artifact{}, logs.Unavailablef(err, "reserve log artifact")
	}
	if !spec.matches(out.Artifact) {
		return artifacts.Artifact{}, logs.ErrConflict
	}
	return out.Artifact, nil
}

// stage makes sure the frozen bytes are in the artifact's staging object. With
// no body (a restart) the object must already exist.
func (r *Retained) stage(ctx context.Context, art artifacts.Artifact, spec fileSpec, body io.ReadSeeker) error {
	obj, err := r.w.Objects.Open(ctx, art.StagingKey())
	switch {
	case err == nil:
		defer obj.Body.Close()
		if obj.Size != spec.size {
			return logs.ErrFrozenBytesLost // different bytes already sit at the immutable key
		}
		return nil
	case !errors.Is(err, artifacts.ErrObjectNotFound):
		return logs.Unavailablef(err, "read staging object")
	}
	if body == nil {
		return logs.ErrFrozenBytesLost
	}
	now := r.now()
	// The write horizon is durable before the object can exist.
	if err := r.w.Repository.RecordTransfer(ctx, art.Scope(), art.ID(), now.Add(r.w.UploadTTL), now); err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return logs.ErrFrozenBytesLost
		}
		return logs.Unavailablef(err, "record transfer")
	}
	if err := r.w.Objects.Put(ctx, artifacts.PutSpec{Key: art.StagingKey(), Body: body, Size: spec.size, Digest: spec.digest, MediaType: spec.media}); err != nil {
		// An unknown outcome (a lost response) is resolved by the next call,
		// which finds the object or uploads again.
		if o, oerr := r.w.Objects.Open(ctx, art.StagingKey()); oerr == nil {
			o.Body.Close()
			return nil
		}
		return logs.Unavailablef(err, "stage log")
	}
	return nil
}

// seal verifies the staged bytes, copies them to a recorded sealed destination
// and commits the artifact usable under a fenced claim.
func (r *Retained) seal(ctx context.Context, info logs.AttemptInfo, art artifacts.Artifact, spec fileSpec) (artifacts.Artifact, error) {
	repo := r.w.Repository
	now := r.now()
	dest := artifacts.ObjectKey(info.Scope, art.ID(), artifacts.SegmentSealed, r.w.IDs.NewToken())
	cr, err := repo.BeginCompletion(ctx, artifacts.BeginCompletion{Scope: info.Scope, Attempt: info.ID, ID: art.ID(), Token: r.w.IDs.NewToken(),
		Destination: dest, Lease: r.w.ClaimLease, Now: now})
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return artifacts.Artifact{}, logs.ErrFrozenBytesLost
		}
		return artifacts.Artifact{}, logs.Unavailablef(err, "begin completion") // includes a live competing claim
	}
	if cr.Claim == nil {
		return cr.Artifact, nil
	}
	claim := *cr.Claim
	ev, err := r.verifyAndSeal(ctx, art, dest, claim, spec)
	if err != nil {
		if errors.Is(err, logs.ErrFrozenBytesLost) {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_ = repo.ReleaseClaim(rctx, info.Scope, art.ID(), claim.Token)
			cancel()
		}
		return artifacts.Artifact{}, err
	}
	done, err := repo.CommitUsable(ctx, artifacts.CommitCommand{Scope: info.Scope, Attempt: info.ID, ID: art.ID(), Claim: claim, Destination: dest,
		Verification: ev, Actor: systemActor, Now: r.now()})
	if err == nil {
		return done, nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if cur, gerr := repo.GetForAttempt(rctx, info.Scope, info.ID, art.ID()); gerr == nil && cur.Usable() {
		return cur, nil // this commit, or an identical concurrent one, won
	}
	return artifacts.Artifact{}, logs.Unavailablef(err, "commit log artifact")
}

func (r *Retained) verifyAndSeal(ctx context.Context, art artifacts.Artifact, dest string, claim artifacts.Claim, spec fileSpec) (artifacts.Verification, error) {
	lctx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	obj, err := r.w.Objects.Open(lctx, art.StagingKey())
	if err != nil {
		if errors.Is(err, artifacts.ErrObjectNotFound) {
			return artifacts.Verification{}, logs.ErrFrozenBytesLost
		}
		return artifacts.Verification{}, logs.Unavailablef(err, "read staging object")
	}
	defer obj.Body.Close()
	if obj.Size != spec.size {
		return artifacts.Verification{}, logs.ErrFrozenBytesLost
	}
	f, err := os.CreateTemp(r.w.SpoolDir, "kodama-logseal-*")
	if err != nil {
		return artifacts.Verification{}, logs.Unavailablef(err, "scratch space")
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(obj.Body, spec.size+1))
	if err != nil {
		return artifacts.Verification{}, logs.Unavailablef(err, "read staging object")
	}
	if n != spec.size || artifacts.DigestOfSum(h.Sum(nil)) != spec.digest {
		return artifacts.Verification{}, logs.ErrFrozenBytesLost // the staged bytes are not the frozen ones
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return artifacts.Verification{}, logs.Unavailablef(err, "scratch space")
	}
	if err := r.w.Objects.Put(lctx, artifacts.PutSpec{Key: dest, Body: f, Size: spec.size, Digest: spec.digest, MediaType: spec.media}); err != nil {
		return artifacts.Verification{}, logs.Unavailablef(err, "seal log")
	}
	return artifacts.Verification{Digest: spec.digest, Size: spec.size, At: r.now()}, nil
}
