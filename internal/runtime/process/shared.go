package process

import (
	"fmt"

	"github.com/optimiware/kodama/internal/execution"
)

// holdWorkspace verifies the run workspace a run_shared attempt names and takes
// its lock: the workspace is the run's, bound to the committed instance UID, not
// stopped or removed and present on disk. It returns the directory the attempt
// sees as /workspace and the unlock the caller defers; a mismatching UID is a
// collision and a stopped workspace is fenced, so an attempt never runs against
// another run's or a fenced volume.
func (r *Runtime) holdWorkspace(spec execution.AttemptSpec) (dir string, unlock func(), err error) {
	if err := spec.Shared.Validate(spec); err != nil {
		return "", nil, err
	}
	if r.res == "" {
		return "", nil, fmt.Errorf("%w: the process runtime has no run resources directory", execution.ErrNotConfigured)
	}
	h := spec.Shared.Workspace
	unlock = r.res.lockName(h.Name)
	fail := func(err error) (string, func(), error) {
		unlock()
		return "", nil, err
	}
	rec, found, err := r.res.load(h.Name)
	switch {
	case err != nil:
		return fail(err)
	case !found:
		return fail(fmt.Errorf("%w: the run has no workspace", execution.ErrConflict))
	case !rec.owns(h) || rec.UID != h.UID:
		return fail(execution.ErrResourceCollision)
	case rec.fenced():
		return fail(execution.ErrResourceFenced)
	case !rec.Created:
		return fail(fmt.Errorf("%w: the run workspace was never created", execution.ErrConflict))
	}
	// The recorded instance, not whatever is at the path: a replacement or lost
	// scratch never runs an attempt.
	state, err := r.res.verifyWorkspace(rec)
	switch {
	case err != nil:
		return fail(err)
	case state == wsMissing:
		return fail(fmt.Errorf("%w: the run workspace is lost", execution.ErrConflict))
	}
	dir = r.res.workspace(h.Name)
	return dir, unlock, nil
}

// withdrawHolder removes the marker of an attempt whose launch failed before
// any process existed.
func (r *Runtime) withdrawHolder(w *workload) {
	r.res.dropHolder(w.spec.Shared.Workspace.Name, w.handle.Name)
}
