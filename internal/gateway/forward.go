package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// InvokeRequest is one workload model call. Attempt and Bearer come from the
// route and the Authorization header; Body is the raw request body. Sink is the
// only way bytes reach the workload.
type InvokeRequest struct {
	Attempt domain.AttemptID
	Bearer  string
	Body    []byte
	Sink    Sink
}

// InvokeResult describes what was recorded. It is returned with an error when
// the request was admitted but did not succeed; callers use Sink.Started() to
// decide whether an error response can still be written.
type InvokeResult struct {
	RequestID RequestID
	State     RequestState
	Code      string
	Usage     *Usage
	// RecordingFailed reports that content recording was on and failed. The
	// workload's result and the usage record are unaffected.
	RecordingFailed bool
}

// authorityLost is the cancellation cause of a failed periodic authority check.
type authorityLost struct{ err error }

func (e *authorityLost) Error() string { return "gateway: authority lost" }
func (e *authorityLost) Unwrap() error { return e.err }

// Invoke runs one streaming chat completion for an attempt.
//
// Order, each step failing closed:
//
//  1. verify the bearer for the route's attempt and model.invoke;
//  2. decode the body strictly and compute the conservative input bound;
//  3. admit atomically (authority recheck, binding, model/limit checks,
//     request count, the run's single unresolved slot). An unknown commit
//     outcome means nothing is sent;
//  4. open the exact pinned credential outside any transaction;
//  5. record the start of forwarding (authority rechecked again);
//  6. stream, flushing each validated frame, while a periodic check re-verifies
//     the attempt and refreshes the request's liveness; when the loop ends for
//     any reason the provider stream is closed immediately, before any database
//     work, and that closure is not evidence of remote completion;
//  7. settle exactly once: usage, token charge, outcome, audit and
//     usage.recorded commit together, with a bounded context that survives the
//     caller's cancellation.
//
// The provider call is never repeated: a failed finalization is retried only as
// a database write, and an explicit workload retry is a new admission.
func (s *Service) Invoke(ctx context.Context, in InvokeRequest) (InvokeResult, error) {
	if in.Sink == nil {
		return InvokeResult{}, fail(ErrInvalid, "a response sink is required")
	}
	if err := in.Attempt.Validate(); err != nil {
		return InvokeResult{}, fail(ErrUnauthenticated, "the workload credential is not valid")
	}
	w, err := s.cfg.Workloads.Authorize(ctx, in.Attempt, in.Bearer)
	if err != nil {
		return InvokeResult{}, authError(err)
	}
	if w.AttemptID != in.Attempt || w.Validate() != nil {
		return InvokeResult{}, fail(ErrUnauthenticated, "the workload credential is not valid")
	}
	comp, err := s.cfg.Decoder.Decode(in.Body)
	if err != nil {
		return InvokeResult{}, Classify(err)
	}
	bound, err := s.cfg.Estimator.InputBound(comp)
	if err != nil {
		return InvokeResult{}, Classify(err)
	}
	id, err := NewRequestID()
	if err != nil {
		return InvokeResult{}, err
	}
	adm, err := s.cfg.Store.Admit(ctx, AdmitCommand{
		Workload: w, RequestID: id, Model: comp.Model, RequestBytes: int64(len(in.Body)), InputBound: bound,
		OutputMax: comp.MaxTokens, Now: s.now(), RecordingAvailable: s.cfg.Recorder != nil,
	})
	if err != nil {
		// A refusal consumed nothing. An unknown commit outcome is treated as
		// "do not send": the request, if it was admitted, is reconciled as unsent.
		return InvokeResult{}, Classify(err)
	}
	return s.forward(ctx, w, adm, comp, in.Sink)
}

func authError(err error) error {
	switch {
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrForbidden):
		return err
	}
	return FailWith(ErrUnavailable, err, "the workload authority could not answer")
}

func (s *Service) forward(ctx context.Context, w Workload, adm Admission, comp Completion, sink Sink) (InvokeResult, error) {
	req := adm.Request
	res := InvokeResult{RequestID: req.ID}
	rctx, cancelRequest := context.WithTimeout(ctx, s.cfg.Timeouts.Request)
	defer cancelRequest()
	sctx, stop := context.WithCancelCause(rctx)
	defer stop(nil)

	var fr *frameRecorder
	if adm.Recording && s.cfg.Recorder != nil {
		fr = newFrameRecorder(s.cfg.MaxRecordedBytes)
	}

	settle := func(o Outcome) (Usage, error) {
		u, err := s.finish(ctx, w, req, o)
		if err == nil {
			res.State, res.Code, res.Usage = u.Result, u.Code, &u
		}
		return u, err
	}
	// unsent finishes a request nothing of which reached the provider.
	unsent := func(code string, state RequestState, cause error) (InvokeResult, error) {
		o := Outcome{Result: state, Code: code, Settlement: SettlementNotSent, Destination: Destination{Origin: adm.Binding.Endpoint.Origin()}}
		if _, err := settle(o); err != nil {
			return res, err
		}
		return res, cause
	}

	cred, err := s.cfg.Credentials.Open(sctx, w, adm.Binding, req.ID)
	if err != nil {
		if sctx.Err() != nil {
			code, state, _ := s.interruption(ctx, sctx)
			return unsent(code, state, FailWith(ErrUnavailable, sctx.Err(), "the request was interrupted"))
		}
		return unsent("credential_unavailable", StateFailed, Classify(err))
	}
	defer cred.Destroy()

	if err := s.cfg.Store.BeginForwarding(sctx, w, req.ID, s.now()); err != nil {
		code, state := "authority_lost", StateFailed
		if sctx.Err() != nil {
			code, state, _ = s.interruption(ctx, sctx)
		}
		return unsent(code, state, Classify(err))
	}

	stopHeartbeat := s.keepAlive(sctx, stop, w, req.ID)
	stream, err := s.cfg.Provider.Open(sctx, ProviderCall{
		Endpoint: adm.Binding.Endpoint, Credential: cred, Completion: comp, MaxTokens: req.OutputMax,
	})
	if err != nil {
		stopHeartbeat()
		o, cerr := s.openFailure(ctx, sctx, adm, err)
		if _, ferr := settle(o); ferr != nil {
			return res, ferr
		}
		return res, cerr
	}
	// The upstream call is stopped as soon as the forwarding loop ends, before
	// any database work: outcome persistence may be slow or blocked and must not
	// keep provider work running. Closing the connection is not evidence that the
	// provider stopped, so it changes nothing about the recorded outcome.
	var closeOnce sync.Once
	closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }
	defer closeStream()

	var (
		model    string
		usage    *TokenUsage
		usageBad bool
		done     bool
		loopErr  error
	)
	for {
		ch, err := stream.Next(sctx)
		if err != nil {
			loopErr = err
			break
		}
		if ch.Done {
			done = true
			break
		}
		if ch.Model != "" && model == "" {
			model = ch.Model
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		usageBad = usageBad || ch.UsageInvalid
		if len(ch.Data) == 0 {
			continue
		}
		if err := sink.Frame(sctx, ch.Data); err != nil {
			loopErr = &sinkError{err: err}
			break
		}
		fr.add(ch.Data)
	}
	closeStream()
	stopHeartbeat()

	o := Outcome{StreamStarted: sink.Started(), ActualModel: model, Destination: stream.Destination()}
	var clientErr error
	switch {
	case done && usageBad:
		o.Result, o.Code, o.Settlement, o.Uncertainty = StateSucceeded, "usage_invalid", SettlementUncertain, UncertaintyUsageInvalid
	case done && usage == nil:
		o.Result, o.Code, o.Settlement, o.Uncertainty = StateSucceeded, "usage_missing", SettlementUncertain, UncertaintyUsageMissing
	case done:
		o.Result, o.Code, o.Settlement, o.Observed = StateSucceeded, "completed", SettlementObserved, usage
	default:
		o.Settlement = SettlementUncertain
		o.Code, o.Uncertainty, o.Result = s.streamFault(ctx, sctx, loopErr)
		clientErr = FailWith(ErrProvider, loopErr, "the model response did not complete")
	}
	u, ferr := settle(o)
	if ferr != nil {
		s.failSink(ctx, sink, "unavailable")
		return res, ferr
	}
	var out error
	switch {
	case u.Result == StateSucceeded:
		if err := sink.Done(rctx); err != nil {
			s.cfg.Logger.Debug("the terminator could not be written", "requestId", string(req.ID))
		}
	case done:
		// The provider reported usage above the verified bounds: a protocol
		// failure, never a success.
		s.failSink(ctx, sink, u.Code)
		out = fail(ErrProvider, "the model response violated the supported protocol")
	default:
		s.failSink(ctx, sink, u.Code)
		out = clientErr
	}
	if fr != nil {
		res.RecordingFailed = s.record(ctx, w, req.ID, comp, fr)
	}
	s.cfg.Logger.Info("model request settled", "requestId", string(req.ID), "attemptId", string(w.AttemptID),
		"state", string(res.State), "code", res.Code, "charged", u.Charged)
	return res, out
}

// failSink reports a sanitized in-stream failure when a response is already
// committed. It never writes a terminator.
func (s *Service) failSink(ctx context.Context, sink Sink, code string) {
	if !sink.Started() {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = sink.Fail(fctx, code)
}

type sinkError struct{ err error }

func (e *sinkError) Error() string { return "gateway: the response could not be written" }
func (e *sinkError) Unwrap() error { return e.err }

// keepAlive rechecks the attempt's current authority and refreshes the
// request's liveness every Recheck while the call is in flight. A failed check
// cancels the call with an authorityLost cause. The returned function stops it
// and waits for it, so no check overlaps the settlement.
func (s *Service) keepAlive(ctx context.Context, stop context.CancelCauseFunc, w Workload, id RequestID) func() {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.cfg.Timeouts.Recheck)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.cfg.Store.Heartbeat(ctx, w, id, s.now()); err != nil {
					if ctx.Err() == nil {
						stop(&authorityLost{err: err})
					}
					return
				}
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(quit)
		}
		<-done
	}
}

// interruption classifies a cancelled call: the attempt lost its authority, the
// deadline passed, or the caller went away.
func (s *Service) interruption(parent, sctx context.Context) (code string, state RequestState, uncertainty string) {
	var lost *authorityLost
	cause := context.Cause(sctx)
	switch {
	case errors.As(cause, &lost):
		if errors.Is(lost.err, ErrForbidden) || errors.Is(lost.err, ErrUnauthenticated) {
			return "attempt_cancelled", StateCancelled, UncertaintyAttemptCancelled
		}
		return "authority_unavailable", StateCancelled, UncertaintyAttemptCancelled
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(parent.Err(), context.DeadlineExceeded):
		return "deadline_exceeded", StateFailed, UncertaintyDeadlineExceeded
	}
	return "client_cancelled", StateCancelled, UncertaintyClientCancelled
}

// streamFault maps the end of a stream that did not reach its terminator.
func (s *Service) streamFault(parent, sctx context.Context, err error) (code, uncertainty string, state RequestState) {
	var se *sinkError
	var fault *StreamError
	switch {
	case errors.As(err, &se):
		return "client_cancelled", UncertaintyClientCancelled, StateCancelled
	case sctx.Err() != nil:
		c, st, u := s.interruption(parent, sctx)
		return c, u, st
	case errors.As(err, &fault):
		switch fault.Kind {
		case FaultTruncated:
			return "stream_truncated", UncertaintyStreamInterrupted, StateFailed
		case FaultIdle:
			return "stream_idle", UncertaintyStreamInterrupted, StateFailed
		case FaultTooLarge:
			return "response_too_large", UncertaintyBoundExceeded, StateFailed
		}
		return "protocol_violation", UncertaintyProtocol, StateFailed
	}
	return "stream_interrupted", UncertaintyStreamInterrupted, StateFailed
}

// openFailure classifies a failure to open the provider stream. Whether request
// bytes may have reached the provider decides the charge: a failure before any
// send charges nothing; a client-error status or redirect (the provider
// processed no tokens) charges nothing; every other failure after a possible
// send charges the request bound.
func (s *Service) openFailure(parent, sctx context.Context, adm Admission, err error) (Outcome, error) {
	origin := Destination{Origin: adm.Binding.Endpoint.Origin()}
	notSent := func(code string, state RequestState) Outcome {
		return Outcome{Result: state, Code: code, Settlement: SettlementNotSent, Destination: origin}
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		if sctx.Err() != nil {
			// The call was interrupted around the send: it may have been sent.
			c, st, u := s.interruption(parent, sctx)
			return Outcome{Result: st, Code: c, Settlement: SettlementUncertain, Uncertainty: u, Destination: origin},
				FailWith(ErrUnavailable, sctx.Err(), "the request was interrupted")
		}
		if errors.Is(err, ErrCredential) {
			return notSent("credential_unavailable", StateFailed), Classify(err)
		}
		return notSent("request_not_sent", StateFailed), Classify(err)
	}
	dest := pe.Destination
	if dest.Origin == "" {
		dest.Origin = origin.Origin
	}
	cerr := FailWith(ErrProvider, err, "the provider could not complete the request")
	switch pe.Kind {
	case ProviderDestination:
		return notSent("destination_not_approved", StateFailed), FailWith(ErrDestination, err, "the destination is not approved")
	case ProviderConnect:
		return notSent("provider_unreachable", StateFailed), cerr
	case ProviderRedirect:
		return Outcome{Result: StateFailed, Code: "provider_redirect", Settlement: SettlementRejected, Destination: dest}, cerr
	case ProviderStatus:
		if pe.Status >= 400 && pe.Status < 500 {
			return Outcome{Result: StateFailed, Code: "provider_rejected", Settlement: SettlementRejected, Destination: dest}, cerr
		}
		return Outcome{Result: StateFailed, Code: "provider_error", Settlement: SettlementUncertain, Uncertainty: UncertaintyProviderError, Destination: dest}, cerr
	case ProviderTimeout:
		return Outcome{Result: StateFailed, Code: "provider_timeout", Settlement: SettlementUncertain, Uncertainty: UncertaintyDeadlineExceeded, Destination: dest}, cerr
	}
	if sctx.Err() != nil {
		c, st, u := s.interruption(parent, sctx)
		return Outcome{Result: st, Code: c, Settlement: SettlementUncertain, Uncertainty: u, Destination: dest}, cerr
	}
	return Outcome{Result: StateFailed, Code: "protocol_violation", Settlement: SettlementUncertain, Uncertainty: UncertaintyProtocol, Destination: dest}, cerr
}

// finish settles one request exactly once. Retries are database recovery only:
// the provider call is never repeated. A commit whose outcome is unknown is
// reread by request identity before anything is retried. The context survives
// the caller's cancellation but is bounded.
func (s *Service) finish(ctx context.Context, w Workload, req ModelRequest, o Outcome) (Usage, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeouts.Finalize)
	defer cancel()
	var last error
	unknown := false // an earlier attempt's commit outcome was not learned
	for i := 0; i < s.cfg.Timeouts.FinalizeAttempts; i++ {
		u, err := s.cfg.Store.Finalize(cctx, FinalizeCommand{
			Scope: w.Scope, RunID: w.RunID, AttemptID: w.AttemptID, RequestID: req.ID, Outcome: o, Now: s.now(),
		})
		if err == nil {
			return u, nil
		}
		last = err
		switch {
		case errors.Is(err, ErrSuperseded):
			// After an unknown commit, "already settled" may be our own commit
			// having landed: the usage record then carries our outcome code, which
			// no reconciliation code equals.
			if unknown {
				if u, ok := s.landed(cctx, w.Scope, req.ID); ok && u.Code == o.Code {
					return u, nil
				}
			}
			return Usage{}, err
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid), errors.Is(err, ErrInvalidTransition):
			return Usage{}, err
		case errors.Is(err, ErrUnknownCommit):
			unknown = true
			if u, ok := s.landed(cctx, w.Scope, req.ID); ok {
				return u, nil
			}
		}
		select {
		case <-cctx.Done():
			return Usage{}, FailWith(ErrUnavailable, last, "the outcome could not be recorded in time")
		case <-time.After(s.cfg.Timeouts.FinalizeBackoff):
		}
	}
	return Usage{}, FailWith(ErrUnavailable, last, "the outcome could not be recorded")
}

// landed rereads a request after a finalization with an unknown commit outcome.
// It reports the usage when the commit is visible.
func (s *Service) landed(ctx context.Context, scope domain.Scope, id RequestID) (Usage, bool) {
	m, err := s.cfg.Store.Request(ctx, scope, id)
	if err != nil || m.InFlight() {
		return Usage{}, false
	}
	u, err := s.cfg.Store.Usage(ctx, scope, id)
	if err != nil {
		return Usage{}, false
	}
	return u, true
}
