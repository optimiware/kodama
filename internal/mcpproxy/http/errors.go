package mcpproxyhttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// CodeInvalidMcpServer is 422: a well-formed registration that violates a rule
// (endpoint shape or approval, credential reference, tool names).
const CodeInvalidMcpServer domain.ErrorCode = "invalid_mcp_server"

// CodeMcpServerConflict is 409: stored state contradicts the request.
const CodeMcpServerConflict domain.ErrorCode = "mcp_server_conflict"

// mapError renders a proxy error. Hidden and missing resources are both a plain
// 404, a stale version is the shared 412, dependency outages are 503, and
// nothing about infrastructure, credentials or destinations reaches the client.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mcpproxy.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, mcpproxy.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, mcpproxy.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, mcpproxy.ErrInvalidTransition):
		// Only a tombstone refuses a change after the version matched.
		return httpapi.NotFoundError()
	case errors.Is(err, mcpproxy.ErrInvalid), errors.Is(err, mcpproxy.ErrDestination), errors.Is(err, mcpproxy.ErrCredential):
		return httpapi.NewError(http.StatusUnprocessableEntity, CodeInvalidMcpServer, "the MCP server registration violates a rule")
	case errors.Is(err, mcpproxy.ErrConflict):
		return httpapi.NewError(http.StatusConflict, CodeMcpServerConflict, "the stored registration contradicts this request")
	case errors.Is(err, mcpproxy.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err // unclassified: the router renders a fixed 500
}
