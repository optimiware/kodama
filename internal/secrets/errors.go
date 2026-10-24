package secrets

import (
	"errors"
	"fmt"
)

// Sentinel failures of the module. They are classified, never carry values, and
// are mapped to transport codes by adapters.
var (
	// ErrInvalid wraps every validation failure of input or state.
	ErrInvalid = errors.New("secrets: invalid")
	// ErrNotFound: the resource does not exist in the caller's tenant/project.
	// Foreign resources are reported the same way.
	ErrNotFound = errors.New("secrets: not found")
	// ErrNameTaken: the name is, or was, used in the project. Deleted names are
	// never reused so pinned references cannot change meaning.
	ErrNameTaken = errors.New("secrets: name already used in the project")
	// ErrDeleted: the resource is logically deleted.
	ErrDeleted = errors.New("secrets: deleted")
	// ErrRevoked: the secret version is revoked (one way) and cannot resolve.
	ErrRevoked = errors.New("secrets: version revoked")
	// ErrInvalidTransition: the entity does not allow the requested change.
	ErrInvalidTransition = errors.New("secrets: invalid transition")
	// ErrUndeclared: the reference or version is not declared for the attempt.
	ErrUndeclared = errors.New("secrets: reference not declared for the attempt")
	// ErrNotPinned: no run pin retains the requested version for the consumer.
	ErrNotPinned = errors.New("secrets: version is not pinned for the consumer")
	// ErrNoActiveVersion: the secret has no resolvable current version.
	ErrNoActiveVersion = errors.New("secrets: no active version")
	// ErrAttemptInactive: the attempt is cancelled, expired or revoked.
	ErrAttemptInactive = errors.New("secrets: attempt authority is not active")
	// ErrAuthorityMissing: no trusted attempt authority could be established.
	ErrAuthorityMissing = errors.New("secrets: attempt authority unavailable")
	// ErrUnavailable: a dependency failed in a retryable way; nothing changed.
	ErrUnavailable = errors.New("secrets: dependency unavailable")
	// ErrFinalizationUncertain: the audit commit's outcome is unknown, so no
	// plaintext was released.
	ErrFinalizationUncertain = errors.New("secrets: resolution finalization uncertain")
	// ErrBackpressure: the event backlog refused the change; nothing changed.
	ErrBackpressure = errors.New("secrets: event backlog is full")
	// ErrInconsistent: stored state contradicts an invariant.
	ErrInconsistent = errors.New("secrets: inconsistent stored state")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ConflictError reports a lost compare-and-swap. Resource names what was
// compared ("secret", "variable", "wrap revision", "value version").
type ConflictError struct {
	Resource string
	Expected int64
	Current  int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("secrets: stale %s: expected %d, current %d", e.Resource, e.Expected, e.Current)
}

// KeyErrorClass classifies a key provider failure.
type KeyErrorClass string

const (
	// KeyUnavailable: the provider is down or timed out; retry later.
	KeyUnavailable KeyErrorClass = "unavailable"
	// KeyDenied: the provider refused the operation.
	KeyDenied KeyErrorClass = "denied"
	// KeyNotFound: the key or retained key version is not configured.
	KeyNotFound KeyErrorClass = "not_found"
	// KeyCorrupt: the wrapped key failed authentication or is malformed.
	KeyCorrupt KeyErrorClass = "corrupt"
)

// KeyError is the classified failure of a KeyWrapper. It never contains key
// material, and no fallback follows it.
type KeyError struct {
	Class KeyErrorClass
	Op    string
	Err   error
}

func (e *KeyError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("secrets: key provider %s: %s", e.Op, e.Class)
	}
	return fmt.Sprintf("secrets: key provider %s: %s: %v", e.Op, e.Class, e.Err)
}

func (e *KeyError) Unwrap() error { return e.Err }

// Retryable reports whether the same request may succeed later.
func (e *KeyError) Retryable() bool { return e.Class == KeyUnavailable }
