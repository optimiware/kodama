package gateway

import (
	"errors"
	"fmt"
)

// Failure classes. Every error leaving the application layer is, or wraps, one
// of these. Messages are fixed, client-safe text: a cause is kept for the
// caller's errors.Is/As and is never rendered, because provider, driver and
// key-provider errors can quote credentials or content.
var (
	// ErrInvalid is a malformed or unsupported request or configuration.
	ErrInvalid = errors.New("gateway: invalid request")
	// ErrUnauthenticated is a missing, expired, retired or foreign workload
	// credential.
	ErrUnauthenticated = errors.New("gateway: unauthenticated")
	// ErrForbidden is a valid identity used for something it does not cover: an
	// ungranted action or a cancelled/revoked attempt.
	ErrForbidden = errors.New("gateway: forbidden")
	// ErrNotFound is an absent or foreign resource; the two are indistinguishable.
	ErrNotFound = errors.New("gateway: not found")
	// ErrHidden and ErrDenied are management authorization refusals.
	ErrHidden = errors.New("gateway: hidden")
	ErrDenied = errors.New("gateway: denied")
	// ErrNotConfigured: the project has no enabled model settings.
	ErrNotConfigured = errors.New("gateway: model access is not configured")
	// ErrModelNotAllowed: the model is not on the run's allow-list.
	ErrModelNotAllowed = errors.New("gateway: model not allowed")
	// ErrLimitExceeded: a bound of the run's policy refuses the request (request
	// count, token allowance, request size, input or output bound).
	ErrLimitExceeded = errors.New("gateway: limit exceeded")
	// ErrBusy: the run's single in-flight slot is occupied.
	ErrBusy = errors.New("gateway: another model request of this run is unresolved")
	// ErrVersionConflict: the expected settings revision is stale.
	ErrVersionConflict = errors.New("gateway: version conflict")
	// ErrCredential: the provider credential is unusable (wrong type, revoked,
	// audience mismatch, key provider failure). It never carries the reason's
	// detail.
	ErrCredential = errors.New("gateway: credential unavailable")
	// ErrDestination: the endpoint is not an approved destination.
	ErrDestination = errors.New("gateway: destination not approved")
	// ErrProvider: the provider refused or failed before any stream byte reached
	// the caller. Retry is an explicit new request.
	ErrProvider = errors.New("gateway: provider failure")
	// ErrRecording: content recording is unavailable or not authorized.
	ErrRecording = errors.New("gateway: recording unavailable")
	// ErrBackpressure: the event backlog refused a write; nothing was committed.
	ErrBackpressure = errors.New("gateway: event backlog is full")
	// ErrUnavailable: a dependency could not give a conclusive answer. When the
	// outcome of a commit is unknown, the wrapped cause is ErrUnknownCommit.
	ErrUnavailable = errors.New("gateway: unavailable")
	// ErrUnknownCommit marks a commit whose outcome is unknown. Admission treats
	// it as "do not send"; finalization rereads by request identity.
	ErrUnknownCommit = errors.New("gateway: commit outcome unknown")
	// ErrSuperseded: the request was already settled or reconciled by someone
	// else; a late finalization changes nothing.
	ErrSuperseded = errors.New("gateway: request already settled")
	// ErrInvalidTransition: the entity does not allow the requested change.
	ErrInvalidTransition = errors.New("gateway: invalid transition")
)

type classified struct {
	kind  error
	msg   string
	cause error
}

func (e *classified) Error() string        { return e.kind.Error() + ": " + e.msg }
func (e *classified) Is(target error) bool { return target == e.kind }
func (e *classified) Unwrap() error        { return e.cause }

func fail(kind error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// Failf builds a classified error with a fixed message.
func Failf(kind error, format string, args ...any) error { return fail(kind, format, args...) }

// FailWith builds a classified error that keeps cause for errors.Is/As only.
func FailWith(kind error, cause error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}

var kinds = []error{
	ErrInvalid, ErrUnauthenticated, ErrForbidden, ErrNotFound, ErrHidden, ErrDenied, ErrNotConfigured, ErrModelNotAllowed,
	ErrLimitExceeded, ErrBusy, ErrVersionConflict, ErrCredential, ErrDestination, ErrProvider, ErrRecording, ErrBackpressure,
	ErrUnavailable, ErrSuperseded, ErrInvalidTransition,
}

// Classify returns err unchanged when it belongs to a known class and
// otherwise wraps it as ErrUnavailable, so an unexpected failure can never read
// as success or as a business decision.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	for _, k := range kinds {
		if errors.Is(err, k) {
			return err
		}
	}
	return FailWith(ErrUnavailable, err, "the dependency could not answer")
}

// PublicMessage returns the fixed, client-safe text of the first classified
// error in err's chain, or "" when none carries one. Adapters render it instead
// of err.Error(), which can include a class prefix, and never render a cause.
func PublicMessage(err error) string {
	for ; err != nil; err = errors.Unwrap(err) {
		if c, ok := err.(*classified); ok {
			return c.msg
		}
	}
	return ""
}
