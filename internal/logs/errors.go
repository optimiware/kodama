package logs

import (
	"errors"
	"fmt"
)

// Failure classes of the log module. Messages are fixed, client-safe texts:
// they never carry log content, provider errors, URLs, storage keys or the
// existence of another tenant's resources.
var (
	// ErrInvalid: the request or a stored value is malformed.
	ErrInvalid = errors.New("logs: invalid request")
	// ErrNotFound: the run, attempt or log is absent, foreign or hidden. The
	// three cases are indistinguishable.
	ErrNotFound = errors.New("logs: not found")
	// ErrDenied: the caller is known but not permitted to read diagnostics.
	ErrDenied = errors.New("logs: denied")
	// ErrUnavailable: a dependency (authority, database, object store,
	// Kubernetes API) failed. It is never an empty successful log.
	ErrUnavailable = errors.New("logs: unavailable")
	// ErrNotConfigured: a required port is absent; operations fail closed.
	ErrNotConfigured = errors.New("logs: not configured")
	// ErrWorkloadNotObserved: the attempt's Job has not been observed yet, so
	// no workload is selected. A similarly named object is never used.
	ErrWorkloadNotObserved = errors.New("logs: workload not observed")
	// ErrWorkloadMissing: the expected Job or Pod is gone.
	ErrWorkloadMissing = errors.New("logs: workload missing")
	// ErrWorkloadMismatch: a Kubernetes object does not belong to the attempt
	// (wrong Job UID, owner or container).
	ErrWorkloadMismatch = errors.New("logs: workload does not match the attempt")
	// ErrNoLog: neither a live workload nor a retained log is available.
	ErrNoLog = errors.New("logs: no log available")
	// ErrBusy: another collector holds the capture claim.
	ErrBusy = errors.New("logs: collection in progress")
	// ErrClaimLost: the collector's claim was taken over (fenced).
	ErrClaimLost = errors.New("logs: claim lost")
	// ErrConflict: a different linkage was already recorded for the capture.
	ErrConflict = errors.New("logs: conflicting linkage")
	// ErrInvalidTransition: the record does not allow the requested change.
	ErrInvalidTransition = errors.New("logs: invalid transition")
	// ErrGenerationsExhausted: the bounded number of capture generations was
	// used; the earlier failure stays recorded.
	ErrGenerationsExhausted = errors.New("logs: capture generations exhausted")
	// ErrFrozenBytesLost: a frozen snapshot's bytes are not stored anywhere any
	// more; that capture is fenced and only a bounded new generation may follow.
	ErrFrozenBytesLost = errors.New("logs: frozen bytes lost")
	// ErrTooManyStreams: the concurrent stream ceiling is reached.
	ErrTooManyStreams = errors.New("logs: too many concurrent streams")
	// ErrLimit: a query or capture bound was exceeded (reported, not failed).
	ErrLimit = errors.New("logs: limit exceeded")
	// ErrProtectionUnknown: retention protection cannot be established, so
	// nothing is eligible for deletion.
	ErrProtectionUnknown = errors.New("logs: retention protection unknown")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func transitionf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}

// Unavailablef wraps a dependency failure without exposing its text to
// transports (which map by class only).
func Unavailablef(cause error, what string) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, what)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, cause)
}
