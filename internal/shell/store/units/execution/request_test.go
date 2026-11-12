package executionstore

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/request"
)

func TestActivateRoundTripWritesRunningRequestAction(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	unit := activationUnit(scope, "exec-1", actions[1], "activate-1")

	result, err := adapter.Activate(context.Background(), unit)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if result.ExecutionVersion != 2 || result.ActionVersion != 2 || !result.Deadline.Equal(unit.Deadline) {
		t.Fatalf("result = %+v; want versions 2 and the persisted deadline", result)
	}

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionRunning || aggregate.Version != 2 || aggregate.CurrentAction != 1 {
		t.Fatalf("execution = %+v; want running version 2 current 1", aggregate)
	}
	action := aggregate.Actions[1]
	if action.State != kernel.ActionRunning || action.Version != 2 {
		t.Fatalf("action = %+v; want running version 2", action)
	}
	if !action.Deadline.Equal(unit.Deadline) || action.Timeout != unit.Timeout {
		t.Fatalf("deadline = %s timeout = %s", action.Deadline, action.Timeout)
	}
	if action.Stop.State != kernel.StopStateConfirmed || action.Stop.Detail != "workload stopped" {
		t.Fatalf("stop evidence = %+v; want the confirmed predecessor stop", action.Stop)
	}
	if action.Request == nil || action.Request.Subject == nil {
		t.Fatalf("request action = %+v; want a resolved subject", action.Request)
	}
	fields := action.Request.Subject.Fields()
	if fields["artifact"].Literal != "artifact-1" {
		t.Fatalf("subject = %+v; want literal artifact-1", fields)
	}
	if action.Request.Answer != nil {
		t.Fatalf("activated request already has an answer: %+v", action.Request.Answer)
	}

	row, err := backend.Read(context.Background(), scope, "actions", string(actions[1]))
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if row.Values["request_assignment"] == nil || row.Values["request_subject"] == nil {
		t.Fatalf("assignment/subject columns missing: %v", row.Values)
	}
	view, err := adapter.Inspect(context.Background(), scope, "exec-1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if view.Actions[1].Decision != "" || view.Actions[1].Subject == nil {
		t.Fatalf("view action = %+v", view.Actions[1])
	}
}

func TestActivateStaleVersionConflictsWithoutPartialState(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	unit := activationUnit(scope, "exec-1", actions[1], "activate-1")
	unit.ExpectedExecutionVersion = 9

	_, err := adapter.Activate(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued || aggregate.Actions[1].State != kernel.ActionPending {
		t.Fatalf("stale activation changed state: %+v", aggregate)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-activate-1"); ok {
		t.Fatal("stale activation must not publish its event intent")
	}
}

func TestActivateReplaysEqualReceipt(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	unit := activationUnit(scope, "exec-1", actions[1], "activate-1")

	first, err := adapter.Activate(context.Background(), unit)
	if err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	second, err := adapter.Activate(context.Background(), unit)
	if err != nil {
		t.Fatalf("second Activate: %v", err)
	}
	if !second.Replayed || second.ExecutionVersion != first.ExecutionVersion || second.ActionVersion != first.ActionVersion {
		t.Fatalf("replay = %+v; want %+v replayed", second, first)
	}
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Actions[1].Version != 2 {
		t.Fatalf("replay applied a second write: %+v", aggregate.Actions[1])
	}
}

func TestActivateChangedReceiptContentIsRefused(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	unit := activationUnit(scope, "exec-1", actions[1], "activate-1")
	if _, err := adapter.Activate(context.Background(), unit); err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	changed := activationUnit(scope, "exec-1", actions[1], "activate-1")
	changed.Receipt.ContentHash = contentHash("c")
	_, err := adapter.Activate(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
}

func TestActivateRefusesCrossScopeUnit(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	_, err := adapter.Activate(context.Background(), activationUnit(otherScope(), "exec-1", actions[1], "activate-1"))
	requireClass(t, err, fault.VersionConflict)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 1 {
		t.Fatalf("cross-scope activation changed the execution: %+v", aggregate)
	}
}

func TestActivateRollsBackEveryEffectOnCommitFailure(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())
	unit := activationUnit(scope, "exec-1", actions[1], "activate-1")

	backend.FailNextCommit(fault.New(fault.DependencyUnavailable, "simulated commit failure"))
	_, err := adapter.Activate(context.Background(), unit)
	requireClass(t, err, fault.DependencyUnavailable)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Actions[1].State != kernel.ActionPending {
		t.Fatalf("failed activation changed the action: %+v", aggregate.Actions[1])
	}
	if _, err := adapter.Activate(context.Background(), unit); err != nil {
		t.Fatalf("retry after failed commit: %v", err)
	}
}

func TestActivateConcurrentOneWinner(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction(), requestAction())

	results := make([]request.ActivationResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	var gate sync.WaitGroup
	gate.Add(1)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			unit := activationUnit(scope, "exec-1", actions[1], "activate-race-"+string(rune('a'+index)))
			gate.Wait()
			results[index], errs[index] = adapter.Activate(context.Background(), unit)
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
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Actions[1].Version != 2 {
		t.Fatalf("action version = %d; want exactly one applied activation", aggregate.Actions[1].Version)
	}
}
