package gateway

// The execution-authority seam is a consumer-owned read port. It is injected
// through an explicit option and is not part of the Dependencies record.

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// ExecutionReader reads the current admitted execution context for an execution
// principal. The gateway re-reads it at every authorization boundary, so a
// stopped execution, a withdrawn permission or an expired action deadline is
// never treated as durable authority.
//
// The consumer type is the admitted execution context: the execution identity
// and owning scope, the current container action identity and deadline, and the
// permission the reader derived from trusted configuration plus the current
// executions/actions facts. The gateway never reads an execution permission
// from a caller-supplied field.
type ExecutionReader interface {
	ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error)
}

// Option configures one enabled gateway with a consumer-owned dependency that
// is not part of the declared Dependencies record.
type Option func(*options) error

// options is the resolved optional composition of one enabled gateway.
type options struct {
	executions ExecutionReader
}

// WithExecutionReader injects the current admitted-execution read seam. The
// gateway refuses to construct an enabled instance without it.
func WithExecutionReader(reader ExecutionReader) Option {
	return func(resolved *options) error {
		if reader == nil {
			return fault.New(fault.InvalidInput, "model gateway execution reader option is nil")
		}
		resolved.executions = reader
		return nil
	}
}
