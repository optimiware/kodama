package definition

import (
	"testing"

	"github.com/optimiware/kodema/internal/id"
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
	if got := absent.TaskFiles(); got != nil {
		t.Fatalf("nil snapshot task files = %v; want nil", got)
	}
}

// TestSnapshotCarriesTaskFilesDeepCopied proves TaskFiles are deep-copied in
// both directions so mutating the caller's map/slices or returned copies cannot
// alter admitted state, and nil when task-free.
func TestSnapshotCarriesTaskFilesDeepCopied(t *testing.T) {
	titleBytes := []byte("Fix auth bug")
	descBytes := []byte{}
	inputFiles := map[string][]byte{
		"title":       titleBytes,
		"description": descBytes,
	}
	snapshot := NewSnapshot(SnapshotInput{
		TaskID:    "task-1",
		Task:      []byte("[]"),
		TaskFiles: inputFiles,
	})

	// Mutate caller-held input map and backing slice.
	titleBytes[0] = 'X'
	inputFiles["extra"] = []byte("mutated")

	got := snapshot.TaskFiles()
	if len(got) != 2 {
		t.Fatalf("task files len = %d; want 2 (%v)", len(got), got)
	}
	if string(got["title"]) != "Fix auth bug" {
		t.Fatalf("task title = %q; want %q", got["title"], "Fix auth bug")
	}
	if got["description"] == nil || len(got["description"]) != 0 {
		t.Fatalf("task description = %#v; want non-nil empty slice", got["description"])
	}

	// Mutate returned copy and verify snapshot is unaffected.
	got["title"][0] = 'Y'
	got["injected"] = []byte("bad")
	again := snapshot.TaskFiles()
	if string(again["title"]) != "Fix auth bug" {
		t.Fatalf("snapshot task title mutated through returned map: %q", again["title"])
	}
	if _, ok := again["injected"]; ok {
		t.Fatal("snapshot task files mutated through returned map")
	}

	taskFree := NewSnapshot(SnapshotInput{Task: nil})
	if taskFree.TaskFiles() != nil {
		t.Fatalf("task-free snapshot task files = %v; want nil", taskFree.TaskFiles())
	}
}
