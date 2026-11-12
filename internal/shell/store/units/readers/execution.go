package readers

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ExecutionReader is the store-backed task.ExecutionReader.
//
// It reads the execution aggregate through the shared execution-store codec, so
// the version, state and persisted stop/cleanup evidence are decoded exactly as
// the Kernel wrote them. It adds no state-machine policy: the task service
// decides whether a predecessor blocks dispatch.
type ExecutionReader struct {
	store *executionstore.Store
}

// NewExecutionReader builds the execution reader over the neutral seams. Both
// an executor and a scanner are required: the aggregate spans the executions
// row and its action rows.
func NewExecutionReader(executor store.Executor, scanner store.Scanner) (*ExecutionReader, error) {
	if executor == nil || scanner == nil {
		return nil, fault.New(fault.InvalidInput, "execution reader requires an executor and a scanner")
	}
	return &ExecutionReader{store: executionstore.New(executor, scanner)}, nil
}

var _ task.ExecutionReader = (*ExecutionReader)(nil)

// Get returns one scoped execution view. An execution owned by another tenant
// or project is not_found, never returned.
func (reader *ExecutionReader) Get(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (task.ExecutionView, error) {
	if err := ctx.Err(); err != nil {
		return task.ExecutionView{}, err
	}
	if err := scope.Validate(); err != nil {
		return task.ExecutionView{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return task.ExecutionView{}, err
	}
	execution, err := reader.store.Load(ctx, scope, executionID)
	if err != nil {
		return task.ExecutionView{}, err
	}
	return task.ExecutionView{
		ID:      execution.ID,
		Version: execution.Version,
		State:   execution.State,
		// The execution store owns the one persisted stop-evidence derivation,
		// so the task service's dispatch decision and the Shell predecessor
		// barrier used by queued discovery and activation cannot drift.
		StopEvidence: executionstore.StopEvidence(execution),
	}, nil
}
