package execution

import (
	"context"
	"errors"
	"time"

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

func decodeContext(at *Attempt) (runtimev1.Context, error) { return decodeAttemptContext(at) }

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
		if err := s.ownsModel(agg.Run); err != nil {
			return Effects{}, err
		}
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
		if at.Status == AttemptRunning && agg.CheckRunDeadline(now) {
			// run_shared: the pinned total deadline (which covers every attempt's own
			// window and gate waits) passed. The failure intent, the revocation of the
			// attempt and the stop of the resources are persisted here, exactly as
			// reconciliation would, and the late result is kept as evidence only. An
			// already accepted result was answered above and stays replayable.
			agg.RecordEvidence(at.ID, EvidenceLateResult, map[string]any{"resultHash": hash, "attemptStatus": string(at.Status)}, now)
			after, err := advance(env, agg)
			if err != nil {
				return Effects{}, err
			}
			return Effects{After: after, Result: resultOutcome{revoked: true}}, nil
		}
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
		if result.Outcome == runtimev1.OutcomeCompleted {
			if step := agg.Step(at.StepID); step != nil {
				step.CaptureArtifactRefs(result, in.Artifacts)
			}
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
	out, _ := res.(resultOutcome)
	if out.revoked {
		// A late or refused result has no acknowledgment to protect: stop the workload now.
		s.terminateRevoked(ctx, agg)
		return ResultAck{}, ErrAttemptRevoked
	}
	// An accepted result is acknowledged before its workload is stopped. The entrypoint is still waiting for
	// the answer; stopping first killed it mid-acknowledgment, so the same outcome read as a finalization
	// failure at the pod and an unavailable result at the server. The stop follows after a short delay. It is
	// not left to the reconciler: a run that this very result made terminal is no longer scanned, so only this
	// call can still stop its workload (as before, a stop lost to a crash in between is not retried there).
	s.terminateAfterAcknowledgment(ctx, agg, in.AttemptID)
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

// terminateAfterAcknowledgment stops the workloads revoked by an accepted result. Only the attempt whose
// result was just acknowledged waits for the acknowledgment window; siblings cancelled by the failure drain have
// no acknowledgment to protect and stop at once. The delayed stop runs detached from the request, which has ended
// by then, and a panic in a runtime must not take the server down with it.
func (s *Service) terminateAfterAcknowledgment(ctx context.Context, agg *Aggregate, acknowledged domain.AttemptID) {
	if agg == nil || len(agg.Revoked) == 0 || s.d.Runtime == nil {
		return
	}
	var now, later []*Attempt
	for _, at := range agg.Revoked {
		if at.ID == acknowledged && s.o.TerminationDelay > 0 {
			later = append(later, at)
		} else {
			now = append(now, at)
		}
	}
	if len(now) > 0 {
		s.terminateRevoked(ctx, &Aggregate{Run: agg.Run, Revoked: now})
	}
	if len(later) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	delayed := &Aggregate{Run: agg.Run, Revoked: later}
	time.AfterFunc(s.o.TerminationDelay, func() {
		defer func() {
			if r := recover(); r != nil {
				s.log(detached, "terminate after acknowledgment", ErrUnavailable, "panic", true)
			}
		}()
		stopCtx, cancel := context.WithTimeout(detached, time.Minute)
		defer cancel()
		s.terminateRevoked(stopCtx, delayed)
	})
}
