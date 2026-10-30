package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	// ErrReleaseRetired indicates that a release was retired and cannot be
	// newly selected for admission, though its historical metadata remains readable.
	ErrReleaseRetired = errors.New("artifacts: release is retired")
	// ErrVersionConflict indicates an optimistic concurrency mismatch on mutable metadata.
	ErrVersionConflict = errors.New("artifacts: version conflict")
)

// Release is an immutable named release manifest plus its retention lifecycle.
type Release struct {
	id        domain.ReleaseID
	manifest  artifactsv1.ReleaseManifest
	version   domain.Version
	createdBy Actor
	createdAt time.Time
	updatedAt time.Time
	retiredAt *time.Time
}

// ReleaseRecord is the persisted form of a Release.
type ReleaseRecord struct {
	ID        domain.ReleaseID
	Manifest  artifactsv1.ReleaseManifest
	Version   domain.Version
	CreatedBy Actor
	CreatedAt time.Time
	UpdatedAt time.Time
	RetiredAt *time.Time
}

// NewRelease constructs version 1 of an active release.
func NewRelease(id domain.ReleaseID, m artifactsv1.ReleaseManifest, createdBy Actor, now time.Time) (Release, error) {
	now = now.UTC().Truncate(time.Microsecond)
	return RestoreRelease(ReleaseRecord{
		ID:        id,
		Manifest:  m,
		Version:   1,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// RestoreRelease validates and restores a persisted release record.
func RestoreRelease(rec ReleaseRecord) (Release, error) {
	if err := rec.ID.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if len(rec.Manifest.Canonical()) == 0 || rec.Manifest.Digest().Validate() != nil {
		return Release{}, invalidf("release requires a valid canonical manifest")
	}
	if err := rec.Version.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if rec.CreatedBy.ID == "" || rec.CreatedBy.Type == "" {
		return Release{}, invalidf("release requires createdBy attribution")
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		return Release{}, invalidf("release timestamps are required")
	}
	var ret *time.Time
	if rec.RetiredAt != nil {
		t := rec.RetiredAt.UTC().Truncate(time.Microsecond)
		ret = &t
	}
	return Release{
		id:        rec.ID,
		manifest:  rec.Manifest,
		version:   rec.Version,
		createdBy: rec.CreatedBy,
		createdAt: rec.CreatedAt.UTC().Truncate(time.Microsecond),
		updatedAt: rec.UpdatedAt.UTC().Truncate(time.Microsecond),
		retiredAt: ret,
	}, nil
}

func (r Release) ID() domain.ReleaseID                  { return r.id }
func (r Release) Scope() domain.Scope                   { return r.manifest.Scope() }
func (r Release) Name() string                          { return r.manifest.Name() }
func (r Release) VersionLabel() string                  { return r.manifest.VersionLabel() }
func (r Release) Manifest() artifactsv1.ReleaseManifest { return r.manifest }
func (r Release) ManifestDigest() Digest                { return Digest(r.manifest.Digest()) }
func (r Release) Version() domain.Version               { return r.version }
func (r Release) CreatedBy() Actor                      { return r.createdBy }
func (r Release) CreatedAt() time.Time                  { return r.createdAt }
func (r Release) UpdatedAt() time.Time                  { return r.updatedAt }
func (r Release) RetiredAt() *time.Time                 { return r.retiredAt }
func (r Release) Retired() bool                         { return r.retiredAt != nil }
func (r Release) Record() ReleaseRecord {
	return ReleaseRecord{
		ID: r.id, Manifest: r.manifest, Version: r.version,
		CreatedBy: r.createdBy, CreatedAt: r.createdAt, UpdatedAt: r.updatedAt, RetiredAt: r.retiredAt,
	}
}

// Retire marks an active release retired and increments its concurrency version.
// Retiring an already retired release at the expected version is idempotent.
func (r Release) Retire(expected domain.Version, now time.Time) (Release, error) {
	if err := expected.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if r.version != expected {
		return Release{}, ErrVersionConflict
	}
	if r.retiredAt != nil {
		return r, nil
	}
	n := r
	t := now.UTC().Truncate(time.Microsecond)
	n.retiredAt = &t
	n.updatedAt = t
	n.version++
	return n, nil
}

// ReleasePinOwner returns the deterministic retention-pin owner of a release.
func ReleasePinOwner(id domain.ReleaseID) PinOwner {
	return PinOwner{Kind: "release", ID: string(id)}
}

// ParseReleaseManifest delegates to artifactsv1.ParseReleaseManifest so persistence
// adapters do not import the wire contracts package directly.
func ParseReleaseManifest(canonical []byte) (artifactsv1.ReleaseManifest, error) {
	return artifactsv1.ParseReleaseManifest(canonical)
}

// DestinationKind, DestinationRef, and PublicationReceipt are aliased in the
// domain package so persistence adapters depend only on internal/artifacts.
type (
	DestinationKind    = artifactsv1.DestinationKind
	DestinationRef     = artifactsv1.DestinationRef
	PublicationReceipt = artifactsv1.PublicationReceipt
)

// NewPublicationReceipt constructs a validated PublicationReceipt from primitive
// persisted columns so persistence adapters do not import contracts/.
func NewPublicationReceipt(
	id domain.PublicationID,
	scope domain.Scope,
	artifactID domain.ArtifactID,
	artifactDigest string,
	destKind string,
	destRef string,
	destDigest string,
	runID domain.RunID,
	attemptID domain.AttemptID,
	stepID string,
	completeGraph bool,
	verifiedAt time.Time,
) (PublicationReceipt, error) {
	r := PublicationReceipt{
		ID:             id,
		Scope:          scope,
		ArtifactID:     artifactID,
		ArtifactDigest: runtimev1.Digest(artifactDigest),
		Destination: DestinationRef{
			Kind:      DestinationKind(destKind),
			Reference: destRef,
			Digest:    runtimev1.Digest(destDigest),
		},
		RunID:         runID,
		AttemptID:     attemptID,
		StepID:        stepID,
		CompleteGraph: completeGraph,
		VerifiedAt:    runtimev1.Timestamp(verifiedAt.UTC().Format(time.RFC3339Nano)),
	}
	return r, r.Validate()
}

// ReleaseAuthority authorizes release, input-fetch and publication operations.
type ReleaseAuthority interface {
	AuthorizeReleaseRead(ctx context.Context, caller Caller, scope domain.Scope) error
	AuthorizeReleaseWrite(ctx context.Context, caller Caller, scope domain.Scope) (Actor, error)
	AuthorizeInputFetch(ctx context.Context, caller Caller, attempt domain.AttemptID, req artifactsv1.InputFetchRequest) (domain.Scope, time.Time, error)
	AuthorizePublication(ctx context.Context, caller Caller, attempt domain.AttemptID, artifactID domain.ArtifactID, dest artifactsv1.DestinationRef) (AttemptGrant, error)
}

// PublicationVerifier verifies that an external destination holds the complete,
// byte-verified copy of an artifact.
type PublicationVerifier interface {
	VerifyCopy(ctx context.Context, scope domain.Scope, artifact Artifact, dest artifactsv1.DestinationRef) error
}

// ReleaseStore persists the artifact catalogue, immutable releases and
// publication receipts.
type ReleaseStore interface {
	ListArtifacts(ctx context.Context, scope domain.Scope, after domain.ArtifactID, limit int) ([]Artifact, error)
	CreateRelease(ctx context.Context, rel Release, requestID string) (Release, bool, error)
	GetRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID) (Release, error)
	FindRelease(ctx context.Context, scope domain.Scope, name, versionLabel string) (Release, error)
	ListReleases(ctx context.Context, scope domain.Scope, nameFilter string, after domain.ReleaseID, limit int) ([]Release, error)
	RetireRelease(ctx context.Context, scope domain.Scope, id domain.ReleaseID, expected domain.Version, actor Actor, requestID string, now time.Time) (Release, error)
	SavePublication(ctx context.Context, pub artifactsv1.PublicationReceipt, actor Actor, requestID string) (artifactsv1.PublicationReceipt, bool, error)
	ListPublications(ctx context.Context, scope domain.Scope, artifactID domain.ArtifactID, after domain.PublicationID, limit int) ([]artifactsv1.PublicationReceipt, error)
}

func (RandomIDs) NewReleaseID() domain.ReleaseID { return domain.ReleaseID("rel_" + randomHex(16)) }
func (RandomIDs) NewPublicationID() domain.PublicationID {
	return domain.PublicationID("pub_" + randomHex(16))
}

// ListArtifacts returns up to limit usable or reserved artifacts in the scope.
func (s *Service) ListArtifacts(ctx context.Context, caller Caller, scope domain.Scope, after domain.ArtifactID, limit int, rs ReleaseStore, auth ReleaseAuthority) ([]Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if after != "" && after.Validate() != nil {
		return nil, invalidf("invalid cursor")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if rs == nil || auth == nil {
		return nil, ErrUnavailable
	}
	if err := auth.AuthorizeReleaseRead(ctx, caller, scope); err != nil {
		return nil, classifyAuthErr(err)
	}
	items, err := rs.ListArtifacts(ctx, scope, after, limit)
	if err != nil {
		return nil, s.dependency("list artifacts", err)
	}
	return items, nil
}

// CreateRelease validates and creates an immutable release manifest and pins all
// referenced artifacts against retention deletion. Identical requests for the
// same (scope, name, versionLabel) return the existing release; different
// manifest contents return ErrIdempotencyConflict.
func (s *Service) CreateRelease(ctx context.Context, caller Caller, in artifactsv1.ReleaseManifestInput, requestID string, rs ReleaseStore, auth ReleaseAuthority) (Release, bool, error) {
	if rs == nil || auth == nil {
		return Release{}, false, ErrUnavailable
	}
	if err := in.Scope.Validate(); err != nil {
		return Release{}, false, invalidf("%v", err)
	}
	actor, err := auth.AuthorizeReleaseWrite(ctx, caller, in.Scope)
	if err != nil {
		return Release{}, false, classifyAuthErr(err)
	}
	manifest, err := artifactsv1.NewReleaseManifest(in)
	if err != nil {
		return Release{}, false, invalidf("%v", err)
	}
	relID := domain.ReleaseID("rel_" + randomHex(16))
	if rid, ok := s.cfg.IDs.(interface{ NewReleaseID() domain.ReleaseID }); ok {
		relID = rid.NewReleaseID()
	}
	rel, err := NewRelease(relID, manifest, actor, s.clock())
	if err != nil {
		return Release{}, false, err
	}
	out, created, err := rs.CreateRelease(ctx, rel, requestID)
	if err != nil {
		return Release{}, false, s.releaseDependency("create release", err)
	}
	return out, created, nil
}

// GetRelease reads a release by ID (including retired releases).
func (s *Service) GetRelease(ctx context.Context, caller Caller, scope domain.Scope, id domain.ReleaseID, rs ReleaseStore, auth ReleaseAuthority) (Release, error) {
	if rs == nil || auth == nil {
		return Release{}, ErrUnavailable
	}
	if err := scope.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if err := auth.AuthorizeReleaseRead(ctx, caller, scope); err != nil {
		return Release{}, classifyAuthErr(err)
	}
	rel, err := rs.GetRelease(ctx, scope, id)
	if err != nil {
		return Release{}, s.releaseDependency("get release", err)
	}
	return rel, nil
}

// ResolveRelease resolves an active release by (name, versionLabel) for run
// admission or deployment selection. Retired releases return ErrReleaseRetired.
func (s *Service) ResolveRelease(ctx context.Context, caller Caller, scope domain.Scope, name, versionLabel string, rs ReleaseStore, auth ReleaseAuthority) (Release, error) {
	if rs == nil || auth == nil {
		return Release{}, ErrUnavailable
	}
	if err := scope.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if name == "" || versionLabel == "" {
		return Release{}, invalidf("release name and versionLabel are required")
	}
	if err := auth.AuthorizeReleaseRead(ctx, caller, scope); err != nil {
		return Release{}, classifyAuthErr(err)
	}
	rel, err := rs.FindRelease(ctx, scope, name, versionLabel)
	if err != nil {
		return Release{}, s.releaseDependency("resolve release", err)
	}
	if rel.Retired() {
		return Release{}, ErrReleaseRetired
	}
	// Verify every referenced artifact is still usable in the scope.
	for entryName, entry := range rel.Manifest().Entries() {
		a, err := s.cfg.Repository.Get(ctx, scope, entry.ArtifactID)
		if err != nil {
			return Release{}, s.releaseDependency("verify release entry "+entryName, err)
		}
		if !a.Usable() || string(a.Digest()) != string(entry.Digest) {
			return Release{}, fmt.Errorf("%w: release artifact %s is no longer available", ErrIncomplete, entry.ArtifactID)
		}
	}
	return rel, nil
}

// ListReleases lists releases of a project, optionally filtered by release name.
func (s *Service) ListReleases(ctx context.Context, caller Caller, scope domain.Scope, nameFilter string, after domain.ReleaseID, limit int, rs ReleaseStore, auth ReleaseAuthority) ([]Release, error) {
	if rs == nil || auth == nil {
		return nil, ErrUnavailable
	}
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if after != "" && after.Validate() != nil {
		return nil, invalidf("invalid cursor")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if err := auth.AuthorizeReleaseRead(ctx, caller, scope); err != nil {
		return nil, classifyAuthErr(err)
	}
	items, err := rs.ListReleases(ctx, scope, nameFilter, after, limit)
	if err != nil {
		return nil, s.releaseDependency("list releases", err)
	}
	return items, nil
}

// RetireRelease marks a release retired under optimistic concurrency and releases
// only that release's artifact retention pins.
func (s *Service) RetireRelease(ctx context.Context, caller Caller, scope domain.Scope, id domain.ReleaseID, expected domain.Version, requestID string, rs ReleaseStore, auth ReleaseAuthority) (Release, error) {
	if rs == nil || auth == nil {
		return Release{}, ErrUnavailable
	}
	if err := scope.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	if err := expected.Validate(); err != nil {
		return Release{}, invalidf("%v", err)
	}
	actor, err := auth.AuthorizeReleaseWrite(ctx, caller, scope)
	if err != nil {
		return Release{}, classifyAuthErr(err)
	}
	rel, err := rs.RetireRelease(ctx, scope, id, expected, actor, requestID, s.clock())
	if err != nil {
		return Release{}, s.releaseDependency("retire release", err)
	}
	return rel, nil
}

// FetchInput authorizes and returns verified metadata and an optional short-lived
// download instruction for an attempt's admitted artifact input.
func (s *Service) FetchInput(ctx context.Context, caller Caller, attempt domain.AttemptID, req artifactsv1.InputFetchRequest, auth ReleaseAuthority) (artifactsv1.InputFetchResponse, error) {
	if auth == nil {
		return artifactsv1.InputFetchResponse{}, ErrUnavailable
	}
	if err := attempt.Validate(); err != nil {
		return artifactsv1.InputFetchResponse{}, invalidf("%v", err)
	}
	if err := req.Validate(); err != nil {
		return artifactsv1.InputFetchResponse{}, invalidf("%v", err)
	}
	now := s.clock()
	scope, grantExpiresAt, err := auth.AuthorizeInputFetch(ctx, caller, attempt, req)
	if err != nil {
		return artifactsv1.InputFetchResponse{}, classifyAuthErr(err)
	}
	if !grantExpiresAt.After(now) {
		return artifactsv1.InputFetchResponse{}, ErrAttemptInactive
	}
	a, err := s.cfg.Repository.Get(ctx, scope, req.ArtifactID)
	if err != nil {
		return artifactsv1.InputFetchResponse{}, s.dependency("load input artifact", err)
	}
	if !a.Usable() {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: input artifact is not usable", ErrIncomplete)
	}
	if string(a.Digest()) != string(req.Digest) {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: input artifact digest does not match admitted pin", ErrDigestMismatch)
	}
	resp := artifactsv1.InputFetchResponse{
		Input:      req.Input,
		ArtifactID: a.ID(),
		Kind:       runtimev1.ArtifactKind(a.Kind()),
		Digest:     runtimev1.Digest(a.Digest()),
	}
	if mt := a.MediaType(); mt != "" {
		resp.MediaType = &mt
	}
	if sz, ok := a.Size(); ok {
		resp.SizeBytes = &sz
	}
	if reg := a.Registry(); reg != "" {
		resp.Registry = &reg
	}
	if !req.Download {
		return resp, nil
	}
	if a.Kind() != KindFile || a.SealedKey() == "" {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: only file inputs support byte download", ErrInvalid)
	}
	expires := now.Add(s.cfg.DownloadTTL)
	if grantExpiresAt.Before(expires) {
		expires = grantExpiresAt
	}
	expires = expires.UTC().Truncate(time.Microsecond)
	if err := s.cfg.Repository.RecordTransfer(ctx, scope, a.ID(), expires, now); err != nil {
		return artifactsv1.InputFetchResponse{}, s.dependency("record input transfer", err)
	}
	tr, err := s.cfg.Objects.PresignDownload(ctx, DownloadSpec{
		Key:       a.SealedKey(),
		MediaType: a.MediaType(),
		ExpiresAt: expires,
	})
	if err != nil {
		return artifactsv1.InputFetchResponse{}, unavailable("presign input download", err)
	}
	headers := map[string]string{}
	for k, v := range tr.Headers {
		headers[k] = v
	}
	resp.Download = &runtimev1.Upload{
		Method:    tr.Method,
		URL:       tr.URL,
		Headers:   headers,
		ExpiresAt: runtimev1.Timestamp(tr.ExpiresAt.UTC().Format(time.RFC3339Nano)),
	}
	return resp, nil
}

// RecordPublication verifies a destination copy outside any SQL transaction,
// reauthorizes the attempt, and records an idempotent PublicationReceipt.
func (s *Service) RecordPublication(ctx context.Context, caller Caller, attempt domain.AttemptID, artifactID domain.ArtifactID, dest artifactsv1.DestinationRef, requestID string, rs ReleaseStore, auth ReleaseAuthority, verifier PublicationVerifier) (artifactsv1.PublicationReceipt, bool, error) {
	if rs == nil || auth == nil || verifier == nil {
		return artifactsv1.PublicationReceipt{}, false, ErrUnavailable
	}
	if err := attempt.Validate(); err != nil {
		return artifactsv1.PublicationReceipt{}, false, invalidf("%v", err)
	}
	if err := artifactID.Validate(); err != nil {
		return artifactsv1.PublicationReceipt{}, false, invalidf("%v", err)
	}
	if err := dest.Validate(); err != nil {
		return artifactsv1.PublicationReceipt{}, false, invalidf("%v", err)
	}
	g, err := auth.AuthorizePublication(ctx, caller, attempt, artifactID, dest)
	if err != nil {
		return artifactsv1.PublicationReceipt{}, false, classifyAuthErr(err)
	}
	if !g.ExpiresAt.After(s.clock()) {
		return artifactsv1.PublicationReceipt{}, false, ErrAttemptInactive
	}
	a, err := s.cfg.Repository.Get(ctx, g.Scope, artifactID)
	if err != nil {
		return artifactsv1.PublicationReceipt{}, false, s.dependency("load published artifact", err)
	}
	if !a.Usable() {
		return artifactsv1.PublicationReceipt{}, false, fmt.Errorf("%w: cannot publish an incomplete or removed artifact", ErrIncomplete)
	}
	if string(a.Digest()) != string(dest.Digest) {
		return artifactsv1.PublicationReceipt{}, false, fmt.Errorf("%w: destination digest differs from source artifact; register a derived artifact instead", ErrDigestMismatch)
	}
	// Verify the destination copy outside any SQL transaction.
	if err := verifier.VerifyCopy(ctx, g.Scope, a, dest); err != nil {
		return artifactsv1.PublicationReceipt{}, false, err
	}
	// Reauthorize after the network verification so a revoked/cancelled attempt
	// cannot commit a receipt.
	g2, err := auth.AuthorizePublication(ctx, caller, attempt, artifactID, dest)
	if err != nil {
		return artifactsv1.PublicationReceipt{}, false, classifyAuthErr(err)
	}
	now := s.clock()
	if !g2.ExpiresAt.After(now) {
		return artifactsv1.PublicationReceipt{}, false, ErrAttemptInactive
	}
	pubID := domain.PublicationID("pub_" + randomHex(16))
	if pid, ok := s.cfg.IDs.(interface{ NewPublicationID() domain.PublicationID }); ok {
		pubID = pid.NewPublicationID()
	}
	receipt := artifactsv1.PublicationReceipt{
		ID:             pubID,
		Scope:          g2.Scope,
		ArtifactID:     a.ID(),
		ArtifactDigest: runtimev1.Digest(a.Digest()),
		Destination:    dest,
		RunID:          g2.Run,
		AttemptID:      g2.Attempt,
		StepID:         g2.Step,
		CompleteGraph:  true,
		VerifiedAt:     runtimev1.Timestamp(now.UTC().Format(time.RFC3339Nano)),
	}
	if err := receipt.Validate(); err != nil {
		return artifactsv1.PublicationReceipt{}, false, invalidf("%v", err)
	}
	saved, created, err := rs.SavePublication(ctx, receipt, workloadActor(g2), requestID)
	if err != nil {
		return artifactsv1.PublicationReceipt{}, false, s.releaseDependency("save publication receipt", err)
	}
	return saved, created, nil
}

func (s *Service) releaseDependency(what string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInProgress), errors.Is(err, ErrInvalid),
		errors.Is(err, ErrInconsistent), errors.Is(err, ErrIdempotencyConflict),
		errors.Is(err, ErrVersionConflict), errors.Is(err, ErrOutputInvalid),
		errors.Is(err, ErrDigestMismatch), errors.Is(err, ErrIncomplete),
		errors.Is(err, ErrReleaseRetired):
		return err
	}
	return unavailable(what, err)
}

// ListPublications lists verified publication receipts for an artifact.
func (s *Service) ListPublications(ctx context.Context, caller Caller, scope domain.Scope, artifactID domain.ArtifactID, after domain.PublicationID, limit int, rs ReleaseStore, auth ReleaseAuthority) ([]artifactsv1.PublicationReceipt, error) {
	if rs == nil || auth == nil {
		return nil, ErrUnavailable
	}
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := artifactID.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if after != "" && after.Validate() != nil {
		return nil, invalidf("invalid cursor")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if err := auth.AuthorizeReleaseRead(ctx, caller, scope); err != nil {
		return nil, classifyAuthErr(err)
	}
	items, err := rs.ListPublications(ctx, scope, artifactID, after, limit)
	if err != nil {
		return nil, s.dependency("list publications", err)
	}
	return items, nil
}

// VerifyFileStream hashes r up to wantSize bytes and verifies that both size
// and SHA-256 digest equal the expected values. ETag alone is never sufficient.
func VerifyFileStream(r io.Reader, wantSize int64, wantDigest Digest) error {
	if wantSize < 0 || wantSize > MaxFileBytes {
		return invalidf("invalid expected size %d", wantSize)
	}
	h := sha256.New()
	limited := io.LimitReader(r, wantSize+1)
	n, err := io.Copy(h, limited)
	if err != nil {
		return unavailable("read destination object", err)
	}
	if n != wantSize {
		return fmt.Errorf("%w: destination size %d differs from expected %d", ErrIncomplete, n, wantSize)
	}
	if got := DigestOfSum(h.Sum(nil)); got != wantDigest {
		return fmt.Errorf("%w: destination bytes hash to %s, want %s", ErrDigestMismatch, got, wantDigest)
	}
	return nil
}

func classifyAuthErr(err error) error {
	switch {
	case errors.Is(err, ErrAuthorityMissing), errors.Is(err, ErrAttemptInactive),
		errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden),
		errors.Is(err, ErrInvalid), errors.Is(err, ErrVersionConflict),
		errors.Is(err, ErrIdempotencyConflict):
		return err
	default:
		return unavailable("authority", err)
	}
}

func sameReleaseManifest(a, b artifactsv1.ReleaseManifest) bool {
	return a.Digest() == b.Digest() && bytes.Equal(a.Canonical(), b.Canonical())
}
