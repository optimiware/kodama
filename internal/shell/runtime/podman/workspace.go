// Retained workspace behavior for the Podman runtime adapter.
//
// The workspace port is separate from the workload port because workspace
// ownership differs from workload ownership. The Shell provisions the named
// volume before admission; this adapter never creates it. Availability is only
// ever reported for the deterministic volume that carries this workspace's
// ownership label, and a missing or foreign volume is evidence, never
// permission to reconstruct scratch state.
//
// Retention is durable and idempotent: Retain writes a marker beside the
// per-workload roots so ownership survives an adapter reconstruction. The
// Kernel retains at activation and calls Release only after every owned
// identity is confirmed stopped and removed, so Release is the single explicit
// way a workspace is finally dropped: it removes the volume only when it is
// owned and unreferenced by any container or start-attempt marker, then clears
// the durable marker. An absent volume is idempotent success, while an
// uncertain removal returns an error instead of fabricating success and leaves
// both the marker and the volume observable for a retry.
package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

const (
	// retentionDirectory holds one durable retention marker per workspace. It
	// is a sibling of the per-workload roots, so removing a workload's
	// materialization never drops workspace retention.
	retentionDirectory = ".workspaces"
	// retentionSuffix is the marker suffix for one workspace identity.
	retentionSuffix = ".json"
)

// retentionRecord is the durable evidence that a workspace is retained. It
// carries no secret material: a workspace identity and a timestamp are enough.
type retentionRecord struct {
	WorkspaceID id.WorkspaceID `json:"workspace_id"`
	RetainedAt  string         `json:"retained_at"`
}

// retentionDir returns the durable retention marker directory.
func retentionDir(root string) string {
	return filepath.Join(root, retentionDirectory)
}

// retentionPath returns the durable retention marker path for one workspace.
func retentionPath(root string, workspace id.WorkspaceID) string {
	return filepath.Join(retentionDir(root), string(workspace)+retentionSuffix)
}

// ownsWorkspaceVolume reports whether the inspected volume is the one this
// adapter/installation owns for the workspace. A deterministic name alone is
// not enough: the workspace ownership label must name exactly this workspace.
func ownsWorkspaceVolume(volume Volume, volumeName string, workspace id.WorkspaceID) bool {
	if volume.Name != "" && volume.Name != volumeName {
		return false
	}
	return volume.Labels[LabelWorkspace] == string(workspace)
}

// isWorkspaceRetained reports whether a durable retention marker exists. An
// unreadable marker is an error, never absence; a symlink or non-regular file
// is refused rather than followed.
func (a *Adapter) isWorkspaceRetained(ctx context.Context, workspace id.WorkspaceID) (bool, error) {
	info, err := a.files.Lstat(ctx, retentionPath(a.root, workspace))
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fault.New(fault.Forbidden, "workspace retention marker is not a regular file")
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fault.Wrap(fault.DependencyUnavailable, "workspace retention state is unreachable", err)
}

// writeWorkspaceRetention durably records workspace ownership before any
// release can observe it. The marker is written through the same atomic
// replacement used for credentials, so a partial write never looks retained.
func (a *Adapter) writeWorkspaceRetention(ctx context.Context, workspace id.WorkspaceID) error {
	if err := a.files.MkdirAll(ctx, retentionDir(a.root), directoryMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace retention directory write outcome is unknown", err)
	}
	payload, err := json.Marshal(retentionRecord{
		WorkspaceID: workspace,
		RetainedAt:  a.clock.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "workspace retention marker is not encodable", err)
	}
	return a.replaceFile(ctx, retentionPath(a.root, workspace), payload)
}

// removeWorkspaceRetention deletes only this workspace's durable marker. An
// absent marker is already clean.
func (a *Adapter) removeWorkspaceRetention(ctx context.Context, workspace id.WorkspaceID) error {
	path := retentionPath(a.root, workspace)
	info, err := a.files.Lstat(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "workspace retention state is unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fault.New(fault.Forbidden, "workspace retention marker is not a regular file")
	}
	if err := a.files.RemoveAll(ctx, path); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace retention marker removal outcome is unknown", err)
	}
	return nil
}

// Status reports availability without changing it. Only an existing volume
// that carries this workspace's ownership label is available; a missing or
// foreign volume is unavailable evidence and is never recreated.
func (a *Adapter) Status(ctx context.Context, workspace id.WorkspaceID) (engine.WorkspaceObservation, error) {
	observation := engine.WorkspaceObservation{WorkspaceID: workspace}
	if err := validateWorkspaceID(workspace); err != nil {
		observation.Detail = "workspace status request workspace is invalid"
		return observation, err
	}
	if err := ctx.Err(); err != nil {
		observation.Detail = "workspace status cancelled before any effect"
		return observation, fault.Wrap(fault.DependencyUnavailable, "workspace status cancelled before any effect", err)
	}

	lock := a.lockForWorkspace(workspace)
	lock.Lock()
	defer lock.Unlock()

	volumeName, err := WorkspaceVolumeName(workspace)
	if err != nil {
		observation.Detail = "workspace volume name is invalid"
		return observation, err
	}
	volume, err := a.libpod.InspectVolume(ctx, volumeName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			observation.Detail = fmt.Sprintf(
				"retained workspace volume %q is missing; a missing workspace is never recreated", volumeName)
			return observation, nil
		}
		observation.Detail = fmt.Sprintf("workspace volume %q state is unreachable", volumeName)
		return observation, fault.Wrap(fault.DependencyUnavailable, "workspace volume state is unreachable", err)
	}
	if !ownsWorkspaceVolume(volume, volumeName, workspace) {
		observation.Detail = fmt.Sprintf(
			"volume %q is not owned by workspace %q; a foreign or unlabelled volume is never treated as the workspace",
			volumeName, workspace)
		return observation, nil
	}
	observation.Available = true
	observation.Detail = fmt.Sprintf(
		"retained workspace volume %q is present and owned by workspace %q", volumeName, workspace)
	return observation, nil
}

// Retain durably records workspace ownership. It is idempotent and never
// creates a missing volume: a missing or foreign volume is a failure.
func (a *Adapter) Retain(ctx context.Context, workspace id.WorkspaceID) error {
	if err := validateWorkspaceID(workspace); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace retention cancelled before any effect", err)
	}

	lock := a.lockForWorkspace(workspace)
	lock.Lock()
	defer lock.Unlock()

	volumeName, err := WorkspaceVolumeName(workspace)
	if err != nil {
		return err
	}
	volume, err := a.libpod.InspectVolume(ctx, volumeName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fault.New(fault.NotFound,
				"retained workspace volume is missing; retention never recreates it")
		}
		return fault.Wrap(fault.DependencyUnavailable, "workspace volume state is unreachable", err)
	}
	if !ownsWorkspaceVolume(volume, volumeName, workspace) {
		return fault.New(fault.Forbidden, "workspace volume is not owned by this workspace")
	}

	retained, err := a.isWorkspaceRetained(ctx, workspace)
	if err != nil {
		return err
	}
	if retained {
		return nil
	}
	return a.writeWorkspaceRetention(ctx, workspace)
}

// Release gives up workspace ownership and removes the volume only when it is
// owned and unreferenced. Retention is not a refusal: the Kernel retains at
// activation and releases only after confirmed cleanup, so a retained marker is
// the normal pre-release state. A successful or already-absent removal clears
// the marker; an uncertain or partial removal is surfaced as an error and
// leaves the marker and volume observable so a retry completes.
func (a *Adapter) Release(ctx context.Context, workspace id.WorkspaceID) error {
	if err := validateWorkspaceID(workspace); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace release cancelled before any effect", err)
	}

	lock := a.lockForWorkspace(workspace)
	lock.Lock()
	defer lock.Unlock()

	volumeName, err := WorkspaceVolumeName(workspace)
	if err != nil {
		return err
	}
	// Validate the durable retention marker before any destructive effect: a
	// corrupt or non-regular marker is refused before the volume is gone. The
	// marker's presence is not a refusal; only its unreadability is.
	if _, err := a.isWorkspaceRetained(ctx, workspace); err != nil {
		return err
	}
	volume, inspectErr := a.libpod.InspectVolume(ctx, volumeName)
	switch {
	case inspectErr == nil:
		if !ownsWorkspaceVolume(volume, volumeName, workspace) {
			return fault.New(fault.Forbidden,
				"workspace volume is not owned by this workspace; a foreign volume is never released")
		}
		if err := a.refuseReleaseWhileReferenced(ctx, workspace); err != nil {
			return err
		}
		if err := a.libpod.RemoveVolume(ctx, volumeName); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return fault.Wrap(fault.UncertainExternalEffect,
					"workspace volume removal outcome is unknown", err)
			}
		}
	case errors.Is(inspectErr, ErrNotFound):
		// Already absent: idempotent release still clears the retention marker.
	default:
		return fault.Wrap(fault.DependencyUnavailable, "workspace volume state is unreachable", inspectErr)
	}
	return a.removeWorkspaceRetention(ctx, workspace)
}

// refuseReleaseWhileReferenced refuses release while any container still names
// the workspace or any durable start attempt may still have delivered a start
// for it. Both are affirmative evidence that the volume may still be in use.
func (a *Adapter) refuseReleaseWhileReferenced(ctx context.Context, workspace id.WorkspaceID) error {
	containers, err := a.libpod.ListContainers(ctx, map[string]string{LabelWorkspace: string(workspace)})
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace container references are unreachable", err)
	}
	if len(containers) > 0 {
		names := make([]string, 0, len(containers))
		for _, container := range containers {
			names = append(names, container.Name)
		}
		sort.Strings(names)
		return fault.New(fault.Forbidden, fmt.Sprintf(
			"workspace is still referenced by container(s) %s", strings.Join(names, ", ")))
	}
	workload, err := a.workspaceStartAttempt(ctx, workspace)
	if err != nil {
		return err
	}
	if workload != "" {
		return fault.New(fault.UncertainExternalEffect, fmt.Sprintf(
			"workspace may still be referenced by an uncertain start attempt for workload %q", workload))
	}
	return nil
}

// workspaceStartAttempt reports a workload whose durable start attempt may
// have delivered a start against this workspace. A marker without a workspace
// attribution is uncertain evidence, never permission to release.
func (a *Adapter) workspaceStartAttempt(ctx context.Context, workspace id.WorkspaceID) (id.WorkloadID, error) {
	entries, err := a.files.ReadDir(ctx, a.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "workspace reference scan is unreachable", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		workload := id.WorkloadID(entry.Name())
		if _, err := id.ParseWorkloadID(string(workload)); err != nil {
			// Retention markers and removal fences are not workload roots.
			continue
		}
		attempted, err := a.hasStartAttempt(ctx, workload)
		if err != nil {
			return "", err
		}
		if !attempted {
			continue
		}
		record, _, found, err := a.readPreparation(ctx, workload)
		if err != nil {
			return "", err
		}
		if found {
			if record.WorkspaceID == workspace {
				return workload, nil
			}
			continue
		}
		markerWorkspace, err := a.readStartAttemptWorkspace(ctx, workload)
		if err != nil {
			return "", err
		}
		if markerWorkspace == "" {
			return "", fault.New(fault.UncertainExternalEffect, fmt.Sprintf(
				"start attempt for workload %q has no workspace attribution", workload))
		}
		if markerWorkspace == workspace {
			return workload, nil
		}
	}
	return "", nil
}

// readStartAttemptWorkspace decodes the workspace attribution from one durable
// start-attempt marker. It is the fallback when the preparation marker is gone.
func (a *Adapter) readStartAttemptWorkspace(ctx context.Context, workload id.WorkloadID) (id.WorkspaceID, error) {
	path := startAttemptPath(a.root, workload)
	info, err := a.files.Lstat(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "start attempt state is unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fault.New(fault.Forbidden, "start attempt marker is not a regular file")
	}
	data, err := a.files.ReadFile(ctx, path)
	if err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "start attempt state is unreadable", err)
	}
	var record startAttemptRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "start attempt marker is not decodable", err)
	}
	return record.WorkspaceID, nil
}

// Adapter implements the declared engine.Workspace port in addition to the
// runtime workload port.
var _ engine.Workspace = (*Adapter)(nil)
