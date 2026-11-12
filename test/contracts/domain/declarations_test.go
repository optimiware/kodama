package domaincontract

// The task cancellation reduction.
//
// The two task store writers (`task.question`/AskUnit and
// `task.cancel`/CancelUnit) never write `executions.state=cancelled` directly:
// that would bypass the Kernel reducer and leave no action rows, no
// StopIntent, no cleanup and no lifecycle events. Both go through the Kernel
// cancel reduction in ONE atomic receipt-first store unit, and the contract
// declares the pieces a consumer can pin without a store:
//
//   - `task.AskUnit.Commit` and `task.CancelUnit.Commit` carry the Kernel
//     cancel commit (`*flow.Commit`, JSON `commit,omitempty`) for a
//     non-terminal current execution, and are nil when there is no execution
//     to cancel or the predecessor is already terminal;
//   - `task.ExecutionAggregateReader` is the consumer-owned scoped aggregate
//     reader the task service uses to build `flow.CancelFact`:
//
//	Load(ctx context.Context, scope tenancy.Scope, id id.ExecutionID)
//	    (kernel.Execution, error)
//
//   - `task.ValidateCancellationCommit` is the pure reference rule that
//     refuses a unit whose declared execution version/identity disagrees with
//     the aggregate, and refuses a live predecessor without a commit.
//
// These vectors are declarations and pure rules only. No store, adapter,
// process or broker is involved.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var (
	cancelScope = tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-1"),
	}
	cancelAt = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	cancelTask          = id.TaskID("task-1")
	cancelExecution     = id.ExecutionID("execution-1")
	cancelOtherExec     = id.ExecutionID("execution-2")
	cancelContainer     = id.ActionID("action-container")
	cancelPendingAction = id.ActionID("action-pending")
	cancelWorkspace     = id.WorkspaceID("workspace-1")
	cancelWorkload      = id.WorkloadID("workload-1")
)

// runningExecution is a running execution with one running container action and
// one pending successor: the cancellation must stop the workload and cancel the
// pending action in the same commit.
func runningExecution() kernel.Execution {
	return kernel.Execution{
		ID:            cancelExecution,
		Version:       7,
		State:         kernel.ExecutionRunning,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: 0,
		Actions: []kernel.Action{
			{
				ID:      cancelContainer,
				Index:   0,
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionRunning,
				Version: 4,
				Container: &kernel.ContainerAction{
					WorkloadID:  cancelWorkload,
					WorkspaceID: cancelWorkspace,
					Launch:      kernel.LaunchIntent{WorkloadID: cancelWorkload, State: kernel.LaunchIssued},
				},
			},
			{
				ID:      cancelPendingAction,
				Index:   1,
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionPending,
				Version: 1,
			},
		},
	}
}

// queuedExecution is an admitted execution that never started: cancelling it
// must cancel the pending request action and never emit a stop intent.
func queuedExecution() kernel.Execution {
	return kernel.Execution{
		ID:            cancelExecution,
		Version:       2,
		State:         kernel.ExecutionQueued,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: -1,
		Actions: []kernel.Action{
			{
				ID:      cancelPendingAction,
				Index:   0,
				Kind:    definition.ActionKindRequest,
				State:   kernel.ActionPending,
				Version: 1,
				Request: &kernel.RequestAction{},
			},
		},
	}
}

// terminalCancelledExecution is an already terminal predecessor: its row is
// PRESERVED, not rewritten, so a task unit carries no commit for it.
func terminalCancelledExecution() kernel.Execution {
	return kernel.Execution{
		ID:            cancelExecution,
		Version:       9,
		State:         kernel.ExecutionCancelled,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: -1,
		Actions: []kernel.Action{
			{
				ID:      cancelContainer,
				Index:   0,
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionCancelled,
				Version: 5,
				Container: &kernel.ContainerAction{
					WorkloadID:  cancelWorkload,
					WorkspaceID: cancelWorkspace,
				},
			},
		},
	}
}

// cancelCommitFor derives the Kernel cancel commit for one non-terminal
// aggregate. Using the real reducer keeps the vector faithful to the Kernel
// cancel path instead of hand-building a lookalike payload.
func cancelCommitFor(t *testing.T, pre kernel.Execution) *flow.Commit {
	t.Helper()
	next, commit, err := flow.Reduce(pre, flow.CancelFact{
		ExecutionID: pre.ID,
		Reason:      flow.CancelRequested,
		At:          cancelAt,
	})
	if err != nil {
		t.Fatalf("flow.Reduce(cancel) refused a cancellable execution %q in state %q: %v", pre.ID, pre.State, err)
	}
	if commit == nil {
		t.Fatalf("flow.Reduce(cancel) returned no commit for %q in state %q", pre.ID, pre.State)
	}
	if next.State != kernel.ExecutionCancelled {
		t.Fatalf("reduced state = %q; want %q", next.State, kernel.ExecutionCancelled)
	}
	return commit
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..")
}

// TestTaskUnitsCarryCancellationCommit pins the declaration: both task units
// carry the Kernel cancel commit as a pointer field with the declared JSON
// tag. A missing field or a value field is a contract break, not an
// implementation detail.
func TestTaskUnitsCarryCancellationCommit(t *testing.T) {
	commitPointer := reflect.TypeOf((*flow.Commit)(nil))
	for _, unit := range []struct {
		name  string
		value any
	}{
		{name: "task.AskUnit", value: task.AskUnit{}},
		{name: "task.CancelUnit", value: task.CancelUnit{}},
	} {
		field, ok := reflect.TypeOf(unit.value).FieldByName("Commit")
		if !ok {
			t.Errorf("%s does not declare Commit *flow.Commit", unit.name)
			continue
		}
		if field.Type != commitPointer {
			t.Errorf("%s.Commit type = %v; want %v", unit.name, field.Type, commitPointer)
		}
		if got := field.Tag.Get("json"); got != "commit,omitempty" {
			t.Errorf("%s.Commit json tag = %q; want %q", unit.name, got, "commit,omitempty")
		}
	}
}

// TestTaskPortsDeclareExecutionAggregateReader pins the consumer-owned scoped
// aggregate reader. The task service must read the current execution aggregate
// (not a lossy view) to build flow.CancelFact, and the port is declared in the
// contract-owned ports.go.
func TestTaskPortsDeclareExecutionAggregateReader(t *testing.T) {
	readerType := reflect.TypeOf((*task.ExecutionAggregateReader)(nil)).Elem()
	if readerType.Kind() != reflect.Interface {
		t.Fatalf("task.ExecutionAggregateReader kind = %v; want an interface", readerType.Kind())
	}
	method, ok := readerType.MethodByName("Load")
	if !ok {
		t.Fatalf("task.ExecutionAggregateReader does not declare Load")
	}
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	scopeType := reflect.TypeOf(tenancy.Scope{})
	executionIDType := reflect.TypeOf(id.ExecutionID(""))
	executionType := reflect.TypeOf(kernel.Execution{})
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	signature := method.Type
	if signature.NumIn() != 3 {
		t.Fatalf("Load has %d parameters; want 3 (context, scope, execution id)", signature.NumIn())
	}
	if signature.In(0) != contextType || signature.In(1) != scopeType || signature.In(2) != executionIDType {
		t.Fatalf("Load parameters = (%v, %v, %v); want (context.Context, tenancy.Scope, id.ExecutionID)",
			signature.In(0), signature.In(1), signature.In(2))
	}
	if signature.NumOut() != 2 {
		t.Fatalf("Load has %d results; want 2 (kernel.Execution, error)", signature.NumOut())
	}
	if signature.Out(0) != executionType || signature.Out(1) != errorType {
		t.Fatalf("Load results = (%v, %v); want (kernel.Execution, error)", signature.Out(0), signature.Out(1))
	}

	// The declaration belongs to the contract-owned file, not an adapter file.
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "shell", "task", "ports.go"))
	if err != nil {
		t.Fatalf("internal/shell/task/ports.go is missing or unreadable: %v", err)
	}
	text := string(data)
	for _, needle := range []string{"type ExecutionAggregateReader interface", "Load(", "tenancy.Scope", "kernel.Execution"} {
		if !strings.Contains(text, needle) {
			t.Errorf("internal/shell/task/ports.go does not name %q", needle)
		}
	}
}

// TestValidateCancellationCommitIsDeclaredInContractFile pins where the pure
// rule lives: an `internal/shell/task/contract_*.go` file. A validator hidden
// in an adapter file would not be a pinned contract declaration.
func TestValidateCancellationCommitIsDeclaredInContractFile(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "shell", "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("internal/shell/task is missing or unreadable: %v", err)
	}
	declared := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "contract_") || !strings.HasSuffix(name, ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("contract file %s is unreadable: %v", name, err)
		}
		if strings.Contains(string(data), "func ValidateCancellationCommit(") {
			declared = true
		}
	}
	if !declared {
		t.Error("no internal/shell/task/contract_*.go declares func ValidateCancellationCommit(")
	}
}
