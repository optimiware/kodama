package execution

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ResultCommit is the trusted finalization input of the result-finalization
// task (T-022). It is not a bypass for workload claims: the request must carry
// its canonical hash, and Artifacts is metadata the artifact module verified
// (ownership, kind, completeness). CommitResult re-checks everything against
// the attempt's pinned context under the run lock.
type ResultCommit struct {
	Scope     domain.Scope
	AttemptID domain.AttemptID
	Request   runtimev1.ResultRequest
	Artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
}

// ResultAck acknowledges durable acceptance. AttemptStatus is derived by the
// server: cancellation or timeout can supersede an otherwise completed result,
// and a failed or needs-input result is a failed attempt.
type ResultAck struct {
	AttemptID     domain.AttemptID
	ResultHash    string
	AttemptStatus AttemptStatus
	Replayed      bool
}

type resultOutcome struct {
	ack     ResultAck
	revoked bool
}

func decodeContext(at *Attempt) (runtimev1.Context, error) {
	c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return runtimev1.Context{}, Unavailablef(err, "the pinned context is unreadable")
	}
	return c, nil
}

// CommitResult records a validated result idempotently under attempt ID and
// canonical result hash. Process exit zero is never success: a completed result
// must satisfy the pinned context's declared outputs and source rule, and a
// successful writer's publication must be settled, in the same transaction that
// settles the writer slot. A repeat with the accepted hash returns the original
// acknowledgment; a different hash conflicts without mutation; a result for an
// attempt that lost authority is kept as evidence and cannot progress the run.
func (s *Service) CommitResult(ctx context.Context, in ResultCommit) (ResultAck, error) {
	if err := firstOf(in.Scope.Validate(), in.AttemptID.Validate()); err != nil {
		return ResultAck{}, ErrNotFound
	}
	if err := in.Request.Validate(); err != nil {
		return ResultAck{}, invalidf("%v", err)
	}
	run, err := s.d.Store.RunOf(ctx, in.Scope, in.AttemptID)
	if err != nil {
		return ResultAck{}, err
	}
	hash := string(in.Request.ResultHash)
	agg, res, err := s.d.Store.Update(ctx, in.Scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		at := agg.Attempt(in.AttemptID)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		now := env.Now()
		if at.ResultHash != "" {
			if at.ResultHash != hash {
				return Effects{}, ErrResultConflict
			}
			return Effects{Result: resultOutcome{ack: ResultAck{AttemptID: at.ID, ResultHash: hash, AttemptStatus: at.Status, Replayed: true}}}, nil
		}
		// The deadline binds at the moment of acceptance, not only when the
		// reconciler runs: a result submitted after the startup or execution
		// deadline is late. The timeout and failure drain are persisted here,
		// under the run lock, exactly as reconciliation would.
		if at.Status == AttemptRunning {
			timedOut := false
			for _, f := range agg.CheckDeadlines(now) {
				timedOut = timedOut || f.ID == at.ID
			}
			if timedOut {
				agg.RecordEvidence(at.ID, EvidenceLateResult, map[string]any{"resultHash": hash, "attemptStatus": string(at.Status)}, now)
				after, err := advance(env, agg)
				if err != nil {
					return Effects{}, err
				}
				return Effects{After: after, Result: resultOutcome{revoked: true}}, nil
			}
		}
		if at.Status != AttemptRunning || !agg.Control(at.ID).Live() {
			agg.RecordEvidence(at.ID, EvidenceLateResult, map[string]any{"resultHash": hash, "attemptStatus": string(at.Status)}, now)
			return Effects{Result: resultOutcome{revoked: true}}, nil
		}
		pinned, err := decodeContext(at)
		if err != nil {
			return Effects{}, err
		}
		result := in.Request.Result
		if cerr := runtimev1.CheckResult(result, pinned, in.Artifacts); cerr != nil {
			var ce *runtimev1.ContractError
			if !errors.As(cerr, &ce) || ce.Code != runtimev1.CodeOutputInvalid || result.Outcome != runtimev1.OutcomeCompleted {
				return Effects{}, invalidf("the result does not satisfy the attempt's context")
			}
			// Missing or wrong outputs fail success finalization; the
			// diagnostics stay readable, no success outputs are exposed.
			return s.failFinalization(env, agg, at, hash, result)
		}
		if err := agg.CompleteAttempt(at, hash, result, now); err != nil {
			return Effects{}, err
		}
		after, err := advance(env, agg, at)
		if err != nil {
			return Effects{}, err
		}
		return Effects{After: after, Result: resultOutcome{ack: ResultAck{AttemptID: at.ID, ResultHash: hash, AttemptStatus: at.Status}}}, nil
	})
	if err != nil {
		return ResultAck{}, err
	}
	s.terminateRevoked(ctx, agg)
	out, _ := res.(resultOutcome)
	if out.revoked {
		return ResultAck{}, ErrAttemptRevoked
	}
	return out.ack, nil
}

// failFinalization fails an attempt whose completed result violates its
// declared outputs: a validation failure, never retried.
func (s *Service) failFinalization(env Env, agg *Aggregate, at *Attempt, hash string, result runtimev1.Result) (Effects, error) {
	now := env.Now()
	from := at.Status
	if err := at.fail(ReasonNoOutputs, ClassValidation, now); err != nil {
		return Effects{}, err
	}
	at.ResultHash = hash
	// The submitted result is kept verbatim (its hash covers it); its outputs
	// are never exposed because the attempt and step did not succeed.
	kept := result
	at.Result = &kept
	agg.recordAttempt(at, from, now)
	c := agg.Control(at.ID)
	c.noteLaunch(LaunchStarted)
	c.revoke(now)
	c.requestTermination(now)
	agg.Revoked = append(agg.Revoked, at)
	agg.RecordFailure(ReasonNoOutputs, at.ID, now)
	after, err := advance(env, agg)
	if err != nil {
		return Effects{}, err
	}
	return Effects{After: after, Result: resultOutcome{ack: ResultAck{AttemptID: at.ID, ResultHash: hash, AttemptStatus: at.Status}}}, nil
}

func firstOf(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
