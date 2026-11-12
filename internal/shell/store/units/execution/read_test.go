package executionstore

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestReadListPaginatesStablyWithoutRepeats(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	for _, executionID := range []id.ExecutionID{"exec-a", "exec-b", "exec-c"} {
		if _, err := adapter.Start(context.Background(), startUnit(scope, executionID, "start-"+string(executionID), containerAction())); err != nil {
			t.Fatalf("Start %s: %v", executionID, err)
		}
	}

	first, err := adapter.List(context.Background(), scope, execution.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Items) != 2 || first.Next == "" {
		t.Fatalf("first page = %d items next=%q; want 2 items and a cursor", len(first.Items), first.Next)
	}
	second, err := adapter.List(context.Background(), scope, execution.ListFilter{Limit: 2, Cursor: first.Next})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 1 || second.Next != "" {
		t.Fatalf("second page = %d items next=%q; want the final item", len(second.Items), second.Next)
	}
	seen := make(map[id.ExecutionID]bool, 3)
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ExecutionID] {
			t.Fatalf("execution %q repeated across pages", item.ExecutionID)
		}
		seen[item.ExecutionID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("pages covered %d executions; want 3", len(seen))
	}
}

func TestReadListFiltersByStateAndScope(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-2", "start-2", containerAction())); err != nil {
		t.Fatalf("Start exec-2: %v", err)
	}
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", commit, "run-1")); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	running, err := adapter.List(context.Background(), scope, execution.ListFilter{State: kernel.ExecutionRunning, Limit: 10})
	if err != nil {
		t.Fatalf("List running: %v", err)
	}
	if len(running.Items) != 1 || running.Items[0].ExecutionID != "exec-1" {
		t.Fatalf("running page = %+v; want only exec-1", running.Items)
	}
	queued, err := adapter.List(context.Background(), scope, execution.ListFilter{State: kernel.ExecutionQueued, Limit: 10})
	if err != nil {
		t.Fatalf("List queued: %v", err)
	}
	if len(queued.Items) != 1 || queued.Items[0].ExecutionID != "exec-2" {
		t.Fatalf("queued page = %+v; want only exec-2", queued.Items)
	}
	other, err := adapter.List(context.Background(), otherScope(), execution.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List other scope: %v", err)
	}
	if len(other.Items) != 0 {
		t.Fatalf("other scope saw %d executions; want 0", len(other.Items))
	}
}

func TestReadLoadAndInspectRefuseUnknownOrForeignExecution(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-1", "start-1", containerAction())); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := adapter.Load(context.Background(), scope, "exec-missing")
	requireClass(t, err, fault.NotFound)
	_, err = adapter.Inspect(context.Background(), scope, "exec-missing")
	requireClass(t, err, fault.NotFound)
	_, err = adapter.Inspect(context.Background(), otherScope(), "exec-1")
	requireClass(t, err, fault.NotFound)
	_, err = adapter.Load(context.Background(), tenancy.Scope{TenantID: "tenant-1"}, "exec-1")
	requireClass(t, err, fault.InvalidInput)
}

func TestReadListRefusesInvalidLimit(t *testing.T) {
	adapter, _ := newStore(t)
	for _, limit := range []int{-1, 1000} {
		_, err := adapter.List(context.Background(), projectScope(), execution.ListFilter{Limit: limit})
		requireClass(t, err, fault.InvalidInput)
	}
}

// TestReadInspectExposesRequestSubjectAndAssignment proves execution.inspect and
// execution.list expose the immutable request facts a responder needs: the
// resolved typed subject and the exact assignment frozen at activation. A
// container action carries neither.
func TestReadInspectExposesRequestSubjectAndAssignment(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-request", containerAction(), requestAction())
	if _, err := adapter.Activate(context.Background(), activationUnit(scope, "exec-request", actions[1], "activate-request")); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	inspected, err := adapter.Inspect(context.Background(), scope, "exec-request")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(inspected.Actions) != 2 {
		t.Fatalf("inspect actions = %d; want 2", len(inspected.Actions))
	}
	requestView := inspected.Actions[1]
	if requestView.Subject == nil {
		t.Fatal("inspect request action carries no immutable subject")
	}
	fields := requestView.Subject.Fields()
	if value := fields["artifact"]; value.Kind != definition.SubjectLiteral || value.Literal != "artifact-1" {
		t.Fatalf("inspect request subject artifact = %+v; want the resolved literal artifact-1", value)
	}
	wantAssignment := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}
	if requestView.Assignment != wantAssignment {
		t.Fatalf("inspect request assignment = %+v; want %+v", requestView.Assignment, wantAssignment)
	}
	if container := inspected.Actions[0]; container.Subject != nil || container.Assignment != (request.Assignment{}) {
		t.Fatalf("container action carries request facts: subject=%v assignment=%+v", container.Subject, container.Assignment)
	}

	// The list projection is the same credential-free action view.
	page, err := adapter.List(context.Background(), scope, execution.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 || len(page.Items[0].Actions) != 2 {
		t.Fatalf("list page = %+v; want one execution with two actions", page.Items)
	}
	listed := page.Items[0].Actions[1]
	if listed.Subject == nil || listed.Subject.Fields()["artifact"].Literal != "artifact-1" ||
		listed.Assignment != wantAssignment {
		t.Fatalf("list request action = %+v; want the same subject and assignment as inspect", listed)
	}
}

// TestReadInspectAndListFailClosedOnCorruptRequestAssignment proves the stored
// request assignment is validated when the execution view is projected. A row
// whose assignment no longer satisfies the declared grammar is corruption: the
// read fails with internal_failure instead of exposing an empty or partial
// assignment, on both execution.inspect and execution.list.
func TestReadInspectAndListFailClosedOnCorruptRequestAssignment(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-request", containerAction(), requestAction())
	if _, err := adapter.Activate(context.Background(), activationUnit(scope, "exec-request", actions[1], "activate-request")); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := adapter.Inspect(context.Background(), scope, "exec-request"); err != nil {
		t.Fatalf("unmodified Inspect: %v", err)
	}

	// A decodable but invalid assignment (an unknown role) must fail closed.
	stored, ok := backend.Snapshot(scope, tableActions, string(actions[1]))
	if !ok {
		t.Fatal("activated request action row is missing")
	}
	version, err := rowUint64(stored.Values, "version")
	if err != nil {
		t.Fatalf("read action version: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         tableActions,
		ID:            string(actions[1]),
		ExpectVersion: version,
		Values:        map[string]any{"request_assignment": []byte(`{"kind":"role","role":"not-a-role"}`)},
	}}}); err != nil {
		t.Fatalf("corrupt request assignment: %v", err)
	}
	_, err = adapter.Inspect(context.Background(), scope, "exec-request")
	requireClass(t, err, fault.InternalFailure)
	_, err = adapter.List(context.Background(), scope, execution.ListFilter{Limit: 10})
	requireClass(t, err, fault.InternalFailure)
}
