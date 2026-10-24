package gatewayhttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Module-defined error codes (documented in api/gateway.yaml).
const (
	CodeCredentialUnusable     domain.ErrorCode = "credential_unusable"
	CodeDestinationNotApproved domain.ErrorCode = "destination_not_approved"
	CodeRecordingUnavailable   domain.ErrorCode = "recording_unavailable"
	CodeModelNotConfigured     domain.ErrorCode = "model_not_configured"
	CodeModelNotAllowed        domain.ErrorCode = "model_not_allowed"
	CodeLimitExceeded          domain.ErrorCode = "limit_exceeded"
	CodeRunBusy                domain.ErrorCode = "run_busy"
	CodeRequestSuperseded      domain.ErrorCode = "request_superseded"
	CodeProviderError          domain.ErrorCode = "provider_error"
	CodeCredentialUnavailable  domain.ErrorCode = "credential_unavailable"
)

// apiError is a rendered failure of the workload route.
type apiError struct {
	status     int
	code       string
	message    string
	retryAfter bool
	challenge  bool
}

// messageOr returns the use case's fixed client-safe text or a default. Causes
// (driver, key-provider, provider errors) are never rendered.
func messageOr(err error, fallback string) string {
	if m := gateway.PublicMessage(err); m != "" {
		return m
	}
	return fallback
}

// mapManagementError converts a use-case error into the shared httpapi error.
func mapManagementError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	err = gateway.Classify(err)
	switch {
	case errors.Is(err, gateway.ErrInvalid):
		return httpapi.BadRequestError(messageOr(err, "the request is not valid"))
	case errors.Is(err, gateway.ErrUnauthenticated):
		return httpapi.UnauthorizedError()
	case errors.Is(err, gateway.ErrDenied), errors.Is(err, gateway.ErrForbidden):
		return httpapi.ForbiddenError()
	case errors.Is(err, gateway.ErrHidden), errors.Is(err, gateway.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, gateway.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, gateway.ErrCredential):
		return httpapi.NewError(http.StatusConflict, CodeCredentialUnusable, messageOr(err, "the credential is not a usable model secret"))
	case errors.Is(err, gateway.ErrDestination):
		return httpapi.NewError(http.StatusConflict, CodeDestinationNotApproved, "the endpoint is not an approved destination")
	case errors.Is(err, gateway.ErrRecording):
		return httpapi.NewError(http.StatusConflict, CodeRecordingUnavailable, "content recording is not available")
	}
	return httpapi.UnavailableError(0, err)
}

// mapWorkloadError converts a use-case error into the workload route's error.
// limit_exceeded is a 403, not a 429: OpenAI-compatible clients retry rate
// limits, and a run's limit never clears by waiting.
func mapWorkloadError(err error) *apiError {
	err = gateway.Classify(err)
	switch {
	case errors.Is(err, gateway.ErrInvalid):
		return &apiError{status: http.StatusBadRequest, code: string(domain.CodeInvalidRequest), message: messageOr(err, "the request is not valid")}
	case errors.Is(err, gateway.ErrUnauthenticated):
		return &apiError{status: http.StatusUnauthorized, code: string(domain.CodeInvalidCredential), message: "the credential is not valid", challenge: true}
	case errors.Is(err, gateway.ErrForbidden):
		return &apiError{status: http.StatusForbidden, code: string(domain.CodeForbidden), message: "the attempt may not use models"}
	case errors.Is(err, gateway.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: string(domain.CodeNotFound), message: "not found"}
	case errors.Is(err, gateway.ErrNotConfigured):
		return &apiError{status: http.StatusConflict, code: string(CodeModelNotConfigured), message: "model access is not configured for this project"}
	case errors.Is(err, gateway.ErrModelNotAllowed):
		return &apiError{status: http.StatusForbidden, code: string(CodeModelNotAllowed), message: "the model is not allowed for this run"}
	case errors.Is(err, gateway.ErrLimitExceeded):
		return &apiError{status: http.StatusForbidden, code: string(CodeLimitExceeded), message: messageOr(err, "a limit of this run is reached")}
	case errors.Is(err, gateway.ErrBusy):
		return &apiError{status: http.StatusConflict, code: string(CodeRunBusy), message: "another model request of this run is unresolved", retryAfter: true}
	case errors.Is(err, gateway.ErrSuperseded):
		return &apiError{status: http.StatusConflict, code: string(CodeRequestSuperseded), message: "the request was settled by reconciliation"}
	case errors.Is(err, gateway.ErrCredential):
		return &apiError{status: http.StatusServiceUnavailable, code: string(CodeCredentialUnavailable), message: "the provider credential cannot be used", retryAfter: true}
	case errors.Is(err, gateway.ErrDestination):
		return &apiError{status: http.StatusBadGateway, code: string(CodeDestinationNotApproved), message: "the destination is not approved"}
	case errors.Is(err, gateway.ErrProvider):
		return &apiError{status: http.StatusBadGateway, code: string(CodeProviderError), message: "the provider could not complete the request"}
	}
	return &apiError{status: http.StatusServiceUnavailable, code: string(domain.CodeUnavailable), message: "the service cannot decide right now; retry", retryAfter: true}
}

func writeEnvelope(w http.ResponseWriter, rid string, e *apiError) {
	body, err := json.Marshal(httpapi.ErrorEnvelope{Error: httpapi.Error{Code: e.code, Message: e.message, RequestId: rid, Details: map[string]any{}}})
	if err != nil {
		body = []byte(`{"error":{"code":"unavailable","message":"the service cannot decide right now; retry","requestId":"` + rid + `","details":{}}}`)
	}
	h := w.Header()
	h.Set(httpapi.RequestIDHeader, rid)
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if e.retryAfter {
		h.Set("Retry-After", "1")
	}
	if e.challenge {
		h.Set("WWW-Authenticate", `Bearer realm="kodama-runtime"`)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(body)
}
