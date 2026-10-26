package kubernetes

import (
	"errors"
	"fmt"
	"time"
)

// Failure classes of the transport. A caller classifies with errors.Is; the
// concrete *Error never carries request or response bodies, because Kubernetes
// validation messages can echo submitted values (including Secret data).
var (
	// ErrNotFound: the object definitely does not exist (HTTP 404 on a read or
	// precondition-free delete).
	ErrNotFound = errors.New("kubernetes: not found")
	// ErrAlreadyExists: a create was refused because the name is taken.
	ErrAlreadyExists = errors.New("kubernetes: already exists")
	// ErrConflict: a precondition failed (resourceVersion or UID changed).
	ErrConflict = errors.New("kubernetes: conflict")
	// ErrForbidden: the API server refused the identity (HTTP 401/403).
	ErrForbidden = errors.New("kubernetes: forbidden")
	// ErrInvalid: the API server rejected the object (HTTP 400/422). The request
	// was not applied.
	ErrInvalid = errors.New("kubernetes: invalid object")
	// ErrThrottled: HTTP 429; the request was not applied.
	ErrThrottled = errors.New("kubernetes: throttled")
	// ErrUnavailable: a read could not complete (timeout, connection loss or
	// 5xx). Nothing is known about the object.
	ErrUnavailable = errors.New("kubernetes: unavailable")
	// ErrUnknownOutcome: a mutation was sent and its result is not known (the
	// response was lost, the request timed out or the server failed after
	// accepting it). The mutation may or may not have been applied; callers read
	// the object back and never assume either.
	ErrUnknownOutcome = errors.New("kubernetes: unknown outcome")
	// ErrExpired: a list continuation token expired; the listing must restart.
	ErrExpired = errors.New("kubernetes: continuation expired")
	// ErrTooLarge: a listing exceeded its page bound or a response its size
	// bound.
	ErrTooLarge = errors.New("kubernetes: response too large")
	// ErrCanceled: the caller's context ended before a read completed.
	ErrCanceled = errors.New("kubernetes: canceled")
)

// Error is one failed API operation.
type Error struct {
	Op     string
	Status int
	// Reason is the server's machine-readable reason (for example
	// "AlreadyExists"), never its message.
	Reason     string
	Kind       error
	RetryAfter time.Duration
	// Cause is the transport error, if any. It is for errors.Is only and is
	// not printed.
	Cause error
}

// Error implements error.
func (e *Error) Error() string {
	s := "kubernetes: " + e.Op + ": " + e.Kind.Error()[len("kubernetes: "):]
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d", e.Status)
		if e.Reason != "" {
			s += " " + e.Reason
		}
		s += ")"
	}
	return s
}

// SafeDiagnostic implements diag.Safe: the operation, class and HTTP status only, scrubbed of any body.
func (e *Error) SafeDiagnostic() string { return e.Error() }

// Is matches the failure class.
func (e *Error) Is(target error) bool { return target == e.Kind }

// Unwrap exposes the transport cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Definite reports whether err proves the operation was not applied. Reads that
// fail are never "definite" about the object: only the specific classes below
// say something.
func Definite(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrThrottled)
}
