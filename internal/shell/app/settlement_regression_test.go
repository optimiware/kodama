package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// pagedSettlementScanner serves one small page at a time so a test
// can exercise discovery beyond a single page without materialising the store's
// full maximum page size.
type pagedSettlementScanner struct {
	rows    []store.Row
	page    int
	queries []store.ControllerScan
}

func (scanner *pagedSettlementScanner) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	scanner.queries = append(scanner.queries, scan)
	filtered := make([]store.Row, 0, len(scanner.rows))
	for _, row := range scanner.rows {
		if scan.Table != "" && row.Table != scan.Table {
			continue
		}
		matches := true
		for column, value := range scan.Equals {
			if row.Values[column] != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if scan.After != "" && settlementRowText(row, "id") <= scan.After {
			continue
		}
		filtered = append(filtered, row)
	}
	return settlementPage(filtered, scanner.page, func(row store.Row) string {
		return settlementRowText(row, "id")
	}), nil
}

func (scanner *pagedSettlementScanner) Read(_ context.Context, table, identity string) (store.Row, error) {
	for _, row := range scanner.rows {
		if row.Table == table && settlementRowText(row, "id") == identity {
			return row, nil
		}
	}
	return store.Row{}, fault.New(fault.NotFound, fmt.Sprintf("%s %q was not found", table, identity))
}

// pagedTaskScanner serves open-task pages for one or more project scopes and
// applies the query equality unless a test deliberately disables it to prove
// the defensive state guard.
type pagedTaskScanner struct {
	rows         map[tenancy.Scope][]store.Row
	page         int
	ignoreEquals bool
	queries      []store.ScanQuery
}

func newPagedTaskScanner(page int) *pagedTaskScanner {
	return &pagedTaskScanner{rows: make(map[tenancy.Scope][]store.Row), page: page}
}

func (scanner *pagedTaskScanner) set(scope tenancy.Scope, rows []store.Row) {
	scanner.rows[scope] = rows
}

func (scanner *pagedTaskScanner) markState(scope tenancy.Scope, taskID string, state task.State) {
	for _, row := range scanner.rows[scope] {
		if settlementRowText(row, "id") == taskID {
			row.Values["state"] = string(state)
		}
	}
}

func (scanner *pagedTaskScanner) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if query.Table != "tasks" {
		return store.QueryPage{}, nil
	}
	scanner.queries = append(scanner.queries, query)
	filtered := make([]store.Row, 0, len(scanner.rows[scope]))
	for _, row := range scanner.rows[scope] {
		matches := true
		if !scanner.ignoreEquals {
			for column, value := range query.Equals {
				if row.Values[column] != value {
					matches = false
					break
				}
			}
		}
		if !matches {
			continue
		}
		if query.After != "" && settlementRowText(row, "id") <= query.After {
			continue
		}
		filtered = append(filtered, row)
	}
	return settlementPage(filtered, scanner.page, func(row store.Row) string {
		return settlementRowText(row, "id")
	}), nil
}

// settlementPage returns at most page rows and an opaque cursor for the next
// page.
func settlementPage(rows []store.Row, page int, identity func(store.Row) string) store.QueryPage {
	if page <= 0 {
		page = 1
	}
	if len(rows) <= page {
		return store.QueryPage{Rows: rows}
	}
	next := identity(rows[page-1])
	return store.QueryPage{Rows: rows[:page], Next: next, HasMore: true}
}

func settlementRowText(row store.Row, column string) string {
	value, _ := row.Values[column].(string)
	return value
}

// recordingProgress records every progress command and reports a completed
// task, matching the real task capability's successful settle result.
type recordingProgress struct {
	commands   []task.ProgressCommand
	onProgress func(task.ProgressCommand)
	errFor     func(task.ProgressCommand) error
}

func (progress *recordingProgress) Progress(_ context.Context, command task.ProgressCommand) (task.ProgressStatus, error) {
	progress.commands = append(progress.commands, command)
	if progress.onProgress != nil {
		progress.onProgress(command)
	}
	if progress.errFor != nil {
		if err := progress.errFor(command); err != nil {
			return task.ProgressStatus{}, err
		}
	}
	return task.ProgressStatus{State: task.StateDone, Applied: true}, nil
}

func (progress *recordingProgress) count(executionID string) int {
	count := 0
	for _, command := range progress.commands {
		if string(command.ExecutionID) == executionID {
			count++
		}
	}
	return count
}

func settlementTaskRow(scope tenancy.Scope, taskID string, state task.State, executionID string) store.Row {
	return store.Row{Table: "tasks", Values: map[string]any{
		"id":                   taskID,
		"tenant_id":            string(scope.TenantID),
		"project_id":           string(scope.ProjectID),
		"state":                string(state),
		"current_execution_id": executionID,
	}}
}

func settlementExecutionRow(scope tenancy.Scope, executionID, taskID string, state kernel.ExecutionState) store.Row {
	return store.Row{Table: "executions", Values: map[string]any{
		"id":         executionID,
		"task_id":    taskID,
		"tenant_id":  string(scope.TenantID),
		"project_id": string(scope.ProjectID),
		"state":      string(state),
	}}
}

// TestSettlementReachesTaskBeyondPageCap proves discovery is bounded by
// unsettled work and carries its task cursor across passes. A target task whose
// current execution succeeded is ordered after more than 64 pages of open
// tasks whose executions are still running; an execution scan capped at 64
// pages would return an internal failure at its cap and never observe the
// target. Once the target settles it must not be reported again.
func TestSettlementReachesTaskBeyondPageCap(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	const preceding = 100 // one row per page, beyond a 64-page cap.
	executions := &pagedSettlementScanner{page: 1}
	tasks := newPagedTaskScanner(1)

	taskRows := make([]store.Row, 0, preceding+1)
	for index := range preceding {
		taskID := fmt.Sprintf("task-%03d", index)
		executionID := fmt.Sprintf("execution-%03d", index)
		taskRows = append(taskRows, settlementTaskRow(scope, taskID, task.StateOpen, executionID))
		executions.rows = append(executions.rows, settlementExecutionRow(scope, executionID, taskID, kernel.ExecutionRunning))
	}
	const targetTask = "task-target"
	const targetExecution = "execution-target"
	taskRows = append(taskRows, settlementTaskRow(scope, targetTask, task.StateOpen, targetExecution))
	executions.rows = append(executions.rows, settlementExecutionRow(scope, targetExecution, targetTask, kernel.ExecutionSucceeded))
	tasks.set(scope, taskRows)

	progress := &recordingProgress{onProgress: func(command task.ProgressCommand) {
		if string(command.TaskID) == targetTask {
			tasks.markState(scope, targetTask, task.StateDone)
		}
	}}
	settlement := newTaskSettlement(executions, executions, tasks, progress)

	for pass := range preceding + 5 {
		if err := settlement.settle(context.Background()); err != nil {
			t.Fatalf("pass %d failed at a page boundary: %v", pass, err)
		}
	}
	if got := progress.count(targetExecution); got != 1 {
		t.Fatalf("target execution settled %d times; want 1", got)
	}
	for index := range preceding {
		if got := progress.count(fmt.Sprintf("execution-%03d", index)); got != 0 {
			t.Fatalf("running execution %03d was reported %d times; want 0", index, got)
		}
	}
	// Later passes must not re-read a done task through Progress.
	for pass := range 5 {
		if err := settlement.settle(context.Background()); err != nil {
			t.Fatalf("later pass %d failed: %v", pass, err)
		}
	}
	if got := progress.count(targetExecution); got != 1 {
		t.Fatalf("done task was reported %d times; want 1", got)
	}
}

// TestSettlementDiscoversScopeBeyondExecutionPageCap proves the
// succeeded-execution scope discovery carries its cursor across passes. The
// target scope's only succeeded execution is ordered after more than 64 pages;
// a scan capped at 64 pages would return an internal failure and never reach
// it.
func TestSettlementDiscoversScopeBeyondExecutionPageCap(t *testing.T) {
	otherScope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	targetScope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-b"}
	const preceding = 100 // one row per page, beyond a 64-page cap.
	executions := &pagedSettlementScanner{page: 1}
	tasks := newPagedTaskScanner(10)

	for index := range preceding {
		executions.rows = append(executions.rows, settlementExecutionRow(
			otherScope, fmt.Sprintf("execution-%03d", index), fmt.Sprintf("task-%03d", index), kernel.ExecutionSucceeded))
	}
	const targetTask = "task-target"
	const targetExecution = "execution-target"
	executions.rows = append(executions.rows, settlementExecutionRow(targetScope, targetExecution, targetTask, kernel.ExecutionSucceeded))
	tasks.set(targetScope, []store.Row{settlementTaskRow(targetScope, targetTask, task.StateOpen, targetExecution)})

	progress := &recordingProgress{onProgress: func(command task.ProgressCommand) {
		if string(command.TaskID) == targetTask {
			tasks.markState(targetScope, targetTask, task.StateDone)
		}
	}}
	settlement := newTaskSettlement(executions, executions, tasks, progress)

	for pass := range 20 {
		if err := settlement.settle(context.Background()); err != nil {
			t.Fatalf("pass %d failed at a page boundary: %v", pass, err)
		}
	}
	if got := progress.count(targetExecution); got != 1 {
		t.Fatalf("target execution settled %d times; want 1", got)
	}
}

// TestSettlementRefusedTaskDoesNotBlockLaterTasks proves a permanent refusal is
// recorded but the pass continues to the remaining rows. The refused task is
// retried on a later pass, while the later task settles in the same pass.
func TestSettlementRefusedTaskDoesNotBlockLaterTasks(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	executions := &pagedSettlementScanner{page: 10, rows: []store.Row{
		settlementExecutionRow(scope, "execution-first", "task-first", kernel.ExecutionSucceeded),
		settlementExecutionRow(scope, "execution-second", "task-second", kernel.ExecutionSucceeded),
	}}
	tasks := newPagedTaskScanner(10)
	tasks.set(scope, []store.Row{
		settlementTaskRow(scope, "task-first", task.StateOpen, "execution-first"),
		settlementTaskRow(scope, "task-second", task.StateOpen, "execution-second"),
	})

	progress := &recordingProgress{errFor: func(command task.ProgressCommand) error {
		if string(command.TaskID) == "task-first" {
			return fault.New(fault.InternalFailure, "permanent settlement refusal")
		}
		return nil
	}}
	settlement := newTaskSettlement(executions, executions, tasks, progress)
	err := settlement.settle(context.Background())
	if err == nil {
		t.Fatal("settle did not report the permanent refusal")
	}
	if got := progress.count("execution-second"); got != 1 {
		t.Fatalf("later task settled %d times; want 1", got)
	}
}

// TestSettlementDoesNotReportFailedExecution proves failed executions are not
// discovery input: the scan asks only for succeeded rows and an open task with
// a failed current execution is not reported to Progress.
func TestSettlementDoesNotReportFailedExecution(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	executions := &pagedSettlementScanner{page: 10, rows: []store.Row{
		// A succeeded execution in the same scope makes the scope discoverable
		// so the task scan actually runs.
		settlementExecutionRow(scope, "execution-scope", "task-scope", kernel.ExecutionSucceeded),
		settlementExecutionRow(scope, "execution-failed", "task-failed", kernel.ExecutionFailed),
	}}
	tasks := newPagedTaskScanner(10)
	tasks.set(scope, []store.Row{
		settlementTaskRow(scope, "task-failed", task.StateOpen, "execution-failed"),
	})

	progress := &recordingProgress{}
	settlement := newTaskSettlement(executions, executions, tasks, progress)
	if err := settlement.settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(progress.commands) != 0 {
		t.Fatalf("failed execution was reported %d times; want 0", len(progress.commands))
	}
	if len(executions.queries) == 0 {
		t.Fatal("settlement did not scan executions for scope discovery")
	}
	if got := executions.queries[0].Equals["state"]; got != string(kernel.ExecutionSucceeded) {
		t.Fatalf("execution discovery state filter = %v; want %q", got, kernel.ExecutionSucceeded)
	}
}

// TestSettlementDoesNotReportDoneTask pins the defensive state guard and the
// open-task filter even when an adapter returns a row outside the query. A done
// task is never re-read by Progress.
func TestSettlementDoesNotReportDoneTask(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	executions := &pagedSettlementScanner{page: 10, rows: []store.Row{
		settlementExecutionRow(scope, "execution-done", "task-done", kernel.ExecutionSucceeded),
	}}
	tasks := newPagedTaskScanner(10)
	tasks.ignoreEquals = true
	tasks.set(scope, []store.Row{
		settlementTaskRow(scope, "task-done", task.StateDone, "execution-done"),
	})

	progress := &recordingProgress{}
	settlement := newTaskSettlement(executions, executions, tasks, progress)
	if err := settlement.settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(progress.commands) != 0 {
		t.Fatalf("done task was reported %d times; want 0", len(progress.commands))
	}
	if len(tasks.queries) == 0 {
		t.Fatal("settlement did not scan tasks")
	}
	if got := tasks.queries[0].Equals["state"]; got != string(task.StateOpen) {
		t.Fatalf("task scan state filter = %v; want %q", got, task.StateOpen)
	}
}
