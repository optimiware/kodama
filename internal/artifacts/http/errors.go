package artifactshttp

import (
	"errors"
	"net/http"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// CodeArtifactIncomplete is the public rendering of an unusable download
// request; it is the runtime contract's existing code, not a new one.
const CodeArtifactIncomplete = runtimev1.CodeArtifactIncomplete

// contractError renders a runtime contract code through its frozen status.
func contractError(code domain.ErrorCode, message string) *httpapi.APIError {
	status, ok := runtimev1.Status(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	return httpapi.NewError(status, code, message)
}

// translate maps a module error to the wire. Messages are fixed texts that never
// include keys, URLs, digests, causes or whether another tenant's reference
// exists. An unrecognized failure is never success and never carries detail.
func translate(err error) error {
	var ce *runtimev1.ContractError
	var api *httpapi.APIError
	switch {
	case errors.As(err, &api):
		return api
	case errors.As(err, &ce):
		return contractError(ce.Code, "the request is not valid")
	case errors.Is(err, artifacts.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, artifacts.ErrAuthorityMissing):
		return httpapi.UnauthorizedError()
	case errors.Is(err, artifacts.ErrAttemptInactive):
		return contractError(runtimev1.CodeAttemptRevoked, "the attempt is no longer active")
	case errors.Is(err, artifacts.ErrForbidden):
		return httpapi.ForbiddenError()
	case errors.Is(err, artifacts.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, artifacts.ErrIdempotencyConflict):
		return httpapi.IdempotencyConflictError()
	case errors.Is(err, artifacts.ErrDigestMismatch):
		return contractError(runtimev1.CodeDigestMismatch, "the content does not match the declared digest")
	case errors.Is(err, artifacts.ErrIncomplete):
		return contractError(runtimev1.CodeArtifactIncomplete, "the artifact upload is not complete")
	case errors.Is(err, artifacts.ErrOutputInvalid):
		return contractError(runtimev1.CodeOutputInvalid, "the artifact is not a valid output of this attempt")
	case errors.Is(err, artifacts.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return httpapi.InternalServerError(err)
}
