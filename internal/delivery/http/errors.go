package deliveryhttp

import (
	"errors"
	"net/http"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Module-defined error codes (documented in api/delivery.yaml).
const (
	// CodeConflict is 409: changed immutable inputs for an existing identity, a
	// different report for a settled outcome, or different intended objects.
	CodeConflict domain.ErrorCode = "conflict"
	// CodeOutcomeUnknown is 409: another apply while the previous outcome is
	// unknown. The objects must be observed and the deployment settled first.
	CodeOutcomeUnknown domain.ErrorCode = "outcome_unknown"
	// CodeNameTaken is 409: the environment name is used in the project.
	CodeNameTaken domain.ErrorCode = "name_taken"
	// CodeInvalidTransition is 422: the lifecycle forbids the change.
	CodeInvalidTransition domain.ErrorCode = "invalid_transition"
	// CodeCredentialUnavailable is 422: a credential reference is missing,
	// foreign, revoked or of the wrong kind.
	CodeCredentialUnavailable domain.ErrorCode = "credential_unavailable"
	// CodeImageProvenance is 422: the image is not a verified output of the
	// stated source, producer and project.
	CodeImageProvenance domain.ErrorCode = "image_provenance_rejected"
	// CodePinRequired is 403: the attempt's immutable pin is absent, foreign or
	// mismatched. There is no fallback to the environment's current settings.
	CodePinRequired domain.ErrorCode = "pin_required"
	// CodeNotConfigured is 501: the installation has not configured the
	// capability the request needs.
	CodeNotConfigured domain.ErrorCode = "not_configured"
)

// mapError renders a delivery error. Messages are fixed texts: they never carry
// credentials, kubeconfig content, foreign identifiers, other tenants'
// resources or the text of a dependency's error. An unclassified failure is a
// fixed 503 (never success, never detail): the use cases classify everything
// they return, so an unknown error is an outage.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return api
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, delivery.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, delivery.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, delivery.ErrUnauthenticated):
		return httpapi.UnauthorizedError()
	case errors.Is(err, delivery.ErrAttemptRevoked):
		return contractError(runtimev1.CodeAttemptRevoked, "the attempt is no longer authorized")
	case errors.Is(err, delivery.ErrPinRequired):
		return httpapi.NewError(http.StatusForbidden, CodePinRequired, "the attempt's pinned configuration is absent or does not match")
	case errors.Is(err, delivery.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, delivery.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, delivery.ErrOutcomeUnknown):
		return httpapi.NewError(http.StatusConflict, CodeOutcomeUnknown, "the previous apply has an unknown outcome; observe its objects and reconcile it first")
	case errors.Is(err, delivery.ErrNameTaken):
		return httpapi.NewError(http.StatusConflict, CodeNameTaken, "the environment name is already used in this project")
	case errors.Is(err, delivery.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeConflict, "the request conflicts with the recorded state")
	case errors.Is(err, delivery.ErrInvalidTransition):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidTransition, "the deployment's state does not allow this change")
	case errors.Is(err, delivery.ErrCredentialUnavailable):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeCredentialUnavailable, "a credential is not available in this project")
	case errors.Is(err, delivery.ErrProvenance):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeImageProvenance, "the image is not a verified output of this source")
	case errors.Is(err, delivery.ErrNotConfigured):
		return httpapi.NewError(http.StatusNotImplemented, CodeNotConfigured, "this capability is not configured")
	}
	// ErrUnavailable, an unknown commit outcome and anything unclassified.
	return httpapi.UnavailableError(time.Second, err)
}

func contractError(code domain.ErrorCode, message string) *httpapi.APIError {
	status, ok := runtimev1.Status(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	return httpapi.NewError(status, code, message)
}
