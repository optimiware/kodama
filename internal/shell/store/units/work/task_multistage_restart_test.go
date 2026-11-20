package work

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	executionstore "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/token"
)

func TestTaskStartAtomicStageTransitionFromDoneUpdatesPipelineInputsAndInitiator(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	const stageTwo = pipeline.PipelineID("pipeline-2")
	const newInitiator = id.PrincipalID("principal-2")

	fixture.seedPipeline(t, scope, testPipeline, "implement", []byte("kind: Pipeline\n"), 1)
	fixture.seedPipeline(t, scope, stageTwo, "integrate", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)

	// Start stage 1 and complete it so task transitions to done.
	first, err := fixture.task.Start(context.Background(), fixture.startUnit(scope, testTask, testExecution, 1, 1, fixture.receipt("task.start", "stage-1", contentHashA)))
	if err != nil {
		t.Fatalf("stage 1 Start: %v", err)
	}
	if _, err := fixture.task.Apply(context.Background(), task.ProgressUnit{
		Owner: scope, TaskID: testTask, ExecutionID: first.ExecutionID, ExpectedLifecycleVersion: 2, At: testNow,
	}); err != nil {
		t.Fatalf("Progress to done: %v", err)
	}

	// Atomically transition from done -> open on stage 2 with replaced inputs and initiator.
	unit := fixture.startUnit(scope, testTask, "execution-2", 3, 1, fixture.receipt("task.start", "stage-2", contentHashB))
	unit.PipelineID = stageTwo
	unit.ExpectedState = task.StateDone
	unit.UpdateInputs = true
	unit.Inputs = map[id.InputKey]string{"branch": "work/task-1"}
	unit.UpdateInitiator = true
	unit.Initiator = newInitiator

	second, err := fixture.task.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("stage 2 Start: %v", err)
	}
	if second.ExecutionID != "execution-2" || second.LifecycleVersion != 4 || second.Replayed {
		t.Fatalf("stage 2 result = %+v", second)
	}

	got, err := fixture.task.Get(context.Background(), scope, testTask)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != task.StateOpen || got.SelectedPipeline != stageTwo || got.Initiator != newInitiator || got.CurrentExecution != "execution-2" {
		t.Fatalf("task after stage 2 = %+v", got)
	}
	if len(got.Inputs) != 1 || got.Inputs["branch"] != "work/task-1" {
		t.Fatalf("task inputs = %+v; want only branch=work/task-1", got.Inputs)
	}
	if len(got.ExecutionHistory) != 2 || got.ExecutionHistory[0] != testExecution || got.ExecutionHistory[1] != "execution-2" {
		t.Fatalf("execution history = %v", got.ExecutionHistory)
	}

	// Identical retry replays without modifying the now-open task.
	replayed, err := fixture.task.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("stage 2 replay: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionID != second.ExecutionID || replayed.LifecycleVersion != second.LifecycleVersion {
		t.Fatalf("stage 2 replay = %+v; want %+v", replayed, second)
	}
}

func TestTaskStartWithCancelCommitAtomicallySupersedesWaitingRequest(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "integrate", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)

	// Start execution 1 with container action 0 + review request action 1.
	firstUnit := fixture.startUnit(scope, testTask, testExecution, 1, 1, fixture.receipt("task.start", "start-1", contentHashA))
	firstUnit.Actions = []task.AdmittedAction{testAction(0), testRequestAction(1)}
	if _, err := fixture.task.Start(context.Background(), firstUnit); err != nil {
		t.Fatalf("Start execution 1: %v", err)
	}
	execAdapter := executionstore.New(fixture.store, fixture.store)
	pre, err := execAdapter.Load(context.Background(), scope, testExecution)
	if err != nil {
		t.Fatalf("Load execution 1: %v", err)
	}
	// Mark action 0 succeeded (confirmed stop) and activate action 1 (request).
	engAdapter := executionstore.NewEngine(fixture.store.Controller())
	_, act0Commit, err := flow.Reduce(pre, flow.ActivateFact{ExecutionID: pre.ID, ActionID: pre.Actions[0].ID, At: testNow})
	if err != nil {
		t.Fatalf("reduce activate 0: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *act0Commit); err != nil {
		t.Fatalf("commit activate 0: %v", err)
	}
	pre, _ = execAdapter.Load(context.Background(), scope, testExecution)
	_, done0Commit, err := flow.Reduce(pre, flow.ObserveFact{
		ExecutionID: pre.ID, ActionID: pre.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exited")),
		At:          testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reduce observe 0: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *done0Commit); err != nil {
		t.Fatalf("commit observe 0: %v", err)
	}
	pre, _ = execAdapter.Load(context.Background(), scope, testExecution)
	_, act1Commit, err := flow.Reduce(pre, flow.ActivateFact{
		ExecutionID: pre.ID, ActionID: pre.Actions[1].ID,
		At: testNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reduce activate 1: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *act1Commit); err != nil {
		t.Fatalf("commit activate 1: %v", err)
	}

	// Now build a superseded cancel commit for execution 1 and apply it atomically with execution 2.
	pre, err = execAdapter.Load(context.Background(), scope, testExecution)
	if err != nil {
		t.Fatalf("reload execution 1: %v", err)
	}
	_, cancelCommit, err := flow.Reduce(pre, flow.CancelFact{
		ExecutionID: pre.ID,
		Reason:      flow.CancelSuperseded,
		Detail:      "main moved",
		At:          testNow.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reduce supersede: %v", err)
	}

	restartUnit := fixture.startUnit(scope, testTask, "execution-2", 2, 1, fixture.receipt("task.restart", "restart-1", contentHashB))
	restartUnit.ExpectedExecutionVersion = pre.Version
	restartUnit.CancelCommit = cancelCommit
	restartUnit.CredentialUpdate = &token.Credential{}

	restarted, err := fixture.task.Start(context.Background(), restartUnit)
	if err != nil {
		t.Fatalf("atomic restart Start: %v", err)
	}
	if restarted.ExecutionID != "execution-2" || restarted.SupersededExecution != testExecution || restarted.LifecycleVersion != 3 || restarted.Replayed {
		t.Fatalf("restart result = %+v", restarted)
	}

	// Predecessor must be cancelled as superseded with NO decision=fail answer.
	superseded, err := execAdapter.Load(context.Background(), scope, testExecution)
	if err != nil {
		t.Fatalf("load superseded execution: %v", err)
	}
	if superseded.State != kernel.ExecutionCancelled {
		t.Fatalf("superseded state = %q; want cancelled", superseded.State)
	}
	if superseded.Failure == nil || superseded.Failure.Kind != kernel.FailureCancelled || superseded.Failure.Reason != "superseded" || superseded.Failure.Detail != "main moved" {
		t.Fatalf("superseded failure = %+v; want cancelled/superseded/main moved", superseded.Failure)
	}
	if superseded.Actions[1].State != kernel.ActionCancelled || superseded.Actions[1].Request.Answer != nil {
		t.Fatalf("superseded review action = state:%q answer:%+v; want cancelled with no answer", superseded.Actions[1].State, superseded.Actions[1].Request.Answer)
	}

	// Replay with identical receipt returns original ExecutionID + SupersededExecution without creating a 3rd execution.
	replayed, err := fixture.task.Start(context.Background(), restartUnit)
	if err != nil {
		t.Fatalf("restart replay: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionID != "execution-2" || replayed.SupersededExecution != testExecution || replayed.LifecycleVersion != 3 {
		t.Fatalf("restart replay = %+v; want %+v", replayed, restarted)
	}
	if count := fixture.countRows(t, scope, "executions", map[string]any{"task_id": string(testTask)}); count != 2 {
		t.Fatalf("executions count = %d; want 2", count)
	}

	// Reusing the same idempotency key with changed content must be refused.
	changed := restartUnit
	changed.Receipt = fixture.receipt("task.restart", "restart-1", contentHashA)
	_, err = fixture.task.Start(context.Background(), changed)
	mustClass(t, err, fault.ChangedIdempotencyContent)
}

func TestTaskRestartStaleExecutionVersionRollsBackAtomically(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "integrate", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)

	if _, err := fixture.task.Start(context.Background(), fixture.startUnit(scope, testTask, testExecution, 1, 1, fixture.receipt("task.start", "start-1", contentHashA))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	execAdapter := executionstore.New(fixture.store, fixture.store)
	pre, _ := execAdapter.Load(context.Background(), scope, testExecution)
	_, cancelCommit, err := flow.Reduce(pre, flow.CancelFact{ExecutionID: pre.ID, Reason: flow.CancelSuperseded, Detail: "stale", At: testNow})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}

	staleUnit := fixture.startUnit(scope, testTask, "execution-2", 2, 1, fixture.receipt("task.restart", "restart-stale", contentHashB))
	staleUnit.ExpectedExecutionVersion = pre.Version + 5 // stale!
	staleUnit.CancelCommit = cancelCommit
	staleUnit.CredentialUpdate = &token.Credential{}

	_, err = fixture.task.Start(context.Background(), staleUnit)
	mustClass(t, err, fault.VersionConflict)

	// Verify zero partial state: execution 1 still queued, execution 2 does not exist.
	still, err := execAdapter.Load(context.Background(), scope, testExecution)
	if err != nil || still.State != kernel.ExecutionQueued {
		t.Fatalf("execution 1 after rolled back restart = %+v (err=%v); want queued", still, err)
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-2"); ok {
		t.Fatal("rolled back restart left replacement execution-2 behind")
	}
}
