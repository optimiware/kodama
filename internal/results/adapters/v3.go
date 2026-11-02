package adapters

import (
	"context"
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The adapters of the source-set result route (runtime v3, T-150).

// ---- execution ---------------------------------------------------------------------

// ResultCommitterV3 is the execution use case that records a source-set result.
// *execution.Service implements it; compose it with the guarded execution store so
// the commit repeats the session check under the run lock.
type ResultCommitterV3 interface {
	CommitResultV3(ctx context.Context, in execution.ResultCommitV3) (execution.ResultAck, error)
}

// ExecutionV3 implements results.FinalizerV3 and results.ContextsV3 over
// execution. Execution accepts a writer's task-branch commit outputs as
// checkpoints from their push records through the workspace BEFORE the
// transaction that settles the writer (a settled slot refuses them); an output
// whose push is not settled is its ErrBusy, which this adapter reports as the
// retryable unavailable the contract's result route has for "repeat".
type ExecutionV3 struct {
	exec *Execution
	svc  ResultCommitterV3
}

var (
	_ results.FinalizerV3 = (*ExecutionV3)(nil)
	_ results.ContextsV3  = (*ExecutionV3)(nil)
)

// NewExecutionV3 requires both collaborators.
func NewExecutionV3(svc ResultCommitterV3, runs Runs) (*ExecutionV3, error) {
	if svc == nil || runs == nil {
		return nil, errors.New("results/adapters: an execution service and store are required")
	}
	return &ExecutionV3{exec: &Execution{runs: runs}, svc: svc}, nil
}

// PinnedV3 implements results.ContextsV3 from the attempt's immutable context. An
// attempt whose context is another version is ErrContextVersionMismatch.
func (e *ExecutionV3) PinnedV3(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev3.ContextV3, error) {
	at, err := e.exec.attempt(ctx, scope, id)
	if err != nil {
		return runtimev3.ContextV3{}, err
	}
	if runtimeauth.WireOf(at.Context) != runtimeauth.WireV3 {
		return runtimev3.ContextV3{}, results.Fail(results.ErrContextVersionMismatch, nil, "the attempt belongs to another runtime version")
	}
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](at.Context)
	if err != nil {
		return runtimev3.ContextV3{}, results.Fail(results.ErrUnavailable, err, "the pinned context is unreadable")
	}
	return c, nil
}

// Accepted implements results.FinalizerV3.
func (e *ExecutionV3) Accepted(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Digest, bool, error) {
	return e.exec.Accepted(ctx, scope, id)
}

// CommitV3 implements results.FinalizerV3. The acknowledgment is built from the
// persisted accepted attempt exactly as Execution.Commit's is: its status and its
// finish time, which is the original acceptance time, so a replay repeats the
// original acknowledgment.
func (e *ExecutionV3) CommitV3(ctx context.Context, c results.CommitV3) (results.Acknowledgment, error) {
	ack, err := e.svc.CommitResultV3(ctx, execution.ResultCommitV3{
		Scope: c.Scope, AttemptID: c.AttemptID, Request: c.Request, Artifacts: c.Artifacts, Issued: c.Issued,
	})
	if err != nil {
		return results.Acknowledgment{}, mapExecutionV3(err)
	}
	return e.exec.acknowledgment(ctx, c.Scope, ack)
}

// mapExecutionV3 adds the version mismatch to mapExecution's classes.
func mapExecutionV3(err error) error {
	if errors.Is(err, execution.ErrContextVersionMismatch) {
		return results.Fail(results.ErrContextVersionMismatch, err, "the attempt belongs to another runtime version")
	}
	return mapExecution(err)
}

// ---- push records -------------------------------------------------------------------

// PushReader is the part of *gitpush.Service the adapter uses.
type PushReader interface {
	Reload(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error)
}

var _ PushReader = (*gitpush.Service)(nil)

// Pushes implements results.Pushes over the push authorizer's current record.
type Pushes struct{ svc PushReader }

var _ results.Pushes = (*Pushes)(nil)

// NewPushes requires the push service.
func NewPushes(svc PushReader) (*Pushes, error) {
	if svc == nil {
		return nil, errors.New("results/adapters: a push service is required")
	}
	return &Pushes{svc: svc}, nil
}

// Push implements results.Pushes: the record as the Git service holds it now. A
// foreign or unknown record, repository or project is not found; anything else
// that is not an answer is unavailable (the result is repeated).
func (p *Pushes) Push(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error) {
	rec, err := p.svc.Reload(ctx, scope, repositoryID, pushID)
	switch {
	case err == nil:
		return rec, nil
	case errors.Is(err, gitpush.ErrNotFound):
		return gitv1.PushRecord{}, results.Fail(results.ErrNotFound, err, "push record not found")
	}
	return gitv1.PushRecord{}, results.Fail(results.ErrUnavailable, err, "the push record could not be read")
}

// ---- runtime-issued grants ------------------------------------------------------------

// GrantRecords is the part of *runtimeauth.Service the adapter uses.
type GrantRecords interface {
	IssuedGitGrants(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error)
}

var _ GrantRecords = (*runtimeauth.Service)(nil)

// Issued implements results.IssuedGrants over the runtime authority's own records
// of the grants it issued to the attempt.
type Issued struct{ svc GrantRecords }

var _ results.IssuedGrants = (*Issued)(nil)

// NewIssued requires the runtime authority service.
func NewIssued(svc GrantRecords) (*Issued, error) {
	if svc == nil {
		return nil, errors.New("results/adapters: a runtime authority is required")
	}
	return &Issued{svc: svc}, nil
}

// Issued implements results.IssuedGrants.
func (i *Issued) Issued(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error) {
	out, err := i.svc.IssuedGitGrants(ctx, scope, attempt)
	if err != nil {
		return nil, results.Fail(results.ErrUnavailable, err, "the issued grants could not be read")
	}
	return out, nil
}
