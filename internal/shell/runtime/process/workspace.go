// Retained-workspace operations for the Process runtime adapter.
//
// The workspace port is separate from the workload port because workspace
// ownership differs from workload ownership: Remove never releases or deletes
// the execution's retained workspace. Retain records durable ownership outside
// both the adapter resource root and the workload-writable workspace, Status
// reports availability without ever recreating scratch state, and Release
// deletes the workspace only after the last live workload identity referencing
// it is gone.
//
// Every operation is serialized by the same adapter lock as Prepare, Start and
// Remove, so a release can never slip between a preparation and its start. The
// retention marker is written atomically (temp file plus rename) and survives an
// adapter reconstruction. Release is the Kernel's single final drop after every
// owned identity is confirmed stopped and removed: it neither requires nor is
// blocked by the marker, because a lost marker must not leak the workspace. A
// failed or partial removal stays visible as an error and leaves the workspace
// and marker in place for a retry.
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

const (
	// retentionDirectoryName holds durable retention markers. It lives under
	// the adapter resource root, outside the workload-writable workspace.
	retentionDirectoryName = "retention"
	// workloadsDirectoryName is the parent of per-identity resource roots.
	workloadsDirectoryName = "workloads"
	// retentionMarkerData is the marker payload. It carries no secret material.
	retentionMarkerData = "retained\n"
	// retentionTempSuffix is the atomic-write staging name.
	retentionTempSuffix = ".tmp"
)

// retentionPath returns the durable retention marker for one workspace.
func (a *Adapter) retentionPath(workspace id.WorkspaceID) string {
	return filepath.Join(a.root, retentionDirectoryName, string(workspace))
}

// workspaceDirFor returns the confined workspace directory for one identity.
// The identity is already validated; the containment check is a second fence.
func (a *Adapter) workspaceDirFor(workspace id.WorkspaceID) (string, error) {
	dir, err := WorkspaceDir(a.workspaceRoot, workspace)
	if err != nil {
		return "", err
	}
	if !pathWithin(a.workspaceRoot, dir) || dir == a.workspaceRoot {
		return "", fault.New(fault.Forbidden, "workspace path is outside the workspace root")
	}
	return dir, nil
}

// Status reports availability evidence for one retained workspace. A missing,
// symlinked or non-directory path is unavailable with an attributable detail;
// it is never recreated, followed or repaired.
func (a *Adapter) Status(ctx context.Context, workspaceID id.WorkspaceID) (engine.WorkspaceObservation, error) {
	observation := engine.WorkspaceObservation{WorkspaceID: workspaceID}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return observation, err
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	dir, err := a.workspaceDirFor(workspaceID)
	if err != nil {
		return observation, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	info, err := a.files.Stat(ctx, dir)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		observation.Detail = fmt.Sprintf("workspace %q is a symlink; it is never followed or recreated", workspaceID)
	case err == nil && !info.IsDir():
		observation.Detail = fmt.Sprintf("workspace %q is not a directory; it is never recreated", workspaceID)
	case err == nil:
		observation.Available = true
		observation.Detail = fmt.Sprintf("workspace %q is available", workspaceID)
	case errors.Is(err, os.ErrNotExist):
		observation.Detail = fmt.Sprintf("workspace %q is missing; it is never recreated", workspaceID)
	default:
		return observation, fault.Wrap(fault.DependencyUnavailable, "workspace status is unreachable", err)
	}
	return observation, nil
}

// Retain records durable ownership of an existing workspace. Retaining an
// already retained workspace is idempotent. A missing or symlinked workspace is
// refused and never created.
func (a *Adapter) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.workspaceDirFor(workspaceID)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.requireWorkspace(ctx, dir); err != nil {
		return err
	}
	marker := a.retentionPath(workspaceID)
	if info, err := a.files.Stat(ctx, marker); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fault.New(fault.Forbidden, "retention marker is not a regular file; retain refused")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "retention marker is unreachable", err)
	}
	if err := a.ensureConfinedDir(ctx, filepath.Dir(marker)); err != nil {
		return err
	}
	temp := marker + retentionTempSuffix
	if err := a.files.WriteFile(ctx, temp, []byte(retentionMarkerData), bookkeepingMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "retention marker write failed", err)
	}
	if err := a.files.Rename(ctx, temp, marker); err != nil {
		// The final marker is only valid once the rename succeeds. Best-effort
		// staging cleanup never fabricates a retained workspace.
		_ = a.files.RemoveAll(ctx, temp)
		return fault.Wrap(fault.DependencyUnavailable, "retention marker rename failed", err)
	}
	return nil
}

// Release gives up retention and removes one workspace. It refuses while any
// live workload identity still references the workspace, but it neither
// requires nor is blocked by the retention marker: the Kernel calls Release as
// the single final drop after every owned identity is confirmed stopped and
// removed, so a missing marker must not leak the workspace. An already-absent
// workspace is a clean release. A failed or partial removal is returned as an
// error with the workspace and marker left observable for a retry.
func (a *Adapter) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.workspaceDirFor(workspaceID)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// A live workload identity keeps both the workspace and its retention.
	held, holder, err := a.workspaceReferenced(ctx, workspaceID)
	if err != nil {
		return err
	}
	if held {
		return fault.New(fault.Forbidden,
			fmt.Sprintf("workspace %q is retained by live workload identity %q; release refused", workspaceID, holder))
	}

	marker := a.retentionPath(workspaceID)

	info, err := a.files.Stat(ctx, dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// An already-absent workspace is a clean release; only a stale marker
		// remains to be cleared.
		return a.clearRetention(ctx, marker)
	case err != nil:
		return fault.Wrap(fault.DependencyUnavailable, "workspace is unreachable", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return fault.New(fault.Forbidden, "workspace path is not a real directory; release refused")
	}

	// Remove the workspace before the marker so a crash or failure leaves the
	// ownership evidence visible instead of silently orphaning the directory.
	if err := a.files.RemoveAll(ctx, dir); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "workspace removal failed", err)
	}
	if _, err := a.files.Stat(ctx, dir); err == nil {
		return fault.New(fault.DependencyUnavailable, "workspace removal did not complete")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "workspace removal outcome is unknown", err)
	}
	return a.clearRetention(ctx, marker)
}

// clearRetention removes one durable retention marker when that can be done
// without following a symlink out of the adapter root. Release never depends on
// the marker, so an unconfined or unreachable retention directory is left
// untouched rather than blocking the workspace drop; the workspace itself is
// always verified gone before success.
func (a *Adapter) clearRetention(ctx context.Context, marker string) error {
	dir := filepath.Dir(marker)
	if !pathWithin(a.root, dir) || dir == a.root {
		return nil
	}
	info, err := a.files.Stat(ctx, dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return nil
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return nil
	}
	if err := a.files.RemoveAll(ctx, marker); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "retention marker removal failed", err)
	}
	if _, err := a.files.Stat(ctx, marker); err == nil {
		return fault.New(fault.DependencyUnavailable, "retention marker removal did not complete")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "retention marker removal outcome is unknown", err)
	}
	return nil
}

// workspaceReferenced reports whether any live workload identity still
// references the workspace. Only adapter-owned workload roots are scanned and a
// symlinked root or entry is refused rather than traversed.
func (a *Adapter) workspaceReferenced(ctx context.Context, workspaceID id.WorkspaceID) (bool, id.WorkloadID, error) {
	workloadsRoot := filepath.Join(a.root, workloadsDirectoryName)
	info, err := a.files.Stat(ctx, workloadsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", fault.Wrap(fault.DependencyUnavailable, "workload roots are unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, "", fault.New(fault.Forbidden, "workload roots are not a confined directory")
	}
	entries, err := a.files.ReadDir(ctx, workloadsRoot)
	if err != nil {
		return false, "", fault.Wrap(fault.DependencyUnavailable, "workload roots are unreadable", err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return false, "", fault.New(fault.Forbidden, "workload root is a symlink; release refused")
		}
		if !entry.IsDir() {
			continue
		}
		workload, parseErr := id.ParseWorkloadID(entry.Name())
		if parseErr != nil {
			// A directory this adapter did not create is not a workload root.
			continue
		}
		tombstoned, tombstoneErr := a.tombstoned(ctx, workload)
		if tombstoneErr != nil {
			return false, "", tombstoneErr
		}
		if tombstoned {
			// A completed or pending removal has no live workload identity.
			continue
		}
		resourceRoot, rootErr := ResourceRoot(a.root, workload)
		if rootErr != nil {
			continue
		}
		loaded, found, manifestErr := a.readManifest(ctx, resourceRoot)
		if manifestErr != nil {
			return false, "", manifestErr
		}
		if found && loaded.WorkspaceID == workspaceID {
			return true, workload, nil
		}
	}
	return false, "", nil
}
