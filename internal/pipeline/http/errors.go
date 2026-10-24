package pipelinehttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Module-defined error codes.
const (
	// CodeInvalidDefinition is 422: a well-formed document that violates a
	// definition rule (graph, reference, type, source, capability or policy).
	CodeInvalidDefinition domain.ErrorCode = "invalid_definition"
	// CodeDefinitionConflict is 409: stored state contradicts the registration.
	CodeDefinitionConflict domain.ErrorCode = "definition_conflict"
)

// mapError renders a pipeline error. Hidden and missing resources are both a
// plain 404, dependency outages and unknown commit outcomes are 503 (retrying a
// content-addressed registration is safe), and nothing about infrastructure
// reaches the client. Diagnostics carry stable codes, document paths and
// constant messages; they never echo document content.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var ve *pipeline.ValidationError
	if errors.As(err, &ve) {
		return validationError(ve)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pipeline.ErrHidden), errors.Is(err, pipeline.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, pipeline.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, pipeline.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, pipeline.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeDefinitionConflict, "the stored definitions contradict this registration; contact an operator")
	case errors.Is(err, pipeline.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}

func validationError(ve *pipeline.ValidationError) *httpapi.APIError {
	diags := make([]DiagnosticDTO, 0, len(ve.Diagnostics))
	for _, d := range ve.Diagnostics {
		diags = append(diags, DiagnosticDTO{Code: d.Code, Path: d.Path, Message: d.Message})
	}
	details := map[string]any{"diagnostics": diags, "truncated": ve.Truncated}
	if ve.Stage == pipeline.StageSyntax {
		return httpapi.BadRequestError("the pipeline document is malformed").WithDetails(details)
	}
	return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidDefinition, "the pipeline definition violates one or more rules").WithDetails(details)
}

// DiagnosticDTO is one finding in an error's details.diagnostics.
type DiagnosticDTO struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}
