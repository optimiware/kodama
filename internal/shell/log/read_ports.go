// Consumer-owned port for the execution.logs.read operation service.
//
// The retained sink can answer "which chunks are stored for this scope,
// execution, action and stream". It cannot answer "is this execution the
// current execution of its own task", which is the binding an execution
// principal needs before it may read its own task's retained content. The log
// capability therefore declares that small association port itself instead of
// importing another capability's reader.

package log

import (
	"context"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ExecutionOwnership is the task/execution association of one execution. A
// task-free execution has no association and is reported as not_found by the
// reader, never as an empty-but-usable task.
type ExecutionOwnership struct {
	TaskID           id.TaskID
	CurrentExecution id.ExecutionID
}

// ExecutionOwnershipReader reads the task association of one execution inside
// one explicit ownership scope. An execution owned by another tenant/project is
// reported as not_found, never returned.
type ExecutionOwnershipReader interface {
	Ownership(context.Context, tenancy.Scope, id.ExecutionID) (ExecutionOwnership, error)
}
