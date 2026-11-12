package executionstore

// Regression coverage: a task replacement execution may be committed while its
// predecessor's stop is still unconfirmed, but the Shell predecessor barrier
// must keep it out of queued discovery and refuse its activation commit until
// every prior execution of the task has confirmed stop. Later confirmation
// releases the queued execution exactly once through authoritative scanning,
// including after a restart and while no notification or broker delivery is
// available.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// barrierClock is the injected authoritative clock of the Kernel test engine.
type barrierClock struct{ at time.Time }

func (clock barrierClock) Now() time.Time { return clock.at }

// barrierRuntime is the smallest Kernel runtime that accepts preparation and
// start and keeps reporting a live workload. Its stop evidence is controlled by
// the test, so the predecessor's barrier can be held unconfirmed or released.
type barrierRuntime struct {
	mu       sync.Mutex
	prepares int
	starts   int
	stop     kernel.StopEvidence
	observe  kernel.Observation
	remove   engineRemoval
}

type engineRemoval struct{ removed bool }

func (runtime *barrierRuntime) Capabilities(context.Context) (kernelengine.Capabilities, error) {
	return kernelengine.Capabilities{Names: []string{"process"}}, nil
}

func (runtime *barrierRuntime) Prepare(_ context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.prepares++
	return kernelengine.Preparation{WorkspaceID: request.WorkspaceID}, nil
}

func (runtime *barrierRuntime) Start(_ context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.starts++
	return kernelengine.StartResult{Outcome: kernelengine.StartAccepted, WorkloadID: request.Identity.WorkloadID}, nil
}

func (runtime *barrierRuntime) Observe(context.Context, kernelengine.ObserveRequest) (kernel.Observation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.observe.Kind == "" {
		return kernel.RunningObservation(), nil
	}
	return runtime.observe, nil
}

func (runtime *barrierRuntime) Logs(context.Context, kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return kernelengine.LogBatch{}, nil
}

func (runtime *barrierRuntime) Stop(context.Context, kernelengine.StopRequest) (kernel.StopEvidence, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.stop, nil
}

func (runtime *barrierRuntime) Remove(context.Context, kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return kernelengine.Removal{Removed: runtime.remove.removed}, nil
}

func (runtime *barrierRuntime) RefreshCredential(context.Context, kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

func (runtime *barrierRuntime) counts() (prepares, starts int) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.prepares, runtime.starts
}

func (runtime *barrierRuntime) setStop(evidence kernel.StopEvidence) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.stop = evidence
}

// barrierWorkspace retains the execution workspace without touching disk.
type barrierWorkspace struct{}

func (barrierWorkspace) Status(context.Context, id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	return kernelengine.WorkspaceObservation{Available: true}, nil
}

func (barrierWorkspace) Retain(context.Context, id.WorkspaceID) error  { return nil }
func (barrierWorkspace) Release(context.Context, id.WorkspaceID) error { return nil }

// startTaskExecution starts one task-bound execution at the exact instant the
// test chooses, so the predecessor ordering is deterministic.
func startTaskExecution(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope,
	taskID id.TaskID, executionID id.ExecutionID, at time.Time, actions ...pipeline.AdmittedAction) {
	t.Helper()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, executionID, string(executionID), actions...)
	unit.Snapshot = testSnapshot(taskID, nil)
	unit.At = at
	if _, err := adapter.Start(context.Background(), unit); err != nil {
		t.Fatalf("start task execution %q: %v", executionID, err)
	}
}

// barrierTransition commits one exact scoped transition and returns the next
// execution version.
func barrierTransition(t *testing.T, adapter *Store, scope tenancy.Scope, executionID id.ExecutionID,
	unit execution.TransitionUnit) uint64 {
	t.Helper()
	result, err := adapter.Transition(context.Background(), unit)
	if err != nil {
		t.Fatalf("transition execution %q: %v", executionID, err)
	}
	return result.ExecutionVersion
}

// activateStartedWorkload drives a queued execution to running with a persisted
// launch identity, matching the Kernel's activation plus start-marker commits.
func activateStartedWorkload(t *testing.T, adapter *Store, scope tenancy.Scope,
	executionID id.ExecutionID, at time.Time, workload id.WorkloadID) id.ActionID {
	t.Helper()
	actionID := snapshotdoc.DeriveActionID(executionID, 0)
	barrierTransition(t, adapter, scope, executionID, execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: 1,
		ExpectedActionVersion:    1,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: 1,
			NextExecutionState:       kernel.ExecutionRunning,
			ActionID:                 actionID,
			ExpectedActionVersion:    1,
			NextActionState:          kernel.ActionRunning,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			Launch: &kernel.LaunchIntent{
				WorkloadID:     workload,
				State:          kernel.LaunchIssued,
				IdempotencyKey: "key-" + string(executionID),
			},
			Deadline: at.Add(90 * time.Second),
			At:       at,
		},
	})
	return actionID
}

// cancelWithPendingStop leaves a terminal execution whose cleanup is still owed
// and whose possibly started workload has only unconfirmed stop evidence.
func cancelWithPendingStop(t *testing.T, adapter *Store, scope tenancy.Scope,
	executionID id.ExecutionID, actionID id.ActionID, at time.Time, workload id.WorkloadID) uint64 {
	t.Helper()
	pending := kernel.PendingStop("stop response lost")
	return barrierTransition(t, adapter, scope, executionID, execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: 2,
		ExpectedActionVersion:    2,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: 2,
			NextExecutionState:       kernel.ExecutionCancelled,
			ActionID:                 actionID,
			ExpectedActionVersion:    2,
			NextActionState:          kernel.ActionCancelled,
			Output:                   flow.OutputChange{Action: flow.OutputDiscard},
			InvalidateCredential:     true,
			StopEvidence:             &pending,
			Cleanup: &flow.CleanupIntent{
				WorkloadID:  workload,
				WorkspaceID: snapshotdoc.DeriveWorkspaceID(executionID),
				Reason:      "task_waiting",
			},
			At: at,
		},
	})
}

// confirmStopBarrier records a confirmed action barrier while cleanup is still
// owed, the rule deriveStopEvidence already accepts.
func confirmStopBarrier(t *testing.T, adapter *Store, scope tenancy.Scope,
	executionID id.ExecutionID, actionID id.ActionID, version, actionVersion uint64, at time.Time) {
	t.Helper()
	confirmed := kernel.ConfirmedStop("runtime confirmed termination")
	barrierTransition(t, adapter, scope, executionID, execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		ExpectedActionVersion:    actionVersion,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       kernel.ExecutionCancelled,
			ActionID:                 actionID,
			ExpectedActionVersion:    actionVersion,
			NextActionState:          kernel.ActionCancelled,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			StopEvidence:             &confirmed,
			At:                       at,
		},
	})
}

// recordRunningStopBarrier persists a confirmed action barrier while the
// execution is still non-terminal. A non-terminal execution may still start
// another action, so this evidence alone must never satisfy the predecessor
// barrier.
func recordRunningStopBarrier(t *testing.T, adapter *Store, scope tenancy.Scope,
	executionID id.ExecutionID, actionID id.ActionID, version, actionVersion uint64, at time.Time) {
	t.Helper()
	confirmed := kernel.ConfirmedStop("runtime confirmed termination")
	barrierTransition(t, adapter, scope, executionID, execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		ExpectedActionVersion:    actionVersion,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       kernel.ExecutionRunning,
			ActionID:                 actionID,
			ExpectedActionVersion:    actionVersion,
			NextActionState:          kernel.ActionRunning,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			StopEvidence:             &confirmed,
			At:                       at,
		},
	})
}

// completeCleanup records completed cleanup, the sufficient stop proof.
func completeCleanup(t *testing.T, adapter *Store, scope tenancy.Scope,
	executionID id.ExecutionID, version uint64, workload id.WorkloadID, at time.Time) {
	t.Helper()
	barrierTransition(t, adapter, scope, executionID, execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		Commit: flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       kernel.ExecutionCancelled,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate: &flow.CleanupUpdate{
				State:       kernel.CleanupComplete,
				WorkloadID:  workload,
				WorkspaceID: snapshotdoc.DeriveWorkspaceID(executionID),
				Attempts:    1,
			},
			At: at,
		},
	})
}

// activationCommitFor builds the exact activation transition the Kernel reducer
// commits for the first pending action of a queued execution.
func activationCommitFor(executionID id.ExecutionID, at time.Time) flow.Commit {
	return flow.Commit{
		ExecutionID:              executionID,
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 snapshotdoc.DeriveActionID(executionID, 0),
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Deadline:                 at.Add(90 * time.Second),
		At:                       at,
	}
}

// requireQueuedAndUnprepared proves a held replacement has no activation
// timestamp, deadline, preparation, credential or workload.
func requireQueuedAndUnprepared(t *testing.T, adapter *Store, backend *memory.Store,
	scope tenancy.Scope, executionID id.ExecutionID) {
	t.Helper()
	aggregate, err := adapter.Load(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("load held replacement %q: %v", executionID, err)
	}
	if aggregate.State != kernel.ExecutionQueued {
		t.Fatalf("held replacement state = %q; want queued", aggregate.State)
	}
	if len(aggregate.Actions) == 0 || aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("held replacement actions = %+v; want a pending first action", aggregate.Actions)
	}
	action := aggregate.Actions[0]
	if !action.ActivatedAt.IsZero() || !action.Deadline.IsZero() {
		t.Fatalf("held replacement activation = %s deadline = %s; want none",
			action.ActivatedAt, action.Deadline)
	}
	if action.Container == nil {
		t.Fatalf("held replacement first action is not a container action: %+v", action)
	}
	if action.Container.WorkloadID != "" || action.Container.Launch.IdempotencyKey != "" ||
		(action.Container.Launch.State != "" && action.Container.Launch.State != kernel.LaunchNotIssued) {
		t.Fatalf("held replacement launch = %+v; want no workload and not issued", action.Container.Launch)
	}
	row, ok := backend.Snapshot(scope, tableExecutions, string(executionID))
	if !ok {
		t.Fatalf("held replacement %q has no execution row", executionID)
	}
	if row.Values["token_nonce"] != nil || row.Values["token_expires_at"] != nil {
		t.Fatalf("held replacement carries a credential: nonce=%v expiry=%v",
			row.Values["token_nonce"], row.Values["token_expires_at"])
	}
}

// TestStopEvidenceRule pins the predecessor stop-barrier rule directly on the
// derivation the activation commit and the task reply decision share. It is a
// table of the exact affirmative and negative cases: a non-terminal execution
// never confirms (whatever evidence it carries), completed cleanup confirms,
// and with cleanup still owed only a confirmed barrier on every possibly
// started container action confirms.
func TestStopEvidenceRule(t *testing.T) {
	launched := func(stop kernel.StopEvidence) kernel.Action {
		return kernel.Action{
			State: kernel.ActionCancelled,
			Stop:  stop,
			Container: &kernel.ContainerAction{
				WorkloadID: "workload-1",
				Launch:     kernel.LaunchIntent{WorkloadID: "workload-1", State: kernel.LaunchIssued},
			},
		}
	}
	recordedStopOnly := func(stop kernel.StopEvidence) kernel.Action {
		return kernel.Action{
			State: kernel.ActionCancelled,
			Stop:  stop,
			Container: &kernel.ContainerAction{
				Launch: kernel.LaunchIntent{State: kernel.LaunchNotIssued},
			},
		}
	}
	neverStarted := kernel.Action{
		State: kernel.ActionCancelled,
		Container: &kernel.ContainerAction{
			Launch: kernel.LaunchIntent{State: kernel.LaunchNotIssued},
		},
	}
	request := kernel.Action{State: kernel.ActionSucceeded, Request: &kernel.RequestAction{}}

	cases := []struct {
		name      string
		execution kernel.Execution
		confirmed bool
	}{
		{
			name: "non-terminal never confirms even with complete cleanup",
			execution: kernel.Execution{State: kernel.ExecutionRunning, Cleanup: kernel.Cleanup{State: kernel.CleanupComplete},
				Actions: []kernel.Action{launched(kernel.ConfirmedStop("stopped"))}},
		},
		{
			name: "terminal and cleanup complete confirms",
			execution: kernel.Execution{State: kernel.ExecutionSucceeded, Cleanup: kernel.Cleanup{State: kernel.CleanupComplete},
				Actions: []kernel.Action{launched(kernel.PendingStop("lost"))}},
			confirmed: true,
		},
		{
			name:      "terminal with cleanup owed and no possibly started action is unconfirmed",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending}},
		},
		{
			name:      "terminal with no cleanup row and no possibly started action is unconfirmed",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupNone}},
		},
		{
			name: "terminal with no cleanup row and a confirmed action barrier confirms",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupNone},
				Actions: []kernel.Action{launched(kernel.ConfirmedStop("stopped"))}},
			confirmed: true,
		},
		{
			name: "terminal with cleanup owed and a never-started action is unconfirmed",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending},
				Actions: []kernel.Action{neverStarted}},
		},
		{
			name: "terminal with cleanup owed and a launched action without a barrier is unconfirmed",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending},
				Actions: []kernel.Action{launched(kernel.StopEvidence{})}},
		},
		{
			name: "terminal with cleanup owed and every launched action confirmed confirms",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending},
				Actions: []kernel.Action{launched(kernel.ConfirmedStop("stopped")), request}},
			confirmed: true,
		},
		{
			name: "terminal with cleanup owed and one launched action unconfirmed is unconfirmed",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending},
				Actions: []kernel.Action{launched(kernel.ConfirmedStop("stopped")), launched(kernel.PendingStop("lost"))}},
		},
		{
			name: "terminal with cleanup owed and a recorded stop on a never-launched action confirms",
			execution: kernel.Execution{State: kernel.ExecutionCancelled, Cleanup: kernel.Cleanup{State: kernel.CleanupPending},
				Actions: []kernel.Action{recordedStopOnly(kernel.ConfirmedStop("never started"))}},
			confirmed: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			evidence := StopEvidence(testCase.execution)
			if evidence.IsConfirmed() != testCase.confirmed {
				t.Fatalf("StopEvidence = %+v (confirmed %t); want confirmed %t",
					evidence, evidence.IsConfirmed(), testCase.confirmed)
			}
		})
	}
}

// TestQueuedDiscoveryHoldsReplacementUntilPredecessorStopConfirmed is the
// reproduction: a committed replacement exists while the predecessor stop is
// unconfirmed. The unfinished scan must keep running (and report the
// predecessor's owed cleanup) without returning the replacement.
func TestQueuedDiscoveryHoldsReplacementUntilPredecessorStopConfirmed(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	version := cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())
	if version != 3 {
		t.Fatalf("predecessor version = %d; want 3", version)
	}

	unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
	})
	if err != nil {
		t.Fatalf("unfinished scan: %v", err)
	}
	present := map[id.ExecutionID]bool{}
	for _, executionID := range unfinished {
		present[executionID] = true
	}
	if present["execution-replacement"] {
		t.Fatalf("unfinished scan = %v; the replacement must stay undiscovered while the predecessor stop is unconfirmed", unfinished)
	}
	if !present["execution-predecessor"] {
		t.Fatalf("unfinished scan = %v; the predecessor's owed cleanup must keep progressing", unfinished)
	}
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")
}

// TestActivationCommitRefusesUnconfirmedPredecessor proves the barrier is
// enforced again at the authoritative commit boundary, not only in discovery.
func TestActivationCommitRefusesUnconfirmedPredecessor(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())

	_, err := engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")
}

// TestEveryPriorExecutionOfTheTaskIsChecked proves one unconfirmed execution
// blocks the replacement no matter where it sits in the task history. Checking
// only the most recent (or only the oldest) predecessor cannot pass both cases.
func TestEveryPriorExecutionOfTheTaskIsChecked(t *testing.T) {
	cases := []struct {
		name        string
		unconfirmed string
	}{
		{name: "oldest unconfirmed", unconfirmed: "execution-first"},
		{name: "newest unconfirmed", unconfirmed: "execution-second"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engineState, adapter, backend := newEngine(t)
			scope := projectScope()
			const taskID id.TaskID = "task-1"
			startTaskExecution(t, adapter, backend, scope, taskID, "execution-first", testNow, containerAction())
			startTaskExecution(t, adapter, backend, scope, taskID, "execution-second", testNow.Add(time.Second), containerAction())
			startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())

			for _, executionID := range []id.ExecutionID{"execution-first", "execution-second"} {
				actionID := activateStartedWorkload(t, adapter, scope, executionID, testNow, id.WorkloadID("workload-"+string(executionID)))
				version := cancelWithPendingStop(t, adapter, scope, executionID, actionID, testNow.Add(time.Second), id.WorkloadID("workload-"+string(executionID)))
				if string(executionID) != testCase.unconfirmed {
					completeCleanup(t, adapter, scope, executionID, version, id.WorkloadID("workload-"+string(executionID)), testNow.Add(2*time.Second))
				}
			}

			unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
				Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
			})
			if err != nil {
				t.Fatalf("unfinished scan: %v", err)
			}
			for _, executionID := range unfinished {
				if executionID == "execution-replacement" {
					t.Fatalf("unfinished scan = %v; %q is unconfirmed so the replacement must stay held", unfinished, testCase.unconfirmed)
				}
			}
			_, err = engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
			requireClass(t, err, fault.VersionConflict)
		})
	}
}

// TestNonTerminalPredecessorBlocksTheReplacement proves a predecessor that can
// still start another action is never treated as stopped. It carries a
// confirmed action barrier, so treating terminality as irrelevant would
// wrongly release the replacement.
func TestNonTerminalPredecessorBlocksTheReplacement(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	recordRunningStopBarrier(t, adapter, scope, "execution-predecessor", actionID, 2, 2, testNow.Add(time.Second))
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())

	unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
	})
	if err != nil {
		t.Fatalf("unfinished scan: %v", err)
	}
	for _, executionID := range unfinished {
		if executionID == "execution-replacement" {
			t.Fatalf("unfinished scan = %v; a running predecessor still blocks dispatch", unfinished)
		}
	}
	_, err = engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")
}

// TestUnreadablePredecessorEvidenceFailsClosed proves malformed persisted stop
// evidence is never treated as confirmation.
func TestUnreadablePredecessorEvidenceFailsClosed(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())

	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         tableActions,
		ID:            string(actionID),
		ExpectVersion: 3,
		Values:        map[string]any{"stop": []byte("{not-json")},
	}}}); err != nil {
		t.Fatalf("corrupt predecessor stop evidence: %v", err)
	}

	if _, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
	}); err == nil {
		t.Fatal("unreadable stop evidence must fail the scan closed, not activate or silently skip")
	}
	_, err := engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
	if err == nil {
		t.Fatal("unreadable stop evidence must refuse the activation commit")
	}
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")
}

// bumpingController mutates a predecessor after the barrier plan read and
// before the activation unit executes, proving the activation unit carries a
// version guard on every predecessor it relied on.
type bumpingController struct {
	store.Controller
	once sync.Once
	bump func()
}

func (controller *bumpingController) Execute(ctx context.Context, executionID string, unit store.Unit) (store.UnitResult, error) {
	controller.once.Do(controller.bump)
	return controller.Controller.Execute(ctx, executionID, unit)
}

// TestActivationCommitGuardsPredecessorVersions proves the activation unit
// locks the predecessor rows it evaluated: a concurrent stop/cleanup commit
// between planning and execution is a version conflict, never a stale
// activation.
func TestActivationCommitGuardsPredecessorVersions(t *testing.T) {
	backend := memory.New()
	adapter := New(backend, backend)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	version := cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())
	confirmStopBarrier(t, adapter, scope, "execution-predecessor", actionID, version, 3, testNow.Add(2*time.Second))

	controller := &bumpingController{Controller: backend.Controller()}
	controller.bump = func() {
		completeCleanup(t, adapter, scope, "execution-predecessor", version+1, "workload-predecessor", testNow.Add(3*time.Second))
	}
	engineState := NewEngine(controller)

	_, err := engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")
}

// TestConfirmedStopBarrierReleasesTheReplacement proves the corrected rule:
// cleanup completion is sufficient but not necessary. A terminal predecessor
// whose every possibly started workload has confirmed stop evidence releases
// the replacement while its cleanup is still pending.
func TestConfirmedStopBarrierReleasesTheReplacement(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	version := cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())
	confirmStopBarrier(t, adapter, scope, "execution-predecessor", actionID, version, 3, testNow.Add(2*time.Second))

	unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
	})
	if err != nil {
		t.Fatalf("unfinished scan: %v", err)
	}
	found := false
	for _, executionID := range unfinished {
		if executionID == "execution-replacement" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unfinished scan = %v; a confirmed predecessor barrier must release the replacement", unfinished)
	}
	activated, err := engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(3*time.Second)))
	if err != nil {
		t.Fatalf("activation commit after confirmed barrier: %v", err)
	}
	if activated.State != kernel.ExecutionRunning || activated.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("activated replacement = %+v; want running", activated)
	}
	if _, err := engineState.Commit(context.Background(), activationCommitFor("execution-replacement", testNow.Add(4*time.Second))); err == nil {
		t.Fatal("a second activation attempt must not release the replacement twice")
	}
}

// TestKernelScanReleasesTheReplacementExactlyOnce drives the real Kernel
// reconciler over the store adapter. Before confirmation the authoritative
// scan runs (no event or broker delivery is involved) and the runtime is never
// asked to prepare or start. Confirmation then releases the replacement once,
// and a restart with no notification does not start it again.
func TestKernelScanReleasesTheReplacementExactlyOnce(t *testing.T) {
	backend := memory.New()
	adapter := New(backend, backend)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())

	runtime := &barrierRuntime{stop: kernel.PendingStop("stop response lost"), remove: engineRemoval{removed: true}}
	state := NewEngine(backend.Controller())
	reconciler := kernelengine.New(state, runtime, barrierWorkspace{}, barrierClock{at: testNow.Add(3 * time.Second)})

	// Authoritative polling only: the predecessor cleanup is driven, the
	// replacement is not discovered and no workload effect is issued.
	if err := reconciler.Scan(context.Background()); err != nil {
		t.Fatalf("scan with an unconfirmed predecessor: %v", err)
	}
	if prepares, starts := runtime.counts(); prepares != 0 || starts != 0 {
		t.Fatalf("unconfirmed predecessor prepared=%d started=%d; want no new work", prepares, starts)
	}
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-replacement")

	// The predecessor's own cleanup remains owed, proving the barrier did not
	// stop the scanner from progressing the predecessor.
	predecessor, err := adapter.Load(context.Background(), scope, "execution-predecessor")
	if err != nil {
		t.Fatalf("load predecessor: %v", err)
	}
	if predecessor.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("predecessor cleanup = %+v; want pending so the scan keeps driving it", predecessor.Cleanup)
	}

	// Later confirmation: the persisted action barrier is confirmed while
	// cleanup is still owed. The next authoritative scan releases the
	// replacement exactly once.
	current, err := adapter.Load(context.Background(), scope, "execution-predecessor")
	if err != nil {
		t.Fatalf("reload predecessor before confirmation: %v", err)
	}
	confirmStopBarrier(t, adapter, scope, "execution-predecessor", actionID,
		current.Version, current.Actions[0].Version, testNow.Add(4*time.Second))
	reconciler = kernelengine.New(NewEngine(backend.Controller()), runtime, barrierWorkspace{}, barrierClock{at: testNow.Add(4 * time.Second)})
	if err := reconciler.Scan(context.Background()); err != nil {
		t.Fatalf("scan after confirmation: %v", err)
	}
	if prepares, starts := runtime.counts(); prepares != 1 || starts != 1 {
		t.Fatalf("released replacement prepared=%d started=%d; want exactly one of each", prepares, starts)
	}
	activated, err := adapter.Load(context.Background(), scope, "execution-replacement")
	if err != nil {
		t.Fatalf("load released replacement: %v", err)
	}
	if activated.State != kernel.ExecutionRunning || activated.Actions[0].State != kernel.ActionRunning ||
		activated.Actions[0].ActivatedAt.IsZero() || activated.Actions[0].Deadline.IsZero() {
		t.Fatalf("released replacement = %+v; want one running activation with a persisted deadline", activated)
	}

	// Restart, still without any notification or broker delivery: the same
	// persisted state is scanned again and the workload is not started twice.
	runtime.setStop(kernel.ConfirmedStop("runtime confirmed termination"))
	restarted := kernelengine.New(NewEngine(backend.Controller()), runtime, barrierWorkspace{}, barrierClock{at: testNow.Add(5 * time.Second)})
	if err := restarted.Scan(context.Background()); err != nil {
		t.Fatalf("scan after restart: %v", err)
	}
	if prepares, starts := runtime.counts(); prepares != 1 || starts != 1 {
		t.Fatalf("restart scan prepared=%d started=%d; want the replacement released exactly once", prepares, starts)
	}
}

// TestUnrelatedWorkProgressesWhileTheBarrierHolds proves the predecessor
// barrier does not stop a task-free execution from being discovered.
func TestUnrelatedWorkProgressesWhileTheBarrierHolds(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-predecessor", testNow, containerAction())
	actionID := activateStartedWorkload(t, adapter, scope, "execution-predecessor", testNow, "workload-predecessor")
	cancelWithPendingStop(t, adapter, scope, "execution-predecessor", actionID, testNow.Add(time.Second), "workload-predecessor")
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-replacement", testNow.Add(2*time.Second), containerAction())
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, "execution-unrelated", "unrelated", containerAction())); err != nil {
		t.Fatalf("start unrelated execution: %v", err)
	}

	unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(3 * time.Second), Limit: 100,
	})
	if err != nil {
		t.Fatalf("unfinished scan: %v", err)
	}
	found := false
	for _, executionID := range unfinished {
		if executionID == "execution-unrelated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unfinished scan = %v; unrelated work must keep progressing", unfinished)
	}
}

// barrierConfirmed evaluates the predecessor barrier of one execution from its
// current row, the same way the activation commit does.
func barrierConfirmed(t *testing.T, engineState *Engine, executionID id.ExecutionID) bool {
	t.Helper()
	row, err := engineState.controller.Read(context.Background(), tableExecutions, string(executionID))
	if err != nil {
		t.Fatalf("read execution %q: %v", executionID, err)
	}
	confirmed, _, err := engineState.taskBarrier(context.Background(), executionID, row)
	if err != nil {
		t.Fatalf("task barrier of %q: %v", executionID, err)
	}
	return confirmed
}

// TestSameInstantQueuedSiblingsAreOrderedByIdentity pins the tie-break: two
// queued executions of one task created at the same instant do not hold each
// other forever. The lower identity goes first, the other waits for its
// confirmed stop, and they never run together.
func TestSameInstantQueuedSiblingsAreOrderedByIdentity(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-b", testNow, containerAction())
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-a", testNow, containerAction())

	unfinished, err := engineState.Scan(context.Background(), kernelengine.ScanRequest{
		Kind: kernelengine.ScanUnfinished, Now: testNow.Add(time.Second), Limit: 100,
	})
	if err != nil {
		t.Fatalf("unfinished scan: %v", err)
	}
	present := map[id.ExecutionID]bool{}
	for _, executionID := range unfinished {
		present[executionID] = true
	}
	if !present["execution-a"] || present["execution-b"] {
		t.Fatalf("unfinished scan = %v; want only the lower identity released", unfinished)
	}

	_, err = engineState.Commit(context.Background(), activationCommitFor("execution-b", testNow.Add(time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-b")

	activated, err := engineState.Commit(context.Background(), activationCommitFor("execution-a", testNow.Add(time.Second)))
	if err != nil {
		t.Fatalf("activation of the first same-instant sibling: %v", err)
	}
	if activated.State != kernel.ExecutionRunning {
		t.Fatalf("first same-instant sibling = %+v; want running", activated)
	}

	// The activated sibling now holds the other until its stop is confirmed.
	_, err = engineState.Commit(context.Background(), activationCommitFor("execution-b", testNow.Add(2*time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-b")

	actionID := snapshotdoc.DeriveActionID("execution-a", 0)
	version := cancelWithPendingStop(t, adapter, scope, "execution-a", actionID, testNow.Add(3*time.Second), "workload-a")
	_, err = engineState.Commit(context.Background(), activationCommitFor("execution-b", testNow.Add(3*time.Second)))
	requireClass(t, err, fault.VersionConflict)
	confirmStopBarrier(t, adapter, scope, "execution-a", actionID, version, 3, testNow.Add(4*time.Second))

	released, err := engineState.Commit(context.Background(), activationCommitFor("execution-b", testNow.Add(5*time.Second)))
	if err != nil {
		t.Fatalf("activation after the first sibling's confirmed stop: %v", err)
	}
	if released.State != kernel.ExecutionRunning {
		t.Fatalf("second same-instant sibling = %+v; want running", released)
	}
}

// TestSameInstantActivatedSiblingAlwaysBlocks proves the order is not a plain
// identity tie-break: an execution that has already been activated holds a
// same-instant sibling whatever their identities, and is not held by it.
func TestSameInstantActivatedSiblingAlwaysBlocks(t *testing.T) {
	cases := []struct {
		name      string
		activated id.ExecutionID
		queued    id.ExecutionID
	}{
		{name: "queued sibling has the lower identity", activated: "execution-b", queued: "execution-a"},
		{name: "queued sibling has the higher identity", activated: "execution-a", queued: "execution-b"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engineState, adapter, backend := newEngine(t)
			scope := projectScope()
			const taskID id.TaskID = "task-1"
			startTaskExecution(t, adapter, backend, scope, taskID, testCase.activated, testNow, containerAction(), requestAction())
			activateStartedWorkload(t, adapter, scope, testCase.activated, testNow, "workload-activated")
			startTaskExecution(t, adapter, backend, scope, taskID, testCase.queued, testNow, containerAction())

			if barrierConfirmed(t, engineState, testCase.queued) {
				t.Fatal("a same-instant sibling passed the barrier while the activated execution is still running")
			}
			_, err := engineState.Commit(context.Background(), activationCommitFor(testCase.queued, testNow.Add(time.Second)))
			requireClass(t, err, fault.VersionConflict)
			requireQueuedAndUnprepared(t, adapter, backend, scope, testCase.queued)

			if !barrierConfirmed(t, engineState, testCase.activated) {
				t.Fatal("a queued same-instant sibling held the already activated execution")
			}
		})
	}
}

// TestSameInstantActivationLocksTheQueuedSuccessor proves the execution that
// goes first guards the sibling it passed: if that sibling changes between the
// barrier read and the activation unit, the activation is a version conflict.
func TestSameInstantActivationLocksTheQueuedSuccessor(t *testing.T) {
	backend := memory.New()
	adapter := New(backend, backend)
	scope := projectScope()
	const taskID id.TaskID = "task-1"
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-a", testNow, containerAction())
	startTaskExecution(t, adapter, backend, scope, taskID, "execution-b", testNow, containerAction())

	controller := &bumpingController{Controller: backend.Controller()}
	controller.bump = func() {
		activateStartedWorkload(t, adapter, scope, "execution-b", testNow, "workload-b")
	}
	engineState := NewEngine(controller)

	_, err := engineState.Commit(context.Background(), activationCommitFor("execution-a", testNow.Add(time.Second)))
	requireClass(t, err, fault.VersionConflict)
	requireQueuedAndUnprepared(t, adapter, backend, scope, "execution-a")
}
