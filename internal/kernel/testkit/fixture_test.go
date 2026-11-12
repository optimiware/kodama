package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

func TestFixtureRestartSeesCommittedStateIntentsDeadlinesAndResources(t *testing.T) {
	fixture := NewFixture(testNow)
	executionID := id.ExecutionID("exec-1")
	workspaceID := id.WorkspaceID("ws-1")
	identity := testIdentity("exec-1", "act-1", "wl-1")

	fixture.Store.Seed(testExecution(
		"exec-1",
		"ws-1",
		testContainerAction("act-1", "build", 0, kernel.ActionPending, 0),
	))
	testPrepare(t, fixture.Runtime, identity, workspaceID)
	testStart(t, fixture.Runtime, identity, workspaceID)
	if err := fixture.Workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}

	deadline := testNow.Add(2 * time.Hour)
	activation := testCommit("exec-1", "act-1", 0, 0)
	activation.Deadline = deadline
	activation.Launch = &kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("wl-1"),
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-1",
	}
	if _, err := fixture.State.Commit(context.Background(), activation); err != nil {
		t.Fatalf("activation Commit() error = %v", err)
	}

	stopIntent := testCommit("exec-1", "act-1", 1, 1)
	stopIntent.Stop = &flow.StopIntent{
		WorkloadID:  id.WorkloadID("wl-1"),
		WorkspaceID: workspaceID,
	}
	if _, err := fixture.State.Commit(context.Background(), stopIntent); err != nil {
		t.Fatalf("stop intent Commit() error = %v", err)
	}

	restarted := fixture.Restart()
	loaded, err := restarted.Load(context.Background(), executionID)
	if err != nil {
		t.Fatalf("restarted Load() error = %v", err)
	}
	if loaded.Version != 2 {
		t.Fatalf("restarted execution version = %d, want 2", loaded.Version)
	}
	if loaded.State != kernel.ExecutionRunning {
		t.Fatalf("restarted execution state = %q, want running", loaded.State)
	}
	if !loaded.Actions[0].Deadline.Equal(deadline) {
		t.Fatalf("restarted deadline = %s, want %s", loaded.Actions[0].Deadline, deadline)
	}
	if loaded.Actions[0].Container.Launch.WorkloadID != "wl-1" {
		t.Fatalf("restarted launch workload = %q, want wl-1", loaded.Actions[0].Container.Launch.WorkloadID)
	}

	stops := fixture.Store.PendingStops()
	if len(stops) != 1 || stops[0].WorkloadID != "wl-1" || stops[0].WorkspaceID != workspaceID {
		t.Fatalf("pending stops = %+v, want one for wl-1/ws-1", stops)
	}

	workload, ok := fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Started {
		t.Fatalf("runtime resource after restart = %+v, want started", workload)
	}
	if !fixture.Workspace.IsRetained(workspaceID) {
		t.Fatal("workspace retention did not survive restart")
	}
}

func TestFixtureScanUnfinishedSurvivesRestart(t *testing.T) {
	fixture := NewFixture(testNow)
	settled := testContainerAction("act-settled", "build", 0, kernel.ActionSucceeded, 2)
	settled.Stop = kernel.ConfirmedStop("workload exited and stop was confirmed")
	window := testExecution("exec-window", "ws-window", settled,
		testContainerAction("act-next", "test", 1, kernel.ActionPending, 0))
	window.State = kernel.ExecutionRunning
	window.CurrentAction = -1
	window.Cleanup = kernel.Cleanup{State: kernel.CleanupNone}
	fixture.Store.Seed(window)

	scan := func(state *State, label string) {
		t.Helper()
		ids, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow})
		if err != nil {
			t.Fatalf("%s Scan(unfinished) error = %v", label, err)
		}
		if !equalExecutionIDs(ids, []id.ExecutionID{"exec-window"}) {
			t.Fatalf("%s Scan(unfinished) = %v, want [exec-window]", label, ids)
		}
	}

	scan(fixture.State, "before restart")
	scan(fixture.Restart(), "after restart")
}

func TestFixtureGuardBlocksReleaseUntilConfirmedStop(t *testing.T) {
	fixture := NewFixture(testNow)
	workspaceID := id.WorkspaceID("ws-1")
	identity := testIdentity("exec-1", "act-1", "wl-1")

	if err := fixture.Workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	testPrepare(t, fixture.Runtime, identity, workspaceID)
	testStart(t, fixture.Runtime, identity, workspaceID)

	if err := fixture.Workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() before stop error = %v, want ErrOwnershipHeld", err)
	}

	evidence, err := fixture.Runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatal("stop acknowledgment was treated as confirmation")
	}
	if err := fixture.Workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() after pending stop error = %v, want ErrOwnershipHeld", err)
	}

	fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.AbsentObservation())
	observation, err := fixture.Runtime.Observe(context.Background(), engine.ObserveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation kind = %q, want absent", observation.Kind)
	}
	if err := fixture.Workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() after absence error = %v, want ErrOwnershipHeld", err)
	}

	fixture.Runtime.ExpireLease(identity.WorkloadID)
	if err := fixture.Workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() after lease expiry error = %v, want ErrOwnershipHeld", err)
	}

	fixture.Runtime.ConfirmNextStop("termination observed")
	if _, err := fixture.Runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatalf("confirmed Stop() error = %v", err)
	}
	if err := fixture.Workspace.Release(context.Background(), workspaceID); err != nil {
		t.Fatalf("Release() after confirmed stop error = %v", err)
	}
	if !fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("confirmed stop did not permit workspace release")
	}
}

func TestFixtureInstancesAreIsolated(t *testing.T) {
	first := NewFixture(testNow)
	second := NewFixture(testNow)

	first.Store.Seed(testExecution("exec-1", "ws-1", testContainerAction("act-1", "build", 0, kernel.ActionPending, 0)))
	if _, ok := second.Store.Execution("exec-1"); ok {
		t.Fatal("second fixture saw the first fixture's state")
	}

	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")
	testPrepare(t, first.Runtime, identity, workspaceID)
	testStart(t, first.Runtime, identity, workspaceID)
	if err := first.Workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}

	if _, ok := second.Runtime.Workload(identity.WorkloadID); ok {
		t.Fatal("second fixture saw the first fixture's runtime resource")
	}
	if second.Workspace.IsRetained(workspaceID) {
		t.Fatal("second fixture saw the first fixture's workspace")
	}
}

func TestFixtureSecretDeliveryFailurePreventsPrepare(t *testing.T) {
	fixture := NewFixture(testNow)
	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")

	fixture.Secrets.FailNext(errors.New("vault unavailable"))
	if _, err := fixture.Runtime.Prepare(context.Background(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		Secrets: []engine.SecretValue{{
			Name:  id.SecretName("TOKEN"),
			Value: "super-secret-value",
		}},
	}); err == nil {
		t.Fatal("Prepare() error = nil, want secret delivery failure")
	}
	if fixture.Secrets.Attempts() != 1 {
		t.Fatalf("secret delivery attempts = %d, want 1", fixture.Secrets.Attempts())
	}
	if _, ok := fixture.Runtime.Workload(identity.WorkloadID); ok {
		t.Fatal("failed secret delivery created a workload resource")
	}
	if fixture.Runtime.PrepareCount() != 0 {
		t.Fatalf("PrepareCount() = %d, want 0", fixture.Runtime.PrepareCount())
	}

	if _, err := fixture.Runtime.Prepare(context.Background(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		Secrets: []engine.SecretValue{{
			Name:  id.SecretName("TOKEN"),
			Value: "super-secret-value",
		}},
	}); err != nil {
		t.Fatalf("retried Prepare() error = %v", err)
	}
	if fixture.Secrets.Attempts() != 2 {
		t.Fatalf("secret delivery attempts after retry = %d, want 2", fixture.Secrets.Attempts())
	}
	names := fixture.Secrets.Names(identity.WorkloadID)
	if len(names) != 1 || names[0] != id.SecretName("TOKEN") {
		t.Fatalf("delivered secret names = %v, want [TOKEN]", names)
	}
	if strings.Contains(fixture.Secrets.String(), "super-secret-value") {
		t.Fatalf("secret summary leaked the value: %q", fixture.Secrets.String())
	}
}

func TestBlockedStartBeforeEffectHasNoEffectUntilRelease(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	barrier := NewBarrier()
	runtime.BlockNextStartBeforeEffect(barrier)

	done := make(chan error, 1)
	go func() {
		_, err := runtime.Start(context.Background(), engine.StartRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		})
		done <- err
	}()

	<-barrier.Reached()
	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatal("prepared workload record disappeared while blocked")
	}
	if workload.Started || workload.StartCount != 0 {
		t.Fatalf("workload while blocked before effect = %+v, want no effect", workload)
	}

	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("Start() after release error = %v", err)
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload after release = %+v, want one effect", workload)
	}
}

func TestBlockedStartAfterEffectPreservesEffect(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	barrier := NewBarrier()
	runtime.BlockNextStartAfterEffect(barrier)

	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := runtime.Start(context.Background(), engine.StartRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		})
		done <- startOutcome{result: result, err: err}
	}()

	<-barrier.Reached()
	workload, _ := runtime.Workload(identity.WorkloadID)
	if !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload while blocked after effect = %+v, want one applied effect", workload)
	}

	barrier.Release()
	got := <-done
	if got.err != nil {
		t.Fatalf("Start() after release error = %v", got.err)
	}
	if got.result.Outcome != engine.StartAccepted {
		t.Fatalf("Start outcome = %q, want accepted", got.result.Outcome)
	}
}

func TestBlockedStartCancellationReturnsAndLeavesNoEffect(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	barrier := NewBarrier()
	runtime.BlockNextStartBeforeEffect(barrier)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Start(ctx, engine.StartRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		})
		done <- err
	}()

	<-barrier.Reached()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked Start after cancel error = %v, want context.Canceled", err)
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.Started || workload.StartCount != 0 {
		t.Fatalf("canceled blocked Start applied an effect: %+v", workload)
	}
	barrier.Release()
}

func TestBlockedStopAfterEffectKeepsRequest(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	barrier := NewBarrier()
	runtime.BlockNextStopAfterEffect(barrier)

	type stopOutcome struct {
		evidence kernel.StopEvidence
		err      error
	}
	done := make(chan stopOutcome, 1)
	go func() {
		evidence, err := runtime.Stop(context.Background(), engine.StopRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		})
		done <- stopOutcome{evidence: evidence, err: err}
	}()

	<-barrier.Reached()
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.StopRequests != 1 || workload.StopConfirmed {
		t.Fatalf("workload while blocked after stop = %+v, want one pending request", workload)
	}

	barrier.Release()
	got := <-done
	if got.err != nil {
		t.Fatalf("Stop() after release error = %v", got.err)
	}
	if got.evidence.IsConfirmed() {
		t.Fatal("Stop() fabricated confirmation")
	}
}

func TestBlockedStopCancellationKeepsRequestAndOwnership(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	barrier := NewBarrier()
	runtime.BlockNextStopAfterEffect(barrier)

	ctx, cancel := context.WithCancel(context.Background())
	type stopOutcome struct {
		evidence kernel.StopEvidence
		err      error
	}
	done := make(chan stopOutcome, 1)
	go func() {
		evidence, err := runtime.Stop(ctx, engine.StopRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		})
		done <- stopOutcome{evidence: evidence, err: err}
	}()

	<-barrier.Reached()
	cancel()
	got := <-done
	if got.err != nil {
		t.Fatalf("blocked Stop after cancel error = %v, want a lost-response evidence result", got.err)
	}
	if got.evidence.IsConfirmed() || got.evidence.State != kernel.StopStateUnconfirmed {
		t.Fatalf("blocked Stop after cancel evidence = %+v, want unconfirmed", got.evidence)
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.StopRequests != 1 || workload.StopConfirmed {
		t.Fatalf("canceled blocked Stop = %+v, want one pending request", workload)
	}
	if err := runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() after canceled stop = %v, want ErrOwnershipHeld", err)
	}
	barrier.Release()
}
