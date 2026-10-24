package logshttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/logs"
)

// translate maps a module error to the wire. Messages are fixed texts that never
// include log content, provider errors, URLs, keys, tokens or whether another
// tenant's reference exists. An unrecognized failure is never success and never
// carries detail.
func translate(err error) error {
	var api *httpapi.APIError
	switch {
	case errors.As(err, &api):
		return api
	case errors.Is(err, logs.ErrInvalid):
		return httpapi.BadRequestError(invalidMessage(err))
	case errors.Is(err, logs.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, logs.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, logs.ErrNoLog), errors.Is(err, logs.ErrWorkloadNotObserved), errors.Is(err, logs.ErrWorkloadMissing):
		return httpapi.NewError(http.StatusNotFound, domain.CodeNotFound, "no log is available for this attempt")
	case errors.Is(err, logs.ErrTooManyStreams):
		return httpapi.RateLimitedError(time.Second)
	case errors.Is(err, logs.ErrUnavailable), errors.Is(err, logs.ErrNotConfigured), errors.Is(err, logs.ErrWorkloadMismatch):
		return httpapi.UnavailableError(time.Second, err)
	}
	return httpapi.InternalServerError(err)
}

// invalidMessage keeps the module's validation texts, which name the rule and
// never a value, and bounds them.
func invalidMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), logs.ErrInvalid.Error()+": ")
	if msg == "" || len(msg) > 200 || strings.Contains(msg, "\n") {
		return "the request is not valid for this operation"
	}
	return msg
}
