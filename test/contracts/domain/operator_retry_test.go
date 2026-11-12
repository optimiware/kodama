package domaincontract

// The conditional operator retry grant on task.start.
//
// KODAMA.md "Conversation handoffs and fresh executions" says an authorized
// operator can start a fresh attempt on a failed open task. The registry
// grants the operator role task.start ONLY when the task is open and its
// CURRENT execution is failed. The condition is the
// failed_task_operator_start authority check, re-read atomically with the
// start commit, exactly like the conditional push_service_admin check:
//
//   - the grant stage admits every listed human role, including operator;
//   - the commit-time check is the authority;
//   - an engineer is unaffected (the condition is not evaluated for an
//     engineer grant);
//   - an operator gets no pipeline selection beyond the task's existing
//     pipeline;
//   - reviewer/viewer/admin without engineer or operator are refused;
//   - there is no unrestricted operator start and no separate retry operation.

import (
	"testing"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestTaskStartOperatorRegistryRow pins the grant set and the
// conditional check. The operator is a human-only grant: service roles stay
// engineer-only because a service is never an operator retry decision.
func TestTaskStartOperatorRegistryRow(t *testing.T) {
	op, ok := authorize.LookupOperation("task.start")
	if !ok {
		t.Fatal("task.start is not registered")
	}
	wantHuman := []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}
	if !sameRolesInOrder(op.Requires.HumanRoles, wantHuman) {
		t.Errorf("task.start human roles = %v; want %v", op.Requires.HumanRoles, wantHuman)
	}
	wantService := []tenancy.Role{tenancy.RoleEngineer}
	if !sameRolesInOrder(op.Requires.ServiceRoles, wantService) {
		t.Errorf("task.start service roles = %v; want %v", op.Requires.ServiceRoles, wantService)
	}
	if !hasCheck(op.Authority, authorize.CheckFailedTaskOperatorStart) {
		t.Error("task.start does not require the conditional failed_task_operator_start check")
	}
	if !hasCheck(op.Authority, authorize.CheckTaskVersion) ||
		!hasCheck(op.Authority, authorize.CheckPipelineVersion) ||
		!hasCheck(op.Authority, authorize.CheckMembership) {
		t.Error("task.start dropped a required version/membership authority read")
	}
	if op.Receipt != authorize.ReceiptRequired {
		t.Errorf("task.start receipt = %q; want required", op.Receipt)
	}
	if !authorize.CheckFailedTaskOperatorStart.Valid() {
		t.Error("failed_task_operator_start is not a declared authority check")
	}
}

// TestOperatorGetsNoPipelineSelection proves the retry grant adds no
// selection privilege: task.select_pipeline stays engineer-only and
// task.create_and_start stays engineer-only.
func TestOperatorGetsNoPipelineSelection(t *testing.T) {
	for _, name := range []authorize.OperationName{"task.select_pipeline", "task.create_and_start"} {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !sameRolesInOrder(op.Requires.HumanRoles, []tenancy.Role{tenancy.RoleEngineer}) {
			t.Errorf("%s human roles = %v; want engineer only", name, op.Requires.HumanRoles)
		}
		if hasCheck(op.Authority, authorize.CheckFailedTaskOperatorStart) {
			t.Errorf("%s carries the operator retry check; only task.start may", name)
		}
	}
}

// TestTaskStartGrantMatrix pins the grant matrix across roles, task states and
// current-execution states. It is the test-side oracle for the rule:
// the operator grant holds only for open+failed; an engineer is unaffected; no
// other role is granted.
func TestTaskStartGrantMatrix(t *testing.T) {
	executionStates := []executionCase{
		{name: "no execution"},
		{name: "failed execution", state: kernel.ExecutionFailed, present: true},
		{name: "running execution", state: kernel.ExecutionRunning, present: true},
		{name: "succeeded execution", state: kernel.ExecutionSucceeded, present: true},
		{name: "cancelled execution", state: kernel.ExecutionCancelled, present: true},
	}
	taskStates := []task.State{task.StateOpen, task.StateWaiting, task.StateDone, task.StateCancelled}
	roleSets := []struct {
		name  string
		roles []tenancy.Role
	}{
		{name: "operator only", roles: []tenancy.Role{tenancy.RoleOperator}},
		{name: "engineer only", roles: []tenancy.Role{tenancy.RoleEngineer}},
		{name: "engineer and operator", roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}},
		{name: "admin only", roles: []tenancy.Role{tenancy.RoleAdmin}},
		{name: "admin and operator", roles: []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleOperator}},
		{name: "reviewer only", roles: []tenancy.Role{tenancy.RoleReviewer}},
		{name: "viewer only", roles: []tenancy.Role{tenancy.RoleViewer}},
		{name: "no roles", roles: nil},
	}

	for _, roles := range roleSets {
		for _, state := range taskStates {
			for _, execution := range executionStates {
				got := startGrantAllows(roles.roles, state, execution)
				want := wantStartGrant(roles.roles, state, execution)
				if got != want {
					t.Errorf("%s / task %q / %s: grant = %v; want %v",
						roles.name, state, execution.name, got, want)
				}
			}
		}
	}

	// The exact boundary cases, stated directly so a regression cannot
	// hide behind the table loop.
	operator := []tenancy.Role{tenancy.RoleOperator}
	if !startGrantAllows(operator, task.StateOpen, executionCase{state: kernel.ExecutionFailed, present: true}) {
		t.Error("operator was refused an open task with a failed current execution")
	}
	for _, execution := range []executionCase{
		{name: "running", state: kernel.ExecutionRunning, present: true},
		{name: "none"},
	} {
		if startGrantAllows(operator, task.StateOpen, execution) {
			t.Errorf("operator was admitted to an open task with %s", execution.name)
		}
	}
	for _, state := range []task.State{task.StateWaiting, task.StateDone, task.StateCancelled} {
		if startGrantAllows(operator, state, executionCase{state: kernel.ExecutionFailed, present: true}) {
			t.Errorf("operator was admitted to a %q task", state)
		}
	}
	if startGrantAllows([]tenancy.Role{tenancy.RoleReviewer}, task.StateOpen, executionCase{state: kernel.ExecutionFailed, present: true}) {
		t.Error("reviewer was admitted to task.start")
	}
	if startGrantAllows([]tenancy.Role{tenancy.RoleViewer}, task.StateOpen, executionCase{state: kernel.ExecutionFailed, present: true}) {
		t.Error("viewer was admitted to task.start")
	}
	if !startGrantAllows([]tenancy.Role{tenancy.RoleEngineer}, task.StateOpen, executionCase{state: kernel.ExecutionFailed, present: true}) {
		t.Error("engineer lost the unchanged failed-open-task start")
	}
}

type executionCase struct {
	name    string
	state   kernel.ExecutionState
	present bool
}

// startGrantAllows is the test-side oracle for the grant. It reads the
// compiled registry rather than restating the grant set:
//
//   - the caller must hold a listed human role (operator is listed);
//   - a non-open task is refused for every role by the task-state rule;
//   - an engineer is unaffected and the condition is not evaluated for that
//     grant;
//   - an operator-only caller needs the conditional
//     failed_task_operator_start check, which holds only when the current
//     execution exists and is failed.
func startGrantAllows(roles []tenancy.Role, state task.State, execution executionCase) bool {
	op, ok := authorize.LookupOperation("task.start")
	if !ok {
		return false
	}
	granted := false
	for _, role := range roles {
		if hasRole(op.Requires.HumanRoles, role) {
			granted = true
			break
		}
	}
	if !granted {
		return false
	}
	if state != task.StateOpen {
		return false
	}
	if hasRole(roles, tenancy.RoleEngineer) {
		return true
	}
	if hasRole(roles, tenancy.RoleOperator) {
		if !hasCheck(op.Authority, authorize.CheckFailedTaskOperatorStart) {
			return false
		}
		return execution.present && execution.state == kernel.ExecutionFailed
	}
	return false
}

// wantStartGrant states the expected matrix directly, independently of the
// registry helper above.
func wantStartGrant(roles []tenancy.Role, state task.State, execution executionCase) bool {
	if hasRole(roles, tenancy.RoleEngineer) {
		return state == task.StateOpen
	}
	if hasRole(roles, tenancy.RoleOperator) {
		return state == task.StateOpen && execution.present && execution.state == kernel.ExecutionFailed
	}
	return false
}

func sameRolesInOrder(got, want []tenancy.Role) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
