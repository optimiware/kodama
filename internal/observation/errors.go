package observation

import (
	"errors"
	"fmt"
)

// Failure classes of the observation module. Transport adapters map them to
// their own codes with errors.Is. Any error that is not one of these classes is a
// dependency failure and never grants or silently skips anything.
var (
	// ErrInvalid: a scope, filter or position violates a value rule.
	ErrInvalid = errors.New("observation: invalid request")
	// ErrNotFound: the project or run does not exist in the requested ownership
	// scope. It is rendered exactly like a hidden resource.
	ErrNotFound = errors.New("observation: not found")
	// ErrHidden: the caller must not learn whether the target exists.
	ErrHidden = errors.New("observation: hidden")
	// ErrDenied: the caller is inside the tenant but not permitted.
	ErrDenied = errors.New("observation: denied")
	// ErrAccessRevoked: access that was current when the stream started is no
	// longer current; the stream stops without disclosing anything further.
	ErrAccessRevoked = errors.New("observation: access revoked")
	// ErrCursorExpired: the position is older than the retained history. The
	// client resynchronizes from a snapshot.
	ErrCursorExpired = errors.New("observation: cursor expired")
	// ErrBadPosition: the position is ahead of the committed high-water mark or
	// otherwise cannot belong to this stream.
	ErrBadPosition = errors.New("observation: position is not valid for this stream")
	// ErrSnapshotTooLarge: the complete snapshot exceeds the configured bound.
	// No cursor is issued for a partial snapshot.
	ErrSnapshotTooLarge = errors.New("observation: snapshot exceeds the configured bound")
	// ErrSlowClient: the client did not accept data within the write bound.
	ErrSlowClient = errors.New("observation: client is too slow")
	// ErrClosed: the session or stream has already ended.
	ErrClosed = errors.New("observation: closed")
	// ErrUnavailable: a required dependency (database, authority, resource
	// binding) is unavailable. It is never access and never an empty result.
	ErrUnavailable = errors.New("observation: unavailable")
	// ErrInvalidConfiguration: a constructor was given a missing or invalid
	// collaborator or option.
	ErrInvalidConfiguration = errors.New("observation: invalid configuration")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Unavailablef wraps cause as an ErrUnavailable failure.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

func misconfigured(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfiguration, fmt.Sprintf(format, args...))
}
