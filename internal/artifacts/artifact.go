package artifacts

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Reservation is the validated, normalized description of the artifact a
// producing attempt asked to keep. Scope, producer and provenance come from the
// attempt's trusted authority; the rest is the caller's immutable declaration.
type Reservation struct {
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Run        domain.RunID
	Step       string
	Name       string
	Kind       Kind
	Role       Role
	Digest     Digest
	MediaType  string // files only
	Size       *int64 // files only
	Registry   string // OCI only: canonical repository, no tag
	Provenance Provenance
}

// Validate checks the declaration against the artifact rules. A file declares a
// media type and size; an OCI registration declares a registry and is always an
// output.
func (r Reservation) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := r.Attempt.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := r.Run.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if r.Step == "" || len(r.Step) > 128 {
		return invalidf("step must be 1..128 bytes")
	}
	if !nameRe.MatchString(r.Name) {
		return invalidf("name must match the safe name syntax")
	}
	if err := r.Digest.Validate(); err != nil {
		return err
	}
	if err := r.Provenance.Validate(); err != nil {
		return err
	}
	switch r.Role {
	case RoleOutput, RoleDiagnostic, RoleBundle:
	default:
		return invalidf("unknown role %q", r.Role)
	}
	switch r.Kind {
	case KindFile:
		if r.Size == nil || *r.Size < 0 || *r.Size > MaxFileBytes || !mediaRe.MatchString(r.MediaType) || r.Registry != "" {
			return invalidf("a file needs a media type, a size within 0..%d and no registry", MaxFileBytes)
		}
	case KindOCI:
		if r.Role != RoleOutput || r.Registry == "" || !registryRe.MatchString(r.Registry) || r.MediaType != "" || r.Size != nil || r.Provenance.IsZero() {
			return invalidf("an OCI registration is an output with a registry, trusted provenance, and no media type or size")
		}
	default:
		return invalidf("unknown kind %q", r.Kind)
	}
	return nil
}

// Artifact is one retained output. Identity, tenant/project, producer, content
// declaration and provenance are immutable; only the lifecycle fields change,
// and only through the transition methods.
type Artifact struct {
	id          domain.ArtifactID
	res         Reservation
	state       State
	disposition Disposition
	staging     string
	sealed      string
	version     domain.Version
	createdAt   time.Time
	updatedAt   time.Time
	// transferExpiresAt is the latest instant any issued upload instruction can
	// still be honored by the object store.
	transferExpiresAt *time.Time
	verifiedAt        *time.Time
	verifiedSize      *int64
	claim             Claim
}

// Claim is the durable, fenced completion lease.
type Claim struct {
	Token      string
	Generation int64
	Until      time.Time
}

// Active reports whether the lease is held at now.
func (c Claim) Active(now time.Time) bool { return c.Token != "" && !now.After(c.Until) }

// Record is the persisted form of an Artifact.
type Record struct {
	ID                domain.ArtifactID
	Reservation       Reservation
	State             State
	Disposition       Disposition
	StagingKey        string
	SealedKey         string
	Version           domain.Version
	CreatedAt         time.Time
	UpdatedAt         time.Time
	TransferExpiresAt *time.Time
	VerifiedAt        *time.Time
	VerifiedSize      *int64
	Claim             Claim
}

// Restore rebuilds an entity from storage and rejects rows that violate its
// invariants, so a corrupted row is never treated as usable.
func Restore(rec Record) (Artifact, error) {
	a := Artifact{id: rec.ID, res: rec.Reservation, state: rec.State, disposition: rec.Disposition,
		staging: rec.StagingKey, sealed: rec.SealedKey, version: rec.Version, createdAt: rec.CreatedAt, updatedAt: rec.UpdatedAt,
		transferExpiresAt: rec.TransferExpiresAt, verifiedAt: rec.VerifiedAt, verifiedSize: rec.VerifiedSize, claim: rec.Claim}
	if err := a.check(); err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	return a, nil
}

func (a Artifact) check() error {
	if err := a.id.Validate(); err != nil {
		return err
	}
	if err := a.res.Validate(); err != nil {
		return err
	}
	if err := a.version.Validate(); err != nil {
		return err
	}
	switch a.disposition {
	case DispositionActive:
	case DispositionAbandoned:
		if a.state != StateReserved {
			return fmt.Errorf("only a reserved artifact can be abandoned")
		}
	case DispositionRemoved:
		if a.state == StateReserved {
			return fmt.Errorf("a reserved artifact is never retention-removed")
		}
	default:
		return fmt.Errorf("unknown disposition %q", a.disposition)
	}
	switch a.res.Kind {
	case KindFile:
		if a.staging == "" {
			return fmt.Errorf("a file has a staging key")
		}
		switch a.state {
		case StateReserved:
			if a.sealed != "" || a.verifiedAt != nil {
				return fmt.Errorf("a reserved file has no sealed content")
			}
		case StateUsable:
			if a.sealed == "" || a.sealed == a.staging || a.verifiedAt == nil || a.verifiedSize == nil || *a.verifiedSize != *a.res.Size {
				return fmt.Errorf("a usable file is sealed and verified at its declared size")
			}
		default:
			return fmt.Errorf("a file cannot be %q", a.state)
		}
	case KindOCI:
		if a.state != StateRegistered || a.staging != "" || a.sealed != "" || a.verifiedAt == nil || a.verifiedSize == nil {
			return fmt.Errorf("an OCI registration is only stored once verified")
		}
	}
	return nil
}

func (a Artifact) ID() domain.ArtifactID         { return a.id }
func (a Artifact) Reservation() Reservation      { return a.res }
func (a Artifact) Scope() domain.Scope           { return a.res.Scope }
func (a Artifact) Attempt() domain.AttemptID     { return a.res.Attempt }
func (a Artifact) Name() string                  { return a.res.Name }
func (a Artifact) Kind() Kind                    { return a.res.Kind }
func (a Artifact) Role() Role                    { return a.res.Role }
func (a Artifact) Digest() Digest                { return a.res.Digest }
func (a Artifact) MediaType() string             { return a.res.MediaType }
func (a Artifact) Registry() string              { return a.res.Registry }
func (a Artifact) Provenance() Provenance        { return a.res.Provenance }
func (a Artifact) State() State                  { return a.state }
func (a Artifact) Disposition() Disposition      { return a.disposition }
func (a Artifact) StagingKey() string            { return a.staging }
func (a Artifact) SealedKey() string             { return a.sealed }
func (a Artifact) Version() domain.Version       { return a.version }
func (a Artifact) CreatedAt() time.Time          { return a.createdAt }
func (a Artifact) UpdatedAt() time.Time          { return a.updatedAt }
func (a Artifact) TransferExpiresAt() *time.Time { return a.transferExpiresAt }
func (a Artifact) VerifiedAt() *time.Time        { return a.verifiedAt }
func (a Artifact) Claim() Claim                  { return a.claim }

// Size returns the declared size of a file, or the verified manifest size of
// an OCI registration.
func (a Artifact) Size() (int64, bool) {
	if a.res.Size != nil {
		return *a.res.Size, true
	}
	if a.verifiedSize != nil {
		return *a.verifiedSize, true
	}
	return 0, false
}

// Usable reports whether the artifact may be referenced as a finished output:
// verified bytes (or a verified registration) that have not been removed.
func (a Artifact) Usable() bool {
	return (a.state == StateUsable || a.state == StateRegistered) && a.disposition == DispositionActive
}

// Open reports whether a reserved file can still be completed.
func (a Artifact) Open() bool {
	return a.state == StateReserved && a.disposition == DispositionActive
}

// Matches reports whether the declaration is the same immutable reservation:
// the replay rule. Provenance and producer step are server-derived and are not
// part of the comparison.
func (a Artifact) Matches(r Reservation) bool {
	x := a.res
	if x.Scope != r.Scope || x.Attempt != r.Attempt || x.Name != r.Name || x.Kind != r.Kind || x.Role != r.Role || x.Digest != r.Digest ||
		x.MediaType != r.MediaType || x.Registry != r.Registry {
		return false
	}
	return (x.Size == nil) == (r.Size == nil) && (x.Size == nil || *x.Size == *r.Size)
}

// CheckCompletion compares the completion request with the reservation. A
// disagreement is a digest mismatch: the producer is naming different content.
func (a Artifact) CheckCompletion(d Digest, size int64) error {
	if a.res.Kind != KindFile {
		if d != a.res.Digest {
			return fmt.Errorf("%w: completion digest differs from the registration", ErrDigestMismatch)
		}
		return nil
	}
	if d != a.res.Digest || size != *a.res.Size {
		return fmt.Errorf("%w: completion declaration differs from the reservation", ErrDigestMismatch)
	}
	return nil
}

// Verification is the evidence that the stored bytes are the declared content.
type Verification struct {
	Digest Digest
	Size   int64
	At     time.Time
}

// Complete transitions a reserved file to usable at the sealed destination. The
// evidence must equal the declaration: there is no way to make different bytes
// usable.
func (a Artifact) Complete(v Verification, sealedKey string, now time.Time) (Artifact, error) {
	if a.res.Kind != KindFile || !a.Open() {
		return Artifact{}, fmt.Errorf("%w: only an open file reservation can be completed", ErrInvalid)
	}
	if v.Digest != a.res.Digest || v.Size != *a.res.Size {
		return Artifact{}, fmt.Errorf("%w: verified content differs from the declaration", ErrDigestMismatch)
	}
	if sealedKey == "" || sealedKey == a.staging {
		return Artifact{}, fmt.Errorf("%w: sealed content needs its own destination", ErrInvalid)
	}
	n := a
	n.state, n.sealed = StateUsable, sealedKey
	at, size := now, v.Size
	n.verifiedAt, n.verifiedSize = &at, &size
	n.claim = Claim{}
	n.version++
	n.updatedAt = now
	return n, n.check()
}

// Abandon marks an expired, never-completed reservation as cleaned up.
func (a Artifact) Abandon(now time.Time) (Artifact, error) {
	if !a.Open() || a.claim.Active(now) {
		return Artifact{}, fmt.Errorf("%w: only an idle open reservation can be abandoned", ErrInvalid)
	}
	n := a
	n.disposition = DispositionAbandoned
	n.version++
	n.updatedAt = now
	return n, n.check()
}

// Remove records that retention deleted the content. Metadata and provenance
// stay.
func (a Artifact) Remove(now time.Time) (Artifact, error) {
	if !a.Usable() {
		return Artifact{}, fmt.Errorf("%w: only usable content can be removed", ErrInvalid)
	}
	n := a
	n.disposition = DispositionRemoved
	n.version++
	n.updatedAt = now
	return n, n.check()
}

// NewFile creates a reserved file artifact.
func NewFile(id domain.ArtifactID, r Reservation, stagingKey string, transferExpires, now time.Time) (Artifact, error) {
	if r.Kind != KindFile {
		return Artifact{}, invalidf("not a file reservation")
	}
	if stagingKey == "" {
		return Artifact{}, invalidf("a staging key is required")
	}
	te := transferExpires
	a := Artifact{id: id, res: r, state: StateReserved, disposition: DispositionActive, staging: stagingKey, version: 1,
		createdAt: now, updatedAt: now, transferExpiresAt: &te}
	return a, a.check()
}

// OCIVerification is what a registry adapter established about a registration.
type OCIVerification struct {
	// Repository is the canonical registry/repository, without tag or digest.
	Repository string
	Digest     Digest
	// ManifestMediaType and ManifestSize describe the verified manifest bytes.
	ManifestMediaType string
	ManifestSize      int64
	// Provenance is the trusted source provenance bound to the content.
	Provenance Provenance
}

// NewRegistered creates a verified OCI registration. Nothing else creates one.
func NewRegistered(id domain.ArtifactID, r Reservation, v OCIVerification, now time.Time) (Artifact, error) {
	if r.Kind != KindOCI {
		return Artifact{}, invalidf("not an OCI registration")
	}
	if v.Digest != r.Digest || v.Repository != r.Registry || v.ManifestSize < 0 || v.Provenance.IsZero() || v.Provenance != r.Provenance {
		return Artifact{}, fmt.Errorf("%w: registry evidence does not match the registration", ErrRegistryContent)
	}
	at, size := now, v.ManifestSize
	a := Artifact{id: id, res: r, state: StateRegistered, disposition: DispositionActive, version: 1, createdAt: now, updatedAt: now,
		verifiedAt: &at, verifiedSize: &size}
	return a, a.check()
}

// Reference is the minimal, scoped view other modules use to decide whether an
// artifact may be consumed as a result output, source bundle or evidence.
type Reference struct {
	ID      domain.ArtifactID
	Scope   domain.Scope
	Attempt domain.AttemptID
	Kind    Kind
	Role    Role
	State   State
	Usable  bool
	Digest  Digest
	// Provenance is empty for a file produced without source.
	Provenance Provenance
}

// Reference returns the artifact's consumer view.
func (a Artifact) Reference() Reference {
	return Reference{ID: a.id, Scope: a.res.Scope, Attempt: a.res.Attempt, Kind: a.res.Kind, Role: a.res.Role,
		State: a.state, Usable: a.Usable(), Digest: a.res.Digest, Provenance: a.res.Provenance}
}

// String never includes keys or digests.
func (a Artifact) String() string {
	return fmt.Sprintf("artifact %s (%s/%s %s)", a.id, a.res.Kind, a.res.Role, strings.ToLower(string(a.state)))
}
