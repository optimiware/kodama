package results

import (
	"context"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// SubmitV3Command is one POST /runtime/v3/attempts/{attemptId}/result call.
type SubmitV3Command struct {
	// Attempt is the attempt named by the route.
	Attempt domain.AttemptID
	Bearer  string
	Request runtimev3.ResultRequestV3
}

// SubmitV3 accepts the final result of an attempt of a source-set run once. The
// order is the frozen one of Submit, with the commit outputs' evidence added
// between validation and finalization:
//
//  1. verify the bearer, the route binding and the source-set wire (an attempt of
//     another wire is ErrContextVersionMismatch);
//  2. validate the normalized request and recompute the canonical hash;
//  3. receipt first: when execution already accepted a hash for the attempt, go
//     straight to commit: the same hash returns the original acknowledgment, a
//     different one result_conflict, and no artifact, grant or push record is read,
//     so an outage or a retention never replaces a receipt decision;
//  4. a new result: validate the artifact evidence and the grant coverage of every
//     commit output against the pinned context, loading the runtime-issued grants
//     from the control plane's own records (a violation reaches execution, which
//     records the durable validation failure and exposes no output);
//  5. verify every commit output against its push record: it must be this
//     attempt's, name the output's ref and commit and be applied (WS-19). An
//     output whose push is authorized, committing or unknown is not accepted yet
//     (503, repeat: a lost confirmation waits for reconciliation, WS-A15); one that
//     can never count is 422 output_invalid;
//  6. protect the consumed artifacts, then commit through execution, which repeats
//     the session check under its run lock and accepts a writer's task-branch
//     outputs as checkpoints through the workspace before the transaction that
//     settles the writer.
//
// Success is never inferred from process exit or logs. An unknown commit outcome is
// ErrUnavailable and the retry observes the same receipt.
func (s *Service) SubmitV3(ctx context.Context, cmd SubmitV3Command) (resp runtimev1.ResultResponse, err error) {
	defer func() { err = Classify(err); s.log(ctx, "submitResultV3", cmd.Attempt, err) }()
	v3 := s.d.V3
	if v3 == nil {
		return resp, Fail(ErrUnavailable, nil, "the source-set result route is not configured")
	}
	if err := cmd.Attempt.Validate(); err != nil {
		return resp, Fail(ErrUnauthenticated, err, "the token is not valid for this attempt")
	}
	p, err := s.d.Authority.Authenticate(ctx, AuthRequest{Attempt: cmd.Attempt, Bearer: cmd.Bearer, Action: ActionResultSubmit, Wire: WireV3})
	if err != nil {
		return resp, err
	}
	if p.AttemptID != cmd.Attempt || p.Action != ActionResultSubmit || p.Scope.Validate() != nil {
		return resp, Fail(ErrUnauthenticated, nil, "the token is not valid for this attempt")
	}
	if ctx, err = s.d.Authority.Bind(ctx, p); err != nil {
		return resp, err
	}
	if err := cmd.Request.Validate(); err != nil {
		return resp, contractError(err)
	}

	commit := CommitV3{Scope: p.Scope, AttemptID: p.AttemptID, Request: cmd.Request}
	_, found, err := v3.Finalizer.Accepted(ctx, p.Scope, p.AttemptID)
	if err != nil {
		return resp, err
	}
	if !found {
		verdict, err := s.validateV3(ctx, v3, p, cmd.Request.Result, &commit)
		if err != nil {
			return resp, err
		}
		commit.Artifacts = verdict.Artifacts
		if err := s.d.Evidence.Protect(ctx, p.Scope, p.AttemptID, verdict.Protect); err != nil {
			return resp, err
		}
	}
	ack, err := v3.Finalizer.CommitV3(ctx, commit)
	if err != nil {
		return resp, err
	}
	if ack.AttemptID != p.AttemptID || ack.ResultHash != cmd.Request.ResultHash {
		return runtimev1.ResultResponse{}, Fail(ErrUnavailable, nil, "the acknowledgment does not match the request")
	}
	resp = runtimev1.ResultResponse{
		AttemptID: ack.AttemptID, ResultHash: ack.ResultHash, AttemptStatus: ack.Status,
		AcceptedAt: runtimev1.Timestamp(ack.AcceptedAt.UTC().Format(time.RFC3339Nano)),
	}
	if err := resp.Validate(); err != nil {
		return runtimev1.ResultResponse{}, Fail(ErrUnavailable, err, "the acknowledgment is not valid")
	}
	return resp, nil
}

// validateV3 is steps 4 and 5: the evidence and grant coverage, then the push
// records of a result that would otherwise succeed.
func (s *Service) validateV3(ctx context.Context, v3 *DependenciesV3, p Principal, res runtimev3.ResultV3, commit *CommitV3) (Verdict, error) {
	pinned, err := v3.Contexts.PinnedV3(ctx, p.Scope, p.AttemptID)
	if err != nil {
		return Verdict{}, err
	}
	if pinned.Scope() != p.Scope || pinned.AttemptID != p.AttemptID || pinned.RunID != p.RunID {
		return Verdict{}, Fail(ErrUnavailable, nil, "the pinned context does not belong to the attempt")
	}
	var evidence map[domain.ArtifactID]ArtifactEvidence
	if ids := ReferencedArtifactsV3(res, pinned); len(ids) > 0 {
		if evidence, err = s.d.Evidence.References(ctx, p.Scope, p.AttemptID, ids); err != nil {
			return Verdict{}, err
		}
	}
	if len(res.Commits) > 0 {
		if commit.Issued, err = v3.Issued.Issued(ctx, p.Scope, p.AttemptID); err != nil {
			return Verdict{}, err
		}
	}
	verdict, err := ValidateV3(res, pinned, evidence, commit.Issued)
	if err != nil || verdict.OutputViolation != nil {
		return verdict, err
	}
	if len(res.Commits) == 0 {
		return verdict, nil
	}
	if err := VerifyCommits(res, p.RunID, p.AttemptID, func(out runtimev3.CommitOutput) (gitv1.PushRecord, error) {
		return v3.Pushes.Push(ctx, p.Scope, out.RepositoryID, out.PushID)
	}); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}
