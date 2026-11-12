package snapshotdoc_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// testTime is a fixed admission time; the codec and Load never depend on it.
func testTime() time.Time {
	return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
}

// TestTaskStartWritePathLoadsThroughTheKernelEngine is the cross-adapter
// compatibility proof: it writes an execution exactly the way a task/trigger
// start does, using only the shared codec and derivations, then loads it
// through the Kernel engine. The work store uses the same snapshotdoc calls,
// so its started executions round-trip through this Load.
func TestTaskStartWritePathLoadsThroughTheKernelEngine(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	backend := memory.New()
	executionID := id.ExecutionID("exec-task-1")
	snapshot := fullSnapshot(t, []byte("conversation\x00bytes"))
	admitted := snapshotdoc.Admitted{
		PipelineID:      "pipe-task",
		PipelineVersion: 4,
		Snapshot:        snapshot,
		Actions:         admittedActions(),
	}
	encoded, err := snapshotdoc.Encode(admitted)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	rows, err := snapshotdoc.ActionRows(executionID, admitted.Actions)
	if err != nil {
		t.Fatalf("ActionRows: %v", err)
	}
	digest := snapshot.YAMLSHA256
	writes := []store.Write{{
		Table:  "executions",
		ID:     string(executionID),
		Insert: true,
		Values: map[string]any{
			"pipeline_id":  string(admitted.PipelineID),
			"task_id":      string(snapshot.TaskID),
			"principal_id": "user-1",
			"state":        "queued",
			"version":      int64(1),
			"yaml_sha256":  append([]byte(nil), digest[:]...),
			"snapshot":     encoded,
			"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
			"created_at":   testTime(),
		},
	}}
	for _, row := range rows {
		writes = append(writes, store.Write{Table: "actions", ID: row.ID, Insert: true, Values: row.Values})
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: writes}); err != nil {
		t.Fatalf("write task-style execution: %v", err)
	}

	aggregate, err := executionstore.NewEngine(backend.Controller()).Load(context.Background(), executionID)
	if err != nil {
		t.Fatalf("Engine.Load: %v", err)
	}
	if aggregate.WorkspaceID != snapshotdoc.DeriveWorkspaceID(executionID) {
		t.Fatalf("workspace = %q; want %q", aggregate.WorkspaceID, snapshotdoc.DeriveWorkspaceID(executionID))
	}
	if !bytes.Equal(aggregate.Snapshot.YAML(), snapshot.YAMLBytes()) {
		t.Fatalf("yaml = %v; want exact %v", aggregate.Snapshot.YAML(), snapshot.YAMLBytes())
	}
	if aggregate.Snapshot.TaskID() != snapshot.TaskID {
		t.Fatalf("task id = %q; want %q", aggregate.Snapshot.TaskID(), snapshot.TaskID)
	}
	task, present := aggregate.Snapshot.Task()
	if !present || !bytes.Equal(task, []byte("conversation\x00bytes")) {
		t.Fatalf("task = %q present=%v", task, present)
	}
	if len(aggregate.Actions) != 2 {
		t.Fatalf("actions = %d; want 2", len(aggregate.Actions))
	}
	build := aggregate.Actions[0]
	if build.ID != snapshotdoc.DeriveActionID(executionID, 0) || build.Kind != definition.ActionKindContainer {
		t.Fatalf("container action = %+v", build)
	}
	if build.Container == nil || build.Container.Definition.Image != admitted.Actions[0].Container.Image {
		t.Fatalf("container definition did not come from the snapshot document: %+v", build.Container)
	}
	if build.Container.WorkspaceID != aggregate.WorkspaceID {
		t.Fatalf("container workspace = %q; want %q", build.Container.WorkspaceID, aggregate.WorkspaceID)
	}
	approve := aggregate.Actions[1]
	if approve.Request == nil || approve.Request.Definition.Assignment != "role/reviewer" {
		t.Fatalf("request definition did not come from the snapshot document: %+v", approve.Request)
	}
	if aggregate.CurrentAction != -1 || aggregate.State != kernel.ExecutionQueued {
		t.Fatalf("execution = %+v; want queued with no current action", aggregate)
	}
}
