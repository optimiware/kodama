package dependencieshttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

// Module-defined error codes. CodeInvalidTransition deliberately has the same
// spelling as the work module's code: a done or cancelled work item refuses a
// link change for the same reason it refuses any edit.
const (
	CodeDependencyCycle   domain.ErrorCode = "dependency_cycle"
	CodeInvalidTransition domain.ErrorCode = "invalid_transition"
)

// mapError renders a dependency error. Hidden, foreign, missing and
// unavailable cases use the shared generic messages, so nothing about
// existence, other tenants or internals leaks; graph failures carry fixed text
// and never a path.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, work.ErrHidden), errors.Is(err, work.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, work.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, work.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, work.ErrInvalid):
		return httpapi.BadRequestError(clientMessage(err, work.ErrInvalid))
	case errors.Is(err, dependencies.ErrGraph):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeDependencyCycle, clientMessage(err, dependencies.ErrGraph))
	case errors.Is(err, work.ErrInvalidTransition):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidTransition, clientMessage(err, work.ErrInvalidTransition))
	case errors.Is(err, work.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}

// clientMessage extracts the module's message after the sentinel's own text.
// Wrapped infrastructure causes are never included because only the first
// segment after the sentinel prefix is used.
func clientMessage(err error, sentinels ...error) string {
	msg := err.Error()
	for _, s := range sentinels {
		if rest, ok := strings.CutPrefix(msg, s.Error()+": "); ok {
			msg = rest
			break
		}
	}
	if i := strings.Index(msg, ": work"); i >= 0 { // a joined cause chain: keep the head
		msg = msg[:i]
	}
	if len(msg) > 512 || msg == "" {
		return "the request could not be processed"
	}
	return msg
}
