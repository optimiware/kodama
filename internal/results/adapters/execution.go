package adapters

import (
	"context"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
)

// ResultCommitter is the execution use case that records a result.
// *execution.Service implements it; compose it with the guarded execution
// store so the commit repeats the session check under the run lock.
type ResultCommitter interface {
	CommitResult(ctx context.Context, in execution.ResultCommit) (execution.ResultAck, error)
}

// Runs reads persisted runs. execution.Store implements it.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Execution implements results.Finalizer and results.Contexts over execution.
type Execution struct {
	svc  ResultCommitter
	runs Runs
}

var (
	_ results.Finalizer = (*Execution)(nil)
	_ results.Contexts  = (*Execution)(nil)
)

// NewExecution requires both collaborators.
func NewExecution(svc ResultCommitter, runs Runs) (*Execution, error) {
	if svc == nil || runs == nil {
		return nil, errors.New("results/adapters: an execution service and store are required")
	}
	return &Execution{svc: svc, runs: runs}, nil
}

func (e *Execution) attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (*execution.Attempt, error) {
	run, err := e.runs.RunOf(ctx, scope, id)
	if err != nil {
		return nil, mapExecution(err)
	}
	agg, err := e.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, mapExecution(err)
	}
	at := agg.Attempt(id)
	if at == nil {
		return nil, results.Fail(results.ErrNotFound, nil, "the attempt was not found")
	}
	return at, nil
}

// Pinned implements results.Contexts from the attempt's immutable context.
func (e *Execution) Pinned(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Context, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return runtimev1.Context{}, err
	}
	c, _, err := DecodePinned(at.Context)
	if err != nil {
		return runtimev1.Context{}, results.Fail(results.ErrUnavailable, err, "the pinned context is unreadable")
	}
	return c, nil
}

// Accepted implements results.Finalizer.
func (e *Execution) Accepted(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Digest, bool, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return "", false, err
	}
	if at.ResultHash == "" {
		return "", false, nil
	}
	return runtimev1.Digest(at.ResultHash), true, nil
}

// Commit implements results.Finalizer. The acknowledgment is built from the
// persisted accepted attempt: its status and its finish time, which is the
// original acceptance time, so a replay repeats the original acknowledgment.
// execution.ResultAck omits the wire-required acceptedAt; it is derived here
// and checked against the persisted hash. If the read fails the caller gets
// ErrUnavailable and the retry observes the same receipt.
func (e *Execution) Commit(ctx context.Context, c results.Commit) (results.Acknowledgment, error) {
	ack, err := e.svc.CommitResult(ctx, execution.ResultCommit{Scope: c.Scope, AttemptID: c.AttemptID, Request: c.Request, Artifacts: c.Artifacts})
	if err != nil {
		return results.Acknowledgment{}, mapExecution(err)
	}
	return e.acknowledgment(ctx, c.Scope, ack)
}

// acknowledgment builds the wire acknowledgment of a committed result from the
// persisted attempt (shared by the v1 and v3 routes).
func (e *Execution) acknowledgment(ctx context.Context, scope domain.Scope, ack execution.ResultAck) (results.Acknowledgment, error) {
	at, err := e.attempt(ctx, scope, ack.AttemptID)
	if err != nil {
		return results.Acknowledgment{}, results.Fail(results.ErrUnavailable, err, "the acknowledgment could not be read back")
	}
	var accepted time.Time
	if at.FinishedAt != nil {
		accepted = *at.FinishedAt
	}
	if at.ResultHash != ack.ResultHash || accepted.IsZero() || !at.Status.Terminal() {
		return results.Acknowledgment{}, results.Fail(results.ErrUnavailable, nil, "the persisted acknowledgment does not match")
	}
	status, ok := wireStatus(at.Status)
	if !ok {
		return results.Acknowledgment{}, results.Fail(results.ErrUnavailable, nil, "the attempt has no acknowledgable status")
	}
	return results.Acknowledgment{AttemptID: ack.AttemptID, ResultHash: runtimev1.Digest(ack.ResultHash), Status: status, AcceptedAt: accepted}, nil
}

func wireStatus(s execution.AttemptStatus) (runtimev1.AttemptStatus, bool) {
	switch s {
	case execution.AttemptSucceeded:
		return runtimev1.AttemptSucceeded, true
	case execution.AttemptFailed:
		return runtimev1.AttemptFailed, true
	case execution.AttemptCancelled:
		return runtimev1.AttemptCancelled, true
	}
	return "", false
}

// mapExecution translates execution's failure classes.
//
//   - A busy writer slot (an unsettled publication), and stale writer evidence
//     at settlement (the reported sourceCommit is not, or not yet, the
//     attempt's published commit), are "retry": /result has no pending response
//     or publication-conflict code, so the result cannot succeed until the
//     publication settles and the caller repeats it (ErrUnavailable, 503). A
//     commit that is never published stays unacknowledged and the wrapper's
//     bounded finalization ends.
//   - Anything unclassified, including an unknown commit outcome, is
//     unavailable; nothing was acknowledged.
func mapExecution(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return results.Fail(results.ErrNotFound, err, "the attempt was not found")
	case errors.Is(err, execution.ErrAttemptRevoked):
		return results.Fail(results.ErrAttemptRevoked, err, "the attempt is no longer authorized")
	case errors.Is(err, execution.ErrResultConflict):
		return results.Fail(results.ErrResultConflict, err, "a different result was already accepted")
	case errors.Is(err, execution.ErrInvalid):
		return results.Fail(results.ErrInvalidRequest, err, "the result does not satisfy the attempt's context")
	case errors.Is(err, execution.ErrStale), errors.Is(err, execution.ErrBusy):
		return results.Fail(results.ErrUnavailable, err, "the writer's publication is unsettled; repeat the result")
	case isAuthority(err):
		return MapAuthority(err)
	}
	return results.Fail(results.ErrUnavailable, err, "execution could not decide")
}
