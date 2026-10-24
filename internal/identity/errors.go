package identity

import (
	"errors"
	"fmt"
)

// Failure classes of the identity module. Each is a sentinel callers classify
// with errors.Is; transport adapters map them to their own codes. Any error that
// is not one of these classes is a dependency failure: it is never a grant.
var (
	// ErrInvalid: the request is malformed or violates a field rule.
	ErrInvalid = errors.New("identity: invalid request")
	// ErrNotFound: the referenced resource does not exist in the caller's scope.
	ErrNotFound = errors.New("identity: not found")
	// ErrDenied: the caller is a member of the target's tenant but not permitted.
	ErrDenied = errors.New("identity: denied")
	// ErrHidden: the caller must not learn whether the target exists (no
	// binding reaches it). Rendered exactly like a missing resource.
	ErrHidden = errors.New("identity: hidden")
	// ErrConflict: a different resource already holds the unique key, or the
	// binding already exists.
	ErrConflict = errors.New("identity: already exists")
	// ErrVersionConflict: the expected resource version is stale.
	ErrVersionConflict = errors.New("identity: version conflict")
	// ErrUnavailable: a required dependency (database, issuer, authority) is
	// unavailable or the outcome of a write is unknown. Callers reread before
	// retrying; nothing is replayed automatically.
	ErrUnavailable = errors.New("identity: unavailable")
	// ErrUnauthenticated: the credential is missing, malformed, expired,
	// has the wrong issuer or audience, or has a bad signature.
	ErrUnauthenticated = errors.New("identity: unauthenticated")
	// ErrNoChange is returned by a transition that would change nothing.
	ErrNoChange = errors.New("identity: no change")
	// ErrBootstrapConflict: bootstrap already completed with other parameters.
	ErrBootstrapConflict = errors.New("identity: bootstrap already completed with different parameters")
	// ErrBootstrapNotEligible: the verified identity is not the configured
	// bootstrap administrator.
	ErrBootstrapNotEligible = errors.New("identity: identity is not eligible for bootstrap")
)

// IsBusinessError reports whether err is one of the module's classified
// failures (as opposed to an infrastructure error).
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrHidden, ErrConflict, ErrVersionConflict,
		ErrUnavailable, ErrUnauthenticated, ErrNoChange, ErrBootstrapConflict, ErrBootstrapNotEligible} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Invalidf builds an ErrInvalid error for adapters validating transport values.
func Invalidf(format string, args ...any) error { return invalid(format, args...) }

// Unavailablef builds an ErrUnavailable error wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}
