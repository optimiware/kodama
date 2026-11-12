package task_test

import (
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func startCommand(actor authorize.Principal, expectedLifecycle uint64) task.StartCommand {
	return task.StartCommand{
		Actor:                    actor,
		Scope:                    actor.Scope,
		TaskID:                   taskID,
		ExpectedLifecycleVersion: expectedLifecycle,
		CallerKey:                "caller-start",
		Content:                  receiptContent(`{"start":"task"}`),
	}
}

func openTaskWithoutExecution() task.Task {
	value := baseTask()
	value.ExecutionHistory = nil
	value.CurrentExecution = ""
	return value
}

// TestStartAdmitsOneQueuedExecution proves ordinary start builds one complete
// admission unit: queued execution, current pointer/history, snapshot, receipt,
// audit, event and dispatch, with no task creation.
func TestStartAdmitsOneQueuedExecution(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(openTaskWithoutExecution())

	result, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "Start")

	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	admitted := env.admission.requestsSnapshot()[0]
	if admitted.Definition.ID != pipelineID {
		t.Fatalf("admitted pipeline = %q; want the task selection %q", admitted.Definition.ID, pipelineID)
	}
	if admitted.Inputs["repository"] != "example/repo" {
		t.Fatalf("admitted inputs = %+v", admitted.Inputs)
	}
	if admitted.Task.TaskID != taskID {
		t.Fatalf("admitted task id = %q; want %q", admitted.Task.TaskID, taskID)
	}
	if env.executions.callCount() != 0 {
		t.Fatalf("execution reader calls = %d; a fresh start has no predecessor", env.executions.callCount())
	}

	if len(env.ports.starts) != 1 {
		t.Fatalf("start port calls = %d; want exactly one", len(env.ports.starts))
	}
	if len(env.ports.creates) != 0 {
		t.Fatalf("create port called during ordinary start")
	}
	unit := env.ports.lastStart(t)
	if unit.Owner != actor.Scope || unit.TaskID != taskID {
		t.Fatalf("start identity = %+v/%q", unit.Owner, unit.TaskID)
	}
	if unit.ExpectedLifecycleVersion != 4 {
		t.Fatalf("expected lifecycle = %d; want 4", unit.ExpectedLifecycleVersion)
	}
	if unit.PipelineID != pipelineID {
		t.Fatalf("pipeline = %q; want %q", unit.PipelineID, pipelineID)
	}
	if unit.ExpectedPipelineVersion != 7 {
		t.Fatalf("expected pipeline version = %d; want admitted 7", unit.ExpectedPipelineVersion)
	}
	if unit.Snapshot.TaskID != taskID {
		t.Fatalf("snapshot task id = %q; want %q", unit.Snapshot.TaskID, taskID)
	}
	if len(unit.Actions) != 1 || unit.Actions[0].Name != "run" {
		t.Fatalf("actions = %+v", unit.Actions)
	}
	if unit.Initiator != actor.ID || unit.ExecutionPrincipal == "" {
		t.Fatalf("principals = %q/%q", unit.Initiator, unit.ExecutionPrincipal)
	}
	if unit.NewExecutionID == "" || unit.NewExecutionID != result.ExecutionID {
		t.Fatalf("new execution = %q; want result %q", unit.NewExecutionID, result.ExecutionID)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("start time = %v; want %v", unit.At, testNow)
	}

	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "task.start", "caller-start", receiptContent(`{"start":"task"}`), testNow)
	if unit.Receipt.ExecutionID != result.ExecutionID {
		t.Fatalf("receipt execution = %q; want %q", unit.Receipt.ExecutionID, result.ExecutionID)
	}
	if unit.Receipt.SnapshotHash == nil || *unit.Receipt.SnapshotHash != unit.Snapshot.YAMLSHA256 {
		t.Fatalf("receipt snapshot hash = %v; want the admitted snapshot hash", unit.Receipt.SnapshotHash)
	}
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "task.start", testNow)
	requireEvent(t, unit.Events, event.TypeExecutionQueued, actor.Scope, string(result.ExecutionID), 1, testNow)
	requireNoEvent(t, unit.Events, event.TypeTaskCreated)
	requirePublish(t, unit.PostCommit)
	requireDispatch(t, unit.PostCommit)

	requireAuthority(t, unit.Authority, "task.start", authorize.CheckMembership)
	taskVersion := requireAuthority(t, unit.Authority, "task.start", authorize.CheckTaskVersion)
	if taskVersion.ExpectedVersion != 4 {
		t.Fatalf("task_version read = %d; want 4", taskVersion.ExpectedVersion)
	}
	pipelineVersion := requireAuthority(t, unit.Authority, "task.start", authorize.CheckPipelineVersion)
	if pipelineVersion.TargetID != string(pipelineID) || pipelineVersion.ExpectedVersion != 7 {
		t.Fatalf("pipeline_version read = %+v; want %q@7", pipelineVersion, pipelineID)
	}
	if result.LifecycleVersion != 5 || result.Replayed {
		t.Fatalf("start result = %+v; want lifecycle 5 fresh", result)
	}
}

// TestStartRefusesWaitingDoneAndCancelled proves a waiting handoff permits no
// ordinary start and terminal tasks stay terminal, before any admission.
func TestStartRefusesWaitingDoneAndCancelled(t *testing.T) {
	cases := map[string]task.Task{
		"waiting":   waitingTask(t),
		"done":      doneTask(),
		"cancelled": cancelledTask(),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
			env.reader.put(value)
			_, err := env.service.Start(t.Context(), startCommand(actor, value.LifecycleVersion))
			requireFaultClass(t, err, fault.VersionConflict)
			if env.admission.callCount() != 0 {
				t.Fatalf("admission calls = %d after a %s start", env.admission.callCount(), name)
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestStartRequiresSelectedPipeline proves an open task without a pipeline is
// refused before admission.
func TestStartRequiresSelectedPipeline(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := openTaskWithoutExecution()
	value.SelectedPipeline = ""
	env.reader.put(value)

	_, err := env.service.Start(t.Context(), startCommand(actor, value.LifecycleVersion))
	requireFaultClass(t, err, fault.InvalidInput)
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d", env.admission.callCount())
	}
	requireNoPortCalls(t, env.ports)
}

// TestStartDispatchWaitsForPredecessorStop proves the queued execution is the
// durable dispatch record: a start commits with a publish effect and dispatches
// only when the predecessor's stop evidence is confirmed. Reply and stop
// confirmation may arrive in either order.
func TestStartDispatchWaitsForPredecessorStop(t *testing.T) {
	cases := map[string]struct {
		view         task.ExecutionView
		wantDispatch bool
	}{
		"stop pending": {
			view:         task.ExecutionView{ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: pendingStop()},
			wantDispatch: false,
		},
		"stop confirmed": {
			view:         task.ExecutionView{ID: executionOne, Version: 2, State: kernel.ExecutionCancelled, StopEvidence: confirmedStop()},
			wantDispatch: true,
		},
		"nonterminal predecessor": {
			view:         task.ExecutionView{ID: executionOne, Version: 2, State: kernel.ExecutionRunning, StopEvidence: pendingStop()},
			wantDispatch: false,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
			value := baseTask()
			env.reader.put(value)
			env.executions.put(testCase.view)

			_, err := env.service.Start(t.Context(), startCommand(actor, value.LifecycleVersion))
			requireNoError(t, err, "Start")

			if calls := env.executions.calledIDs(); len(calls) != 1 || calls[0] != executionOne {
				t.Fatalf("execution reads = %v; want the predecessor %q", calls, executionOne)
			}
			unit := env.ports.lastStart(t)
			requirePublish(t, unit.PostCommit)
			if testCase.wantDispatch {
				requireDispatch(t, unit.PostCommit)
			} else {
				requireNoDispatch(t, unit.PostCommit)
			}
		})
	}
}

// TestStartPropagatesPortVersionConflict proves the expected lifecycle version
// travels in the unit and a stale version is refused at the commit boundary.
func TestStartPropagatesPortVersionConflict(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := openTaskWithoutExecution()
	env.reader.put(value)
	env.ports.setStart(func(unit task.StartUnit) (task.StartResult, error) {
		return task.StartResult{}, fault.New(fault.VersionConflict, "stale lifecycle version")
	})

	_, err := env.service.Start(t.Context(), startCommand(actor, 3))
	requireFaultClass(t, err, fault.VersionConflict)
	unit := env.ports.lastStart(t)
	if unit.ExpectedLifecycleVersion != 3 {
		t.Fatalf("unit expected lifecycle = %d; want the caller's stale 3", unit.ExpectedLifecycleVersion)
	}
}

// TestStartRetryConvergesOnOriginalExecution models a lost response after the
// start committed: the identical retry must return the original execution id
// and a single applied admission.
func TestStartRetryConvergesOnOriginalExecution(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	env.reader.put(openTaskWithoutExecution())

	type committed struct {
		result task.StartResult
		hash   string
	}
	var mu sync.Mutex
	receipts := make(map[string]committed)
	applied := 0
	original := task.StartResult{ExecutionID: "execution-original", LifecycleVersion: 5}
	env.ports.setStart(func(unit task.StartUnit) (task.StartResult, error) {
		mu.Lock()
		defer mu.Unlock()
		key := unit.Receipt.Key.CallerKey
		if previous, ok := receipts[key]; ok {
			if previous.hash != unit.Receipt.ContentHash {
				return task.StartResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
			}
			replayed := previous.result
			replayed.Replayed = true
			return replayed, nil
		}
		receipts[key] = committed{result: original, hash: unit.Receipt.ContentHash}
		applied++
		return task.StartResult{}, fault.New(fault.DependencyUnavailable, "response lost after commit")
	})

	if _, err := env.service.Start(t.Context(), startCommand(actor, 4)); err == nil {
		t.Fatal("first start reported success despite the lost response")
	} else {
		requireFaultClass(t, err, fault.DependencyUnavailable)
	}
	result, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "Start retry")
	if result.ExecutionID != original.ExecutionID || !result.Replayed {
		t.Fatalf("retry result = %+v; want original %q replayed", result, original.ExecutionID)
	}
	if applied != 1 {
		t.Fatalf("applied starts = %d; want 1", applied)
	}
}

// TestStartRefusesCrossScope proves another scope's task id is never admitted.
func TestStartRefusesCrossScope(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	value := openTaskWithoutExecution()
	value.Scope = otherScope()
	env.reader.put(value)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireFaultClass(t, err, fault.NotFound)
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d", env.admission.callCount())
	}
	requireNoPortCalls(t, env.ports)

	outsider := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	outsider.Scope = otherScope()
	command := startCommand(outsider, 4)
	command.Scope = projectScope()
	_, err = env.service.Start(t.Context(), command)
	requireFaultClass(t, err, fault.Forbidden)
}

// failedOpenTask is one open task whose current execution failed at the
// supplied version.
func failedOpenTask(t *testing.T, env *testEnv, version uint64) task.Task {
	t.Helper()
	value := baseTask()
	env.executions.put(task.ExecutionView{
		ID: executionOne, Version: version, State: kernel.ExecutionFailed, StopEvidence: confirmedStop(),
	})
	return value
}

// TestOperatorStartCarriesFailedTaskConditionalRead proves an operator-only
// task.start carries the conditional failed_task_operator_start read naming the
// observed current execution and its version, so the commit boundary can
// re-read both atomically. Engineers never carry it.
func TestOperatorStartCarriesFailedTaskConditionalRead(t *testing.T) {
	env := newTestEnv(t)
	env.reader.put(failedOpenTask(t, env, 3))
	actor := humanActor(operatorPrincipal, tenancy.RoleOperator)

	result, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "operator Start")

	unit := env.ports.lastStart(t)
	conditional := requireAuthority(t, unit.Authority, "task.start", authorize.CheckFailedTaskOperatorStart)
	if conditional.TargetID != string(executionOne) {
		t.Fatalf("failed_task_operator_start target = %q; want the current execution %q", conditional.TargetID, executionOne)
	}
	if conditional.ExpectedVersion != 3 {
		t.Fatalf("failed_task_operator_start expected version = %d; want the observed 3", conditional.ExpectedVersion)
	}
	if conditional.PrincipalID != actor.ID {
		t.Fatalf("failed_task_operator_start principal = %q; want %q", conditional.PrincipalID, actor.ID)
	}
	// The ordinary version/membership reads are still carried.
	requireAuthority(t, unit.Authority, "task.start", authorize.CheckMembership)
	requireAuthority(t, unit.Authority, "task.start", authorize.CheckTaskVersion)
	requireAuthority(t, unit.Authority, "task.start", authorize.CheckPipelineVersion)
	if result.LifecycleVersion != 5 || result.Replayed {
		t.Fatalf("start result = %+v; want lifecycle 5 fresh", result)
	}
}

// TestEngineerStartOmitsOperatorConditionalRead proves the engineer grant is
// unchanged: the condition is not evaluated, so no conditional read is built.
func TestEngineerStartOmitsOperatorConditionalRead(t *testing.T) {
	env := newTestEnv(t)
	env.reader.put(failedOpenTask(t, env, 3))
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "engineer Start")
	requireNoAuthority(t, env.ports.lastStart(t).Authority, authorize.CheckFailedTaskOperatorStart)
}

// TestEngineerAndOperatorStartOmitsOperatorConditionalRead proves a caller who
// also holds the engineer grant is evaluated as an engineer.
func TestEngineerAndOperatorStartOmitsOperatorConditionalRead(t *testing.T) {
	env := newTestEnv(t)
	env.principals.put(identity.Principal{
		ID: operatorPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
		Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, Enabled: true, Version: 1,
	})
	env.reader.put(failedOpenTask(t, env, 3))
	actor := humanActor(operatorPrincipal, tenancy.RoleEngineer, tenancy.RoleOperator)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "engineer+operator Start")
	requireNoAuthority(t, env.ports.lastStart(t).Authority, authorize.CheckFailedTaskOperatorStart)
}

// TestOperatorStartWithoutExecutionCarriesEmptyTarget proves a task with no
// current execution still carries the conditional read, with the contract's
// empty target and zero version, so the commit boundary fails closed.
func TestOperatorStartWithoutExecutionCarriesEmptyTarget(t *testing.T) {
	env := newTestEnv(t)
	env.reader.put(openTaskWithoutExecution())
	actor := humanActor(operatorPrincipal, tenancy.RoleOperator)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "operator Start without execution")
	conditional := requireAuthority(t, env.ports.lastStart(t).Authority, "task.start", authorize.CheckFailedTaskOperatorStart)
	if conditional.TargetID != "" || conditional.ExpectedVersion != 0 {
		t.Fatalf("no-execution conditional read = %+v; want an empty target and zero version", conditional)
	}
}

// TestOperatorStartRefusesNonOpenTask proves the existing task-state rule still
// refuses a waiting/done/cancelled task for every role before admission.
func TestOperatorStartRefusesNonOpenTask(t *testing.T) {
	for _, state := range []task.State{task.StateWaiting, task.StateDone, task.StateCancelled} {
		env := newTestEnv(t)
		value := failedOpenTask(t, env, 3)
		value.State = state
		env.reader.put(value)
		actor := humanActor(operatorPrincipal, tenancy.RoleOperator)

		_, err := env.service.Start(t.Context(), startCommand(actor, 4))
		requireFaultClass(t, err, fault.VersionConflict)
		if env.admission.callCount() != 0 {
			t.Fatalf("%s: admission calls = %d", state, env.admission.callCount())
		}
		requireNoPortCalls(t, env.ports)
	}
}

// TestOperatorStartKeepsTheExistingPipeline proves the retry grant adds no
// pipeline selection: admission always uses the task's selected pipeline.
func TestOperatorStartKeepsTheExistingPipeline(t *testing.T) {
	env := newTestEnv(t)
	env.reader.put(failedOpenTask(t, env, 3))
	actor := humanActor(operatorPrincipal, tenancy.RoleOperator)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "operator Start")
	if admitted := env.admission.requestsSnapshot()[0]; admitted.Definition.ID != pipelineID {
		t.Fatalf("operator admitted pipeline = %q; want the task selection %q", admitted.Definition.ID, pipelineID)
	}
}

// TestOperatorStartConditionalReadUsesTheReReadActor proves the conditional
// grant is decided from the re-read principal, not the stale role the command
// claimed. A command claiming engineer while the current principal is
// operator-only must still carry the failed-task conditional read.
func TestOperatorStartConditionalReadUsesTheReReadActor(t *testing.T) {
	env := newTestEnv(t)
	env.principals.put(identity.Principal{
		ID: operatorPrincipal, Kind: identity.KindHuman, Scope: projectScope(),
		Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 2,
	})
	env.reader.put(failedOpenTask(t, env, 3))
	// The command's cached claim still says engineer; the re-read principal is
	// operator-only.
	actor := humanActor(operatorPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Start(t.Context(), startCommand(actor, 4))
	requireNoError(t, err, "operator Start with a stale engineer claim")
	conditional := requireAuthority(t, env.ports.lastStart(t).Authority, "task.start", authorize.CheckFailedTaskOperatorStart)
	if conditional.PrincipalID != operatorPrincipal {
		t.Fatalf("conditional read principal = %q; want the re-read operator %q", conditional.PrincipalID, operatorPrincipal)
	}
}
