package factory

import (
	"context"
	"testing"
)

// TestWorkspaceVolumePresentFailsClosed proves the volume probe reports an
// unknown Podman state as an error instead of absence. A canceled context makes
// the CLI invocation fail without touching Podman; a fail-open probe would
// report "absent" and let a cleanup assertion pass without evidence.
func TestWorkspaceVolumePresentFailsClosed(t *testing.T) {
	harness := &Harness{T: t}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	present, err := harness.WorkspaceVolumePresent(ctx, "workspace-1")
	if err == nil {
		t.Fatalf("WorkspaceVolumePresent on a failed probe = %t, nil error; want a probe error", present)
	}
	if present {
		t.Fatalf("WorkspaceVolumePresent on a failed probe reported the volume present")
	}
}
