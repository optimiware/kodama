package runtimesupport

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"

	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// RunOwnerEnv overrides the owner pid recorded in cleanup labels. A helper
// process started by the suite sets it to the parent suite's pid so the
// resources it creates stay scoped to, and cleanable by, the run that spawned
// it.
const RunOwnerEnv = "KODAMA_TEST_RUN_OWNER_PID"

// RunOwnerPID is the owner-pid label value that scopes this run's resources.
func RunOwnerPID() string {
	if text := strings.TrimSpace(os.Getenv(RunOwnerEnv)); text != "" {
		if pid, err := strconv.Atoi(text); err == nil && pid > 0 {
			return strconv.Itoa(pid)
		}
	}
	return strconv.Itoa(os.Getpid())
}

// RunLabels are the labels every suite-created container and volume carries.
// They never replace a product label such as kodama.dev/workspace; they only
// add the run scope a sweep may filter on.
func RunLabels() map[string]string {
	return map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: RunOwnerPID(),
	}
}

// OwnedByRun reports whether labels carry this run's test and owner scope.
func OwnedByRun(labels map[string]string) bool {
	return labels[harness.LabelTest] == "1" &&
		labels[harness.LabelOwnerPID] == RunOwnerPID()
}

// ScopedLibpod decorates a Libpod so every container it creates carries this
// run's cleanup labels. Product identity labels are preserved; nothing else
// about the spec changes.
func ScopedLibpod(libpod runtimepodman.Libpod) runtimepodman.Libpod {
	return scopedLibpod{Libpod: libpod}
}

type scopedLibpod struct{ runtimepodman.Libpod }

func (s scopedLibpod) CreateContainer(ctx context.Context, spec runtimepodman.ContainerSpec) (runtimepodman.ContainerState, error) {
	labels := make(map[string]string, len(spec.Labels)+2)
	maps.Copy(labels, spec.Labels)
	maps.Copy(labels, RunLabels())
	spec.Labels = labels
	return s.Libpod.CreateContainer(ctx, spec)
}

// RemoveRunContainers removes only containers this run owns. A foreign
// container, even one carrying the product workspace label, is never touched.
func RemoveRunContainers(ctx context.Context) error {
	return removeContainersByFilters(ctx, runFilters())
}

// RemoveRunContainersInWorkspace narrows the run-scoped sweep to one workspace.
// It still requires this run's test and owner labels.
func RemoveRunContainersInWorkspace(ctx context.Context, workspaceID string) error {
	filters := append(runFilters(), "label="+WorkspaceLabel+"="+workspaceID)
	return removeContainersByFilters(ctx, filters)
}

// RemoveRunVolumes removes only volumes this run owns. Callers sweep containers
// first; a still-in-use volume is reported, never force-deleted together with
// the container using it.
func RemoveRunVolumes(ctx context.Context) error {
	args := append([]string{"volume", "ls"}, filterArgs(runFilters())...)
	args = append(args, "--format", "{{.Name}}")
	result, err := RunPodman(ctx, args...)
	if err != nil {
		return fmt.Errorf("runtimesupport: list run volumes: %w", err)
	}
	var errs []error
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if _, err := RunPodman(ctx, "volume", "rm", name); err != nil && !missingVolume(err) {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveContainerByName force-removes exactly one container the caller created
// and tracked. It is used for resources this test owns; no label-wide sweep is
// involved.
func RemoveContainerByName(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("runtimesupport: remove container needs a name")
	}
	if _, err := RunPodman(ctx, "rm", "--force", "--volumes", name); err != nil && !missingContainer(err) {
		return err
	}
	return nil
}

// ContainerRunning reports the observed running state of one named container.
func ContainerRunning(ctx context.Context, name string) (bool, error) {
	result, err := RunPodman(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(result.Stdout)) == "true", nil
}

func runFilters() []string {
	return []string{
		"label=" + harness.LabelTest + "=1",
		"label=" + harness.LabelOwnerPID + "=" + RunOwnerPID(),
	}
}

func filterArgs(filters []string) []string {
	args := make([]string, 0, len(filters)*2)
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	return args
}

func removeContainersByFilters(ctx context.Context, filters []string) error {
	args := append([]string{"ps", "-a"}, filterArgs(filters)...)
	args = append(args, "--format", "{{.Names}}")
	result, err := RunPodman(ctx, args...)
	if err != nil {
		return fmt.Errorf("runtimesupport: list run containers: %w", err)
	}
	var errs []error
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		// Force is safe here: the name came from an exact run-scoped filter and
		// a failed test may have left the container running.
		if _, err := RunPodman(ctx, "rm", "--force", "--volumes", name); err != nil && !missingContainer(err) {
			errs = append(errs, fmt.Errorf("remove container %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
