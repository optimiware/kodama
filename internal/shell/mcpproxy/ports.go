package mcpproxy

import (
	"context"
	"io"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// Transport performs one bounded external MCP tool call. It receives only the
// trusted request the proxy built from configuration and validated arguments.
//
// A transport reports a known pre-effect unavailability as
// *UpstreamError{Dispatched: false, Class: fault.DependencyUnavailable}. Every
// other failure — including an error after the request may have been written —
// is ambiguous and must be treated as an uncertain external effect.
type Transport interface {
	Call(context.Context, UpstreamRequest) (io.ReadCloser, error)
}

// ExecutionReader reads the current admitted execution context for an execution
// principal. The proxy re-reads it at every authorization boundary, so a
// stopped execution or a withdrawn permission is never treated as durable
// authority.
//
// Implementations return the execution's identity and owning scope/version/
// state together with the current container action's identity, kind, state and
// deadline, read from the executions/actions columns. They derive the
// AdmittedExecution permission from trusted configuration plus those current
// facts; no execution permission is read from a dedicated column and the reader
// is never a grant of authority on its own.
type ExecutionReader interface {
	ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error)
}
