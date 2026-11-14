// Preparation behavior: confined read-only activation files, the retained
// workspace volume, the rendered fail-fast script, reserved environment,
// idempotency and every failure that must prevent a start.
package podman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func mustInputPath(t *testing.T, key id.InputKey) string {
	t.Helper()
	path, err := workspace.InputPath(key)
	if err != nil {
		t.Fatalf("workspace.InputPath(%q): %v", key, err)
	}
	return path
}

func mustOutputPath(t *testing.T, action id.ActionName, key id.OutputKey) string {
	t.Helper()
	path, err := workspace.OutputPath(action, key)
	if err != nil {
		t.Fatalf("workspace.OutputPath(%q, %q): %v", action, key, err)
	}
	return path
}

func TestPrepareMaterializesConfinedSnapshot(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	input := fx.hostPath(mustInputPath(t, "input-a"))
	if got := readHostFile(t, input); string(got) != "line1\nline2\n" {
		t.Fatalf("input bytes = %q, want exact admitted bytes", got)
	}
	if mode := hostFileMode(t, input); mode != 0o444 {
		t.Fatalf("input mode = %o, want 0444", mode)
	}

	output := fx.hostPath(mustOutputPath(t, "build", "out-a"))
	if got := readHostFile(t, output); string(got) != "out" {
		t.Fatalf("output bytes = %q, want %q", got, "out")
	}
	if mode := hostFileMode(t, output); mode != 0o444 {
		t.Fatalf("output mode = %o, want 0444", mode)
	}

	task := fx.hostPath(workspace.TaskPath)
	if got := readHostFile(t, task); string(got) != `{"id":"task-1"}` {
		t.Fatalf("task bytes = %q, want the exact task snapshot", got)
	}

	secret := fx.hostPath(filepath.Join(workspace.SecretRoot, "API_KEY"))
	if got := readHostFile(t, secret); string(got) != "s3cr3t" {
		t.Fatalf("secret bytes = %q, want exact declared value", got)
	}
	if mode := hostFileMode(t, secret); mode != 0o444 {
		t.Fatalf("secret mode = %o, want 0444", mode)
	}

	if got := readHostFile(t, fx.credentialPath()); string(got) != "tok-abc" {
		t.Fatalf("credential bytes = %q, want tok-abc", got)
	}
	if mode := hostFileMode(t, fx.credentialPath()); mode != 0o444 {
		t.Fatalf("credential mode = %o, want 0444", mode)
	}

	wantScript, err := script.Render(fx.request.Container.Commands)
	if err != nil {
		t.Fatalf("script.Render returned error: %v", err)
	}
	if got := readHostFile(t, fx.scriptPath()); string(got) != wantScript {
		t.Fatalf("script bytes = %q, want script.Render output %q", got, wantScript)
	}
	if mode := hostFileMode(t, fx.scriptPath()); mode != 0o444 {
		t.Fatalf("script mode = %o, want 0444", mode)
	}

	for _, operation := range []string{"CreateContainer", "InspectContainer", "StartContainer", "StopContainer", "RemoveContainer", "ReadLogs"} {
		if calls := fx.libpod.callsFor(operation); calls != 0 {
			t.Fatalf("Prepare called %s %d times, want 0", operation, calls)
		}
	}
	if calls := fx.libpod.inspectedVolumes(); len(calls) != 1 || calls[0] != fx.volumeName {
		t.Fatalf("Inspected volumes = %v, want the retained workspace volume %q", calls, fx.volumeName)
	}
	if calls := fx.libpod.inspectedImages(); len(calls) != 1 || calls[0] != testImage {
		t.Fatalf("Inspected images = %v, want the workload image %q", calls, testImage)
	}
	if volume, ok := fx.libpod.volume(fx.volumeName); !ok {
		t.Fatal("Prepare removed the retained workspace volume")
	} else if volume.Labels[LabelWorkspace] != string(fx.request.WorkspaceID) {
		t.Fatalf("workspace volume labels = %v", volume.Labels)
	}
}

func TestPrepareInspectsExistingWorkspaceVolume(t *testing.T) {
	t.Run("missing volume", func(t *testing.T) {
		fx := newFixture(t)
		fx.libpod.deleteVolume(fx.volumeName)
		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertFaultClass(t, err, fault.NotFound)
		if calls := fx.files.callCount("WriteFile"); calls != 0 {
			t.Fatalf("missing workspace volume wrote %d files", calls)
		}
		result, startErr := fx.startResult(t)
		if startErr == nil {
			t.Fatal("Start hid a missing workspace volume")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("start outcome = %q, want unknown", result.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatalf("Start created a container without a workspace volume")
		}
	})

	t.Run("foreign workspace volume", func(t *testing.T) {
		fx := newFixture(t)
		fx.libpod.addVolume(fx.volumeName, map[string]string{LabelWorkspace: "workspace-2"})
		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertFaultClass(t, err, fault.Forbidden)
		if calls := fx.files.callCount("WriteFile"); calls != 0 {
			t.Fatalf("foreign workspace volume wrote %d files", calls)
		}
	})
}

func TestPrepareFailsFastWhenWorkloadImageIsAbsentLocally(t *testing.T) {
	t.Run("missing image refuses before writing preparation", func(t *testing.T) {
		fx := newFixture(t)
		fx.libpod.deleteImage(testImage)

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertFaultClass(t, err, fault.DependencyUnavailable)
		wantDetail := `workload image "` + testImage + `" is not present in the local container store; pull it before starting the execution`
		if message, ok := fault.MessageOf(err); !ok || message != wantDetail {
			t.Fatalf("Prepare error message = %q (ok=%v); want %q", message, ok, wantDetail)
		}
		if calls := fx.files.callCount("WriteFile"); calls != 0 {
			t.Fatalf("missing workload image wrote %d files", calls)
		}
		if hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("missing workload image left a materialization root behind")
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatalf("missing workload image called CreateContainer %d times", calls)
		}

		result, startErr := fx.startResult(t)
		if startErr == nil {
			t.Fatal("Start hid a missing workload image")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("start outcome = %q, want unknown", result.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatal("Start created a container after preparation refused an absent image")
		}

		// Once the image is present locally, preparation and start succeed.
		fx.libpod.addImage(testImage)
		fx.prepare(t)
		accepted := fx.start(t)
		if accepted.Outcome != engine.StartAccepted {
			t.Fatalf("start outcome after pulling image = %q, want accepted", accepted.Outcome)
		}
	})

	t.Run("unreachable image store refuses as dependency unavailable", func(t *testing.T) {
		fx := newFixture(t)
		fx.libpod.setImageError(errFakeUnavailable)

		_, err := fx.adapter.Prepare(context.Background(), fx.request)
		assertFaultClass(t, err, fault.DependencyUnavailable)
		if strings.Contains(err.Error(), errFakeUnavailable.Error()) {
			t.Fatalf("Prepare leaked private image store error: %v", err)
		}
		if calls := fx.files.callCount("WriteFile"); calls != 0 {
			t.Fatalf("unreachable image store wrote %d files", calls)
		}
	})
}

func TestPrepareRefusesUnsafeOrIncompleteInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*engine.PrepareRequest)
	}{
		{"unpinned image", func(request *engine.PrepareRequest) {
			request.Container.Image = "registry.example.com/tools/build:latest"
		}},
		{"empty image", func(request *engine.PrepareRequest) {
			request.Container.Image = ""
		}},
		{"no commands", func(request *engine.PrepareRequest) {
			request.Container.Commands = nil
		}},
		{"NUL command", func(request *engine.PrepareRequest) {
			request.Container.Commands = []string{"echo a\x00b"}
		}},
		{"traversal input key", func(request *engine.PrepareRequest) {
			request.Inputs = map[id.InputKey]string{"../../etc/passwd": "x"}
		}},
		{"empty credential", func(request *engine.PrepareRequest) {
			request.Credential = ""
		}},
		{"invalid action name", func(request *engine.PrepareRequest) {
			request.ActionName = "not a slug"
		}},
		{"invalid task id", func(request *engine.PrepareRequest) {
			request.TaskID = "not a task"
		}},
		{"reserved environment", func(request *engine.PrepareRequest) {
			request.Container.Env = append(request.Container.Env,
				definition.EnvVar{Name: id.EnvAction, Value: "spoofed"})
		}},
		{"reserved secret name", func(request *engine.PrepareRequest) {
			request.Secrets = append(request.Secrets,
				engine.SecretValue{Name: id.SecretTokenName, Value: "spoofed"})
		}},
		{"invalid output action", func(request *engine.PrepareRequest) {
			request.Outputs = map[id.ActionName]kernel.Outputs{"not a slug": {"out-a": "x"}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fx := newFixture(t)
			testCase.mutate(&fx.request)
			_, err := fx.adapter.Prepare(context.Background(), fx.request)
			assertFaultClass(t, err, fault.InvalidInput)
			if calls := fx.files.callCount("WriteFile"); calls != 0 {
				t.Fatalf("%s wrote %d files before refusing", testCase.name, calls)
			}
			if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
				t.Fatalf("%s created a container", testCase.name)
			}
			if hostFileExists(t, fx.workloadRoot()) {
				t.Fatalf("%s left a materialization root behind", testCase.name)
			}
		})
	}
}

func TestPrepareRefusesSymlinkEscape(t *testing.T) {
	fx := newFixture(t)
	outside := t.TempDir()
	if err := os.MkdirAll(fx.workloadRoot(), 0o755); err != nil {
		t.Fatalf("creating workload root: %v", err)
	}
	if err := os.Symlink(outside, fx.kodamaRoot()); err != nil {
		t.Fatalf("creating escape symlink: %v", err)
	}

	_, err := fx.adapter.Prepare(context.Background(), fx.request)
	assertFaultClass(t, err, fault.Forbidden)

	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("reading outside directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("Prepare wrote %d entries through a symlink outside the root", len(entries))
	}

	result, startErr := fx.startResult(t)
	if startErr == nil {
		t.Fatal("Start hid a preparation that never completed")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("start outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatal("Start created a container from an unsafe preparation")
	}
}

func TestPrepareIsIdempotentAndRefusesChangedContent(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	input := fx.hostPath(mustInputPath(t, "input-a"))
	original := readHostFile(t, input)

	fx.prepare(t)
	if got := readHostFile(t, input); string(got) != string(original) {
		t.Fatalf("idempotent Prepare changed input bytes: %q", got)
	}

	fx.request.Inputs["input-a"] = "changed\n"
	_, err := fx.adapter.Prepare(context.Background(), fx.request)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)
	if got := readHostFile(t, input); string(got) != string(original) {
		t.Fatalf("changed preparation overwrote the committed input: %q", got)
	}

	fx.request.Inputs["input-a"] = "line1\nline2\n"
	fx.request.Secrets[0].Value = "rotated-secret"
	_, err = fx.adapter.Prepare(context.Background(), fx.request)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)

	fx.request.Secrets[0].Value = "s3cr3t"
	result := fx.start(t)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q, want accepted after a refused change", result.Outcome)
	}
	specs := fx.libpod.createSpecs()
	if len(specs) != 1 {
		t.Fatalf("created %d containers, want 1", len(specs))
	}
	if specs[0].Image != testImage {
		t.Fatalf("container image = %q, want the pinned image", specs[0].Image)
	}
}

func TestPreparePartialFailureNeverAuthorizesStart(t *testing.T) {
	fx := newFixture(t)
	fx.files.failOnCall("WriteFile", 2, errFakeUnavailable)

	_, err := fx.adapter.Prepare(context.Background(), fx.request)
	assertFaultClass(t, err, fault.DependencyUnavailable)

	result, startErr := fx.startResult(t)
	if startErr == nil {
		t.Fatal("Start hid a partial preparation")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("start outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatal("Start created a container from a partial preparation")
	}

	fx.files.clearFailOn("WriteFile")
	fx.prepare(t)
	accepted := fx.start(t)
	if accepted.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome after recovery = %q, want accepted", accepted.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1 after recovery", calls)
	}
}

func TestStartBuildsPinnedContainerSpec(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.start(t)

	specs := fx.libpod.createSpecs()
	if len(specs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Name != fx.containerName {
		t.Fatalf("container name = %q, want stable %q", spec.Name, fx.containerName)
	}
	if spec.Image != testImage {
		t.Fatalf("container image = %q, want the digest-pinned image", spec.Image)
	}
	if spec.RestartPolicy != "no" {
		t.Fatalf("restart policy = %q, want no automatic replay", spec.RestartPolicy)
	}
	wantEntrypoint := []string{script.ShellPath, "/kodama/script"}
	if len(spec.Entrypoint) != len(wantEntrypoint) ||
		spec.Entrypoint[0] != wantEntrypoint[0] || spec.Entrypoint[1] != wantEntrypoint[1] {
		t.Fatalf("entrypoint = %v, want %v", spec.Entrypoint, wantEntrypoint)
	}

	wantEnv := []string{
		joinEnv("BUILD_MODE", "release"),
		joinEnv("KODAMA_URL", testKodamaURL),
		joinEnv("KODAMA_EXECUTION", string(fx.request.Identity.ExecutionID)),
		joinEnv("KODAMA_ACTION", string(fx.request.ActionName)),
		joinEnv("KODAMA_TASK", string(fx.request.TaskID)),
	}
	if len(spec.Env) != len(wantEnv) {
		t.Fatalf("env = %v, want exactly %v", spec.Env, wantEnv)
	}
	for index := range wantEnv {
		if spec.Env[index] != wantEnv[index] {
			t.Fatalf("env[%d] = %q, want %q (full %v)", index, spec.Env[index], wantEnv[index], spec.Env)
		}
	}

	for key, want := range fx.containerLabels() {
		if spec.Labels[key] != want {
			t.Fatalf("label %q = %q, want %q", key, spec.Labels[key], want)
		}
	}

	if len(spec.Mounts) != 2 {
		t.Fatalf("mounts = %+v, want exactly the kodama bind and the workspace volume", spec.Mounts)
	}
	var bind, volume *Mount
	for index := range spec.Mounts {
		switch spec.Mounts[index].Type {
		case "bind":
			bind = &spec.Mounts[index]
		case "volume":
			volume = &spec.Mounts[index]
		}
	}
	if bind == nil {
		t.Fatalf("mounts = %+v, want a directory bind", spec.Mounts)
	}
	if bind.Source != fx.kodamaRoot() {
		t.Fatalf("bind source = %q, want the daemon-visible directory %q", bind.Source, fx.kodamaRoot())
	}
	if bind.Destination != "/kodama" || !bind.ReadOnly {
		t.Fatalf("bind = %+v, want /kodama read-only", *bind)
	}
	if info, err := os.Stat(bind.Source); err != nil || !info.IsDir() {
		t.Fatalf("bind source %q is not a directory: %v", bind.Source, err)
	}
	if !hostFileExists(t, filepath.Join(bind.Source, "secrets", "KODAMA_TOKEN")) {
		t.Fatal("the credential is not inside the daemon-visible bind directory")
	}
	if volume == nil {
		t.Fatalf("mounts = %+v, want the workspace volume", spec.Mounts)
	}
	if volume.Source != fx.volumeName {
		t.Fatalf("volume source = %q, want retained volume %q", volume.Source, fx.volumeName)
	}
	if volume.Destination != "/workspace" || volume.ReadOnly {
		t.Fatalf("volume mount = %+v, want /workspace read-write", *volume)
	}

	serialized := strings.Join(append(append([]string{}, spec.Env...), spec.Entrypoint...), "\n")
	for _, forbidden := range []string{"s3cr3t", "tok-abc"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("container spec leaked %q: %q", forbidden, serialized)
		}
		for key, value := range spec.Labels {
			if strings.Contains(key+value, forbidden) {
				t.Fatalf("container label leaked %q", forbidden)
			}
		}
	}
}

func TestStartOmitsTaskContextWhenTaskFree(t *testing.T) {
	fx := newFixture(t)
	fx.request.TaskID = ""
	fx.request.Task = nil
	fx.prepare(t)

	if hostFileExists(t, fx.hostPath(workspace.TaskPath)) {
		t.Fatal("task-free preparation wrote a task snapshot")
	}

	fx.start(t)
	specs := fx.libpod.createSpecs()
	if len(specs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(specs))
	}
	for _, entry := range specs[0].Env {
		if strings.HasPrefix(entry, "KODAMA_TASK=") {
			t.Fatalf("task-free container carries %q", entry)
		}
	}
	if specs[0].Env[len(specs[0].Env)-1] != joinEnv("KODAMA_ACTION", string(fx.request.ActionName)) {
		t.Fatalf("reserved env does not end with KODAMA_ACTION: %v", specs[0].Env)
	}
}

func TestPrepareRefusesWithoutStartingOrWritingTheWorkspace(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	for _, path := range fx.files.callsFor("WriteFile") {
		if strings.HasPrefix(path, fx.volumeName) {
			t.Fatalf("Prepare wrote into the retained workspace volume path %q", path)
		}
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("Prepare started %d containers", calls)
	}
}
