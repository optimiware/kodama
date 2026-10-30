package entrypoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// resultRequest normalizes a verdict into a validated result and its hash.
func (a *attempt) resultRequest(v verdict) (runtimev1.ResultRequest, error) {
	r := runtimev1.Result{
		Outcome:               v.outcome,
		ExitCode:              v.exit,
		Question:              v.question,
		SourceCommit:          v.source,
		Outputs:               map[string]runtimev1.Value{},
		DiagnosticArtifactIDs: append([]domain.ArtifactID{}, a.diag...),
	}
	if v.reason != "" {
		reason := v.reason
		r.Reason = &reason
	}
	for k, val := range v.outputs {
		r.Outputs[k] = val
	}
	if err := runtimev1.CheckResult(r, a.c, a.artifacts); err != nil {
		return runtimev1.ResultRequest{}, err
	}
	hash, err := runtimev1.ResultHash(r)
	if err != nil {
		return runtimev1.ResultRequest{}, err
	}
	return runtimev1.ResultRequest{ResultHash: hash, Result: r}, nil
}

// submission is a normalized, checked and hashed result on the wire the attempt
// speaks: exactly one of v1 (v1 and v2 wires) and v3 is set.
type submission struct {
	hash    runtimev1.Digest
	outcome runtimev1.Outcome
	v1      *runtimev1.ResultRequest
	v3      *runtimev3.ResultRequestV3
}

// buildResult is resultRequest on the attempt's own wire.
func (a *attempt) buildResult(v verdict) (submission, error) {
	if a.c3 != nil {
		req, err := a.resultRequestV3(v)
		if err != nil {
			return submission{}, err
		}
		return submission{hash: req.ResultHash, outcome: req.Result.Outcome, v3: &req}, nil
	}
	req, err := a.resultRequest(v)
	if err != nil {
		return submission{}, err
	}
	return submission{hash: req.ResultHash, outcome: req.Result.Outcome, v1: &req}, nil
}

// resultRequestV3 normalizes a verdict into a validated v3 result: the pushed
// commits instead of a source commit, checked against the attempt's own Git
// grant and the runtime-issued grants it requested.
func (a *attempt) resultRequestV3(v verdict) (runtimev3.ResultRequestV3, error) {
	r := runtimev3.ResultV3{
		Outcome:               v.outcome,
		ExitCode:              v.exit,
		Question:              v.question,
		Commits:               []runtimev3.CommitOutput{},
		Outputs:               map[string]runtimev1.Value{},
		DiagnosticArtifactIDs: append([]domain.ArtifactID{}, a.diag...),
	}
	if v.outcome == runtimev1.OutcomeCompleted {
		r.Commits = append(r.Commits, v.commits...)
	}
	if v.reason != "" {
		reason := v.reason
		r.Reason = &reason
	}
	for k, val := range v.outputs {
		r.Outputs[k] = val
	}
	if err := runtimev3.CheckResult(r, *a.c3, a.artifacts, a.issued...); err != nil {
		return runtimev3.ResultRequestV3{}, err
	}
	hash, err := runtimev3.ResultHash(r)
	if err != nil {
		return runtimev3.ResultRequestV3{}, err
	}
	return runtimev3.ResultRequestV3{ResultHash: hash, Result: r}, nil
}

// submitResult sends the result on the attempt's wire.
func (a *attempt) submitResult(ctx context.Context, req submission) (runtimev1.ResultResponse, error) {
	if req.v3 != nil {
		return a.submitV3(ctx, *req.v3)
	}
	return a.submit(ctx, *req.v1)
}

// submitV3 is submit for the v3 result: the identical payload is resubmitted while
// the outcome is unknown.
func (a *attempt) submitV3(ctx context.Context, req runtimev3.ResultRequestV3) (runtimev1.ResultResponse, error) {
	backoff := a.s.cfg.SettleBackoff
	for {
		ack, err := a.s.v3.SubmitResultV3(ctx, req)
		if err == nil || !isTransport(err) {
			return ack, err
		}
		a.s.log.Warn("result submission outcome unknown; resubmitting the same payload", "error", redactErr(err))
		if err := sleep(ctx, backoff); err != nil {
			return ack, err
		}
		backoff = min(backoff*2, maxSettleBackoff)
	}
}

// finalize uploads the entrypoint notes, then submits the result and returns
// the exit code. The same payload is retried on transport loss; a conflicting
// accepted result is never rewritten.
func (a *attempt) finalize(v verdict) int {
	log := a.s.log
	log.Info("finalizing")
	if a.authorityLost() {
		// Revoked: nothing is uploaded, proposed or submitted any more.
		log.Error("authority lost; not finalizing", "error", context.Cause(a.runCtx).Error())
		return ExitRevoked
	}
	ctx := a.finalCtx()
	if ab := a.uploadNotes(); ab != nil {
		log.Error("finalization aborted", "error", ab.err.Error())
		return ab.exit
	}
	req, err := a.buildResult(v)
	if err != nil {
		// A result that cannot be valid is replaced by a failed one that can.
		a.note("internal: result for outcome %s was invalid: %v", v.outcome, err)
		log.Error("normalized result invalid", "error", err.Error())
		req, err = a.buildResult(failed(ReasonOutputInvalid, v.exit))
		if err != nil {
			return ExitError
		}
	}
	for downgraded := false; ; downgraded = true {
		ack, err := a.submitResult(ctx, req)
		switch {
		case err == nil:
			if ack.AttemptID != a.c.AttemptID || ack.ResultHash != req.hash {
				log.Error("result acknowledgment does not match the submission", "ackAttempt", ack.AttemptID, "ackHash", ack.ResultHash)
				return ExitFinalization
			}
			log.Info("result acknowledged", "outcome", req.outcome, "hash", req.hash, "attemptStatus", ack.AttemptStatus)
			return ExitOK
		case apiCode(err) == runtimev1.CodeResultConflict:
			log.Error("the server holds a different result for this attempt; not rewritten", "hash", req.hash)
			return ExitResultConflict
		case authorityDenied(err) || a.authorityLost():
			// Revoked, or the bearer token / attempt binding was refused: the
			// result is not rejected on its merits, so it is neither replaced
			// nor retried.
			log.Error("result refused: attempt no longer authorized", "error", err.Error())
			return ExitRevoked
		case ctx.Err() != nil || isTransport(err):
			log.Error("finalization did not complete within its bound", "error", redactErr(err))
			return ExitFinalization
		case downgraded || !rejectedByServer(err):
			// A contract violation in the response (ProtocolError) leaves the
			// outcome unknown: the result may be durable, so it is not
			// replaced by a different one.
			log.Error("result not acknowledged", "error", err.Error())
			return ExitFinalization
		}
		// The server refused the result with a 4xx (for example an output it
		// cannot verify), so nothing was accepted. Replace it once with a
		// failed result.
		log.Error("result rejected; submitting a failed result instead", "error", err.Error())
		req, err = a.buildResult(failed(ReasonResultRejected, v.exit))
		if err != nil {
			return ExitError
		}
	}
}

// authorityDenied reports a refusal of the caller's authority rather than of
// the payload: the attempt is revoked, or the token or Job binding is refused.
func authorityDenied(err error) bool {
	switch apiCode(err) {
	case runtimev1.CodeAttemptRevoked, domain.CodeInvalidCredential, domain.CodeForbidden:
		return true
	}
	return false
}

// rejectedByServer reports a definitive 4xx refusal of the submission.
func rejectedByServer(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500
}

// submit sends the result, retrying the identical payload while the outcome is
// unknown (transport loss) until the finalization budget ends.
func (a *attempt) submit(ctx context.Context, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	backoff := a.s.cfg.SettleBackoff
	for {
		ack, err := a.s.rt.SubmitResult(ctx, req)
		if err == nil || !isTransport(err) {
			return ack, err
		}
		a.s.log.Warn("result submission outcome unknown; resubmitting the same payload", "error", redactErr(err))
		if err := sleep(ctx, backoff); err != nil {
			return ack, err
		}
		backoff = min(backoff*2, maxSettleBackoff)
	}
}

// uploadNotes registers the entrypoint's own explanation of a failure as a
// further diagnostic artifact. The notes are sanitized with the same values as
// the command log and bounded by what is left of the aggregate diagnostic
// budget. They explain the entrypoint's decisions; they say nothing about the
// completeness of the command log (the sidecar does).
func (a *attempt) uploadNotes() *abortError {
	if a.notes.Len() == 0 {
		return nil
	}
	b, err := sanitizeNotes(a.notes.String(), a.logNeedles(), a.diagnosticLeft())
	if err != nil || len(b) == 0 {
		return nil
	}
	f, err := os.CreateTemp(a.s.cfg.ScratchDir, "kodama-notes-*.log")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(b)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return nil
	}
	_, _, ab := a.uploadDiagnostic(filepath.Clean(f.Name()), contract.EntrypointLogName, contract.LogMediaType, int64(len(b)))
	return ab
}
