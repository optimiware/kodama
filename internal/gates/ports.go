package gates

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// The ports below are small and owned by this module. Adapters implement them;
// the composition root wires them. A missing implementation fails closed:
// nothing here has an allow-all default, and no port exposes pgx, HTTP or S3
// types.

// Authority resolves a verified principal to an attributable actor and answers
// whether the actor currently holds a permission. Authorize returns nil to
// allow, ErrDenied or ErrHidden to refuse, and any other error for an outage
// (never access). The transaction-bound variant used by decisions and
// consumption is TxAuthority.
type Authority interface {
	ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (Actor, error)
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// TxAuthority is the permission check bound to one open tenant transaction. It
// reads current bindings through the transaction and holds the user's
// authorization revision until it ends, so a revocation committed while the
// transaction waited on a lock is seen.
type TxAuthority interface {
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// RunReader loads a run with its steps and attempts. execution.Store satisfies
// it.
type RunReader interface {
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// EvidenceMeta is the verified metadata of one artifact: ownership, producer,
// provenance and content digest. It carries no storage key.
type EvidenceMeta struct {
	ID      domain.ArtifactID
	Scope   domain.Scope
	RunID   domain.RunID
	StepID  string
	Attempt domain.AttemptID
	// Digest is the digest of the verified content.
	Digest string
	// Usable is true for verified, retained content.
	Usable bool
	// Size is the verified size of a file artifact.
	Size int64
	// Repository and Commit are the trusted source provenance the artifact
	// module recorded: the repository and the commit the producing attempt
	// worked from (its input), not necessarily a commit it published. Both are
	// "" only when the producer had no source; such an artifact is not
	// connected to any candidate.
	Repository string
	Commit     string
}

// Evidence reads artifact metadata and content. Describe returns ErrNotFound
// for an absent or foreign artifact and ErrUnavailable for an outage. Read
// returns the bytes after verifying the declared size and digest; it refuses
// objects larger than max, reports ErrEvidence for a missing or mismatching
// object and ErrUnavailable for an outage.
type Evidence interface {
	Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (EvidenceMeta, error)
	Read(ctx context.Context, scope domain.Scope, meta EvidenceMeta, max int64) ([]byte, error)
}

// Pins protects an artifact from retention deletion on behalf of a gate.
// Pinning is idempotent. There is no automatic unpin of historical evidence.
type Pins interface {
	Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, gate ID) error
}

// VerifiedSource is what verifying a candidate establishes: the repository of
// the work item's workspace the commit belongs to.
type VerifiedSource struct {
	Repository string
}

// Candidates verifies that a commit is a legitimate source of the work item:
// it belongs to the work item's retained workspace history. It returns the
// repository the commit was verified in. ErrEvidence refuses an unverifiable
// commit; ErrUnavailable reports an outage. An answer without a repository is
// unusable and is refused by the caller.
type Candidates interface {
	Verify(ctx context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (VerifiedSource, error)
}

// DeploymentSnapshot is the current deployment, its trusted ownership and
// source, and its configuration digest.
type DeploymentSnapshot struct {
	ID           string
	ConfigDigest string
	// Scope is the tenant and project owning the deployment row.
	Scope domain.Scope
	// Candidate is the source commit the deployment row records as its
	// candidate.
	Candidate string
}

// Deployments resolves a deployment and its configuration through the
// deployment module, never from a request. ErrNotFound for an absent or
// foreign deployment. Without it a deployment-bearing gate fails closed.
//
// Obligations of an implementation: the answer is the deployment row of the
// given scope with its recorded candidate commit (never a value supplied by a
// caller); a deployment of another project or tenant is ErrNotFound. The
// snapshot is also read inside the protected progression transaction (the
// transaction-bound binder of the gate store), where the read must come from
// that transaction so the configuration is current through the protected
// transition, and must not perform a network call or take a lock that
// precedes the execution locks of the transaction.
type Deployments interface {
	Snapshot(ctx context.Context, scope domain.Scope, id string) (DeploymentSnapshot, error)
}

// ConformanceExpectation is what trusted pinned data says about a candidate's
// conformance evidence: the design baseline pinned in the producing attempt's
// immutable context, the declared report output of the producer, and the
// producer's accepted publication.
type ConformanceExpectation struct {
	// Baseline is the pinned baseline, or nil when the producer had none.
	Baseline *runtimev1.DesignBaseline
	// ReportID identifies the report artifact, or "" when the producer's
	// declarations and outputs do not identify exactly one.
	ReportID domain.ArtifactID
	// ProducerAttempt is the successful attempt that produced the candidate.
	ProducerAttempt domain.AttemptID
	// PublishedCommit is the source commit the producer's accepted result
	// published.
	PublishedCommit string
}

// Conformance identifies the expected baseline and report of an agent-produced
// candidate from immutable run data. An agent never selects the required
// checks or the report. A missing mapping yields a zero expectation, which
// blocks approval.
type Conformance interface {
	Expected(ctx context.Context, agg *execution.Aggregate, candidate execution.GateCandidate) (ConformanceExpectation, error)
}

// RequestPlan is a fully prepared gate ready to commit together with its
// binding to the execution step. Everything outside the transaction (evidence,
// conformance, candidate and deployment verification, pinning) is resolved.
type RequestPlan struct {
	Gate *Gate
	// Inputs are the execution-resolved inputs the subject was built from. The
	// transaction recomputes them under the run lock and refuses a mismatch.
	Inputs    execution.GateInputs
	Actor     Actor
	RequestID string
}

// DecisionPlan is a validated decision ready to commit. The actor was derived
// from the verified principal and has the reviewer authority at preparation;
// the transaction checks it again.
type DecisionPlan struct {
	Scope     domain.Scope
	GateID    ID
	Command   DecideCommand
	RequestID string
}

// StepOutcome reports what processing a decided gate did to its step.
type StepOutcome struct {
	Completed bool
	Rejected  bool
	Cancelled bool
	// Waiting is true when the step is still pending.
	Waiting bool
}

// ListQuery filters and pages gates ordered by (created_at, id).
type ListQuery struct {
	Statuses []Status
	RunID    domain.RunID
	After    *Position
	Limit    int
}

// Position is the continuation key of a list.
type Position struct {
	CreatedAt time.Time
	ID        ID
}

// StepRef addresses a gate step for scans.
type StepRef struct {
	Scope  domain.Scope
	RunID  domain.RunID
	StepID string
}

// GateRef addresses a gate for scans.
type GateRef struct {
	Scope domain.Scope
	ID    ID
}

// Store persists gates. One method call is one transaction: state, audit and
// outbox events commit together or not at all, and every operation is scoped
// by tenant and project.
type Store interface {
	Get(ctx context.Context, scope domain.Scope, id ID) (*Gate, error)
	// ByStep returns the gate of a run step or ErrNotFound.
	ByStep(ctx context.Context, scope domain.Scope, run domain.RunID, step string) (*Gate, error)
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]*Gate, error)
	// Request creates the gate and binds it to its ready step in one
	// transaction, unless one exists for the step (created is false).
	Request(ctx context.Context, p *RequestPlan) (g *Gate, created bool, err error)
	// Decide records the decision after rechecking current authority, version,
	// subject digest, deadline and current subject facts. A definitive
	// staleness is returned as *StaleError and changes nothing.
	Decide(ctx context.Context, p *DecisionPlan) (*Gate, error)
	// Invalidate records the definitive staleness once, without touching the
	// decision. created is false for a repeat.
	Invalidate(ctx context.Context, scope domain.Scope, id ID, e *StaleError, requestID string) (g *Gate, created bool, err error)
	// Reevaluate loads the current facts of an undecided or approved gate and
	// returns *StaleError for a definitive staleness, nil otherwise. An outage
	// is ErrUnavailable. It writes nothing.
	Reevaluate(ctx context.Context, scope domain.Scope, id ID) error
	// CompleteStep consumes an applicable approval: it rechecks the gate, the
	// current facts and the original reviewer's authority under the run lock
	// and performs the explicit pending-to-succeeded step transition.
	CompleteStep(ctx context.Context, scope domain.Scope, id ID) (StepOutcome, error)
	// RejectStep fails the pending step of a rejected gate.
	RejectStep(ctx context.Context, scope domain.Scope, id ID) (StepOutcome, error)

	// ReadyGateSteps lists pending gate steps of nonterminal runs without a
	// gate, in bounded pages ordered by (project, run, step).
	ReadyGateSteps(ctx context.Context, tenant domain.TenantID, after StepRef, limit int) ([]StepRef, error)
	// OpenGates lists gates of nonterminal runs that were not invalidated,
	// ordered by (project, id).
	OpenGates(ctx context.Context, tenant domain.TenantID, after GateRef, limit int) ([]GateRef, error)
}

// Tenants enumerates tenants for background reconciliation.
type Tenants interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}
