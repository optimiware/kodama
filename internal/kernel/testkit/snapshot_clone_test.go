package testkit

import (
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// TestSnapshotClonePreservesTaskID pins that the testkit's defensive snapshot
// copy keeps the task identity, so seeded and loaded executions do
// not silently lose the task context an engine Prepare would carry.
func TestSnapshotClonePreservesTaskID(t *testing.T) {
	snapshot := definition.NewSnapshot(definition.SnapshotInput{
		YAML:   []byte("name: clone\n"),
		TaskID: "task-1",
		Task:   []byte(`{"id":"task-1"}`),
	})

	cloned := cloneSnapshot(snapshot)
	if got := cloned.TaskID(); got != id.TaskID("task-1") {
		t.Fatalf("cloned snapshot task id = %q; want task-1", got)
	}

	execution := testExecution("exec-clone-task", "ws-clone-task", testContainerAction("act-build", "build", 0, kernel.ActionPending, 0))
	execution.Snapshot = snapshot
	store := NewStore()
	store.Seed(execution)

	loaded, ok := store.Execution(execution.ID)
	if !ok {
		t.Fatal("seeded execution is missing from the store")
	}
	if got := loaded.Snapshot.TaskID(); got != id.TaskID("task-1") {
		t.Fatalf("stored snapshot task id = %q; want task-1", got)
	}

	// The store's defensive copy must not alias the caller's snapshot.
	execution.Snapshot = definition.NewSnapshot(definition.SnapshotInput{TaskID: "mutated"})
	loaded, _ = store.Execution(execution.ID)
	if got := loaded.Snapshot.TaskID(); got != id.TaskID("task-1") {
		t.Fatalf("stored snapshot task id mutated through caller: %q", got)
	}
}
