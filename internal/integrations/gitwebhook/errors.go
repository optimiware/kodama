package gitwebhook

import (
	"errors"
	"fmt"
)

// Stable failure classes. Callers match with errors.Is.
var (
	// ErrUnauthenticated is returned for every notification that cannot be tied
	// to a trigger and a valid signature: unknown or disabled trigger, missing
	// or malformed credential headers, stale timestamp, revoked secret and a
	// wrong signature are indistinguishable to the sender.
	ErrUnauthenticated = errors.New("gitwebhook: notification not authenticated")
	// ErrInvalid is a malformed but authenticated payload or command.
	ErrInvalid = errors.New("gitwebhook: invalid request")
	// ErrForeignRepository is an authenticated notification naming a repository
	// other than the one the trigger and the project are bound to.
	ErrForeignRepository = errors.New("gitwebhook: repository is not the trigger's repository")
	// ErrDeliveryConflict reuses a delivery identifier with a different body.
	ErrDeliveryConflict = errors.New("gitwebhook: delivery identifier reused with a different payload")
	// ErrRejected is a final, recorded refusal of an authenticated delivery
	// (the Receipt carries the reason). Redelivery returns the same refusal.
	ErrRejected = errors.New("gitwebhook: delivery rejected")
	// ErrUnavailable is a dependency outage. The delivery stays retryable under
	// the same identifier and no run exists unless it was already recorded.
	ErrUnavailable = errors.New("gitwebhook: dependency unavailable")
	// ErrAdmissionClosed is returned by the run port for a key whose refusal
	// was durably decided; it is a permanent ErrRunRefused.
	ErrAdmissionClosed = fmt.Errorf("%w: the delivery's admission was refused", ErrRunRefused)
	// ErrAdmissionBusy is a retryable refusal: another processor holds the
	// delivery's admission lease. It is an ErrUnavailable.
	ErrAdmissionBusy = fmt.Errorf("%w: another processor is admitting this delivery", ErrUnavailable)
	// ErrNotFound is a missing or foreign (other tenant/project) record.
	ErrNotFound = errors.New("gitwebhook: not found")
	// ErrDenied is an authorization refusal.
	ErrDenied = errors.New("gitwebhook: access denied")
	// ErrConflict is an optimistic-concurrency or uniqueness conflict.
	ErrConflict = errors.New("gitwebhook: conflict")
	// ErrRunRefused is returned by the run port for a permanent admission
	// refusal (invalid definition, conflicting source, denied authority).
	ErrRunRefused = errors.New("gitwebhook: run admission refused")
	// ErrSecretNotFound is returned by the secret port when the reference is
	// missing, revoked or not visible in the scope.
	ErrSecretNotFound = errors.New("gitwebhook: secret not found")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func unavailable(op string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, cause)
}
