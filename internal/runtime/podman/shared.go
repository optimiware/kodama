package podman

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// A run_shared attempt (AttemptSpec.Shared, seams S1-S3) differs from an
// attempt-private one in exactly these ways, all applied by the Runtime itself
// so that no Program can omit or override them:
//
//   - the run's workspace volume, named by Shared.Workspace and verified against
//     its durable record, labels and bound instance UID, is mounted read-write
//     at /workspace (and /workspace is never an in-memory tmpfs)
//   - the container runs as the installation's non-root workload user and joins
//     the run network (and the configured attempt network, if any)
//   - only the pinned step role is applied: all capabilities dropped, then the
//     role's capabilitiesAdd; no-new-privileges exactly when none is added; a
//     role field Podman cannot enforce refuses the launch
//
// The workspace's lock is held across the launch, so a Stop of the workspace
// waits for the create and a create that follows a confirmed Stop is fenced.

// sharedMount is what a verified Shared launch adds to the container create.
type sharedMount struct {
	volume   string
	user     string
	capAdd   []string
	escalate bool
	// userns: the role puts the workload in a user namespace of its own
	// (userNamespace: pod): --userns=auto, verified on the created container.
	userns   bool
	networks []string
}

// sharedRole checks that Podman can enforce the pinned step role and returns
// the capabilities to add. A field it cannot enforce is refused (fail closed):
// the launch never silently runs with a weaker shape than the policy pinned.
func sharedRole(role execution.RoleShape) (capAdd []string, userns bool, err error) {
	refuse := func(what string) ([]string, bool, error) {
		return nil, false, fmt.Errorf("%w: the podman runtime cannot enforce the pinned role's %s", execution.ErrNotConfigured, what)
	}
	switch {
	case role.User != "nonroot":
		return refuse("user")
	case role.UserNamespace != "" && role.UserNamespace != "pod":
		return refuse("user namespace")
	// Every capability is scoped to the workload's own user namespace: without
	// one the role cannot add any.
	case role.UserNamespace == "" && len(role.CapabilitiesAdd) > 0:
		return refuse("capabilities outside a user namespace")
	case role.Seccomp != "" && role.Seccomp != "RuntimeDefault":
		return refuse("seccomp profile")
	case role.LSM != "":
		return refuse("LSM profile")
	case len(role.Devices) > 0:
		return refuse("devices")
	case role.ProcMount != "":
		return refuse("proc mount")
	}
	for _, c := range role.CapabilitiesAdd {
		if c != "SETUID" && c != "SETGID" {
			return refuse("capability " + c)
		}
	}
	return slices.Clone(role.CapabilitiesAdd), role.UserNamespace == "pod", nil
}

// holdWorkspace verifies a shared attempt's launch and takes the workspace
// lock; the caller defers the returned unlock until the container exists.
func (r *Runtime) holdWorkspace(ctx context.Context, spec execution.AttemptSpec) (*sharedMount, func(), error) {
	if err := spec.Shared.Validate(spec); err != nil {
		return nil, nil, err
	}
	if !r.cfg.RunResources {
		return nil, nil, fmt.Errorf("%w: the podman runtime has run resources disabled", execution.ErrNotConfigured)
	}
	capAdd, userns, err := sharedRole(spec.Shared.Role)
	if err != nil {
		return nil, nil, err
	}
	h := spec.Shared.Workspace
	unlock, err := r.resLocks.lock(ctx, h.Name)
	if err != nil {
		return nil, nil, err
	}
	e := engine{binary: r.cfg.Binary}
	rec, found, err := r.res.load(h.Name)
	if err == nil {
		err = verifyWorkspaceRecord(rec, found, spec.Scope, spec.RunID, h.UID)
	}
	if err == nil {
		err = verifyVolume(ctx, e, rec)
	}
	netName := execution.ResourceName(spec.Scope, spec.RunID, execution.KindNetwork, execution.KeyNetwork)
	if err == nil {
		err = requireRunNetwork(ctx, e, r.res, spec.Scope, spec.RunID, netName)
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	m := &sharedMount{
		volume: h.Name, user: strconv.Itoa(r.cfg.WorkloadUID) + ":" + strconv.Itoa(r.cfg.WorkloadGID),
		capAdd: capAdd, escalate: len(capAdd) > 0, userns: userns, networks: []string{netName},
	}
	if r.cfg.Network != "none" {
		m.networks = append(m.networks, r.cfg.Network)
	}
	return m, unlock, nil
}

// verifyWorkspaceRecord checks that a record is the run's workspace, bound to
// uid when one is given, live and created. A mismatch is a collision, a stopped
// workspace is fenced and a missing one is a conflict (execution orders the
// workspace before everything that mounts it).
func verifyWorkspaceRecord(rec resRecord, found bool, scope domain.Scope, run domain.RunID, uid string) error {
	switch {
	case !found:
		return fmt.Errorf("%w: the run has no workspace", execution.ErrConflict)
	case rec.Kind != execution.KindWorkspace || rec.Scope != scope || rec.RunID != run || (uid != "" && rec.UID != uid):
		return execution.ErrResourceCollision
	case rec.fenced():
		return execution.ErrResourceFenced
	case !rec.Created:
		return fmt.Errorf("%w: the run workspace was never created", execution.ErrConflict)
	}
	return nil
}

// verifyVolume checks the record's volume is there and is the one the record
// pinned: labels, instance UID and creation time.
func verifyVolume(ctx context.Context, e engine, rec resRecord) error {
	vol, found, err := e.volume(ctx, rec.Name)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: the run workspace volume is gone", execution.ErrConflict)
	}
	return rec.verified(vol.Labels, vol.CreatedAt)
}

// requireRunNetwork verifies that the run's network is ours, created and not
// stopping: a workload never attaches to a foreign or fenced network.
func requireRunNetwork(ctx context.Context, e engine, st *resStore, scope domain.Scope, run domain.RunID, name string) error {
	rec, found, err := st.load(name)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: the run has no network", execution.ErrConflict)
	case rec.Kind != execution.KindNetwork || rec.Scope != scope || rec.RunID != run:
		return execution.ErrResourceCollision
	case rec.fenced():
		return execution.ErrResourceFenced
	case !rec.Created:
		return fmt.Errorf("%w: the run network was never created", execution.ErrConflict)
	}
	nw, ok, err := e.network(ctx, name)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%w: the run network is gone", execution.ErrConflict)
	}
	return rec.verified(nw.Labels, nw.ID)
}
