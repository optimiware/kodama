package runtimeauth

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ---- consumer-owned execution ports --------------------------------------

// AttemptPhase is the execution module's authoritative view of an attempt.
type AttemptPhase string

const (
	// AttemptActive may be admitted and may use its granted actions.
	AttemptActive AttemptPhase = "active"
	// AttemptCancelled was cancelled or timed out: all authority is gone.
	AttemptCancelled AttemptPhase = "cancelled"
	// AttemptFinished reached a terminal state: no new authority.
	AttemptFinished AttemptPhase = "finished"
)

// AttemptState is what the execution owner (T-020) tells this module about an
// attempt. It is authoritative: every field is trusted, nothing in it comes
// from a request.
type AttemptState struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Phase     AttemptPhase
	// Revision is the authority revision. The execution owner increments it
	// whenever the attempt's Job binding, pinned context or allowed actions
	// change; it does not change for unrelated progress. A grant registered at
	// another revision is stale and denied.
	Revision int64
	// Job is the Job created for this attempt.
	Job ExpectedJob
	// ContextDigest references the immutable pinned context (SHA-256 of its
	// canonical document).
	ContextDigest Digest
	// Actions are the actions the attempt currently allows. The effective
	// authority is the grant ceiling intersected with this set.
	Actions ActionSet
}

// Validate checks the structure.
func (s AttemptState) Validate() error {
	if err := firstErr(s.Scope.Validate(), s.RunID.Validate(), s.AttemptID.Validate(), s.Job.Validate()); err != nil {
		return fail(ErrUnavailable, "the execution state is malformed")
	}
	switch s.Phase {
	case AttemptActive, AttemptCancelled, AttemptFinished:
	default:
		return fail(ErrUnavailable, "the execution state has an unknown phase")
	}
	if s.Revision < 1 || s.Revision > domain.MaxSafeInteger || s.ContextDigest.IsZero() {
		return fail(ErrUnavailable, "the execution state is incomplete")
	}
	return nil
}

// AttemptObserver reads attempt state without taking locks. Its answer is
// advisory (used before a Kubernetes observation); every mutation re-reads and
// locks through GrantTx.LockAttempt. ErrNotFound means no such attempt.
type AttemptObserver interface {
	ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptState, error)
}

// JobObservation asks Kubernetes about the workload that claims an attempt.
// Claimed values are evidence, never authority.
type JobObservation struct {
	Scope          domain.Scope
	AttemptID      domain.AttemptID
	Expected       ExpectedJob
	ClaimedJobName string
	ClaimedJobUID  string
	ClaimedPodName string
	ClaimedPodUID  string
}

// JobEvidence is what Kubernetes reports for the observed Job and Pod.
type JobEvidence struct {
	Namespace    string
	JobName      string
	JobUID       string
	PodNamespace string
	PodName      string
	PodUID       string
	// PodOwnerUID is the UID of the Pod's controlling owner reference.
	PodOwnerUID string
}

// ErrNotObserved is returned by a JobObserver when the Job or Pod definitely
// does not exist. Any other error means the observation was inconclusive.
var ErrNotObserved = fail(ErrNotFound, "the workload was not observed")

// JobObserver verifies observed Job/Pod ownership in the tenant's namespace
// (T-021 supplies the Kubernetes adapter). It is called outside any SQL
// transaction.
type JobObserver interface {
	ObserveJob(ctx context.Context, obs JobObservation) (JobEvidence, error)
}

// ---- persistence ports ---------------------------------------------------

// Locator is the ownership of the grant matching a credential digest. It
// carries no context and no authority; every later step rechecks the
// credential inside a tenant transaction.
type Locator struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
}

// LockMode selects the row lock taken when reading a grant.
type LockMode int

const (
	// LockNone reads without a lock (used only to learn immutable ownership).
	LockNone LockMode = iota
	// LockShare blocks revocation and consumption while a decision is made.
	LockShare
	// LockUpdate is taken to consume or revoke.
	LockUpdate
)

// Store is the persistence port of runtime grants.
type Store interface {
	// Locate resolves a credential digest to its owner through a restricted,
	// digest-filtered lookup. An unknown digest is ErrInvalidCredential.
	Locate(ctx context.Context, digest Digest) (Locator, error)
	// Do runs fn in one transaction scoped to tenant (RLS context set). fn
	// returning nil commits; any error rolls back. fn must not make network
	// calls. An error from the commit itself has an unknown outcome and is
	// reported as ErrUnavailable.
	Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx GrantTx) error) error
}

// GrantTx is the transaction-bound view of grants and execution state.
//
// Lock order, for every transaction in the system: the attempt's execution
// state first (LockAttempt), then grant rows. Cancellation takes the same
// order, which is what serializes it with admission and refresh.
type GrantTx interface {
	// LockAttempt re-reads the authoritative attempt state and locks it
	// against concurrent cancellation (a lock that conflicts with the
	// execution owner's cancel). ErrNotFound if the attempt does not exist in
	// the scope.
	LockAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptState, error)
	// GrantByCredential and GrantBySession return the grant (ErrNotFound when
	// absent or invisible to the tenant) under the requested lock.
	GrantByCredential(ctx context.Context, digest Digest, mode LockMode) (AdmissionGrant, error)
	GrantBySession(ctx context.Context, session SessionID, mode LockMode) (AdmissionGrant, error)
	// InsertGrant stores a new grant. When a grant for the attempt exists it
	// returns that grant and inserted == false; the caller compares.
	InsertGrant(ctx context.Context, g AdmissionGrant) (stored AdmissionGrant, inserted bool, err error)
	// SaveConsumption persists the first consumption of a locked grant.
	SaveConsumption(ctx context.Context, g AdmissionGrant) error
	// SaveRevocation persists the revocation of a locked grant.
	SaveRevocation(ctx context.Context, g AdmissionGrant) error
	// RevokeAttempt revokes every grant of the attempt and reports how many
	// were newly revoked.
	RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error)
}

// ---- tokens --------------------------------------------------------------

// IssueRequest is the content of one token.
type IssueRequest struct {
	Tenant     domain.TenantID
	Session    SessionID
	Generation int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// TokenClaims are the verified claims of a runtime token. They identify the
// tenant and the session only; project, attempt, workload and actions come
// from the durable grant.
type TokenClaims struct {
	Tenant     domain.TenantID
	Session    SessionID
	TokenID    string
	Generation int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// TokenIssuer signs runtime tokens. Issue fails with ErrInvalidCredential when
// req.Generation is no longer the current signing generation (a rotation raced
// the request) and with ErrUnavailable when signing fails.
type TokenIssuer interface {
	Generation() int64
	Issue(req IssueRequest) (string, error)
}

// TokenVerifier verifies a bearer token: signature, issuer, the fixed
// audience, expiry, algorithm and key. Failures are ErrInvalidCredential; an
// unusable verifier is ErrUnavailable.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (TokenClaims, error)
}
