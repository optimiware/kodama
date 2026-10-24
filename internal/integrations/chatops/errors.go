package chatops

import (
	"errors"
	"fmt"
)

// Failure classes of the module. Every error leaving the application layer is,
// or wraps, one of these; an unclassified error is ErrUnavailable so an
// unexpected failure can never read as a business decision or as success.
// Messages are fixed and client-safe: they never carry endpoint URLs,
// credentials, response bodies or the text of a dependency's error.
var (
	// ErrInvalid is input that violates the module's rules.
	ErrInvalid = errors.New("chatops: invalid")
	// ErrNotFound is a missing, foreign or hidden resource; the three are
	// indistinguishable.
	ErrNotFound = errors.New("chatops: not found")
	// ErrDenied is a caller without current authority for the operation.
	ErrDenied = errors.New("chatops: denied")
	// ErrVersionConflict is a stale expected resource version.
	ErrVersionConflict = errors.New("chatops: version conflict")
	// ErrInvalidTransition is a lifecycle change the state machine forbids.
	ErrInvalidTransition = errors.New("chatops: invalid transition")
	// ErrStaleClaim is a finalization by a holder that no longer owns the claim.
	ErrStaleClaim = errors.New("chatops: stale delivery claim")
	// ErrConflict is a request that changes an immutable input of an existing
	// identity (for example a delivery whose content differs).
	ErrConflict = errors.New("chatops: conflict")
	// ErrNameTaken is a destination name already used in the project.
	ErrNameTaken = errors.New("chatops: name taken")
	// ErrLimit is a destination count above the configured per-project limit.
	ErrLimit = errors.New("chatops: destination limit reached")
	// ErrNotConfigured is a capability whose required collaborator was not
	// supplied (for example a credential source for an authenticated
	// destination). It is a refusal, never an allow-all default.
	ErrNotConfigured = errors.New("chatops: not configured")
	// ErrPolicy is a destination the installation's outbound policy refuses.
	ErrPolicy = errors.New("chatops: destination refused by policy")
	// ErrCredentialUnavailable is a credential reference that is missing,
	// revoked, foreign or not bound to the recipient.
	ErrCredentialUnavailable = errors.New("chatops: credential unavailable")
	// ErrFenced is delivery work refused because the tenant's delivery is
	// fenced pending post-restore reconciliation.
	ErrFenced = errors.New("chatops: deliveries are fenced")
	// ErrUnavailable is a dependency that could not give a conclusive answer or
	// a commit whose outcome is unknown. Retrying with the same identity observes
	// whatever committed.
	ErrUnavailable = errors.New("chatops: unavailable")
)

var kinds = []error{ErrInvalid, ErrNotFound, ErrDenied, ErrVersionConflict, ErrInvalidTransition, ErrStaleClaim, ErrConflict,
	ErrNameTaken, ErrLimit, ErrNotConfigured, ErrPolicy, ErrCredentialUnavailable, ErrFenced, ErrUnavailable}

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
