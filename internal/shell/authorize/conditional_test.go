package authorize_test

// Conditional-grant vectors for the operator task.start retry.
//
// The grant stage admits the operator role on task.start, but the operator
// grant is conditional: the caller's grant is not complete until the
// failed_task_operator_start check has been read at the commit boundary. The
// matrix still answers the grant question ("allow"), so the commit path must
// ask ConditionalGrantChecks and carry every returned check. An operator-only
// principal therefore never holds an unconditional task.start grant.

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestConditionalGrantChecksOperatorRetry(t *testing.T) {
	start := operationNamed(t, "task.start")
	engineer := humanPrincipal(tenancy.RoleEngineer, projectScope())
	operator := humanPrincipal(tenancy.RoleOperator, projectScope())
	both := humanPrincipalWithRoles([]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, projectScope())
	reviewer := humanPrincipal(tenancy.RoleReviewer, projectScope())
	viewer := humanPrincipal(tenancy.RoleViewer, projectScope())
	admin := humanPrincipal(tenancy.RoleAdmin, projectScope())
	service := servicePrincipal([]tenancy.Role{tenancy.RoleEngineer}, projectScope())
	disabledOperator := humanPrincipal(tenancy.RoleOperator, projectScope())
	disabledOperator.Enabled = false

	cases := []struct {
		name      string
		principal authorize.Principal
		want      []authorize.AuthorityCheck
	}{
		{name: "operator only", principal: operator, want: []authorize.AuthorityCheck{authorize.CheckFailedTaskOperatorStart}},
		{name: "engineer only", principal: engineer},
		{name: "engineer and operator", principal: both},
		{name: "reviewer", principal: reviewer},
		{name: "viewer", principal: viewer},
		{name: "admin", principal: admin},
		{name: "service engineer", principal: service},
		{name: "disabled operator", principal: disabledOperator},
	}
	for _, testCase := range cases {
		got := authorize.ConditionalGrantChecks(start, testCase.principal)
		if !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("%s conditional checks = %v; want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestOperatorTaskStartIsAllowIfCheck proves the matrix grant for an
// operator-only principal is never sufficient on its own: the grant is
// returned as allow while ConditionalGrantChecks names the check that must be
// carried into the commit unit.
func TestOperatorTaskStartIsAllowIfCheck(t *testing.T) {
	start := operationNamed(t, "task.start")
	matrix := authorize.NewMatrix()
	operator := humanPrincipal(tenancy.RoleOperator, projectScope())

	decision, err := matrix.Authorize(context.Background(), operator, "task.start", projectScope())
	requireNoError(t, err)
	if decision != authorize.DecisionAllow {
		t.Fatalf("operator task.start grant = %q; want the conditional allow %q", decision, authorize.DecisionAllow)
	}
	checks := authorize.ConditionalGrantChecks(start, operator)
	if !reflect.DeepEqual(checks, []authorize.AuthorityCheck{authorize.CheckFailedTaskOperatorStart}) {
		t.Fatalf("operator task.start conditional checks = %v; want the failed-task check", checks)
	}
	if !declaresCheck(start, authorize.CheckFailedTaskOperatorStart) {
		t.Fatal("task.start does not declare failed_task_operator_start")
	}

	engineer := humanPrincipal(tenancy.RoleEngineer, projectScope())
	decision, err = matrix.Authorize(context.Background(), engineer, "task.start", projectScope())
	requireNoError(t, err)
	if decision != authorize.DecisionAllow {
		t.Fatalf("engineer task.start grant = %q; want allow", decision)
	}
	if checks := authorize.ConditionalGrantChecks(start, engineer); len(checks) != 0 {
		t.Fatalf("engineer task.start conditional checks = %v; the condition is not evaluated for an engineer grant", checks)
	}

	reviewer := humanPrincipal(tenancy.RoleReviewer, projectScope())
	decision, err = matrix.Authorize(context.Background(), reviewer, "task.start", projectScope())
	requireNoError(t, err)
	if decision != authorize.DecisionDeny {
		t.Fatalf("reviewer task.start grant = %q; want deny", decision)
	}
	if checks := authorize.ConditionalGrantChecks(start, reviewer); len(checks) != 0 {
		t.Fatalf("reviewer task.start conditional checks = %v; a denied principal requires no conditional read", checks)
	}
}

// TestOnlyTaskStartDeclaresTheOperatorRetryCheck proves the conditional check
// is not accidentally attached to another operation.
func TestOnlyTaskStartDeclaresTheOperatorRetryCheck(t *testing.T) {
	for _, operation := range operationsWithCheck(t, authorize.CheckFailedTaskOperatorStart) {
		if operation != "task.start" {
			t.Errorf("operation %q declares failed_task_operator_start; only task.start may", operation)
		}
	}
	start := operationNamed(t, "task.start")
	if !declaresCheck(start, authorize.CheckFailedTaskOperatorStart) {
		t.Fatal("task.start dropped the failed_task_operator_start check")
	}
}

func declaresCheck(operation authorize.Operation, want authorize.AuthorityCheck) bool {
	return slices.Contains(operation.Authority, want)
}

// operationsWithCheck sweeps the declared registry for every operation that
// declares the check.
func operationsWithCheck(t *testing.T, want authorize.AuthorityCheck) []authorize.OperationName {
	t.Helper()
	var names []authorize.OperationName
	for _, operation := range authorize.Operations() {
		if declaresCheck(operation, want) {
			names = append(names, operation.Name)
		}
	}
	return names
}
