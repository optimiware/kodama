package task_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	executionPrincipalID = id.PrincipalID("principal-execution")
	otherTaskID          = id.TaskID("task-2")
)

// registerExecutionPrincipal stores one enabled execution principal and returns
// the authenticated actor that presents its execution credential.
func registerExecutionPrincipal(env *testEnv, execution id.ExecutionID) authorize.Principal {
	env.principals.put(identity.Principal{
		ID:          executionPrincipalID,
		Kind:        identity.KindExecution,
		Scope:       projectScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: execution,
	})
	return executionActor(executionPrincipalID, execution)
}

// allowEveryActor makes the grant stage permissive so a refusal proves the
// service's own execution-authority guard, not the authorizer fixture.
func allowEveryActor(env *testEnv) {
	env.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionAllow, nil
	}
}

// useRegistryMatrix replaces the fixture's permissive grant stub with the real
// declared operation matrix, so the tests see the same refusal an execution
// principal gets from the shared authorizer.
func useRegistryMatrix(env *testEnv) {
	matrix := authorize.NewMatrix()
	env.authorizer.authorize = matrix.Authorize
}

// TestExecutionActorMayUseItsOwnCurrentTask proves an execution principal is
// accepted for read, content edit, ask and history when it is the task's
// current execution, and that the allow comes from its admitted execution
// permission rather than a human/service grant.
func TestExecutionActorMayUseItsOwnCurrentTask(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		env := newTestEnv(t)
		useRegistryMatrix(env)
		actor := registerExecutionPrincipal(env, executionOne)
		env.reader.put(baseTask())

		current, err := env.service.Read(t.Context(), readCommand(actor))
		requireNoError(t, err, "Read by own execution")
		if current.ID != taskID || current.CurrentExecution != executionOne {
			t.Fatalf("read task = %+v; want %q at %q", current, taskID, executionOne)
		}
	})

	t.Run("ask", func(t *testing.T) {
		env := newTestEnv(t)
		useRegistryMatrix(env)
		actor := registerExecutionPrincipal(env, executionOne)
		env.reader.put(baseTask())
		env.executions.put(openExecution(2))

		result, err := env.service.Ask(t.Context(), askCommand(actor, 4, "Which candidate?", roleAssignment(t, tenancy.RoleReviewer)))
		requireNoError(t, err, "Ask by own execution")
		if result.LifecycleVersion != 5 {
			t.Fatalf("ask lifecycle = %d; want 5", result.LifecycleVersion)
		}
		unit := env.ports.lastAsk(t)
		if unit.Question != "Which candidate?" {
			t.Fatalf("ask question = %q", unit.Question)
		}
		executionVersion := requireAuthority(t, unit.Authority, "task.ask", authorize.CheckExecutionVersion)
		if executionVersion.TargetID != string(executionOne) || executionVersion.ExpectedVersion != 2 {
			t.Fatalf("execution_version read = %+v; want the asking execution %q@2", executionVersion, executionOne)
		}
		if env.ports.totalCalls() != 1 {
			t.Fatalf("commit port calls = %d; want one ask unit", env.ports.totalCalls())
		}
	})

	t.Run("content edit", func(t *testing.T) {
		env := newTestEnv(t)
		useRegistryMatrix(env)
		actor := registerExecutionPrincipal(env, executionOne)
		env.reader.put(baseTask())

		_, err := env.service.Edit(t.Context(), editCommand(actor, task.EditContent))
		requireNoError(t, err, "content edit by own execution")
		unit := env.ports.lastEdit(t)
		if unit.Kind != task.EditContent || unit.Title != "Updated title" {
			t.Fatalf("edit unit = %+v; want content edit", unit)
		}
	})

	t.Run("history", func(t *testing.T) {
		env := newTestEnv(t)
		useRegistryMatrix(env)
		actor := registerExecutionPrincipal(env, executionOne)
		env.reader.put(baseTask())

		_, err := env.service.History(t.Context(), historyCommand(actor, 5, ""))
		requireNoError(t, err, "history by own execution")
		if queries := env.reader.historyQueries(); len(queries) != 1 || queries[0].Limit != 5 {
			t.Fatalf("history queries = %+v; want one with limit 5", queries)
		}
	})
}

// TestExecutionActorRefusesForeignOrStaleTask proves an execution principal
// bound to another task's execution, or to a previous execution of this task,
// is refused Forbidden before any commit, admission or history read.
func TestExecutionActorRefusesForeignOrStaleTask(t *testing.T) {
	foreign := func() task.Task {
		value := baseTask()
		value.ExecutionHistory = []id.ExecutionID{executionOne, executionTwo}
		value.CurrentExecution = executionOne
		return value
	}
	other := baseTask()
	other.ID = otherTaskID
	other.ExecutionHistory = []id.ExecutionID{executionTwo}
	other.CurrentExecution = executionTwo

	scenarios := []struct {
		name      string
		execution id.ExecutionID
		value     task.Task
		other     *task.Task
	}{
		{name: "another task's current execution", execution: executionTwo, value: baseTask(), other: &other},
		{name: "stale previous execution", execution: executionTwo, value: foreign()},
	}

	operations := []struct {
		name string
		call func(*testEnv, authorize.Principal) error
	}{
		{
			name: "read",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Read(t.Context(), readCommand(actor))
				return err
			},
		},
		{
			name: "ask",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Ask(t.Context(), askCommand(actor, 4, "Foreign question?", roleAssignment(t, tenancy.RoleReviewer)))
				return err
			},
		},
		{
			name: "content edit",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Edit(t.Context(), editCommand(actor, task.EditContent))
				return err
			},
		},
		{
			name: "history",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.History(t.Context(), historyCommand(actor, 5, ""))
				return err
			},
		},
	}

	for _, scenario := range scenarios {
		for _, operation := range operations {
			t.Run(scenario.name+"/"+operation.name, func(t *testing.T) {
				env := newTestEnv(t)
				allowEveryActor(env)
				actor := registerExecutionPrincipal(env, scenario.execution)
				env.reader.put(scenario.value)
				if scenario.other != nil {
					env.reader.put(*scenario.other)
				}
				env.executions.put(openExecution(2))

				err := operation.call(env, actor)
				requireFaultClass(t, err, fault.Forbidden)
				requireNoPortCalls(t, env.ports)
				if env.admission.callCount() != 0 {
					t.Fatalf("admission calls = %d; want none", env.admission.callCount())
				}
				if env.executions.callCount() != 0 {
					t.Fatalf("execution reads = %d; want none before the binding refusal", env.executions.callCount())
				}
				if queries := env.reader.historyQueries(); len(queries) != 0 {
					t.Fatalf("history read for a foreign execution: %+v", queries)
				}
			})
		}
	}
}

// TestExecutionActorRefusesOutOfScopeOperationsUnderRegistryMatrix proves an
// execution principal's admitted permission does not widen it into task
// creation or conversation comments: both keep the shared matrix refusal.
func TestExecutionActorRefusesOutOfScopeOperationsUnderRegistryMatrix(t *testing.T) {
	operations := []struct {
		name string
		call func(*testEnv, authorize.Principal) error
	}{
		{
			name: "create",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Create(t.Context(), createCommand(actor, "Execution-created task"))
				return err
			},
		},
		{
			name: "comment",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Edit(t.Context(), editCommand(actor, task.EditComment))
				return err
			},
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			env := newTestEnv(t)
			useRegistryMatrix(env)
			actor := registerExecutionPrincipal(env, executionOne)
			env.reader.put(baseTask())

			err := operation.call(env, actor)
			// The declared matrix refuses an execution principal before it can
			// reach a membership grant.
			requireFaultClass(t, err, fault.InvalidInput)
			requireNoPortCalls(t, env.ports)
			if env.admission.callCount() != 0 {
				t.Fatalf("admission calls = %d; want none", env.admission.callCount())
			}
			if env.executions.callCount() != 0 {
				t.Fatalf("execution reads = %d; want none", env.executions.callCount())
			}
		})
	}
}

// TestExecutionActorRefusesNonExecutionOperations proves an execution
// principal cannot cancel, start, reply, select a pipeline or create-and-start:
// the registry declares no execution permission for those operations.
func TestExecutionActorRefusesNonExecutionOperations(t *testing.T) {
	operations := []struct {
		name string
		call func(*testEnv, authorize.Principal) error
	}{
		{
			name: "cancel",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Cancel(t.Context(), cancelCommand(actor, 4))
				return err
			},
		},
		{
			name: "start",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Start(t.Context(), startCommand(actor, 4))
				return err
			},
		},
		{
			name: "reply",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "answer"))
				return err
			},
		},
		{
			name: "select_pipeline",
			call: func(env *testEnv, actor authorize.Principal) error {
				_, err := env.service.Edit(t.Context(), editCommand(actor, task.EditSelectPipeline))
				return err
			},
		},
		{
			name: "create_and_start",
			call: func(env *testEnv, actor authorize.Principal) error {
				command := createCommand(actor, "Execution-created task")
				command.Start = true
				_, err := env.service.Create(t.Context(), command)
				return err
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			env := newTestEnv(t)
			allowEveryActor(env)
			actor := registerExecutionPrincipal(env, executionOne)
			env.reader.put(baseTask())
			env.executions.put(openExecution(2))

			err := operation.call(env, actor)
			requireFaultClass(t, err, fault.Forbidden)
			requireNoPortCalls(t, env.ports)
			if env.admission.callCount() != 0 {
				t.Fatalf("admission calls = %d; want none", env.admission.callCount())
			}
			if env.executions.callCount() != 0 {
				t.Fatalf("execution reads = %d; want none", env.executions.callCount())
			}
			if env.pipelines.callCount() != 0 {
				t.Fatalf("pipeline reads = %d; want none", env.pipelines.callCount())
			}
		})
	}
}
