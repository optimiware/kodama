package delivery

import (
	"errors"
	"fmt"
)

// Failure classes of the module. Every error leaving the application layer is,
// or wraps, one of these; an unclassified error is ErrUnavailable so an
// unexpected failure can never read as a business decision or as success.
// Messages are fixed and client-safe: they never carry credentials, kubeconfig
// content, foreign identifiers or the text of a dependency's error.
var (
	// ErrInvalid is input that violates the module's rules.
	ErrInvalid = errors.New("delivery: invalid")
	// ErrNotFound is a missing, foreign or hidden resource; the three are
	// indistinguishable.
	ErrNotFound = errors.New("delivery: not found")
	// ErrDenied is a caller without current authority for the operation,
	// including a protected callback whose pinned context does not match.
	ErrDenied = errors.New("delivery: denied")
	// ErrUnauthenticated is a missing, unknown, expired or foreign credential.
	ErrUnauthenticated = errors.New("delivery: unauthenticated")
	// ErrAttemptRevoked is an attempt that no longer holds authority.
	ErrAttemptRevoked = errors.New("delivery: attempt revoked")
	// ErrConflict is a request that changes an immutable input of an existing
	// identity, or an outcome that contradicts a settled one.
	ErrConflict = errors.New("delivery: conflict")
	// ErrVersionConflict is a stale expected resource version.
	ErrVersionConflict = errors.New("delivery: version conflict")
	// ErrInvalidTransition is a lifecycle change the state machine forbids.
	ErrInvalidTransition = errors.New("delivery: invalid transition")
	// ErrOutcomeUnknown blocks a new apply while the previous outcome is
	// unknown: the objects must be observed and reconciled first.
	ErrOutcomeUnknown = errors.New("delivery: outcome unknown")
	// ErrNameTaken is an environment name already used in the project.
	ErrNameTaken = errors.New("delivery: name taken")
	// ErrCredentialUnavailable is a missing, foreign, revoked or wrongly typed
	// credential reference.
	ErrCredentialUnavailable = errors.New("delivery: credential unavailable")
	// ErrPinRequired is an absent, foreign or mismatched immutable pin. It never
	// falls back to the environment's current settings.
	ErrPinRequired = errors.New("delivery: pinned context required")
	// ErrProvenance is an image whose verified source, digest, producer or
	// ownership does not match the deployment.
	ErrProvenance = errors.New("delivery: image provenance rejected")
	// ErrNotConfigured is a capability whose required collaborators were not
	// supplied. It is a refusal, never an allow-all default.
	ErrNotConfigured = errors.New("delivery: not configured")
	// ErrUnavailable is a dependency that could not give a conclusive answer or
	// a commit whose outcome is unknown. Nothing was acknowledged; retrying with
	// the same identity observes whatever committed.
	ErrUnavailable = errors.New("delivery: unavailable")
)

// ErrBackpressure is a full event backlog. The producing change rolled back.
var ErrBackpressure = fmt.Errorf("%w: event backlog is full", ErrUnavailable)

var kinds = []error{ErrInvalid, ErrNotFound, ErrDenied, ErrUnauthenticated, ErrAttemptRevoked, ErrConflict, ErrVersionConflict,
	ErrInvalidTransition, ErrOutcomeUnknown, ErrNameTaken, ErrCredentialUnavailable, ErrPinRequired, ErrProvenance, ErrNotConfigured, ErrUnavailable}

type classified struct {
	kind  error
	msg   string
	cause error
}

func (e *classified) Error() string        { return e.kind.Error() + ": " + e.msg }
func (e *classified) Is(target error) bool { return target == e.kind }
func (e *classified) Unwrap() error        { return e.cause }

// Fail builds a classified error with a fixed message. cause is kept for the
// caller's diagnostics (errors.Is/As) and is never rendered.
func Fail(kind error, cause error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}

func invalidf(format string, args ...any) error { return Fail(ErrInvalid, nil, format, args...) }

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
	return Fail(ErrUnavailable, err, "the dependency could not answer")
}
