package runtimeauth

import (
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
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
	// ErrResourceRevoked is a preparation or service whose authority ended: stop
	// intent, run cancellation or terminal state, or its deadline (403
	// resource_revoked).
	ErrResourceRevoked = errors.New("runtimeauth: resource revoked")
	// ErrResourceUnbound is a resource whose workload UID execution has not bound
	// yet (409 resource_unbound; the caller retries later).
	ErrResourceUnbound = errors.New("runtimeauth: resource unbound")
	// ErrContextVersionMismatch is an attempt of one runtime wire reaching the
	// other wire's route (409 context_version_mismatch); nothing was changed.
	ErrContextVersionMismatch = errors.New("runtimeauth: context version mismatch")
	// ErrReceiptConflict is a different preparation receipt than the recorded one
	// (409 receipt_conflict).
	ErrReceiptConflict = errors.New("runtimeauth: receipt conflict")
	// ErrIdempotencyConflict is a repeat of a once-only exchange (the begin of an
	// attempt) with a different body (409 idempotency_conflict).
	ErrIdempotencyConflict = errors.New("runtimeauth: idempotency conflict")
	// ErrRefused is a business refusal of the Git grant routes that carries the
	// contract's own code (a *runtimev1.ContractError: not_found, forbidden,
	// hosting_mismatch, repository_not_empty, ...). The handler renders the code
	// the operation's inventory permits.
	ErrRefused = errors.New("runtimeauth: refused")
	// ErrPublicationConflict is an integration grant that cannot be issued because
	// the target ref no longer is what the approval was given over (base_moved) or
	// already is the approved candidate (already_integrated). The concrete error is
	// a *PublicationConflict (409 publication_conflict); no grant exists.
	ErrPublicationConflict = errors.New("runtimeauth: publication conflict")
	// ErrAlreadyIntegrated is the PublicationConflict whose default branch already
	// is the approved candidate: the integration was applied, nothing is left to
	// authorize and no grant is issued. A Go caller may treat it as success.
	ErrAlreadyIntegrated = errors.New("runtimeauth: already integrated")
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

// refusal carries a contract error as an ErrRefused.
type refusal struct{ ce *runtimev1.ContractError }

func (r *refusal) Error() string        { return ErrRefused.Error() + ": " + string(r.ce.Code) }
func (r *refusal) Is(target error) bool { return target == ErrRefused }
func (r *refusal) Unwrap() error        { return r.ce }

// Refused classifies a contract error (a gitauth or gates refusal, or one built
// here) as ErrRefused, keeping it reachable with errors.As.
func Refused(ce *runtimev1.ContractError) error { return &refusal{ce: ce} }

func refuse(code domain.ErrorCode, msg string) error {
	return Refused(&runtimev1.ContractError{Code: code, Message: msg})
}

// PublicationConflict is the 409 publication_conflict of the integration route.
type PublicationConflict struct {
	// Reason is "base_moved" or "already_integrated".
	Reason string
	// ObservedTip is the target ref's tip the control plane read (the superseding
	// commit for base_moved, the approved candidate for already_integrated).
	ObservedTip string
}

func (e *PublicationConflict) Error() string { return ErrPublicationConflict.Error() + ": " + e.Reason }
func (e *PublicationConflict) Is(target error) bool {
	return target == ErrPublicationConflict || (target == ErrAlreadyIntegrated && e.Reason == "already_integrated")
}

var kinds = []error{ErrRefused, ErrPublicationConflict, ErrInvalidRequest, ErrInvalidCredential, ErrForbidden, ErrAttemptRevoked, ErrAdmissionConsumed, ErrProfileUnsupported, ErrConflict, ErrNotFound, ErrResourceRevoked, ErrResourceUnbound, ErrContextVersionMismatch, ErrReceiptConflict, ErrIdempotencyConflict, ErrUnavailable}

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
