//go:build component

package runtimeconformance

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// runLabelArgs renders this run's cleanup labels as Podman CLI arguments.
func runLabelArgs() []string {
	labels := runtimesupport.RunLabels()
	args := make([]string, 0, len(labels)*2)
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	return args
}

// TestRunScopedSweepLeavesForeignWorkspaceResources proves the suite's
// post-run sweep removes only resources carrying kodama.test=1 and this run's
// kodama.owner-pid. A container and a volume carrying only the product
// kodama.dev/workspace label (as a real Kodama installation, or a concurrent
// shard, would) must survive. Resources this run created must be removed.
func TestRunScopedSweepLeavesForeignWorkspaceResources(t *testing.T) {
	ctx := t.Context()
	service := runtimesupport.StartService(t)
	runtimesupport.ResolveImage(ctx, t, service.Podman(), runtimesupport.ImageAlpineTag)

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	foreignContainer := "kodama-i02d-foreign-" + suffix
	foreignVolume := "kodama-i02d-foreign-vol-" + suffix
	ownContainer := "kodama-i02d-own-" + suffix
	ownVolume := "kodama-i02d-own-vol-" + suffix

	// Foreign resources: product workspace label only, no test/owner scope.
	foreignWorkspace := "foreign-workspace-" + suffix
	if _, err := runtimesupport.RunPodman(ctx, "volume", "create",
		"--label", runtimesupport.WorkspaceLabel+"="+foreignWorkspace,
		foreignVolume); err != nil {
		t.Fatalf("create foreign volume: %v", err)
	}
	if _, err := runtimesupport.RunPodman(ctx,
		"run", "-d", "--name", foreignContainer,
		"--label", runtimesupport.WorkspaceLabel+"="+foreignWorkspace,
		"-v", foreignVolume+":/data",
		runtimesupport.ImageAlpineTag, "sleep", "60",
	); err != nil {
		t.Fatalf("run foreign container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = runtimesupport.RunPodman(cleanupCtx, "rm", "--force", "--volumes", foreignContainer)
		_, _ = runtimesupport.RunPodman(cleanupCtx, "volume", "rm", foreignVolume)
	})

	// Own resources: exactly the run labels the fixtures attach.
	ownArgs := []string{"volume", "create"}
	ownArgs = append(ownArgs, runLabelArgs()...)
	ownArgs = append(ownArgs, ownVolume)
	if _, err := runtimesupport.RunPodman(ctx, ownArgs...); err != nil {
		t.Fatalf("create own volume: %v", err)
	}
	runArgs := []string{"run", "-d", "--name", ownContainer}
	runArgs = append(runArgs, runLabelArgs()...)
	runArgs = append(runArgs, "-v", ownVolume+":/data", runtimesupport.ImageAlpineTag, "sleep", "60")
	if _, err := runtimesupport.RunPodman(ctx, runArgs...); err != nil {
		t.Fatalf("run own container: %v", err)
	}

	// The same two sweeps TestMain runs at suite exit.
	if err := runtimesupport.RemoveRunContainers(ctx); err != nil {
		t.Fatalf("RemoveRunContainers: %v", err)
	}
	if err := runtimesupport.RemoveRunVolumes(ctx); err != nil {
		t.Fatalf("RemoveRunVolumes: %v", err)
	}

	// Foreign resources survive untouched.
	running, err := runtimesupport.ContainerRunning(ctx, foreignContainer)
	if err != nil {
		t.Fatalf("foreign container disappeared: %v", err)
	}
	if !running {
		t.Fatal("foreign container was stopped by the run-scoped sweep")
	}
	if !runtimesupport.VolumeExists(ctx, foreignVolume) {
		t.Fatal("foreign volume was removed by the run-scoped sweep")
	}

	// This run's resources are gone.
	if _, err := runtimesupport.RunPodman(ctx, "container", "exists", ownContainer); err == nil {
		t.Fatal("run-owned container survived the run-scoped sweep")
	}
	if runtimesupport.VolumeExists(ctx, ownVolume) {
		t.Fatal("run-owned volume survived the run-scoped sweep")
	}
}

// TestRunScopedSweepFiltersOnRunLabels proves the sweep never selects by the
// product workspace label alone: a foreign container carrying the same
// run-scoped workspace id as this run still survives.
func TestRunScopedSweepFiltersOnRunLabels(t *testing.T) {
	ctx := t.Context()
	service := runtimesupport.StartService(t)
	runtimesupport.ResolveImage(ctx, t, service.Podman(), runtimesupport.ImageAlpineTag)

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	foreignContainer := "kodama-i02d-foreign-same-" + suffix
	if _, err := runtimesupport.RunPodman(ctx, "run", "-d", "--name", foreignContainer,
		"--label", runtimesupport.WorkspaceLabel+"="+runtimesupport.RunScoped("workspace-podman-1"),
		runtimesupport.ImageAlpineTag, "sleep", "60"); err != nil {
		t.Fatalf("run foreign container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = runtimesupport.RunPodman(cleanupCtx, "rm", "--force", "--volumes", foreignContainer)
	})

	if err := runtimesupport.RemoveRunContainers(ctx); err != nil {
		t.Fatalf("RemoveRunContainers: %v", err)
	}
	if running, err := runtimesupport.ContainerRunning(ctx, foreignContainer); err != nil || !running {
		t.Fatalf("foreign container with a colliding workspace label was removed: running=%v err=%v", running, err)
	}
}
