package factory

import (
	"context"
	"fmt"
	"sort"

	"github.com/optimiware/kodama/internal/id"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// RunningContainers returns the subset of the run's label-selected containers
// that Podman reports as running. Containers lists every container including
// stopped ones, so a request wait (which keeps the stopped earlier workload and
// the retained workspace) needs this filter to prove no workload is running.
func (h *Harness) RunningContainers(ctx context.Context, key, value string) ([]string, error) {
	h.T.Helper()
	names, err := h.Containers(ctx, key, value)
	if err != nil {
		return nil, err
	}
	running := make([]string, 0, len(names))
	for _, name := range names {
		active, err := runtimesupport.ContainerRunning(ctx, name)
		if err != nil {
			return nil, err
		}
		if active {
			running = append(running, name)
		}
	}
	return running, nil
}

// ProbeContainerName returns the run-scoped Podman name for one probe
// container base. StartProbeContainer and RemoveProbeContainer act on one
// exact name, so a fixed literal would let two concurrent suite runs collide:
// one run's force-remove could delete the other's probe, or `podman run --name`
// would fail with "name already in use". The run scope keeps the name stable
// inside one run while separating concurrent runs.
func ProbeContainerName(base string) string {
	return probeContainerNameFor(runtimesupport.RunScope(), base)
}

// probeContainerNameFor maps a probe base through an explicit run scope. It is
// the seam the unit test uses to prove two independent runs derive distinct
// names without starting a container.
func probeContainerNameFor(scope runtimesupport.Scope, base string) string {
	return scope.ID(base)
}

// StartProbeContainer starts one detached container on the harness image that
// carries this run's cleanup labels plus one exact label pair. It is a positive
// control for a label-filtered evidence probe: an empty result is only
// meaningful once the same probe is shown to observe a real container selected
// by that label. The caller removes it with RemoveProbeContainer.
func (h *Harness) StartProbeContainer(ctx context.Context, name, labelKey, labelValue string) error {
	h.T.Helper()
	if !h.fixture.valid() {
		return fmt.Errorf("factory: probe container needs a fixture identifier")
	}
	labels := h.fixture.labels()
	labels[labelKey] = labelValue
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := []string{"run", "--detach", "--name", name}
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, h.Image.Reference, "sleep", "120")
	if _, err := runtimesupport.RunPodman(ctx, args...); err != nil {
		return fmt.Errorf("start probe container %s: %w", name, err)
	}
	return nil
}

// StartWorkspaceProbeContainer starts one detached container that mounts a
// retained workspace volume and carries the workspace label. It is the
// external-reference obstruction a cleanup-recovery test needs: workspace
// release must refuse while a real container references the volume, so the
// failure is genuine and attributable rather than a fake hook.
func (h *Harness) StartWorkspaceProbeContainer(ctx context.Context, name, workspaceID string) error {
	h.T.Helper()
	if !h.fixture.valid() {
		return fmt.Errorf("factory: workspace probe container needs a fixture identifier")
	}
	if workspaceID == "" {
		return fmt.Errorf("factory: workspace probe container needs a workspace identity")
	}
	volume, err := runtimepodman.WorkspaceVolumeName(id.WorkspaceID(workspaceID))
	if err != nil {
		return fmt.Errorf("workspace probe volume name: %w", err)
	}
	labels := h.fixture.labels()
	labels[runtimepodman.LabelWorkspace] = workspaceID
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := []string{"run", "--detach", "--name", name}
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, "--volume", volume+":/workspace", h.Image.Reference, "sleep", "300")
	if _, err := runtimesupport.RunPodman(ctx, args...); err != nil {
		return fmt.Errorf("start workspace probe container %s: %w", name, err)
	}
	return nil
}

// RemoveProbeContainer force-removes one probe container this test started.
func (h *Harness) RemoveProbeContainer(ctx context.Context, name string) error {
	h.T.Helper()
	return runtimesupport.RemoveContainerByName(ctx, name)
}

// RunningExecutionContainer returns the one running workload container of an
// execution, or "" while none is running yet. More than one running container
// for one execution is refused: it would make the workload file probe
// ambiguous.
func (h *Harness) RunningExecutionContainer(ctx context.Context, executionID string) (string, error) {
	h.T.Helper()
	running, err := h.RunningContainers(ctx, runtimepodman.LabelExecution, executionID)
	if err != nil {
		return "", err
	}
	switch len(running) {
	case 0:
		return "", nil
	case 1:
		return running[0], nil
	default:
		return "", fmt.Errorf("execution %s has %d running workload containers %v", executionID, len(running), running)
	}
}

// ExecContainer runs one command inside a running workload container through
// the Podman CLI and returns its standard output. It is the workload-side
// release/read path: the test never assumes a fixed container name and never
// mounts the workspace from the host.
func (h *Harness) ExecContainer(ctx context.Context, container string, args ...string) (string, error) {
	h.T.Helper()
	if container == "" {
		return "", fmt.Errorf("factory: container command needs a container name")
	}
	command := append([]string{"exec", container}, args...)
	result, err := runtimesupport.RunPodman(ctx, command...)
	if err != nil {
		return "", fmt.Errorf("factory: podman exec %v: %w", command, err)
	}
	return string(result.Stdout), nil
}

// ReadContainerFile reads one file from a running workload container.
func (h *Harness) ReadContainerFile(ctx context.Context, container, path string) (string, error) {
	h.T.Helper()
	return h.ExecContainer(ctx, container, "cat", path)
}

// TouchContainerFile creates or updates one file in a running workload
// container. The race workloads block on such a file; releasing it is the
// deterministic workload-exit control.
func (h *Harness) TouchContainerFile(ctx context.Context, container, path string) error {
	h.T.Helper()
	_, err := h.ExecContainer(ctx, container, "touch", path)
	return err
}
