package observationhttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/observation"
)

// Module-defined codes.
const (
	// CodeCursorExpired is 410: the cursor is older than the retained history.
	CodeCursorExpired domain.ErrorCode = "cursor_expired"
	// CodeSnapshotTooLarge is 503: the complete snapshot exceeds the bound.
	CodeSnapshotTooLarge domain.ErrorCode = "snapshot_too_large"
)

const retryAfter = time.Second

// mapError renders an observation failure before any response is committed.
// Hidden and missing resources are indistinguishable; internals never leak.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, observation.ErrHidden), errors.Is(err, observation.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, observation.ErrDenied), errors.Is(err, observation.ErrAccessRevoked):
		return httpapi.ForbiddenError()
	case errors.Is(err, observation.ErrCursorExpired):
		return httpapi.NewError(http.StatusGone, CodeCursorExpired,
			"the cursor is older than the retained history; fetch a snapshot and resume from its cursor")
	case errors.Is(err, observation.ErrBadPosition):
		return invalidCursor()
	case errors.Is(err, observation.ErrSnapshotTooLarge):
		e := httpapi.NewError(http.StatusServiceUnavailable, CodeSnapshotTooLarge,
			"the current state is too large for a complete snapshot; narrow the stream with a type filter")
		e.Cause = err
		return e
	case errors.Is(err, observation.ErrInvalid):
		return httpapi.BadRequestError("the request is not valid for this stream")
	case errors.Is(err, observation.ErrUnavailable):
		return httpapi.UnavailableError(retryAfter, err)
	}
	return err // unclassified (including a context ending): the router renders a fixed 500/503
}

// invalidCursor is the generic response of every cursor rejection, identical to
// the shared codec's, so a probe learns nothing about which check failed.
func invalidCursor() *httpapi.APIError {
	e := httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request")
	e.Cause = httpapi.ErrInvalidCursor
	return e
}
