package mcpproxy

import (
	"errors"
	"fmt"
)

// Failure classes of the module. Callers classify with errors.Is; transport
// adapters map them to their own codes. Messages are constant text: they never
// carry tool arguments or results, credentials, session identifiers or remote
// diagnostics.
var (
	// ErrInvalid is a malformed or rule-violating request or draft.
	ErrInvalid = errors.New("mcpproxy: invalid")
	// ErrNotFound is an absent, foreign or hidden resource. A foreign project,
	// registration, session or tool is indistinguishable from a missing one.
	ErrNotFound = errors.New("mcpproxy: not found")
	// ErrUnauthenticated is a missing, invalid, expired or retired credential.
	ErrUnauthenticated = errors.New("mcpproxy: unauthenticated")
	// ErrDenied is an authenticated caller that lacks the authority.
	ErrDenied = errors.New("mcpproxy: denied")
	// ErrVersionConflict is a stale expected registration version.
	ErrVersionConflict = errors.New("mcpproxy: version conflict")
	// ErrConflict is stored state that contradicts the request.
	ErrConflict = errors.New("mcpproxy: conflict")
	// ErrBindingInvalid is a run binding whose registration or credential
	// changed after it was frozen. The binding is never silently replaced.
	ErrBindingInvalid = errors.New("mcpproxy: binding invalidated")
	// ErrSessionInvalid is an expired, closed, failed, invalidated or
	// process-lost remote session. Reported to callers as not found.
	ErrSessionInvalid = errors.New("mcpproxy: session invalid")
	// ErrToolHidden is a tool outside the registration/profile intersection.
	ErrToolHidden = errors.New("mcpproxy: tool not permitted")
	// ErrLimitReached is the run's claimed tool-call ceiling.
	ErrLimitReached = errors.New("mcpproxy: tool call limit reached")
	// ErrDuplicate is a request identifier that was already admitted. The
	// call is never dispatched a second time.
	ErrDuplicate = errors.New("mcpproxy: duplicate request")
	// ErrInvalidTransition is an entity transition its state does not allow.
	ErrInvalidTransition = errors.New("mcpproxy: invalid transition")
	// ErrUnavailable is a dependency that gave no conclusive answer, or a
	// dependency that is not configured. Nothing was dispatched.
	ErrUnavailable = errors.New("mcpproxy: unavailable")
	// ErrAdmissionUncertain refines ErrUnavailable: the admission commit may
	// or may not have landed, so the call was NOT dispatched.
	ErrAdmissionUncertain = errors.New("mcpproxy: admission outcome uncertain")
	// ErrOutcomeUnknown is a dispatched call whose remote effect is unknown.
	// It is never retried automatically and implies no rollback.
	ErrOutcomeUnknown = errors.New("mcpproxy: remote outcome unknown")
	// ErrRemoteFailed is a definitive remote protocol error reply. Even then
	// the proxy makes no claim that the remote effect did not happen.
	ErrRemoteFailed = errors.New("mcpproxy: remote reported an error")
	// ErrDestination is an endpoint outside the approved destinations.
	ErrDestination = errors.New("mcpproxy: destination not approved")
	// ErrProtocol is a remote that does not speak the pinned protocol.
	ErrProtocol = errors.New("mcpproxy: unsupported protocol")
	// ErrCredential is a credential that is missing, revoked, foreign,
	// malformed or addressed to another recipient.
	ErrCredential = errors.New("mcpproxy: credential unusable")
	// ErrTooLarge is a request or response beyond the configured bounds.
	ErrTooLarge = errors.New("mcpproxy: too large")
)

// Unavailablef builds an ErrUnavailable wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func transitionf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}
