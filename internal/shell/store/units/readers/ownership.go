package readers

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// OwnershipReader is the store-backed log.ExecutionOwnershipReader.
//
// It reads the execution's owning task and that task's current execution. A
// task-free execution has no association and is not_found; a cross-scope
// execution is not_found, never returned.
type OwnershipReader struct {
	executor store.Executor
}

// NewOwnershipReader builds the ownership reader.
func NewOwnershipReader(executor store.Executor) (*OwnershipReader, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "execution ownership reader requires an executor")
	}
	return &OwnershipReader{executor: executor}, nil
}

var _ log.ExecutionOwnershipReader = (*OwnershipReader)(nil)

// Ownership returns the task association of one execution inside one scope.
func (reader *OwnershipReader) Ownership(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (log.ExecutionOwnership, error) {
	if err := ctx.Err(); err != nil {
		return log.ExecutionOwnership{}, err
	}
	if err := scope.Validate(); err != nil {
		return log.ExecutionOwnership{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return log.ExecutionOwnership{}, err
	}
	row, err := reader.executor.Read(ctx, scope, "executions", string(executionID))
	if err != nil {
		return log.ExecutionOwnership{}, err
	}
	taskID, err := rowNullableString(row, "task_id")
	if err != nil {
		return log.ExecutionOwnership{}, err
	}
	if taskID == "" {
		return log.ExecutionOwnership{}, fault.New(fault.NotFound, "execution has no owning task")
	}
	taskRow, err := reader.executor.Read(ctx, scope, "tasks", taskID)
	if err != nil {
		return log.ExecutionOwnership{}, err
	}
	current, err := rowNullableString(taskRow, "current_execution_id")
	if err != nil {
		return log.ExecutionOwnership{}, err
	}
	return log.ExecutionOwnership{
		TaskID:           id.TaskID(taskID),
		CurrentExecution: id.ExecutionID(current),
	}, nil
}
