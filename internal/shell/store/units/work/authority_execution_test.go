package work

// An execution principal holds no membership row. Its current-authority
// membership fact is the registry's execution permission plus a running
// execution row in the read's scope, never a human or service grant. These
// tests pin that shape so the task.create/edit/comment/ask units the real task
// service builds for an execution principal are not refused at the commit
// boundary, while a non-running, out-of-scope or unpermitted execution still is.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const executionPrincipal = "execution-principal-1"

// executionMembershipRead builds one current-membership authority read for an
// execution principal.
func executionMembershipRead(scope tenancy.Scope, permission authorize.ExecutionPermission) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:   "task.edit",
		Required:    authorize.Requires{Execution: permission},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: executionPrincipal,
	}
}

// TestAuthorityMembershipAllowsRunningExecutionPrincipal proves the commit
// boundary accepts an execution principal's membership read when the registry
// declares an execution permission and the principal's execution row is running
// in scope. The unit's own execution_version/execution_running guards remain the
// only execution locks, so the membership read adds no lock of its own.
func TestAuthorityMembershipAllowsRunningExecutionPrincipal(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedExecutionForPrincipal(t, scope, testExecution, "", executionPrincipal, executionStateRunning, 1)

	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{executionMembershipRead(scope, authorize.ExecOwnTaskContent)},
		authorityContext{TaskID: testTask})
	if err != nil {
		t.Fatalf("a running execution principal's membership read was refused: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("an execution membership read added %d locks; want none", len(locks))
	}
}

// TestAuthorityMembershipRefusesExecutionWithoutPermission proves the registry's
// execution permission is part of the membership fact: an operation that
// declares no execution permission (or an unset/unknown one) refuses an
// execution principal even while its row is running.
func TestAuthorityMembershipRefusesExecutionWithoutPermission(t *testing.T) {
	for _, permission := range []authorize.ExecutionPermission{
		authorize.ExecNone,
		authorize.ExecutionPermission(""),
		authorize.ExecutionPermission("not_a_permission"),
	} {
		name := string(permission)
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			fixture.seedExecutionForPrincipal(t, scope, testExecution, "", executionPrincipal, executionStateRunning, 1)

			_, err := authorityLocks(context.Background(), fixture.store, fixture.store,
				[]authorize.AuthorityRead{executionMembershipRead(scope, permission)},
				authorityContext{TaskID: testTask})
			mustClass(t, err, fault.Forbidden)
		})
	}
}

// TestAuthorityMembershipRefusesNonRunningExecution proves a queued, succeeded,
// failed or cancelled execution row cannot satisfy the membership check: only a
// currently running execution grants its registry permission.
func TestAuthorityMembershipRefusesNonRunningExecution(t *testing.T) {
	for _, state := range []string{"queued", "succeeded", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			fixture.seedExecutionForPrincipal(t, scope, testExecution, "", executionPrincipal, state, 1)

			_, err := authorityLocks(context.Background(), fixture.store, fixture.store,
				[]authorize.AuthorityRead{executionMembershipRead(scope, authorize.ExecOwnTaskContent)},
				authorityContext{TaskID: testTask})
			mustClass(t, err, fault.Forbidden)
		})
	}
}

// TestAuthorityMembershipRefusesExecutionOutOfScope proves scope containment is
// part of the fact: an execution row in another project cannot authorize a
// membership read in this project.
func TestAuthorityMembershipRefusesExecutionOutOfScope(t *testing.T) {
	fixture := newFixture(t)
	fixture.seedExecutionForPrincipal(t, otherScope(), testExecution, "", executionPrincipal, executionStateRunning, 1)

	_, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{executionMembershipRead(testScope(), authorize.ExecOwnTaskContent)},
		authorityContext{TaskID: testTask})
	mustClass(t, err, fault.Forbidden)
}

// TestAuthorityMembershipRefusesTenantScopedExecutionPrincipal proves an
// execution principal is project-scoped: a tenant-scoped read never resolves an
// execution row and is refused rather than silently allowed.
func TestAuthorityMembershipRefusesTenantScopedExecutionPrincipal(t *testing.T) {
	fixture := newFixture(t)
	scope := tenancy.Scope{TenantID: testTenant}
	fixture.seedExecutionForPrincipal(t, testScope(), testExecution, "", executionPrincipal, executionStateRunning, 1)

	_, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{executionMembershipRead(scope, authorize.ExecOwnTaskContent)},
		authorityContext{})
	mustClass(t, err, fault.Forbidden)
}
