package artifacts

import (
	"errors"
	"fmt"
)

// Classified errors. Adapters translate their driver and provider failures to
// these sentinels; transports map them to wire codes. Messages are fixed and
// never include storage keys, URLs, digests of foreign content or causes.
var (
	// ErrInvalid is a malformed request or value.
	ErrInvalid = errors.New("artifacts: invalid request")
	// ErrNotFound is a missing, foreign or hidden reference. The three cases are
	// deliberately indistinguishable.
	ErrNotFound = errors.New("artifacts: not found")
	// ErrForbidden is an authenticated caller refused by the authority.
	ErrForbidden = errors.New("artifacts: forbidden")
	// ErrAuthorityMissing: no trusted authority record for the credential.
	ErrAuthorityMissing = errors.New("artifacts: attempt authority missing")
	// ErrAttemptInactive: the attempt is revoked, cancelled or expired.
	ErrAttemptInactive = errors.New("artifacts: attempt is not active")
	// ErrIdempotencyConflict: the name was reserved with different metadata, or
	// the earlier reservation is no longer open.
	ErrIdempotencyConflict = errors.New("artifacts: reservation conflict")
	// ErrOutputInvalid: the artifact is not a permitted output of the attempt,
	// exceeds its limits or names an unverifiable registration.
	ErrOutputInvalid = errors.New("artifacts: output not permitted")
	// ErrLimitExceeded is an aggregate limit violation (an ErrOutputInvalid).
	ErrLimitExceeded = fmt.Errorf("%w: limit exceeded", ErrOutputInvalid)
	// ErrIncomplete: the stored bytes are absent or not the declared size.
	ErrIncomplete = errors.New("artifacts: upload incomplete")
	// ErrDigestMismatch: declared and actual content identity disagree.
	ErrDigestMismatch = errors.New("artifacts: digest mismatch")
	// ErrUnavailable is a dependency failure or an unknown outcome. It is never
	// success; the operation is safe to retry.
	ErrUnavailable = errors.New("artifacts: unavailable")
	// ErrInProgress: another completion currently holds the lease.
	ErrInProgress = fmt.Errorf("%w: completion in progress", ErrUnavailable)
	// ErrClaimLost: a fenced completion or deletion lost its claim.
	ErrClaimLost = errors.New("artifacts: claim lost")
	// ErrObjectNotFound is returned by the object store for an absent key.
	ErrObjectNotFound = errors.New("artifacts: object not found")
	// ErrPinned: retention may not remove a pinned artifact.
	ErrPinned = errors.New("artifacts: artifact is pinned")
	// ErrInconsistent: persisted state violates an entity invariant.
	ErrInconsistent = errors.New("artifacts: inconsistent stored state")
	// ErrRetentionDisabled: no retention policy was injected.
	ErrRetentionDisabled = errors.New("artifacts: retention policy not configured")

	// OCI registration failures.
	ErrRegistryDenied      = fmt.Errorf("%w: registry or repository not permitted", ErrOutputInvalid)
	ErrRegistryContent     = fmt.Errorf("%w: registry content does not match the digest", ErrOutputInvalid)
	ErrProvenanceMissing   = fmt.Errorf("%w: source provenance cannot be established", ErrOutputInvalid)
	ErrRegistryUnavailable = fmt.Errorf("%w: registry unavailable", ErrUnavailable)
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
