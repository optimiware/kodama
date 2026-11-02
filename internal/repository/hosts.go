package repository

import (
	"context"
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Hosts is the control plane's view of one tenant's Git worker. The tenant
// selects the worker (the adapter resolves it with Workers); no method can
// address another tenant. It is the consumer-owned port frozen in
// contracts/git/README.md (S2); T-128/T-129 provide the adapters.
//
// Every method is idempotent on the repository ID (and OperationID where the
// request has one) and safe to retry. A call that fails after the request may
// have been sent has Outcome Unknown: the caller retries the same request, and
// never assumes it was applied or not. No SQL lock or transaction is held
// across a Hosts call.
type Hosts interface {
	// Initialize creates the bare repository at the worker's ID-derived path with
	// the server-owned configuration. Created is false when it already existed.
	Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error)
	// ReadRefs lists one namespace page; Empty is true when the repository has no refs.
	ReadRefs(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error)
	// ReadRefValues reads exact refs under the worker's mutation lock. Quiescent
	// is true only if no receive or server operation was in progress.
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	// ReadJournal returns entries after SinceSequence for one receive or one ref.
	// HeadSequence below a head the caller already saw means an older volume.
	ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error)
	// ReceiveLiveness reports whether the worker still runs the receive and which
	// instance answered.
	ReceiveLiveness(ctx context.Context, tenant domain.TenantID, repositoryID, receiveID string) (gitv1.ReceiveLivenessResponse, error)
	// SetRetainedRef pins a commit at refs/kodama/runs/<runId>/<n>; a different
	// existing value is retained_ref_exists.
	SetRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error)
	// DeleteRetainedRef releases a pin whose value is ExpectedCommit; another value is ref_changed.
	DeleteRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error)
	// CreateBranch creates a Kodama-managed branch (refs/heads/kodama/work/<workItemId>,
	// an agent branch or a run branch; never the default branch, a tag or a
	// server ref) at Commit, for the workspace's task branch (WS-17, T-147). The
	// worker journals it under OperationID. Idempotent on the ref: already at
	// Commit is success with Created false; at another commit it is
	// branch_exists (409) and nothing changes. After an Unknown outcome the
	// caller repeats the identical request: the worker first observes the ref
	// (and the journal for OperationID) and converges: the ref at Commit is
	// success whether or not the earlier attempt finished its journal entry.
	CreateBranch(ctx context.Context, tenant domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error)
	// DeleteBranch prunes a task, agent or run branch whose value is ExpectedCommit.
	DeleteBranch(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error)
	// Maintain runs git maintenance, serialized with pushes.
	Maintain(ctx context.Context, tenant domain.TenantID, req gitv1.MaintainRequest) (gitv1.MaintainResponse, error)
}

// Outcome classifies a failed Hosts call.
type Outcome string

const (
	// Refused: the worker answered with a contract error (Err); nothing changed.
	// Do not retry the same request unchanged.
	Refused Outcome = "refused"
	// Unavailable: the request provably was not delivered (no worker, connection
	// refused, not ready). Retry with backoff.
	Unavailable Outcome = "unavailable"
	// Unknown: the request may have been applied (timeout or reset after sending,
	// an unreadable response). Retry the identical request; the repository ID and
	// OperationID make the repeat safe.
	Unknown Outcome = "unknown"
)

// CallError is the only error a Hosts adapter returns for a failed call.
type CallError struct {
	Outcome Outcome
	Err     *gitv1.ContractError // set for Refused
}

func (e *CallError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("repository: hosts call %s: %s", e.Outcome, e.Err.Error())
	}
	return fmt.Sprintf("repository: hosts call %s", e.Outcome)
}

// Unwrap exposes the contract error of a refusal.
func (e *CallError) Unwrap() error {
	if e.Err == nil {
		return nil
	}
	return e.Err
}

// outcomeOf classifies any error of a Hosts call. A cancelled or expired
// context is the caller's, never an outcome; every other error that is not a
// CallError is treated as Unknown, because the request may have been sent.
func outcomeOf(err error) Outcome {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.Outcome
	}
	return Unknown
}

// Workers resolves a tenant's Git worker (S2/S3; provider T-129). It is the
// discovery port of the Hosts adapters, declared here beside the port it
// serves.
type Workers interface {
	// Endpoint returns the Service endpoint of the tenant's worker. A tenant
	// without a worker (not yet provisioned, being replaced) is an unavailable
	// error, retryable; it never returns another tenant's endpoint. The result is
	// a cache hint: callers must tolerate a replaced worker (new WorkerInstance).
	Endpoint(ctx context.Context, tenant domain.TenantID) (Endpoint, error)
}

// Endpoint is a worker's address.
type Endpoint struct {
	BaseURL string // https://<service>.<namespace>.svc, for gitv1.WorkerBasePath
}
