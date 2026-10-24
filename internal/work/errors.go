package work

import (
	"errors"
	"fmt"
)

// Failure classes of the work module. Each is a sentinel callers classify with
// errors.Is; the concrete error wraps it with a client-safe message. Transport
// adapters map them to their own status codes (see internal/work/http).
var (
	// ErrInvalid: the request is malformed or violates a field rule.
	ErrInvalid = errors.New("work: invalid request")
	// ErrNotFound: the work item or a referenced resource does not exist in the
	// caller's scope. Hidden references are indistinguishable from missing ones.
	ErrNotFound = errors.New("work: not found")
	// ErrDenied: the caller is authenticated but not permitted.
	ErrDenied = errors.New("work: denied")
	// ErrHidden: the caller must not learn whether the resource exists.
	ErrHidden = errors.New("work: hidden")
	// ErrVersionConflict: the expected resource version is stale.
	ErrVersionConflict = errors.New("work: version conflict")
	// ErrInvalidTransition: the requested state change is not an allowed edge,
	// or the item's state forbids the operation.
	ErrInvalidTransition = errors.New("work: invalid transition")
	// ErrPrerequisite: an allowed edge has an unmet condition (missing
	// specification, blocked item, unsatisfied dependency, missing evidence,
	// missing reason or unacknowledged finding).
	ErrPrerequisite = errors.New("work: prerequisite not met")
	// ErrWorkspaceBusy: a writer attempt is unsettled; cancel the run and retry
	// after settlement.
	ErrWorkspaceBusy = errors.New("work: workspace busy")
	// ErrWIPLimit: active work-in-progress capacity is exhausted.
	ErrWIPLimit = errors.New("work: active work limit reached")
	// ErrStaleSubject: the observed content revision, candidate or evidence is
	// no longer the current review subject.
	ErrStaleSubject = errors.New("work: stale review subject")
	// ErrNotHuman: the operation requires a human decision-maker.
	ErrNotHuman = errors.New("work: a human decision is required")
	// ErrUnavailable: a required dependency (database, authority, guard) is
	// unavailable or the outcome of a write is unknown. The caller rereads the
	// resource before retrying; nothing is replayed automatically.
	ErrUnavailable = errors.New("work: unavailable")
	// ErrNotActionable: the work item is an epic, which has no workspace and
	// admits no execution.
	ErrNotActionable = errors.New("work: not an actionable work item")
	// ErrCancelled: the work item is cancelled and admits no new work.
	ErrCancelled = errors.New("work: work item cancelled")
	// ErrNoChange is returned by an update function that decided nothing needs
	// to be written. It never reaches callers of the Service.
	ErrNoChange = errors.New("work: no change")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func prerequisite(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPrerequisite, fmt.Sprintf(format, args...))
}

// Invalidf builds an ErrInvalid error for adapters that validate transport
// values before reaching the entity.
func Invalidf(format string, args ...any) error { return invalid(format, args...) }

// Unavailablef builds an ErrUnavailable error wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}
