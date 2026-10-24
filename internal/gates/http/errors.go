package gateshttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Module-defined error codes.
const (
	// CodeStaleGateSubject is 409: the digest is not the gate's, the gate expired
	// or was invalidated, or the current facts differ from its subject.
	CodeStaleGateSubject domain.ErrorCode = "stale_gate_subject"
	// CodeInvalidTransition is 422: the gate is already decided.
	CodeInvalidTransition domain.ErrorCode = "invalid_transition"
	// CodeEvidenceInvalid is 422: referenced evidence failed verification.
	CodeEvidenceInvalid domain.ErrorCode = "evidence_invalid"
)

// mapError renders a gate error. Hidden and missing resources are both a plain
// 404, dependency outages are 503 with Retry-After, and nothing about
// infrastructure, peers, evidence content or foreign identifiers reaches the
// client: every message is constant. Conformance denials carry T-003's code
// (conformance_blocked and its siblings), which has no HTTP status in the
// runtime contract; this route maps all of them to 422.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var se *gates.StaleError
	var ce *gates.ConformanceError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gates.ErrHidden), errors.Is(err, gates.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, gates.ErrDenied), errors.Is(err, gates.ErrReviewerAuthority):
		return httpapi.ForbiddenError()
	case errors.Is(err, gates.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, gates.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.As(err, &se):
		return httpapi.NewError(http.StatusConflict, CodeStaleGateSubject, "the gate subject is not current; reread the gate").
			WithDetails(map[string]any{"reason": se.Reason})
	case errors.Is(err, gates.ErrStaleSubject):
		return httpapi.NewError(http.StatusConflict, CodeStaleGateSubject, "the gate subject is not current; reread the gate").
			WithDetails(map[string]any{"reason": "stale"})
	case errors.As(err, &ce):
		return httpapi.NewError(http.StatusUnprocessableEntity, domain.ErrorCode(ce.Code), "the design conformance evidence does not permit approval")
	case errors.Is(err, gates.ErrConformance):
		return httpapi.NewError(http.StatusUnprocessableEntity, domain.ErrorCode(gates.CodeConformanceMissing), "the design conformance evidence does not permit approval")
	case errors.Is(err, gates.ErrInvalidTransition):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidTransition, "the gate cannot take this decision in its current state")
	case errors.Is(err, gates.ErrEvidence):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeEvidenceInvalid, "the gate evidence could not be verified")
	case errors.Is(err, gates.ErrConflict):
		return httpapi.NewError(http.StatusConflict, domain.CodeIdempotencyConflict, "the request conflicts with the current state")
	case errors.Is(err, gates.ErrUnavailable), errors.Is(err, gates.ErrNotConfigured):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}
