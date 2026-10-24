package gateway

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RequestState is the explicit lifecycle of one model request:
//
//	admitted --begin--> forwarding --finalize--> succeeded | failed | cancelled | unknown
//	   |                    |
//	   | finalize(unsent)   | abandon (worker lost)
//	   v                    v
//	 failed               unknown --settle (trusted evidence)--> settled
//
// admitted: the request was counted and holds the run's slot; nothing was sent.
// forwarding: a send may have started; from here the remote effect is uncertain
// until the stream ends. unknown: a worker was lost or its response was interrupted
// without trusted evidence of remote settlement; the request keeps holding the
// slot until explicitly settled. Only admitted, forwarding and unknown hold slots.
type RequestState string

const (
	StateAdmitted   RequestState = "admitted"
	StateForwarding RequestState = "forwarding"
	StateSucceeded  RequestState = "succeeded"
	StateFailed     RequestState = "failed"
	StateCancelled  RequestState = "cancelled"
	StateUnknown    RequestState = "unknown"
	StateSettled    RequestState = "settled"
)

// Valid reports whether s is a known state.
func (s RequestState) Valid() bool {
	switch s {
	case StateAdmitted, StateForwarding, StateSucceeded, StateFailed, StateCancelled, StateUnknown, StateSettled:
		return true
	}
	return false
}

// HoldsSlot reports whether a request in this state occupies the run's single
// unresolved slot.
func (s RequestState) HoldsSlot() bool {
	return s == StateAdmitted || s == StateForwarding || s == StateUnknown
}

// ModelRequest is one admitted model call. It carries identifiers, the
// requested model and numeric bounds only: no prompt, completion, tool
// argument, provider error text or content digest ever belongs to it.
type ModelRequest struct {
	ID        RequestID
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	// Ordinal is the run-wide admission count at admission (1-based).
	Ordinal int64
	Model   string
	// InputBound and OutputMax are the conservative input estimate and the
	// enforced output limit; ChargeBound is their sum, the charge of an
	// uncertain outcome.
	InputBound, OutputMax, ChargeBound int64
	// Recording is the effective content recording decision at admission.
	Recording bool
	// RecordingOutcome is "", "recorded" or "failed".
	RecordingOutcome string
	State            RequestState
	Code             string
	StreamStarted    bool
	AdmittedAt       time.Time
	ForwardingAt     *time.Time
	FinishedAt       *time.Time
	HeartbeatAt      time.Time
}

// NewModelRequest creates an admitted request. bound is what RunBudget.Admit
// returned.
func NewModelRequest(id RequestID, w Workload, ordinal int64, model string, inputBound, outputMax int64, recording bool, now time.Time) (ModelRequest, error) {
	r := ModelRequest{
		ID: id, Scope: w.Scope, RunID: w.RunID, AttemptID: w.AttemptID, Ordinal: ordinal, Model: model,
		InputBound: inputBound, OutputMax: outputMax, ChargeBound: inputBound + outputMax, Recording: recording,
		State: StateAdmitted, AdmittedAt: now.UTC(), HeartbeatAt: now.UTC(),
	}
	if err := firstErr(id.Validate(), w.Validate()); err != nil {
		return ModelRequest{}, err
	}
	if ordinal < 1 || !ValidModelName(model) || inputBound < 0 || outputMax < 1 {
		return ModelRequest{}, fail(ErrInvalid, "the request values are out of range")
	}
	return r, nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Terminal reports a state that can change no further except by evidence.
func (r ModelRequest) Terminal() bool { return !r.State.HoldsSlot() }

// InFlight reports a request whose worker may still finalize or heartbeat.
func (r ModelRequest) InFlight() bool {
	return r.State == StateAdmitted || r.State == StateForwarding
}

// BeginForwarding records that a send is about to start. After it the request
// can no longer be treated as "never sent".
func (r *ModelRequest) BeginForwarding(now time.Time) error {
	if r.State != StateAdmitted {
		return fail(ErrInvalidTransition, "only an admitted request can start forwarding")
	}
	t := now.UTC()
	r.State, r.ForwardingAt, r.HeartbeatAt = StateForwarding, &t, t
	return nil
}

// Beat records that the owning worker is alive.
func (r *ModelRequest) Beat(now time.Time) error {
	if r.State != StateAdmitted && r.State != StateForwarding {
		return fail(ErrSuperseded, "the request is no longer in flight")
	}
	r.HeartbeatAt = now.UTC()
	return nil
}

// Finalize applies the worker's observed outcome exactly once and derives the
// usage record. An outcome that leaves the remote completion unknown (see
// Outcome.HoldsSlot) moves the request to unknown, not to failed or cancelled.
// An unsent or rejected request may finish from admitted or forwarding; every
// other outcome needs forwarding. An unsent request that was cancelled ends
// failed (the only exit from admitted besides forwarding), keeping its
// cancellation code. A request that is already
// terminal or unknown is ErrSuperseded: accounting is exactly-once, and a late
// worker cannot rewrite what reconciliation recorded.
func (r *ModelRequest) Finalize(o Outcome, now time.Time) (Usage, error) {
	if r.State != StateAdmitted && r.State != StateForwarding {
		return Usage{}, fail(ErrSuperseded, "the request is already settled")
	}
	if err := o.validate(); err != nil {
		return Usage{}, err
	}
	if r.State == StateAdmitted && o.Settlement != SettlementNotSent {
		return Usage{}, fail(ErrInvalidTransition, "a request that never started forwarding was not sent")
	}
	if r.State == StateForwarding && o.Settlement == SettlementNotSent && o.Destination.IP != "" {
		return Usage{}, fail(ErrInvalidTransition, "a request that reached a destination may have been sent")
	}
	if r.State == StateAdmitted && o.Result == StateCancelled {
		// The lifecycle leaves admitted only to forwarding or failed: a
		// cancellation before any send ends failed, keeping its reason code,
		// zero charge and not-sent uncertainty.
		o.Result = StateFailed
	}
	u := r.usage(o, now)
	t := now.UTC()
	r.State, r.Code, r.StreamStarted, r.FinishedAt = u.Result, u.Code, o.StreamStarted, &t
	return u, nil
}

func (r ModelRequest) usage(o Outcome, now time.Time) Usage {
	u := Usage{
		RequestID: r.ID, Scope: r.Scope, RunID: r.RunID, AttemptID: r.AttemptID, RequestedModel: r.Model,
		Destination: o.Destination, Result: o.Result, Code: o.Code, RecordedAt: now.UTC(), Basis: BasisEstimated,
	}
	if o.ActualModel != "" && ValidModelName(o.ActualModel) {
		u.ActualModel, u.ModelConfirmed = o.ActualModel, true
	}
	switch o.Settlement {
	case SettlementNotSent:
		u.Uncertainty = UncertaintyNotSent
	case SettlementRejected:
		u.Uncertainty = UncertaintyProviderRejected
	case SettlementObserved:
		in, out := o.Observed.Input, o.Observed.Output
		u.InputTokens, u.OutputTokens = &in, &out
		if in > r.InputBound || out > r.OutputMax {
			// The provider reported more than the verified bounds allow: charge
			// what it says (never less than the bound) and refuse to call the
			// stream a success.
			u.Uncertainty = UncertaintyBoundExceeded
			u.Charged = max(o.Observed.Total(), r.ChargeBound)
			u.Code, u.Result = "bound_exceeded", StateFailed
			return u
		}
		u.Basis, u.Uncertainty, u.Charged = BasisObserved, UncertaintyNone, o.Observed.Total()
	default:
		u.Uncertainty, u.Charged = o.Uncertainty, r.ChargeBound
		if o.HoldsSlot() {
			// The remote completion is unknown: the request is unresolved, not
			// failed or cancelled, until trusted evidence settles it.
			u.Result = StateUnknown
		}
	}
	return u
}

// MarkUnknown records that the worker owning a forwarding request is gone and
// the remote outcome is not known. The request is charged its bound and keeps
// holding the run's slot: lease expiry alone never reopens it.
func (r *ModelRequest) MarkUnknown(now time.Time) (Usage, error) {
	if r.State != StateForwarding {
		return Usage{}, fail(ErrInvalidTransition, "only a forwarding request can become unknown")
	}
	o := Outcome{Result: StateFailed, Code: "worker_lost", Settlement: SettlementUncertain, Uncertainty: UncertaintyWorkerLost}
	u := r.usage(o, now)
	t := now.UTC()
	r.State, r.Code, r.FinishedAt = StateUnknown, o.Code, &t
	u.Result = StateUnknown
	return u, nil
}

// AbandonUnsent finishes an admitted request whose worker is gone before any
// send could have started: nothing reached the provider, so it is charged
// nothing and releases the slot.
func (r *ModelRequest) AbandonUnsent(now time.Time) (Usage, error) {
	if r.State != StateAdmitted {
		return Usage{}, fail(ErrInvalidTransition, "only an admitted request can be abandoned unsent")
	}
	return r.Finalize(Outcome{Result: StateFailed, Code: "unsent_abandoned", Settlement: SettlementNotSent}, now)
}

// Settle records that trusted evidence established the provider's outcome of an
// unknown request. The charge stays; only the slot is released.
func (r *ModelRequest) Settle(now time.Time) error {
	if r.State != StateUnknown {
		return fail(ErrInvalidTransition, "only an unknown request can be settled by evidence")
	}
	r.State = StateSettled
	return nil
}
