package qa

import (
	"errors"
	"fmt"
)

// Failure classes of the QA mapping. Each is a sentinel callers classify with
// errors.Is; concrete errors wrap it with a client-safe message that never
// repeats storage keys, foreign identifiers or causes.
var (
	// ErrInvalid: the report or request is malformed or violates a field rule.
	ErrInvalid = errors.New("qa: invalid request")
	// ErrNotFound: the attempt (or another addressed resource) does not exist in
	// the caller's scope. Foreign and absent are the same answer.
	ErrNotFound = errors.New("qa: not found")
	// ErrDenied: the caller may not record QA findings. Only the trusted control
	// plane may (mapping is a post-completion control-plane operation, not a
	// runtime action); workloads, people and operators are refused, and a
	// person uses the ordinary work interface. It is also the class of a
	// mapping authority that the work module no longer grants.
	ErrDenied = errors.New("qa: denied")
	// ErrNotReady: the attempt has not finished with an accepted completed result
	// that names a report (AR-06).
	ErrNotReady = errors.New("qa: attempt has no accepted report")
	// ErrContext: the report disagrees with the attempt's pinned acceptance
	// context (work item, content revision, criteria) or the attempt was not
	// pinned to an environment.
	ErrContext = errors.New("qa: report does not match the pinned context")
	// ErrWrongDeployment: the report names a deployment or configuration that is
	// not the verified, succeeded deployment the attempt was pinned to. An
	// absent, foreign and mismatched deployment are one class (DQ-10).
	ErrWrongDeployment = errors.New("qa: wrong deployment")
	// ErrEvidence: a referenced artifact is not verified, usable output of the
	// attempt with the stated digest and kind (DQ-01).
	ErrEvidence = errors.New("qa: evidence not verified")
	// ErrConflict: an existing record contradicts the mapping (a different work
	// item holds the defect identity).
	ErrConflict = errors.New("qa: conflict")
	// ErrUnavailable: a dependency failed or an outcome is unknown. Nothing is
	// lost: repeating the same command converges on the same defects.
	ErrUnavailable = errors.New("qa: unavailable")
)

func failf(class error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", class, fmt.Sprintf(format, args...))
}

func invalidf(format string, args ...any) error { return failf(ErrInvalid, format, args...) }

// Failf builds an error of class with a client-safe message, for adapters.
func Failf(class error, format string, args ...any) error { return failf(class, format, args...) }

// Unavailablef wraps a dependency failure as ErrUnavailable.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return failf(ErrUnavailable, format, args...)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

// Classified reports whether err is one of this module's classes.
func Classified(err error) bool {
	for _, c := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrNotReady, ErrContext, ErrWrongDeployment, ErrEvidence, ErrConflict, ErrUnavailable} {
		if errors.Is(err, c) {
			return true
		}
	}
	return false
}
