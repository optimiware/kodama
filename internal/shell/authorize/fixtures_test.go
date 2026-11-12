package authorize_test

import (
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Implementation surface used by these tests:
//
//	authorize.NewMatrix() authorize.Authorizer
//	authorize.AuthorizeExecution(authorize.AdmittedExecution, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
//	authorize.Confirm([]authorize.AuthorityRead, []authorize.AuthorityFact) error

const (
	testTenant    = id.TenantID("tenant-a")
	testProject   = id.ProjectID("project-a")
	otherTenant   = id.TenantID("tenant-b")
	otherProject  = id.ProjectID("project-b")
	testPrincipal = id.PrincipalID("principal-1")
	testService   = id.PrincipalID("svc-git")
	testExecution = id.ExecutionID("exec-1")
	testTask      = "task-1"
	testAction    = id.ActionID("action-1")
	testBinding   = "binding-1"
	testVersion   = uint64(7)
)

var testDeadline = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant}
}

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherTenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: otherTenant, ProjectID: otherProject}
}

func otherProjectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: otherProject}
}

// scopeFor returns the natural scope shape of one registry row.
func scopeFor(operation authorize.Operation) tenancy.Scope {
	if operation.Scope == authorize.ScopeTenant {
		return tenantScope()
	}
	return projectScope()
}

func humanPrincipal(role tenancy.Role, scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{role},
		Enabled: true,
		Version: 3,
	}
}

func humanPrincipalWithRoles(roles []tenancy.Role, scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 3,
	}
}

func servicePrincipal(roles []tenancy.Role, scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      testService,
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   append([]tenancy.Role(nil), roles...),
		Enabled: true,
		Version: 2,
	}
}

func executionPrincipal(scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:          testPrincipal,
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     4,
		ExecutionID: testExecution,
	}
}

func admittedExecution(permission authorize.ExecutionPermission, scope tenancy.Scope) authorize.AdmittedExecution {
	return authorize.AdmittedExecution{
		Principal:   executionPrincipal(scope),
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  permission,
		Deadline:    testDeadline,
	}
}

func containsRole(roles []tenancy.Role, want tenancy.Role) bool {
	return slices.Contains(roles, want)
}

func operationNamed(t *testing.T, name authorize.OperationName) authorize.Operation {
	t.Helper()
	operation, ok := authorize.LookupOperation(name)
	if !ok {
		t.Fatalf("operation %q is not registered", name)
	}
	return operation
}

// wantFaultClass asserts that err is a fault whose class is one of classes.
func wantFaultClass(t *testing.T, err error, classes ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal error, got nil")
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no declared fault class", err)
	}
	if slices.Contains(classes, class) {
		return
	}
	t.Fatalf("fault class = %q, want one of %v", class, classes)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
