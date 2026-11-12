package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestCancelRunningExecutionBuildsCompleteTransition(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator)
	content := receiptContent(`{"execution_id":"exec-1","expected_execution_version":1}`)

	result, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  content,
	})
	requireNoError(t, err, "Cancel")
	if result.NextExecution != kernel.ExecutionCancelled {
		t.Fatalf("next execution = %q; want cancelled", result.NextExecution)
	}
	if result.NextAction != kernel.ActionCancelled {
		t.Fatalf("next action = %q; want cancelled", result.NextAction)
	}

	if fixture.transitions.callCount() != 1 {
		t.Fatalf("transition port calls = %d; want one", fixture.transitions.callCount())
	}
	unit := fixture.transitions.captured()[0]
	if unit.Owner != scope {
		t.Fatalf("unit owner = %+v; want %+v", unit.Owner, scope)
	}
	if unit.ExecutionID != aggregate.ID || unit.ExpectedExecutionVersion != aggregate.Version {
		t.Fatalf("unit execution = %q@%d; want %q@%d", unit.ExecutionID, unit.ExpectedExecutionVersion, aggregate.ID, aggregate.Version)
	}
	if unit.ExpectedActionVersion != aggregate.Actions[0].Version {
		t.Fatalf("unit action version = %d; want %d", unit.ExpectedActionVersion, aggregate.Actions[0].Version)
	}
	if unit.Commit.NextExecutionState != kernel.ExecutionCancelled || unit.Commit.NextActionState != kernel.ActionCancelled {
		t.Fatalf("commit states = %q/%q; want cancelled/cancelled", unit.Commit.NextExecutionState, unit.Commit.NextActionState)
	}
	if !unit.Commit.InvalidateCredential {
		t.Fatal("cancel did not revoke execution authority in the commit")
	}
	if unit.Commit.Stop == nil {
		t.Fatal("cancel of a running container did not request a stop")
	}
	if unit.Commit.Stop.WorkloadID != "workload-1" || unit.Commit.Stop.WorkspaceID != aggregate.WorkspaceID {
		t.Fatalf("stop intent = %+v; want workload-1 in %q", unit.Commit.Stop, aggregate.WorkspaceID)
	}
	if unit.Commit.StopEvidence != nil {
		t.Fatal("cancel fabricated stop evidence; a stop request is not confirmation")
	}
	if unit.Commit.Cleanup == nil || unit.Commit.Cleanup.WorkloadID != "workload-1" {
		t.Fatalf("cancel cleanup = %+v; want cleanup intent for workload-1", unit.Commit.Cleanup)
	}
	if unit.Commit.CleanupUpdate == nil || unit.Commit.CleanupUpdate.State != kernel.CleanupPending {
		t.Fatalf("cancel cleanup progress = %+v; want pending", unit.Commit.CleanupUpdate)
	}

	requireReceipt(t, unit.Receipt, scope, actor.ID, "execution.cancel", "cancel-key-1", content, testNow)
	requireAudit(t, unit.Audit, scope, actor.ID, "execution.cancel", testNow)
	requireEvent(t, unit.Events, event.TypeExecutionCancelled, scope, string(aggregate.ID), actor.ID, testNow)
	requireAuthority(t, unit.Authority, "execution.cancel", authorize.CheckMembership)
	requireAuthority(t, unit.Authority, "execution.cancel", authorize.CheckExecutionVersion)
	requirePostCommit(t, unit.PostCommit, event.PostPublishEvent)

	committed, ok := fixture.store.Execution(aggregate.ID)
	if !ok {
		t.Fatal("cancelled execution disappeared")
	}
	if committed.State != kernel.ExecutionCancelled {
		t.Fatalf("stored execution = %q; want cancelled", committed.State)
	}
	if committed.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("stored cleanup = %+v; want pending", committed.Cleanup)
	}
	if committed.Actions[0].Stop.IsConfirmed() {
		t.Fatal("stored action reports confirmed stop without observation")
	}
}

func TestCancelQueuedExecutionCancelsPendingActionsWithoutStop(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := baseExecution(
		containerAction("act-0", "build", 0, time.Hour, "candidate"),
		containerAction("act-1", "test", 1, time.Hour, "result"),
	)
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireNoError(t, err, "Cancel queued")
	unit := fixture.transitions.captured()[0]
	if unit.Commit.Stop != nil {
		t.Fatalf("queued cancel requested a stop: %+v", unit.Commit.Stop)
	}
	if len(unit.Commit.PendingCancellations) != 2 {
		t.Fatalf("pending cancellations = %d; want both pending actions", len(unit.Commit.PendingCancellations))
	}
	for _, update := range unit.Commit.PendingCancellations {
		if update.NextState != kernel.ActionCancelled {
			t.Fatalf("pending action %q next state = %q; want cancelled", update.ActionID, update.NextState)
		}
	}
}

func TestCancelRefusesStaleExpectedVersion(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version + 1,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestCancelRefusesCrossScopeExecution(t *testing.T) {
	fixture := newTestService(t)
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(projectScope("tenant-1", "project-1"), aggregate)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-2", "project-9", "principal-9", tenancy.RoleOperator),
		Scope:                    projectScope("tenant-2", "project-9"),
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.NotFound)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestCancelUnknownExecutionIsNotTreatedAsTerminal(t *testing.T) {
	fixture := newTestService(t)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    projectScope("tenant-1", "project-1"),
		ExecutionID:              id.ExecutionID("missing-execution"),
		ExpectedExecutionVersion: 1,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.NotFound)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}

func TestCancelCrashBeforeEffectThenRetrySucceedsOnce(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(scope, aggregate)
	fixture.store.failNextBefore(fault.New(fault.DependencyUnavailable, "commit store unavailable"))
	command := execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	}
	if _, err := fixture.service.Cancel(context.Background(), command); err == nil {
		t.Fatal("cancel unexpectedly succeeded across a storage failure")
	}
	before, _ := fixture.store.Execution(aggregate.ID)
	if before.State != kernel.ExecutionRunning {
		t.Fatalf("execution state after crash before effect = %q; want running", before.State)
	}
	if fixture.store.commitCount() != 0 {
		t.Fatalf("commits after crash before effect = %d; want zero", fixture.store.commitCount())
	}

	if _, err := fixture.service.Cancel(context.Background(), command); err != nil {
		t.Fatalf("retried Cancel: %v", err)
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after retry = %d; want one", fixture.store.commitCount())
	}
	after, _ := fixture.store.Execution(aggregate.ID)
	if after.State != kernel.ExecutionCancelled {
		t.Fatalf("execution state after retry = %q; want cancelled", after.State)
	}
}

func TestCancelLostResponseAfterEffectConvergesWithoutSecondCommit(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(scope, aggregate)
	fixture.store.failNextAfter(fault.New(fault.DependencyUnavailable, "response lost after commit"))
	command := execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	}
	if _, err := fixture.service.Cancel(context.Background(), command); err == nil {
		t.Fatal("cancel unexpectedly returned a response across a lost commit response")
	}
	committed, _ := fixture.store.Execution(aggregate.ID)
	if committed.State != kernel.ExecutionCancelled {
		t.Fatalf("state after lost response = %q; want cancelled (effect committed)", committed.State)
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after lost response = %d; want one", fixture.store.commitCount())
	}

	result, err := fixture.service.Cancel(context.Background(), command)
	requireNoError(t, err, "retried Cancel")
	if !result.Replayed {
		t.Fatal("identical cancel retry did not report a replay")
	}
	if fixture.store.commitCount() != 1 {
		t.Fatalf("commits after retry = %d; want one (no second effect)", fixture.store.commitCount())
	}
	if fixture.transitions.callCount() != 1 {
		t.Fatalf("transition port calls = %d; want one", fixture.transitions.callCount())
	}
}

func TestCancelDuplicateReturnsRecordedResult(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(scope, aggregate)
	command := execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	}
	if _, err := fixture.service.Cancel(context.Background(), command); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	result, err := fixture.service.Cancel(context.Background(), command)
	requireNoError(t, err, "duplicate Cancel")
	if !result.Replayed {
		t.Fatal("duplicate cancel did not report a replay")
	}
	if fixture.transitions.callCount() != 1 {
		t.Fatalf("transition port calls = %d; want one", fixture.transitions.callCount())
	}
}

func TestCancelRefusesSettledSuccessWithoutReopening(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := baseExecution(containerAction("act-0", "build", 0, time.Hour))
	aggregate.State = kernel.ExecutionSucceeded
	aggregate.Actions[0].State = kernel.ActionSucceeded
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClassIn(t, err, fault.ChangedIdempotencyContent, fault.InvalidInput)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
	stored, _ := fixture.store.Execution(aggregate.ID)
	if stored.State != kernel.ExecutionSucceeded {
		t.Fatalf("settled execution state = %q; want succeeded", stored.State)
	}
}

func TestCancelRefusesWithoutAuthority(t *testing.T) {
	fixture := newTestService(t)
	fixture.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour))
	fixture.store.Seed(scope, aggregate)
	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    humanActor("tenant-1", "project-1", "principal-1"),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.Forbidden)
	if fixture.transitions.callCount() != 0 {
		t.Fatalf("transition port calls = %d; want zero", fixture.transitions.callCount())
	}
}
