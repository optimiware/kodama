package podman

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/kernel/engine"
)

const testGitURL = "https://host.containers.internal:8443/git"

// toolboxFixture is a fixture whose adapter mounts a toolbox and knows the
// installation's Git service.
func toolboxFixture(t *testing.T, binary string) (*fixture, string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "kodema")
	if err := os.WriteFile(source, []byte(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t)
	// The shared request names an earlier output of the action itself, which
	// an execution cannot hold: an action is the only writer of its directory.
	fx.request.Outputs = map[id.ActionName]kernel.Outputs{"clone": {"commit": "abc"}}
	fx.adapter = fx.newToolboxAdapter(t, source)
	return fx, source
}

func (f *fixture) newToolboxAdapter(t *testing.T, source string) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		Libpod:      f.libpod,
		Files:       f.files,
		Root:        f.root,
		KodemaURL:   testKodemaURL,
		ToolboxFile: source,
		GitURL:      testGitURL,
		Cleanup:     CleanupPolicy{Attempts: 2},
		Clock:       f.clock,
	})
	if err != nil {
		t.Fatalf("New with a toolbox: %v", err)
	}
	return adapter
}

func (f *fixture) createdSpec(t *testing.T) ContainerSpec {
	t.Helper()
	specs := f.libpod.createSpecs()
	if len(specs) != 1 {
		t.Fatalf("created containers = %d; want 1", len(specs))
	}
	return specs[0]
}

func mountAt(spec ContainerSpec, destination string) (Mount, bool) {
	for _, mount := range spec.Mounts {
		if mount.Destination == destination {
			return mount, true
		}
	}
	return Mount{}, false
}

// TestToolboxIsInstalledOnceAndMountedReadOnly pins the toolbox mount: one
// installation copy named by the binary's digest, executable and never
// writable, bound read-only at /kodema/bin on a mount point inside the
// activation directory.
func TestToolboxIsInstalledOnceAndMountedReadOnly(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)

	mount, ok := mountAt(spec, "/kodema/bin")
	if !ok {
		t.Fatalf("mounts %+v hold no toolbox", spec.Mounts)
	}
	if mount.Type != "bind" || !mount.ReadOnly {
		t.Fatalf("toolbox mount = %+v; want a read-only bind", mount)
	}
	if filepath.Dir(mount.Source) != filepath.Join(fx.root, ".toolbox") {
		t.Fatalf("toolbox source %q is not an installation copy below the adapter root", mount.Source)
	}
	installed := filepath.Join(mount.Source, "kodema")
	if got := readHostFile(t, installed); string(got) != "toolbox-v1" {
		t.Fatalf("installed toolbox = %q; want the configured binary", got)
	}
	if mode := hostFileMode(t, installed); mode != 0o555 {
		t.Fatalf("installed toolbox mode = %o; want 0555", mode)
	}
	if info, err := os.Stat(fx.hostPath("/kodema/bin")); err != nil || !info.IsDir() {
		t.Fatalf("the activation directory has no /kodema/bin mount point: %v", err)
	}
	if entries, _ := os.ReadDir(fx.hostPath("/kodema/bin")); len(entries) != 0 {
		t.Fatalf("the mount point holds %d entries; want it empty", len(entries))
	}

	// A second workload reuses the copy instead of writing it again.
	writes := fx.files.countPath("WriteFile", installed)
	second := fx.request
	second.Identity = engine.EffectIdentity{ExecutionID: "exec-1", ActionID: "action-2", WorkloadID: "workload-2", IdempotencyKey: "idem-2"}
	if _, err := fx.adapter.Prepare(context.Background(), second); err != nil {
		t.Fatalf("Prepare of a second workload: %v", err)
	}
	if got := fx.files.countPath("WriteFile", installed); got != writes {
		t.Fatalf("the toolbox was written %d more time(s) for a second workload", got-writes)
	}
}

// TestToolboxLauncherWorkingDirectoryAndOutputs pins what the toolbox changes
// in the container: the launcher, the working directory and, for an action
// that declares outputs, its own writable and bounded output directory.
func TestToolboxLauncherWorkingDirectoryAndOutputs(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)

	if spec.WorkingDir != "/workspace" {
		t.Fatalf("working directory = %q; want /workspace", spec.WorkingDir)
	}
	want := []string{"/bin/sh", "-c", `export PATH="${PATH:+$PATH:}/kodema/bin"; /bin/sh /kodema/script && exec /kodema/bin/kodema output push`}
	if !slices.Equal(spec.Entrypoint, want) {
		t.Fatalf("entrypoint = %q; want %q", spec.Entrypoint, want)
	}
	outputs, ok := mountAt(spec, "/kodema/outputs/build")
	if !ok {
		t.Fatalf("mounts %+v hold no output directory for the action", spec.Mounts)
	}
	if outputs.Type != "tmpfs" || outputs.ReadOnly || outputs.SizeBytes != 1<<20 {
		t.Fatalf("output mount = %+v; want a writable tmpfs bounded to 1 MiB", outputs)
	}
	if info, err := os.Stat(fx.hostPath("/kodema/outputs/build")); err != nil || !info.IsDir() {
		t.Fatalf("the activation directory has no mount point for the output directory: %v", err)
	}
	// An earlier action's output stays a read-only file beside it.
	if got := readHostFile(t, fx.hostPath("/kodema/outputs/clone/commit")); string(got) != "abc" {
		t.Fatalf("earlier output = %q", got)
	}
}

// TestToolboxActionWithoutOutputsGetsNoOutputDirectory pins that an action
// that declares no output runs the script alone.
func TestToolboxActionWithoutOutputsGetsNoOutputDirectory(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.request.Container.Outputs = nil
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)

	want := []string{"/bin/sh", "-c", `export PATH="${PATH:+$PATH:}/kodema/bin"; exec /bin/sh /kodema/script`}
	if !slices.Equal(spec.Entrypoint, want) {
		t.Fatalf("entrypoint = %q; want %q", spec.Entrypoint, want)
	}
	if mount, ok := mountAt(spec, "/kodema/outputs/build"); ok {
		t.Fatalf("an action without outputs got %+v", mount)
	}
	if hostFileExists(t, fx.hostPath("/kodema/outputs/build")) {
		t.Fatal("an action without outputs got an output mount point")
	}
}

// TestWithoutAToolboxTheContainerIsUnchanged pins the adapter without a
// toolbox: the plain script entrypoint, the two original mounts and no output
// directory, even for an action that declares outputs.
func TestWithoutAToolboxTheContainerIsUnchanged(t *testing.T) {
	fx := newFixture(t)
	fx.request.Outputs = nil
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)

	if want := []string{"/bin/sh", "/kodema/script"}; !slices.Equal(spec.Entrypoint, want) {
		t.Fatalf("entrypoint = %q; want %q", spec.Entrypoint, want)
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("mounts = %+v; want the activation bind and the workspace volume only", spec.Mounts)
	}
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "GIT_CONFIG_") || strings.HasPrefix(entry, "KODEMA_GIT_URL=") {
			t.Fatalf("environment holds %q without a configured Git service", entry)
		}
	}
}

// TestGitServiceIsConfiguredForItsOriginOnly pins the Git environment of a
// workload and that a pipeline cannot shadow it.
func TestGitServiceIsConfiguredForItsOriginOnly(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.request.Container.Env = append(fx.request.Container.Env,
		definition.EnvVar{Name: "KODEMA_GIT_URL", Value: "https://evil.example"},
		definition.EnvVar{Name: "GIT_CONFIG_COUNT", Value: "1"},
		definition.EnvVar{Name: "GIT_CONFIG_KEY_0", Value: "credential.helper"},
		definition.EnvVar{Name: "GIT_CONFIG_VALUE_0", Value: "store"},
		definition.EnvVar{Name: "GIT_AUTHOR_NAME", Value: "kept"},
	)
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)

	for _, want := range []string{
		"KODEMA_GIT_URL=" + testGitURL,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://host.containers.internal:8443.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.https://host.containers.internal:8443.helper",
		"GIT_CONFIG_VALUE_1=/kodema/bin/kodema --trusted-origin 'https://host.containers.internal:8443' git credential",
		"GIT_AUTHOR_NAME=kept",
	} {
		if !slices.Contains(spec.Env, want) {
			t.Fatalf("environment %q lacks %q", spec.Env, want)
		}
	}
	for _, refused := range []string{"KODEMA_GIT_URL=https://evil.example", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=store"} {
		if slices.Contains(spec.Env, refused) {
			t.Fatalf("environment %q kept the pipeline's %q", spec.Env, refused)
		}
	}
	// Every name appears once: the driver sends the environment as a map.
	seen := map[string]bool{}
	for _, entry := range spec.Env {
		name, _, _ := strings.Cut(entry, "=")
		if seen[name] {
			t.Fatalf("environment %q names %s twice", spec.Env, name)
		}
		seen[name] = true
	}
}

// TestStartFollowsThePreparedToolbox pins that a reconstructed adapter with a
// newer toolbox, or with none, starts a prepared workload with the copy and
// the Git service Prepare recorded.
func TestStartFollowsThePreparedToolbox(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.prepare(t)
	prepared, _, found, err := fx.adapter.readPreparation(context.Background(), fx.request.Identity.WorkloadID)
	if err != nil || !found {
		t.Fatalf("read preparation: found=%t err=%v", found, err)
	}

	newer := filepath.Join(t.TempDir(), "kodema")
	if err := os.WriteFile(newer, []byte("toolbox-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	upgraded := fx.newToolboxAdapter(t, newer)
	if upgraded.toolboxDir == prepared.Toolbox {
		t.Fatal("two different binaries share one installation copy")
	}
	if _, err := upgraded.Prepare(t.Context(), fx.request); err != nil {
		t.Fatalf("retrying the original Prepare after upgrade: %v", err)
	}
	if _, err := upgraded.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err != nil {
		t.Fatalf("Start on the upgraded adapter: %v", err)
	}
	spec := fx.createdSpec(t)
	mount, ok := mountAt(spec, "/kodema/bin")
	if !ok || mount.Source != prepared.Toolbox {
		t.Fatalf("toolbox mount = %+v; want the prepared copy %q", mount, prepared.Toolbox)
	}
	if got := readHostFile(t, filepath.Join(mount.Source, "kodema")); string(got) != "toolbox-v1" {
		t.Fatalf("the prepared workload runs toolbox %q; want the one it was prepared with", got)
	}
}

func TestLegacyPreparationKeepsImageWorkingDirectoryAfterUpgrade(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	workload := fx.request.Identity.WorkloadID
	record, _, _, err := fx.adapter.readPreparation(t.Context(), workload)
	if err != nil {
		t.Fatal(err)
	}
	record.WorkingDir = "" // 0.2.3 had none of the toolbox or working_dir fields.
	data, err := encodePreparationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.files.WriteFile(t.Context(), preparationPath(fx.root, workload), data, readOnlyMode); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "kodema")
	if err := os.WriteFile(source, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx.adapter = fx.newToolboxAdapter(t, source)
	fx.prepare(t)
	fx.start(t)
	spec := fx.createdSpec(t)
	if spec.WorkingDir != "" || !slices.Equal(spec.Entrypoint, []string{"/bin/sh", "/kodema/script"}) || len(spec.Mounts) != 2 {
		t.Fatalf("upgrade changed a legacy launch: workdir=%q entrypoint=%q mounts=%+v", spec.WorkingDir, spec.Entrypoint, spec.Mounts)
	}
}

func TestPrepareRefusesToolboxReplacedSinceStartup(t *testing.T) {
	fx, source := toolboxFixture(t, "before")
	if err := os.WriteFile(source, []byte("after"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := fx.adapter.Prepare(t.Context(), fx.request)
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if hostFileExists(t, filepath.Join(fx.adapter.toolboxDir, "kodema")) {
		t.Fatal("installed replacement bytes under the previous digest")
	}
}

// TestToolboxConfigurationIsRefusedWhenUnusable pins the configuration
// refusals of the toolbox file and the workload Git URL.
func TestToolboxConfigurationIsRefusedWhenUnusable(t *testing.T) {
	directory := t.TempDir()
	usable := filepath.Join(directory, "kodema")
	if err := os.WriteFile(usable, []byte("toolbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name    string
		toolbox string
		gitURL  string
	}{
		{name: "a relative toolbox path", toolbox: "kodema"},
		{name: "a missing toolbox file", toolbox: filepath.Join(directory, "absent")},
		{name: "a toolbox directory", toolbox: directory},
		{name: "a plain-http Git URL", toolbox: usable, gitURL: "http://git.example.com"},
		{name: "a Git URL with user information", toolbox: usable, gitURL: "https://user@git.example.com"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := New(Config{
				Libpod:      newFakeLibpod(),
				Files:       newFakeFiles(),
				Root:        t.TempDir(),
				KodemaURL:   testKodemaURL,
				ToolboxFile: testCase.toolbox,
				GitURL:      testCase.gitURL,
				Cleanup:     CleanupPolicy{Attempts: 1},
				Clock:       newFakeClock(),
			})
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

// TestRemoveKeepsTheInstalledToolbox pins that removing a workload removes its
// own materialization and mount points and never the installation copy
// another workload may still run from.
func TestRemoveKeepsTheInstalledToolbox(t *testing.T) {
	fx, _ := toolboxFixture(t, "toolbox-v1")
	fx.prepare(t)
	installed := filepath.Join(fx.adapter.toolboxDir, "kodema")
	if _, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove left the workload's materialization")
	}
	if !hostFileExists(t, installed) {
		t.Fatal("Remove deleted the installed toolbox")
	}
}
