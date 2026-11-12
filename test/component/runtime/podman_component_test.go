//go:build component

package runtimeconformance

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanRuntimeManifestEnvironmentPermissionsAndExit proves the real
// Podman runtime materializes the manifest into the read-only bind, injects
// only the declared environment, applies explicit file modes/ownership and
// reports an affirmative exit.
func TestPodmanRuntimeManifestEnvironmentPermissionsAndExit(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-1")
	identity := testIdentity("exec-c1", "action-c1", "workload-c1", "idem-c1")
	commands := []string{
		`env | sort > /workspace/env.txt`,
		`stat -c '%a %u %g' /kodama/inputs/input-a > /workspace/modes.txt`,
		`stat -c '%a %u %g' /kodama/secrets/API_KEY >> /workspace/modes.txt`,
		`stat -c '%a %u %g' /kodama/script >> /workspace/modes.txt`,
		`cat /kodama/inputs/input-a > /workspace/input-copy.txt`,
		`cat /kodama/task.json > /workspace/task-copy.json`,
		`cat /kodama/secrets/API_KEY`,
		`cat /kodama/secrets/KODAMA_TOKEN`,
		`echo done > /workspace/done.txt`,
	}
	request := fixture.request(identity, commands,
		[]definition.EnvVar{{Name: "BUILD_MODE", Value: "release"}},
		[]engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t-value"}},
		"tok-abc",
	)
	t.Setenv("KODAMA_HOST_CANARY", "must-not-leak")
	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	start, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want accepted", start.Outcome)
	}
	observation := waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	code, ok := observation.ExitStatus()
	if !ok || code != 0 {
		t.Fatalf("exit status = %d/%v; want 0/true (%+v)", code, ok, observation)
	}
	if !observation.StopConfirmed() {
		t.Fatalf("exited observation has no confirmed stop: %+v", observation)
	}

	env := string(readVolume(t, ctx, fixture, "env.txt"))
	for _, want := range []string{
		"BUILD_MODE=release",
		"KODAMA_URL=" + testKodamaURL,
		"KODAMA_EXECUTION=" + string(identity.ExecutionID),
		"KODAMA_ACTION=build",
		"KODAMA_TASK=task-1",
	} {
		if !strings.Contains(env, want+"\n") && !strings.Contains(env, want) {
			t.Fatalf("container environment is missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "KODAMA_HOST_CANARY") {
		t.Fatalf("host environment leaked into the container:\n%s", env)
	}
	if strings.Contains(env, "s3cr3t-value") || strings.Contains(env, "tok-abc") {
		t.Fatalf("secret material appeared in the container environment:\n%s", env)
	}
	if strings.Contains(env, "API_KEY=") || strings.Contains(env, "KODAMA_TOKEN=") {
		t.Fatalf("secret/credential name appeared as an environment variable:\n%s", env)
	}

	modes := strings.TrimSpace(string(readVolume(t, ctx, fixture, "modes.txt")))
	if modes != "444 0 0\n444 0 0\n444 0 0" {
		t.Fatalf("materialized modes/ownership = %q; want 444/0/0 for every file", modes)
	}
	if got := string(readVolume(t, ctx, fixture, "input-copy.txt")); got != "line1\nline2\n" {
		t.Fatalf("input bytes = %q", got)
	}
	if got := string(readVolume(t, ctx, fixture, "task-copy.json")); got != `{"id":"task-1"}` {
		t.Fatalf("task snapshot = %q", got)
	}

	batch, err := fixture.adapter.Logs(ctx, engine.LogsRequest{Identity: identity, Limit: 1 << 20})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	logText := ""
	for _, chunk := range batch.Chunks {
		logText += string(chunk.Data)
	}
	if strings.Contains(logText, "s3cr3t-value") || strings.Contains(logText, "tok-abc") {
		t.Fatalf("secret or credential leaked into retained output: %q", logText)
	}
	if !strings.Contains(logText, "[REDACTED]") {
		t.Fatalf("redaction marker is absent from output: %q", logText)
	}

	containerName, err := podman.ContainerName(identity.WorkloadID)
	if err != nil {
		t.Fatalf("ContainerName: %v", err)
	}
	limits, err := runtimesupport.RunPodman(ctx, "inspect", "--format",
		"{{.HostConfig.CpuQuota}} {{.HostConfig.CpuPeriod}} {{.HostConfig.Memory}}", containerName)
	if err != nil {
		t.Fatalf("inspect resource limits: %v", err)
	}
	if got := strings.TrimSpace(string(limits.Stdout)); got != "100000 100000 1073741824" {
		t.Fatalf("container resource limits = %q; want 1 CPU / 1 GiB", got)
	}

	evidence, err := fixture.adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want removed", removal)
	}
	if !runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("Remove touched the retained workspace volume")
	}
	if err := fixture.adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("workspace volume still exists after Release")
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "manifest-environment-permissions-exit",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// readVolume reads one file from the fixture's retained workspace volume.
func readVolume(t *testing.T, ctx context.Context, fixture *podmanFixture, logical string) []byte {
	t.Helper()
	data, found := runtimesupport.ReadVolumeFile(ctx, t, fixture.volumeName, logical)
	if !found {
		t.Fatalf("workspace file %q is missing", logical)
	}
	return data
}
