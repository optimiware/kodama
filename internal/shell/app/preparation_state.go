package app

// The composition materializes an activation credential inside the Kernel's
// Prepare call, after the Kernel committed its launch-attempt marker and took
// its snapshot. A first-credential issue advances the shared execution row
// version, so the Kernel's immediately following preparation-failure commit
// still carries the pre-credential expected version and would lose the
// compare-and-commit boundary to a version conflict. Reconcile then re-reads a
// running, launch-issued execution that is only observed, leaving it running
// forever with a live credential instead of failing preparation.
//
// preparationState rebases exactly that one commit: the preparation-failure
// commit whose expected version is one below the version this composition's own
// credential commit produced for the same execution. Every other transition,
// and every failure whose version does not match the recorded credential
// commit, keeps the ordinary compare-and-commit boundary. The rebase is not a
// version override: the commit is still applied with a compare-and-commit, so a
// genuine concurrent transition that advanced the row further still wins with a
// version conflict. The rebase never fabricates a failure and never changes the
// failure kind, reason or detail.

import (
	"context"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// issuedCredentialVersions reports the execution-row version produced by this
// composition's own activation credential commit.
type issuedCredentialVersions interface {
	issuedVersion(id.ExecutionID) (uint64, bool)
	forgetIssuedVersion(id.ExecutionID)
}

// preparationState decorates the Kernel state port so the preparation-failure
// commit that immediately follows this composition's own credential commit is
// rebased onto the post-credential execution revision.
type preparationState struct {
	inner    kernelengine.State
	versions issuedCredentialVersions
}

var _ kernelengine.State = (*preparationState)(nil)

func newPreparationState(inner kernelengine.State, versions issuedCredentialVersions) *preparationState {
	return &preparationState{inner: inner, versions: versions}
}

func (state *preparationState) Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	return state.inner.Load(ctx, executionID)
}

func (state *preparationState) Scan(ctx context.Context, request kernelengine.ScanRequest) ([]id.ExecutionID, error) {
	return state.inner.Scan(ctx, request)
}

// Commit applies one Kernel transition. Only a preparation-failure commit whose
// expected version is exactly one below the version this composition's own
// credential commit produced is rebased. The inner state's compare-and-commit
// still decides the outcome, so a concurrent transition wins.
func (state *preparationState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	rebased := false
	if state.versions != nil && commit.Failure != nil &&
		commit.Failure.Kind == kernel.FailurePreparation &&
		commit.ExpectedExecutionVersion != 0 {
		if version, ok := state.versions.issuedVersion(commit.ExecutionID); ok &&
			version != 0 && commit.ExpectedExecutionVersion+1 == version {
			commit.ExpectedExecutionVersion = version
			rebased = true
		}
	}
	updated, err := state.inner.Commit(ctx, commit)
	if rebased {
		// The one failure commit that can follow the credential commit has been
		// attempted. Whether it committed or lost a genuine concurrent
		// transition, the recorded revision is no longer needed.
		state.versions.forgetIssuedVersion(commit.ExecutionID)
	}
	return updated, err
}
