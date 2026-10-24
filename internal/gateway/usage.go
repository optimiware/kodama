package gateway

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Basis says how a usage record's tokens were obtained. It matches the
// usage.recorded event contract.
type Basis string

const (
	// BasisObserved: complete, valid usage reported by the provider.
	BasisObserved Basis = "observed"
	// BasisEstimated: the charge is the admitted request bound (or zero for a
	// request that provably consumed nothing), not provider-reported usage.
	BasisEstimated Basis = "estimated"
)

// Uncertainty codes of a usage record. "none" is the only code of a complete
// observation; every other code says why the charge is an estimate.
const (
	UncertaintyNone              = "none"
	UncertaintyNotSent           = "not_sent"
	UncertaintyProviderRejected  = "provider_rejected"
	UncertaintyUsageMissing      = "usage_missing"
	UncertaintyUsageInvalid      = "usage_invalid"
	UncertaintyStreamInterrupted = "stream_interrupted"
	UncertaintyClientCancelled   = "client_cancelled"
	UncertaintyAttemptCancelled  = "attempt_cancelled"
	UncertaintyDeadlineExceeded  = "deadline_exceeded"
	UncertaintyProviderError     = "provider_error"
	UncertaintyBoundExceeded     = "bound_exceeded"
	UncertaintyProtocol          = "protocol_violation"
	UncertaintyWorkerLost        = "worker_lost"
)

var uncertaintyCodes = map[string]bool{
	UncertaintyNone: true, UncertaintyNotSent: true, UncertaintyProviderRejected: true, UncertaintyUsageMissing: true,
	UncertaintyUsageInvalid: true, UncertaintyStreamInterrupted: true, UncertaintyClientCancelled: true,
	UncertaintyAttemptCancelled: true, UncertaintyDeadlineExceeded: true, UncertaintyProviderError: true,
	UncertaintyBoundExceeded: true, UncertaintyProtocol: true, UncertaintyWorkerLost: true,
}

// Settlement classifies what the gateway knows about the remote effect of a
// request, which decides its charge.
type Settlement string

const (
	// SettlementNotSent: no request byte reached the provider. Charge zero.
	SettlementNotSent Settlement = "not_sent"
	// SettlementRejected: the provider answered with a client-error status
	// before streaming anything; it processed no tokens. Charge zero.
	SettlementRejected Settlement = "rejected"
	// SettlementObserved: the stream completed and the provider reported valid
	// usage. Charge the observed tokens.
	SettlementObserved Settlement = "observed"
	// SettlementUncertain: the request was sent and usage is missing, invalid or
	// the stream did not complete. Charge the request bound.
	SettlementUncertain Settlement = "uncertain"
)

// TokenUsage is validated provider-reported usage.
type TokenUsage struct{ Input, Output int64 }

// Total is the sum of both counts.
func (u TokenUsage) Total() int64 { return u.Input + u.Output }

// Validate rejects negative or unsupported counts.
func (u TokenUsage) Validate() error {
	if u.Input < 0 || u.Output < 0 || u.Input > domain.MaxSafeInteger/2 || u.Output > domain.MaxSafeInteger/2 {
		return fail(ErrInvalid, "token counts are out of range")
	}
	return nil
}

// Destination is where the request actually went.
type Destination struct {
	// Origin is the approved origin that was contacted.
	Origin string
	// IP is the validated address that was dialed ("" when never connected).
	IP string
}

// Outcome is what the forwarding path observed about one request. The entity
// derives the usage record and charge from it; callers do not choose a charge.
type Outcome struct {
	Result        RequestState // succeeded, failed or cancelled
	Code          string       // stable reason code
	Settlement    Settlement
	Uncertainty   string // required for SettlementUncertain; one of the closed set
	StreamStarted bool   // a byte of the response reached the workload
	Observed      *TokenUsage
	// ActualModel is the model the provider reported for the stream ("" when it
	// reported none). The requested model is recorded separately.
	ActualModel string
	Destination Destination
}

// HoldsSlot reports whether the outcome leaves the remote completion unknown: the
// request was sent and its stream did not complete (truncation, interruption,
// cancellation, deadline, provider or protocol failure). Such a request is
// charged its bound but keeps the run's single slot until trusted evidence
// settles it. A completed response whose usage is missing or invalid is not
// uncertain remote completion: it is charged the bound and releases the slot.
func (o Outcome) HoldsSlot() bool {
	return o.Settlement == SettlementUncertain &&
		o.Uncertainty != UncertaintyUsageMissing && o.Uncertainty != UncertaintyUsageInvalid
}

func (o Outcome) validate() error {
	switch o.Result {
	case StateSucceeded, StateFailed, StateCancelled:
	default:
		return fail(ErrInvalidTransition, "an outcome is succeeded, failed or cancelled")
	}
	if !ValidCode(o.Code) {
		return fail(ErrInvalid, "the outcome code is malformed")
	}
	switch o.Settlement {
	case SettlementNotSent, SettlementRejected:
		if o.StreamStarted || o.Observed != nil || o.Result == StateSucceeded {
			return fail(ErrInvalid, "an unsent or rejected request produced no stream")
		}
	case SettlementObserved:
		if o.Observed == nil || o.Result != StateSucceeded {
			return fail(ErrInvalid, "an observed settlement needs usage and a completed stream")
		}
		if err := o.Observed.Validate(); err != nil {
			return err
		}
	case SettlementUncertain:
		if !uncertaintyCodes[o.Uncertainty] || o.Uncertainty == UncertaintyNone {
			return fail(ErrInvalid, "an uncertain settlement needs an uncertainty code")
		}
	default:
		return fail(ErrInvalid, "unknown settlement")
	}
	return nil
}

// Usage is the durable record of what one request cost and where it went. It is
// operational evidence and run-budget accounting (tokens, attribution,
// uncertainty), not pricing, payment, subscription or entitlement authority. Its
// run and attempt come from the verified workload, the actual model and
// destination from the transport. A model the provider did not confirm is
// recorded as unconfirmed (ModelConfirmed false), never as the requested one.
type Usage struct {
	RequestID      RequestID
	Scope          domain.Scope
	RunID          domain.RunID
	AttemptID      domain.AttemptID
	RequestedModel string
	// ActualModel is the provider-reported model, "" when unconfirmed.
	ActualModel    string
	ModelConfirmed bool
	Destination    Destination
	InputTokens    *int64
	OutputTokens   *int64
	// Charged is what the run's budget was charged.
	Charged     int64
	Basis       Basis
	Uncertainty string
	Result      RequestState
	Code        string
	RecordedAt  time.Time
}

// EventModel is the model name carried by the usage event: the confirmed model
// or, when unconfirmed, the requested one.
func (u Usage) EventModel() string {
	if u.ModelConfirmed {
		return u.ActualModel
	}
	return u.RequestedModel
}
