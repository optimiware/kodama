package results

import (
	"context"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// The ports of the source-set result route (runtime v3, T-150). An attempt of a
// run that pins a source set reports the refs it updated as commit outputs
// ({repositoryId, ref, commit, pushId}); a commit output is a reference to
// evidence, never evidence, so this module verifies each against the push record
// of the Git service before execution may count it (WS-19, GR-06). The external
// publication path (the v1 and v2 routes: Submit, ProposeSource) is untouched.

// ContextsV3 returns the immutable pinned ContextV3 of an attempt, read without
// locks: the document never changes after dispatch. An attempt of another
// context version is ErrContextVersionMismatch.
type ContextsV3 interface {
	PinnedV3(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev3.ContextV3, error)
}

// Pushes is this module's view of the push records of the internal Git service
// (internal/gitpush, T-130): the one read that decides whether a commit output
// counts.
type Pushes interface {
	// Push returns the current push record of the repository in the scope. A
	// foreign or unknown record, repository or project is ErrNotFound,
	// indistinguishable. The record is read fresh: the answer is a state at a
	// moment (an entry that is committing now may be applied a moment later) and
	// the caller never caches it.
	Push(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error)
}

// IssuedGrants loads the claims of the runtime Git grants (seed, integration) the
// control plane recorded for an attempt: the runtime authority's own records,
// never anything a caller supplied. A commit output on the default branch is
// recognized by one of them (runtimev3.CheckResult).
type IssuedGrants interface {
	Issued(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error)
}

// CommitV3 is the trusted finalization input handed to execution for a source-set
// attempt: the v3 result request, the artifact metadata the artifact owner
// verified and the runtime-issued grants loaded from the control plane's records.
// Execution re-checks all of it under its run lock and accepts a writer's
// task-branch commit outputs as checkpoints from their push records before the
// transaction that settles the writer (the workspace's accept-before-settle
// order).
type CommitV3 struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
	Request   runtimev3.ResultRequestV3
	Artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
	Issued    []gitv1.GrantClaims
}

// FinalizerV3 is the execution owner's v3 result transition.
type FinalizerV3 interface {
	// Accepted reports the result hash the attempt already accepted, if any.
	Accepted(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (hash runtimev1.Digest, found bool, err error)
	// CommitV3 records the result idempotently under attempt ID and hash. An
	// output whose push is not settled yet, or whose checkpoint the workspace
	// cannot accept yet, is a retryable ErrUnavailable and changes nothing.
	CommitV3(ctx context.Context, c CommitV3) (Acknowledgment, error)
}

// DependenciesV3 are the ports of the source-set route. They are added to a
// Service with Service.WithV3 and every one is required: a missing port fails
// the composition, never a check. The Authority, Evidence and the clock are the
// Service's own.
type DependenciesV3 struct {
	Contexts  ContextsV3
	Pushes    Pushes
	Issued    IssuedGrants
	Finalizer FinalizerV3
}
