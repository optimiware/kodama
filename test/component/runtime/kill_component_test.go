//go:build component

package runtimeconformance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanKillHelperProcess is the child controller for
// TestPodmanRuntimeKilledControllerBetweenLaunchAndObservation. It prepares and
// creates the container, then blocks inside the start call after publishing a
// marker. The parent SIGKILLs it between launch intent and observation.
func TestPodmanKillHelperProcess(t *testing.T) {
	if os.Getenv("KODAMA_RUNTIME_HELPER") != "1" {
		t.Skip("helper process only")
	}
	root := os.Getenv("KODAMA_HELPER_ROOT")
	socket := os.Getenv("KODAMA_HELPER_SOCKET")
	marker := os.Getenv("KODAMA_HELPER_MARKER")
	image := os.Getenv("KODAMA_HELPER_IMAGE")
	// The parent chooses the complete run-scoped identity and workspace; the
	// helper must never mint its own, or the killed controller's durable state
	// and container would belong to a different run namespace.
	identity := engine.EffectIdentity{
		ExecutionID:    id.ExecutionID(os.Getenv("KODAMA_HELPER_EXECUTION")),
		ActionID:       id.ActionID(os.Getenv("KODAMA_HELPER_ACTION")),
		WorkloadID:     id.WorkloadID(os.Getenv("KODAMA_HELPER_WORKLOAD")),
		IdempotencyKey: os.Getenv("KODAMA_HELPER_IDEMPOTENCY"),
	}
	workspaceID := id.WorkspaceID(os.Getenv("KODAMA_HELPER_WORKSPACE"))
	if identity.ExecutionID == "" || identity.WorkloadID == "" || workspaceID == "" {
		t.Fatal("helper environment is missing the parent-chosen identity/workspace")
	}

	libpod, err := podmandriver.NewLibpod(podmandriver.LibpodConfig{SocketPath: socket, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("helper NewLibpod: %v", err)
	}
	files, err := podmandriver.NewFiles(root)
	if err != nil {
		t.Fatalf("helper NewFiles: %v", err)
	}
	adapter, err := podman.New(podman.Config{
		Libpod:    runtimesupport.ScopedLibpod(&blockingStartLibpod{Libpod: libpod, marker: marker}),
		Files:     files,
		Root:      root,
		KodamaURL: testKodamaURL,
		Cleanup:   podman.CleanupPolicy{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("helper adapter: %v", err)
	}
	request := testPrepareRequest(identity, workspaceID, "build", "task-1",
		testContainer(image, []string{`echo launched >> /workspace/launch-count`}, nil, nil), nil, "tok-abc")
	if _, err := adapter.Prepare(context.Background(), request); err != nil {
		t.Fatalf("helper Prepare: %v", err)
	}
	_, _ = adapter.Start(context.Background(), engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	t.Fatal("helper returned from a blocked start")
}

// blockingStartLibpod publishes a marker and blocks inside the start call.
type blockingStartLibpod struct {
	podman.Libpod
	marker string
}

func (b *blockingStartLibpod) StartContainer(ctx context.Context, name string) error {
	if err := os.WriteFile(b.marker, []byte("start-issued"), 0o644); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestPodmanRuntimeKilledControllerBetweenLaunchAndObservation SIGKILLs a real
// controller after it persisted the launch intent and created the container but
// before it observed the start. A reconstructed controller must report unknown,
// never never-started, and must not replay the command.
func TestPodmanRuntimeKilledControllerBetweenLaunchAndObservation(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-kill")
	identity := testIdentity("exec-kill", "action-kill", "workload-kill", "idem-kill")
	marker := filepath.Join(t.TempDir(), "start-issued")

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	helper := exec.Command(executable, "-test.run=^TestPodmanKillHelperProcess$", "-test.v")
	helper.Env = append(os.Environ(),
		"KODAMA_RUNTIME_HELPER=1",
		"KODAMA_HELPER_ROOT="+fixture.root,
		"KODAMA_HELPER_SOCKET="+fixture.service.Socket(),
		"KODAMA_HELPER_MARKER="+marker,
		"KODAMA_HELPER_IMAGE="+fixture.image.Reference,
		"KODAMA_HELPER_EXECUTION="+string(identity.ExecutionID),
		"KODAMA_HELPER_ACTION="+string(identity.ActionID),
		"KODAMA_HELPER_WORKLOAD="+string(identity.WorkloadID),
		"KODAMA_HELPER_IDEMPOTENCY="+identity.IdempotencyKey,
		"KODAMA_HELPER_WORKSPACE="+string(fixture.workspaceID),
		// The helper's container belongs to the parent suite's run scope, so a
		// sweep by the parent still removes it and dead-owner cleanup sees the
		// parent's pid, not the killed helper's.
		runtimesupport.RunOwnerEnv+"="+runtimesupport.RunOwnerPID(),
	)
	var output bytes.Buffer
	helper.Stdout = &output
	helper.Stderr = &output
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()
	waitForFile(t, ctx, marker)
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()

	recovered := newPodmanAdapterOnly(t, fixture)
	observation, err := recovered.Observe(ctx, engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Observe after kill: %v", err)
	}
	if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
		t.Fatalf("killed controller collapsed into never-started: %+v", observation)
	}
	start, err := recovered.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err == nil || start.Outcome != engine.StartUnknown {
		t.Fatalf("reconstructed start after kill = %q/%v; want unknown", start.Outcome, err)
	}
	if _, found := runtimesupport.ReadVolumeFile(ctx, t, fixture.volumeName, "launch-count"); found {
		t.Fatal("the killed controller's command ran before observation")
	}
	if names := podmanContainerNames(t, ctx, fixture.workspaceID); len(names) != 1 {
		t.Fatalf("workspace containers after kill = %v; want exactly one created container", names)
	}
	if err := runtimesupport.RemoveRunContainersInWorkspace(ctx, string(fixture.workspaceID)); err != nil {
		t.Fatalf("remove created container: %v", err)
	}
	if removal, err := recovered.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if err := recovered.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "killed-controller-between-launch-and-observation",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}
