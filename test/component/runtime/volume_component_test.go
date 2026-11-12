//go:build component

package runtimeconformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanVolumeRemoveRefusesInUseVolume proves the real Podman seam never
// force-deletes a workspace volume that another live container mounts. Podman
// must refuse the removal (409), the adapter must see an uncertain failure, and
// both the volume and the running container must remain untouched.
func TestPodmanVolumeRemoveRefusesInUseVolume(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-volume")

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	container := "kodama-i02d-inuse-" + suffix
	args := []string{
		"run", "-d", "--name", container,
		"--label", runtimesupport.WorkspaceLabel + "=" + string(fixture.workspaceID),
	}
	args = append(args, runLabelArgs()...)
	args = append(args, "-v", fixture.volumeName+":/data", fixture.image.Reference, "sleep", "60")
	if _, err := runtimesupport.RunPodman(ctx, args...); err != nil {
		t.Fatalf("run container mounting the workspace volume: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = runtimesupport.RemoveContainerByName(cleanupCtx, container)
	})

	err := fixture.libpod.RemoveVolume(ctx, fixture.volumeName)
	if err == nil {
		t.Fatal("removing an in-use volume reported success")
	}
	if errors.Is(err, podman.ErrNotFound) {
		t.Fatalf("in-use volume removal = %v; must not be ErrNotFound", err)
	}
	if errors.Is(err, podman.ErrAlreadyExists) {
		t.Fatalf("in-use volume removal = %v; must not be ErrAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("in-use volume removal = %v; want the 409 in-use conflict", err)
	}

	if !runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("in-use workspace volume was removed")
	}
	running, runErr := runtimesupport.ContainerRunning(ctx, container)
	if runErr != nil {
		t.Fatalf("container mounting the volume disappeared: %v", runErr)
	}
	if !running {
		t.Fatal("container mounting the volume was stopped by the refused removal")
	}
}
