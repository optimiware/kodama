// Preparation behavior: one confined all-or-nothing activation snapshot, the
// existing workspace attachment, the generated fail-fast script, the merged
// reserved environment and the changed-content refusal.
package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
)

// TestPrepareMaterializesConfinedActivationSnapshot proves one complete,
// all-or-nothing snapshot outside the workspace: exact input bytes, earlier
// succeeded outputs, the optional task file, declared secrets and the mutable
// credential, each a read-only regular file, plus the generated script.
func TestPrepareMaterializesConfinedActivationSnapshot(t *testing.T) {
	fx := newFixture(t)

	preparation := prepareTestWorkload(t, fx.adapter, fx.request)
	if preparation.WorkspaceID != fx.request.WorkspaceID {
		t.Fatalf("preparation workspace = %q; want %q", preparation.WorkspaceID, fx.request.WorkspaceID)
	}
	if preparation.Detail == "" {
		t.Fatal("preparation detail is empty")
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("Prepare spawned a process")
	}

	assertHostFile(t, fx.hostPath(t, "/kodama/inputs/input-a"), "line1\nline2")
	assertHostFile(t, fx.hostPath(t, "/kodama/outputs/build/out-a"), "out")
	assertHostFile(t, fx.hostPath(t, "/kodama/task.json"), `{"id":"task-1"}`)
	assertHostFile(t, fx.hostPath(t, "/kodama/secrets/API_KEY"), "s3cr3t")
	assertHostFile(t, fx.hostPath(t, "/kodama/secrets/KODAMA_TOKEN"), "tok-abc")

	wantScript, err := script.Render(fx.request.Container.Commands)
	if err != nil {
		t.Fatalf("script.Render returned error: %v", err)
	}
	assertHostFile(t, filepath.Join(fx.resourceRoot(t), "script"), wantScript)

	for _, path := range fx.files.callsFor("WriteFile") {
		assertUnder(t, path, fx.root)
		if path == fx.workspaceDir || strings.HasPrefix(path, fx.workspaceDir+string(os.PathSeparator)) {
			t.Fatalf("Prepare wrote %q into the execution workspace", path)
		}
	}
	entries := workspaceEntries(t, fx.workspaceDir)
	if !slices.Equal(entries, []string{workspaceSentinel}) {
		t.Fatalf("workspace entries = %v; want only the seeded sentinel", entries)
	}
}

// TestSpawnRequestCarriesGeneratedScriptAndReservedContext pins the exact
// fail-fast argv, the unshadowable reserved environment order and the confined
// paths handed to the executor. Secrets never reach argv or the environment.
func TestSpawnRequestCarriesGeneratedScriptAndReservedContext(t *testing.T) {
	fx := newFixture(t)
	fx.request.Container.Env = []definition.EnvVar{
		{Name: "BUILD_MODE", Value: "release"},
		{Name: "EXTRA", Value: "1"},
	}
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)

	if count := fx.executor.spawnRequestCount(); count != 1 {
		t.Fatalf("executor saw %d spawn requests; want 1", count)
	}
	spawn := fx.executor.spawnRequests()[0]
	resourceRoot := fx.resourceRoot(t)
	scriptPath := filepath.Join(resourceRoot, "script")

	if spawn.Identity != fx.request.Identity {
		t.Fatalf("spawn identity = %+v; want the persisted identity", spawn.Identity)
	}
	if spawn.Group != fx.groupID(t) {
		t.Fatalf("spawn group = %q; want %q", spawn.Group, fx.groupID(t))
	}
	if want := []string{script.ShellPath, scriptPath}; !slices.Equal(spawn.Command, want) {
		t.Fatalf("spawn command = %q; want %q", spawn.Command, want)
	}
	if spawn.Dir != fx.workspaceDir {
		t.Fatalf("spawn dir = %q; want the existing workspace %q", spawn.Dir, fx.workspaceDir)
	}
	if spawn.Root != resourceRoot {
		t.Fatalf("spawn root = %q; want %q", spawn.Root, resourceRoot)
	}
	if want := filepath.Join(resourceRoot, "logs", "stdout"); spawn.StdoutPath != want {
		t.Fatalf("spawn stdout path = %q; want %q", spawn.StdoutPath, want)
	}
	if want := filepath.Join(resourceRoot, "logs", "stderr"); spawn.StderrPath != want {
		t.Fatalf("spawn stderr path = %q; want %q", spawn.StderrPath, want)
	}
	assertUnder(t, spawn.StdoutPath, resourceRoot)
	assertUnder(t, spawn.StderrPath, resourceRoot)

	wantEnv := []string{
		"BUILD_MODE=release",
		"EXTRA=1",
		"KODAMA_URL=" + testKodamaURL,
		"KODAMA_EXECUTION=exec-1",
		"KODAMA_ACTION=build",
		"KODAMA_TASK=task-1",
	}
	if !slices.Equal(spawn.Env, wantEnv) {
		t.Fatalf("spawn env = %q; want %q", spawn.Env, wantEnv)
	}
	if got := spawn.Env[4]; strings.Contains(got, "action-1") {
		t.Fatalf("KODAMA_ACTION leaked the action id: %q", got)
	}

	wantScript, err := script.Render(fx.request.Container.Commands)
	if err != nil {
		t.Fatalf("script.Render returned error: %v", err)
	}
	if got := readHostFile(t, scriptPath); got != wantScript {
		t.Fatalf("script bytes = %q; want %q", got, wantScript)
	}
	for _, command := range fx.request.Container.Commands {
		if !strings.Contains(wantScript, command) {
			t.Fatalf("script lost command %q", command)
		}
	}
	if strings.Contains(wantScript, "set -x") {
		t.Fatal("generated script enables tracing")
	}
	for _, argument := range spawn.Command {
		if argument == "-x" || argument == "--xtrace" {
			t.Fatalf("spawn argv enables tracing: %q", spawn.Command)
		}
	}

	for _, value := range append(append([]string(nil), spawn.Command...), spawn.Env...) {
		for _, secret := range []string{"s3cr3t", "tok-abc", "KODAMA_TOKEN"} {
			if strings.Contains(value, secret) {
				t.Fatalf("spawn input leaked %q in %q", secret, value)
			}
		}
	}
}

// TestPrepareOmitsTaskContextWhenTaskFree proves a task-free execution writes no
// task file and omits KODAMA_TASK.
func TestPrepareOmitsTaskContextWhenTaskFree(t *testing.T) {
	fx := newFixture(t)
	fx.request.TaskID = ""
	fx.request.Task = nil

	prepareTestWorkload(t, fx.adapter, fx.request)
	assertNoPath(t, fx.hostPath(t, "/kodama/task.json"))

	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	spawn := fx.executor.spawnRequests()[0]
	wantEnv := []string{
		"BUILD_MODE=release",
		"KODAMA_URL=" + testKodamaURL,
		"KODAMA_EXECUTION=exec-1",
		"KODAMA_ACTION=build",
	}
	if !slices.Equal(spawn.Env, wantEnv) {
		t.Fatalf("task-free env = %q; want %q", spawn.Env, wantEnv)
	}
}

// TestPrepareRefusesReservedEnvironmentNames proves the reserved context is
// unshadowable: a declaration that tries to set one is refused before any
// effect.
func TestPrepareRefusesReservedEnvironmentNames(t *testing.T) {
	for _, reserved := range id.ReservedEnvNames() {
		t.Run(reserved, func(t *testing.T) {
			fx := newFixture(t)
			fx.request.Container.Env = append(fx.request.Container.Env,
				definition.EnvVar{Name: id.EnvName(reserved), Value: "hijack"})

			_, err := fx.adapter.Prepare(context.Background(), fx.request)
			assertRefusedClass(t, err, fault.InvalidInput)
			if writes := fx.files.callsFor("WriteFile"); len(writes) != 0 {
				t.Fatalf("refused Prepare wrote %v", writes)
			}
			if fx.executor.spawnCount() != 0 {
				t.Fatal("refused Prepare spawned a process")
			}
		})
	}
}

// TestPrepareRefusesMissingWorkspace proves preparation attaches an existing
// workspace and never recreates a missing one.
func TestPrepareRefusesMissingWorkspace(t *testing.T) {
	fx := newFixture(t)
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}

	_, err := fx.adapter.Prepare(context.Background(), fx.request)
	assertRefusedClass(t, err, fault.NotFound)
	assertNoPath(t, fx.workspaceDir)
	for _, path := range fx.files.callsFor("MkdirAll") {
		if path == fx.workspaceDir || strings.HasPrefix(path, fx.workspaceDir+string(os.PathSeparator)) {
			t.Fatalf("Prepare recreated the workspace at %q", path)
		}
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("Prepare spawned a process")
	}
}

// TestPrepareRefusesTraversalSymlinkAndReservedCollision proves path
// confinement: traversal keys, a symlinked activation root and a stored secret
// that shadows the credential mount are all refused without writing secret
// material outside the adapter root.
func TestPrepareRefusesTraversalSymlinkAndReservedCollision(t *testing.T) {
	t.Run("traversal input key", func(t *testing.T) {
		fx := newFixture(t)
		fx.request.Inputs[".."] = "escape"

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertRefusedClass(t, err, fault.InvalidInput)
		if fx.executor.spawnCount() != 0 {
			t.Fatal("refused Prepare spawned a process")
		}
	})

	t.Run("slash input key", func(t *testing.T) {
		fx := newFixture(t)
		fx.request.Inputs["a/b"] = "escape"

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertRefusedClass(t, err, fault.InvalidInput)
	})

	t.Run("symlinked activation root", func(t *testing.T) {
		fx := newFixture(t)
		outside := t.TempDir()
		resourceRoot := fx.resourceRoot(t)
		if err := os.MkdirAll(resourceRoot, 0o755); err != nil {
			t.Fatalf("creating resource root: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(resourceRoot, "kodama")); err != nil {
			t.Fatalf("creating symlink: %v", err)
		}

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertRefusedClass(t, err, fault.InvalidInput, fault.Forbidden)
		for _, path := range fx.files.callsFor("WriteFile") {
			assertUnder(t, path, fx.root)
		}
		for _, entry := range workspaceEntries(t, outside) {
			info, err := os.Lstat(filepath.Join(outside, entry))
			if err != nil {
				t.Fatalf("stat escaped entry: %v", err)
			}
			if info.Mode().IsRegular() {
				t.Fatalf("Prepare wrote a file outside the adapter root through a symlink: %q", entry)
			}
		}
		if fx.executor.spawnCount() != 0 {
			t.Fatal("refused Prepare spawned a process")
		}
	})

	t.Run("reserved secret name", func(t *testing.T) {
		fx := newFixture(t)
		fx.request.Secrets = append(fx.request.Secrets,
			engine.SecretValue{Name: id.SecretName(id.SecretTokenName), Value: "shadow"})

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertRefusedClass(t, err, fault.InvalidInput)
		assertNoPath(t, fx.credentialPath(t))
		if fx.executor.spawnCount() != 0 {
			t.Fatal("refused Prepare spawned a process")
		}
	})
}

// TestPrepareRefusesPartialPreparation proves a write failure never authorizes
// a start and a later retry completes the same identity.
func TestPrepareRefusesPartialPreparation(t *testing.T) {
	fx := newFixture(t)
	fx.files.injectBefore("WriteFile", "/kodama/task.json", errors.New("disk full"))

	_, err := fx.adapter.Prepare(context.Background(), fx.request)
	if err == nil {
		t.Fatal("Prepare hid a partial preparation failure")
	}

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err == nil && result.Outcome == engine.StartAccepted {
		t.Fatal("partial preparation authorized a start")
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("partial preparation spawned a process")
	}

	fx.files.clear("WriteFile", "/kodama/task.json")
	prepareTestWorkload(t, fx.adapter, fx.request)
	assertHostFile(t, fx.hostPath(t, "/kodama/task.json"), `{"id":"task-1"}`)
	if fx.executor.spawnCount() != 0 {
		t.Fatal("re-preparation spawned a process")
	}
}

// TestPrepareIsIdempotentForSameIdentity proves re-preparing unchanged content
// neither rewrites nor restarts the workload.
func TestPrepareIsIdempotentForSameIdentity(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	scriptPath := filepath.Join(fx.resourceRoot(t), "script")
	before := readHostFile(t, scriptPath)

	second := prepareTestWorkload(t, fx.adapter, fx.request)
	if second.WorkspaceID != fx.request.WorkspaceID {
		t.Fatalf("idempotent preparation workspace = %q", second.WorkspaceID)
	}
	if got := readHostFile(t, scriptPath); got != before {
		t.Fatal("idempotent preparation changed the script")
	}

	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if fx.executor.spawnCount() != 1 {
		t.Fatalf("start spawned %d processes; want 1", fx.executor.spawnCount())
	}

	prepareTestWorkload(t, fx.adapter, fx.request)
	if fx.executor.spawnCount() != 1 {
		t.Fatalf("re-preparation after start spawned %d processes; want 1", fx.executor.spawnCount())
	}
}

// TestPrepareRefusesChangedContent proves a retry with different activation
// content for the same identity reports changed_idempotency_content and keeps
// the original snapshot.
func TestPrepareRefusesChangedContent(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)

	changed := fx.request
	changed.Inputs = map[id.InputKey]string{"input-a": "different"}
	_, err := fx.adapter.Prepare(context.Background(), changed)
	assertRefusedClass(t, err, fault.ChangedIdempotencyContent)

	assertHostFile(t, fx.hostPath(t, "/kodama/inputs/input-a"), "line1\nline2")
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if fx.executor.spawnCount() != 1 {
		t.Fatalf("start after a refused re-prepare spawned %d processes; want 1", fx.executor.spawnCount())
	}
}

// TestPrepareRefusesDifferentWorkspaceForSameIdentity proves a live preparation
// is never silently rebound to another workspace.
func TestPrepareRefusesDifferentWorkspaceForSameIdentity(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)

	otherWorkspace, err := WorkspaceDir(fx.workspaceRoot, "workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if err := os.MkdirAll(otherWorkspace, 0o755); err != nil {
		t.Fatalf("creating second workspace: %v", err)
	}
	rebound := fx.request
	rebound.WorkspaceID = "workspace-2"

	_, err = fx.adapter.Prepare(context.Background(), rebound)
	assertRefusedClass(t, err, fault.Forbidden)
	if entries := workspaceEntries(t, otherWorkspace); len(entries) != 0 {
		t.Fatalf("rebound preparation wrote into the other workspace: %v", entries)
	}
}

// TestReconstructedAdapterReusesPreparation proves a fresh adapter on the same
// root recovers the prepared snapshot by identity and still refuses changed
// content.
func TestReconstructedAdapterReusesPreparation(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)

	reconstructed := newTestAdapter(t, fx.config())
	if _, err := reconstructed.Prepare(context.Background(), fx.request); err != nil {
		t.Fatalf("reconstructed Prepare returned error: %v", err)
	}

	changed := fx.request
	changed.Inputs = map[id.InputKey]string{"input-a": "different"}
	_, err := reconstructed.Prepare(context.Background(), changed)
	assertRefusedClass(t, err, fault.ChangedIdempotencyContent)

	startTestWorkload(t, reconstructed, fx.request.Identity, fx.request.WorkspaceID)
	if fx.executor.spawnCount() != 1 {
		t.Fatalf("reconstructed start spawned %d processes; want 1", fx.executor.spawnCount())
	}
}

// TestPrepareNeverCopiesSecretsIntoWorkspace proves secret material stays in the
// adapter-owned root and is never copied into the shared workspace.
func TestPrepareNeverCopiesSecretsIntoWorkspace(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)

	if entries := workspaceEntries(t, fx.workspaceDir); !slices.Equal(entries, []string{workspaceSentinel}) {
		t.Fatalf("workspace entries = %v; want only the seeded sentinel", entries)
	}
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q; want %q", got, workspaceSentinelData)
	}
	for _, path := range fx.files.callsFor("WriteFile") {
		assertUnder(t, path, fx.root)
	}
	for _, entry := range workspaceEntries(t, fx.workspaceDir) {
		data := readHostFile(t, filepath.Join(fx.workspaceDir, entry))
		for _, secret := range []string{"s3cr3t", "tok-abc"} {
			if strings.Contains(data, secret) {
				t.Fatalf("workspace file %q contains secret material", entry)
			}
		}
	}
}

// TestPrepareRefusesEmptyOrNULCommands proves an unusable command list never
// produces a runnable workload.
func TestPrepareRefusesEmptyOrNULCommands(t *testing.T) {
	cases := []struct {
		name     string
		commands []string
	}{
		{name: "nil commands", commands: nil},
		{name: "empty commands", commands: []string{}},
		{name: "nul command", commands: []string{"echo a\x00b"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.request.Container.Commands = testCase.commands

			_, err := fx.adapter.Prepare(context.Background(), fx.request)
			assertRefusedClass(t, err, fault.InvalidInput)
			assertNoPath(t, filepath.Join(fx.resourceRoot(t), "script"))
			if fx.executor.spawnCount() != 0 {
				t.Fatal("refused Prepare spawned a process")
			}
		})
	}
}
