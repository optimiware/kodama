package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// KernelResources implements the kernel's ResourceManager port with Podman
// objects: a named volume per run workspace (prepared by a one-shot container
// when the run has sources), a container per run-scoped service on an internal
// run network, reachable by the service's name.
//
// Ensure is idempotent on ResourceSpec.Identity under the same adoption and
// fence rules as KernelRuntime.Start. A workspace is stopped (fenced) at once
// and released only when no container still runs on it; it is removed only when
// no container references it. Removal is confirmed (ResourceRemoved) only when
// the engine says the object is gone; ResourceUnknown never counts.
//
// Refused before any effect: project- and tenant-lifetime services (the
// backend does not advertise persistent_services), services with secrets,
// unpinned images, and a workspace with sources when no Preparer is configured.
type KernelResources struct{ core *kernelCore }

var _ kr.ResourceManager = (*KernelResources)(nil)

// NewKernelResources opens the resource manager over cfg.StateDir.
func NewKernelResources(cfg KernelConfig) (*KernelResources, error) {
	c, err := newKernelCore(cfg)
	if err != nil {
		return nil, err
	}
	return &KernelResources{core: c}, nil
}

// Capabilities implements kr.ResourceManager.
func (r *KernelResources) Capabilities(context.Context) (kr.CapabilitySet, error) {
	return append(kr.CapabilitySet(nil), KernelCapabilities...), nil
}

func resourceClass(k kr.ResourceKind) string {
	if k == kr.ResourceWorkspace {
		return classWorkspace
	}
	return classService
}

func resourceName(k kr.ResourceKind, scope domain.Scope, identity string) string {
	if k == kr.ResourceWorkspace {
		return objectName("kv-", scope, identity)
	}
	return objectName("ks-", scope, identity)
}

func prepName(scope domain.Scope, identity string) string {
	return objectName("kp-", scope, identity)
}

func handleRec(h kr.ResourceHandle) krec {
	return krec{
		Name: resourceName(h.Kind, h.Scope, h.Identity), Class: resourceClass(h.Kind), Scope: h.Scope, RunID: h.RunID,
		Identity: h.Identity, Fence: h.Fence,
	}
}

func handleOf(spec kr.ResourceSpec) kr.ResourceHandle {
	return kr.ResourceHandle{Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Name: spec.Name, Identity: spec.Identity, Fence: spec.Fence}
}

// verifiedWorkspace checks that h is the ready, unfenced workspace of the run,
// that its volume is the instance the record pinned, and returns the volume name.
func (c *kernelCore) verifiedWorkspace(ctx context.Context, h kr.ResourceHandle, scope domain.Scope, run domain.RunID) (string, error) {
	if h.Kind != kr.ResourceWorkspace || h.Scope != scope || h.RunID != run {
		return "", refuse("workspace %s does not belong to this run", h.Name)
	}
	name := resourceName(kr.ResourceWorkspace, scope, h.Identity)
	rec, found, err := c.load(name)
	switch {
	case err != nil:
		return "", err
	case !found || rec.Class != classWorkspace || rec.Scope != scope || rec.RunID != run || rec.Identity != h.Identity:
		return "", refuse("the run has no workspace %s", h.Name)
	case rec.fenced():
		return "", fmt.Errorf("%w: the workspace %s is stopped", kr.ErrStaleFence, h.Name)
	case rec.ObjectID == "" || (rec.PrepRequired != "" && rec.Prepared != rec.PrepRequired):
		return "", refuse("the workspace %s is not ready", h.Name)
	}
	vol, ok, err := c.pm.inspectVolume(ctx, name)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", refuse("the workspace volume of %s is gone", h.Name)
	case !owned(vol.Labels, rec) || vol.CreatedAt != rec.ObjectID:
		return "", refuse("the workspace volume of %s is not its recorded instance", h.Name)
	}
	return name, nil
}

// verifiedService checks that h is a created, unfenced service of the run.
func (c *kernelCore) verifiedService(h kr.ResourceHandle, scope domain.Scope, run domain.RunID) (krec, error) {
	rec, found, err := c.load(resourceName(kr.ResourceService, scope, h.Identity))
	switch {
	case err != nil:
		return krec{}, err
	case !found || rec.Class != classService || rec.Scope != scope || rec.RunID != run || rec.Identity != h.Identity:
		return krec{}, refuse("the run has no service %s", h.Name)
	case rec.fenced():
		return krec{}, fmt.Errorf("%w: the service %s is stopped", kr.ErrStaleFence, h.Name)
	case rec.ObjectID == "" || rec.Network == "":
		return krec{}, refuse("the service %s was not created", h.Name)
	}
	return rec, nil
}

// Ensure implements kr.ResourceManager.
func (r *KernelResources) Ensure(ctx context.Context, spec kr.ResourceSpec) (kr.ResourceObservation, error) {
	if err := spec.Validate(); err != nil {
		return kr.ResourceObservation{}, notReached(err)
	}
	if err := KernelCapabilities.Satisfies(spec.Requires); err != nil {
		return kr.ResourceObservation{}, notReached(err)
	}
	if spec.Lifetime != kr.LifetimePipeline {
		if err := KernelCapabilities.Satisfies(kr.CapabilitySet{kr.CapPersistentService}); err != nil {
			return kr.ResourceObservation{}, notReached(err)
		}
	}
	if spec.Kind == kr.ResourceWorkspace {
		return r.ensureWorkspace(ctx, spec)
	}
	return r.ensureService(ctx, spec)
}

func (r *KernelResources) ensureWorkspace(ctx context.Context, spec kr.ResourceSpec) (kr.ResourceObservation, error) {
	c := r.core
	want := handleRec(handleOf(spec))
	want.SpecDigest = spec.SpecDigest
	if len(spec.Preparation.Sources) > 0 {
		if c.cfg.Preparer == nil {
			return kr.ResourceObservation{}, notReached(fmt.Errorf("%w: preparing sources needs a preparer, which is not configured", kr.ErrCapability))
		}
		want.PrepRequired = spec.Preparation.Digest
	}
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return kr.ResourceObservation{}, notReached(err)
	}
	defer unlock()
	rec, err := c.admit(want)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if err := c.ensureObject(ctx, c.volumeObject(), &rec); err != nil {
		return kr.ResourceObservation{}, err
	}
	if rec.PrepRequired != "" && rec.Prepared != rec.PrepRequired && !rec.PrepFailed {
		if err := r.startPreparation(ctx, &rec, spec); err != nil {
			return kr.ResourceObservation{}, err
		}
	}
	return r.observeLocked(ctx, handleOf(spec), &rec)
}

// startPreparation creates (once) and starts the one-shot preparation container.
// It has its own record, so an adopted run does not prepare twice.
func (r *KernelResources) startPreparation(ctx context.Context, ws *krec, spec kr.ResourceSpec) error {
	c := r.core
	want := krec{
		Name: prepName(ws.Scope, ws.Identity), Class: classPreparation, Scope: ws.Scope, RunID: ws.RunID,
		Identity: ws.Identity, SpecDigest: ws.PrepRequired, Fence: ws.Fence,
	}
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return err
	}
	defer unlock()
	prec, err := c.admit(want)
	if err != nil {
		return err
	}
	ct, err := c.ensureContainer(ctx, &prec, func() (ctrSpec, error) {
		job, err := c.cfg.Preparer.Preparation(ctx, *spec.Preparation)
		if err != nil {
			return ctrSpec{}, notReached(err)
		}
		if !pinned(job.Image) || len(job.Command) == 0 {
			return ctrSpec{}, refuse("the preparation needs a digest-pinned image and a command")
		}
		s := ctrSpec{
			image: job.Image, entrypoint: job.Command, env: []string{"HOME=/tmp", "TMPDIR=/tmp"}, tmpfs: []string{"/tmp"},
			volumes: []string{ws.Name + ":/workspace:U"}, networks: []string{c.cfg.Network},
		}
		if job.Network != "" {
			s.networks = []string{job.Network}
		}
		if job.SecretsDir != "" {
			s.volumes = append(s.volumes, job.SecretsDir+":/kodama/secrets:ro")
		}
		for _, k := range sortedKeys(job.Env) {
			s.env = append(s.env, k+"="+job.Env[k])
		}
		return s, nil
	})
	if err != nil {
		return err
	}
	_, err = c.startContainer(ctx, prec, ct)
	return err
}

// advancePreparation records the outcome of the preparation container: exit 0
// applies the inputs, any other exit or a lost container fails the workspace.
// It reports whether the preparation is still pending.
func (r *KernelResources) advancePreparation(ctx context.Context, ws *krec) (pending bool, err error) {
	c := r.core
	if ws.PrepRequired == "" || ws.Prepared == ws.PrepRequired || ws.PrepFailed {
		return false, nil
	}
	name := prepName(ws.Scope, ws.Identity)
	unlock, err := c.locks.lock(ctx, name)
	if err != nil {
		return true, err
	}
	defer unlock()
	prec, found, err := c.load(name)
	if err != nil || !found {
		return true, err // not started yet: Ensure starts it
	}
	ct, ok, err := c.findContainer(ctx, &prec)
	switch {
	case err != nil && !errors.Is(err, errForeign):
		return true, err
	case !ok && (prec.Unanswered || prec.ObjectID == ""):
		return true, nil
	case !ok:
		ws.PrepFailed = true // created and gone before the outcome was recorded
		return false, c.save(*ws)
	case !ct.started() || ct.State.Running:
		return true, nil
	case ct.State.ExitCode != 0:
		ws.PrepFailed = true
		return false, c.save(*ws)
	}
	ws.Prepared = ws.PrepRequired
	if err := c.save(*ws); err != nil {
		return true, err
	}
	// The one-shot container has done its work; removing it frees the volume.
	if _, found, err := c.removeContainer(ctx, prec); err == nil && !found {
		prec.Stopped, prec.Stopping, prec.Removed = true, true, true
		_ = c.save(prec)
	}
	return false, nil
}

func (r *KernelResources) ensureService(ctx context.Context, spec kr.ResourceSpec) (kr.ResourceObservation, error) {
	c, svc := r.core, spec.Service
	if len(svc.Secrets) > 0 {
		return kr.ResourceObservation{}, refuse("service %s declares secrets, which the podman backend cannot deliver", svc.Name)
	}
	if !pinned(svc.Image) {
		return kr.ResourceObservation{}, refuse("service image %q is not pinned by digest", svc.Image)
	}
	for key := range svc.Env {
		if !envKey.MatchString(key) {
			return kr.ResourceObservation{}, refuse("environment name %q", key)
		}
	}
	want := handleRec(handleOf(spec))
	want.SpecDigest = spec.SpecDigest
	want.Ports = svc.Ports
	want.Network = networkName(spec.Scope, spec.RunID)
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return kr.ResourceObservation{}, notReached(err)
	}
	defer unlock()
	rec, err := c.admit(want)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	ct, err := c.ensureContainer(ctx, &rec, func() (ctrSpec, error) {
		if err := c.ensureNetwork(ctx, spec.Scope, spec.RunID, spec.Fence); err != nil {
			return ctrSpec{}, err
		}
		s := ctrSpec{
			image: svc.Image, env: []string{"HOME=/tmp", "TMPDIR=/tmp"}, tmpfs: []string{"/tmp", "/run"},
			networks: []string{rec.Network}, aliases: []string{svc.Name},
		}
		if len(svc.Commands) > 0 {
			s.entrypoint, s.args = []string{"/bin/sh", "-ec"}, []string{strings.Join(svc.Commands, "\n")}
		}
		for _, k := range sortedKeys(svc.Env) {
			s.env = append(s.env, k+"="+svc.Env[k])
		}
		return s, nil
	})
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if _, err := c.startContainer(ctx, rec, ct); err != nil {
		return kr.ResourceObservation{}, err
	}
	return r.observeLocked(ctx, handleOf(spec), &rec)
}

// observeLocked reports a resource from the engine and the record. The caller
// holds the lock of the record.
func (r *KernelResources) observeLocked(ctx context.Context, h kr.ResourceHandle, rec *krec) (kr.ResourceObservation, error) {
	obs := kr.ResourceObservation{Handle: h, ObservedAt: r.core.now()}
	var err error
	if rec.Class == classWorkspace {
		obs.State, obs.Reason, err = r.workspaceState(ctx, rec)
	} else {
		obs.State, obs.Reason, err = r.serviceState(ctx, rec)
	}
	if err == nil && obs.State == kr.ResourceRemoved {
		// A removal is not confirmed while its network cleanup is still owed.
		owed, oerr := r.core.networkOwed(*rec)
		if oerr != nil {
			return obs, oerr
		}
		if owed {
			obs.State = kr.ResourceUnknown
		}
	}
	return obs, err
}

func (r *KernelResources) workspaceState(ctx context.Context, rec *krec) (kr.ResourceState, kr.Reason, error) {
	c := r.core
	// An outstanding create whose volume is observed is adopted here: bound and
	// resolved, so nothing is left unknown once the object is there.
	found, ours, err := c.bindObject(ctx, c.volumeObject(), rec)
	switch {
	case err != nil:
		return "", "", err
	case !found && rec.Removed:
		return kr.ResourceRemoved, kr.ReasonNone, nil
	case !found && rec.Unanswered:
		return kr.ResourceUnknown, kr.ReasonNone, nil
	case !found && rec.ObjectID != "":
		return kr.ResourceFailed, kr.ReasonWorkspaceLost, nil
	case !found:
		return kr.ResourceRemoved, kr.ReasonNone, nil // nothing was ever created
	case !ours && rec.Removed:
		return kr.ResourceRemoved, kr.ReasonNone, nil
	case !ours && rec.Unanswered:
		// A foreign volume at the name proves nothing about the outstanding create.
		return kr.ResourceUnknown, kr.ReasonNone, nil
	case !ours:
		return kr.ResourceFailed, kr.ReasonWorkspaceLost, nil // a replacement under our name
	}
	pending, err := r.advancePreparation(ctx, rec)
	switch {
	case err != nil:
		return "", "", err
	case rec.Stopped:
		return kr.ResourceStopped, kr.ReasonNone, nil
	case rec.PrepFailed:
		return kr.ResourceFailed, kr.ReasonPreparationFailed, nil
	case pending:
		return kr.ResourceStarting, kr.ReasonNone, nil
	case rec.Stopping:
		// Fenced but not yet released: a holder may still exist. One whose create
		// is unresolved cannot be seen from the engine.
		if un, err := c.unresolvedHolders(rec.Name); err != nil {
			return "", "", err
		} else if len(un) > 0 {
			return kr.ResourceUnknown, kr.ReasonNone, nil
		}
	}
	return kr.ResourceReady, kr.ReasonNone, nil
}

func (r *KernelResources) serviceState(ctx context.Context, rec *krec) (kr.ResourceState, kr.Reason, error) {
	c := r.core
	ct, found, err := c.findContainer(ctx, rec)
	if err != nil && !errors.Is(err, errForeign) {
		return "", "", err
	}
	switch {
	case !found && rec.Removed:
		return kr.ResourceRemoved, kr.ReasonNone, nil
	case !found && rec.Unanswered:
		return kr.ResourceUnknown, kr.ReasonNone, nil
	case !found && rec.ObjectID != "":
		return kr.ResourceFailed, kr.ReasonServiceLost, nil
	case !found:
		return kr.ResourceRemoved, kr.ReasonNone, nil // nothing was ever created
	case !ct.started():
		return kr.ResourceStarting, kr.ReasonNone, nil
	case ct.State.Running:
		if rec.Stopping {
			return kr.ResourceStarting, kr.ReasonNone, nil // a stop is under way
		}
		ready, err := r.listening(ctx, rec)
		if err != nil {
			return "", "", err
		}
		if ready {
			return kr.ResourceReady, kr.ReasonNone, nil
		}
		return kr.ResourceStarting, kr.ReasonNone, nil
	case rec.Stopped:
		return kr.ResourceStopped, kr.ReasonNone, nil
	}
	return kr.ResourceFailed, kr.ReasonServiceLost, nil
}

// listening reports whether every declared port of a running service accepts
// connections. It reads the container's own socket table, so it needs no tool
// in the image beyond cat; a service without ports is ready once it runs. A
// container that cannot answer is not ready (never assumed ready).
func (r *KernelResources) listening(ctx context.Context, rec *krec) (bool, error) {
	if len(rec.Ports) == 0 {
		return true, nil
	}
	res, err := r.core.pm.run(ctx, "exec", rec.ObjectID, "cat", "/proc/net/tcp", "/proc/net/tcp6")
	if err != nil {
		if errors.Is(err, kr.ErrUncertain) {
			return false, err
		}
		return false, nil
	}
	if res.code != 0 {
		return false, nil
	}
	open := map[int]bool{}
	for _, line := range strings.Split(res.stdout, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" { // 0A is TCP_LISTEN
			continue
		}
		if _, port, ok := strings.Cut(f[1], ":"); ok {
			if n, err := strconv.ParseUint(port, 16, 32); err == nil {
				open[int(n)] = true
			}
		}
	}
	for _, p := range rec.Ports {
		if !open[p] {
			return false, nil
		}
	}
	return true, nil
}

// bound loads the record a handle names. found is false when this adapter never
// ran the resource (another scope or never recorded): such a resource is
// observed as removed, and nothing of another scope is reachable by name.
func (r *KernelResources) bound(h kr.ResourceHandle) (krec, bool, error) {
	if err := h.Validate(); err != nil {
		return krec{}, false, err
	}
	rec, found, err := r.core.load(handleRec(h).Name)
	if err != nil || !found {
		return krec{}, false, err
	}
	if err := recordMatches(rec, handleRec(h)); err != nil {
		return krec{}, false, err
	}
	return rec, true, nil
}

func removedObs(h kr.ResourceHandle, at kr.Clock) kr.ResourceObservation {
	return kr.ResourceObservation{Handle: h, State: kr.ResourceRemoved, ObservedAt: at.Now().UTC()}
}

// Observe implements kr.ResourceManager.
func (r *KernelResources) Observe(ctx context.Context, h kr.ResourceHandle) (kr.ResourceObservation, error) {
	rec, found, err := r.bound(h)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if !found {
		return removedObs(h, r.core.cfg.Clock), nil
	}
	unlock, err := r.core.locks.lock(ctx, rec.Name)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	defer unlock()
	if rec, found, err = r.bound(h); err != nil || !found {
		return removedObs(h, r.core.cfg.Clock), err
	}
	return r.observeLocked(ctx, h, &rec)
}

// Logs implements kr.ResourceManager. A workspace has no output. The record is
// loaded and validated under the lock, never before it: findContainer persists
// what it learns, and a snapshot taken before the lock could roll back a newer
// fence or stop intent.
func (r *KernelResources) Logs(ctx context.Context, h kr.ResourceHandle, opts kr.LogOptions) (io.ReadCloser, error) {
	if h.Kind == kr.ResourceWorkspace {
		return nil, fmt.Errorf("%w: a workspace has no output", domain.ErrConflict)
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	unlock, err := r.core.locks.lock(ctx, handleRec(h).Name)
	if err != nil {
		return nil, err
	}
	rec, found, err := r.bound(h)
	if err != nil || !found {
		unlock()
		if err == nil {
			err = fmt.Errorf("%w: no service %s", domain.ErrConflict, h.Name)
		}
		return nil, err
	}
	ct, ok, err := r.core.findContainer(ctx, &rec)
	unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: the container of %s is not observed", domain.ErrConflict, h.Name)
	}
	return r.core.logStream(ctx, ct.ID, opts.Follow), nil
}

// Stop implements kr.ResourceManager. The stop intent is recorded first and
// fences the identity (a later Ensure creates nothing); a stop from a stale
// fence is refused. A workspace is stopped once no container runs on it.
func (r *KernelResources) Stop(ctx context.Context, h kr.ResourceHandle) (kr.ResourceObservation, error) {
	return r.settle(ctx, h, false)
}

// Remove implements kr.ResourceManager. It confirms with ResourceRemoved only
// when the engine no longer has the object and no creation is unresolved.
func (r *KernelResources) Remove(ctx context.Context, h kr.ResourceHandle) (kr.ResourceObservation, error) {
	return r.settle(ctx, h, true)
}

func (r *KernelResources) settle(ctx context.Context, h kr.ResourceHandle, remove bool) (kr.ResourceObservation, error) {
	if err := h.Validate(); err != nil {
		return kr.ResourceObservation{}, err
	}
	c := r.core
	want := handleRec(h)
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	defer unlock()
	if remove {
		// A cleanup from a fence older than the run network's is refused before
		// any effect, whichever resource it comes through.
		if err := c.checkNetworkFence(h.Scope, h.RunID, h.Fence); err != nil {
			return kr.ResourceObservation{}, err
		}
	}
	rec, err := c.fenceStop(want)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if rec.Class == classWorkspace {
		err = r.settleWorkspace(ctx, &rec, remove)
	} else {
		err = r.settleService(ctx, &rec, remove)
	}
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if err := c.save(rec); err != nil {
		return kr.ResourceObservation{}, err
	}
	// Cleanup of the run's network belongs to resource settlement. The removal
	// carries a durable obligation (rec.NetPending) that is met only when the
	// network is confirmed removed or owned by a container that still uses it; a
	// network whose create is unresolved, or whose removal fails, leaves it
	// recorded, and Observe honours it too.
	if remove && rec.Removed {
		if _, err := c.settleNetworkFor(ctx, &rec); err != nil {
			return kr.ResourceObservation{}, err
		}
	}
	return r.observeLocked(ctx, h, &rec)
}

func (r *KernelResources) settleService(ctx context.Context, rec *krec, remove bool) error {
	c := r.core
	ct, found, err := c.findContainer(ctx, rec)
	if err != nil && !errors.Is(err, errForeign) {
		return err
	}
	if found {
		if ct, found, err = c.stopContainer(ctx, *rec, ct); err != nil {
			return err
		}
		if found && ct.State.Running {
			return nil // not stopped yet: the intent stays recorded
		}
		if found && remove {
			if _, found, err = c.removeContainer(ctx, *rec); err != nil {
				return err
			}
			if found {
				return uncertain("the container of %s still exists after removal", rec.Identity)
			}
		}
	}
	rec.Stopped = true
	if remove && !found && !rec.Unanswered {
		rec.Removed = true
	}
	return nil
}

// settleWorkspace stops (fences) the workspace and, when asked, removes its
// volume. The volume is released only when no container still runs on it and no
// holder is unresolved, and removed only when none references it: containers are
// the engine's to remove first, and the engine is never forced to drop a volume
// in use. A holder whose create outcome is unresolved is invisible to the engine,
// so it is read from the durable holder intents written before every launch; the
// workspace is neither released nor removed while one exists.
func (r *KernelResources) settleWorkspace(ctx context.Context, rec *krec, remove bool) error {
	c := r.core
	prepUnresolved, err := r.retirePreparation(ctx, rec)
	if err != nil {
		return err
	}
	// A volume whose create response was lost is adopted before anything else, so
	// its identity is bound and the outstanding create resolved.
	found, ours, err := c.bindObject(ctx, c.volumeObject(), rec)
	if err != nil {
		return err
	}
	if err := c.resolveHolders(ctx, rec.Name); err != nil {
		return err
	}
	holders, err := r.holders(ctx, rec.Name)
	if err != nil {
		return err
	}
	for id, state := range holders {
		if state == "running" || state == "stopping" || state == "paused" {
			return nil // not released: a container still runs on it
		}
		if remove {
			return fmt.Errorf("%w: container %s still references the workspace of %s", kr.ErrIllegalTransition, id[:12], rec.Identity)
		}
	}
	unresolved, err := c.unresolvedHolders(rec.Name)
	if err != nil {
		return err
	}
	if prepUnresolved {
		unresolved = append(unresolved, "preparation")
	}
	if len(unresolved) > 0 {
		if remove {
			return uncertain("%d create(s) holding the workspace of %s are unresolved; it is not removed", len(unresolved), rec.Identity)
		}
		return nil // fenced, not released
	}
	rec.Stopped = true
	if !remove {
		return nil
	}
	if found && ours {
		res, err := c.pm.run(ctx, "volume", "rm", rec.Name)
		if err != nil {
			return err
		}
		if res.code != 0 && !strings.Contains(res.stderr, "no such volume") {
			return uncertain("podman volume rm: %s", strings.TrimSpace(res.stderr))
		}
		if _, still, err := c.pm.inspectVolume(ctx, rec.Name); err != nil {
			return err
		} else if still {
			return uncertain("the volume of %s still exists after removal", rec.Identity)
		}
	}
	if !rec.Unanswered {
		rec.Removed = true
	}
	return nil
}

// retirePreparation fences the workspace's preparation container and removes it
// once it no longer runs. It reports whether the preparation's create is still
// unresolved.
func (r *KernelResources) retirePreparation(ctx context.Context, ws *krec) (unresolved bool, err error) {
	c := r.core
	name := prepName(ws.Scope, ws.Identity)
	unlock, err := c.locks.lock(ctx, name)
	if err != nil {
		return false, err
	}
	defer unlock()
	prec, found, err := c.load(name)
	if err != nil {
		return false, err
	}
	if !found {
		// A preparation create that never happened must never land later.
		want := krec{Name: name, Class: classPreparation, Scope: ws.Scope, RunID: ws.RunID, Identity: ws.Identity, Fence: ws.Fence}
		_, err := c.fenceStop(want)
		return false, err
	}
	if prec.Removed {
		return false, nil
	}
	prec.Stopping = true
	if err := c.save(prec); err != nil {
		return false, err
	}
	ct, ok, err := c.findContainer(ctx, &prec)
	if err != nil && !errors.Is(err, errForeign) {
		return false, err
	}
	if ok {
		if ct, ok, err = c.stopContainer(ctx, prec, ct); err != nil {
			return false, err
		}
		if ok && ct.State.Running {
			return false, nil
		}
		if ok {
			if _, ok, err = c.removeContainer(ctx, prec); err != nil {
				return false, err
			} else if ok {
				return false, uncertain("the preparation container of %s still exists after removal", ws.Identity)
			}
		}
	}
	prec.Stopped = true
	prec.Removed = !ok && !prec.Unanswered
	if err := c.save(prec); err != nil {
		return false, err
	}
	return !ok && prec.Unanswered, nil
}

// holders lists every container, running or not, that references the volume.
// An empty answer is an answer only when the engine said so.
func (r *KernelResources) holders(ctx context.Context, volume string) (map[string]string, error) {
	res, err := r.core.pm.query(ctx, "ps", "--all", "--no-trunc", "--filter", "volume="+volume, "--format", "{{.ID}} {{.State}}")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		id, state, ok := strings.Cut(line, " ")
		if !ok {
			return nil, errors.New("podman: unreadable ps output")
		}
		out[id] = strings.ToLower(strings.TrimSpace(state))
	}
	return out, nil
}
