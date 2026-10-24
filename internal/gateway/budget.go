package gateway

import "github.com/optimiware/kodama/internal/domain"

// RunBudget is the run-wide accounting of model requests: how many were
// admitted, how many tokens were charged, and which single request (if any)
// is unresolved. It is the one place the three admission rules live:
//
//   - at most one unresolved request per run (a request in flight, or one whose
//     remote outcome is unknown, holds the slot);
//   - at most Limits.MaxRequests admissions, counted at admission;
//   - new admission only while charged tokens are below the allowance.
//
// Charged tokens are observed usage or, when usage is missing, incomplete or
// uncertain, the request bound. A request is admitted while charged <
// allowance and may charge up to RequestBound, and only one request is
// unresolved at a time, so charged tokens stay below allowance + RequestBound.
// This holds for charges within the verified provider/input/output bounds. It is
// an operational token-overshoot bound, not a monetary guarantee, price or
// entitlement. Usage a provider reports above those bounds is a protocol failure
// charged at the larger of the report and the bound (see Usage), never clamped.
//
// Counters only ever increase. The PostgreSQL adapter loads the row FOR UPDATE,
// applies these methods and writes the result in the same transaction.
type RunBudget struct {
	Binding    RunBinding
	Admitted   int64
	Charged    int64
	Unresolved RequestID
}

// CheckLimits validates a request's size and bounds against the run's policy
// without consuming anything.
func (b RunBudget) CheckLimits(requestBytes, inputBound, outputMax int64) error {
	l := b.Binding.Limits
	if requestBytes < 1 || requestBytes > l.MaxRequestBytes {
		return fail(ErrLimitExceeded, "the request body exceeds the run's size limit")
	}
	if outputMax < 1 || outputMax > l.MaxOutputTokens {
		return fail(ErrLimitExceeded, "the requested output exceeds the run's output limit")
	}
	if inputBound < 0 || inputBound > l.MaxInputTokens {
		return fail(ErrLimitExceeded, "the request input exceeds the run's input limit")
	}
	return nil
}

// Admit applies the admission rules and, when they hold, consumes one request
// and takes the slot. The returned bound is what the request is charged when
// its usage is uncertain.
func (b *RunBudget) Admit(id RequestID, requestBytes, inputBound, outputMax int64) (bound int64, err error) {
	if err := id.Validate(); err != nil {
		return 0, err
	}
	if err := b.CheckLimits(requestBytes, inputBound, outputMax); err != nil {
		return 0, err
	}
	if b.Unresolved != "" {
		return 0, fail(ErrBusy, "another model request of this run is unresolved")
	}
	if b.Admitted >= b.Binding.Limits.MaxRequests {
		return 0, fail(ErrLimitExceeded, "the run's request limit is reached")
	}
	if b.Charged >= b.Binding.Limits.TokenAllowance {
		return 0, fail(ErrLimitExceeded, "the run's token allowance is used")
	}
	b.Admitted++
	b.Unresolved = id
	return inputBound + outputMax, nil
}

// Settle charges tokens for the unresolved request and, when release is true,
// frees the slot. An uncertain remote outcome charges but does not release.
// The charge saturates at the largest supported integer instead of overflowing.
func (b *RunBudget) Settle(id RequestID, charge int64, release bool) error {
	if b.Unresolved != id || id == "" {
		return fail(ErrSuperseded, "the request does not hold the run's slot")
	}
	if charge < 0 {
		return fail(ErrInvalid, "a charge cannot be negative")
	}
	if charge > domain.MaxSafeInteger-b.Charged {
		b.Charged = domain.MaxSafeInteger
	} else {
		b.Charged += charge
	}
	if release {
		b.Unresolved = ""
	}
	return nil
}

// Release frees the slot of a request whose uncertainty was resolved by trusted
// evidence. It charges nothing further.
func (b *RunBudget) Release(id RequestID) error {
	if b.Unresolved != id || id == "" {
		return fail(ErrSuperseded, "the request does not hold the run's slot")
	}
	b.Unresolved = ""
	return nil
}
