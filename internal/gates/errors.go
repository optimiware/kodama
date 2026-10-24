package gates

import (
	"errors"
	"fmt"
)

// Failure classes of the gate module. Each is a sentinel callers classify with
// errors.Is; the concrete error wraps it with a client-safe message that never
// carries evidence content, repository contents or foreign identifiers.
var (
	// ErrInvalid: the request or a value is malformed.
	ErrInvalid = errors.New("gates: invalid request")
	// ErrNotFound: the gate or a referenced resource is absent from the
	// caller's scope. Hidden and foreign references are indistinguishable.
	ErrNotFound = errors.New("gates: not found")
	// ErrDenied / ErrHidden: authorization refusals.
	ErrDenied = errors.New("gates: denied")
	ErrHidden = errors.New("gates: hidden")
	// ErrVersionConflict: the expected gate version is not the current one.
	ErrVersionConflict = errors.New("gates: version conflict")
	// ErrStaleSubject: the gate's subject no longer matches the current
	// content, source, definition, evidence or deployment configuration, or the
	// gate expired or was invalidated. The prior decision is unchanged.
	ErrStaleSubject = errors.New("gates: stale gate subject")
	// ErrInvalidTransition: the entity does not allow the change (a second
	// decision, a decision on a step that is no longer waiting).
	ErrInvalidTransition = errors.New("gates: invalid transition")
	// ErrConformance: agent candidate conformance evidence is missing, stale,
	// wrong or blocked; approval and progression are refused.
	ErrConformance = errors.New("gates: conformance evidence")
	// ErrEvidence: a referenced artifact failed the ownership, provenance or
	// content checks. No gate exists for unverifiable evidence.
	ErrEvidence = errors.New("gates: evidence")
	// ErrReviewerAuthority: the deciding reviewer no longer holds the reviewer
	// authority. It is a current-authority check, not an invalidation.
	ErrReviewerAuthority = errors.New("gates: reviewer authority")
	// ErrUnavailable: a dependency is unavailable or the outcome of a write is
	// unknown. It denies progression and never fabricates an invalidation.
	ErrUnavailable = errors.New("gates: unavailable")
	// ErrNotConfigured: a required port is absent; the operation fails closed.
	ErrNotConfigured = errors.New("gates: not configured")
	// ErrConflict: stored state contradicts the request.
	ErrConflict = errors.New("gates: conflict")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Invalidf builds an ErrInvalid for adapters validating transport values.
func Invalidf(format string, args ...any) error { return invalidf(format, args...) }

func transitionf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}

// Unavailablef builds an ErrUnavailable wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

// StaleError is a definitive staleness of a gate subject. Reason is a content
// free reason code. The superseding facts, when known, ride the invalidation
// event.
type StaleError struct {
	// Definitive is true when current facts differ from the subject: only a
	// definitive staleness is recorded as an invalidation. An expired gate, a
	// stale digest in a request and an unavailable check are not.
	Definitive          bool
	Reason              string
	SupersedingCommit   string
	SupersedingRevision int64
}

func (e *StaleError) Error() string { return "gates: stale gate subject: " + e.Reason }
func (e *StaleError) Unwrap() error { return ErrStaleSubject }

func stale(reason string) *StaleError { return &StaleError{Reason: reason} }

func definitive(reason string) *StaleError { return &StaleError{Reason: reason, Definitive: true} }

// ConformanceError carries the denial code of a conformance failure. The codes
// are T-003's non-wire codes (conformance_blocked and its siblings) plus the
// gate's own identification codes.
type ConformanceError struct {
	Code string
}

func (e *ConformanceError) Error() string { return "gates: conformance evidence: " + e.Code }
func (e *ConformanceError) Unwrap() error { return ErrConformance }

// Denial codes the gate adds to T-003's conformance codes.
const (
	CodeConformanceMissing         = "conformance_missing"
	CodeConformanceBaselineMissing = "conformance_baseline_missing"
	CodeConformanceEvidenceInvalid = "conformance_evidence_invalid"
	CodeConformanceMalformed       = "conformance_malformed"
)

// IsBusinessError reports whether err is one of the module's classified
// failures.
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrHidden, ErrVersionConflict, ErrStaleSubject, ErrInvalidTransition,
		ErrConformance, ErrEvidence, ErrReviewerAuthority, ErrUnavailable, ErrNotConfigured, ErrConflict} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
