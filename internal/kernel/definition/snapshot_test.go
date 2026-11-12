package definition

import (
	"testing"

	"github.com/optimiware/kodama/internal/id"
)

// TestSnapshotCarriesTaskID pins the prepare context: the admitted snapshot
// keeps the task identity separately from the opaque task bytes, and a later
// mutation of the admission input cannot rewrite it.
func TestSnapshotCarriesTaskID(t *testing.T) {
	input := SnapshotInput{
		TaskID: "task-1",
		Task:   []byte(`{"id":"task-1"}`),
	}
	snapshot := NewSnapshot(input)
	if got := snapshot.TaskID(); got != id.TaskID("task-1") {
		t.Fatalf("snapshot task id = %q; want task-1", got)
	}

	// The constructor copies the input, so a caller-held input cannot rewrite
	// admission.
	input.TaskID = "mutated"
	if got := snapshot.TaskID(); got != id.TaskID("task-1") {
		t.Fatalf("snapshot task id mutated through input: %q", got)
	}
}

// TestSnapshotTaskIDEmptyWhenTaskFree proves a task-free snapshot reports an
// empty task identity, including the nil-snapshot accessor.
func TestSnapshotTaskIDEmptyWhenTaskFree(t *testing.T) {
	snapshot := NewSnapshot(SnapshotInput{Task: nil})
	if got := snapshot.TaskID(); got != "" {
		t.Fatalf("task-free snapshot task id = %q; want empty", got)
	}
	if _, ok := snapshot.Task(); ok {
		t.Fatal("task-free snapshot reports task bytes")
	}

	var absent *Snapshot
	if got := absent.TaskID(); got != "" {
		t.Fatalf("nil snapshot task id = %q; want empty", got)
	}
}
