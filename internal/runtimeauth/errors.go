package runtimeauth

import (
	"errors"
	"fmt"
)

// Failure classes. Every error leaving the application layer is, or wraps, one
// of these; anything else is treated as ErrUnavailable so an unexpected failure
// can never read as success or as a business decision.
var (
	// ErrInvalidRequest is a request this module cannot interpret.
	ErrInvalidRequest = errors.New("runtimeauth: invalid request")
	// ErrInvalidCredential is a missing, unknown, expired, foreign or retired
	// admission credential or runtime token (401).
	ErrInvalidCredential = errors.New("runtimeauth: invalid credential")
	// ErrForbidden is a valid credential used for something it does not cover:
	// a Job/Pod that is not the attempt's, or an action outside the grant (403).
	ErrForbidden = errors.New("runtimeauth: forbidden")
	// ErrAttemptRevoked is a cancelled, finished or revoked attempt (403).
	ErrAttemptRevoked = errors.New("runtimeauth: attempt revoked")
	// ErrAdmissionConsumed is a second admission with another nonce (409).
	ErrAdmissionConsumed = errors.New("runtimeauth: admission already consumed")
	// ErrProfileUnsupported is an incompatible wrapper/profile pairing (422).
	ErrProfileUnsupported = errors.New("runtimeauth: wrapper not supported by the profile")
	// ErrConflict is a registration that contradicts the stored immutable grant.
	ErrConflict = errors.New("runtimeauth: conflicting registration")
	// ErrNotFound is an absent attempt or grant. The HTTP layer never discloses
	// it as such.
	ErrNotFound = errors.New("runtimeauth: not found")
	// ErrUnavailable is a dependency (database, signing key, execution state,
	// Kubernetes observation) that could not give a conclusive answer. Callers
	// retry; nothing was granted.
	ErrUnavailable = errors.New("runtimeauth: unavailable")
)

// classified carries a fixed, client-safe message and an optional cause. The
// cause is for the caller's own diagnostics (errors.Is/As); it is never logged
// or rendered because provider and driver errors can quote secrets.
type classified struct {
	kind  error
	msg   string
	cause error
}

func (e *classified) Error() string { return e.kind.Error() + ": " + e.msg }
func (e *classified) Is(target error) bool {
	return target == e.kind
}
func (e *classified) Unwrap() error { return e.cause }

func fail(kind error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...)}
}

func failWith(kind error, cause error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}

var kinds = []error{ErrInvalidRequest, ErrInvalidCredential, ErrForbidden, ErrAttemptRevoked, ErrAdmissionConsumed, ErrProfileUnsupported, ErrConflict, ErrNotFound, ErrUnavailable}

// Classify returns err unchanged when it belongs to a known failure class and
// otherwise wraps it as ErrUnavailable (fail closed).
func Classify(err error) error {
	if err == nil {
		return nil
	}
	for _, k := range kinds {
		if errors.Is(err, k) {
			return err
		}
	}
	return failWith(ErrUnavailable, err, "the dependency could not answer")
}
