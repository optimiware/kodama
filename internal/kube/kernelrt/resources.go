package kernelrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

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
// workspace. A workspace is removed only when the claim and its volume are gone
// and the volume is not attached; a volume that is kept is retained evidence.
type ReleaseEvidence struct {
	ClaimGone bool `json:"claimGone"`
	// Volume is the volume the claim was bound to; empty when it never bound.
	Volume        string `json:"volume,omitempty"`
	VolumeGone    bool   `json:"volumeGone"`
	Attached      bool   `json:"attached,omitempty"`
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
	// Retained means the volume will not be reclaimed by the cluster.
	Retained bool `json:"retained,omitempty"`
}

// Released reports whether every release condition holds.
func (e ReleaseEvidence) Released() bool { return e.ClaimGone && e.VolumeGone && !e.Attached }

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
// the identity under the same adoption and fence rules as Runtime.Start.
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
	if _, err := r.claim(ctx, t.Namespace, id, spec.Fence); err != nil {
		return runtime.ResourceObservation{}, notReached(err)
	}
	var eerr error
	if spec.Kind == runtime.ResourceWorkspace {
		eerr = r.ensureWorkspace(ctx, t, id)
	} else {
		eerr = r.ensureService(ctx, t, id, *spec.Service, spec.Fence)
	}
	if eerr != nil {
		return runtime.ResourceObservation{}, eerr
	}
	return r.observeResource(ctx, t.Namespace, id, h)
}

// createGuarded issues one create under the in-flight marker and classifies the
// outcome: success and a verified existing object adopt, a definite refusal is
// runtime.ErrNotReached and anything else is uncertain.
func (c *core) createGuarded(ctx context.Context, ns string, id ident, create func() error, readBack func() error) error {
	if err := c.markInflight(ctx, ns, id); err != nil {
		return notReached(err)
	}
	err := create()
	switch {
	case err == nil:
		c.clearInflight(ctx, ns, id)
		return nil
	case errors.Is(err, kubernetes.ErrAlreadyExists):
		if rerr := readBack(); rerr != nil {
			return rerr
		}
		c.clearInflight(ctx, ns, id)
		return nil
	case kubernetes.Definite(err):
		c.clearInflight(ctx, ns, id)
		return notReached(fmt.Errorf("the cluster refused the object: %w", err))
	}
	return uncertain(err)
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

func (c *core) verifyPVC(p kubernetes.PersistentVolumeClaim, id ident) error {
	if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return fmt.Errorf("%w: claim %s", ErrForeign, p.Metadata.Name)
	}
	if !id.digestMatches(p.Metadata.Annotations) {
		return fmt.Errorf("%w: the workspace exists with another specification digest", runtime.ErrSpecMismatch)
	}
	if len(p.Spec.AccessModes) != 1 || p.Spec.AccessModes[0] != "ReadWriteOncePod" {
		return unverified("the claim is not ReadWriteOncePod")
	}
	return nil
}

func (c *core) ensureWorkspace(ctx context.Context, t Target, id ident) error {
	p, found, err := c.getPVC(ctx, t.Namespace, id.base())
	if err != nil {
		return notReached(err)
	}
	if found {
		if err := c.verifyPVC(p, id); err != nil {
			return notReached(err)
		}
		c.clearInflight(ctx, t.Namespace, id)
		return nil
	}
	return c.createGuarded(ctx, t.Namespace, id,
		func() error { _, err := c.cluster.CreatePVC(ctx, t.Namespace, c.workspaceClaim(id, t)); return err },
		func() error {
			p, found, err := c.getPVC(ctx, t.Namespace, id.base())
			if err != nil || !found {
				return uncertain(errors.Join(err, errors.New("the claim exists but cannot be read")))
			}
			if err := c.verifyPVC(p, id); err != nil {
				return notReached(err)
			}
			return nil
		})
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

func (c *core) ensureService(ctx context.Context, t Target, id ident, s runtime.ServiceSpec, fence runtime.Fence) error {
	pod, found, err := c.getPod(ctx, t.Namespace, id.base())
	if err != nil {
		return notReached(err)
	}
	if found {
		if err := c.verifyPod(pod, id); err != nil {
			return notReached(err)
		}
		c.clearInflight(ctx, t.Namespace, id)
	} else if err := c.createGuarded(ctx, t.Namespace, id,
		func() error {
			_, err := c.cluster.CreatePod(ctx, t.Namespace, c.servicePod(id, s, t, fence))
			return err
		},
		func() error {
			p, ok, err := c.getPod(ctx, t.Namespace, id.base())
			if err != nil || !ok {
				return uncertain(errors.Join(err, errors.New("the Pod exists but cannot be read")))
			}
			if err := c.verifyPod(p, id); err != nil {
				return notReached(err)
			}
			return nil
		}); err != nil {
		return err
	}
	if len(s.Ports) == 0 {
		return nil
	}
	// The Pod now exists, so any failure from here is uncertain, not "not reached".
	svc, ok, err := c.getService(ctx, t.Namespace, id.base())
	switch {
	case err != nil:
		return uncertain(err)
	case ok:
		if !id.owns(svc.Metadata.Labels, svc.Metadata.Annotations) {
			return fmt.Errorf("%w: service %s", ErrForeign, svc.Metadata.Name)
		}
		return nil
	}
	_, err = c.cluster.CreateService(ctx, t.Namespace, c.serviceObject(id, s, t))
	if err != nil && !errors.Is(err, kubernetes.ErrAlreadyExists) {
		return uncertain(err)
	}
	return nil
}

// Observe reports the resource by its stable identity.
func (r *Resources) Observe(ctx context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	t, id, err := r.handleTarget(ctx, h)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	return r.observeResource(ctx, t.Namespace, id, h)
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

func (c *core) observeResource(ctx context.Context, ns string, id ident, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	rec, found, err := c.readRecord(ctx, ns, id)
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if id.kind == kindWorkspace {
		return c.observeWorkspace(ctx, ns, id, h, rec, found)
	}
	return c.observeService(ctx, ns, id, h, rec, found)
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
	if err := c.verifyPVC(p, id); err != nil {
		return runtime.ResourceObservation{}, err
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

func (c *core) observeService(ctx context.Context, ns string, id ident, h runtime.ResourceHandle, rec record, found bool) (runtime.ResourceObservation, error) {
	pod, pfound, err := c.getPod(ctx, ns, id.base())
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	if !pfound {
		return c.absent(h, rec, found, runtime.ReasonServiceLost), nil
	}
	if err := c.verifyPod(pod, id); err != nil {
		return runtime.ResourceObservation{}, err
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
		if _, ok, err := c.getService(ctx, ns, id.base()); err != nil {
			return runtime.ResourceObservation{}, err
		} else if !ok {
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
	return r.observeResource(ctx, t.Namespace, id, h)
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
		ev, rec, err = c.deleteWorkspace(ctx, ns, id, rec)
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
	case !rec.quiet(c.opts.Clock(), c.opts.CreateQuiescence):
		return c.robs(h, runtime.ResourceUnknown, runtime.ReasonNone), nil
	}
	if _, err := c.settle(ctx, ns, id, h.Fence, recRemoved); err != nil {
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

// deleteWorkspace deletes the claim under its UID after remembering the volume
// it is bound to, then gathers the release evidence.
func (c *core) deleteWorkspace(ctx context.Context, ns string, id ident, rec record) (ReleaseEvidence, record, error) {
	p, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return ReleaseEvidence{}, rec, err
	}
	if found {
		if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
			return ReleaseEvidence{}, rec, fmt.Errorf("%w: claim %s", ErrForeign, p.Metadata.Name)
		}
		// Remember the volume before the claim goes: the claim is the only
		// pointer to it.
		if v := p.Spec.VolumeName; v != "" && rec.volume != v {
			rec, err = c.mutate(ctx, ns, id, func(r *record) { r.volume = v })
			if err != nil {
				return ReleaseEvidence{}, rec, err
			}
		}
		if p.Metadata.DeletionTimestamp == "" {
			if err := c.cluster.DeletePVC(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
				return ReleaseEvidence{}, rec, fmt.Errorf("kernelrt: deleting the workspace claim: %w", err)
			}
		}
	}
	ev, err := c.evidence(ctx, ns, id, rec)
	return ev, rec, err
}

// evidence reads the release evidence of a workspace.
func (c *core) evidence(ctx context.Context, ns string, id ident, rec record) (ReleaseEvidence, error) {
	_, found, err := c.getPVC(ctx, ns, id.base())
	if err != nil {
		return ReleaseEvidence{}, err
	}
	ev := ReleaseEvidence{ClaimGone: !found, Volume: rec.volume, VolumeGone: rec.volume == ""}
	if rec.volume == "" {
		return ev, nil
	}
	pv, err := c.cluster.GetPV(ctx, rec.volume)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		ev.VolumeGone = true
	case err != nil:
		return ev, fmt.Errorf("kernelrt: reading the volume: %w", err)
	default:
		ev.ReclaimPolicy = pv.Spec.PersistentVolumeReclaimPolicy
		ev.Retained = pv.Spec.PersistentVolumeReclaimPolicy == "Retain" && ev.ClaimGone
	}
	attachments, err := c.cluster.ListVolumeAttachments(ctx)
	if err != nil {
		return ev, fmt.Errorf("kernelrt: listing volume attachments: %w", err)
	}
	for _, a := range attachments {
		if v := a.Spec.Source.PersistentVolumeName; v != nil && *v == rec.volume && a.Status.Attached {
			ev.Attached = true
		}
	}
	return ev, nil
}

// ReleaseEvidence reports the storage release evidence of a workspace handle.
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
	return r.evidence(ctx, t.Namespace, id, rec)
}
