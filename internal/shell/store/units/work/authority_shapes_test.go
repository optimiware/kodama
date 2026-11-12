package work

// The authority mapper is the in-transaction guard of every work port. These
// tests pin the exact set of current-authority checks the real task service,
// inbox, pipeline management and trigger processor emit, and prove every shape
// maps. A new check on one of those operations, or an unmappable shape, fails
// the build instead of silently refusing a legitimate service commit.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// serviceEmittedChecks is the complete set of current-authority checks the
// services wired to these adapters emit, including the checks the trigger
// processor adds outside the registry's per-operation list.
func serviceEmittedChecks() []authorize.AuthorityCheck {
	return []authorize.AuthorityCheck{
		authorize.CheckMembership,
		authorize.CheckPrincipalEnabled,
		authorize.CheckServiceEnabled,
		authorize.CheckTaskVersion,
		authorize.CheckExecutionVersion,
		authorize.CheckPipelineVersion,
		authorize.CheckBindingVersion,
		authorize.CheckPushServiceAdmin,
		authorize.CheckFailedTaskOperatorStart,
	}
}

// serviceOperations are the registry operations the task service, inbox,
// pipeline management and trigger processor commit under.
func serviceOperations() []authorize.OperationName {
	return []authorize.OperationName{
		"task.create",
		"task.create_and_start",
		"task.edit",
		"task.comment",
		"task.select_pipeline",
		"task.start",
		"task.ask",
		"task.reply",
		"task.cancel",
		"task.inbox",
		"pipeline.apply",
		"pipeline.update",
		"pipeline.delete",
		"pipeline.start",
		"event.read",
	}
}

// seedAuthorityShapes seeds one row of every kind the versioned reads name.
func seedAuthorityShapes(t *testing.T) *fixture {
	t.Helper()
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedMembership(t, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer, tenancy.RoleAdmin)
	fixture.seedServiceWithRoles(t, scope, "service-1", serviceStateEnabled, 1, tenancy.RoleEngineer)
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("v1"), 1)
	fixture.seedBinding(t, scope, "binding-1", "repo-1", "service-1", 1)
	fixture.createTask(t, scope, testTask)
	// The operator retry read names the task's current execution, so the
	// shared fixture points the task at one failed execution.
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     string(testTask),
			Expect: map[string]any{"lifecycle_version": int64(1)},
			Values: map[string]any{"current_execution_id": string(testExecution)},
		}},
	}); err != nil {
		t.Fatalf("point task at execution: %v", err)
	}
	// The execution row belongs to a distinct principal so the human
	// membership reads are not mistaken for an execution principal.
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "executions",
			ID:     string(testExecution),
			Insert: true,
			Values: map[string]any{
				"pipeline_id":  testPipeline,
				"task_id":      string(testTask),
				"principal_id": "execution-principal-1",
				"state":        string(kernel.ExecutionFailed),
				"version":      int64(1),
				"created_at":   testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	return fixture
}

// TestAuthorityLocksMapEveryServiceEmittedCheck enumerates the checks the
// services emit and asserts each one maps, including the implicit-target and
// zero-version shapes the real services build.
func TestAuthorityLocksMapEveryServiceEmittedCheck(t *testing.T) {
	fixture := seedAuthorityShapes(t)
	scope := testScope()
	required := engineerRequires()

	read := func(check authorize.AuthorityCheck, principal id.PrincipalID, target string, expected uint64) authorize.AuthorityRead {
		return authorize.AuthorityRead{
			Operation:       "task.start",
			Required:        required,
			Check:           check,
			Scope:           scope,
			PrincipalID:     principal,
			TargetID:        target,
			ExpectedVersion: expected,
		}
	}

	cases := []struct {
		name   string
		read   authorize.AuthorityRead
		target authorityContext
	}{
		{
			name: "membership",
			read: read(authorize.CheckMembership, testPrincipal, "", 0),
		},
		{
			name: "principal_enabled",
			read: read(authorize.CheckPrincipalEnabled, testPrincipal, "", 0),
		},
		{
			name: "service_enabled acting service",
			read: read(authorize.CheckServiceEnabled, "service-1", "", 0),
		},
		{
			name:   "task_version implicit zero (content/comment edit)",
			read:   read(authorize.CheckTaskVersion, testPrincipal, "", 0),
			target: authorityContext{TaskID: testTask},
		},
		{
			name:   "task_version implicit pinned (start/ask/reply/cancel/select)",
			read:   read(authorize.CheckTaskVersion, testPrincipal, "", 1),
			target: authorityContext{TaskID: testTask},
		},
		{
			name: "execution_version",
			read: read(authorize.CheckExecutionVersion, testPrincipal, testExecution, 1),
		},
		{
			name: "pipeline_version existing",
			read: read(authorize.CheckPipelineVersion, testPrincipal, testPipeline, 1),
		},
		{
			name:   "pipeline_version new apply",
			read:   read(authorize.CheckPipelineVersion, testPrincipal, "", 0),
			target: authorityContext{NewPipeline: true},
		},
		{
			name: "binding_version",
			read: read(authorize.CheckBindingVersion, testPrincipal, "binding-1", 1),
		},
		{
			name: "push_service_admin",
			read: read(authorize.CheckPushServiceAdmin, testPrincipal, "", 0),
		},
		{
			name:   "failed_task_operator_start",
			read:   read(authorize.CheckFailedTaskOperatorStart, testPrincipal, testExecution, 1),
			target: authorityContext{TaskID: testTask},
		},
	}
	covered := make(map[authorize.AuthorityCheck]bool, len(cases))
	for _, test := range cases {
		covered[test.read.Check] = true
		if _, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{test.read}, test.target); err != nil {
			t.Fatalf("%s: authority read does not map: %v", test.name, err)
		}
	}

	emitted := make(map[authorize.AuthorityCheck]bool, len(serviceEmittedChecks()))
	for _, check := range serviceEmittedChecks() {
		emitted[check] = true
		if !covered[check] {
			t.Fatalf("emitted check %q has no mapping case", check)
		}
	}
	for _, operation := range serviceOperations() {
		registered, ok := authorize.LookupOperation(operation)
		if !ok {
			t.Fatalf("service operation %q is not declared", operation)
		}
		for _, check := range registered.Authority {
			if !emitted[check] {
				t.Fatalf("operation %q declares check %q that the work adapters do not map", operation, check)
			}
		}
	}
}

// TestAuthorityLocksRefuseUnmappableShapes keeps the mapper strict: a shape no
// service emits is still invalid_input, never a silent skip.
func TestAuthorityLocksRefuseUnmappableShapes(t *testing.T) {
	fixture := seedAuthorityShapes(t)
	scope := testScope()
	base := authorize.AuthorityRead{
		Operation:   "task.start",
		Required:    engineerRequires(),
		Scope:       scope,
		PrincipalID: testPrincipal,
	}
	cases := []struct {
		name   string
		read   authorize.AuthorityRead
		target authorityContext
	}{
		{
			name: "task version with no target anywhere",
			read: authorize.AuthorityRead{Check: authorize.CheckTaskVersion, ExpectedVersion: 1},
		},
		{
			name:   "pipeline version with no target and an expected version",
			read:   authorize.AuthorityRead{Check: authorize.CheckPipelineVersion, ExpectedVersion: 1},
			target: authorityContext{NewPipeline: true},
		},
		{
			name: "pipeline version with no target outside a new apply",
			read: authorize.AuthorityRead{Check: authorize.CheckPipelineVersion},
		},
		{
			name: "unknown check",
			read: authorize.AuthorityRead{Check: authorize.AuthorityCheck("not_a_check")},
		},
	}
	for _, test := range cases {
		read := test.read
		read.Operation = base.Operation
		read.Required = base.Required
		read.Scope = base.Scope
		read.PrincipalID = base.PrincipalID
		_, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, test.target)
		mustClass(t, err, fault.InvalidInput)
	}
}
