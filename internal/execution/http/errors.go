package executionhttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Module-defined error codes.
const (
	// CodeAdmissionRefused is 422: the definition, a step kind, an input, a secret
	// reference or the source commit was rejected before any state was created.
	CodeAdmissionRefused domain.ErrorCode = "admission_refused"
	// CodeRunConflict is 409: a stale content revision or a state that contradicts
	// the request.
	CodeRunConflict domain.ErrorCode = "run_conflict"
	// CodeWorkItemNotRunnable is 409: the work item is cancelled or not actionable.
	CodeWorkItemNotRunnable domain.ErrorCode = "work_item_not_runnable"
)

// mapError renders an execution error. Hidden and missing resources are both a
// plain 404, dependency outages are 503 with Retry-After, and nothing about
// infrastructure, peers or values reaches the client: every message is constant.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrPurposeDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, execution.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, execution.ErrAdmissionRefused):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeAdmissionRefused, "the run was refused before dispatch; check the definition, inputs and source")
	case errors.Is(err, execution.ErrWorkCancelled), errors.Is(err, execution.ErrNotActionable):
		return httpapi.NewError(http.StatusConflict, CodeWorkItemNotRunnable, "the work item admits no new work")
	case errors.Is(err, execution.ErrConflict), errors.Is(err, execution.ErrInvalidTransition), errors.Is(err, execution.ErrStale):
		return httpapi.NewError(http.StatusConflict, CodeRunConflict, "the request conflicts with the current state of the run or work item")
	case errors.Is(err, execution.ErrUnavailable), errors.Is(err, execution.ErrNotConfigured), errors.Is(err, execution.ErrBusy), errors.Is(err, execution.ErrQuotaExceeded):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}
