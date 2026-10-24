package agentshttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Module-defined error codes.
const (
	// CodeInvalidProfile is 422: a well-formed profile that violates a rule
	// (field syntax, bounds, design baseline, unapproved model or tool).
	CodeInvalidProfile domain.ErrorCode = "invalid_profile"
	// CodeProfileUnsupported is 422: the wrapper, harness and image combination
	// or a claimed control has no compatibility evidence (AR-02).
	CodeProfileUnsupported domain.ErrorCode = "profile_unsupported"
	// CodeProfileConflict is 409: stored state contradicts the registration.
	CodeProfileConflict domain.ErrorCode = "profile_conflict"
)

// mapError renders an agents error. Hidden and missing resources are both a
// plain 404, dependency outages and unknown commit outcomes are 503 (retrying a
// content-addressed registration is safe), and nothing about infrastructure,
// instructions or other profile content reaches the client. Diagnostics carry
// stable codes, paths and constant messages.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var ve *agents.ValidationError
	if errors.As(err, &ve) {
		return validationError(ve)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agents.ErrHidden), errors.Is(err, agents.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, agents.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, agents.ErrUnsupported):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeProfileUnsupported,
			"the wrapper, harness and image combination or a claimed control has no compatibility evidence")
	case errors.Is(err, agents.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, agents.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeProfileConflict, "the stored profiles contradict this registration; contact an operator")
	case errors.Is(err, agents.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}

func validationError(ve *agents.ValidationError) *httpapi.APIError {
	diags := make([]DiagnosticDTO, 0, len(ve.Diagnostics))
	for _, d := range ve.Diagnostics {
		diags = append(diags, DiagnosticDTO{Code: d.Code, Path: d.Path, Message: d.Message})
	}
	return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidProfile, "the agent profile violates one or more rules").
		WithDetails(map[string]any{"diagnostics": diags, "truncated": ve.Truncated})
}

// DiagnosticDTO is one finding in an error's details.diagnostics.
type DiagnosticDTO struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}
