package workhttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
)

// Module-defined error codes.
const (
	CodeInvalidTransition domain.ErrorCode = "invalid_transition"
	CodePrerequisiteUnmet domain.ErrorCode = "prerequisite_unmet"
	CodeWorkspaceBusy     domain.ErrorCode = "workspace_busy"
	CodeWIPLimitReached   domain.ErrorCode = "wip_limit_reached"
	CodeStaleReviewSubj   domain.ErrorCode = "stale_review_subject"
	CodeHumanRequired     domain.ErrorCode = "human_required"
)

// mapError renders a work error. Messages of business failures are the
// module's constant, client-safe text; hidden, missing and unavailable cases use
// the shared generic messages so nothing about existence or internals leaks.
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
	case errors.Is(err, work.ErrNotHuman):
		return httpapi.NewError(http.StatusForbidden, CodeHumanRequired, "this decision must be made by a person holding the required permission")
	case errors.Is(err, work.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, work.ErrInvalid):
		return httpapi.BadRequestError(clientMessage(err, work.ErrInvalid))
	case errors.Is(err, work.ErrInvalidTransition), errors.Is(err, work.ErrNotActionable), errors.Is(err, work.ErrCancelled):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidTransition, clientMessage(err, work.ErrInvalidTransition, work.ErrNotActionable, work.ErrCancelled))
	case errors.Is(err, work.ErrPrerequisite):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodePrerequisiteUnmet, clientMessage(err, work.ErrPrerequisite))
	case errors.Is(err, work.ErrWorkspaceBusy):
		return httpapi.NewError(http.StatusConflict, CodeWorkspaceBusy, "a writer attempt is unsettled; cancel the run and retry after it settles")
	case errors.Is(err, work.ErrWIPLimit):
		return httpapi.NewError(http.StatusConflict, CodeWIPLimitReached, "the project's active work limit is reached")
	case errors.Is(err, work.ErrStaleSubject):
		return httpapi.NewError(http.StatusConflict, CodeStaleReviewSubj, clientMessage(err, work.ErrStaleSubject))
	case errors.Is(err, work.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}

// clientMessage extracts the module's message after the sentinel's own text,
// for example "work: invalid request: title must not be empty" -> "title must not
// be empty". Wrapped infrastructure causes are never included because only the
// first segment after the sentinel prefix is used.
func clientMessage(err error, sentinels ...error) string {
	msg := err.Error()
	for _, s := range sentinels {
		if rest, ok := strings.CutPrefix(msg, s.Error()+": "); ok {
			msg = rest
			break
		}
	}
	if i := strings.Index(msg, ": work: "); i >= 0 { // a joined cause chain: keep the head
		msg = msg[:i]
	}
	if len(msg) > 512 || msg == "" {
		return "the request could not be processed"
	}
	return msg
}
