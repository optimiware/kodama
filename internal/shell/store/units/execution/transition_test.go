package executionstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func transitionUnit(scope tenancy.Scope, executionID id.ExecutionID, commit flow.Commit, key string) execution.TransitionUnit {
	return execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              executionID,
		ExpectedExecutionVersion: commit.ExpectedExecutionVersion,
		ExpectedActionVersion:    commit.ExpectedActionVersion,
		Commit:                   commit,
		Receipt:                  testReceipt(scope, key, "d"),
		Audit:                    []event.Audit{testAudit(scope, key)},
		Events:                   []event.Envelope{testEvent(scope, key, string(executionID))},
	}
}

func TestTransitionStagesThenPromotesOutputsAndClearsCredential(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	action := actions[0]

	stage := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action,
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"artifact": "built"}},
		Deadline:                 testNow.Add(90 * time.Second),
		At:                       testNow,
	}
	unit := transitionUnit(scope, "exec-1", stage, "stage-1")
	unit.CredentialUpdate = &token.Credential{Nonce: make([]byte, 32), Expiry: testNow.Add(time.Hour)}
	if _, err := adapter.Transition(context.Background(), unit); err != nil {
		t.Fatalf("stage transition: %v", err)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionRunning || aggregate.Version != 2 {
		t.Fatalf("execution = %+v; want running version 2", aggregate)
	}
	if aggregate.Actions[0].State != kernel.ActionRunning || aggregate.Actions[0].Version != 2 {
		t.Fatalf("action = %+v; want running version 2", aggregate.Actions[0])
	}
	if !aggregate.Actions[0].StagedOutputsSet || aggregate.Actions[0].StagedOutputs["artifact"] != "built" {
		t.Fatalf("staged outputs = %+v set=%v", aggregate.Actions[0].StagedOutputs, aggregate.Actions[0].StagedOutputsSet)
	}
	if aggregate.Actions[0].ActivatedAt.IsZero() || !aggregate.Actions[0].Deadline.Equal(testNow.Add(90*time.Second)) {
		t.Fatalf("deadline = %s activated = %s", aggregate.Actions[0].Deadline, aggregate.Actions[0].ActivatedAt)
	}
	row, err := backend.Read(context.Background(), scope, "executions", "exec-1")
	if err != nil {
		t.Fatalf("read executions: %v", err)
	}
	if row.Values["token_nonce"] == nil || row.Values["token_expires_at"] == nil {
		t.Fatalf("credential pair was not written: %v", row.Values)
	}

	promote := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionSucceeded,
		ActionID:                 action,
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionSucceeded,
		Output:                   flow.OutputChange{Action: flow.OutputPromote, Values: kernel.Outputs{"artifact": "built"}},
		InvalidateCredential:     true,
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", promote, "promote-1")); err != nil {
		t.Fatalf("promote transition: %v", err)
	}
	aggregate = loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Actions[0].Outputs["artifact"] != "built" || aggregate.Actions[0].StagedOutputsSet || aggregate.Actions[0].StagedOutputs != nil {
		t.Fatalf("settled outputs = %+v staged=%+v set=%v", aggregate.Actions[0].Outputs, aggregate.Actions[0].StagedOutputs, aggregate.Actions[0].StagedOutputsSet)
	}
	row, err = backend.Read(context.Background(), scope, "executions", "exec-1")
	if err != nil {
		t.Fatalf("read executions: %v", err)
	}
	if row.Values["token_nonce"] != nil || row.Values["token_expires_at"] != nil {
		t.Fatalf("credential pair was not cleared: %v", row.Values)
	}
}

func TestTransitionCancelPersistsCleanupAndPendingCancellations(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())

	cancel := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		NextCurrentAction:        new(-1),
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		PendingCancellations: []flow.ActionUpdate{
			{ActionID: actions[1], ExpectedVersion: 1, NextState: kernel.ActionCancelled},
		},
		Cleanup:       &flow.CleanupIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1", Reason: "cancelled"},
		CleanupUpdate: &flow.CleanupUpdate{State: kernel.CleanupPending, WorkloadID: "wl-1", WorkspaceID: "ws-1"},
		At:            testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", cancel, "cancel-1")); err != nil {
		t.Fatalf("cancel transition: %v", err)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionCancelled || aggregate.CurrentAction != -1 {
		t.Fatalf("execution = %+v; want cancelled with no current action", aggregate)
	}
	if aggregate.Actions[0].State != kernel.ActionPending || aggregate.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("actions = %+v", aggregate.Actions)
	}
	if aggregate.Cleanup.State != kernel.CleanupPending || aggregate.Cleanup.WorkloadID != "wl-1" || aggregate.Cleanup.WorkspaceID != "ws-1" {
		t.Fatalf("cleanup = %+v; want pending wl-1/ws-1", aggregate.Cleanup)
	}
}

func TestTransitionStaleVersionConflictsWithoutPartialState(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())

	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 99,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    99,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	_, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", commit, "stale-1"))
	requireClass(t, err, fault.VersionConflict)

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionQueued || aggregate.Version != 1 || aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("stale commit changed state: %+v", aggregate)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-stale-1"); ok {
		t.Fatal("stale commit must not publish its event intent")
	}
}

func TestTransitionPersistsLaunchObservationAndFailure(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	observation := kernel.ExitedObservation(0, kernel.ConfirmedStop("exit observed"))
	activate := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Launch:                   &kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchIssued, IdempotencyKey: "start-key"},
		Observation:              &observation,
		Deadline:                 testNow.Add(90 * time.Second),
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", activate, "activate-evidence")); err != nil {
		t.Fatalf("activate transition: %v", err)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	action := aggregate.Actions[0]
	if action.Container == nil {
		t.Fatalf("action has no container value: %+v", action)
	}
	if action.Container.WorkloadID != "wl-1" {
		t.Fatalf("workload id = %q; want wl-1", action.Container.WorkloadID)
	}
	if action.Container.Launch != (kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchIssued, IdempotencyKey: "start-key"}) {
		t.Fatalf("launch = %+v; want the exact persisted intent", action.Container.Launch)
	}
	if action.Container.Observation.Kind != kernel.ObservationExited {
		t.Fatalf("observation = %+v; want exited", action.Container.Observation)
	}
	if !action.ActivatedAt.Equal(testNow) {
		t.Fatalf("activated at = %s; want %s", action.ActivatedAt, testNow)
	}

	failure := &kernel.Failure{Kind: kernel.FailureTimeout, Reason: "deadline_reached", Detail: "reached"}
	fail := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 actions[0],
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionFailed,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Failure:                  failure,
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", fail, "fail-evidence")); err != nil {
		t.Fatalf("failure transition: %v", err)
	}
	aggregate = loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Failure == nil || *aggregate.Failure != *failure {
		t.Fatalf("failure = %+v; want %+v", aggregate.Failure, failure)
	}
	if aggregate.State != kernel.ExecutionFailed || aggregate.Actions[0].State != kernel.ActionFailed {
		t.Fatalf("execution/action state = %s/%s", aggregate.State, aggregate.Actions[0].State)
	}
}

func TestTransitionReplaysEqualReceipt(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"artifact": "a"}},
		At:                       testNow,
	}
	unit := transitionUnit(scope, "exec-1", commit, "replay-1")
	first, err := adapter.Transition(context.Background(), unit)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	second, err := adapter.Transition(context.Background(), unit)
	if err != nil {
		t.Fatalf("second transition: %v", err)
	}
	if !second.Replayed || second.ExecutionVersion != first.ExecutionVersion || second.ActionVersion != first.ActionVersion {
		t.Fatalf("replay = %+v; want %+v replayed", second, first)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 2 {
		t.Fatalf("replay applied a second write: version %d", aggregate.Version)
	}
}

func TestTransitionRefusesCrossScopeUnit(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	_, err := adapter.Transition(context.Background(), transitionUnit(otherScope(), "exec-1", commit, "cross-1"))
	requireClass(t, err, fault.VersionConflict)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 1 {
		t.Fatalf("cross-scope unit changed the execution: %+v", aggregate)
	}
}

func TestTransitionConcurrentOneWinner(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())

	results := make([]execution.TransitionResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	var gate sync.WaitGroup
	gate.Add(1)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			commit := flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 actions[0],
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				Output:                   flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"artifact": "a"}},
				At:                       testNow,
			}
			gate.Wait()
			results[index], errs[index] = adapter.Transition(
				context.Background(),
				transitionUnit(scope, "exec-1", commit, "race-"+string(rune('a'+index))),
			)
		}(index)
	}
	gate.Done()
	wait.Wait()

	winners := 0
	for index := range errs {
		if errs[index] == nil {
			winners++
			continue
		}
		requireClass(t, errs[index], fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly 1", winners)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("version = %d; want exactly one applied transition", aggregate.Version)
	}
}

func TestTransitionChangedReceiptContentIsRefused(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"artifact": "a"}},
		At:                       testNow,
	}
	unit := transitionUnit(scope, "exec-1", commit, "changed-1")
	if _, err := adapter.Transition(context.Background(), unit); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	changed := transitionUnit(scope, "exec-1", commit, "changed-1")
	changed.Receipt.ContentHash = contentHash("c")
	_, err := adapter.Transition(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 2 {
		t.Fatalf("changed content applied a second write: version %d", aggregate.Version)
	}
}

func TestTransitionOutputStageVersusSettlementRace(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	activate := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-1", activate, "run-1")); err != nil {
		t.Fatalf("activate: %v", err)
	}

	stage := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"artifact": "staged"}},
		At:                       testNow,
	}
	settle := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 2,
		NextExecutionState:       kernel.ExecutionSucceeded,
		ActionID:                 actions[0],
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionSucceeded,
		Output:                   flow.OutputChange{Action: flow.OutputPromote, Values: kernel.Outputs{"artifact": "settled"}},
		InvalidateCredential:     true,
		At:                       testNow,
	}
	units := []execution.TransitionUnit{
		transitionUnit(scope, "exec-1", stage, "race-stage"),
		transitionUnit(scope, "exec-1", settle, "race-settle"),
	}
	errs := make([]error, len(units))
	var wait sync.WaitGroup
	var gate sync.WaitGroup
	gate.Add(1)
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			gate.Wait()
			_, errs[index] = adapter.Transition(context.Background(), units[index])
		}(index)
	}
	gate.Done()
	wait.Wait()

	winners := 0
	for index := range errs {
		if errs[index] == nil {
			winners++
			continue
		}
		requireClass(t, errs[index], fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly one of stage/settle", winners)
	}
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	action := aggregate.Actions[0]
	if action.Version != 3 {
		t.Fatalf("action version = %d; want exactly one applied write", action.Version)
	}
	staged := action.StagedOutputsSet && action.StagedOutputs["artifact"] == "staged"
	settled := action.Outputs["artifact"] == "settled" && !action.StagedOutputsSet
	if staged == settled {
		t.Fatalf("exactly one outcome must be visible; staged=%v settled=%v action=%+v", staged, settled, action)
	}
}
