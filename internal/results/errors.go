package results

import (
	"errors"
	"fmt"
)

// Failure classes of the module. Every error leaving the application layer is,
// or wraps, one of these; an unclassified error is treated as ErrUnavailable
// so an unexpected failure can never read as success or as a business decision.
// Messages are fixed and client-safe: they never carry artifact content,
// foreign identifiers, tokens or the text of a dependency's error.
var (
	// ErrInvalidRequest is a request that violates the frozen contract.
	ErrInvalidRequest = errors.New("results: invalid request")
	// ErrUnauthenticated is a missing, unknown, expired, retired or foreign
	// credential, or a token that does not match the route's attempt.
	ErrUnauthenticated = errors.New("results: unauthenticated")
	// ErrForbidden is a valid credential used for an action it does not cover.
	ErrForbidden = errors.New("results: forbidden")
	// ErrAttemptRevoked is an attempt that no longer holds authority (cancelled,
	// timed out, revoked) or a late result for it.
	ErrAttemptRevoked = errors.New("results: attempt revoked")
	// ErrNotFound is a missing, foreign or hidden reference; the three are
	// indistinguishable.
	ErrNotFound = errors.New("results: not found")
	// ErrArtifactIncomplete is a referenced artifact that exists in the
	// attempt's scope but is not usable yet.
	ErrArtifactIncomplete = errors.New("results: artifact incomplete")
	// ErrOutputInvalid is a result that violates its declared outputs in a way
	// that no durable finalization can record.
	ErrOutputInvalid = errors.New("results: output invalid")
	// ErrResultHashMismatch is a resultHash that does not cover the result.
	ErrResultHashMismatch = errors.New("results: result hash mismatch")
	// ErrResultConflict is a different result for an attempt that already
	// accepted one.
	ErrResultConflict = errors.New("results: result conflict")
	// ErrPublicationConflict is a lost expected-tip race or a changed proposal.
	// The concrete error is a *PublicationConflict.
	ErrPublicationConflict = errors.New("results: publication conflict")
	// ErrUnavailable is a dependency that could not give a conclusive answer, a
	// publication or writer settlement that is still unsettled, or an unknown
	// commit outcome. Nothing was acknowledged; the caller repeats the request.
	ErrUnavailable = errors.New("results: unavailable")
)

var kinds = []error{ErrInvalidRequest, ErrUnauthenticated, ErrForbidden, ErrAttemptRevoked, ErrNotFound, ErrArtifactIncomplete,
	ErrOutputInvalid, ErrResultHashMismatch, ErrResultConflict, ErrPublicationConflict, ErrUnavailable}

type classified struct {
	kind  error
	msg   string
	cause error
}

func (e *classified) Error() string        { return e.kind.Error() + ": " + e.msg }
func (e *classified) Is(target error) bool { return target == e.kind }
func (e *classified) Unwrap() error        { return e.cause }

// Fail builds a classified error with a fixed message. cause is kept for the
// caller's own diagnostics (errors.Is/As) and is never rendered.
func Fail(kind error, cause error, format string, args ...any) error {
	return &classified{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}

// PublicationConflict carries the observed branch tip of a lost race.
type PublicationConflict struct {
	// Reason is a stable, content-free word such as "tip_moved".
	Reason string
	// ObservedTip is the remote tip seen by the publisher; empty when unknown.
	ObservedTip string
}

func (e *PublicationConflict) Error() string        { return ErrPublicationConflict.Error() + ": " + e.Reason }
func (e *PublicationConflict) Is(target error) bool { return target == ErrPublicationConflict }

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
