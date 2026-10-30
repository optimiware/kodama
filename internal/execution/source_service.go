package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// The use cases of the runtime v3 routes (T-150) over a source-set run. They are
// the v3 counterparts of RecordPreparationReceipt, RecordBegin and CommitResult
// and obey the same rules: the caller has verified the bearer, the use case
// rechecks everything under the run lock, and a mutation that must be atomic with
// revocation checks the runtime grant through the transaction's GrantFence.

// ErrContextVersionMismatch: an attempt or resource of one runtime context
// version reached another version's use case (409 context_version_mismatch). It
// has no side effects.
var ErrContextVersionMismatch = fmt.Errorf("%w: context_version_mismatch", ErrConflict)

// RecordPreparationReceiptV3 records the trusted checkout receipt of a source-set
// run's one preparation (see RecordPreparationReceipt for the grant claim and
// the fence).
func (s *Service) RecordPreparationReceiptV3(ctx context.Context, scope domain.Scope, run domain.RunID, claim GrantClaim, req runtimev3.PreparationReceiptRequestV3) (ReceiptAck, error) {
	if err := claim.Validate(); err != nil {
		return ReceiptAck{}, err
	}
	var ack ReceiptAck
	_, _, err := s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		if err := s.ownsModel(agg.Run); err != nil {
			return Effects{}, err
		}
		if !agg.Run.SourceRun() {
			return Effects{}, ErrContextVersionMismatch
		}
		fence, err := fenceOf(env)
		if err != nil {
			return Effects{}, err
		}
		if err := fence.ResourceGrantLive(ctx, scope, run, KeyPreparation, claim); err != nil {
			return Effects{}, err
		}
		d, replay, err := agg.RecordPreparationReceiptV3(req, env.Now())
		if err != nil {
			return Effects{}, err
		}
		at := env.Now()
		if prep := agg.Resource(KeyPreparation); prep != nil && prep.ReceiptAt != nil {
			at = *prep.ReceiptAt
		}
		ack = ReceiptAck{Digest: d, RecordedAt: at, Replayed: replay}
		return Effects{}, nil
	})
	return ack, err
}

// RecordBeginV3 journals the command-begin authorization of an attempt of a
// source-set run durably, before the entrypoint starts the command: the
// observation covers every source in order (see RecordBegin).
func (s *Service) RecordBeginV3(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim GrantClaim, req runtimev3.BeginRequestV3) (BeginAck, error) {
	if err := firstOf(scope.Validate(), attempt.Validate()); err != nil {
		return BeginAck{}, ErrNotFound
	}
	if err := claim.Validate(); err != nil {
		return BeginAck{}, err
	}
	run, err := s.d.Store.RunOf(ctx, scope, attempt)
	if err != nil {
		return BeginAck{}, err
	}
	var ack BeginAck
	_, res, err := s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		ack = BeginAck{}
		if err := s.ownsModel(agg.Run); err != nil {
			return Effects{}, err
		}
		at := agg.Attempt(attempt)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		if !agg.Run.SourceRun() || !isContextV3(at.Context) {
			return Effects{}, ErrContextVersionMismatch
		}
		pinned, err := runtimev3.DecodeRequest[runtimev3.ContextV3](at.Context)
		if err != nil {
			return Effects{}, Unavailablef(err, "the pinned context is unreadable")
		}
		if agg.CheckRunDeadline(env.Now()) {
			after, aerr := advance(env, agg)
			return Effects{After: after, Result: beginRevoked{}}, aerr
		}
		fence, err := fenceOf(env)
		if err != nil {
			return Effects{}, err
		}
		if err := fence.AttemptGrantLive(ctx, scope, attempt, claim); err != nil {
			return Effects{}, err
		}
		rec, replay, err := agg.RecordBeginV3(BeginInputV3{Attempt: at, Request: req, Context: pinned}, env.Now())
		if err != nil {
			return Effects{}, err
		}
		ack = BeginAck{AttemptID: rec.AttemptID, BeganAt: rec.BeganAt, WorkspaceState: rec.WorkspaceState, IgnoredPresent: rec.IgnoredPresent, Replayed: replay}
		return Effects{}, nil
	})
	if _, revoked := res.(beginRevoked); err == nil && revoked {
		return BeginAck{}, ErrAttemptRevoked
	}
	return ack, err
}

// ResultCommitV3 is the trusted finalization input of a source-set attempt: the
// v3 result request and the artifact metadata the artifact module verified, as
// ResultCommit, plus the runtime-issued grants (seed, integration) the control
// plane recorded for this attempt, loaded from its own records and never from
// the caller.
type ResultCommitV3 struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
	Request   runtimev3.ResultRequestV3
	Artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
	Issued    []gitv1.GrantClaims
}

// CommitResultV3 records a validated v3 result idempotently under the attempt ID
// and its canonical hash, with the rules of CommitResult. A completed writer's
// commit outputs on its work item's task branch are accepted as checkpoints from
// their push records BEFORE the transaction that settles its writer slot (a
// settled slot refuses them), and the writer is settled without a source commit.
//
//   - an output whose push is still authorized, committing or unknown is not
//     accepted yet: the call fails ErrBusy and nothing changes, the caller
//     repeats it (the same hash replays);
//   - an output the workspace refuses (no applied push record, a secret in the
//     commit, another writer) fails the attempt as an invalid output, like a
//     result that violates its declared outputs; nothing is accepted;
//   - outputs on a run branch or an agent branch are references to evidence only:
//     the head of the source moves with them (runtimev3.CheckResult checked them
//     against the attempt's grants) and nothing else is accepted.
//
// Checkpoints are accepted before the result transaction, as the workspace's
// accept-before-settle ordering requires; they stay accepted if that transaction
// then fails or the attempt ends otherwise (a cancellation, a deadline, a
// conflicting result): the commit was verified against its push record, and the
// workspace's accepted commit never moves back. A repeat of the same result
// finds them accepted and repeats nothing.
//
// A result for an attempt whose context is another version is
// ErrContextVersionMismatch and changes nothing.
func (s *Service) CommitResultV3(ctx context.Context, in ResultCommitV3) (ResultAck, error) {
	if err := firstOf(in.Scope.Validate(), in.AttemptID.Validate()); err != nil {
		return ResultAck{}, ErrNotFound
	}
	if err := in.Request.Validate(); err != nil {
		return ResultAck{}, invalidf("%v", err)
	}
	runID, err := s.d.Store.RunOf(ctx, in.Scope, in.AttemptID)
	if err != nil {
		return ResultAck{}, err
	}
	view, err := s.d.Store.Get(ctx, in.Scope, runID)
	if err != nil {
		return ResultAck{}, err
	}
	at := view.Attempt(in.AttemptID)
	if at == nil {
		return ResultAck{}, ErrNotFound
	}
	if !view.Run.SourceRun() || !isContextV3(at.Context) {
		return ResultAck{}, ErrContextVersionMismatch
	}
	result := in.Request.Result
	refused, err := s.acceptCheckpoints(ctx, view, at, result)
	if err != nil {
		return ResultAck{}, err
	}
	failReason := ReasonNoOutputs
	if refused {
		failReason = ReasonOutputInvalid
	}
	return s.commitResult(ctx, commitInput{
		scope: in.Scope, attempt: in.AttemptID, hash: string(in.Request.ResultHash), result: resultV1Of(result), failReason: failReason,
		check: func(at *Attempt) (error, error) {
			pinned, err := runtimev3.DecodeRequest[runtimev3.ContextV3](at.Context)
			if err != nil {
				return nil, Unavailablef(err, "the pinned context is unreadable")
			}
			if refused {
				return &runtimev1.ContractError{Code: runtimev1.CodeOutputInvalid, Message: "a commit output was refused"}, nil
			}
			return runtimev3.CheckResult(result, pinned, in.Artifacts, in.Issued...), nil
		},
		complete: func(agg *Aggregate, at *Attempt, hash string, now time.Time) error {
			return agg.CompleteAttemptV3(at, hash, result, now)
		},
	})
}

// acceptCheckpoints accepts, outside any transaction, the task-branch commits a
// completed writer of a work item's workspace reports. It reports refused when
// the workspace refused an output (the attempt then fails as an invalid output).
// An attempt that is not running, already has its result, is not the writer, has
// no workspace or reports nothing has nothing to accept.
func (s *Service) acceptCheckpoints(ctx context.Context, view *Aggregate, at *Attempt, result runtimev3.ResultV3) (refused bool, err error) {
	if result.Outcome != runtimev1.OutcomeCompleted || len(result.Commits) == 0 || at.ResultHash != "" || at.Status != AttemptRunning ||
		!at.Writer() || at.WorkspaceID == "" || view.Run.Sources == nil {
		return false, nil
	}
	for _, out := range result.Commits {
		if !taskBranchOutput(view.Run, out) {
			continue
		}
		if s.d.InternalWorkspaces == nil {
			return false, fmt.Errorf("%w: the internal workspace seam is required to accept a writer's checkpoints", ErrNotConfigured)
		}
		switch err := s.d.InternalWorkspaces.AcceptCheckpoint(ctx, at.Scope, at.ID, CommitOutput{
			RepositoryID: out.RepositoryID, Ref: out.Ref, Commit: string(out.Commit), PushID: out.PushID}); {
		case err == nil:
		case errors.Is(err, ErrAdmissionRefused), errors.Is(err, ErrDenied), errors.Is(err, ErrConflict), errors.Is(err, ErrInvalid):
			return true, nil
		case errors.Is(err, ErrBusy), errors.Is(err, ErrStale), errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotConfigured):
			return false, err
		default:
			return false, Unavailablef(err, "the checkpoint could not be accepted")
		}
	}
	return false, nil
}

// taskBranchOutput reports an output on the writable source's branch of a work
// item's run: the task branch, whose commits become the workspace's accepted
// history. A run branch or an agent branch is never a checkpoint.
func taskBranchOutput(run *Run, out runtimev3.CommitOutput) bool {
	if run.Origin.Kind != OriginWorkItem {
		return false
	}
	for _, e := range run.Sources.Entries {
		if e.Access == gitv1.AccessWrite && e.Branch != nil && e.RepositoryID == out.RepositoryID && *e.Branch == out.Ref {
			return true
		}
	}
	return false
}
