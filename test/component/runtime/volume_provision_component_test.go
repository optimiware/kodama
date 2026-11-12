//go:build component

package runtimeconformance

import (
	"maps"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanDriverCreateVolumeProvisionsRetainedWorkspace proves the composed
// server's missing primitive end to end on the real rootless Podman API: the
// deterministic workspace volume is absent, the adapter's Retain refuses it,
// the Libpod driver creates the owned volume, Retain then succeeds, a second
// create is idempotent, and cleanup removes the volume.
func TestPodmanDriverCreateVolumeProvisionsRetainedWorkspace(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-provision")

	// Start from the state the composed server reaches before its first
	// activation: the deterministic workspace volume does not exist yet.
	if err := runtimesupport.RemoveWorkspaceVolume(ctx, fixture.volumeName); err != nil {
		t.Fatalf("remove pre-created fixture volume: %v", err)
	}
	if runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatalf("workspace volume %q still exists after removal", fixture.volumeName)
	}
	// The gap under test: retention never recreates a missing volume.
	assertFaultClass(t, fixture.adapter.Retain(ctx, fixture.workspaceID), fault.NotFound)

	// The Shell provisions the volume with the ownership label the adapter
	// validates, plus this run's cleanup labels so a sweep can scope it.
	labels := map[string]string{podman.LabelWorkspace: string(fixture.workspaceID)}
	maps.Copy(labels, runtimesupport.RunLabels())
	created, err := fixture.libpod.CreateVolume(ctx, podman.Volume{Name: fixture.volumeName, Labels: labels})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if created.Name != fixture.volumeName {
		t.Fatalf("created volume name = %q; want %q", created.Name, fixture.volumeName)
	}
	if created.Labels[podman.LabelWorkspace] != string(fixture.workspaceID) {
		t.Fatalf("created volume labels = %v; want the workspace ownership label", created.Labels)
	}
	if !runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatalf("volume %q was not created", fixture.volumeName)
	}
	observed, err := runtimesupport.VolumeLabels(ctx, fixture.volumeName)
	if err != nil {
		t.Fatalf("read created volume labels: %v", err)
	}
	if observed[podman.LabelWorkspace] != string(fixture.workspaceID) {
		t.Fatalf("observed volume labels = %v; want the workspace ownership label", observed)
	}

	// With the volume provisioned the adapter's Retain succeeds.
	if err := fixture.adapter.Retain(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Retain after provisioning: %v", err)
	}

	// A retry after a lost create response is idempotent: the driver inspects
	// the existing same-owner volume instead of creating a second one.
	again, err := fixture.libpod.CreateVolume(ctx, podman.Volume{Name: fixture.volumeName, Labels: labels})
	if err != nil {
		t.Fatalf("idempotent CreateVolume: %v", err)
	}
	if again.Name != fixture.volumeName || again.Labels[podman.LabelWorkspace] != string(fixture.workspaceID) {
		t.Fatalf("idempotent create returned %+v; want the existing owned volume", again)
	}

	// Cleanup removes the run-owned volume.
	if err := runtimesupport.RemoveWorkspaceVolume(ctx, fixture.volumeName); err != nil {
		t.Fatalf("remove workspace volume: %v", err)
	}
	if runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatalf("cleanup left workspace volume %q behind", fixture.volumeName)
	}
}
