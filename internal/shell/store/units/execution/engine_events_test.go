package executionstore

// The engine commit path is the only emitter of the execution/action lifecycle
// classes (emitter ownership): the cross-scope controller seam commits the
// projected events atomically with the Kernel state writes. These tests drive
// the real Kernel reconciler (internal/kernel/engine over the deterministic
// testkit runtime/workspace/clock fakes) and the real reducer through the
// adapter over the memory store, then read the committed events back through
// the scoped executor.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// literalRequestAction is one request action whose subject is fully resolvable
// at activation, so it can be the first action of an execution.
func literalRequestAction() pipeline.AdmittedAction {
	return pipeline.AdmittedAction{
		Index:   0,
		Name:    "approve",
		Kind:    definition.ActionKindRequest,
		Timeout: 60 * time.Second,
		Request: &definition.Request{
			Assignment: "role/reviewer",
			Subject: definition.NewSubject(map[string]definition.SubjectValue{
				"artifact": {Kind: definition.SubjectLiteral, Literal: "artifact-1"},
			}),
			Outputs: []id.OutputKey{"decision"},
		},
	}
}

// engineRun wires the real Kernel reconciler over this package's engine adapter
// and the deterministic Kernel fakes.
type engineRun struct {
	reconciler *engine.Engine
	adapter    *Store
	backend    *memory.Store
	runtime    *testkit.Runtime
	workspace  *testkit.Workspace
	clock      *testkit.Clock
}

func newEngineRun(t *testing.T) engineRun {
	t.Helper()
	clock := testkit.NewClock(testNow)
	backend := memory.NewWithClock(func() time.Time { return clock.Now() })
	adapter := New(backend, backend)
	runtime := testkit.NewRuntime()
	runtime.SetCapabilities("process")
	workspace := testkit.NewWorkspace()
	workspace.SetGuard(runtime.OwnershipHeld)
	reconciler := engine.New(NewEngine(backend.Controller()), runtime, workspace, clock)
	return engineRun{
		reconciler: reconciler,
		adapter:    adapter,
		backend:    backend,
		runtime:    runtime,
		workspace:  workspace,
		clock:      clock,
	}
}

// committedTypes returns the committed event types visible in scope in
// publication-sequence order.
func committedTypes(backend *memory.Store, scope tenancy.Scope) []event.Type {
	committed := backend.Events(scope)
	types := make([]event.Type, len(committed))
	for index, record := range committed {
		types[index] = record.Envelope.Type
	}
	return types
}

// countType returns how many committed events of one type are visible in scope.
func countType(backend *memory.Store, scope tenancy.Scope, eventType event.Type) int {
	count := 0
	for _, record := range backend.Events(scope) {
		if record.Envelope.Type == eventType {
			count++
		}
	}
	return count
}

// onlyType returns the committed envelopes of one type in publication order.
func onlyType(backend *memory.Store, scope tenancy.Scope, eventType event.Type) []event.Envelope {
	var envelopes []event.Envelope
	for _, record := range backend.Events(scope) {
		if record.Envelope.Type == eventType {
			envelopes = append(envelopes, record.Envelope)
		}
	}
	return envelopes
}

// assertScopedEventRead proves every committed event is readable through the
// scoped executor in the execution's scope, and nowhere else.
func assertScopedEventRead(t *testing.T, backend *memory.Store, scope tenancy.Scope, foreign tenancy.Scope) {
	t.Helper()
	for _, record := range backend.Events(scope) {
		row, err := backend.Read(context.Background(), scope, "events", string(record.Envelope.ID))
		if err != nil {
			t.Fatalf("scoped read of event %q: %v", record.Envelope.ID, err)
		}
		if row.Values["type"] != string(record.Envelope.Type) {
			t.Fatalf("event row type = %v; want %q", row.Values["type"], record.Envelope.Type)
		}
		if _, err := backend.Read(context.Background(), foreign, "events", string(record.Envelope.ID)); err == nil {
			t.Fatalf("event %q was readable from a foreign scope", record.Envelope.ID)
		}
	}
}

// TestEngineCommitPublishesTheFullRunLifecycleOnceInOrder drives the real Kernel
// reducer for a container execution that starts, succeeds and settles the
// execution, and asserts the engine commit path publishes each lifecycle class
// exactly once in the projection's total order.
func TestEngineCommitPublishesTheFullRunLifecycleOnceInOrder(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	if types := committedTypes(backend, scope); len(types) != 1 || types[0] != event.TypeExecutionQueued {
		t.Fatalf("start events = %v; want exactly the queued event", types)
	}

	// queued -> running: execution.started then the activation action.transition.
	current := loadExecution(t, adapter, scope, "exec-1")
	_, activation, err := flow.Reduce(current, flow.ActivateFact{
		ExecutionID: "exec-1",
		ActionID:    actions[0],
		At:          testNow,
	})
	if err != nil || activation == nil {
		t.Fatalf("reduce activation: %v %+v", err, activation)
	}
	if _, err := engineState.Commit(context.Background(), *activation); err != nil {
		t.Fatalf("engine activation commit: %v", err)
	}
	if got := committedTypes(backend, scope); !equalTypes(got, []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
	}) {
		t.Fatalf("after activation events = %v", got)
	}

	// running action succeeds and settles the execution: execution.succeeded,
	// the pending execution.cleanup travelling in the same terminal commit,
	// then the success action.transition.
	current = loadExecution(t, adapter, scope, "exec-1")
	_, success, err := flow.Reduce(current, flow.ObserveFact{
		ExecutionID: "exec-1",
		ActionID:    actions[0],
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          testNow.Add(time.Minute),
	})
	if err != nil || success == nil {
		t.Fatalf("reduce success: %v %+v", err, success)
	}
	if _, err := engineState.Commit(context.Background(), *success); err != nil {
		t.Fatalf("engine success commit: %v", err)
	}

	want := []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
		event.TypeExecutionSucceeded,
		event.TypeCleanup,
		event.TypeActionTransition,
	}
	if got := committedTypes(backend, scope); !equalTypes(got, want) {
		t.Fatalf("full run events = %v; want %v", got, want)
	}
	for _, eventType := range []event.Type{
		event.TypeExecutionStarted,
		event.TypeExecutionSucceeded,
		event.TypeActionTransition,
	} {
		if got := countType(backend, scope, eventType); got != 1 && !(eventType == event.TypeActionTransition && got == 2) {
			t.Fatalf("%s count = %d; want exactly one lifecycle transition per class", eventType, got)
		}
	}

	// Subject identity/version: the execution events name the committed
	// execution version, the action events the committed action version.
	started := onlyType(backend, scope, event.TypeExecutionStarted)[0]
	if started.SubjectID != "exec-1" || started.SubjectVersion != 2 {
		t.Fatalf("execution.started subject = %q@%d; want exec-1@2", started.SubjectID, started.SubjectVersion)
	}
	succeeded := onlyType(backend, scope, event.TypeExecutionSucceeded)[0]
	if succeeded.SubjectID != "exec-1" || succeeded.SubjectVersion != 3 {
		t.Fatalf("execution.succeeded subject = %q@%d; want exec-1@3", succeeded.SubjectID, succeeded.SubjectVersion)
	}
	transitions := onlyType(backend, scope, event.TypeActionTransition)
	if transitions[0].SubjectID != string(actions[0]) || transitions[0].SubjectVersion != 2 {
		t.Fatalf("activation transition subject = %q@%d", transitions[0].SubjectID, transitions[0].SubjectVersion)
	}
	if transitions[1].SubjectID != string(actions[0]) || transitions[1].SubjectVersion != 3 {
		t.Fatalf("success transition subject = %q@%d", transitions[1].SubjectID, transitions[1].SubjectVersion)
	}
	if started.Source != event.SystemSource || !started.At.Equal(testNow) {
		t.Fatalf("event source/time = %+v/%s; want the trusted system producer at %s", started.Source, started.At, testNow)
	}

	assertScopedEventRead(t, backend, scope, otherScope())
}

// TestEngineReconcilePublishesLifecycleThroughTheAdapter drives the real Kernel
// reconciler over the testkit fakes through a complete container run and asserts
// the adapter publishes the lifecycle classes once plus the terminal cleanup
// progress.
func TestEngineReconcilePublishesLifecycleThroughTheAdapter(t *testing.T) {
	ctx := context.Background()
	run := newEngineRun(t)
	scope := projectScope()
	startedExecution(t, run.adapter, run.backend, scope, "exec-1", containerAction())

	if _, err := run.reconciler.Reconcile(ctx, "exec-1"); err != nil {
		t.Fatalf("Reconcile activation: %v", err)
	}
	running := loadExecution(t, run.adapter, scope, "exec-1")
	if running.State != kernel.ExecutionRunning || running.CurrentAction != 0 {
		t.Fatalf("activation state = %+v; want running current action 0", running)
	}
	if running.Actions[0].Container == nil || running.Actions[0].Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("activation launch = %+v; want an issued identity", running.Actions[0].Container)
	}
	if got := countType(run.backend, scope, event.TypeExecutionStarted); got != 1 {
		t.Fatalf("execution.started count = %d; want 1", got)
	}
	if got := countType(run.backend, scope, event.TypeActionTransition); got != 1 {
		t.Fatalf("activation action.transition count = %d; want 1", got)
	}

	// The fake runtime reports a confirmed exit, so the real reconciler settles
	// the action and execution, commits the pending cleanup with the terminal
	// outcome, then drives that cleanup to completion.
	workloadID := running.Actions[0].Container.WorkloadID
	run.runtime.ConfirmNextStop("termination observed")
	if _, err := run.runtime.Stop(ctx, engine.StopRequest{
		Identity:    engine.EffectIdentity{ExecutionID: "exec-1", ActionID: running.Actions[0].ID, WorkloadID: workloadID},
		WorkspaceID: running.WorkspaceID,
	}); err != nil {
		t.Fatalf("confirm workload stop: %v", err)
	}
	run.runtime.QueueObservation(workloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")))
	if _, err := run.reconciler.Reconcile(ctx, "exec-1"); err != nil {
		t.Fatalf("Reconcile success/cleanup: %v", err)
	}
	settled := loadExecution(t, run.adapter, scope, "exec-1")
	if settled.State != kernel.ExecutionSucceeded {
		t.Fatalf("settled state = %q; want succeeded", settled.State)
	}
	if settled.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q; want complete", settled.Cleanup.State)
	}

	want := []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
		event.TypeExecutionSucceeded,
		event.TypeCleanup,
		event.TypeActionTransition,
		event.TypeCleanup,
	}
	if got := committedTypes(run.backend, scope); !equalTypes(got, want) {
		t.Fatalf("reconciled run events = %v; want %v", got, want)
	}
	if got := countType(run.backend, scope, event.TypeExecutionStarted); got != 1 {
		t.Fatalf("execution.started count = %d; want 1", got)
	}
	if got := countType(run.backend, scope, event.TypeExecutionSucceeded); got != 1 {
		t.Fatalf("execution.succeeded count = %d; want 1", got)
	}
	if got := countType(run.backend, scope, event.TypeActionTransition); got != 2 {
		t.Fatalf("action.transition count = %d; want 2", got)
	}
	if got := countType(run.backend, scope, event.TypeCleanup); got != 2 {
		t.Fatalf("execution.cleanup count = %d; want 2 (pending then complete)", got)
	}
	if got := countType(run.backend, scope, event.TypeRequestActivated); got != 0 {
		t.Fatalf("request.activated count = %d; want the internal alias never projected", got)
	}
	assertScopedEventRead(t, run.backend, scope, otherScope())
}

// TestEngineRequestActivationPublishesActionRequestedOnce proves a request
// action activated by the engine publishes the public action.requested exactly
// once, after its action.transition, and a second reconcile publishes nothing.
func TestEngineRequestActivationPublishesActionRequestedOnce(t *testing.T) {
	ctx := context.Background()
	run := newEngineRun(t)
	scope := projectScope()
	actions := startedExecution(t, run.adapter, run.backend, scope, "exec-req", literalRequestAction())

	if _, err := run.reconciler.Reconcile(ctx, "exec-req"); err != nil {
		t.Fatalf("Reconcile request activation: %v", err)
	}
	want := []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
		event.TypeActionRequested,
	}
	if got := committedTypes(run.backend, scope); !equalTypes(got, want) {
		t.Fatalf("request activation events = %v; want %v", got, want)
	}
	requested := onlyType(run.backend, scope, event.TypeActionRequested)
	if len(requested) != 1 {
		t.Fatalf("action.requested count = %d; want exactly 1", len(requested))
	}
	if requested[0].SubjectID != string(actions[0]) || requested[0].SubjectVersion != 2 {
		t.Fatalf("action.requested subject = %q@%d", requested[0].SubjectID, requested[0].SubjectVersion)
	}
	if got := countType(run.backend, scope, event.TypeRequestActivated); got != 0 {
		t.Fatalf("request.activated count = %d; want 0", got)
	}

	// A request waits for an authorized Shell settlement: another reconcile
	// pass must not re-publish the activation.
	if _, err := run.reconciler.Reconcile(ctx, "exec-req"); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := countType(run.backend, scope, event.TypeActionRequested); got != 1 {
		t.Fatalf("action.requested count after second reconcile = %d; want 1", got)
	}
	if got := countType(run.backend, scope, event.TypeActionTransition); got != 1 {
		t.Fatalf("action.transition count after second reconcile = %d; want 1", got)
	}
}

// TestEngineFailureAndCancellationPublishOnce proves the engine-driven failure
// and cancellation paths publish execution.failed / execution.cancelled exactly
// once, and that cancellation publishes its committed cleanup progress.
func TestEngineFailureAndCancellationPublishOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("failure", func(t *testing.T) {
		run := newEngineRun(t)
		scope := projectScope()
		startedExecution(t, run.adapter, run.backend, scope, "exec-fail", containerAction())
		if _, err := run.reconciler.Reconcile(ctx, "exec-fail"); err != nil {
			t.Fatalf("Reconcile activation: %v", err)
		}
		running := loadExecution(t, run.adapter, scope, "exec-fail")
		run.runtime.QueueObservation(running.Actions[0].Container.WorkloadID,
			kernel.ExitedObservation(1, kernel.ConfirmedStop("termination observed")))
		if _, err := run.reconciler.Reconcile(ctx, "exec-fail"); err != nil {
			t.Fatalf("Reconcile failure: %v", err)
		}
		settled := loadExecution(t, run.adapter, scope, "exec-fail")
		if settled.State != kernel.ExecutionFailed {
			t.Fatalf("settled state = %q; want failed", settled.State)
		}
		if got := countType(run.backend, scope, event.TypeExecutionFailed); got != 1 {
			t.Fatalf("execution.failed count = %d; want 1", got)
		}
		if got := countType(run.backend, scope, event.TypeExecutionSucceeded); got != 0 {
			t.Fatalf("execution.succeeded count = %d; want 0 on a failure", got)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		run := newEngineRun(t)
		scope := projectScope()
		startedExecution(t, run.adapter, run.backend, scope, "exec-cancel", containerAction())
		if _, err := run.reconciler.Reconcile(ctx, "exec-cancel"); err != nil {
			t.Fatalf("Reconcile activation: %v", err)
		}
		if err := run.reconciler.Cancel(ctx, "exec-cancel", flow.CancelRequested); err != nil {
			t.Fatalf("engine Cancel: %v", err)
		}
		cancelled := loadExecution(t, run.adapter, scope, "exec-cancel")
		if cancelled.State != kernel.ExecutionCancelled {
			t.Fatalf("state = %q; want cancelled", cancelled.State)
		}
		if got := countType(run.backend, scope, event.TypeExecutionCancelled); got != 1 {
			t.Fatalf("execution.cancelled count = %d; want 1", got)
		}
		if got := countType(run.backend, scope, event.TypeCleanup); got < 1 {
			t.Fatalf("execution.cleanup count = %d; want the committed pending progress", got)
		}
		if got := countType(run.backend, scope, event.TypeExecutionFailed); got != 0 {
			t.Fatalf("execution.failed count = %d; want 0 on a cancellation", got)
		}
	})
}

// TestEngineStaleAndDuplicateCommitPublishNothing proves a replay of an applied
// commit (or any stale expected version) is the unit's version_conflict and
// publishes no second event.
func TestEngineStaleAndDuplicateCommitPublishNothing(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	current := loadExecution(t, adapter, scope, "exec-1")
	_, activation, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: "exec-1", ActionID: actions[0], At: testNow})
	if err != nil || activation == nil {
		t.Fatalf("reduce activation: %v %+v", err, activation)
	}
	if _, err := engineState.Commit(context.Background(), *activation); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	before := committedTypes(backend, scope)

	_, err = engineState.Commit(context.Background(), *activation)
	requireClass(t, err, fault.VersionConflict)
	if got := committedTypes(backend, scope); !equalTypes(got, before) {
		t.Fatalf("replayed commit published extra events: %v -> %v", before, got)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("replayed commit changed the execution: %+v", aggregate)
	}
}

// TestEngineCommitIsAtomicWhenTheUnitFails injects a failure after the unit is
// fully staged: neither the state writes nor the event rows become visible, so a
// crash between state and events cannot leave one without the other.
func TestEngineCommitIsAtomicWhenTheUnitFails(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	current := loadExecution(t, adapter, scope, "exec-1")
	_, activation, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: "exec-1", ActionID: actions[0], At: testNow})
	if err != nil || activation == nil {
		t.Fatalf("reduce activation: %v %+v", err, activation)
	}

	injected := fault.New(fault.InternalFailure, "injected commit failure")
	backend.FailNextCommit(injected)
	_, err = engineState.Commit(context.Background(), *activation)
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("commit error = %v; want the injected failure", err)
	}

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued {
		t.Fatalf("failed unit changed the execution: %+v", aggregate)
	}
	if aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("failed unit changed the action: %+v", aggregate.Actions[0])
	}
	if got := committedTypes(backend, scope); !equalTypes(got, []event.Type{event.TypeExecutionQueued}) {
		t.Fatalf("failed unit published events: %v", got)
	}

	// A retry after the injected failure commits state and events together.
	if _, err := engineState.Commit(context.Background(), *activation); err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if got := committedTypes(backend, scope); !equalTypes(got, []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
	}) {
		t.Fatalf("retry events = %v", got)
	}
}

// TestEngineEventsAreSecretFree proves the projected facts carry only ids,
// states, versions, kinds and bounded reason classes: staged outputs and the
// free-form failure detail never reach an event row.
func TestEngineEventsAreSecretFree(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	current := loadExecution(t, adapter, scope, "exec-1")
	_, activation, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: "exec-1", ActionID: actions[0], At: testNow})
	if err != nil || activation == nil {
		t.Fatalf("reduce activation: %v %+v", err, activation)
	}
	if _, err := engineState.Commit(context.Background(), *activation); err != nil {
		t.Fatalf("activation commit: %v", err)
	}

	const outputSecret = "super-secret-output-value"
	current = loadExecution(t, adapter, scope, "exec-1")
	_, staged, err := flow.Reduce(current, flow.OutputFact{
		ExecutionID: "exec-1",
		ActionID:    actions[0],
		Outputs:     kernel.Outputs{"artifact": outputSecret},
		At:          testNow,
	})
	if err != nil || staged == nil {
		t.Fatalf("reduce output staging: %v %+v", err, staged)
	}
	if _, err := engineState.Commit(context.Background(), *staged); err != nil {
		t.Fatalf("stage outputs commit: %v", err)
	}
	current = loadExecution(t, adapter, scope, "exec-1")
	_, success, err := flow.Reduce(current, flow.ObserveFact{
		ExecutionID: "exec-1",
		ActionID:    actions[0],
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
		At:          testNow.Add(time.Minute),
	})
	if err != nil || success == nil {
		t.Fatalf("reduce success: %v %+v", err, success)
	}
	if _, err := engineState.Commit(context.Background(), *success); err != nil {
		t.Fatalf("success commit: %v", err)
	}

	const failureSecret = "super-secret-failure-detail"
	actions2 := startedExecution(t, adapter, backend, scope, "exec-2", containerAction())
	current = loadExecution(t, adapter, scope, "exec-2")
	_, activation2, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: "exec-2", ActionID: actions2[0], At: testNow})
	if err != nil || activation2 == nil {
		t.Fatalf("reduce activation 2: %v %+v", err, activation2)
	}
	if _, err := engineState.Commit(context.Background(), *activation2); err != nil {
		t.Fatalf("activation 2 commit: %v", err)
	}
	current = loadExecution(t, adapter, scope, "exec-2")
	nextCurrent := -1
	failure := flow.Commit{
		ExecutionID:              "exec-2",
		ExpectedExecutionVersion: current.Version,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 actions2[0],
		ExpectedActionVersion:    current.Actions[0].Version,
		NextActionState:          kernel.ActionFailed,
		NextCurrentAction:        &nextCurrent,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Failure: &kernel.Failure{
			Kind:   kernel.FailureRuntime,
			Reason: "workload_exited",
			Detail: failureSecret,
		},
		At: testNow.Add(time.Minute),
	}
	if _, err := engineState.Commit(context.Background(), failure); err != nil {
		t.Fatalf("failure commit: %v", err)
	}

	for _, record := range backend.Events(scope) {
		data := string(record.Envelope.Data)
		if strings.Contains(data, outputSecret) {
			t.Fatalf("event %q leaked staged output content: %s", record.Envelope.Type, data)
		}
		if strings.Contains(data, failureSecret) {
			t.Fatalf("event %q leaked the failure detail: %s", record.Envelope.Type, data)
		}
		for _, forbidden := range []string{"token", "nonce", "credential", "secret"} {
			if strings.Contains(strings.ToLower(data), forbidden) {
				t.Fatalf("event %q carries a credential-bearing key %q: %s", record.Envelope.Type, forbidden, data)
			}
		}
	}
	failed := onlyType(backend, scope, event.TypeExecutionFailed)
	if len(failed) != 1 || !strings.Contains(string(failed[0].Data), "workload_exited") {
		t.Fatalf("failure event = %v; want the bounded reason class", failed)
	}
}

// TestEngineConcurrentCommitsPublishOneSetOfEvents proves two engine commits on
// one execution serialize: exactly one wins, the loser is version_conflict, and
// only the winner's event set is committed.
func TestEngineConcurrentCommitsPublishOneSetOfEvents(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	current := loadExecution(t, adapter, scope, "exec-1")
	_, activation, err := flow.Reduce(current, flow.ActivateFact{ExecutionID: "exec-1", ActionID: actions[0], At: testNow})
	if err != nil || activation == nil {
		t.Fatalf("reduce activation: %v %+v", err, activation)
	}

	var wait sync.WaitGroup
	errs := make([]error, 2)
	for index := range errs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errs[index] = engineState.Commit(context.Background(), *activation)
		}(index)
	}
	wait.Wait()

	successes := 0
	conflicts := 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			conflicts++
			continue
		}
		t.Fatalf("unexpected commit error: %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent commits: %d success, %d conflict; want exactly one winner", successes, conflicts)
	}
	want := []event.Type{
		event.TypeExecutionQueued,
		event.TypeExecutionStarted,
		event.TypeActionTransition,
	}
	if got := committedTypes(backend, scope); !equalTypes(got, want) {
		t.Fatalf("concurrent commit events = %v; want one winner's %v", got, want)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("execution version = %d; want exactly one applied commit", aggregate.Version)
	}
}

// equalTypes compares two event type sequences.
func equalTypes(got, want []event.Type) bool {
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
