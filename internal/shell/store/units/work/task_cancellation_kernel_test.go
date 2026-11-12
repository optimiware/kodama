package work

// Both task cancel paths (`task.question`/AskUnit and
// `task.cancel`/CancelUnit) route the task's current execution through the
// Kernel cancel reduction in ONE atomic receipt-first store unit. These tests
// drive the REAL task service over the REAL work adapter and the REAL
// execution adapter on the memory store, with the Kernel engine and the fake
// runtime, and prove:
//
//   - a running container cancel commits the Kernel reduction: execution
//     cancelled, cleanup pending, the action cancelled and the reducer's
//     conditional StopIntent carried in the unit;
//   - a queued predecessor cancels with no StopIntent; a running request
//     cancels with no StopIntent;
//   - a terminal predecessor is preserved byte-identically with no Commit;
//   - the adapter never writes executions.state directly: a live predecessor
//     without a Kernel Commit is refused invalid_input;
//   - the engine stops the workload and completes cleanup, and replacement
//     dispatch waits for confirmed stop;
//   - concurrent cancel/settlement, ask/cancel and reply/stop races have one
//     winner and no partial state;
//   - a lost-response replay returns the original result with no re-apply;
//   - a failed commit rolls the whole unit back.

import (
	"context"
	"reflect"
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
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// requestIntegrationAdmission admits one request action so the Kernel can
// activate a request wait instead of a container workload.
type requestIntegrationAdmission struct {
	pipelines *Pipelines
	scope     tenancy.Scope
}

func (admission requestIntegrationAdmission) Admit(ctx context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	current, err := admission.pipelines.Get(ctx, admission.scope, request.Definition.ID)
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         current.YAML,
		Inputs:       request.Inputs,
		TaskID:       request.Task.TaskID,
		Conversation: request.Task.Conversation,
	})
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	requestAction := &definition.Request{
		Assignment: "role/reviewer",
		Subject: definition.NewSubject(map[string]definition.SubjectValue{
			"ref": {Kind: definition.SubjectLiteral, Literal: "main"},
		}),
		Outputs: []id.OutputKey{"decision"},
	}
	return pipeline.AdmissionResult{
		PipelineID:      current.ID,
		PipelineVersion: current.Version,
		Snapshot:        snapshot,
		Actions: []pipeline.AdmittedAction{{
			Index:   0,
			Name:    id.ActionName("approve"),
			Kind:    definition.ActionKindRequest,
			Timeout: time.Minute,
			Request: requestAction,
		}},
	}, nil
}

// cancellationEngine wires the Kernel engine over the execution adapter and
// the fake runtime beside one real-service environment.
type cancellationEngine struct {
	env       *realServiceEnv
	engine    *engine.Engine
	runtime   *testkit.Runtime
	workspace *testkit.Workspace
}

func newCancellationEngineEnv(t *testing.T) *cancellationEngine {
	t.Helper()
	return newCancellationEngineEnvWith(t, nil)
}

func newCancellationEngineEnvWith(t *testing.T, admission func(*fixture, tenancy.Scope) task.Admission) *cancellationEngine {
	t.Helper()
	env := newRealServiceEnvWith(t, admission)
	runtime := testkit.NewRuntime()
	workspace := testkit.NewWorkspace()
	reconciler := engine.New(
		executionstore.NewEngine(env.fixture.store.Controller()),
		runtime,
		workspace,
		integrationClock{now: testNow},
	)
	return &cancellationEngine{env: env, engine: reconciler, runtime: runtime, workspace: workspace}
}

// createAndStart admits one queued execution for a fresh task.
func (env *cancellationEngine) createAndStart(t *testing.T, caller string) task.CreateResult {
	t.Helper()
	created, err := env.env.service.Create(context.Background(), task.CreateCommand{
		Actor:      env.env.actor(),
		Scope:      env.env.scope,
		Title:      "Task " + caller,
		PipelineID: testPipeline,
		Start:      true,
		CallerKey:  caller,
		Content:    integrationContent(caller),
	})
	if err != nil {
		t.Fatalf("create-and-start %q: %v", caller, err)
	}
	return created
}

// reconcile drives the Kernel engine until it needs new evidence.
func (env *cancellationEngine) reconcile(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	execution, err := env.engine.Reconcile(context.Background(), executionID)
	if err != nil {
		t.Fatalf("engine.Reconcile(%q): %v", executionID, err)
	}
	return execution
}

// load reads the committed aggregate through the execution adapter.
func (env *cancellationEngine) load(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	execution, err := executionstore.New(env.env.fixture.store, env.env.fixture.store).
		Load(context.Background(), env.env.scope, executionID)
	if err != nil {
		t.Fatalf("load execution %q: %v", executionID, err)
	}
	return execution
}

// startRunningTask creates a task, starts it, and drives the engine until the
// container workload is running.
func (env *cancellationEngine) startRunningTask(t *testing.T, caller string) (task.CreateResult, kernel.Execution) {
	t.Helper()
	env.env.fixture.seedPipeline(t, env.env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created := env.createAndStart(t, caller)
	running := env.reconcile(t, created.ExecutionID)
	if running.State != kernel.ExecutionRunning || running.CurrentAction != 0 {
		t.Fatalf("execution %q = %s current %d; want running with action 0",
			created.ExecutionID, running.State, running.CurrentAction)
	}
	return created, running
}

// settleSucceeded drives the running container to a confirmed successful exit.
func (env *cancellationEngine) settleSucceeded(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	current := env.load(t, executionID)
	workload := current.Actions[current.CurrentAction].Container.WorkloadID
	env.runtime.QueueObservation(workload, kernel.ExitedObservation(0, kernel.ConfirmedStop("exit 0")))
	settled := env.reconcile(t, executionID)
	if settled.State != kernel.ExecutionSucceeded {
		t.Fatalf("settled execution = %s; want succeeded", settled.State)
	}
	return settled
}

// settleFailed drives the running container to a non-zero exit.
func (env *cancellationEngine) settleFailed(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	current := env.load(t, executionID)
	workload := current.Actions[current.CurrentAction].Container.WorkloadID
	env.runtime.QueueObservation(workload, kernel.ExitedObservation(1, kernel.ConfirmedStop("exit 1")))
	settled := env.reconcile(t, executionID)
	if settled.State != kernel.ExecutionFailed {
		t.Fatalf("settled execution = %s; want failed", settled.State)
	}
	return settled
}

// settleCancelled cancels the running execution through the Kernel engine,
// leaving the task untouched.
func (env *cancellationEngine) settleCancelled(t *testing.T, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	if err := env.engine.Cancel(context.Background(), executionID, flow.CancelRequested); err != nil {
		t.Fatalf("engine.Cancel(%q): %v", executionID, err)
	}
	settled := env.load(t, executionID)
	if settled.State != kernel.ExecutionCancelled {
		t.Fatalf("settled execution = %s; want cancelled", settled.State)
	}
	return settled
}

// eventCount counts committed events of one canonical type.
func eventCount(t *testing.T, f *fixture, scope tenancy.Scope, eventType event.Type) int {
	t.Helper()
	return f.countRows(t, scope, "events", map[string]any{"type": string(eventType)})
}

// actionRows reads every action row of one execution in index order.
func actionRows(t *testing.T, f *fixture, scope tenancy.Scope, executionID id.ExecutionID) []store.Row {
	t.Helper()
	rows, err := scanAll(context.Background(), f.store, scope, store.ScanQuery{
		Table:   "actions",
		Equals:  map[string]any{"execution_id": string(executionID)},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		t.Fatalf("scan actions: %v", err)
	}
	return rows
}

// cleanupRow reads the durable cleanup row of one execution, if any.
func cleanupRow(t *testing.T, f *fixture, scope tenancy.Scope, executionID id.ExecutionID) (store.Row, bool) {
	t.Helper()
	return f.scanRow(t, scope, "cleanup", map[string]any{"execution_id": string(executionID)})
}

// manualReceipt builds one valid receipt with a unique identity and key, so a
// hand-built unit never collides with the service-allocated receipts.
func manualReceipt(scope tenancy.Scope, operation, caller string) receipt.Record {
	return receipt.Record{
		ID: id.ReceiptID("receipt-manual-" + caller),
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: testPrincipal,
			Operation:   operation,
			CallerKey:   caller,
		},
		ContentHash: contentHashA,
		At:          testNow,
	}
}

// manualEvent builds one valid event with a unique identity.
func manualEvent(scope tenancy.Scope, identity, subject string) event.Envelope {
	return event.Envelope{
		ID:             id.EventID("event-manual-" + identity),
		Type:           event.TypeExecutionQueued,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: testPrincipal},
		At:             testNow,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: 3,
	}
}

// hasPostCommitDispatch reports whether one unit carries a dispatch effect.
func hasPostCommitDispatch(effects []event.PostCommitEffect) bool {
	for _, effect := range effects {
		if effect.Kind == event.PostDispatch {
			return true
		}
	}
	return false
}

// requireNoDispatchEvent proves no dispatch effect was committed for a unit.
func requireNoDispatchEvent(t *testing.T, effects []event.PostCommitEffect) {
	t.Helper()
	if hasPostCommitDispatch(effects) {
		t.Fatalf("unit requested dispatch before confirmed stop: %+v", effects)
	}
}

// TestTaskCancelRunningContainerAppliesKernelCancel is case (a): a running
// container execution is cancelled through the Kernel reduction, the engine
// stops the workload, cleanup completes, and the task row/events change with
// the execution.
func TestTaskCancelRunningContainerAppliesKernelCancel(t *testing.T) {
	env := newCancellationEngineEnv(t)
	created, running := env.startRunningTask(t, "cancel-running")
	workload := running.Actions[0].Container.WorkloadID
	if workload == "" {
		t.Fatal("the started container carries no workload identity")
	}

	beforeCancelled := eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled)
	beforeAction := eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition)
	beforeCleanup := eventCount(t, env.env.fixture, env.env.scope, event.TypeCleanup)
	beforeTaskCancelled := eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskCancelled)
	result, err := env.env.service.Cancel(context.Background(), task.CancelCommand{
		Actor:                    env.env.actor(),
		Scope:                    env.env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: 2,
		CallerKey:                "cancel-running",
		Content:                  integrationContent("cancel-running"),
	})
	if err != nil {
		t.Fatalf("task cancel: %v", err)
	}
	if result.LifecycleVersion != 3 || result.Replayed {
		t.Fatalf("cancel result = %+v; want a fresh lifecycle version 3", result)
	}

	unit := env.env.ports.lastCancel()
	if unit.Commit == nil {
		t.Fatal("the cancel unit carries no Kernel commit")
	}
	if unit.ExpectedExecutionVersion != running.Version {
		t.Fatalf("expected execution version = %d; want %d", unit.ExpectedExecutionVersion, running.Version)
	}
	if unit.Commit.Stop == nil || unit.Commit.Stop.WorkloadID != workload {
		t.Fatalf("the cancel commit carries no StopIntent for %q: %+v", workload, unit.Commit.Stop)
	}

	execution, ok := env.env.fixture.storedRow(t, env.env.scope, "executions", string(created.ExecutionID))
	if !ok {
		t.Fatal("the cancelled execution disappeared")
	}
	if textValue(execution, "state") != string(kernel.ExecutionCancelled) {
		t.Fatalf("execution state = %q; want cancelled", textValue(execution, "state"))
	}
	if versionValue(execution, "version") != running.Version+1 {
		t.Fatalf("execution version = %d; want %d", versionValue(execution, "version"), running.Version+1)
	}
	actions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if len(actions) != 1 || textValue(actions[0], "state") != string(kernel.ActionCancelled) {
		t.Fatalf("action rows = %+v; want the running action cancelled", actions)
	}
	cleanup, ok := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if !ok {
		t.Fatal("the cancel committed no cleanup row")
	}
	if textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("cleanup state = %q; want pending", textValue(cleanup, "state"))
	}
	if textValue(cleanup, "workload_id") != string(workload) {
		t.Fatalf("cleanup workload = %q; want %q", textValue(cleanup, "workload_id"), workload)
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) != beforeCancelled+1 {
		t.Fatal("the cancel unit did not publish execution.cancelled exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition) != beforeAction+1 {
		t.Fatal("the cancel unit did not publish action.transition exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeCleanup) != beforeCleanup+1 {
		t.Fatal("the cancel unit did not publish execution.cleanup exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskCancelled) != beforeTaskCancelled+1 {
		t.Fatal("the cancel unit did not publish task.cancelled exactly once")
	}

	// The engine stops the workload and completes cleanup after confirmed stop.
	env.runtime.ConfirmNextStop("termination observed")
	final := env.reconcile(t, created.ExecutionID)
	if final.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q; want complete", final.Cleanup.State)
	}
	settled, ok := env.runtime.Workload(workload)
	if !ok || !settled.Removed {
		t.Fatalf("workload %q was not removed: %+v", workload, settled)
	}
	if settled.StopRequests == 0 {
		t.Fatal("the engine never requested the workload stop")
	}
}

// TestTaskAskRunningContainerAppliesKernelCancel is case (b): a question
// handoff cancels a running container through the Kernel path, persists the
// question, and the engine then stops the workload.
func TestTaskAskRunningContainerAppliesKernelCancel(t *testing.T) {
	env := newCancellationEngineEnv(t)
	created, running := env.startRunningTask(t, "ask-running")
	workload := running.Actions[0].Container.WorkloadID

	beforeQuestion := eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskQuestion)
	beforeCancelled := eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled)
	beforeAction := eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition)
	beforeCleanup := eventCount(t, env.env.fixture, env.env.scope, event.TypeCleanup)
	asked, err := env.env.service.Ask(context.Background(), task.AskCommand{
		Actor:                    env.env.actor(),
		Scope:                    env.env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: 2,
		Question:                 "Which branch should I target?",
		Assignment:               taskAssignment(),
		CallerKey:                "ask-running",
		Content:                  integrationContent("ask-running"),
	})
	if err != nil {
		t.Fatalf("task ask: %v", err)
	}
	if asked.LifecycleVersion != 3 || asked.QuestionVersion != 1 {
		t.Fatalf("ask result = %+v; want lifecycle 3 and question 1", asked)
	}

	unit := env.env.ports.lastAsk()
	if unit.Commit == nil || unit.Commit.Stop == nil || unit.Commit.Stop.WorkloadID != workload {
		t.Fatalf("the ask unit carries no StopIntent for %q: %+v", workload, unit.Commit)
	}

	row := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
	if textValue(row, "state") != string(task.StateWaiting) {
		t.Fatalf("task state = %q; want waiting", textValue(row, "state"))
	}
	if row.Values["question_id"] == nil {
		t.Fatal("the ask committed no outstanding question")
	}
	execution, ok := env.env.fixture.storedRow(t, env.env.scope, "executions", string(created.ExecutionID))
	if !ok || textValue(execution, "state") != string(kernel.ExecutionCancelled) {
		t.Fatalf("execution row = %+v; want cancelled", execution.Values)
	}
	actions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if len(actions) != 1 || textValue(actions[0], "state") != string(kernel.ActionCancelled) {
		t.Fatalf("action rows = %+v; want cancelled", actions)
	}
	cleanup, ok := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if !ok || textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("cleanup row = %+v; want pending", cleanup.Values)
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskQuestion) != beforeQuestion+1 {
		t.Fatal("the ask unit did not publish task.question exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) != beforeCancelled+1 {
		t.Fatal("the ask unit did not publish execution.cancelled exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition) != beforeAction+1 {
		t.Fatal("the ask unit did not publish action.transition exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeCleanup) != beforeCleanup+1 {
		t.Fatal("the ask unit did not publish execution.cleanup exactly once")
	}

	env.runtime.ConfirmNextStop("termination observed")
	final := env.reconcile(t, created.ExecutionID)
	if final.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q; want complete", final.Cleanup.State)
	}
}

// TestTaskCancelQueuedPredecessorHasNoStopIntent is case (c): a queued
// predecessor never started, so its pending action is cancelled with no
// StopIntent.
func TestTaskCancelQueuedPredecessorHasNoStopIntent(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Queued cancel", PipelineID: testPipeline, Start: true,
		CallerKey: "queued-cancel", Content: integrationContent("queued-cancel"),
	})
	if err != nil {
		t.Fatalf("create-and-start: %v", err)
	}
	if _, err := env.service.Cancel(ctx, task.CancelCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "queued-cancel", Content: integrationContent("queued-cancel"),
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	unit := env.ports.lastCancel()
	if unit.Commit == nil {
		t.Fatal("the queued cancel carries no Kernel commit")
	}
	if unit.Commit.Stop != nil {
		t.Fatalf("a queued predecessor carried a StopIntent: %+v", unit.Commit.Stop)
	}
	execution, ok := env.fixture.storedRow(t, env.scope, "executions", string(created.ExecutionID))
	if !ok || textValue(execution, "state") != string(kernel.ExecutionCancelled) {
		t.Fatalf("execution row = %+v; want cancelled", execution.Values)
	}
	actions := actionRows(t, env.fixture, env.scope, created.ExecutionID)
	if len(actions) != 1 || textValue(actions[0], "state") != string(kernel.ActionCancelled) {
		t.Fatalf("action rows = %+v; want the pending action cancelled", actions)
	}
	cleanup, ok := cleanupRow(t, env.fixture, env.scope, created.ExecutionID)
	if !ok || textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("cleanup row = %+v; want pending", cleanup.Values)
	}
	if eventCount(t, env.fixture, env.scope, event.TypeExecutionCancelled) != 1 {
		t.Fatal("execution.cancelled was not published exactly once")
	}
	if eventCount(t, env.fixture, env.scope, event.TypeActionTransition) != 1 {
		t.Fatal("action.transition was not published exactly once")
	}
	if eventCount(t, env.fixture, env.scope, event.TypeTaskCancelled) != 1 {
		t.Fatal("task.cancelled was not published exactly once")
	}
}

// TestTaskCancelRequestWaitHasNoStopIntent is case (d): a running request wait
// is cancelled with no StopIntent.
func TestTaskCancelRequestWaitHasNoStopIntent(t *testing.T) {
	env := newCancellationEngineEnvWith(t, func(f *fixture, scope tenancy.Scope) task.Admission {
		return requestIntegrationAdmission{pipelines: f.pipelines, scope: scope}
	})
	env.env.fixture.seedPipeline(t, env.env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	created := env.createAndStart(t, "request-cancel")
	running := env.reconcile(t, created.ExecutionID)
	if running.State != kernel.ExecutionRunning || running.CurrentAction != 0 {
		t.Fatalf("request execution = %s current %d; want running request", running.State, running.CurrentAction)
	}
	if running.Actions[0].Request == nil || running.Actions[0].Request.Subject == nil {
		t.Fatalf("request action = %+v; want a resolved subject", running.Actions[0])
	}

	beforeCancelled := eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled)
	beforeAction := eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition)
	if _, err := env.env.service.Cancel(context.Background(), task.CancelCommand{
		Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "request-cancel", Content: integrationContent("request-cancel"),
	}); err != nil {
		t.Fatalf("cancel request wait: %v", err)
	}
	unit := env.env.ports.lastCancel()
	if unit.Commit == nil {
		t.Fatal("the request cancel carries no Kernel commit")
	}
	if unit.Commit.Stop != nil {
		t.Fatalf("a request wait carried a StopIntent: %+v", unit.Commit.Stop)
	}
	execution, ok := env.env.fixture.storedRow(t, env.env.scope, "executions", string(created.ExecutionID))
	if !ok || textValue(execution, "state") != string(kernel.ExecutionCancelled) {
		t.Fatalf("execution row = %+v; want cancelled", execution.Values)
	}
	actions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if len(actions) != 1 || textValue(actions[0], "state") != string(kernel.ActionCancelled) {
		t.Fatalf("action rows = %+v; want the request cancelled", actions)
	}
	cleanup, ok := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if !ok || textValue(cleanup, "state") != string(kernel.CleanupPending) {
		t.Fatalf("cleanup row = %+v; want pending", cleanup.Values)
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) != beforeCancelled+1 {
		t.Fatal("the cancel unit did not publish execution.cancelled exactly once")
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeActionTransition) != beforeAction+1 {
		t.Fatal("the cancel unit did not publish action.transition exactly once")
	}
}

// TestTaskTerminalPredecessorIsPreserved is case (e): a terminal predecessor
// carries no Commit and its execution/action/cleanup rows are byte-identical
// afterwards; only the task row and the unit's own event change.
func TestTaskTerminalPredecessorIsPreserved(t *testing.T) {
	cases := []struct {
		name    string
		settle  func(*cancellationEngine, *testing.T, id.ExecutionID) kernel.Execution
		command string
	}{
		{name: "succeeded", settle: (*cancellationEngine).settleSucceeded, command: "ask"},
		{name: "failed", settle: (*cancellationEngine).settleFailed, command: "cancel"},
		{name: "cancelled", settle: (*cancellationEngine).settleCancelled, command: "cancel"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newCancellationEngineEnv(t)
			created, _ := env.startRunningTask(t, "terminal-"+testCase.name)
			testCase.settle(env, t, created.ExecutionID)

			beforeExecution := env.env.executionRow(t, created.ExecutionID)
			beforeActions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
			beforeCleanup, beforeHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
			beforeEvents := env.env.fixture.commitCounts(t, env.env.scope)

			ctx := context.Background()
			if testCase.command == "ask" {
				if _, err := env.env.service.Ask(ctx, task.AskCommand{
					Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
					ExpectedLifecycleVersion: 2, Question: "Which branch?",
					Assignment: taskAssignment(), CallerKey: "terminal-ask",
					Content: integrationContent("terminal-ask"),
				}); err != nil {
					t.Fatalf("ask over a terminal predecessor: %v", err)
				}
				if unit := env.env.ports.lastAsk(); unit.Commit != nil {
					t.Fatalf("a terminal predecessor carried a commit: %+v", unit.Commit)
				}
				if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskQuestion) != 1 {
					t.Fatal("task.question was not published exactly once")
				}
			} else {
				if _, err := env.env.service.Cancel(ctx, task.CancelCommand{
					Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
					ExpectedLifecycleVersion: 2, CallerKey: "terminal-cancel",
					Content: integrationContent("terminal-cancel"),
				}); err != nil {
					t.Fatalf("cancel over a terminal predecessor: %v", err)
				}
				if unit := env.env.ports.lastCancel(); unit.Commit != nil {
					t.Fatalf("a terminal predecessor carried a commit: %+v", unit.Commit)
				}
				if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskCancelled) != 1 {
					t.Fatal("task.cancelled was not published exactly once")
				}
			}

			afterExecution := env.env.executionRow(t, created.ExecutionID)
			if !reflect.DeepEqual(beforeExecution, afterExecution) {
				t.Fatalf("the terminal execution row changed:\nbefore = %+v\nafter  = %+v", beforeExecution, afterExecution)
			}
			afterActions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
			if !reflect.DeepEqual(beforeActions, afterActions) {
				t.Fatalf("terminal action rows changed:\nbefore = %+v\nafter  = %+v", beforeActions, afterActions)
			}
			afterCleanup, afterHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
			if beforeHasCleanup != afterHasCleanup || !reflect.DeepEqual(beforeCleanup, afterCleanup) {
				t.Fatalf("terminal cleanup row changed:\nbefore = %+v\nafter  = %+v", beforeCleanup, afterCleanup)
			}
			// Only the unit's own task event (plus its receipt/audit) is new.
			afterEvents := env.env.fixture.commitCounts(t, env.env.scope)
			if afterEvents.events != beforeEvents.events+1 {
				t.Fatalf("terminal handoff published %d events; want exactly one task event",
					afterEvents.events-beforeEvents.events)
			}
			if afterEvents.receipts != beforeEvents.receipts+1 || afterEvents.audits != beforeEvents.audits+1 {
				t.Fatal("terminal handoff did not commit exactly one receipt and one audit row")
			}
			if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) > 1 {
				t.Fatal("the terminal handoff re-published execution.cancelled")
			}
		})
	}
}

// TestTaskCancelEngineSettlementRaceOneWinner is case (f): the Kernel commit
// and the task cancel share the execution version; one wins and the loser is
// version_conflict with no partial state.
func TestTaskCancelEngineSettlementRaceOneWinner(t *testing.T) {
	t.Run("engine settles first", func(t *testing.T) {
		env := newCancellationEngineEnv(t)
		created, running := env.startRunningTask(t, "race-engine-first")
		settlement := env.successCommit(t, running)

		reached := make(chan struct{})
		release := make(chan struct{})
		env.env.ports.setHook(func() {
			close(reached)
			<-release
		})
		var cancelErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, cancelErr = env.env.service.Cancel(context.Background(), task.CancelCommand{
				Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
				ExpectedLifecycleVersion: 2, CallerKey: "race-engine-first",
				Content: integrationContent("race-engine-first"),
			})
		}()
		<-reached
		if _, err := executionstore.NewEngine(env.env.fixture.store.Controller()).
			Commit(context.Background(), settlement); err != nil {
			t.Fatalf("engine settlement commit: %v", err)
		}
		close(release)
		<-done
		mustClass(t, cancelErr, fault.VersionConflict)

		execution := env.env.executionRow(t, created.ExecutionID)
		if textValue(execution, "state") != string(kernel.ExecutionSucceeded) {
			t.Fatalf("execution state = %q; want succeeded", textValue(execution, "state"))
		}
		row := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
		if textValue(row, "state") != string(task.StateOpen) {
			t.Fatalf("a losing cancel changed the task: %+v", row.Values)
		}
		if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskCancelled) != 0 {
			t.Fatal("a losing cancel published task.cancelled")
		}
	})

	t.Run("cancel first", func(t *testing.T) {
		env := newCancellationEngineEnv(t)
		created, running := env.startRunningTask(t, "race-cancel-first")
		stale := env.successCommit(t, running)

		if _, err := env.env.service.Cancel(context.Background(), task.CancelCommand{
			Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
			ExpectedLifecycleVersion: 2, CallerKey: "race-cancel-first",
			Content: integrationContent("race-cancel-first"),
		}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		_, err := executionstore.NewEngine(env.env.fixture.store.Controller()).
			Commit(context.Background(), stale)
		mustClass(t, err, fault.VersionConflict)

		execution := env.env.executionRow(t, created.ExecutionID)
		if textValue(execution, "state") != string(kernel.ExecutionCancelled) {
			t.Fatalf("execution state = %q; want cancelled", textValue(execution, "state"))
		}
		if versionValue(execution, "version") != running.Version+1 {
			t.Fatalf("execution version = %d; want %d", versionValue(execution, "version"), running.Version+1)
		}
		row := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
		if textValue(row, "state") != string(task.StateCancelled) {
			t.Fatalf("task state = %q; want cancelled", textValue(row, "state"))
		}
		if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskCancelled) != 1 {
			t.Fatal("task.cancelled was not published exactly once")
		}
		if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) != 1 {
			t.Fatal("execution.cancelled was not published exactly once")
		}
	})
}

// successCommit reduces one confirmed-success observation of the running
// container action.
func (env *cancellationEngine) successCommit(t *testing.T, running kernel.Execution) flow.Commit {
	t.Helper()
	workload := running.Actions[running.CurrentAction].Container.WorkloadID
	env.runtime.QueueObservation(workload, kernel.ExitedObservation(0, kernel.ConfirmedStop("exit 0")))
	pre := env.load(t, running.ID)
	_, commit, err := flow.Reduce(pre, flow.ObserveFact{
		ExecutionID: running.ID,
		ActionID:    running.Actions[running.CurrentAction].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exit 0")),
		At:          testNow.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("reduce success observation: %v", err)
	}
	if commit == nil {
		t.Fatal("success observation reduced to no commit")
	}
	return *commit
}

// TestTaskAskCancelRaceOneWinner is case (f): ask and cancel built from the
// same task/execution versions race; one wins with no partial state.
func TestTaskAskCancelRaceOneWinner(t *testing.T) {
	env := newCancellationEngineEnv(t)
	created, running := env.startRunningTask(t, "race-ask-cancel")
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, results[0] = env.env.service.Ask(context.Background(), task.AskCommand{
			Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
			ExpectedLifecycleVersion: 2, Question: "Which branch?",
			Assignment: taskAssignment(), CallerKey: "race-ask", Content: integrationContent("race-ask"),
		})
	}()
	go func() {
		defer wait.Done()
		<-start
		_, results[1] = env.env.service.Cancel(context.Background(), task.CancelCommand{
			Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
			ExpectedLifecycleVersion: 2, CallerKey: "race-cancel",
			Content: integrationContent("race-cancel"),
		})
	}()
	close(start)
	wait.Wait()

	winners, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		default:
			mustClass(t, err, fault.VersionConflict)
			conflicts++
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("race produced %d winners and %d conflicts; want one of each", winners, conflicts)
	}
	execution := env.env.executionRow(t, created.ExecutionID)
	if textValue(execution, "state") != string(kernel.ExecutionCancelled) || versionValue(execution, "version") != running.Version+1 {
		t.Fatalf("execution row = %+v; want one cancellation at version %d", execution.Values, running.Version+1)
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeExecutionCancelled) != 1 {
		t.Fatal("execution.cancelled was not published exactly once")
	}
	row := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
	state := textValue(row, "state")
	if state != string(task.StateWaiting) && state != string(task.StateCancelled) {
		t.Fatalf("task state = %q; want exactly one winner", state)
	}
}

// TestTaskReplyDispatchWaitsForConfirmedStop is case (f): a reply built while
// the predecessor stop is unconfirmed never dispatches, while a reply built
// after cleanup completes does dispatch.
func TestTaskReplyDispatchWaitsForConfirmedStop(t *testing.T) {
	env := newCancellationEngineEnv(t)
	env.env.fixture.seedPipeline(t, env.env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)

	// Task A: reply is built while the predecessor cleanup is still pending.
	first := env.createAndStart(t, "dispatch-wait")
	env.reconcile(t, first.ExecutionID)
	if _, err := env.env.service.Ask(context.Background(), task.AskCommand{
		Actor: env.env.actor(), Scope: env.env.scope, TaskID: first.TaskID,
		ExpectedLifecycleVersion: 2, Question: "Which branch?",
		Assignment: taskAssignment(), CallerKey: "dispatch-wait-ask",
		Content: integrationContent("dispatch-wait-ask"),
	}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	question := env.env.fixture.storedTask(t, env.env.scope, first.TaskID)
	questionID := task.QuestionID(textValue(question, "question_id"))

	reached := make(chan struct{})
	release := make(chan struct{})
	env.env.ports.setHook(func() {
		close(reached)
		<-release
	})
	var replyErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, replyErr = env.env.service.Reply(context.Background(), task.ReplyCommand{
			Actor: env.env.actor(), Scope: env.env.scope, TaskID: first.TaskID,
			ExpectedLifecycleVersion: 3, QuestionID: questionID, ExpectedQuestionVersion: 1,
			Answer: "main", CallerKey: "dispatch-wait-reply",
			Content: integrationContent("dispatch-wait-reply"),
		})
	}()
	<-reached
	// The reply unit is already built with unconfirmed stop evidence. Complete
	// the predecessor cleanup while the unit waits at the adapter: the cleanup
	// commit advances the execution the reply's authority read pinned, so the
	// stale reply loses the version guard instead of dispatching.
	env.runtime.ConfirmNextStop("termination observed")
	if final := env.reconcile(t, first.ExecutionID); final.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("predecessor cleanup = %q; want complete", final.Cleanup.State)
	}
	close(release)
	<-done
	mustClass(t, replyErr, fault.VersionConflict)
	requireNoDispatchEvent(t, env.env.ports.lastReply().PostCommit)
	row := env.env.fixture.storedTask(t, env.env.scope, first.TaskID)
	if textValue(row, "state") != string(task.StateWaiting) {
		t.Fatalf("a losing reply changed the task: %+v", row.Values)
	}
	if count := env.env.fixture.countRows(t, env.env.scope, "executions", map[string]any{"task_id": string(first.TaskID)}); count != 1 {
		t.Fatalf("a losing reply committed %d executions; want 1", count)
	}
	if eventCount(t, env.env.fixture, env.env.scope, event.TypeTaskReply) != 0 {
		t.Fatal("a losing reply published task.reply")
	}

	// Task B: reply is built after the predecessor cleanup is complete.
	second := env.createAndStart(t, "dispatch-confirmed")
	env.reconcile(t, second.ExecutionID)
	if _, err := env.env.service.Ask(context.Background(), task.AskCommand{
		Actor: env.env.actor(), Scope: env.env.scope, TaskID: second.TaskID,
		ExpectedLifecycleVersion: 2, Question: "Which branch?",
		Assignment: taskAssignment(), CallerKey: "dispatch-confirmed-ask",
		Content: integrationContent("dispatch-confirmed-ask"),
	}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	env.runtime.ConfirmNextStop("termination observed")
	if final := env.reconcile(t, second.ExecutionID); final.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("predecessor cleanup = %q; want complete", final.Cleanup.State)
	}
	question = env.env.fixture.storedTask(t, env.env.scope, second.TaskID)
	if _, err := env.env.service.Reply(context.Background(), task.ReplyCommand{
		Actor: env.env.actor(), Scope: env.env.scope, TaskID: second.TaskID,
		ExpectedLifecycleVersion: 3, QuestionID: task.QuestionID(textValue(question, "question_id")),
		ExpectedQuestionVersion: 1, Answer: "main", CallerKey: "dispatch-confirmed-reply",
		Content: integrationContent("dispatch-confirmed-reply"),
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if !hasPostCommitDispatch(env.env.ports.lastReply().PostCommit) {
		t.Fatal("a reply after confirmed stop did not request dispatch")
	}
}

// TestTaskCancellationReplayReturnsOriginal is case (g): an equal retry of an
// already-settled unit returns the original result and re-applies/publishes
// nothing.
func TestTaskCancellationReplayReturnsOriginal(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		env := newRealServiceEnv(t)
		ctx := context.Background()
		env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
		created, err := env.service.Create(ctx, task.CreateCommand{
			Actor: env.actor(), Scope: env.scope, Title: "Replay cancel", PipelineID: testPipeline, Start: true,
			CallerKey: "replay-cancel", Content: integrationContent("replay-cancel"),
		})
		if err != nil {
			t.Fatalf("create-and-start: %v", err)
		}
		first, err := env.service.Cancel(ctx, task.CancelCommand{
			Actor: env.actor(), Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
			CallerKey: "replay-cancel", Content: integrationContent("replay-cancel"),
		})
		if err != nil {
			t.Fatalf("first cancel: %v", err)
		}
		beforeEvents := env.fixture.commitCounts(t, env.scope)
		beforeExecution := env.executionRow(t, created.ExecutionID)

		second, err := env.service.Cancel(ctx, task.CancelCommand{
			Actor: env.actor(), Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
			CallerKey: "replay-cancel", Content: integrationContent("replay-cancel"),
		})
		if err != nil {
			t.Fatalf("replayed cancel: %v", err)
		}
		if !second.Replayed || second.LifecycleVersion != first.LifecycleVersion {
			t.Fatalf("replayed result = %+v; want the original with Replayed", second)
		}
		if unit := env.ports.lastCancel(); unit.Commit != nil {
			t.Fatalf("the replay unit carries a commit: %+v", unit.Commit)
		}
		afterEvents := env.fixture.commitCounts(t, env.scope)
		if afterEvents != beforeEvents {
			t.Fatalf("a replay published rows: before %+v after %+v", beforeEvents, afterEvents)
		}
		afterExecution := env.executionRow(t, created.ExecutionID)
		if !reflect.DeepEqual(beforeExecution, afterExecution) {
			t.Fatalf("a replay changed the execution:\nbefore = %+v\nafter  = %+v", beforeExecution, afterExecution)
		}
	})

	t.Run("ask", func(t *testing.T) {
		env := newRealServiceEnv(t)
		ctx := context.Background()
		env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
		created, err := env.service.Create(ctx, task.CreateCommand{
			Actor: env.actor(), Scope: env.scope, Title: "Replay ask", PipelineID: testPipeline, Start: true,
			CallerKey: "replay-ask", Content: integrationContent("replay-ask"),
		})
		if err != nil {
			t.Fatalf("create-and-start: %v", err)
		}
		first, err := env.service.Ask(ctx, task.AskCommand{
			Actor: env.actor(), Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
			Question: "Which branch?", Assignment: taskAssignment(),
			CallerKey: "replay-ask", Content: integrationContent("replay-ask"),
		})
		if err != nil {
			t.Fatalf("first ask: %v", err)
		}
		beforeEvents := env.fixture.commitCounts(t, env.scope)
		beforeExecution := env.executionRow(t, created.ExecutionID)

		second, err := env.service.Ask(ctx, task.AskCommand{
			Actor: env.actor(), Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
			Question: "Which branch?", Assignment: taskAssignment(),
			CallerKey: "replay-ask", Content: integrationContent("replay-ask"),
		})
		if err != nil {
			t.Fatalf("replayed ask: %v", err)
		}
		if !second.Replayed || second.LifecycleVersion != first.LifecycleVersion {
			t.Fatalf("replayed result = %+v; want the original with Replayed", second)
		}
		if unit := env.ports.lastAsk(); unit.Commit != nil {
			t.Fatalf("the replay unit carries a commit: %+v", unit.Commit)
		}
		afterEvents := env.fixture.commitCounts(t, env.scope)
		if afterEvents != beforeEvents {
			t.Fatalf("a replay published rows: before %+v after %+v", beforeEvents, afterEvents)
		}
		afterExecution := env.executionRow(t, created.ExecutionID)
		if !reflect.DeepEqual(beforeExecution, afterExecution) {
			t.Fatalf("a replay changed the execution:\nbefore = %+v\nafter  = %+v", beforeExecution, afterExecution)
		}
	})
}

// TestTaskCancelRollbackLeavesNoPartialState is case (h): a failure after the
// execution rows are planned leaves the task, execution, action, cleanup,
// receipt and events all unchanged.
func TestTaskCancelRollbackLeavesNoPartialState(t *testing.T) {
	env := newCancellationEngineEnv(t)
	created, running := env.startRunningTask(t, "rollback")
	beforeTask := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
	beforeExecution := env.env.executionRow(t, created.ExecutionID)
	beforeActions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	_, beforeHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
	beforeCounts := env.env.fixture.commitCounts(t, env.env.scope)

	env.env.fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
	_, err := env.env.service.Cancel(context.Background(), task.CancelCommand{
		Actor: env.env.actor(), Scope: env.env.scope, TaskID: created.TaskID,
		ExpectedLifecycleVersion: 2, CallerKey: "rollback",
		Content: integrationContent("rollback"),
	})
	mustClass(t, err, fault.DependencyUnavailable)

	afterTask := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
	if !reflect.DeepEqual(beforeTask, afterTask) {
		t.Fatalf("a failed cancel changed the task:\nbefore = %+v\nafter  = %+v", beforeTask, afterTask)
	}
	afterExecution := env.env.executionRow(t, created.ExecutionID)
	if !reflect.DeepEqual(beforeExecution, afterExecution) {
		t.Fatalf("a failed cancel changed the execution:\nbefore = %+v\nafter  = %+v", beforeExecution, afterExecution)
	}
	afterActions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	if !reflect.DeepEqual(beforeActions, afterActions) {
		t.Fatalf("a failed cancel changed actions:\nbefore = %+v\nafter  = %+v", beforeActions, afterActions)
	}
	if _, afterHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID); afterHasCleanup != beforeHasCleanup {
		t.Fatal("a failed cancel committed a cleanup row")
	}
	if afterCounts := env.env.fixture.commitCounts(t, env.env.scope); afterCounts != beforeCounts {
		t.Fatalf("a failed cancel published rows: before %+v after %+v", beforeCounts, afterCounts)
	}
	if running.Version != versionValue(afterExecution, "version") {
		t.Fatal("a failed cancel advanced the execution version")
	}
}

// TestTaskCancellationRefusesLiveExecutionWithoutCommit is case (i) and the
// case (j) regression: a live predecessor with no Kernel Commit is refused
// invalid_input and the adapter never writes executions.state directly.
func TestTaskCancellationRefusesLiveExecutionWithoutCommit(t *testing.T) {
	env := newCancellationEngineEnv(t)
	created, running := env.startRunningTask(t, "no-commit")
	beforeTask := env.env.fixture.storedTask(t, env.env.scope, created.TaskID)
	beforeExecution := env.env.executionRow(t, created.ExecutionID)
	beforeActions := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID)
	_, beforeHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID)
	beforeCounts := env.env.fixture.commitCounts(t, env.env.scope)

	cancel := task.CancelUnit{
		Owner:                    env.env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: 2,
		ExpectedExecutionVersion: running.Version,
		Receipt:                  manualReceipt(env.env.scope, "task.cancel", "no-commit-cancel"),
		Audit:                    []event.Audit{env.env.fixture.audit(env.env.scope, "task.cancel")},
		Events:                   []event.Envelope{manualEvent(env.env.scope, "no-commit-cancel", string(created.TaskID))},
	}
	_, err := env.env.fixture.task.Cancel(context.Background(), cancel)
	mustClass(t, err, fault.InvalidInput)

	ask := task.AskUnit{
		Owner:                    env.env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: 2,
		ExpectedExecutionVersion: running.Version,
		QuestionID:               "question-no-commit",
		Question:                 "Which branch?",
		Assignment:               taskAssignment(),
		Receipt:                  manualReceipt(env.env.scope, "task.ask", "no-commit-ask"),
		Audit:                    []event.Audit{env.env.fixture.audit(env.env.scope, "task.ask")},
		Events:                   []event.Envelope{manualEvent(env.env.scope, "no-commit-ask", string(created.TaskID))},
	}
	_, err = env.env.fixture.task.Ask(context.Background(), ask)
	mustClass(t, err, fault.InvalidInput)

	if after := env.env.fixture.storedTask(t, env.env.scope, created.TaskID); !reflect.DeepEqual(beforeTask, after) {
		t.Fatalf("a refused unit changed the task:\nbefore = %+v\nafter  = %+v", beforeTask, after)
	}
	if after := env.env.executionRow(t, created.ExecutionID); !reflect.DeepEqual(beforeExecution, after) {
		t.Fatalf("a refused unit changed the execution:\nbefore = %+v\nafter  = %+v", beforeExecution, after)
	}
	if after := actionRows(t, env.env.fixture, env.env.scope, created.ExecutionID); !reflect.DeepEqual(beforeActions, after) {
		t.Fatalf("a refused unit changed actions:\nbefore = %+v\nafter  = %+v", beforeActions, after)
	}
	if _, afterHasCleanup := cleanupRow(t, env.env.fixture, env.env.scope, created.ExecutionID); afterHasCleanup != beforeHasCleanup {
		t.Fatal("a refused unit committed a cleanup row")
	}
	if after := env.env.fixture.commitCounts(t, env.env.scope); after != beforeCounts {
		t.Fatalf("a refused unit published rows: before %+v after %+v", beforeCounts, after)
	}
}

// taskAssignment is one valid role assignment for a conversation question.
func taskAssignment() request.Assignment {
	return request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}
}
