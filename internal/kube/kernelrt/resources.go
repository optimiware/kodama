package kernelrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Resources owns the run's workspace claim and its support services.
type Resources struct{ *core }

// Capabilities reports what the adapter enforces.
func (r *Resources) Capabilities(ctx context.Context) (runtime.CapabilitySet, error) {
	return r.capabilities(), ctx.Err()
}

// ReleaseEvidence is what the cluster shows about the storage of a removed
// workspace. A workspace is removed only when the claim and every volume it was
// or may have been bound to are gone, no volume is attached, and enough time
// has passed since the claim disappeared that a binding still in progress would
// have shown. A volume that is kept is retained evidence.
type ReleaseEvidence struct {
	ClaimGone bool `json:"claimGone"`
	// Volumes are the volumes found for the claim: the one it was seen bound to
	// and every volume whose claimRef names it.
	Volumes    []string `json:"volumes,omitempty"`
	VolumeGone bool     `json:"volumeGone"`
	Attached   bool     `json:"attached,omitempty"`
	// ReclaimPolicy is the policy of the first surviving volume.
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
	// Retained means a surviving volume will not be reclaimed by the cluster.
	Retained bool `json:"retained,omitempty"`
	// Unconfirmed means the claim is gone but its binding cannot yet be ruled
	// out: its deletion was not observed long enough ago for a volume still
	// being bound to have appeared.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
}

// Released reports whether every release condition holds.
func (e ReleaseEvidence) Released() bool {
	return e.ClaimGone && e.VolumeGone && !e.Attached && !e.Unconfirmed
}

func (c *core) validateEnsure(spec runtime.ResourceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Lifetime != runtime.LifetimePipeline {
		return fmt.Errorf("%w: %s lifetime services are not provided", runtime.ErrCapability, spec.Lifetime)
	}
	switch spec.Kind {
	case runtime.ResourceWorkspace:
		p := spec.Preparation
		if len(p.Sources) > 0 || len(p.Credentials) > 0 {
			return fmt.Errorf("%w: workspace preparation from sources is not provided by this backend", runtime.ErrCapability)
		}
	default:
		s := spec.Service
		if !pinned(s.Image) {
			return fmt.Errorf("%w: image %q is not pinned by digest", runtime.ErrCapability, s.Image)
		}
		if len(s.Secrets) > 0 {
			return fmt.Errorf("%w: service secret bindings are not provided by this backend", runtime.ErrCapability)
		}
		return validateEnv(s.Env)
	}
	return nil
}

func (c *core) handleOf(id ident, fence runtime.Fence) runtime.ResourceHandle {
	k := runtime.ResourceService
	if id.kind == kindWorkspace {
		k = runtime.ResourceWorkspace
	}
	return runtime.ResourceHandle{Scope: id.scope, RunID: id.run, Kind: k, Name: id.name, Identity: id.id, Fence: fence}
}

func (c *core) robs(h runtime.ResourceHandle, st runtime.ResourceState, reason runtime.Reason) runtime.ResourceObservation {
	return runtime.ResourceObservation{Handle: h, State: st, Reason: reason, ObservedAt: c.opts.Clock()}
}

// Ensure creates or adopts the workspace claim or service. It is idempotent on
// the identity under the same adoption and fence rules as Runtime.Start, and
// every create runs under the conditional reservation of guarded. An
// established object that has disappeared is reported lost, never recreated.
func (r *Resources) Ensure(ctx context.Context, spec runtime.ResourceSpec) (runtime.ResourceObservation, error) {
	if err := r.validateEnsure(spec); err != nil {
		return runtime.ResourceObservation{}, notReached(err)
	}
	t, err := r.admit(ctx, spec.Scope, spec.Requires)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	id := resourceIdent(spec.Scope, spec.RunID, spec.Kind, spec.Name, spec.Identity, spec.SpecDigest, r.cfg.Backend)
	h := r.handleOf(id, spec.Fence)
	if spec.Kind == runtime.ResourceWorkspace {
		if _, err := r.storageClass(ctx); err != nil {
			return runtime.ResourceObservation{}, err
		}
	}
	rec, err := r.claim(ctx, t.Namespace, id, spec.Fence)
	if err != nil {
		return runtime.ResourceObservation{}, notReached(err)
	}
	var eerr error
	if spec.Kind == runtime.ResourceWorkspace {
		eerr = r.ensureWorkspace(ctx, t, id, rec, spec.Fence)
	} else {
		eerr = r.ensureService(ctx, t, id, rec, *spec.Service, spec.Fence)
	}
	if eerr != nil {
		return runtime.ResourceObservation{}, eerr
	}
	return r.observeResource(ctx, t, id, h)
}

func (c *core) getPVC(ctx context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, bool, error) {
	p, err := c.cluster.GetPVC(ctx, ns, name)
	switch {
	case err == nil:
		return p, true, nil
	case errors.Is(err, kubernetes.ErrNotFound):
		return p, false, nil
	}
	return p, false, fmt.Errorf("kernelrt: reading the workspace claim: %w", err)
}

func (c *core) ensureWorkspace(ctx context.Context, t Target, id ident, rec record, fence runtime.Fence) error {
	ns := t.Namespace
	p, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return notReached(err)
	}
	if rec, err = c.current(ctx, ns, id, fence); err != nil {
		return notReached(err)
	}
	if found {
		if err := c.verifyPVC(p, id, rec); err != nil {
			return notReached(err)
		}
		if _, err := c.establishClaim(ctx, ns, id, rec, p); err != nil {
			return notReached(err)
		}
		return nil
	}
	if rec.created[opPVC] != "" {
		return nil // lost; the observation says so
	}
	return c.guarded(ctx, ns, id, guard{
		op: opPVC, fence: fence,
		create: func() (string, error) {
			out, err := c.cluster.CreatePVC(ctx, ns, c.workspaceClaim(id, t))
			return out.Metadata.UID, err
		},
		readBack: func() (string, error) {
			p, ok, err := c.getPVC(ctx, ns, id.base())
			if err != nil || !ok {
				return "", uncertain(errors.Join(err, errors.New("the claim exists but cannot be read")))
			}
			rc, _, err := c.readRecord(ctx, ns, id)
			if err != nil {
				return "", uncertain(err)
			}
			if err := c.verifyPVC(p, id, rc); err != nil {
				return "", notReached(err)
			}
			return p.Metadata.UID, nil
		},
		reap: func(uid string) error { return c.reapClaim(ctx, ns, id, uid) },
	})
}

// deleteClaim deletes the workspace claim under its UID after re-reading it. It
// returns the claim it deleted or saw, and whether it is still present.
func (c *core) deleteClaim(ctx context.Context, ns string, id ident) (kubernetes.PersistentVolumeClaim, error) {
	p, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil || !found {
		return p, err
	}
	if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return p, fmt.Errorf("%w: claim %s", ErrForeign, p.Metadata.Name)
	}
	if p.Metadata.DeletionTimestamp == "" {
		if err := c.cluster.DeletePVC(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return p, fmt.Errorf("kernelrt: deleting the workspace claim: %w", err)
		}
	}
	return p, nil
}

// reapClaim deletes the claim with exactly this UID. Before it does, the UID
// and any volume the claim is bound to are written to the record as cleanup
// evidence, apart from the authoritative established UID: the claim is the only
// pointer to its volume, and a volume found later through its claimRef must not
// be mistaken for someone else's. If the evidence cannot be recorded the claim
// is left alone.
func (c *core) reapClaim(ctx context.Context, ns string, id ident, uid string) error {
	p, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return err
	}
	ours := found && p.Metadata.UID == uid && id.owns(p.Metadata.Labels, p.Metadata.Annotations)
	vol := ""
	if ours {
		vol = p.Spec.VolumeName
	}
	if _, err := c.mutate(ctx, ns, id, func(r *record) error {
		r.addCompensated(uid)
		r.addVolumes(vol)
		return nil
	}); err != nil {
		return err
	}
	if !ours {
		return nil
	}
	if err := c.cluster.DeletePVC(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: uid}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the workspace claim: %w", err)
	}
	return nil
}

// reapPod deletes the service Pod with exactly this UID.
func (c *core) reapPod(ctx context.Context, ns string, id ident, uid string) error {
	p, found, err := c.getPod(ctx, ns, id.base())
	if err != nil || !found || p.Metadata.UID != uid || !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return err
	}
	if err := c.cluster.DeletePod(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: uid, Propagation: "Foreground"}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the Pod: %w", err)
	}
	return nil
}

// reapService deletes the Service with exactly this UID.
func (c *core) reapService(ctx context.Context, ns string, id ident, uid string) error {
	s, found, err := c.getService(ctx, ns, id.base())
	if err != nil || !found || s.Metadata.UID != uid || !id.owns(s.Metadata.Labels, s.Metadata.Annotations) {
		return err
	}
	if err := c.cluster.DeleteService(ctx, ns, s.Metadata.Name, kubernetes.DeleteOptions{UID: uid}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the Service: %w", err)
	}
	return nil
}

func (c *core) getPod(ctx context.Context, ns, name string) (kubernetes.Pod, bool, error) {
	p, err := c.cluster.GetPod(ctx, ns, name)
	switch {
	case err == nil:
		return p, true, nil
	case errors.Is(err, kubernetes.ErrNotFound):
		return p, false, nil
	}
	return p, false, fmt.Errorf("kernelrt: reading the service Pod: %w", err)
}

func (c *core) getService(ctx context.Context, ns, name string) (kubernetes.Service, bool, error) {
	s, err := c.cluster.GetService(ctx, ns, name)
	switch {
	case err == nil:
		return s, true, nil
	case errors.Is(err, kubernetes.ErrNotFound):
		return s, false, nil
	}
	return s, false, fmt.Errorf("kernelrt: reading the Service: %w", err)
}

// afterPod marks an error from a step that follows the Pod create as uncertain:
// the Pod exists, so the Ensure as a whole did reach the cluster.
func afterPod(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(runtime.ErrUncertain, err)
}

func (c *core) ensureService(ctx context.Context, t Target, id ident, rec record, s runtime.ServiceSpec, fence runtime.Fence) error {
	ns := t.Namespace
	pod, found, err := c.getPod(ctx, ns, id.base())
	if err != nil {
		return notReached(err)
	}
	if rec, err = c.current(ctx, ns, id, fence); err != nil {
		return notReached(err)
	}
	switch {
	case found:
		if err := c.verifyPod(pod, id, t, rec); err != nil {
			return notReached(err)
		}
		if err := c.establish(ctx, ns, id, rec, opPod, pod.Metadata.UID); err != nil {
			return notReached(err)
		}
	case rec.created[opPod] != "":
		return nil // the established Pod was lost: never silently replaced
	default:
		err := c.guarded(ctx, ns, id, guard{
			op: opPod, fence: fence,
			create: func() (string, error) {
				out, err := c.cluster.CreatePod(ctx, ns, c.servicePod(id, s, t, fence))
				return out.Metadata.UID, err
			},
			readBack: func() (string, error) {
				p, ok, err := c.getPod(ctx, ns, id.base())
				if err != nil || !ok {
					return "", uncertain(errors.Join(err, errors.New("the Pod exists but cannot be read")))
				}
				rc, _, err := c.readRecord(ctx, ns, id)
				if err != nil {
					return "", uncertain(err)
				}
				if err := c.verifyPod(p, id, t, rc); err != nil {
					return "", notReached(err)
				}
				return p.Metadata.UID, nil
			},
			reap: func(uid string) error { return c.reapPod(ctx, ns, id, uid) },
		})
		if err != nil {
			return err
		}
	}
	if len(s.Ports) == 0 {
		return nil
	}
	// The Pod now exists: from here a failure is uncertain, not "not reached".
	rec, ok, err := c.readRecord(ctx, ns, id)
	if err != nil || !ok {
		return afterPod(errors.Join(err, errors.New("the record cannot be read")))
	}
	svc, sfound, err := c.getService(ctx, ns, id.base())
	if err != nil {
		return afterPod(err)
	}
	switch {
	case sfound:
		if err := c.verifyServiceObject(svc, id, rec); err != nil {
			return afterPod(err)
		}
		return afterPod(c.establish(ctx, ns, id, rec, opService, svc.Metadata.UID))
	case rec.created[opService] != "":
		return nil // lost
	}
	return afterPod(c.guarded(ctx, ns, id, guard{
		op: opService, fence: fence,
		create: func() (string, error) {
			out, err := c.cluster.CreateService(ctx, ns, c.serviceObject(id, s, t))
			return out.Metadata.UID, err
		},
		readBack: func() (string, error) {
			sv, ok, err := c.getService(ctx, ns, id.base())
			if err != nil || !ok {
				return "", uncertain(errors.Join(err, errors.New("the Service exists but cannot be read")))
			}
			rc, _, err := c.readRecord(ctx, ns, id)
			if err != nil {
				return "", uncertain(err)
			}
			if err := c.verifyServiceObject(sv, id, rc); err != nil {
				return "", notReached(err)
			}
			return sv.Metadata.UID, nil
		},
		reap: func(uid string) error { return c.reapService(ctx, ns, id, uid) },
	}))
}

// Observe reports the resource by its stable identity.
func (r *Resources) Observe(ctx context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	return r.observeResource(ctx, t, id, h)
}

func (c *core) handleTarget(ctx context.Context, h runtime.ResourceHandle) (Target, ident, error) {
	if err := h.Validate(); err != nil {
		return Target{}, ident{}, err
	}
	t, err := c.targets.Target(ctx, h.Scope)
	if err != nil {
		return Target{}, ident{}, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	return t, handleIdent(h, c.cfg.Backend), nil
}

func (c *core) observeResource(ctx context.Context, t Target, id ident, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	rec, found, err := c.readRecord(ctx, t.Namespace, id)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if id.kind == kindWorkspace {
		return c.observeWorkspace(ctx, t.Namespace, id, h, rec, found)
	}
	return c.observeService(ctx, t, id, h, rec, found)
}

// absent maps "the object is not there" using the record: nothing was ever
// issued or removal was confirmed (removed), a create may still land (unknown),
// or the object disappeared (lost) or was stopped.
func (c *core) absent(h runtime.ResourceHandle, rec record, found bool, lost runtime.Reason) runtime.ResourceObservation {
	switch {
	case !found, rec.state == recRemoved:
		return c.robs(h, runtime.ResourceRemoved, runtime.ReasonNone)
	case !rec.quiet(c.opts.Clock(), c.opts.CreateQuiescence):
		return c.robs(h, runtime.ResourceUnknown, runtime.ReasonNone)
	case rec.state == recStopped:
		return c.robs(h, runtime.ResourceStopped, runtime.ReasonNone)
	}
	return c.robs(h, runtime.ResourceFailed, lost)
}

func (c *core) observeWorkspace(ctx context.Context, ns string, id ident, h runtime.ResourceHandle, rec record, found bool) (runtime.ResourceObservation, error) {
	p, pfound, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if !pfound {
		return c.absent(h, rec, found, runtime.ReasonWorkspaceLost), nil
	}
	if err := c.verifyPVC(p, id, rec); err != nil {
		return runtime.ResourceObservation{}, err
	}
	// Recovery from an uncertain create: the verified instance is recorded before
	// it is reported usable, so a later loss is never papered over by an empty
	// replacement.
	if found {
		if _, err := c.establishClaim(ctx, ns, id, rec, p); err != nil {
			return runtime.ResourceObservation{}, err
		}
	}
	stopped := found && rec.state != recActive
	switch {
	case stopped:
		return c.robs(h, runtime.ResourceStopped, runtime.ReasonNone), nil
	case p.Metadata.DeletionTimestamp != "" || p.Status.Phase == "Lost":
		return c.robs(h, runtime.ResourceFailed, runtime.ReasonWorkspaceLost), nil
	case p.Status.Phase == "Bound":
		return c.robs(h, runtime.ResourceReady, runtime.ReasonNone), nil
	}
	// Pending: a class that binds on first use is usable now; the claim binds
	// when the first step Pod is scheduled.
	sc, err := c.cluster.GetStorageClass(ctx, c.cfg.StorageClass)
	if err != nil {
		return runtime.ResourceObservation{}, fmt.Errorf("kernelrt: reading the storage class: %w", err)
	}
	if sc.VolumeBindingMode == "WaitForFirstConsumer" {
		return c.robs(h, runtime.ResourceReady, runtime.ReasonNone), nil
	}
	return c.robs(h, runtime.ResourceStarting, runtime.ReasonNone), nil
}

func (c *core) observeService(ctx context.Context, t Target, id ident, h runtime.ResourceHandle, rec record, found bool) (runtime.ResourceObservation, error) {
	ns := t.Namespace
	pod, pfound, err := c.getPod(ctx, ns, id.base())
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if !pfound {
		return c.absent(h, rec, found, runtime.ReasonServiceLost), nil
	}
	if err := c.verifyPod(pod, id, t, rec); err != nil {
		return runtime.ResourceObservation{}, err
	}
	if found {
		if err := c.establish(ctx, ns, id, rec, opPod, pod.Metadata.UID); err != nil {
			return runtime.ResourceObservation{}, err
		}
	}
	if found && rec.state != recActive {
		return c.robs(h, runtime.ResourceStopped, runtime.ReasonNone), nil
	}
	if pod.Metadata.DeletionTimestamp != "" {
		return c.robs(h, runtime.ResourceFailed, runtime.ReasonServiceLost), nil
	}
	switch pod.Status.Phase {
	case "Succeeded", "Failed":
		return c.robs(h, runtime.ResourceFailed, runtime.ReasonServiceLost), nil
	case "Running":
	case "", "Pending":
		return c.robs(h, runtime.ResourceStarting, runtime.ReasonNone), nil
	default:
		return c.robs(h, runtime.ResourceUnknown, runtime.ReasonNone), nil
	}
	ready := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerSvc && cs.Ready {
			ready = true
		}
	}
	if !ready {
		return c.robs(h, runtime.ResourceStarting, runtime.ReasonNone), nil
	}
	if len(pod.Spec.Containers[0].Ports) > 0 {
		svc, ok, err := c.getService(ctx, ns, id.base())
		switch {
		case err != nil:
			return runtime.ResourceObservation{}, err
		case ok:
			if err := c.verifyServiceObject(svc, id, rec); err != nil {
				return runtime.ResourceObservation{}, err
			}
			if found {
				if err := c.establish(ctx, ns, id, rec, opService, svc.Metadata.UID); err != nil {
					return runtime.ResourceObservation{}, err
				}
			}
		case rec.created[opService] != "":
			return c.robs(h, runtime.ResourceFailed, runtime.ReasonServiceLost), nil
		default:
			return c.robs(h, runtime.ResourceStarting, runtime.ReasonNone), nil
		}
	}
	return c.robs(h, runtime.ResourceReady, runtime.ReasonNone), nil
}

// Logs streams a service's output; a workspace has none.
func (r *Resources) Logs(ctx context.Context, h runtime.ResourceHandle, _ runtime.LogOptions) (io.ReadCloser, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return nil, err
	}
	if id.kind != kindService {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "a workspace has no logs"}
	}
	pod, found, err := r.getPod(ctx, t.Namespace, id.base())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "the service has no Pod"}
	}
	if !id.owns(pod.Metadata.Labels, pod.Metadata.Annotations) {
		return nil, fmt.Errorf("%w: pod %s", ErrForeign, pod.Metadata.Name)
	}
	data, err := r.cluster.PodLog(ctx, t.Namespace, pod.Metadata.Name, containerSvc)
	if err != nil {
		return nil, fmt.Errorf("kernelrt: reading the log: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Stop fences the identity, then stops the resource: a service's Pod and
// Service are deleted; a workspace claim is kept, with its data, until Remove.
func (r *Resources) Stop(ctx context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if _, err := r.settle(ctx, t.Namespace, id, h.Fence, recStopped); err != nil {
		return runtime.ResourceObservation{}, err
	}
	if id.kind == kindService {
		if _, err := r.deleteServiceObjects(ctx, t.Namespace, id); err != nil {
			return runtime.ResourceObservation{}, err
		}
	}
	return r.observeResource(ctx, t, id, h)
}

// Remove deletes the resource and confirms with ResourceRemoved only when the
// cluster shows nothing left, no create can still land and, for a workspace,
// the volume was released (see ReleaseEvidence). A retained volume is reported
// as ResourceFailed with ReasonCleanupFailed, never as removed.
func (r *Resources) Remove(ctx context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	return r.removeResource(ctx, t.Namespace, id, h)
}

func (c *core) removeResource(ctx context.Context, ns string, id ident, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	rec, err := c.settle(ctx, ns, id, h.Fence, recStopped)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	var remaining bool
	reason := runtime.ReasonNone
	if id.kind == kindWorkspace {
		var ev ReleaseEvidence
		ev, _, err = c.deleteWorkspace(ctx, ns, id, rec)
		if err != nil {
			return runtime.ResourceObservation{}, err
		}
		remaining = !ev.Released()
		if ev.Retained {
			reason = runtime.ReasonCleanupFailed
		}
	} else {
		remaining, err = c.deleteServiceObjects(ctx, ns, id)
		if err != nil {
			return runtime.ResourceObservation{}, err
		}
	}
	switch {
	case reason != runtime.ReasonNone:
		return c.robs(h, runtime.ResourceFailed, reason), nil
	case remaining:
		return c.robs(h, runtime.ResourceStopped, runtime.ReasonNone), nil
	}
	// The closing write re-reads the record: a create reserved since the deletes
	// above keeps the identity open.
	if _, err := c.settle(ctx, ns, id, h.Fence, recRemoved); err != nil {
		if errors.Is(err, errNotQuiet) {
			return c.robs(h, runtime.ResourceUnknown, runtime.ReasonNone), nil
		}
		return runtime.ResourceObservation{}, err
	}
	return c.robs(h, runtime.ResourceRemoved, runtime.ReasonNone), nil
}

// deleteServiceObjects deletes the Service and the Pod under their UIDs after
// re-reading them, and reports whether either remains.
func (c *core) deleteServiceObjects(ctx context.Context, ns string, id ident) (bool, error) {
	svc, sfound, err := c.getService(ctx, ns, id.base())
	if err != nil {
		return false, err
	}
	if sfound {
		if !id.owns(svc.Metadata.Labels, svc.Metadata.Annotations) {
			return false, fmt.Errorf("%w: service %s", ErrForeign, svc.Metadata.Name)
		}
		if err := c.cluster.DeleteService(ctx, ns, svc.Metadata.Name, kubernetes.DeleteOptions{UID: svc.Metadata.UID}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return false, fmt.Errorf("kernelrt: deleting the Service: %w", err)
		}
	}
	pod, pfound, err := c.getPod(ctx, ns, id.base())
	if err != nil {
		return false, err
	}
	if pfound {
		if !id.owns(pod.Metadata.Labels, pod.Metadata.Annotations) {
			return false, fmt.Errorf("%w: pod %s", ErrForeign, pod.Metadata.Name)
		}
		if pod.Metadata.DeletionTimestamp == "" {
			if err := c.cluster.DeletePod(ctx, ns, pod.Metadata.Name, kubernetes.DeleteOptions{UID: pod.Metadata.UID, Propagation: "Foreground"}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
				return false, fmt.Errorf("kernelrt: deleting the Pod: %w", err)
			}
		}
	}
	_, sfound, err = c.getService(ctx, ns, id.base())
	if err != nil || sfound {
		return true, err
	}
	_, pfound, err = c.getPod(ctx, ns, id.base())
	return pfound, err
}

// deleteWorkspace deletes the claim under its UID. Before it does, the claim's
// UID and the volume it is bound to are written to the record, because the claim
// is the only pointer to them; the time the claim was first seen gone is
// written too, which starts the wait that rules out a binding in progress.
func (c *core) deleteWorkspace(ctx context.Context, ns string, id ident, rec record) (ReleaseEvidence, record, error) {
	p, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return ReleaseEvidence{}, rec, err
	}
	if found {
		if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
			return ReleaseEvidence{}, rec, fmt.Errorf("%w: claim %s", ErrForeign, p.Metadata.Name)
		}
		if rec, err = c.establishClaim(ctx, ns, id, rec, p); err != nil {
			return ReleaseEvidence{}, rec, err
		}
		if _, err := c.deleteClaim(ctx, ns, id); err != nil {
			return ReleaseEvidence{}, rec, err
		}
		if _, still, err := c.getPVC(ctx, ns, id.base()); err != nil {
			return ReleaseEvidence{}, rec, err
		} else if still {
			return c.evidenceAndPersist(ctx, ns, id, rec)
		}
	}
	// The claim is gone: remember since when, once.
	if rec.claimGone.IsZero() && c.issuedClaim(rec) {
		now := c.opts.Clock()
		rec, err = c.mutate(ctx, ns, id, func(r *record) error {
			if r.claimGone.IsZero() {
				r.claimGone = now
			}
			return nil
		})
		if err != nil {
			return ReleaseEvidence{}, rec, err
		}
	}
	return c.evidenceAndPersist(ctx, ns, id, rec)
}

// evidenceAndPersist reads the release evidence and records every volume
// identity it found, so a later pass still knows them after the claim and then
// the volume object are gone.
func (c *core) evidenceAndPersist(ctx context.Context, ns string, id ident, rec record) (ReleaseEvidence, record, error) {
	ev, known, err := c.evidence(ctx, ns, id, rec)
	// Persist what was discovered before judging the failure: an attachment list
	// that cannot be read must not cost the identities already found, which the
	// volume object may not outlive.
	if len(known) > 0 && !containsAll(rec.volumes, known) {
		next, perr := c.mutate(ctx, ns, id, func(r *record) error { r.addVolumes(known...); return nil })
		if perr != nil {
			return ev, rec, errors.Join(err, perr)
		}
		rec = next
	}
	return ev, rec, err
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// establishClaim records the claim's UID and the volume it is bound to. A UID
// that differs from the recorded one is a replaced claim.
func (c *core) establishClaim(ctx context.Context, ns string, id ident, rec record, p kubernetes.PersistentVolumeClaim) (record, error) {
	uid, vol := p.Metadata.UID, p.Spec.VolumeName
	if got := rec.created[opPVC]; got != "" && got != uid {
		return rec, unverified("the claim was replaced by another instance")
	}
	if rec.created[opPVC] == uid && (vol == "" || slices.Contains(rec.volumes, vol)) {
		return rec, nil
	}
	return c.mutate(ctx, ns, id, func(r *record) error {
		if err := r.checkEstablish(opPVC, uid); err != nil {
			return err
		}
		r.created[opPVC] = uid
		r.addVolumes(vol)
		return nil
	})
}

// issuedClaim reports whether a claim create was ever issued for the record.
func (c *core) issuedClaim(rec record) bool {
	return rec.marked(opPVC) || rec.created[opPVC] != ""
}

// evidence reads the release evidence of a workspace. Volumes are discovered by
// the claimRef that names the claim, not only by the name remembered from the
// claim: a claim deleted before the adapter saw it bound still leaves its
// volume behind.
//
// It also returns every volume identity the claim is known to have had: the
// persisted ones, which outlive the volume object, and those found now. An
// attachment is judged against all of them, not only against volumes that still
// exist.
func (c *core) evidence(ctx context.Context, ns string, id ident, rec record) (ReleaseEvidence, []string, error) {
	_, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return ReleaseEvidence{}, nil, err
	}
	ev := ReleaseEvidence{ClaimGone: !found}
	pvs, err := c.cluster.ListPVs(ctx)
	if err != nil {
		return ev, nil, fmt.Errorf("kernelrt: listing volumes: %w", err)
	}
	known := slices.Clone(rec.volumes)
	var names []string
	for _, pv := range pvs {
		ref := pv.Spec.ClaimRef
		byClaim := ref != nil && ref.Namespace == ns && ref.Name == id.base() &&
			(rec.created[opPVC] == "" || ref.UID == "" || ref.UID == rec.created[opPVC] || slices.Contains(rec.compensated, ref.UID))
		if byClaim || slices.Contains(rec.volumes, pv.Metadata.Name) {
			names = append(names, pv.Metadata.Name)
			if !slices.Contains(known, pv.Metadata.Name) {
				known = append(known, pv.Metadata.Name)
			}
			ev.Volumes = append(ev.Volumes, pv.Metadata.Name)
			if ev.ReclaimPolicy == "" {
				ev.ReclaimPolicy = pv.Spec.PersistentVolumeReclaimPolicy
			}
			if pv.Spec.PersistentVolumeReclaimPolicy == "Retain" && ev.ClaimGone {
				ev.Retained = true
			}
		}
	}
	slices.Sort(ev.Volumes)
	slices.Sort(known)
	ev.VolumeGone = len(names) == 0
	if ev.ClaimGone && c.issuedClaim(rec) {
		now := c.opts.Clock()
		ev.Unconfirmed = rec.claimGone.IsZero() || now.Before(rec.claimGone.Add(c.opts.CreateQuiescence))
	}
	attachments, err := c.cluster.ListVolumeAttachments(ctx)
	if err != nil {
		return ev, known, fmt.Errorf("kernelrt: listing volume attachments: %w", err)
	}
	for _, a := range attachments {
		if v := a.Spec.Source.PersistentVolumeName; v != nil && slices.Contains(known, *v) && a.Status.Attached {
			ev.Attached = true
		}
	}
	return ev, known, nil
}

// ReleaseEvidence reports the storage release evidence of a workspace handle. It
// only reads.
func (r *Resources) ReleaseEvidence(ctx context.Context, h runtime.ResourceHandle) (ReleaseEvidence, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	if id.kind != kindWorkspace {
		return ReleaseEvidence{}, invalidf("only a workspace has release evidence")
	}
	rec, found, err := r.readRecord(ctx, t.Namespace, id)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	if !found {
		return ReleaseEvidence{ClaimGone: true, VolumeGone: true}, nil
	}
	ev, _, err := r.evidence(ctx, t.Namespace, id, rec)
	return ev, err
}
