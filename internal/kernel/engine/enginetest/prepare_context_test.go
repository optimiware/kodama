package enginetest

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// prepareContextExecution builds a queued execution whose snapshot carries
// literal pipeline env defaults and a task identity, and whose build action
// overrides one default and adds one action-only entry.
func prepareContextExecution(executionID, workspaceID string, taskID id.TaskID, task []byte) kernel.Execution {
	build := containerAction("act-build", "build", 0, time.Hour)
	build.Container.Definition.Env = []definition.EnvVar{
		{Name: "BUILD_MODE", Value: "action-release"},
		{Name: "ACTION_ONLY", Value: "yes"},
	}
	return kernel.Execution{
		ID:          id.ExecutionID(executionID),
		State:       kernel.ExecutionQueued,
		WorkspaceID: id.WorkspaceID(workspaceID),
		Snapshot: definition.NewSnapshot(definition.SnapshotInput{
			Pipeline: definition.Pipeline{
				Inputs: []id.InputKey{"repository"},
				Env: []definition.EnvVar{
					{Name: "LOG_LEVEL", Value: "info"},
					{Name: "BUILD_MODE", Value: "pipeline-default"},
				},
				Actions: []definition.Action{{
					Name:    "build",
					Timeout: time.Hour,
					Container: &definition.Container{
						Image:    "registry.example/app@sha256:abc",
						Commands: []string{"run"},
						Env: []definition.EnvVar{
							{Name: "BUILD_MODE", Value: "action-release"},
							{Name: "ACTION_ONLY", Value: "yes"},
						},
					},
				}},
			},
			YAML:   []byte("name: prepare-context\n"),
			Inputs: map[string]string{"repository": "example/repo"},
			Task:   task,
			TaskID: taskID,
		}),
		Actions:       []kernel.Action{build},
		CurrentAction: -1,
	}
}

// TestPrepareRequestCarriesPrepareContext pins the prepare context:
// the runtime sees the action slug (not the action id), the admitted task id,
// and pipeline env defaults merged under the action env with action precedence
// and stable declaration order. Building the request must not mutate admitted
// state.
func TestPrepareRequestCarriesPrepareContext(t *testing.T) {
	execution := prepareContextExecution("exec-prepare-context", "ws-prepare-context", "task-1", []byte(`{"id":"task-1"}`))
	h := newHarness(t, execution)

	h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	if h.runtime.prepareCount() != 1 {
		t.Fatalf("Prepare calls = %d; want 1", h.runtime.prepareCount())
	}
	request := h.runtime.lastPrepare()

	if request.ActionName != id.ActionName("build") {
		t.Fatalf("prepare action name = %q; want the action slug %q", request.ActionName, "build")
	}
	if request.ActionName == id.ActionName(request.Identity.ActionID) {
		t.Fatalf("prepare action name = %q; must be the slug, not the action id %q", request.ActionName, request.Identity.ActionID)
	}
	if request.TaskID != id.TaskID("task-1") {
		t.Fatalf("prepare task id = %q; want task-1", request.TaskID)
	}

	wantEnv := []definition.EnvVar{
		{Name: "LOG_LEVEL", Value: "info"},
		{Name: "BUILD_MODE", Value: "action-release"},
		{Name: "ACTION_ONLY", Value: "yes"},
	}
	if len(request.Container.Env) != len(wantEnv) {
		t.Fatalf("prepare env = %+v; want %+v", request.Container.Env, wantEnv)
	}
	for index, want := range wantEnv {
		if request.Container.Env[index] != want {
			t.Fatalf("prepare env[%d] = %+v; want %+v (defaults first, overridden in place, extras appended)",
				index, request.Container.Env[index], want)
		}
	}

	// The admitted snapshot and action declaration keep their original values;
	// the merge only affects the request copy.
	loaded := h.load()
	pipeline := loaded.Snapshot.Pipeline()
	if len(pipeline.Env) != 2 || pipeline.Env[1].Value != "pipeline-default" {
		t.Fatalf("admitted pipeline env mutated by prepare: %+v", pipeline.Env)
	}
	persisted := mustAction(t, loaded, "build")
	if persisted.Container == nil || persisted.Container.Definition.Env[0].Value != "action-release" {
		t.Fatalf("admitted action env mutated by prepare: %+v", persisted.Container)
	}

	// Mutating the request must not reach the persisted snapshot or action.
	request.Container.Env[0].Value = "mutated"
	request.Container.Env = append(request.Container.Env, definition.EnvVar{Name: "EXTRA", Value: "x"})
	loaded = h.load()
	if loaded.Snapshot.Pipeline().Env[0].Value != "info" {
		t.Fatal("mutating the prepare request changed the admitted pipeline env")
	}
	persisted = mustAction(t, loaded, "build")
	if persisted.Container.Definition.Env[0].Value != "action-release" {
		t.Fatal("mutating the prepare request changed the admitted action env")
	}
}

// TestPrepareRequestOmitsTaskIDWhenTaskFree proves a task-free execution sends
// an empty task identity and no task bytes.
func TestPrepareRequestOmitsTaskIDWhenTaskFree(t *testing.T) {
	execution := prepareContextExecution("exec-prepare-task-free", "ws-prepare-task-free", "", nil)
	h := newHarness(t, execution)

	h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	request := h.runtime.lastPrepare()
	if request.ActionName != id.ActionName("build") {
		t.Fatalf("prepare action name = %q; want build", request.ActionName)
	}
	if request.TaskID != "" {
		t.Fatalf("task-free prepare task id = %q; want empty", request.TaskID)
	}
	if len(request.Task) != 0 {
		t.Fatalf("task-free prepare task bytes = %q; want none", request.Task)
	}
}
