package qa

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. The adapters sub-package
// implements them over the execution, delivery, artifact and work modules; the
// composition root wires them. A missing implementation fails closed: nothing
// here has an allow-all default, and no port exposes pgx, HTTP or S3 types.

// ActorType classifies the caller of Record. The type comes from the trusted
// transport that authenticated the call, never from a report or a request body.
type ActorType string

const (
	// ActorSystem is a trusted control-plane component built by the composition
	// root (a reconciler or an event consumer), never by a transport.
	ActorSystem ActorType = "system"
	// ActorWorkload is a runtime workload identity, the QA attempt's own
	// included. It is recognized only to be refused: the attempt is finished
	// when its report is mapped and runtime authority has no mapping action.
	ActorWorkload ActorType = "workload"
	// ActorUser and ActorOperator are recognized only to be refused: a person
	// uses the ordinary work interface and decides at the gate.
	ActorUser     ActorType = "user"
	ActorOperator ActorType = "operator"
)

// Actor is the verified identity of the caller.
type Actor struct {
	Type ActorType
	ID   string
}

// Caller is the verified cause of a Record call.
type Caller struct {
	Actor     Actor
	RequestID string
}

// AttemptFacts are the trusted facts of one QA attempt, read from the execution
// module's persisted records: its owner, its immutable pinned context (verified
// against the stored digest, scope and identity) and its accepted result.
type AttemptFacts struct {
	Scope   domain.Scope
	Run     domain.RunID
	Step    string
	Attempt domain.AttemptID
	// Context is the immutable pinned context the attempt ran with, projected to
	// the version-independent members the mapping reads.
	Context runtimev1.Context
	// ContextVersion is the runtime context version the attempt was pinned to: 1
	// (also the zero value), 2 or 3. Only version 3 carries a source set.
	ContextVersion int64
	// Sources is the pinned source set of a context v3 (empty for other versions
	// and for a source-free run).
	Sources SourceSet
	// Completed is true only for a succeeded attempt whose accepted result is
	// the completed outcome.
	Completed bool
	// Outputs are the string values of the accepted result's outputs (an
	// artifact output holds the artifact identifier).
	Outputs map[string]string
	// Diagnostics are the diagnostic artifacts the accepted result lists.
	Diagnostics []domain.ArtifactID
}

// Attempts reads trusted attempt facts. ErrNotFound for an attempt that does not
// exist in the scope (a foreign attempt is not found, not denied).
type Attempts interface {
	Facts(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptFacts, error)
}

// DeploymentFacts are the deployment module's recorded facts about one
// deployment: ownership, the environment and configuration it was applied with,
// its candidate and image, and its outcome.
type DeploymentFacts struct {
	ID    string
	Scope domain.Scope
	// Environment and ConfigDigest are the environment and the immutable
	// configuration snapshot the deployment was recorded under.
	Environment  domain.EnvironmentID
	ConfigDigest runtimev1.Digest
	// Candidate is the source commit the deployment was built from.
	Candidate runtimev1.GitCommit
	// Image is the digest of the deployed image.
	Image runtimev1.Digest
	// Succeeded is true for a deployment whose outcome is succeeded.
	Succeeded bool
	DeletedAt *time.Time
}

// Deployments resolves a deployment through the deployment module, never from a
// request. ErrNotFound for an absent or foreign deployment.
type Deployments interface {
	Deployment(ctx context.Context, scope domain.Scope, id string) (DeploymentFacts, error)
}

// EvidenceMeta is the artifact module's verified metadata of one artifact:
// ownership, producer and content identity. It carries no storage key.
type EvidenceMeta struct {
	ID      domain.ArtifactID
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	Step    string
	// Digest is the digest of the verified content.
	Digest    runtimev1.Digest
	MediaType string
	Size      int64
	// File is true for a file artifact (not an OCI registration).
	File bool
	// Usable is true for verified, retained content.
	Usable bool
}

// Artifacts reads and protects artifacts. Describe returns ErrNotFound for an
// absent or foreign artifact and ErrUnavailable for an outage. Read returns the
// bytes after verifying the declared size and digest, refuses an object larger
// than max and reports ErrEvidence for missing or mismatching content. Pin
// protects an artifact from retention deletion for owner (a defect identifier);
// it is idempotent and a removed artifact is ErrNotFound.
type Artifacts interface {
	Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (EvidenceMeta, error)
	Read(ctx context.Context, scope domain.Scope, meta EvidenceMeta, max int64) ([]byte, error)
	Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner string) error
}

// Criterion is one acceptance criterion of a defect.
type Criterion struct {
	ID   string
	Text string
}

// SubjectFacts are the work module's facts about the work item under test.
type SubjectFacts struct {
	ID              domain.WorkItemID
	ContentRevision domain.ContentRevision
	Cancelled       bool
}

// EvidenceLink references one artifact from a defect.
type EvidenceLink struct {
	ArtifactID domain.ArtifactID
	Note       string
}

// DefectDraft is everything the defect work item is created from. ID is the
// deterministic identity derived from Key.
type DefectDraft struct {
	Scope domain.Scope
	ID    domain.WorkItemID
	Key   string
	// Payload is the digest of the finding's substantive content
	// (Observation.PayloadDigest). The adapter records it in the defect's
	// immutable creation record and compares it on every later report of the
	// same identity.
	Payload  string
	Attempt  domain.AttemptID
	Title    string
	Spec     string
	Criteria []Criterion
	Evidence []EvidenceLink
	// RequestID correlates the work history and events with the caller's request.
	RequestID string
}

// Defect is the work module's view of a filed defect.
type Defect struct {
	ID      domain.WorkItemID
	Version domain.Version
	State   string
	// Terminal is true for a done or cancelled defect, which accepts no edits.
	Terminal bool
	// Links are the artifact links the defect holds.
	Links []domain.ArtifactID
}

// Defects reaches the work module. Open is create-or-observe under the
// deterministic identity: when the defect exists (created earlier, by a retry,
// a concurrent call, or a lost-acknowledgment commit) it is returned with
// created=false and nothing is written. Attach adds the links the defect lacks
// (bounded by the work item's link limit); a terminal defect is not edited.
// Verify answers, before anything is written, whether the identity is free or
// held by a defect of the same finding payload; a defect created from a
// different payload (or without a recorded payload) is ErrConflict. Open applies
// the same comparison. Every write is an ordinary work-module mutation: state, history, audit and
// outbox event commit together or not at all.
type Defects interface {
	Subject(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (SubjectFacts, error)
	Verify(ctx context.Context, draft DefectDraft) error
	Open(ctx context.Context, draft DefectDraft) (defect Defect, created bool, err error)
	Attach(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, requestID string, id domain.WorkItemID, links []EvidenceLink) (defect Defect, added int, err error)
}
