package task_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func readCommand(actor authorize.Principal) task.ReadCommand {
	return task.ReadCommand{
		Actor:  actor,
		Scope:  actor.Scope,
		TaskID: taskID,
	}
}

// principalFor turns one authenticated actor into the current principal row the
// service re-reads before it reads the task.
func principalFor(actor authorize.Principal) identity.Principal {
	return identity.Principal{
		ID:          actor.ID,
		Kind:        actor.Kind,
		Scope:       actor.Scope,
		Roles:       append([]tenancy.Role(nil), actor.Roles...),
		Enabled:     actor.Enabled,
		Version:     actor.Version,
		ExecutionID: actor.ExecutionID,
	}
}

// TestReadAllowsRegistryRoles proves task.read is a pure authorized read for
// every human and service role the declared registry grants.
func TestReadAllowsRegistryRoles(t *testing.T) {
	cases := []struct {
		name string
		role tenancy.Role
		kind identity.PrincipalKind
		id   id.PrincipalID
	}{
		{name: "human engineer", role: tenancy.RoleEngineer, kind: identity.KindHuman, id: engineerPrincipal},
		{name: "human viewer", role: tenancy.RoleViewer, kind: identity.KindHuman, id: viewerPrincipal},
		{name: "service engineer", role: tenancy.RoleEngineer, kind: identity.KindService, id: serviceAPrincipal},
		{name: "service viewer", role: tenancy.RoleViewer, kind: identity.KindService, id: serviceBPrincipal},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			var actor authorize.Principal
			if testCase.kind == identity.KindHuman {
				actor = humanActor(testCase.id, testCase.role)
			} else {
				actor = serviceActor(testCase.id, testCase.role)
			}
			env.principals.put(principalFor(actor))
			env.reader.put(baseTask())

			result, err := env.service.Read(t.Context(), readCommand(actor))
			requireNoError(t, err, "Read")
			if !reflect.DeepEqual(result, baseTask()) {
				t.Fatalf("task = %+v; want %+v", result, baseTask())
			}
			calls := env.authorizer.captured()
			if len(calls) != 1 || calls[0].Operation != "task.read" || calls[0].Scope != projectScope() {
				t.Fatalf("authorizer calls = %+v; want one task.read", calls)
			}
			requireAuthority(t, env.authority.lastReads(), "task.read", authorize.CheckMembership)
			if env.reader.getCallCount() != 1 {
				t.Fatalf("task reads = %d; want one", env.reader.getCallCount())
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestReadRefusesUngrantedOrStalePrincipals proves a denied role, a disabled
// principal, a missing current membership and a mismatched actor scope all
// refuse Forbidden before the task reader is called.
func TestReadRefusesUngrantedOrStalePrincipals(t *testing.T) {
	t.Run("human reviewer", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a denied role", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("human operator", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a denied role", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("service reviewer", func(t *testing.T) {
		env := newTestEnv(t)
		actor := serviceActor(serviceAPrincipal, tenancy.RoleReviewer)
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a denied role", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("disabled principal", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		disabled := principalFor(actor)
		disabled.Enabled = false
		env.principals.put(disabled)
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a disabled principal", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("authorizer denies", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
			return authorize.DecisionDeny, nil
		}
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a denied authorizer", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("actor scope mismatch", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		actor.Scope = otherScope()
		env.reader.put(baseTask())
		command := readCommand(actor)
		command.Scope = projectScope()

		_, err := env.service.Read(t.Context(), command)
		requireFaultClass(t, err, fault.Forbidden)
		if env.authorizer.callCount() != 0 {
			t.Fatalf("authorizer calls = %d; want none before a scope refusal", env.authorizer.callCount())
		}
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none for a scope mismatch", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})

	t.Run("membership removed", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			return allowFacts(reads, func(read authorize.AuthorityRead) bool {
				return read.Check == authorize.CheckMembership
			}), nil
		}
		env.reader.put(baseTask())

		_, err := env.service.Read(t.Context(), readCommand(actor))
		requireFaultClass(t, err, fault.Forbidden)
		if env.reader.getCallCount() != 0 {
			t.Fatalf("task reads = %d; want none after membership removal", env.reader.getCallCount())
		}
		requireNoPortCalls(t, env.ports)
	})
}

// TestReadRefusesCrossScopeTask proves a task outside the command scope is
// reported not_found instead of being returned.
func TestReadRefusesCrossScopeTask(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	foreign := baseTask()
	foreign.Scope = otherScope()
	env.reader.get = func(context.Context, tenancy.Scope, id.TaskID) (task.Task, error) {
		return foreign, nil
	}

	_, err := env.service.Read(t.Context(), readCommand(actor))
	requireFaultClass(t, err, fault.NotFound)
	if env.reader.getCallCount() != 1 {
		t.Fatalf("task reads = %d; want one binding read", env.reader.getCallCount())
	}
	requireNoPortCalls(t, env.ports)
}

// TestReadPassesNotFoundThrough proves a missing task surfaces the reader's
// not_found fault unchanged.
func TestReadPassesNotFoundThrough(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Read(t.Context(), readCommand(actor))
	requireFaultClass(t, err, fault.NotFound)
	if env.reader.getCallCount() != 1 {
		t.Fatalf("task reads = %d; want one", env.reader.getCallCount())
	}
	requireNoPortCalls(t, env.ports)
}

// TestExecutionActorReadsOwnCurrentTask proves an execution principal may read
// the task it is currently running: the first read is the own-task binding
// read and the returned task is the second, authoritative read.
func TestExecutionActorReadsOwnCurrentTask(t *testing.T) {
	env := newTestEnv(t)
	actor := registerExecutionPrincipal(env, executionOne)
	env.reader.put(baseTask())

	result, err := env.service.Read(t.Context(), readCommand(actor))
	requireNoError(t, err, "Read by own execution")
	if !reflect.DeepEqual(result, baseTask()) {
		t.Fatalf("task = %+v; want %+v", result, baseTask())
	}
	if env.reader.getCallCount() != 2 {
		t.Fatalf("task reads = %d; want binding read plus result read", env.reader.getCallCount())
	}
	calls := env.authorizer.captured()
	if len(calls) != 0 {
		t.Fatalf("authorizer calls = %+v; want none: an execution grant uses AuthorizeExecution", calls)
	}
	requireAuthority(t, env.authority.lastReads(), "task.read", authorize.CheckMembership)
	requireNoPortCalls(t, env.ports)
}

// TestExecutionActorReadRefusesForeignOrStaleTask proves an execution
// principal bound to another task's execution, a previous execution of this
// task or a task in another scope is refused before any read beyond the
// binding read.
func TestExecutionActorReadRefusesForeignOrStaleTask(t *testing.T) {
	stale := baseTask()
	stale.ExecutionHistory = []id.ExecutionID{executionOne, executionTwo}
	stale.CurrentExecution = executionOne

	otherScopeTask := baseTask()
	otherScopeTask.Scope = otherScope()
	otherScopeTask.ExecutionHistory = []id.ExecutionID{executionTwo}
	otherScopeTask.CurrentExecution = executionTwo

	scenarios := []struct {
		name      string
		execution id.ExecutionID
		value     task.Task
		class     fault.Class
	}{
		{name: "another task's current execution", execution: executionTwo, value: baseTask(), class: fault.Forbidden},
		{name: "stale previous execution", execution: executionTwo, value: stale, class: fault.Forbidden},
		{name: "task in another scope", execution: executionTwo, value: otherScopeTask, class: fault.NotFound},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			env := newTestEnv(t)
			allowEveryActor(env)
			actor := registerExecutionPrincipal(env, scenario.execution)
			env.reader.put(scenario.value)

			_, err := env.service.Read(t.Context(), readCommand(actor))
			requireFaultClass(t, err, scenario.class)
			if env.reader.getCallCount() != 1 {
				t.Fatalf("task reads = %d; want only the binding read", env.reader.getCallCount())
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestReadResultCarriesNoCredentials proves the read result is the task row
// alone and refusal messages never echo credential material.
func TestReadResultCarriesNoCredentials(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(baseTask())

	result, err := env.service.Read(t.Context(), readCommand(actor))
	requireNoError(t, err, "Read")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	assertNoCredentialMaterial(t, string(encoded))

	denied := newTestEnv(t)
	_, err = denied.service.Read(t.Context(), readCommand(humanActor(reviewerPrincipal, tenancy.RoleReviewer)))
	requireFaultClass(t, err, fault.Forbidden)
	assertNoCredentialMaterial(t, err.Error())
}

func assertNoCredentialMaterial(t *testing.T, value string) {
	t.Helper()
	lower := strings.ToLower(value)
	for _, marker := range []string{"credential", "token", "secret"} {
		if strings.Contains(lower, marker) {
			t.Fatalf("value %q contains credential marker %q", value, marker)
		}
	}
}
