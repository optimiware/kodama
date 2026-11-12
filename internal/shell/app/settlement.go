package app

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Settlement discovery is bounded by unsettled work rather than factory
// history:
//
//   - A succeeded execution only matters when it is the current execution of an
//     open task. The settlement pass therefore scans the tasks table in the
//     open state, follows each task's current execution, and reports progress
//     only when that execution succeeded. A task that is already done drops out
//     of the scan, and failed/cancelled executions are never inspected for
//     progress because the task capability accepts only a current success.
//   - The cross-scope controller read authority is exactly executions,
//     actions, cleanup and events; it cannot list tasks. A bounded page of
//     succeeded executions per pass therefore discovers the project scopes that
//     contain terminal work, while the scoped scanner reads the tasks in those
//     scopes. This scan carries its cursor across passes and wraps after the
//     end, so it can never starve rows behind a page cap.
//   - Task pages carry their cursor across passes per scope and wrap after the
//     end. A permanently refused task is recorded once and does not stop later
//     rows from being observed.
const (
	// settlementScopePagesPerPass bounds the scope-discovery work in one pass.
	// The cursor is retained, so a larger execution history is covered over
	// successive passes instead of failing at a cap.
	settlementScopePagesPerPass = 8
	// settlementScopeBatch bounds how many known scopes get a task page in one
	// pass. Scopes are visited round-robin, so every known scope is reached.
	settlementScopeBatch = 8
	// settlementTaskPageSize is the task page bound; MaxQueryLimit is the
	// largest page the store scanner accepts.
	settlementTaskPageSize = store.MaxQueryLimit
)

// progressSettler is the app-owned task-progress settlement port. The task
// capability owns the current-only, version-guarded completion decision; the
// composition only reports the terminal execution observation.
type progressSettler interface {
	Progress(context.Context, task.ProgressCommand) (task.ProgressStatus, error)
}

// settlementScanner is the cross-scope controller scan seam used to discover
// the project scopes that contain succeeded executions. It is the same seam
// the Kernel recovery scan consumes; the owning tenant/project scope is read
// from the execution row.
type settlementScanner interface {
	Scan(context.Context, store.ControllerScan) (store.QueryPage, error)
}

// settlementExecutionReader reads one execution row by identity through the
// same cross-scope controller seam. Settlement follows a task's current
// execution pointer and needs the authoritative state of that row.
type settlementExecutionReader interface {
	Read(context.Context, string, string) (store.Row, error)
}

// taskSettlement reports persisted current-execution success to the task
// capability. Discovery is a rescan of durable state rather than an in-memory
// hook, so a crash between the execution commit and the settlement observation
// loses nothing: the next start scans open tasks again.
type taskSettlement struct {
	scanner    settlementScanner
	executions settlementExecutionReader
	tasks      store.Scanner
	progress   progressSettler

	// scopes and order are the project scopes discovered from succeeded
	// executions. The set is bounded by the number of projects, not by factory
	// history.
	scopes     map[tenancy.Scope]struct{}
	order      []tenancy.Scope
	nextScope  int
	scopeAfter string
	// taskAfter carries one keyset cursor per known scope across passes. An
	// empty cursor starts a fresh page sequence, which is also the restart
	// state.
	taskAfter map[tenancy.Scope]string
}

// newTaskSettlement wires the discovery seam, the execution reader, the task
// scanner and the task progress port.
func newTaskSettlement(
	scanner settlementScanner,
	executions settlementExecutionReader,
	tasks store.Scanner,
	progress progressSettler,
) *taskSettlement {
	return &taskSettlement{
		scanner:    scanner,
		executions: executions,
		tasks:      tasks,
		progress:   progress,
		scopes:     make(map[tenancy.Scope]struct{}),
		taskAfter:  make(map[tenancy.Scope]string),
	}
}

// settle performs one bounded discovery pass over unsettled work. A task that
// is open and whose current execution succeeded is reported to the task
// capability. A single permanent refusal is returned after the pass so the
// supervised loop backs off and logs its fault class once per pause instead of
// retrying it in a hot loop. Concurrent/stale observations are benign no-ops.
func (settlement *taskSettlement) settle(ctx context.Context) error {
	if settlement == nil || settlement.scanner == nil || settlement.executions == nil ||
		settlement.tasks == nil || settlement.progress == nil {
		return fault.New(fault.InternalFailure, "task settlement is not wired")
	}
	var firstErr error
	if err := settlement.discoverScopes(ctx, &firstErr); err != nil {
		return err
	}
	if err := settlement.settleTasks(ctx, &firstErr); err != nil {
		return err
	}
	return firstErr
}

// discoverScopes advances the succeeded-execution discovery cursor by at most
// settlementScopePagesPerPass pages and remembers every project scope it sees.
// It never fails because a page bound was reached: the cursor is carried to the
// next pass and wraps to the beginning after the last page. A scan error is
// recorded and the pass still uses the scopes already known; the cursor is left
// in place so the next pass retries the same page.
func (settlement *taskSettlement) discoverScopes(ctx context.Context, firstErr *error) error {
	query := store.ScanQuery{
		Table:   "executions",
		Equals:  map[string]any{"state": string(kernel.ExecutionSucceeded)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	}
	for range settlementScopePagesPerPass {
		if err := ctx.Err(); err != nil {
			return err
		}
		query.After = settlement.scopeAfter
		result, err := settlement.scanner.Scan(ctx, store.ControllerScan{ScanQuery: query})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if *firstErr == nil {
				*firstErr = err
			}
			return nil
		}
		for _, row := range result.Rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			scope, err := settlementScope(row)
			if err != nil {
				if *firstErr == nil {
					*firstErr = err
				}
				continue
			}
			settlement.rememberScope(scope)
		}
		if !result.HasMore {
			settlement.scopeAfter = ""
			return nil
		}
		settlement.scopeAfter = result.Next
	}
	return nil
}

// rememberScope adds one discovered project scope to the round-robin set.
func (settlement *taskSettlement) rememberScope(scope tenancy.Scope) {
	if _, known := settlement.scopes[scope]; known {
		return
	}
	settlement.scopes[scope] = struct{}{}
	settlement.order = append(settlement.order, scope)
}

// settleTasks advances one task page for a bounded batch of known scopes. A
// scope with many open tasks is resumed from its own cursor on a later pass; a
// scope with a permanent refusal does not stop the remaining scopes.
func (settlement *taskSettlement) settleTasks(ctx context.Context, firstErr *error) error {
	if len(settlement.order) == 0 {
		return nil
	}
	batch := min(settlementScopeBatch, len(settlement.order))
	for offset := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		scope := settlement.order[(settlement.nextScope+offset)%len(settlement.order)]
		if err := settlement.settleScope(ctx, scope, firstErr); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if *firstErr == nil {
				*firstErr = err
			}
		}
	}
	settlement.nextScope = (settlement.nextScope + batch) % len(settlement.order)
	return nil
}

// settleScope advances one open-task page for one project scope and reports
// each open task whose current execution succeeded. The cursor advances even
// when a task is refused, so a permanently refused task cannot starve later
// rows.
func (settlement *taskSettlement) settleScope(ctx context.Context, scope tenancy.Scope, firstErr *error) error {
	query := store.ScanQuery{
		Table:   "tasks",
		Equals:  map[string]any{"state": string(task.StateOpen)},
		OrderBy: []store.Order{{Column: "id"}},
		After:   settlement.taskAfter[scope],
		Limit:   settlementTaskPageSize,
	}
	result, err := settlement.tasks.Query(ctx, scope, query)
	if err != nil {
		return err
	}
	for _, row := range result.Rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := settlement.observeTask(ctx, scope, row); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if *firstErr == nil {
				*firstErr = err
			}
		}
	}
	if result.HasMore {
		settlement.taskAfter[scope] = result.Next
	} else {
		settlement.taskAfter[scope] = ""
	}
	return nil
}

// observeTask reports one open task's current execution to the task
// capability when that execution succeeded. An open task with no current
// execution, or with a running/failed/cancelled one, needs no progress
// observation.
func (settlement *taskSettlement) observeTask(ctx context.Context, scope tenancy.Scope, row store.Row) error {
	state, err := settlementText(row, "state")
	if err != nil {
		return err
	}
	if task.State(state) != task.StateOpen {
		return nil
	}
	taskText, err := settlementText(row, "id")
	if err != nil {
		return err
	}
	if taskText == "" {
		return fault.New(fault.InternalFailure, "open task row has no identity")
	}
	executionText, err := settlementText(row, "current_execution_id")
	if err != nil {
		return err
	}
	if executionText == "" {
		return nil
	}
	execution, err := settlement.executions.Read(ctx, "executions", executionText)
	if err != nil {
		return err
	}
	executionState, err := settlementText(execution, "state")
	if err != nil {
		return err
	}
	if kernel.ExecutionState(executionState) != kernel.ExecutionSucceeded {
		return nil
	}
	executionTask, err := settlementText(execution, "task_id")
	if err != nil {
		return err
	}
	if executionTask == "" {
		return fault.New(fault.InternalFailure, "current execution has no owning task")
	}
	if executionTask != taskText {
		return fault.New(fault.InternalFailure, "current execution belongs to another task")
	}
	executionScope, err := settlementScope(execution)
	if err != nil {
		return err
	}
	if executionScope != scope {
		return fault.New(fault.InternalFailure, "current execution is outside the task scope")
	}
	executionID, err := settlementText(execution, "id")
	if err != nil {
		return err
	}
	if executionID == "" {
		return fault.New(fault.InternalFailure, "current execution row has no identity")
	}
	if _, err := settlement.progress.Progress(ctx, task.ProgressCommand{
		Scope:       executionScope,
		TaskID:      id.TaskID(taskText),
		ExecutionID: id.ExecutionID(executionID),
		Outcome:     task.ProgressSucceeded,
	}); err != nil {
		if settlementBenign(err) {
			return nil
		}
		return err
	}
	return nil
}

// settlementScope reads the owning project scope from a controller or task row.
func settlementScope(row store.Row) (tenancy.Scope, error) {
	tenantText, err := settlementText(row, "tenant_id")
	if err != nil {
		return tenancy.Scope{}, err
	}
	projectText, err := settlementText(row, "project_id")
	if err != nil {
		return tenancy.Scope{}, err
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenantText), ProjectID: id.ProjectID(projectText)}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.InternalFailure, "row has no usable owning scope", err)
	}
	return scope, nil
}

// settlementBenign reports whether a progress refusal is an expected
// concurrency outcome rather than a fault: a newer task transition won the
// version guard or the task is no longer present in scope.
func settlementBenign(err error) bool {
	class, ok := fault.ClassOf(err)
	if !ok {
		return false
	}
	return class == fault.VersionConflict || class == fault.NotFound
}

// settlementText reads one text column from a row. A missing or NULL column is
// the empty string; a value of another type is an internal failure.
func settlementText(row store.Row, column string) (string, error) {
	value, present := row.Values[column]
	if !present || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("settlement row %s column %q is not text", row.Table, column))
	}
	return text, nil
}
