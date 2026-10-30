package artifacts

import (
	"context"
	"io"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// AttemptGrant is the current, trusted authority of one runtime attempt:
// everything the module needs to authorize and bound a reservation without
// believing the request. It is supplied by the execution module (T-016/T-020/
// T-027) and is re-read on every call; this module never caches it.
type AttemptGrant struct {
	Scope   domain.Scope
	Attempt domain.AttemptID
	Run     domain.RunID
	Step    string
	// ExpiresAt bounds the attempt's authority. No transfer instruction is
	// issued for longer, so renewed instructions never extend it.
	ExpiresAt time.Time
	// Writer says whether the attempt may produce a source bundle.
	Writer bool
	// Source is the trusted source the attempt works from, or the zero value.
	Source Provenance
	// Outputs are the declared artifact outputs.
	Outputs []OutputDecl
	// Inputs are the verified file artifacts pinned for this attempt.
	Inputs []InputRef
	Limits Limits
}

// InputRef is one verified artifact descriptor the attempt may read.
type InputRef struct {
	ID     domain.ArtifactID
	Digest Digest
}

func (g AttemptGrant) declared(name string) (OutputDecl, bool) {
	for _, o := range g.Outputs {
		if o.Name == name {
			return o, true
		}
	}
	return OutputDecl{}, false
}

// AttemptAuthority is the consumer-owned port to the execution module. Until an
// adapter is wired, every runtime operation is denied: there is no permissive
// default and no caller-supplied "authorized" flag.
type AttemptAuthority interface {
	// Grant returns the attempt's current authority for the verified caller.
	// It returns ErrAuthorityMissing when no trusted record binds the caller to
	// the attempt, ErrAttemptInactive for a revoked, cancelled or expired one,
	// and ErrNotFound for an attempt the caller may not address. Any other error
	// is a dependency failure; every operation fails closed on it.
	Grant(ctx context.Context, caller Caller, attempt domain.AttemptID) (AttemptGrant, error)
}

// AttemptReadAuthority is the optional consumer-read authority. It is separate
// from AttemptAuthority so a producer authority cannot silently grant reads.
type AttemptReadAuthority interface {
	GrantRead(ctx context.Context, caller Caller, attempt domain.AttemptID) (AttemptGrant, error)
}

// ReadSubject describes the artifact a reader asks about, so the authority can
// apply role-specific rules such as diagnostic visibility.
type ReadSubject struct {
	ID      domain.ArtifactID
	Attempt domain.AttemptID
	Kind    Kind
	Role    Role
	// Download is true when content access, not only metadata, is requested.
	Download bool
}

// ReadAuthority authorizes principal access to artifact metadata and content,
// using current membership on every call.
type ReadAuthority interface {
	// AuthorizeRead returns nil to allow, ErrForbidden to refuse, ErrNotFound to
	// hide the artifact's existence, and any other error for an outage (treated
	// as a refusal).
	AuthorizeRead(ctx context.Context, caller Caller, scope domain.Scope, subject ReadSubject) error
}

// Transfer is a short-lived, scoped transfer instruction. It is not an
// identity and is never stored.
type Transfer struct {
	Method    string
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

// UploadSpec asks for a PUT instruction for a staging object.
type UploadSpec struct {
	Key       string
	MediaType string
	Digest    Digest
	Size      int64
	ExpiresAt time.Time
}

// DownloadSpec asks for a GET instruction for a sealed object.
type DownloadSpec struct {
	Key       string
	MediaType string
	ExpiresAt time.Time
}

// PutSpec seals verified bytes at a destination.
type PutSpec struct {
	Key       string
	Body      io.ReadSeeker
	Size      int64
	Digest    Digest
	MediaType string
}

// Object is an opened stored object.
type Object struct {
	Body io.ReadCloser
	Size int64
}

// ObjectStore is the consumer-owned port to the file byte store. Keys are the
// logical keys produced by ObjectKey; the adapter owns the bucket and prefix and
// refuses keys outside its owned prefix. No SDK or provider type crosses it.
type ObjectStore interface {
	PresignUpload(ctx context.Context, spec UploadSpec) (Transfer, error)
	PresignDownload(ctx context.Context, spec DownloadSpec) (Transfer, error)
	// Open returns ErrObjectNotFound for an absent key.
	Open(ctx context.Context, key string) (Object, error)
	// Put writes the bytes at an unused key.
	Put(ctx context.Context, spec PutSpec) error
	// Delete removes the key; an absent key is success.
	Delete(ctx context.Context, key string) error
}

// OCIRequest asks a registry adapter to verify a registration.
type OCIRequest struct {
	Scope    domain.Scope
	Attempt  domain.AttemptID
	Run      domain.RunID
	Registry string // the caller's spelling; never trusted for ownership
	Digest   Digest
}

// RegistryVerifier verifies, by digest, that content exists in a registry the
// tenant/project may use, and binds it to trusted source provenance. It
// returns ErrRegistryDenied, ErrRegistryContent, ErrProvenanceMissing for
// unverifiable registrations and ErrRegistryUnavailable for outages.
type RegistryVerifier interface {
	Verify(ctx context.Context, req OCIRequest) (OCIVerification, error)
}

// RetentionSubject is what a retention policy may base its decision on.
type RetentionSubject struct {
	ID         domain.ArtifactID
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Kind       Kind
	Role       Role
	CreatedAt  time.Time
	VerifiedAt time.Time
}

// Retention is the injected policy deciding whether usable content may be
// deleted. This module supplies the mechanism only: with no Retention, nothing
// usable is ever deleted. The production policy is operational (O-11).
type Retention interface {
	Eligible(ctx context.Context, subject RetentionSubject, now time.Time) (bool, error)
}

// IDs generates artifact identifiers and unguessable nonces.
type IDs interface {
	NewArtifactID() domain.ArtifactID
	// NewToken returns a fresh random token (claims, key nonces).
	NewToken() string
}

// Actor is the identity recorded in the audit entry of a transition.
type Actor struct {
	Type string // "workload" or "system"
	ID   string
}

// ReserveCommand is one atomic reservation.
type ReserveCommand struct {
	Artifact Artifact
	Limits   Limits
	Actor    Actor
	// RequestID is the server-generated request identifier, for the audit entry.
	RequestID string
	Now       time.Time
}

// ReserveResult says whether the artifact was created by this call. When it was
// not, Artifact is the existing one and nothing was written.
type ReserveResult struct {
	Artifact Artifact
	Created  bool
}

// BeginCompletion asks for the fenced completion lease.
type BeginCompletion struct {
	Scope       domain.Scope
	Attempt     domain.AttemptID
	ID          domain.ArtifactID
	Token       string
	Destination string
	Lease       time.Duration
	Now         time.Time
}

// ClaimResult carries the artifact; Claim is nil when the artifact needs no
// completion (already usable, or a registration).
type ClaimResult struct {
	Artifact Artifact
	Claim    *Claim
}

// CommitCommand finalizes a completion under its claim.
type CommitCommand struct {
	Scope        domain.Scope
	Attempt      domain.AttemptID
	ID           domain.ArtifactID
	Claim        Claim
	Destination  string
	Verification Verification
	Actor        Actor
	RequestID    string
	Now          time.Time
}

// PinOwner identifies a consumer protecting an artifact from retention.
type PinOwner struct {
	Kind string
	ID   string
}

// DeletionClaim is a leased, fenced object deletion.
type DeletionClaim struct {
	Scope      domain.Scope
	ArtifactID domain.ArtifactID
	Key        string
	Token      string
}

// Repository is the persistence port. Implementations are tenant-scoped,
// atomic, and never call the object store or a registry inside a transaction.
type Repository interface {
	// Reserve creates the artifact unless one exists for (scope, attempt, name),
	// atomically enforcing the aggregate limits. It never overwrites.
	Reserve(ctx context.Context, cmd ReserveCommand) (ReserveResult, error)
	// FindByName returns the artifact of an attempt by name or ErrNotFound.
	FindByName(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, name string) (Artifact, error)
	// Get returns an artifact of the scope or ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (Artifact, error)
	// GetForAttempt is Get restricted to the producing attempt.
	GetForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (Artifact, error)
	// ReferencesForAttempt returns the attempt's artifacts among ids; absent
	// and foreign ids are omitted.
	ReferencesForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]Reference, error)
	// RecordTransfer durably records the latest issued transfer expiry before
	// any instruction exists. ErrNotFound when the artifact has no open or
	// usable file content.
	RecordTransfer(ctx context.Context, scope domain.Scope, id domain.ArtifactID, expires, now time.Time) error

	// BeginCompletion takes the fenced completion lease and durably records the
	// candidate destination before any object is written to it. ErrInProgress
	// while another live lease exists.
	BeginCompletion(ctx context.Context, cmd BeginCompletion) (ClaimResult, error)
	// ReleaseClaim ends a lease that produced a definitive failure.
	ReleaseClaim(ctx context.Context, scope domain.Scope, id domain.ArtifactID, token string) error
	// CommitUsable makes the artifact usable if the claim is still current,
	// atomically with its audit record. ErrClaimLost when fenced out.
	CommitUsable(ctx context.Context, cmd CommitCommand) (Artifact, error)

	// Pin protects a usable artifact from retention deletion; Unpin releases.
	Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner PinOwner, now time.Time) error
	Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner PinOwner) error
}

// CleanupPolicy parameterizes orphan and retention sweeps. The module defines
// no production values (O-11): zero durations disable the corresponding
// destructive step.
type CleanupPolicy struct {
	// StagingGrace is added to the latest possible write time of an object
	// before it may be deleted (in-flight transfer allowance).
	StagingGrace time.Duration
	// AbandonAfter is how long after its latest transfer expiry an idle,
	// never-completed reservation is abandoned. Zero disables abandonment.
	AbandonAfter time.Duration
	// DeletionLease bounds one worker's hold on an object deletion.
	DeletionLease time.Duration
	// Batch bounds the work of one sweep step.
	Batch int
}

// CleanupStore is the persistence port of the reconciler.
type CleanupStore interface {
	Tenants(ctx context.Context) ([]domain.TenantID, error)
	// AbandonExpired marks idle expired reservations abandoned, with audit.
	AbandonExpired(ctx context.Context, tenant domain.TenantID, now time.Time, p CleanupPolicy) (int, error)
	// ClaimDeletions promotes eligible objects to durable deletion intent and
	// leases a batch of them.
	ClaimDeletions(ctx context.Context, tenant domain.TenantID, now time.Time, p CleanupPolicy, token string) ([]DeletionClaim, error)
	// AckDeletion records a successful deletion if the lease is still held.
	AckDeletion(ctx context.Context, claim DeletionClaim, now time.Time, p CleanupPolicy) error
	// RetentionCandidates lists usable, unpinned artifacts after the cursor.
	RetentionCandidates(ctx context.Context, tenant domain.TenantID, after domain.ArtifactID, limit int) ([]RetentionSubject, error)
	// BeginRetentionDeletion marks content removed and records deletion
	// intent, serialized against pins. ErrPinned when protected.
	BeginRetentionDeletion(ctx context.Context, scope domain.Scope, id domain.ArtifactID, now time.Time, actor Actor) error
}
