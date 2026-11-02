package workspace

import (
	"context"
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Consumer-owned ports of the internally hosted model (ADR 0011). They are
// optional in Dependencies: without them the internal operations fail closed
// with ErrNotConfigured and the external publisher path is unaffected.

// Hosts is the workspace's use of the tenant's Git worker: the subset of
// repository.Hosts (contracts/git/README.md, S2) it needs. Every method is
// idempotent on the repository and its OperationID and safe to retry; a call
// that fails after the request may have been sent has an unknown outcome, so the
// workspace reconciles by observing the ref (ReadRefValues, ReadJournal) and
// never assumes. No SQL transaction is open across any call.
type Hosts interface {
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error)
	CreateBranch(ctx context.Context, tenant domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error)
	SetRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error)
}

// InternalRepository is the repository module's answer for one repository of
// the project.
type InternalRepository struct {
	ID RepositoryID
	// URL is the repository's Git endpoint; CheckoutSecretRef the opaque
	// reference the read-credential resolver of the inspector understands (a
	// service read grant for the repository). Neither is a secret.
	URL               string
	CheckoutSecretRef string
	DefaultBranch     string
	Hosting           gitv1.Hosting
	Status            gitv1.RepositoryStatus
}

// InternalRepositories resolves repositories inside the scope: ErrNotFound for
// an unknown or foreign one, indistinguishably.
type InternalRepositories interface {
	InternalRepository(ctx context.Context, scope domain.Scope, id RepositoryID) (InternalRepository, error)
}

// PushRecords reloads the durable push record of a repository (gitpush.Service
// .Reload implements it). The record is the only evidence a checkpoint is
// accepted from; an unknown or foreign push is a contract error with code
// not_found.
type PushRecords interface {
	Reload(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error)
}

// WriterGate is gitpush's gate for WS-11 on internally hosted repositories
// (gitpush.Service.UnknownBlocksWriter implements it): whether an entry for the
// branch is unknown, in which case the writer must not be reassigned.
type WriterGate interface {
	UnknownBlocksWriter(ctx context.Context, scope domain.Scope, repositoryID, branchRef string) (bool, error)
}

// RefPushes is the workspace's read of the push records that touched one ref of
// a repository (provider: gitpush; see the README, "What gitpush must
// provide"). It is the evidence the workspace needs to decide whether a movement
// of a task branch is human work, whether an attempt's push is still in flight
// and whether archive may complete. A foreign or unknown repository is a
// contract error with code not_found.
type RefPushes interface {
	// UnsettledOnRef reports whether any push record of the repository has an
	// entry for ref that is authorized, committing or unknown.
	UnsettledOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error)
	// AppliedOnRef returns up to limit push records that have an applied entry
	// for ref, newest first (by the order their entries applied).
	AppliedOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]gitv1.PushRecord, error)
}

// Relation says how a commit relates to the commit accepted before it.
type Relation string

const (
	// RelationDescends: the commit is a strict descendant of the accepted
	// commit (a fast-forward of it).
	RelationDescends Relation = "descends"
	// RelationContained: the commit equals or is an ancestor of the accepted
	// commit (an earlier push of the same history).
	RelationContained Relation = "contained"
	// RelationDiverged: neither: the history was rewritten or is unrelated.
	RelationDiverged Relation = "diverged"
)

// InspectRequest asks for the objects a push introduced to be read from the
// internal host with read-only authority.
type InspectRequest struct {
	Repo RepoRef
	// Ref is the task branch the push updated; Commit the pushed value. The
	// branch may have moved on since: the inspector must also find Commit in
	// history that the branch contains.
	Ref    string
	Commit Commit
	// Since is the commit accepted before; the objects reachable from Commit and
	// not from Since are scanned. It is never the zero value.
	Since   Commit
	Needles [][]byte
}

// PushInspector reads a pushed commit's content server-side (the push is
// already applied; an entrypoint-side scan is not trusted). Failures are
// ErrSecretInSource when a needle appears in the introduced objects,
// ErrLimitExceeded, or ErrUnavailable.
type PushInspector interface {
	Inspect(ctx context.Context, req InspectRequest) (Relation, error)
}

// isContract reports whether err carries the contract error code.
func isContract(err error, code domain.ErrorCode) bool {
	var ce *gitv1.ContractError
	return errors.As(err, &ce) && ce.Code == code
}

func contractCode(err error) (domain.ErrorCode, bool) {
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	return "", false
}
