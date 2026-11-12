package executionstore

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestStartRoundTripPersistsQueuedExecutionAndActions(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, "exec-1", "start-1", containerAction(), requestAction())
	unit.Snapshot = testSnapshot("task-1", []byte("conversation bytes\x00\x01"))

	result, err := adapter.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.ExecutionID != "exec-1" || result.PipelineVersion != 3 || result.Replayed {
		t.Fatalf("result = %+v; want exec-1 version 3 fresh", result)
	}

	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.State != kernel.ExecutionQueued || aggregate.Version != 1 {
		t.Fatalf("execution = %+v; want queued version 1", aggregate)
	}
	if aggregate.CurrentAction != -1 {
		t.Fatalf("current action = %d; want -1", aggregate.CurrentAction)
	}
	if len(aggregate.Actions) != 2 {
		t.Fatalf("actions = %d; want 2", len(aggregate.Actions))
	}
	build := aggregate.Actions[0]
	if build.ID != snapshotdoc.DeriveActionID("exec-1", 0) || build.Name != "build" || build.State != kernel.ActionPending {
		t.Fatalf("build action = %+v", build)
	}
	if build.Container == nil || build.Container.Definition.Image != containerAction().Container.Image {
		t.Fatalf("container definition did not round-trip: %+v", build.Container)
	}
	if build.Timeout.String() != "1m30s" {
		t.Fatalf("timeout = %s; want 1m30s", build.Timeout)
	}
	approve := aggregate.Actions[1]
	if approve.Request == nil || approve.Request.Definition.Assignment != "role/reviewer" {
		t.Fatalf("request definition did not round-trip: %+v", approve.Request)
	}
	if got := string(aggregate.Snapshot.YAML()); got != "name: build\non: push\n" {
		t.Fatalf("yaml = %q; want exact admitted bytes", got)
	}
	if got := aggregate.Snapshot.Inputs()["target"]; got != "linux" {
		t.Fatalf("input = %q; want linux", got)
	}
	if got := aggregate.Snapshot.Runtime().Capabilities; len(got) != 1 || got[0] != "process" {
		t.Fatalf("runtime = %v; want [process]", got)
	}
	task, present := aggregate.Snapshot.Task()
	if !present || string(task) != "conversation bytes\x00\x01" {
		t.Fatalf("task = %q present=%v; want exact admitted bytes", task, present)
	}
	if aggregate.Snapshot.TaskID() != "task-1" {
		t.Fatalf("task id = %q; want task-1", aggregate.Snapshot.TaskID())
	}
	if aggregate.Cleanup.State != kernel.CleanupNone {
		t.Fatalf("cleanup = %+v; want none", aggregate.Cleanup)
	}

	view, err := adapter.Inspect(context.Background(), scope, "exec-1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if view.PipelineID != pipelineIDFor(scope) || view.TaskID != "task-1" || len(view.Actions) != 2 {
		t.Fatalf("view = %+v", view)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-start-1"); !ok {
		t.Fatal("queued event intent was not committed with the execution")
	}
	if _, ok := backend.Snapshot(scope, "audit", "audit-start-1"); !ok {
		t.Fatal("audit row was not committed with the execution")
	}
}

func TestStartDerivesStableWorkspaceForContainerActions(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-1", "start-ws", containerAction(), requestAction())); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := snapshotdoc.DeriveWorkspaceID("exec-1")
	if want == "" {
		t.Fatal("derived workspace identity is empty")
	}

	first := loadExecution(t, adapter, scope, "exec-1")
	if first.WorkspaceID != want {
		t.Fatalf("workspace = %q; want %q", first.WorkspaceID, want)
	}
	if first.Actions[0].Container == nil || first.Actions[0].Container.WorkspaceID != want {
		t.Fatalf("container workspace = %+v; want %q", first.Actions[0].Container, want)
	}
	second := loadExecution(t, adapter, scope, "exec-1")
	if second.WorkspaceID != want {
		t.Fatalf("workspace changed between loads: %q then %q", first.WorkspaceID, second.WorkspaceID)
	}

	// A planned wait keeps the same execution, so it keeps the same workspace.
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-wait", "start-wait", requestOnlyAction())); err != nil {
		t.Fatalf("Start request-only: %v", err)
	}
	waiting := loadExecution(t, adapter, scope, "exec-wait")
	if waiting.WorkspaceID != snapshotdoc.DeriveWorkspaceID("exec-wait") {
		t.Fatalf("planned-wait workspace = %q; want the same derived workspace", waiting.WorkspaceID)
	}
}

func TestStartReplaysEqualReceiptWithoutSecondWrite(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, "exec-1", "start-1", containerAction())

	first, err := adapter.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second, err := adapter.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !second.Replayed || second.ExecutionID != first.ExecutionID || second.PipelineVersion != first.PipelineVersion {
		t.Fatalf("replay = %+v; want %+v replayed", second, first)
	}

	changed := startUnit(scope, "exec-2", "start-1", containerAction())
	changed.Receipt.ContentHash = contentHash("c")
	_, err = adapter.Start(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	if _, err := adapter.Load(context.Background(), scope, "exec-2"); err == nil {
		t.Fatal("changed receipt content must not commit a second execution")
	}
}

func TestStartStalePipelineVersionConflictsWithoutPartialState(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, "exec-1", "start-1", containerAction())
	unit.ExpectedPipelineVersion = 2

	_, err := adapter.Start(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, err := adapter.Load(context.Background(), scope, "exec-1"); err == nil {
		t.Fatal("stale pipeline version must not commit the execution")
	}
	if _, ok := backend.Snapshot(scope, "events", "event-start-1"); ok {
		t.Fatal("stale pipeline version must not commit the event intent")
	}
}

func TestStartRefusesCrossScopeReads(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-1", "start-1", containerAction())); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := adapter.Load(context.Background(), otherScope(), "exec-1")
	requireClass(t, err, fault.NotFound)
	_, err = adapter.Load(context.Background(), tenancy.Scope{TenantID: "tenant-1"}, "exec-1")
	requireClass(t, err, fault.InvalidInput)
}

func TestStartRollsBackEveryEffectOnCommitFailure(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, "exec-1", "start-1", containerAction())

	backend.FailNextCommit(fault.New(fault.DependencyUnavailable, "simulated commit failure"))
	_, err := adapter.Start(context.Background(), unit)
	requireClass(t, err, fault.DependencyUnavailable)
	if _, err := adapter.Load(context.Background(), scope, "exec-1"); err == nil {
		t.Fatal("failed commit must leave no execution row")
	}
	if _, ok := backend.Snapshot(scope, "receipts", "receipt-start-1"); ok {
		t.Fatal("failed commit must leave no receipt")
	}

	if _, err := adapter.Start(context.Background(), unit); err != nil {
		t.Fatalf("retry after failed commit: %v", err)
	}
}

func TestStartConcurrentDuplicateReceiptCommitsOnce(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)

	units := []struct {
		execution id.ExecutionID
		suffix    string
	}{
		{"exec-a", "start-a"},
		{"exec-b", "start-b"},
	}
	results := make([]execution.StartResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for index, entry := range units {
		wait.Add(1)
		go func(index int, value id.ExecutionID) {
			defer wait.Done()
			unit := startUnit(scope, value, "start-race", containerAction())
			unit.Receipt.ID = id.ReceiptID("receipt-start-race")
			unit.Audit = []event.Audit{testAudit(scope, "start-race")}
			unit.Events = []event.Envelope{testEvent(scope, "start-race", string(value))}
			start.Wait()
			results[index], errs[index] = adapter.Start(context.Background(), unit)
		}(index, entry.execution)
	}
	start.Done()
	wait.Wait()

	for index := range errs {
		if errs[index] != nil {
			t.Fatalf("start %d: %v", index, errs[index])
		}
	}
	winner := results[0].ExecutionID
	if winner != "exec-a" && winner != "exec-b" {
		t.Fatalf("winner = %q", winner)
	}
	loser := id.ExecutionID("exec-a")
	if winner == "exec-a" {
		loser = "exec-b"
	}
	if _, err := adapter.Load(context.Background(), scope, winner); err != nil {
		t.Fatalf("winner load: %v", err)
	}
	if _, err := adapter.Load(context.Background(), scope, loser); err == nil {
		t.Fatal("the losing duplicate receipt must not commit a second execution")
	}
}
