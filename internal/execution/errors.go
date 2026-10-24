package execution

import (
	"errors"
	"fmt"
)

// Failure classes of the execution module. Each is a sentinel callers classify
// with errors.Is; the concrete error wraps it with a client-safe message that
// never carries secrets, repository contents or foreign identifiers.
var (
	// ErrInvalid: the request is malformed or violates a field rule.
	ErrInvalid = errors.New("execution: invalid request")
	// ErrNotFound: the run or a referenced resource does not exist in the
	// caller's scope. Hidden and foreign references are indistinguishable.
	ErrNotFound = errors.New("execution: not found")
	// ErrDenied / ErrHidden: authorization refusals.
	ErrDenied = errors.New("execution: denied")
	ErrHidden = errors.New("execution: hidden")
	// ErrConflict: stored state contradicts the request (a conflicting source
	// or specification reference, a conflicting result hash).
	ErrConflict = errors.New("execution: conflict")
	// ErrInvalidTransition: the entity does not allow the requested change.
	ErrInvalidTransition = errors.New("execution: invalid transition")
	// ErrUnavailable: a dependency is unavailable, not configured, or the
	// outcome of a write is unknown. Nothing is replayed automatically.
	ErrUnavailable = errors.New("execution: unavailable")
	// ErrNotConfigured: a required port is absent; the operation fails closed.
	ErrNotConfigured = errors.New("execution: not configured")
	// ErrQuotaExceeded: tenant or project concurrency is exhausted.
	ErrQuotaExceeded = errors.New("execution: concurrency limit reached")
	// ErrBusy: the row is locked by another claim; try another candidate.
	ErrBusy = errors.New("execution: busy")
	// ErrStale: observed evidence (writer tip, run version) no longer holds.
	ErrStale = errors.New("execution: stale evidence")
	// ErrAttemptRevoked: the attempt has no current authority; late results and
	// observations are recorded as evidence only.
	ErrAttemptRevoked = errors.New("execution: attempt authority revoked")
	// ErrResultConflict: a different result hash was already accepted.
	ErrResultConflict = errors.New("execution: conflicting result")
	// ErrUnvalidated: a result lacks the trusted validation evidence.
	ErrUnvalidated = errors.New("execution: result is not validated")
	// ErrWorkCancelled: the work item is cancelled and admits no new work.
	ErrWorkCancelled = errors.New("execution: work item cancelled")
	// ErrNotActionable: the work item is an epic.
	ErrNotActionable = errors.New("execution: work item is not actionable")
	// ErrSourceNotReady: the retained source reference is not confirmed.
	ErrSourceNotReady = errors.New("execution: source retention is not confirmed")
	// ErrPurposeDenied: the run purpose is not admitted through this path.
	ErrPurposeDenied = errors.New("execution: run purpose not allowed")
	// ErrNothingRunnable is returned by ClaimRunnable when no run can dispatch.
	ErrNothingRunnable = errors.New("execution: nothing runnable")
	// ErrOutcomeUnknown: an attempt's external effects are unknown until a
	// trusted reconciliation supplies affirmative evidence.
	ErrOutcomeUnknown = errors.New("execution: outcome unknown")
	// ErrAdmissionRefused: definition, configuration, capability or source
	// admission rejected the run before dispatch (HTTP 422).
	ErrAdmissionRefused = errors.New("execution: admission refused")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func transitionf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}

// Invalidf builds an ErrInvalid for adapters validating transport values.
func Invalidf(format string, args ...any) error { return invalidf(format, args...) }

// Unavailablef builds an ErrUnavailable wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

// Refusedf builds an ErrAdmissionRefused with a stable, content-free message.
func Refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAdmissionRefused, fmt.Sprintf(format, args...))
}

// IsBusinessError reports whether err is one of the module's classified
// failures (as opposed to an unclassified infrastructure error).
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrHidden, ErrConflict, ErrInvalidTransition, ErrUnavailable,
		ErrNotConfigured, ErrQuotaExceeded, ErrBusy, ErrStale, ErrAttemptRevoked, ErrResultConflict, ErrUnvalidated, ErrWorkCancelled,
		ErrNotActionable, ErrSourceNotReady, ErrPurposeDenied, ErrNothingRunnable, ErrOutcomeUnknown, ErrAdmissionRefused} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
