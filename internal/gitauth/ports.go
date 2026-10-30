package gitauth

import (
	"context"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// ---- provided ports (the contract's gitauth interfaces) ---------------------

// Credentials issues what a person asks for through the public API
// (contracts/git/README.md, "gitauth issuance and verification").
type Credentials interface {
	IssueHuman(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error)
	IssueSeed(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error)
}

// Verifier resolves a presented Git credential for the Git endpoint and the push
// authorizer.
type Verifier interface {
	// Verify returns the effective claims of the credential's grant when the
	// credential is known, unexpired, unrevoked and issued for tenant, and the
	// authority that can change after issuance still holds (see Service.Verify).
	// Every refusal is invalid_credential with one message.
	Verify(ctx context.Context, tenant domain.TenantID, credential string) (gitv1.GrantClaims, error)
	// ConsumeSeed marks the seed grant consumed when its receive is authorized.
	ConsumeSeed(ctx context.Context, tenant domain.TenantID, grantID, receiveID string) error
	// Revoke is idempotent.
	Revoke(ctx context.Context, tenant domain.TenantID, grantID string) error
}

// AttemptGrantRequest is what execution asks of gitauth for a run's workloads
// (the execution.GitGrants port of contracts/git/README.md; its adapter
// converts the types).
type AttemptGrantRequest struct {
	Kind         gitv1.GrantKind // preparation, attempt or seed
	Actor        gitv1.Actor
	Repositories []gitv1.GrantRepository
	// ExpiresAt is bounded by the attempt's own deadline and the grant lifetime
	// bounds.
	ExpiresAt time.Time
}

// GrantHandle is an issued grant with its credential, for delivery only.
type GrantHandle struct {
	GrantID    string
	Credential Secret
	ExpiresAt  time.Time
}

// ForwardingSigner signs forwarding assertions: the control plane's half. It
// holds signing keys and is never given to a worker.
type ForwardingSigner interface {
	SignForwarding(ctx context.Context, a gitv1.ForwardingAssertion) (token string, err error)
}

// ForwardingVerifier is the worker-side check of workerTenant: signature and key
// (public keys only), audience, validity window at now and gitv1.CheckForwarding.
// A holder can verify an assertion and can never mint one.
type ForwardingVerifier interface {
	VerifyForwarding(ctx context.Context, token string, workerTenant domain.TenantID, now time.Time) (gitv1.ForwardingAssertion, error)
}

// WorkerIdentityIssuer issues and renews tenant-bound worker identities: the
// control plane's half. A worker is given an already issued token and renews it
// through the control plane; it never holds an identity signing key.
type WorkerIdentityIssuer interface {
	SignWorkerIdentity(ctx context.Context, w gitv1.WorkerIdentity) (token string, err error)
	RenewWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, lifetime time.Duration) (string, error)
}

// WorkerIdentityVerifier is the control-plane check of a route of pathTenant:
// signature, key, audience, validity window and gitv1.CheckWorkerCall.
type WorkerIdentityVerifier interface {
	VerifyWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error)
}

// Assertions is the contract's combined port for a process that holds both
// halves (the control plane, and tests): signing and verification of forwarding
// assertions and worker identities with key rotation. The adapter is package
// jwt (Signer, Verifier, Tokens); a worker is wired with a ForwardingVerifier
// over public keys only.
type Assertions interface {
	ForwardingSigner
	ForwardingVerifier
	WorkerIdentityIssuer
	WorkerIdentityVerifier
}

// ---- consumer-owned ports ----------------------------------------------------

// Store is the grant persistence port. Do is one tenant transaction: everything
// the function wrote commits or is discarded together, and an unknown commit
// outcome is ErrUnavailable. The transaction a caller already holds is joined
// through the adapter's Bind function (postgres.Bind, memory.Bind), the same
// seam runtimeauth uses, so execution revokes an attempt's grants in the
// transaction that cancels it.
type Store interface {
	Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx GrantTx) error) error
}

// GrantTx is what a use case sees inside one tenant transaction. Every method is
// confined to the transaction's tenant.
type GrantTx interface {
	// LockSeed takes the repository's seed lock until the transaction ends. Seed
	// issuance and seed consumption both take it (in that order: seed lock, then
	// grant rows), so issuance, with its eligibility checks, and a receive's
	// consumption together with the push record the caller creates in the same
	// transaction are decided one after the other.
	LockSeed(ctx context.Context, repositoryID string) error
	// GrantByDigest finds a grant by its credential digest (ErrNotFound).
	GrantByDigest(ctx context.Context, d Digest) (Grant, error)
	// GrantByID finds a grant by ID, locking its row when lock is true (ErrNotFound).
	GrantByID(ctx context.Context, id string, lock bool) (Grant, error)
	// GrantByIssueKey finds a workload grant by its idempotency key (ErrNotFound).
	GrantByIssueKey(ctx context.Context, key string, lock bool) (Grant, error)
	// Insert stores a new grant; a duplicate ID, digest or issue key is ErrConflict.
	Insert(ctx context.Context, g Grant) error
	// Update stores the changeable part of a grant: the credential digest of an
	// unrevoked grant, the write-once consumption and the irreversible
	// revocation. ErrNotFound when the grant does not exist.
	Update(ctx context.Context, g Grant) error
	// RevokeAttempt revokes every unrevoked grant of the attempt in the scope and
	// returns how many it revoked.
	RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error)
	// RevokeSeeds revokes the unrevoked, unconsumed seed grants of a repository.
	RevokeSeeds(ctx context.Context, repositoryID string, now time.Time) (int, error)
	// RevokeTenant revokes every unrevoked grant of the transaction's tenant, of
	// every kind and project, and returns how many it revoked.
	RevokeTenant(ctx context.Context, now time.Time) (int, error)
	// ActiveGrants counts the tenant's grants that are neither revoked nor
	// expired at now.
	ActiveGrants(ctx context.Context, now time.Time) (int, error)
	// RecordAudit appends an audit record of the tenant in the transaction.
	RecordAudit(ctx context.Context, r AuditRecord) error
}

// Repositories is gitauth's view of the repository lifecycle module (T-127).
type Repositories interface {
	// Repository returns the repository of the scope's project. A foreign or
	// unknown repository is ErrNotFound, indistinguishable.
	Repository(ctx context.Context, scope domain.Scope, repositoryID string) (gitv1.Repository, error)
}

// Pushes is gitauth's view of the push records (T-130).
type Pushes interface {
	// UnsettledPush reports whether the repository has any unsettled push record
	// of any kind: its derived status is authorized or unknown (gitv1.UnsettledPush).
	UnsettledPush(ctx context.Context, scope domain.Scope, repositoryID string) (bool, error)
}

// AttemptLease is execution's authoritative view of an attempt for Git grants.
type AttemptLease struct {
	// Live is true while the attempt is neither cancelled, finished, revoked nor
	// superseded.
	Live bool
	// Deadline is the end of the attempt's lease; zero means no bound beyond the
	// grant lifetimes.
	Deadline time.Time
}

// Attempts is gitauth's view of execution (T-148/T-150).
type Attempts interface {
	// Lease reads the attempt's current state; an unknown attempt of the scope is
	// ErrNotFound.
	Lease(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptLease, error)
}

// GateDecision is the gates module's current decision for an integration.
type GateDecision struct {
	// Approved is true when the gate was approved; Current when that decision has
	// not been superseded or invalidated.
	Approved bool
	Current  bool
	// RunID and AttemptID identify the integration step the gate belongs to.
	RunID     domain.RunID
	AttemptID domain.AttemptID
	// RepositoryID, Base and Candidate are the applicable subject: the repository
	// whose default branch is advanced from Base to the approved Candidate.
	RepositoryID string
	Base         string
	Candidate    string
}

// Gates is gitauth's view of the gates module.
type Gates interface {
	// Decision returns the gate's decision in the scope; an unknown or foreign
	// gate is ErrNotFound.
	Decision(ctx context.Context, scope domain.Scope, gateID string) (GateDecision, error)
}
