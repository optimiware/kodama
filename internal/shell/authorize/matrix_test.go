package authorize_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestHumanGrantMatrixAllowsExactlyTheDeclaredRoles walks the whole declared
// 51-operation registry and asserts that a verified human holding exactly one
// role is allowed exactly when that role appears in the operation's human grant
// set. Admin and Reviewer therefore never imply each other, and operations with
// no human grant admit no human.
func TestHumanGrantMatrixAllowsExactlyTheDeclaredRoles(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	operations := authorize.Operations()
	if len(operations) != 51 {
		t.Fatalf("registry has %d operations; want the declared 51", len(operations))
	}

	for _, operation := range operations {
		scope := scopeFor(operation)
		for _, role := range tenancy.Roles() {
			decision, err := matrix.Authorize(ctx, humanPrincipal(role, scope), operation.Name, scope)
			if err != nil {
				t.Fatalf("%s for human %s: unexpected error: %v", operation.Name, role, err)
			}
			if !decision.Valid() {
				t.Fatalf("%s for human %s: decision %q is not a declared decision", operation.Name, role, decision)
			}
			want := containsRole(operation.Requires.HumanRoles, role)
			if got := decision == authorize.DecisionAllow; got != want {
				t.Errorf("%s for human %s: allow = %v, want %v (declared roles %v)",
					operation.Name, role, got, want, operation.Requires.HumanRoles)
			}
		}
	}
}

// TestServiceGrantMatrixAllowsExactlyTheDeclaredServiceRoles asserts a service
// principal is allowed exactly when the operation lists one of its current
// service roles, and never for an Admin-only or execution-only operation.
func TestServiceGrantMatrixAllowsExactlyTheDeclaredServiceRoles(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	serviceRoles := tenancy.ServiceRoles()
	if len(serviceRoles) == 0 {
		t.Fatal("service role vocabulary is empty")
	}
	for _, operation := range authorize.Operations() {
		scope := scopeFor(operation)
		for _, role := range serviceRoles {
			decision, err := matrix.Authorize(ctx, servicePrincipal([]tenancy.Role{role}, scope), operation.Name, scope)
			if err != nil {
				t.Fatalf("%s for service %s: unexpected error: %v", operation.Name, role, err)
			}
			want := containsRole(operation.Requires.ServiceRoles, role)
			if got := decision == authorize.DecisionAllow; got != want {
				t.Errorf("%s for service %s: allow = %v, want %v (declared service roles %v)",
					operation.Name, role, got, want, operation.Requires.ServiceRoles)
			}
		}
	}
}

// TestServiceNeverReceivesAdmin asserts a service principal carrying the Admin
// role is structurally refused instead of being treated as an administrator,
// and that every Admin-only operation refuses every service role.
func TestServiceNeverReceivesAdmin(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	adminService := servicePrincipal([]tenancy.Role{tenancy.RoleAdmin}, projectScope())
	decision, err := matrix.Authorize(ctx, adminService, "service.create", projectScope())
	wantFaultClass(t, err, fault.InvalidInput, fault.Forbidden)
	if decision == authorize.DecisionAllow {
		t.Fatal("a service principal holding Admin was allowed to administer services")
	}

	for _, operation := range authorize.Operations() {
		if len(operation.Requires.ServiceRoles) != 0 {
			continue
		}
		scope := scopeFor(operation)
		for _, role := range tenancy.ServiceRoles() {
			decision, err := matrix.Authorize(ctx, servicePrincipal([]tenancy.Role{role}, scope), operation.Name, scope)
			if err != nil {
				t.Fatalf("%s for service %s: unexpected error: %v", operation.Name, role, err)
			}
			if decision != authorize.DecisionDeny {
				t.Errorf("%s admits service role %s although it declares no service grant", operation.Name, role)
			}
		}
	}
}

// TestExecutionGrantMatrixAllowsOnlyTheAdmittedPermission asserts an admitted
// execution is allowed exactly when the operation's declared execution
// permission equals the permission admitted with the execution principal.
// ExecNone never grants anything.
func TestExecutionGrantMatrixAllowsOnlyTheAdmittedPermission(t *testing.T) {
	permissions := authorize.ExecutionPermissions()
	if len(permissions) == 0 {
		t.Fatal("execution permission vocabulary is empty")
	}
	for _, operation := range authorize.Operations() {
		scope := scopeFor(operation)
		for _, permission := range permissions {
			decision, err := authorize.AuthorizeExecution(admittedExecution(permission, scope), operation.Name, scope)
			if err != nil {
				t.Fatalf("%s for execution permission %s: unexpected error: %v", operation.Name, permission, err)
			}
			want := permission != authorize.ExecNone && operation.Requires.Execution == permission
			if got := decision == authorize.DecisionAllow; got != want {
				t.Errorf("%s for execution permission %s: allow = %v, want %v (declared permission %s)",
					operation.Name, permission, got, want, operation.Requires.Execution)
			}
		}
	}
}

// TestExecutionAuthorityNeverCoversAdministrationResolutionOrReply pins the
// operations an execution must never reach even when it presents every
// admitted permission.
func TestExecutionAuthorityNeverCoversAdministrationResolutionOrReply(t *testing.T) {
	neverExecution := []authorize.OperationName{
		"tenant.create",
		"tenant.rotate_key",
		"membership.grant",
		"membership.revoke",
		"resource.binding.register",
		"pipeline.apply",
		"pipeline.delete",
		"secret.metadata.read",
		"service.credential.issue",
		"task.reply",
		"task.cancel",
		"execution.request.resolve",
	}
	for _, name := range neverExecution {
		operation := operationNamed(t, name)
		if operation.Requires.Execution != authorize.ExecNone {
			t.Fatalf("%s declares execution permission %s; the test premise is stale", name, operation.Requires.Execution)
		}
		scope := scopeFor(operation)
		for _, permission := range authorize.ExecutionPermissions() {
			decision, err := authorize.AuthorizeExecution(admittedExecution(permission, scope), name, scope)
			if err != nil {
				t.Fatalf("%s for execution permission %s: unexpected error: %v", name, permission, err)
			}
			if decision != authorize.DecisionDeny {
				t.Errorf("%s admitted execution permission %s although executions must never reach it", name, permission)
			}
		}
	}
}

// TestExecutionCannotSatisfyHumanOrServiceGrants asserts execution authority is
// a separate closed vocabulary: no admitted permission substitutes for a human
// or service grant, and a human holding every role cannot satisfy an
// execution-only operation.
func TestExecutionCannotSatisfyHumanOrServiceGrants(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	everyRole := tenancy.Roles()
	for _, name := range []authorize.OperationName{"task.edit", "event.publish", "model.invoke", "tool.invoke"} {
		operation := operationNamed(t, name)
		scope := scopeFor(operation)
		decision, err := matrix.Authorize(ctx, humanPrincipalWithRoles(everyRole, scope), name, scope)
		if err != nil {
			t.Fatalf("%s for a human holding every role: unexpected error: %v", name, err)
		}
		if decision != authorize.DecisionAllow {
			t.Errorf("%s denied a human holding the declared engineer role", name)
		}
	}

	outputs := operationNamed(t, "execution.action.outputs")
	if outputs.Requires.Execution != authorize.ExecOwnOutputs {
		t.Fatalf("execution.action.outputs declares %s; the test premise is stale", outputs.Requires.Execution)
	}
	scope := scopeFor(outputs)
	decision, err := matrix.Authorize(ctx, humanPrincipalWithRoles(everyRole, scope), outputs.Name, scope)
	if err != nil {
		t.Fatalf("execution.action.outputs for a human holding every role: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Error("a human holding every role satisfied an execution-only operation")
	}
	decision, err = matrix.Authorize(ctx, servicePrincipal(tenancy.ServiceRoles(), scope), outputs.Name, scope)
	if err != nil {
		t.Fatalf("execution.action.outputs for a service holding every service role: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Error("a service holding every service role satisfied an execution-only operation")
	}
}

// TestAdminAndReviewerRemainDistinct pins the explicit authority split.
func TestAdminAndReviewerRemainDistinct(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	cases := []struct {
		operation authorize.OperationName
		role      tenancy.Role
		want      bool
	}{
		{"membership.grant", tenancy.RoleAdmin, true},
		{"membership.grant", tenancy.RoleReviewer, false},
		{"membership.list", tenancy.RoleAdmin, true},
		{"membership.list", tenancy.RoleReviewer, false},
		{"pipeline.apply", tenancy.RoleAdmin, false},
		{"pipeline.apply", tenancy.RoleReviewer, false},
		{"pipeline.apply", tenancy.RoleEngineer, true},
		{"task.edit", tenancy.RoleAdmin, false},
		{"task.edit", tenancy.RoleReviewer, false},
		{"task.edit", tenancy.RoleEngineer, true},
		{"task.inbox", tenancy.RoleReviewer, true},
		{"task.inbox", tenancy.RoleAdmin, false},
		{"actor.verify", tenancy.RoleAdmin, false},
		{"actor.verify", tenancy.RoleReviewer, false},
	}
	for _, testCase := range cases {
		operation := operationNamed(t, testCase.operation)
		scope := scopeFor(operation)
		decision, err := matrix.Authorize(ctx, humanPrincipal(testCase.role, scope), testCase.operation, scope)
		if err != nil {
			t.Fatalf("%s for human %s: unexpected error: %v", testCase.operation, testCase.role, err)
		}
		if got := decision == authorize.DecisionAllow; got != testCase.want {
			t.Errorf("%s for human %s: allow = %v, want %v", testCase.operation, testCase.role, got, testCase.want)
		}
	}
}

// TestGrantStageAdmitsEveryRoleForAssignedOperations documents that the grant
// stage is not the assignment authority: task.reply and
// execution.request.resolve admit every human role and every non-Admin service
// role because the in-commit assignment check is the authority.
func TestGrantStageAdmitsEveryRoleForAssignedOperations(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	for _, name := range []authorize.OperationName{"task.reply", "execution.request.resolve"} {
		operation := operationNamed(t, name)
		scope := scopeFor(operation)
		for _, role := range tenancy.Roles() {
			decision, err := matrix.Authorize(ctx, humanPrincipal(role, scope), name, scope)
			if err != nil {
				t.Fatalf("%s for human %s: unexpected error: %v", name, role, err)
			}
			if decision != authorize.DecisionAllow {
				t.Errorf("%s denied human role %s at the grant stage", name, role)
			}
		}
		for _, role := range tenancy.ServiceRoles() {
			decision, err := matrix.Authorize(ctx, servicePrincipal([]tenancy.Role{role}, scope), name, scope)
			if err != nil {
				t.Fatalf("%s for service %s: unexpected error: %v", name, role, err)
			}
			if decision != authorize.DecisionAllow {
				t.Errorf("%s denied service role %s at the grant stage", name, role)
			}
		}
	}
}

// TestUnknownOperationsAreRefused asserts empty and unregistered names never
// authorize, including the separate resource-operation vocabulary.
func TestUnknownOperationsAreRefused(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	for _, name := range []authorize.OperationName{"", "task.delete", "git.ref.update", "git.ref.read", "TASK.READ", " actor.verify"} {
		scope := projectScope()
		decision, err := matrix.Authorize(ctx, humanPrincipal(tenancy.RoleAdmin, scope), name, scope)
		wantFaultClass(t, err, fault.InvalidInput, fault.Forbidden)
		if decision == authorize.DecisionAllow {
			t.Errorf("unknown operation %q was allowed", name)
		}

		decision, err = authorize.AuthorizeExecution(admittedExecution(authorize.ExecOwnTaskContent, scope), name, scope)
		wantFaultClass(t, err, fault.InvalidInput, fault.Forbidden)
		if decision == authorize.DecisionAllow {
			t.Errorf("unknown operation %q was allowed for an execution", name)
		}
	}
}

// TestMalformedPrincipalsAreRefused asserts a structurally invalid principal is
// an error, never a silent allow or deny.
func TestMalformedPrincipalsAreRefused(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()
	scope := projectScope()

	cases := []struct {
		name      string
		principal authorize.Principal
	}{
		{"empty id", authorize.Principal{Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true}},
		{"unknown role", authorize.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.Role("owner")}, Enabled: true}},
		{"unknown kind", authorize.Principal{ID: testPrincipal, Kind: identity.PrincipalKind("robot"), Scope: scope, Enabled: true}},
		{"human with execution id", authorize.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, ExecutionID: testExecution}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := matrix.Authorize(ctx, testCase.principal, "task.read", scope)
			wantFaultClass(t, err, fault.InvalidInput, fault.Forbidden)
			if decision == authorize.DecisionAllow {
				t.Fatal("malformed principal was allowed")
			}
		})
	}

	// An execution principal carries its permission separately, so the plain
	// authorizer cannot answer for it.
	decision, err := matrix.Authorize(ctx, executionPrincipal(scope), "task.read", scope)
	wantFaultClass(t, err, fault.InvalidInput, fault.Forbidden)
	if decision == authorize.DecisionAllow {
		t.Fatal("an execution principal was authorized without its admitted permission")
	}
}

// TestScopeContainment pins cross-scope refusal and the one-directional scope
// containment rule.
func TestScopeContainment(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	cases := []struct {
		name      string
		principal authorize.Principal
		operation authorize.OperationName
		scope     tenancy.Scope
		want      authorize.Decision
	}{
		{"same project", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", projectScope(), authorize.DecisionAllow},
		{"other project in same tenant", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", otherProjectScope(), authorize.DecisionDeny},
		{"other tenant", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", otherTenantScope(), authorize.DecisionDeny},
		{"tenant admin in own tenant", humanPrincipal(tenancy.RoleAdmin, tenantScope()), "project.create", tenantScope(), authorize.DecisionAllow},
		{"tenant admin in another tenant", humanPrincipal(tenancy.RoleAdmin, tenantScope()), "project.create", otherTenantScope(), authorize.DecisionDeny},
		{"tenant admin inside a project of its tenant", humanPrincipal(tenancy.RoleAdmin, tenantScope()), "resource.binding.read", projectScope(), authorize.DecisionAllow},
		{"project-scoped admin at tenant scope", humanPrincipal(tenancy.RoleAdmin, projectScope()), "tenant.create", tenantScope(), authorize.DecisionDeny},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := matrix.Authorize(ctx, testCase.principal, testCase.operation, testCase.scope)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if decision != testCase.want {
				t.Fatalf("decision = %q, want %q", decision, testCase.want)
			}
		})
	}

	// Execution scope containment follows the same rule.
	admitted := admittedExecution(authorize.ExecOwnTaskContent, projectScope())
	decision, err := authorize.AuthorizeExecution(admitted, "task.read", otherProjectScope())
	if err != nil {
		t.Fatalf("cross-project execution check: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Fatalf("execution in project %q was allowed in project %q", projectScope().ProjectID, otherProjectScope().ProjectID)
	}
}

// TestScopeShapeIsRefused asserts an operation is only evaluated against its
// declared scope shape, and an unusable scope is an invalid input.
func TestScopeShapeIsRefused(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()

	cases := []struct {
		name      string
		principal authorize.Principal
		operation authorize.OperationName
		scope     tenancy.Scope
	}{
		{"project operation with tenant scope", humanPrincipal(tenancy.RoleEngineer, tenantScope()), "task.read", tenantScope()},
		{"tenant operation with project scope", humanPrincipal(tenancy.RoleAdmin, projectScope()), "project.create", projectScope()},
		{"empty tenant", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", tenancy.Scope{ProjectID: testProject}},
		{"invalid project", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", tenancy.Scope{TenantID: testTenant, ProjectID: "bad/project"}},
		{"invalid tenant", humanPrincipal(tenancy.RoleEngineer, projectScope()), "task.read", tenancy.Scope{TenantID: "bad tenant"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := matrix.Authorize(ctx, testCase.principal, testCase.operation, testCase.scope)
			wantFaultClass(t, err, fault.InvalidInput)
			if decision == authorize.DecisionAllow {
				t.Fatal("a malformed scope was allowed")
			}
		})
	}
}

// TestDisabledPrincipalIsDenied asserts a disabled principal holds no current
// grant, without turning the answer into a transport error.
func TestDisabledPrincipalIsDenied(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()
	scope := projectScope()

	human := humanPrincipal(tenancy.RoleEngineer, scope)
	human.Enabled = false
	decision, err := matrix.Authorize(ctx, human, "task.edit", scope)
	if err != nil {
		t.Fatalf("disabled human: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Fatalf("disabled human decision = %q, want deny", decision)
	}

	service := servicePrincipal([]tenancy.Role{tenancy.RoleEngineer}, scope)
	service.Enabled = false
	decision, err = matrix.Authorize(ctx, service, "pipeline.read", scope)
	if err != nil {
		t.Fatalf("disabled service: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Fatalf("disabled service decision = %q, want deny", decision)
	}

	admitted := admittedExecution(authorize.ExecOwnTaskContent, scope)
	admitted.Principal.Enabled = false
	decision, err = authorize.AuthorizeExecution(admitted, "task.read", scope)
	if err != nil {
		t.Fatalf("disabled execution: unexpected error: %v", err)
	}
	if decision != authorize.DecisionDeny {
		t.Fatalf("disabled execution decision = %q, want deny", decision)
	}
}

// TestAdmittedExecutionMustMatchItsPrincipal asserts the admitted execution
// context is validated instead of being trusted.
func TestAdmittedExecutionMustMatchItsPrincipal(t *testing.T) {
	scope := projectScope()

	admitted := admittedExecution(authorize.ExecOwnTaskContent, scope)
	admitted.ExecutionID = "exec-2"
	decision, err := authorize.AuthorizeExecution(admitted, "task.read", scope)
	wantFaultClass(t, err, fault.InvalidInput)
	if decision == authorize.DecisionAllow {
		t.Fatal("an admitted execution with a mismatched execution id was allowed")
	}

	admitted = admittedExecution(authorize.ExecutionPermission("admin"), scope)
	decision, err = authorize.AuthorizeExecution(admitted, "task.read", scope)
	wantFaultClass(t, err, fault.InvalidInput)
	if decision == authorize.DecisionAllow {
		t.Fatal("an admitted execution with an unknown permission was allowed")
	}
}

// TestMatrixDoesNotCacheGrants asserts one authorizer instance answers from the
// principal presented on each call, so a previously allowed attempt cannot be
// replayed after the roles change.
func TestMatrixDoesNotCacheGrants(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()
	scope := projectScope()

	principal := humanPrincipal(tenancy.RoleEngineer, scope)
	decision, err := matrix.Authorize(ctx, principal, "task.edit", scope)
	requireNoError(t, err)
	if decision != authorize.DecisionAllow {
		t.Fatal("engineer was denied task.edit")
	}

	principal.Roles = []tenancy.Role{tenancy.RoleViewer}
	decision, err = matrix.Authorize(ctx, principal, "task.edit", scope)
	requireNoError(t, err)
	if decision != authorize.DecisionDeny {
		t.Fatal("a removed engineer role still allowed task.edit")
	}

	principal.Roles = nil
	decision, err = matrix.Authorize(ctx, principal, "task.edit", scope)
	requireNoError(t, err)
	if decision != authorize.DecisionDeny {
		t.Fatal("a principal with no roles still allowed task.edit")
	}
}
