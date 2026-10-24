package metrics

import (
	"errors"
	"fmt"
)

// Failure classes of the metrics module. Callers classify with errors.Is; the
// concrete error wraps the sentinel with a client-safe message.
var (
	// ErrInvalid: the request is malformed (scope, identifiers, window, batch).
	ErrInvalid = errors.New("metrics: invalid request")
	// ErrDenied: the caller is authenticated but not permitted to read work.
	ErrDenied = errors.New("metrics: denied")
	// ErrHidden: the caller must not learn whether the scope exists.
	ErrHidden = errors.New("metrics: hidden")
	// ErrUnavailable: the authority or database could not answer. Reads have no
	// effects, so the caller may retry after a fresh authorization.
	ErrUnavailable = errors.New("metrics: unavailable")
)

// Invalidf builds an ErrInvalid error.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Unavailablef builds an ErrUnavailable error wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}
