package chatopshttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// Module-defined error codes (documented in api/chatops.yaml).
const (
	// CodeNameTaken is 409: the destination name is used in the project.
	CodeNameTaken domain.ErrorCode = "name_taken"
	// CodeConflict is 409: the request conflicts with the recorded state.
	CodeConflict domain.ErrorCode = "conflict"
	// CodeInvalidTransition is 422: the lifecycle forbids the change.
	CodeInvalidTransition domain.ErrorCode = "invalid_transition"
	// CodeDestinationLimit is 422: the project has the maximum number of destinations.
	CodeDestinationLimit domain.ErrorCode = "destination_limit"
	// CodeDestinationRefused is 422: the installation's outbound policy refuses the endpoint.
	CodeDestinationRefused domain.ErrorCode = "destination_refused"
	// CodeCredentialUnavailable is 422: the credential is missing, revoked,
	// foreign or not bound to the recipient.
	CodeCredentialUnavailable domain.ErrorCode = "credential_unavailable"
	// CodeNotConfigured is 501: the installation has not configured the capability.
	CodeNotConfigured domain.ErrorCode = "not_configured"
)

// mapError renders a ChatOps error. Messages are fixed texts: they never carry
// endpoint URLs, credential references, foreign identifiers or the text of a
// dependency's error. An unclassified failure is a fixed 503 (never success,
// never detail): the use cases classify everything they return, so an unknown
// error is an outage.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return api
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, chatops.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, chatops.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, chatops.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, chatops.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, chatops.ErrNameTaken):
		return httpapi.NewError(http.StatusConflict, CodeNameTaken, "the destination name is already used in this project")
	case errors.Is(err, chatops.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeConflict, "the request conflicts with the recorded state")
	case errors.Is(err, chatops.ErrInvalidTransition):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidTransition, "the delivery's state does not allow this change")
	case errors.Is(err, chatops.ErrLimit):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeDestinationLimit, "the project has the maximum number of destinations")
	case errors.Is(err, chatops.ErrPolicy):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeDestinationRefused, "the endpoint is not approved by the installation policy")
	case errors.Is(err, chatops.ErrCredentialUnavailable):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeCredentialUnavailable, "the credential is not available for this destination")
	case errors.Is(err, chatops.ErrNotConfigured):
		return httpapi.NewError(http.StatusNotImplemented, CodeNotConfigured, "this capability is not configured")
	}
	// ErrUnavailable, an unknown commit outcome and anything unclassified.
	return httpapi.UnavailableError(time.Second, err)
}
