package task_test

// Both task cancel paths build the Kernel cancel reduction.
//
// `task.ask` (AskUnit) and `task.cancel` (CancelUnit) never write
// `executions.state = cancelled` directly. For a known current execution the
// service loads the version-guarded aggregate through
// `task.ExecutionAggregateReader`, reduces `flow.CancelFact` exactly as the
// execution service does, carries the resulting `flow.Commit` and the
// aggregate version, validates the unit with
// `task.ValidateCancellationCommit`, and appends
// `event.ExcludeOwned(event.ProjectLifecycle(pre, commit))` after its own task
// event. A terminal predecessor is preserved with no commit; no predecessor
// carries no commit and a zero execution version.
//
// These tests pin the exact unit the adapter applies, not the SQL.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	cancelWorkspace     = id.WorkspaceID("workspace-1")
	cancelWorkload      = id.WorkloadID("workload-1")
	cancelContainer     = id.ActionID("action-container")
	cancelRequestAction = id.ActionID("action-request")
	cancelPending       = id.ActionID("action-pending")
)

// queuedAggregate is admitted but never started: cancelling it cancels every
// pending action and carries no stop intent.
func queuedAggregate() kernel.Execution {
	return kernel.Execution{
		ID:            executionOne,
		Version:       2,
		State:         kernel.ExecutionQueued,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: -1,
		Actions: []kernel.Action{{
			ID:      cancelRequestAction,
			Index:   0,
			Kind:    definition.ActionKindRequest,
			State:   kernel.ActionPending,
			Version: 1,
			Request: &kernel.RequestAction{},
		}},
	}
}

// runningContainerAggregate is a running container with one running action and
// one pending successor: the reduction stops the workload, cancels both actions
// and owes cleanup.
func runningContainerAggregate() kernel.Execution {
	return kernel.Execution{
		ID:            executionOne,
		Version:       7,
		State:         kernel.ExecutionRunning,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: 0,
		Actions: []kernel.Action{
			{
				ID:      cancelContainer,
				Index:   0,
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionRunning,
				Version: 4,
				Container: &kernel.ContainerAction{
					WorkloadID:  cancelWorkload,
					WorkspaceID: cancelWorkspace,
					Launch:      kernel.LaunchIntent{WorkloadID: cancelWorkload, State: kernel.LaunchIssued},
				},
			},
			{
				ID:      cancelPending,
				Index:   1,
				Kind:    definition.ActionKindContainer,
				State:   kernel.ActionPending,
				Version: 1,
			},
		},
	}
}

// runningRequestAggregate waits at a request action: it cancels the request
// action but never emits a stop intent because no workload exists.
func runningRequestAggregate() kernel.Execution {
	return kernel.Execution{
		ID:            executionOne,
		Version:       3,
		State:         kernel.ExecutionRunning,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: 0,
		Actions: []kernel.Action{{
			ID:      cancelRequestAction,
			Index:   0,
			Kind:    definition.ActionKindRequest,
			State:   kernel.ActionRunning,
			Version: 3,
			Request: &kernel.RequestAction{},
		}},
	}
}

func terminalAggregate(state kernel.ExecutionState, version uint64, cleanup kernel.Cleanup) kernel.Execution {
	aggregate := kernel.Execution{
		ID:            executionOne,
		Version:       version,
		State:         state,
		WorkspaceID:   cancelWorkspace,
		CurrentAction: -1,
		Cleanup:       cleanup,
		Actions: []kernel.Action{{
			ID:      cancelContainer,
			Index:   0,
			Kind:    definition.ActionKindContainer,
			State:   kernel.ActionCancelled,
			Version: 5,
			Container: &kernel.ContainerAction{
				WorkloadID:  cancelWorkload,
				WorkspaceID: cancelWorkspace,
			},
		}},
	}
	if state == kernel.ExecutionFailed {
		aggregate.Failure = &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "crashed"}
	}
	return aggregate
}

// seedAggregate wires the task's current-execution pointer, the predecessor
// view and the aggregate the new reader returns.
func seedAggregate(env *testEnv, aggregate kernel.Execution) {
	value := baseTask()
	value.CurrentExecution = aggregate.ID
	env.reader.put(value)
	env.executions.put(task.ExecutionView{ID: aggregate.ID, Version: aggregate.Version, State: aggregate.State})
	env.executions.putAggregate(aggregate)
}

type cancellationUnit struct {
	commit                   *flow.Commit
	expectedExecutionVersion uint64
	events                   []event.Envelope
	authority                []authorize.AuthorityRead
}

func askUnitFor(t *testing.T, aggregate kernel.Execution) cancellationUnit {
	t.Helper()
	env := newTestEnv(t)
	seedAggregate(env, aggregate)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	_, err := env.service.Ask(t.Context(), askCommand(actor, 4, "Which candidate?", roleAssignment(t, tenancy.RoleReviewer)))
	requireNoError(t, err, "Ask")
	unit := env.ports.lastAsk(t)
	return cancellationUnit{
		commit:                   unit.Commit,
		expectedExecutionVersion: unit.ExpectedExecutionVersion,
		events:                   unit.Events,
		authority:                unit.Authority,
	}
}

func cancelUnitFor(t *testing.T, aggregate kernel.Execution) cancellationUnit {
	t.Helper()
	env := newTestEnv(t)
	seedAggregate(env, aggregate)
	actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
	_, err := env.service.Cancel(t.Context(), cancelCommand(actor, 4))
	requireNoError(t, err, "Cancel")
	unit := env.ports.lastCancel(t)
	return cancellationUnit{
		commit:                   unit.Commit,
		expectedExecutionVersion: unit.ExpectedExecutionVersion,
		events:                   unit.Events,
		authority:                unit.Authority,
	}
}

// TestTaskCancellationBuildsKernelCancelCommit pins the exact commit for every
// predecessor state on both task cancel paths.
func TestTaskCancellationBuildsKernelCancelCommit(t *testing.T) {
	cases := []struct {
		name      string
		aggregate kernel.Execution
		terminal  bool
		wantStop  bool
	}{
		{name: "queued", aggregate: queuedAggregate()},
		{name: "running container with a pending successor", aggregate: runningContainerAggregate(), wantStop: true},
		{name: "running at a request wait", aggregate: runningRequestAggregate()},
		{name: "terminal succeeded", aggregate: terminalAggregate(kernel.ExecutionSucceeded, 8, kernel.Cleanup{}), terminal: true},
		{name: "terminal failed", aggregate: terminalAggregate(kernel.ExecutionFailed, 6, kernel.Cleanup{}), terminal: true},
		{name: "terminal cancelled", aggregate: terminalAggregate(kernel.ExecutionCancelled, 9, kernel.Cleanup{}), terminal: true},
		{name: "terminal with cleanup pending", aggregate: terminalAggregate(kernel.ExecutionCancelled, 10, kernel.Cleanup{State: kernel.CleanupPending}), terminal: true},
	}
	for _, testCase := range cases {
		for _, path := range []struct {
			name  string
			run   func(*testing.T, kernel.Execution) cancellationUnit
			owner event.Type
		}{
			{name: "ask", run: askUnitFor, owner: event.TypeTaskQuestion},
			{name: "cancel", run: cancelUnitFor, owner: event.TypeTaskCancelled},
		} {
			t.Run(path.name+"/"+testCase.name, func(t *testing.T) {
				unit := path.run(t, testCase.aggregate)
				assertCancellationUnit(t, unit, testCase.aggregate, path.owner, testCase.terminal, testCase.wantStop)
			})
		}
	}
}

// TestTaskCancellationWithoutPredecessorCarriesNoCommit pins the no-execution
// path: no aggregate read, no commit and only the owner event.
func TestTaskCancellationWithoutPredecessorCarriesNoCommit(t *testing.T) {
	for _, path := range []struct {
		name  string
		run   func(*testing.T) cancellationUnit
		owner event.Type
	}{
		{
			name: "ask",
			run: func(t *testing.T) cancellationUnit {
				t.Helper()
				env := newTestEnv(t)
				env.reader.put(baseTaskWithNoExecution())
				actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
				_, err := env.service.Ask(t.Context(), askCommand(actor, 4, "Which candidate?", roleAssignment(t, tenancy.RoleReviewer)))
				requireNoError(t, err, "Ask")
				if calls := env.executions.loadCalls(); len(calls) != 0 {
					t.Fatalf("aggregate loads = %v; want none without a predecessor", calls)
				}
				unit := env.ports.lastAsk(t)
				return cancellationUnit{commit: unit.Commit, expectedExecutionVersion: unit.ExpectedExecutionVersion, events: unit.Events, authority: unit.Authority}
			},
			owner: event.TypeTaskQuestion,
		},
		{
			name: "cancel",
			run: func(t *testing.T) cancellationUnit {
				t.Helper()
				env := newTestEnv(t)
				env.reader.put(baseTaskWithNoExecution())
				actor := humanActor(operatorPrincipal, tenancy.RoleOperator)
				_, err := env.service.Cancel(t.Context(), cancelCommand(actor, 4))
				requireNoError(t, err, "Cancel")
				if calls := env.executions.loadCalls(); len(calls) != 0 {
					t.Fatalf("aggregate loads = %v; want none without a predecessor", calls)
				}
				unit := env.ports.lastCancel(t)
				return cancellationUnit{commit: unit.Commit, expectedExecutionVersion: unit.ExpectedExecutionVersion, events: unit.Events, authority: unit.Authority}
			},
			owner: event.TypeTaskCancelled,
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			unit := path.run(t)
			if unit.commit != nil {
				t.Fatalf("commit = %+v; want none without a predecessor", unit.commit)
			}
			if unit.expectedExecutionVersion != 0 {
				t.Fatalf("expected execution version = %d; want 0", unit.expectedExecutionVersion)
			}
			assertOwnerOnlyEvents(t, unit.events, path.owner)
			requireNoAuthority(t, unit.authority, authorize.CheckExecutionVersion)
		})
	}
}

func baseTaskWithNoExecution() task.Task {
	value := baseTask()
	value.CurrentExecution = ""
	value.ExecutionHistory = nil
	return value
}

func assertCancellationUnit(
	t *testing.T,
	unit cancellationUnit,
	aggregate kernel.Execution,
	owner event.Type,
	terminal bool,
	wantStop bool,
) {
	t.Helper()
	if unit.expectedExecutionVersion != aggregate.Version {
		t.Fatalf("expected execution version = %d; want the aggregate version %d", unit.expectedExecutionVersion, aggregate.Version)
	}
	operation := authorize.OperationName("task.cancel")
	if owner == event.TypeTaskQuestion {
		operation = "task.ask"
	}
	executionVersion := requireAuthority(t, unit.authority, operation, authorize.CheckExecutionVersion)
	if executionVersion.TargetID != string(aggregate.ID) || executionVersion.ExpectedVersion != aggregate.Version {
		t.Fatalf("execution_version read = %+v; want %q@%d", executionVersion, aggregate.ID, aggregate.Version)
	}
	if terminal {
		if unit.commit != nil {
			t.Fatalf("commit = %+v; a terminal predecessor must be preserved with no commit", unit.commit)
		}
		assertOwnerOnlyEvents(t, unit.events, owner)
		return
	}
	commit := unit.commit
	if commit == nil {
		t.Fatal("non-terminal predecessor carries no cancel commit")
	}
	if commit.ExecutionID != aggregate.ID {
		t.Fatalf("commit execution = %q; want %q", commit.ExecutionID, aggregate.ID)
	}
	if commit.ExpectedExecutionVersion != aggregate.Version {
		t.Fatalf("commit expected version = %d; want %d", commit.ExpectedExecutionVersion, aggregate.Version)
	}
	if commit.NextExecutionState != kernel.ExecutionCancelled {
		t.Fatalf("commit next state = %q; want cancelled", commit.NextExecutionState)
	}
	if commit.CleanupUpdate == nil || commit.CleanupUpdate.State != kernel.CleanupPending {
		t.Fatalf("commit cleanup = %+v; want pending", commit.CleanupUpdate)
	}
	if commit.Cleanup == nil || commit.Cleanup.Reason != "cancelled" {
		t.Fatalf("commit cleanup intent = %+v; want reason cancelled", commit.Cleanup)
	}
	if commit.Output.Action != flow.OutputDiscard {
		t.Fatalf("commit output change = %q; want discard", commit.Output.Action)
	}
	if !commit.InvalidateCredential {
		t.Fatal("commit does not invalidate the credential")
	}
	assertCommitActions(t, commit, aggregate)
	if wantStop {
		if commit.Stop == nil {
			t.Fatal("running container cancel carries no stop intent")
		}
		if commit.Stop.WorkloadID != cancelWorkload || commit.Stop.WorkspaceID != cancelWorkspace {
			t.Fatalf("stop intent = %+v; want workload %q workspace %q", commit.Stop, cancelWorkload, cancelWorkspace)
		}
	} else if commit.Stop != nil {
		t.Fatalf("stop intent = %+v; want none for a predecessor with no running workload", commit.Stop)
	}
	assertCancellationEvents(t, unit.events, owner, aggregate)
}

// assertCommitActions checks the primary action and every pending cancellation
// against the aggregate.
func assertCommitActions(t *testing.T, commit *flow.Commit, aggregate kernel.Execution) {
	t.Helper()
	running := -1
	for index, action := range aggregate.Actions {
		if action.State == kernel.ActionRunning {
			running = index
		}
	}
	if running < 0 {
		if commit.ActionID != "" || commit.ExpectedActionVersion != 0 || commit.NextActionState != "" {
			t.Fatalf("commit names primary action %q; the aggregate has none running", commit.ActionID)
		}
	} else {
		primary := aggregate.Actions[running]
		if commit.ActionID != primary.ID || commit.ExpectedActionVersion != primary.Version || commit.NextActionState != kernel.ActionCancelled {
			t.Fatalf("commit primary action = %q@%d->%q; want %q@%d->cancelled",
				commit.ActionID, commit.ExpectedActionVersion, commit.NextActionState, primary.ID, primary.Version)
		}
	}
	pending := make(map[id.ActionID]uint64)
	for _, action := range aggregate.Actions {
		if action.State == kernel.ActionPending {
			pending[action.ID] = action.Version
		}
	}
	if len(commit.PendingCancellations) != len(pending) {
		t.Fatalf("pending cancellations = %+v; want every pending action %v", commit.PendingCancellations, pending)
	}
	for _, update := range commit.PendingCancellations {
		version, ok := pending[update.ActionID]
		if !ok {
			t.Fatalf("pending cancellation names %q, not a pending aggregate action", update.ActionID)
		}
		if update.ExpectedVersion != version || update.NextState != kernel.ActionCancelled {
			t.Fatalf("pending cancellation %+v; want %q@%d->cancelled", update, update.ActionID, version)
		}
	}
}

type wantLifecycleEvent struct {
	eventType      event.Type
	subjectID      string
	subjectVersion uint64
}

func assertCancellationEvents(t *testing.T, events []event.Envelope, owner event.Type, aggregate kernel.Execution) {
	t.Helper()
	want := []wantLifecycleEvent{{eventType: owner, subjectID: string(taskID), subjectVersion: 5}}
	if !aggregate.State.Terminal() {
		want = append(want,
			wantLifecycleEvent{eventType: event.TypeExecutionCancelled, subjectID: string(aggregate.ID), subjectVersion: aggregate.Version + 1},
			wantLifecycleEvent{eventType: event.TypeCleanup, subjectID: string(aggregate.ID), subjectVersion: aggregate.Version + 1},
		)
		for _, action := range aggregate.Actions {
			if action.State != kernel.ActionRunning && action.State != kernel.ActionPending {
				continue
			}
			want = append(want, wantLifecycleEvent{
				eventType:      event.TypeActionTransition,
				subjectID:      string(action.ID),
				subjectVersion: action.Version + 1,
			})
		}
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v; want %v", eventTypes(events), want)
	}
	for index, expected := range want {
		envelope := events[index]
		if envelope.Type != expected.eventType || envelope.SubjectID != expected.subjectID || envelope.SubjectVersion != expected.subjectVersion {
			t.Fatalf("event %d = %q %q@%d; want %q %q@%d", index,
				envelope.Type, envelope.SubjectID, envelope.SubjectVersion,
				expected.eventType, expected.subjectID, expected.subjectVersion)
		}
		if !envelope.At.Equal(testNow) {
			t.Fatalf("event %d time = %v; want %v", index, envelope.At, testNow)
		}
		if envelope.Scope != projectScope() {
			t.Fatalf("event %d scope = %+v; want %+v", index, envelope.Scope, projectScope())
		}
		if envelope.ID == "" {
			t.Fatalf("event %d has no deterministic identity", index)
		}
	}
}

func assertOwnerOnlyEvents(t *testing.T, events []event.Envelope, owner event.Type) {
	t.Helper()
	if len(events) != 1 || events[0].Type != owner || events[0].SubjectID != string(taskID) || events[0].SubjectVersion != 5 {
		t.Fatalf("events = %v; want only the owner event %q", eventTypes(events), owner)
	}
}

// TestTaskCancellationEventIDsAreDeterministicAcrossReplay proves the appended
// projection derives stable ids from the committed versions, so a replayed
// build publishes the same lifecycle identities.
func TestTaskCancellationEventIDsAreDeterministicAcrossReplay(t *testing.T) {
	first := cancelUnitFor(t, runningContainerAggregate())
	second := cancelUnitFor(t, runningContainerAggregate())
	if len(first.events) != len(second.events) {
		t.Fatalf("event counts differ: %d vs %d", len(first.events), len(second.events))
	}
	for index := range first.events {
		if first.events[index].ID != second.events[index].ID {
			t.Fatalf("event %d id = %q on the first build and %q on the replay", index, first.events[index].ID, second.events[index].ID)
		}
	}
}

// TestTaskCancellationRefusesInvalidAggregate proves a unit that disagrees with
// the aggregate never reaches the port.
func TestTaskCancellationRefusesInvalidAggregate(t *testing.T) {
	t.Run("aggregate identity disagrees with the task pointer", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(baseTask())
		env.executions.put(task.ExecutionView{ID: executionOne, Version: 7, State: kernel.ExecutionRunning})
		wrong := runningContainerAggregate()
		wrong.ID = executionTwo
		env.executions.setLoad(func(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error) {
			return wrong, nil
		})

		_, err := env.service.Cancel(t.Context(), cancelCommand(humanActor(operatorPrincipal, tenancy.RoleOperator), 4))
		requireFaultClass(t, err, fault.InvalidInput)
		requireNoPortCalls(t, env.ports)
	})

	t.Run("reduction refuses more than one running action", func(t *testing.T) {
		env := newTestEnv(t)
		aggregate := runningContainerAggregate()
		aggregate.Actions[1].State = kernel.ActionRunning
		aggregate.Actions[1].Version = 2
		seedAggregate(env, aggregate)

		_, err := env.service.Cancel(t.Context(), cancelCommand(humanActor(operatorPrincipal, tenancy.RoleOperator), 4))
		requireFaultClass(t, err, fault.InvalidInput)
		requireNoPortCalls(t, env.ports)
	})
}

// TestTaskCancellationPropagatesAggregateReaderFailures proves the scoped
// aggregate read fails the operation closed: a missing aggregate for a known
// pointer is the uniform not_found, a dependency failure keeps its class, and
// neither reaches the commit port.
func TestTaskCancellationPropagatesAggregateReaderFailures(t *testing.T) {
	t.Run("missing aggregate for a known pointer", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(baseTask())
		env.executions.put(task.ExecutionView{ID: executionOne, Version: 7, State: kernel.ExecutionRunning})
		env.executions.removeAggregate(executionOne)

		_, err := env.service.Cancel(t.Context(), cancelCommand(humanActor(operatorPrincipal, tenancy.RoleOperator), 4))
		requireFaultClass(t, err, fault.NotFound)
		requireNoPortCalls(t, env.ports)
	})

	t.Run("dependency failure", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(baseTask())
		env.executions.put(task.ExecutionView{ID: executionOne, Version: 7, State: kernel.ExecutionRunning})
		env.executions.setLoad(func(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error) {
			return kernel.Execution{}, fault.New(fault.DependencyUnavailable, "aggregate store unavailable")
		})

		_, err := env.service.Ask(t.Context(), askCommand(humanActor(engineerPrincipal, tenancy.RoleEngineer), 4, "Question?", roleAssignment(t, tenancy.RoleReviewer)))
		requireFaultClass(t, err, fault.DependencyUnavailable)
		requireNoPortCalls(t, env.ports)
	})
}

// TestTaskCancellationCarriesExpectedVersionsForPortConflict proves the unit's
// expected versions are the loaded aggregate's, so a concurrent engine change
// is the port's version_conflict at the atomic boundary.
func TestTaskCancellationCarriesExpectedVersionsForPortConflict(t *testing.T) {
	env := newTestEnv(t)
	seedAggregate(env, runningContainerAggregate())
	env.ports.setCancel(func(unit task.CancelUnit) (task.CancelResult, error) {
		if unit.Commit == nil || unit.Commit.ExpectedExecutionVersion != 7 {
			t.Fatalf("unit commit = %+v; want expected version 7", unit.Commit)
		}
		return task.CancelResult{}, fault.New(fault.VersionConflict, "engine advanced the execution")
	})

	_, err := env.service.Cancel(t.Context(), cancelCommand(humanActor(operatorPrincipal, tenancy.RoleOperator), 4))
	requireFaultClass(t, err, fault.VersionConflict)
	if len(env.ports.cancels) != 1 {
		t.Fatalf("cancel port calls = %d; want the port to re-check the version", len(env.ports.cancels))
	}
}

// TestTaskCancellationUsesCommandScopeForAggregateRead pins the scoped read:
// the task service never guesses another scope.
func TestTaskCancellationUsesCommandScopeForAggregateRead(t *testing.T) {
	env := newTestEnv(t)
	seedAggregate(env, queuedAggregate())
	_, err := env.service.Cancel(t.Context(), cancelCommand(humanActor(operatorPrincipal, tenancy.RoleOperator), 4))
	requireNoError(t, err, "Cancel")
	scopes := env.executions.loadedScopes()
	if len(scopes) != 1 || scopes[0] != projectScope() {
		t.Fatalf("aggregate read scopes = %+v; want %+v", scopes, projectScope())
	}
}
