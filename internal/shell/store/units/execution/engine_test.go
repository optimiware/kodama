package executionstore

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
)

func newEngine(t *testing.T) (*Engine, *Store, *memory.Store) {
	t.Helper()
	adapter, backend := newStore(t)
	return NewEngine(backend.Controller()), adapter, backend
}

func TestEngineLoadReadsAcrossScopesWithoutCallerScope(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	seedPipeline(t, backend, projectScope(), 3)
	seedPipeline(t, backend, otherScope(), 3)
	if _, err := adapter.Start(context.Background(), startUnit(projectScope(), "exec-1", "start-1", containerAction())); err != nil {
		t.Fatalf("Start project-1: %v", err)
	}
	if _, err := adapter.Start(context.Background(), startUnit(otherScope(), "exec-2", "start-2", containerAction())); err != nil {
		t.Fatalf("Start project-2: %v", err)
	}

	for _, executionID := range []id.ExecutionID{"exec-1", "exec-2"} {
		aggregate, err := engineState.Load(context.Background(), executionID)
		if err != nil {
			t.Fatalf("Engine.Load %s: %v", executionID, err)
		}
		if aggregate.ID != executionID || aggregate.State != kernel.ExecutionQueued {
			t.Fatalf("aggregate = %+v", aggregate)
		}
	}
	if _, err := engineState.Load(context.Background(), "exec-missing"); err == nil {
		t.Fatal("unknown execution must be not_found")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("class = %v; want not_found", err)
	}
}

func TestEngineCommitDerivesOwningScopeAndReturnsPostCommitState(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
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
		Launch:                   &kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchNotIssued, IdempotencyKey: "key-1"},
		Deadline:                 testNow.Add(90 * time.Second),
		At:                       testNow,
	}
	aggregate, err := engineState.Commit(context.Background(), commit)
	if err != nil {
		t.Fatalf("Engine.Commit: %v", err)
	}
	if aggregate.Version != 2 || aggregate.State != kernel.ExecutionRunning || aggregate.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("post-commit aggregate = %+v", aggregate)
	}
	if aggregate.Actions[0].Container == nil || aggregate.Actions[0].Container.Launch.WorkloadID != "wl-1" {
		t.Fatalf("launch intent not persisted: %+v", aggregate.Actions[0].Container)
	}
	if !aggregate.Actions[0].ActivatedAt.Equal(testNow) {
		t.Fatalf("activated at = %s", aggregate.Actions[0].ActivatedAt)
	}

	scoped := loadExecution(t, adapter, scope, "exec-1")
	if scoped.Version != 2 {
		t.Fatalf("scoped load version = %d; want 2", scoped.Version)
	}
	if _, err := adapter.Load(context.Background(), otherScope(), "exec-1"); err == nil {
		t.Fatal("the controller commit must not widen the execution scope")
	}
}

func TestEngineCommitRefusesRowsOutsideTheOwningScope(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	seedPipeline(t, backend, projectScope(), 3)
	seedPipeline(t, backend, otherScope(), 3)
	actions := startedExecution(t, adapter, backend, projectScope(), "exec-1", containerAction())
	foreign := startedExecution(t, adapter, backend, otherScope(), "exec-2", containerAction())

	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		PendingCancellations:     []flow.ActionUpdate{{ActionID: foreign[0], ExpectedVersion: 1, NextState: kernel.ActionCancelled}},
		At:                       testNow,
	}
	_, err := engineState.Commit(context.Background(), commit)
	if err == nil {
		t.Fatal("a foreign-scope action write must be refused")
	}
	class, ok := fault.ClassOf(err)
	if !ok || (class != fault.Forbidden && class != fault.VersionConflict) {
		t.Fatalf("class = %v; want forbidden or version_conflict", err)
	}
	if aggregate := loadExecution(t, adapter, projectScope(), "exec-1"); aggregate.Version != 1 {
		t.Fatalf("refused commit changed exec-1: %+v", aggregate)
	}
	if aggregate := loadExecution(t, adapter, otherScope(), "exec-2"); aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("refused commit changed the foreign execution: %+v", aggregate.Actions[0])
	}
}

func TestEngineCommitStaleVersionConflictsWithoutPartialState(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 9,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    9,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	_, err := engineState.Commit(context.Background(), commit)
	requireClass(t, err, fault.VersionConflict)
	if aggregate := loadExecution(t, adapter, scope, "exec-1"); aggregate.Version != 1 {
		t.Fatalf("stale commit changed the execution: %+v", aggregate)
	}
}

func TestEngineScanDueHonorsThePersistedDeadline(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	deadline := testNow.Add(60 * time.Second)
	commit := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Deadline:                 deadline,
		At:                       testNow,
	}
	if _, err := engineState.Commit(context.Background(), commit); err != nil {
		t.Fatalf("commit: %v", err)
	}

	before, err := engineState.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: deadline.Add(-time.Nanosecond), Limit: 10})
	if err != nil {
		t.Fatalf("scan before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("scan before deadline = %v; want empty", before)
	}
	at, err := engineState.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanDue, Now: deadline, Limit: 10})
	if err != nil {
		t.Fatalf("scan at deadline: %v", err)
	}
	if len(at) != 1 || at[0] != "exec-1" {
		t.Fatalf("scan at deadline = %v; want [exec-1]", at)
	}
}

func TestEngineScanUnfinishedCoversCleanupAndProgress(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	// Queued with pending actions owes progress.
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-queued", "start-q", containerAction())); err != nil {
		t.Fatalf("start queued: %v", err)
	}
	// A running container action may own an external effect.
	containerActions := startedExecution(t, adapter, backend, scope, "exec-container", containerAction())
	activate := flow.Commit{
		ExecutionID:              "exec-container",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 containerActions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Deadline:                 testNow.Add(60 * time.Second),
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-container", activate, "run-c")); err != nil {
		t.Fatalf("activate container: %v", err)
	}
	// A running request action waits for an answer and is not unfinished.
	requestActions := startedExecution(t, adapter, backend, scope, "exec-request", containerAction(), requestAction())
	activateRequest := flow.Commit{
		ExecutionID:              "exec-request",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 requestActions[1],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Deadline:                 testNow.Add(60 * time.Second),
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-request", activateRequest, "run-r")); err != nil {
		t.Fatalf("activate request: %v", err)
	}

	unfinished, err := engineState.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow, Limit: 100})
	if err != nil {
		t.Fatalf("scan unfinished: %v", err)
	}
	present := make(map[id.ExecutionID]bool, len(unfinished))
	for _, executionID := range unfinished {
		present[executionID] = true
	}
	for _, want := range []id.ExecutionID{"exec-queued", "exec-container"} {
		if !present[want] {
			t.Fatalf("unfinished scan = %v; want %q", unfinished, want)
		}
	}
	if present["exec-request"] {
		t.Fatalf("a running request action must not be unfinished: %v", unfinished)
	}

	// Cancellation leaves cleanup owed even though the execution is terminal.
	cancel := flow.Commit{
		ExecutionID:              "exec-queued",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Cleanup:                  &flow.CleanupIntent{WorkloadID: "wl-queued", Reason: "cancelled"},
		CleanupUpdate:            &flow.CleanupUpdate{State: kernel.CleanupPending, WorkloadID: "wl-queued"},
		At:                       testNow,
	}
	if _, err := adapter.Transition(context.Background(), transitionUnit(scope, "exec-queued", cancel, "cancel-q")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	after, err := engineState.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow, Limit: 100})
	if err != nil {
		t.Fatalf("scan after cancel: %v", err)
	}
	if len(after) == 0 {
		t.Fatal("pending cleanup must stay unfinished")
	}

	limited, err := engineState.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: testNow, Limit: 1})
	if err != nil {
		t.Fatalf("limited scan: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limited scan = %v; want one identity", limited)
	}
}

// TestEngineLoadsATaskStyleSnapshotDocument simulates the task/trigger
// write path: the execution and action rows are written with the shared
// snapshotdoc codec only, then loaded through this package's Kernel engine. It
// proves a task-started execution round-trips through Engine.Load.
func TestEngineLoadsATaskStyleSnapshotDocument(t *testing.T) {
	engineState, _, backend := newEngine(t)
	scope := projectScope()
	snapshot := testSnapshot("task-1", []byte("conversation\x00bytes"))
	actions := []pipeline.AdmittedAction{containerAction(), requestAction()}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      pipelineIDFor(scope),
		PipelineVersion: 3,
		Snapshot:        snapshot,
		Actions:         actions,
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	rows, err := snapshotdoc.ActionRows("exec-task", actions)
	if err != nil {
		t.Fatalf("ActionRows: %v", err)
	}
	digest := snapshot.YAMLSHA256
	writes := []store.Write{{
		Table:  "executions",
		ID:     "exec-task",
		Insert: true,
		Values: map[string]any{
			"pipeline_id":  string(pipelineIDFor(scope)),
			"task_id":      string(snapshot.TaskID),
			"principal_id": "exec-principal-1",
			"state":        string(kernel.ExecutionQueued),
			"version":      int64(1),
			"yaml_sha256":  append([]byte(nil), digest[:]...),
			"snapshot":     document,
			"workspace_id": string(snapshotdoc.DeriveWorkspaceID("exec-task")),
			"created_at":   testNow,
		},
	}}
	for _, row := range rows {
		writes = append(writes, store.Write{Table: "actions", ID: row.ID, Insert: true, Values: row.Values})
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: writes}); err != nil {
		t.Fatalf("task-style write: %v", err)
	}

	aggregate, err := engineState.Load(context.Background(), "exec-task")
	if err != nil {
		t.Fatalf("Engine.Load: %v", err)
	}
	if aggregate.WorkspaceID != snapshotdoc.DeriveWorkspaceID("exec-task") {
		t.Fatalf("workspace = %q", aggregate.WorkspaceID)
	}
	if string(aggregate.Snapshot.YAML()) != "name: build\non: push\n" || aggregate.Snapshot.TaskID() != "task-1" {
		t.Fatalf("snapshot did not round-trip: %+v", aggregate.Snapshot)
	}
	if task, present := aggregate.Snapshot.Task(); !present || string(task) != "conversation\x00bytes" {
		t.Fatalf("task = %q present=%v", task, present)
	}
	if len(aggregate.Actions) != 2 || aggregate.Actions[0].ID != snapshotdoc.DeriveActionID("exec-task", 0) {
		t.Fatalf("actions = %+v", aggregate.Actions)
	}
	if aggregate.Actions[0].Container == nil || aggregate.Actions[1].Request == nil {
		t.Fatalf("definitions did not come from the snapshot document: %+v", aggregate.Actions)
	}
}

// unfinishedScan returns the set of executions one unfinished recovery pass
// reports at the authoritative time now.
func unfinishedScan(t *testing.T, state *Engine, now time.Time) map[id.ExecutionID]bool {
	t.Helper()
	identities, err := state.Scan(context.Background(), engine.ScanRequest{Kind: engine.ScanUnfinished, Now: now, Limit: 100})
	if err != nil {
		t.Fatalf("unfinished scan at %s: %v", now, err)
	}
	present := make(map[id.ExecutionID]bool, len(identities))
	for _, executionID := range identities {
		present[executionID] = true
	}
	return present
}

// TestEngineScanUnfinishedHonorsTheCleanupRetrySchedule asserts the durable
// cleanup retry deadline gates the unfinished scan: a pending cleanup whose
// next_retry_at is in the future is not reported (and so is not reattempted),
// equality is due, the initial intent is promptly eligible, and the schedule
// survives a fresh engine over the same persisted rows.
func TestEngineScanUnfinishedHonorsTheCleanupRetrySchedule(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	ctx := context.Background()

	cancel := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Cleanup:                  &flow.CleanupIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1", Reason: "cancelled"},
		CleanupUpdate:            &flow.CleanupUpdate{State: kernel.CleanupPending, WorkloadID: "wl-1", WorkspaceID: "ws-1"},
		At:                       testNow,
	}
	if _, err := adapter.Transition(ctx, transitionUnit(scope, "exec-1", cancel, "cancel-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The initial cleanup intent is promptly eligible at exactly commit.At.
	if !unfinishedScan(t, engineState, testNow)["exec-1"] {
		t.Fatal("an initial cleanup intent must be unfinished at commit.At")
	}

	failedAt := testNow.Add(10 * time.Second)
	if _, err := engineState.Commit(ctx, cleanupProgressCommit("exec-1", 2, kernel.ExecutionCancelled, 1, "dependency_unavailable: log read failed", failedAt)); err != nil {
		t.Fatalf("failed cleanup commit: %v", err)
	}
	retryAt, attempts := cleanupNextRetryAt(t, backend, scope, "exec-1")
	if attempts != 1 || !retryAt.Equal(failedAt.Add(time.Second)) {
		t.Fatalf("failed attempt = (%s, %d); want a one-second retry after %s", retryAt, attempts, failedAt)
	}
	versionAfterFailure := loadExecution(t, adapter, scope, "exec-1").Version

	// A not-yet-due retry is excluded from every pass; the row is not rewritten.
	for range 3 {
		scanAt := retryAt.Add(-time.Nanosecond)
		if unfinishedScan(t, engineState, scanAt)["exec-1"] {
			t.Fatalf("cleanup due at %s must not be scanned at %s", retryAt, scanAt)
		}
	}
	if version := loadExecution(t, adapter, scope, "exec-1").Version; version != versionAfterFailure {
		t.Fatalf("excluded scans changed the execution version: %d; want %d", version, versionAfterFailure)
	}

	// Due means next_retry_at <= Now: the boundary itself is included.
	if !unfinishedScan(t, engineState, retryAt)["exec-1"] {
		t.Fatalf("cleanup due exactly at %s must be scanned", retryAt)
	}

	// The schedule is a persisted column, so a fresh engine over the same rows
	// observes the same deadline.
	restarted := NewEngine(backend.Controller())
	if unfinishedScan(t, restarted, retryAt.Add(-time.Nanosecond))["exec-1"] {
		t.Fatal("a restarted engine must honor the persisted cleanup retry schedule")
	}
	if !unfinishedScan(t, restarted, retryAt)["exec-1"] {
		t.Fatal("a restarted engine must scan the cleanup once the persisted retry is due")
	}
}
