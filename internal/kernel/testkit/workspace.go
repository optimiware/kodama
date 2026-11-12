package testkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// Workspace is a deterministic engine.Workspace. Retention is separate from
// workload ownership: a workspace stays available across waits and restarts
// until the owner explicitly releases it.
//
// Status never recreates a missing or lost workspace. A lost workspace remains
// owned (so the caller must still clean up and fail visibly) but is never
// reported available again. Release may be guarded, which is how the fixture
// refuses release while a workload still needs the workspace.
type Workspace struct {
	mu       sync.Mutex
	records  map[id.WorkspaceID]*workspaceRecord
	guard    func(id.WorkspaceID) error
	retains  int
	releases int
}

type workspaceRecord struct {
	retained bool
	released bool
	lost     bool
}

// NewWorkspace returns an empty workspace fake.
func NewWorkspace() *Workspace {
	return &Workspace{records: make(map[id.WorkspaceID]*workspaceRecord)}
}

// SetGuard installs the release guard. A non-nil error refuses the release and
// leaves retention unchanged. The guard runs while the workspace lock is held,
// so it must not call back into the workspace.
func (w *Workspace) SetGuard(guard func(id.WorkspaceID) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.guard = guard
}

// Status reports availability without changing it.
func (w *Workspace) Status(ctx context.Context, workspaceID id.WorkspaceID) (engine.WorkspaceObservation, error) {
	if err := ctx.Err(); err != nil {
		return engine.WorkspaceObservation{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	observation := engine.WorkspaceObservation{WorkspaceID: workspaceID}
	record := w.records[workspaceID]
	switch {
	case record == nil:
		observation.Detail = "workspace was never retained"
	case record.lost:
		observation.Detail = "workspace was lost; scratch state is never recreated"
	case record.released:
		observation.Detail = "workspace was released"
	case record.retained:
		observation.Available = true
		observation.Detail = "workspace is retained"
	}
	return observation, nil
}

// Retain records ownership of a workspace. Retaining an already retained
// workspace is idempotent; a lost or released workspace is never recreated.
func (w *Workspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if workspaceID == "" {
		return fault.New(fault.InvalidInput, "retain requires a workspace identity")
	}
	record := w.records[workspaceID]
	switch {
	case record == nil:
		w.records[workspaceID] = &workspaceRecord{retained: true}
		w.retains++
		return nil
	case record.lost:
		return ErrWorkspaceLost
	case record.released:
		return fault.New(fault.InvalidInput, fmt.Sprintf("workspace %q was released and cannot be recreated", workspaceID))
	case record.retained:
		return nil
	default:
		w.records[workspaceID] = &workspaceRecord{retained: true}
		w.retains++
		return nil
	}
}

// Release gives up workspace ownership. An unknown workspace is a not-found
// fault. The guard runs before any state changes, so a refused release keeps
// the workspace retained and available.
func (w *Workspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	record := w.records[workspaceID]
	if record == nil {
		return fault.New(fault.NotFound, fmt.Sprintf("workspace %q is unknown", workspaceID))
	}
	if record.released {
		return nil
	}
	if w.guard != nil {
		if err := w.guard(workspaceID); err != nil {
			return err
		}
	}
	record.retained = false
	record.released = true
	w.releases++
	return nil
}

// Lose reports that the retained workspace disappeared. Ownership is retained
// and availability is gone; the workspace is never recreated.
func (w *Workspace) Lose(workspaceID id.WorkspaceID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	record := w.records[workspaceID]
	if record == nil {
		w.records[workspaceID] = &workspaceRecord{lost: true}
		return
	}
	record.lost = true
}

// IsRetained reports whether the workspace is currently owned. A lost
// workspace is still retained.
func (w *Workspace) IsRetained(workspaceID id.WorkspaceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	record := w.records[workspaceID]
	return record != nil && record.retained && !record.released
}

// IsReleased reports whether ownership was explicitly given up.
func (w *Workspace) IsReleased(workspaceID id.WorkspaceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	record := w.records[workspaceID]
	return record != nil && record.released
}

// RetainCount returns the number of actual retention transitions. Idempotent
// retains do not increase it.
func (w *Workspace) RetainCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.retains
}

// ReleaseCount returns the number of actual release transitions. Idempotent
// releases do not increase it.
func (w *Workspace) ReleaseCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.releases
}
