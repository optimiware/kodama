package results

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are consumer-owned: this module states what it needs and the
// adapters in adapters/ satisfy them over the execution, artifact, workspace
// and runtime-authority modules. A missing port fails closed: there is no
// allow-all default.

// Action is the runtime action a route requires.
type Action string

const (
	// ActionSourcePropose authorizes POST /source.
	ActionSourcePropose Action = "source.propose"
	// ActionResultSubmit authorizes POST /result.
	ActionResultSubmit Action = "result.submit"
)

// AuthRequest is a bearer's request to use an action on the attempt named by
// the route.
type AuthRequest struct {
	Attempt domain.AttemptID
	Bearer  string
	Action  Action
	// Wire is the runtime wire of the route being served: WireV3 for the source-set
	// result route. The zero value is the earlier routes' behavior (no wire is
	// asserted; the route's own guard and execution refuse another wire's attempt).
	// An attempt of another wire than a stated one is ErrContextVersionMismatch
	// and nothing is read or changed.
	Wire int64
}

// WireV3 is the source-set runtime wire (runtimeauth.WireV3, runtime contract v3).
const WireV3 int64 = 3

// Principal is the verified authority of a workload for one action. Every
// identifier in it comes from the durable grant, never from the request.
type Principal struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Action    Action
	// Proof is opaque to this module: the authority adapter reads it back in
	// Bind to repeat the check inside the protected mutation's transaction.
	Proof any
}

// Authority verifies bearer tokens against the runtime grant store.
//
// A decision made by Authenticate is not a permit: it is a first, cheap check.
// Bind returns a context under which the transaction-guarded stores repeat the
// session check inside the mutation's own transaction, after taking the
// execution locks (see postgres.ExecutionStore and postgres.WorkspaceStore).
type Authority interface {
	// Authenticate verifies the bearer and the route binding. Failures are
	// ErrUnauthenticated, ErrForbidden, ErrAttemptRevoked or ErrUnavailable.
	Authenticate(ctx context.Context, req AuthRequest) (Principal, error)
	// Bind attaches the principal's session to ctx for guarded mutations. A
	// principal this authority did not issue is refused: an unbound context
	// would silently skip the in-transaction check.
	Bind(ctx context.Context, p Principal) (context.Context, error)
}

// Contexts returns the immutable pinned context of an attempt. It is read
// without locks: the document never changes after dispatch.
type Contexts interface {
	Pinned(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev1.Context, error)
}

// Provenance is the trusted source an artifact's producer worked from: one
// repository at one commit, or (source-set runs) the digest of the whole set it
// worked from.
type Provenance struct {
	RepositoryURL string
	Commit        string
	// SourceSet is the digest of the source set the producer worked from; empty
	// for a single source or no source.
	SourceSet string
}

// IsZero reports an artifact produced without source.
func (p Provenance) IsZero() bool { return p == Provenance{} }

// ArtifactEvidence is the artifact owner's verified view of one artifact of the
// attempt: ownership, kind, role, completeness and trusted provenance.
type ArtifactEvidence struct {
	ID         domain.ArtifactID
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Kind       runtimev1.ArtifactKind
	Role       runtimev1.ArtifactRole
	Usable     bool
	Digest     runtimev1.Digest
	Provenance Provenance
}

// Evidence reads artifact metadata through the artifact owner and protects
// consumed evidence from retention.
type Evidence interface {
	// References returns the attempt's artifacts among ids. An id that is
	// absent, belongs to another tenant, project or attempt, or was removed is
	// omitted, so a consumer cannot tell the cases apart.
	References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]ArtifactEvidence, error)
	// Protect pins the artifacts for the attempt's result consumer so retention
	// cannot remove them. It is idempotent.
	Protect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) error
}

// Commit is the trusted finalization input handed to execution. Artifacts is
// metadata the artifact owner verified; execution re-checks it against the
// pinned context under its run lock.
type Commit struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
	Request   runtimev1.ResultRequest
	Artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
}

// Acknowledgment is the durable acceptance of a result. AcceptedAt is the
// persisted acceptance time, so every replay repeats the original value.
type Acknowledgment struct {
	AttemptID  domain.AttemptID
	ResultHash runtimev1.Digest
	Status     runtimev1.AttemptStatus
	AcceptedAt time.Time
}

// Finalizer is the execution owner's result transition.
type Finalizer interface {
	// Accepted reports the result hash the attempt already accepted, if any.
	Accepted(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (hash runtimev1.Digest, found bool, err error)
	// Commit records the result idempotently under attempt ID and hash.
	Commit(ctx context.Context, c Commit) (Acknowledgment, error)
}

// SourceOutcome is an accepted (200) or pending (202) source proposal.
type SourceOutcome struct {
	// HTTPStatus is 200 for accepted and 202 for pending.
	HTTPStatus int
	Response   runtimev1.SourceProposalResponse
}

// Source is the workspace owner's publication coordination.
type Source interface {
	// Propose validates and publishes the proposal for the attempt in scope.
	// A lost expected-tip race is a *PublicationConflict.
	Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (SourceOutcome, error)
}
