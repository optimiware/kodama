package identityhttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
)

// CodeAlreadyExists is the module-defined code of a duplicate binding or project key.
const CodeAlreadyExists domain.ErrorCode = "already_exists"

// mapError renders an identity error. Messages of business failures are the
// module's constant client-safe text; hidden, missing and unavailable cases use
// the shared generic messages so nothing about existence or internals leaks.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden), errors.Is(err, identity.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, identity.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, identity.ErrUnauthenticated):
		return httpapi.UnauthorizedError()
	case errors.Is(err, identity.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, identity.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeAlreadyExists, "a binding or project with these identifying values already exists")
	case errors.Is(err, identity.ErrInvalid):
		return httpapi.BadRequestError(clientMessage(err, identity.ErrInvalid))
	case errors.Is(err, identity.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}

// clientMessage extracts the module's message after the sentinel's own text.
func clientMessage(err error, s error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, s.Error()+": "); ok {
		msg = rest
	}
	if i := strings.Index(msg, ": identity: "); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 512 || msg == "" {
		return "the request could not be processed"
	}
	return msg
}
