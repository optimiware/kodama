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

// ErrSecretUnavailable: a declared secret could not be materialized into the
// cluster. The cause is deliberately not carried; it could echo a value.
var ErrSecretUnavailable = errors.New("kernelrt: declared secrets are unavailable")

// Runtime runs one step attempt per binding as one Kubernetes Job.
type Runtime struct{ *core }

// Capabilities reports what the adapter enforces.
func (r *Runtime) Capabilities(ctx context.Context) (runtime.CapabilitySet, error) {
	return r.capabilities(), ctx.Err()
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidValue, fmt.Sprintf(format, args...))
}

func (c *core) checkBinding(b runtime.RuntimeBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Backend != c.cfg.Backend {
		return invalidf("binding names backend %q, this is %q", b.Backend, c.cfg.Backend)
	}
	return nil
}

func (c *core) validateStart(req runtime.StartRequest) error {
	b := req.Binding
	if err := c.checkBinding(b); err != nil {
		return err
	}
	if err := req.Spec.Validate(); err != nil {
		return err
	}
	if err := validateEnv(req.Spec.Env); err != nil {
		return err
	}
	if !pinned(req.Spec.Image) {
		return fmt.Errorf("%w: image %q is not pinned by digest", runtime.ErrCapability, req.Spec.Image)
	}
	if err := req.Workspace.Validate(); err != nil {
		return err
	}
	w := req.Workspace
	if w.Kind != runtime.ResourceWorkspace || w.Scope != b.Scope || w.RunID != b.RunID {
		return invalidf("the workspace belongs to another run")
	}
	for _, s := range req.Services {
		if err := s.Validate(); err != nil {
			return err
		}
		if s.Kind != runtime.ResourceService || s.Scope != b.Scope || s.RunID != b.RunID {
			return invalidf("service %s belongs to another run", s.Name)
		}
	}
	m := req.Secrets
	switch {
	case len(m.Refs) == 0 && len(req.Spec.Secrets) == 0:
	case len(m.Refs) == 0:
		return fmt.Errorf("%w: the step declares secrets that were not materialized", ErrSecretUnavailable)
	default:
		if m.Scope != b.Scope || m.RunID != b.RunID || m.StepKey != b.StepKey {
			return invalidf("the secret materialization belongs to another step")
		}
		if err := validateSecretDir(m.Directory); err != nil {
			return err
		}
		for _, ref := range m.Refs {
			if err := ref.Validate(); err != nil {
				return err
			}
			// Only declared references, and only ones this scope may use.
			if !slices.Contains(req.Spec.Secrets, ref) || !ref.Owner.AvailableTo(b.Scope) {
				return invalidf("secret %q is not declared for this step or not available to its scope", ref.Name)
			}
		}
		if c.secrets == nil {
			return fmt.Errorf("%w: no secret source is configured", runtime.ErrCapability)
		}
	}
	return nil
}

func (c *core) getJob(ctx context.Context, ns, name string) (kubernetes.Job, bool, error) {
	j, err := c.cluster.GetJob(ctx, ns, name)
	switch {
	case err == nil:
		return j, true, nil
	case errors.Is(err, kubernetes.ErrNotFound):
		return kubernetes.Job{}, false, nil
	}
	return kubernetes.Job{}, false, fmt.Errorf("kernelrt: reading the Job: %w", err)
}

func uncertain(err error) error { return fmt.Errorf("%w: %w", runtime.ErrUncertain, err) }

// workspaceIdent and serviceIdent derive the identity of a handle's object.
func handleIdent(h runtime.ResourceHandle, backend string) ident {
	return resourceIdent(h.Scope, h.RunID, h.Kind, h.Name, h.Identity, "", backend)
}

// Start launches or adopts the Job of the binding. It is idempotent on the
// workload id: the same specification digest is adopted, another one is
// runtime.ErrSpecMismatch, and a writer older than the identity's record is
// runtime.ErrStaleFence. Every refusal before the create wraps
// runtime.ErrNotReached. The create itself runs under a conditional
// reservation (see guarded): a Start that was paused while a newer writer
// stopped or removed the workload cannot create it, and one whose create lands
// afterwards deletes it again.
func (r *Runtime) Start(ctx context.Context, req runtime.StartRequest) (runtime.WorkloadObservation, error) {
	if err := r.validateStart(req); err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	b := req.Binding
	id := workloadIdent(b)
	t, err := r.admit(ctx, b.Scope, req.Spec.Requires)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	ws := handleIdent(req.Workspace, r.cfg.Backend)
	if err := r.requireWorkspace(ctx, t.Namespace, ws); err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	rec, err := r.claim(ctx, t.Namespace, id, b.Fence)
	if err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	secretsDir := ""
	if len(req.Secrets.Refs) > 0 {
		secretsDir = req.Secrets.Directory
	}
	if rec.workspace != "" && (rec.workspace != ws.base() || rec.secretsDir != secretsDir) {
		return runtime.WorkloadObservation{}, notReached(invalidf("the workload was started against another workspace or secret layout"))
	}
	job, found, err := r.getJob(ctx, t.Namespace, id.base())
	if err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	// The read above may have been preceded by a pause: decide on the record as
	// it is now, and only while this writer still has authority.
	if rec, err = r.current(ctx, t.Namespace, id, b.Fence); err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	if found {
		if err := r.verifyJob(job, id, t, rec); err != nil {
			return runtime.WorkloadObservation{}, notReached(err)
		}
		if err := r.establish(ctx, t.Namespace, id, rec, opJob, job.Metadata.UID); err != nil {
			return runtime.WorkloadObservation{}, notReached(err)
		}
		return r.observe(ctx, t, b)
	}
	if rec.created[opJob] != "" {
		// An established Job vanished. It is never silently recreated: that would
		// run the step's commands a second time. The observation reports it.
		return r.observe(ctx, t, b)
	}

	material := ""
	if len(req.Secrets.Refs) > 0 {
		material, err = r.ensureMaterial(ctx, t.Namespace, id, b.Fence, req.Secrets)
		if err != nil {
			return runtime.WorkloadObservation{}, err
		}
	}
	var svcs []serviceRef
	for _, s := range req.Services {
		svcs = append(svcs, serviceRef{name: s.Name, base: handleIdent(s, r.cfg.Backend).base()})
	}
	j := r.stepJob(id, req, t, ws.base(), material, serviceHostEnv(t.Namespace, svcs))
	err = r.guarded(ctx, t.Namespace, id, guard{
		op: opJob, fence: b.Fence,
		mod: func(rc *record) error {
			if rc.workspace != "" && (rc.workspace != ws.base() || rc.secretsDir != secretsDir) {
				return invalidf("the workload was started against another workspace or secret layout")
			}
			rc.workspace, rc.secretsDir = ws.base(), secretsDir
			return nil
		},
		create: func() (string, error) {
			out, err := r.cluster.CreateJob(ctx, t.Namespace, j)
			return out.Metadata.UID, err
		},
		readBack: func() (string, error) {
			existing, ok, err := r.getJob(ctx, t.Namespace, id.base())
			if err != nil || !ok {
				return "", uncertain(errors.Join(err, errors.New("the Job exists but cannot be read")))
			}
			rc, _, err := r.readRecord(ctx, t.Namespace, id)
			if err != nil {
				return "", uncertain(err)
			}
			if err := r.verifyJob(existing, id, t, rc); err != nil {
				return "", notReached(err)
			}
			return existing.Metadata.UID, nil
		},
		reap: func(uid string) error { return r.reapJob(ctx, t.Namespace, id, uid) },
	})
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	return r.observe(ctx, t, b)
}

// requireWorkspace verifies that the run's workspace exists, is owned by this
// run and is still active before a step is attached to it.
func (c *core) requireWorkspace(ctx context.Context, ns string, ws ident) error {
	rec, found, err := c.readRecord(ctx, ns, ws)
	switch {
	case err != nil:
		return err
	case !found || rec.state != recActive:
		return fmt.Errorf("%w: the workspace is not active", ErrClosed)
	}
	pvc, err := c.cluster.GetPVC(ctx, ns, ws.base())
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return fmt.Errorf("kernelrt: the workspace claim does not exist")
	case err != nil:
		return fmt.Errorf("kernelrt: reading the workspace claim: %w", err)
	case !ws.owns(pvc.Metadata.Labels, pvc.Metadata.Annotations):
		return fmt.Errorf("%w: workspace claim %s", ErrForeign, pvc.Metadata.Name)
	case pvc.Metadata.DeletionTimestamp != "" || pvc.Status.Phase == "Lost":
		return fmt.Errorf("kernelrt: the workspace claim is %s", pvcCondition(pvc))
	}
	return checkInstance(rec, true, opPVC, pvc.Metadata.UID)
}

func pvcCondition(p kubernetes.PersistentVolumeClaim) string {
	if p.Metadata.DeletionTimestamp != "" {
		return "terminating"
	}
	return "lost"
}

// ensureMaterial creates, or reuses, the immutable Secret holding the step's
// declared secret files, under the same reservation protocol as the Job.
func (c *core) ensureMaterial(ctx context.Context, ns string, id ident, fence runtime.Fence, m runtime.Materialization) (string, error) {
	name := id.materialName()
	files, err := c.secrets.Files(ctx, m)
	if err != nil || len(files) == 0 {
		return "", notReached(ErrSecretUnavailable)
	}
	s := kubernetes.Secret{
		Metadata:  kubernetes.ObjectMeta{Name: name, Labels: id.labels(roleMaterial), Annotations: id.annotations()},
		Type:      "Opaque",
		Immutable: ptr(true),
		Data:      files,
	}
	err = c.guarded(ctx, ns, id, guard{
		op: opSecret, fence: fence,
		create: func() (string, error) {
			out, err := c.cluster.CreateSecret(ctx, ns, s)
			return out.Metadata.UID, err
		},
		readBack: func() (string, error) {
			got, err := c.cluster.GetSecret(ctx, ns, name)
			if err != nil {
				return "", uncertain(fmt.Errorf("kernelrt: reading the step secrets: %w", err))
			}
			if !id.owns(got.Metadata.Labels, got.Metadata.Annotations) || got.Metadata.Labels[labelRole] != roleMaterial {
				return "", notReached(fmt.Errorf("%w: secret %s", ErrForeign, name))
			}
			return got.Metadata.UID, nil
		},
		reap: func(uid string) error { return c.reapMaterial(ctx, ns, id, uid) },
	})
	return name, err
}

// Observe reports the workload by its stable identity.
func (r *Runtime) Observe(ctx context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if err := r.checkBinding(b); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	t, err := r.targets.Target(ctx, b.Scope)
	if err != nil {
		return runtime.WorkloadObservation{}, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	return r.observe(ctx, t, b)
}

func (c *core) obs(b runtime.RuntimeBinding, phase runtime.WorkloadPhase, res runtime.Result) runtime.WorkloadObservation {
	return runtime.WorkloadObservation{Binding: b, Phase: phase, Result: res, ObservedAt: c.opts.Clock()}
}

// observe maps the Job, its Pods and the identity's record to a phase. Absent is
// reported only when nothing exists, including Pods whose Job is gone, and no
// create can still land.
func (c *core) observe(ctx context.Context, t Target, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	ns := t.Namespace
	id := workloadIdent(b)
	rec, found, err := c.readRecord(ctx, ns, id)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	job, jfound, err := c.getJob(ctx, ns, id.base())
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	if !jfound {
		// Pods outlive a Job that was deleted without garbage collection; they
		// may still be running the step.
		orphans, err := c.orphanPods(ctx, ns, id)
		if err != nil {
			return runtime.WorkloadObservation{}, err
		}
		if len(orphans) > 0 {
			return c.obs(b, orphanPhase(orphans), runtime.Result{}), nil
		}
		now := c.opts.Clock()
		switch {
		case !found, rec.state == recRemoved:
			return c.obs(b, runtime.PhaseAbsent, runtime.Result{}), nil
		case !rec.quiet(now, c.opts.CreateQuiescence):
			return c.obs(b, runtime.PhaseUnknown, runtime.Result{}), nil
		case rec.state == recStopped:
			return c.obs(b, runtime.PhaseExited, runtime.Result{Code: runtime.ResultCancelled, Reason: runtime.ReasonCancelled}), nil
		}
		return c.obs(b, runtime.PhaseAbsent, runtime.Result{}), nil
	}
	if err := c.verifyJob(job, id, t, rec); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	// Recovery from an uncertain create: record the verified instance before it
	// is reported, so a later loss is not answered by running the step again.
	if found {
		if err := c.establish(ctx, ns, id, rec, opJob, job.Metadata.UID); err != nil {
			return runtime.WorkloadObservation{}, err
		}
	}
	pods, err := c.jobPods(ctx, ns, id, job)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	for _, p := range pods {
		if err := c.verifyJobPod(p, id, t, rec); err != nil {
			return runtime.WorkloadObservation{}, err
		}
	}
	phase, res := jobPhase(job, pods)
	return c.obs(b, phase, res), nil
}

// orphanPods lists the Pods that carry the identity's evidence and name its Job
// as controller, whether or not the Job still exists.
func (c *core) orphanPods(ctx context.Context, ns string, id ident) ([]kubernetes.Pod, error) {
	all, err := c.cluster.ListPods(ctx, ns, labelObject+"="+id.base())
	if err != nil {
		return nil, fmt.Errorf("kernelrt: listing Pods: %w", err)
	}
	return ownedPods(all, id, id.base(), ""), nil
}

// orphanPhase is what Pods without a Job show: running unless every Pod has
// terminated, in which case the result is unknown (there is no Job to confirm
// it). Termination is never inferred from absence of a Job.
func orphanPhase(pods []kubernetes.Pod) runtime.WorkloadPhase {
	for _, p := range pods {
		if p.Metadata.DeletionTimestamp != "" || (p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed") {
			return runtime.PhaseRunning
		}
	}
	return runtime.PhaseUnknown
}

func (c *core) jobPods(ctx context.Context, ns string, id ident, job kubernetes.Job) ([]kubernetes.Pod, error) {
	all, err := c.cluster.ListPods(ctx, ns, labelObject+"="+id.base())
	if err != nil {
		return nil, fmt.Errorf("kernelrt: listing Pods: %w", err)
	}
	return ownedPods(all, id, job.Metadata.Name, job.Metadata.UID), nil
}

// jobPhase maps Job and Pod status to a workload phase. A terminating Job is
// still running: only absence confirms it is gone.
func jobPhase(job kubernetes.Job, pods []kubernetes.Pod) (runtime.WorkloadPhase, runtime.Result) {
	st := job.Status
	if job.Metadata.DeletionTimestamp != "" {
		return runtime.PhaseRunning, runtime.Result{}
	}
	if _, ok := st.Condition("Complete"); ok || st.Succeeded > 0 {
		return runtime.PhaseExited, runtime.Result{Code: runtime.ResultSucceeded}
	}
	failed, hasCond := st.Condition("Failed")
	if hasCond || st.Failed > 0 {
		if hasCond && failed.Reason == "DeadlineExceeded" {
			return runtime.PhaseExited, runtime.Result{Code: runtime.ResultTimedOut, Reason: runtime.ReasonStepTimeout}
		}
		for _, p := range pods {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == containerStep && cs.State.Terminated != nil {
					t := cs.State.Terminated
					if t.ExitCode == 0 {
						return runtime.PhaseExited, runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}
					}
					return runtime.PhaseExited, runtime.Result{Code: runtime.ResultFailed, ExitCode: int(t.ExitCode), Reason: runtime.ReasonCommandFailed}
				}
			}
		}
		return runtime.PhaseExited, runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}
	}
	return runtime.PhaseRunning, runtime.Result{}
}

// Logs returns a bounded snapshot of the step container's output. Follow is
// satisfied by the snapshot: the Kubernetes adapter does not stream.
func (r *Runtime) Logs(ctx context.Context, b runtime.RuntimeBinding, _ runtime.LogOptions) (io.ReadCloser, error) {
	if err := r.checkBinding(b); err != nil {
		return nil, err
	}
	t, err := r.targets.Target(ctx, b.Scope)
	if err != nil {
		return nil, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	id := workloadIdent(b)
	job, found, err := r.getJob(ctx, t.Namespace, id.base())
	if err != nil {
		return nil, err
	}
	var pods []kubernetes.Pod
	switch {
	case found:
		if !id.owns(job.Metadata.Labels, job.Metadata.Annotations) {
			return nil, fmt.Errorf("%w: job %s", ErrForeign, job.Metadata.Name)
		}
		pods, err = r.jobPods(ctx, t.Namespace, id, job)
	default:
		pods, err = r.orphanPods(ctx, t.Namespace, id)
	}
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "the workload has no Pod"}
	}
	data, err := r.cluster.PodLog(ctx, t.Namespace, pods[0].Metadata.Name, containerStep)
	if err != nil {
		return nil, fmt.Errorf("kernelrt: reading the log: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Stop fences the identity (a stale writer is refused and nothing changes),
// then terminates the workload: a Job that has not exited is deleted, and Pods
// whose Job is gone are deleted too. A Job that already exited is kept so its
// result stays observable until Remove.
func (r *Runtime) Stop(ctx context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if err := r.checkBinding(b); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	t, err := r.targets.Target(ctx, b.Scope)
	if err != nil {
		return runtime.WorkloadObservation{}, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	id := workloadIdent(b)
	if _, err := r.settle(ctx, t.Namespace, id, b.Fence, recStopped); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	job, found, err := r.getJob(ctx, t.Namespace, id.base())
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	if found {
		if !id.owns(job.Metadata.Labels, job.Metadata.Annotations) {
			return runtime.WorkloadObservation{}, fmt.Errorf("%w: job %s", ErrForeign, job.Metadata.Name)
		}
		pods, err := r.jobPods(ctx, t.Namespace, id, job)
		if err != nil {
			return runtime.WorkloadObservation{}, err
		}
		if phase, _ := jobPhase(job, pods); phase == runtime.PhaseRunning && job.Metadata.DeletionTimestamp == "" {
			if err := r.deleteJob(ctx, t.Namespace, job); err != nil {
				return runtime.WorkloadObservation{}, err
			}
		}
	} else if err := r.deleteOrphanPods(ctx, t.Namespace, id); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	return r.observe(ctx, t, b)
}

// deleteJob deletes the Job the caller just read, under its UID.
func (c *core) deleteJob(ctx context.Context, ns string, job kubernetes.Job) error {
	err := c.cluster.DeleteJob(ctx, ns, job.Metadata.Name, kubernetes.DeleteOptions{UID: job.Metadata.UID, Propagation: "Foreground"})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the Job: %w", err)
	}
	return nil
}

// deleteOrphanPods deletes, under their UIDs, the Pods the identity still owns
// that are not already terminating.
func (c *core) deleteOrphanPods(ctx context.Context, ns string, id ident) error {
	pods, err := c.orphanPods(ctx, ns, id)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		if err := c.cluster.DeletePod(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Foreground"}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return fmt.Errorf("kernelrt: deleting a Pod: %w", err)
		}
	}
	return nil
}

// Remove stops the workload, deletes the Job, its Pods and its step secrets,
// and confirms with PhaseAbsent only when nothing is left and no create can
// still land. Until then it reports the workload still present or unknown, and
// the caller repeats it.
func (r *Runtime) Remove(ctx context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if err := r.checkBinding(b); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	t, err := r.targets.Target(ctx, b.Scope)
	if err != nil {
		return runtime.WorkloadObservation{}, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	return r.removeWorkload(ctx, t.Namespace, workloadIdent(b), b)
}

func (c *core) removeWorkload(ctx context.Context, ns string, id ident, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if _, err := c.settle(ctx, ns, id, b.Fence, recStopped); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	remaining, err := c.deleteWorkloadObjects(ctx, ns, id)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	if remaining {
		return c.obs(b, runtime.PhaseRunning, runtime.Result{}), nil
	}
	// The closing write re-reads the record: a create reserved since the delete
	// above keeps the identity open.
	if _, err := c.settle(ctx, ns, id, b.Fence, recRemoved); err != nil {
		if errors.Is(err, errNotQuiet) {
			return c.obs(b, runtime.PhaseUnknown, runtime.Result{}), nil
		}
		return runtime.WorkloadObservation{}, err
	}
	return c.obs(b, runtime.PhaseAbsent, runtime.Result{}), nil
}

// deleteWorkloadObjects deletes what the identity owns, re-reading each object
// and deleting it under its UID, then reports whether anything remains.
func (c *core) deleteWorkloadObjects(ctx context.Context, ns string, id ident) (remaining bool, err error) {
	job, found, err := c.getJob(ctx, ns, id.base())
	if err != nil {
		return false, err
	}
	if found {
		if !id.owns(job.Metadata.Labels, job.Metadata.Annotations) {
			return false, fmt.Errorf("%w: job %s", ErrForeign, job.Metadata.Name)
		}
		if job.Metadata.DeletionTimestamp == "" {
			if err := c.deleteJob(ctx, ns, job); err != nil {
				return false, err
			}
		}
	}
	// Pods may outlive their Job; the name of the owner is enough evidence
	// together with the identity's labels and annotations.
	if err := c.deleteOrphanPods(ctx, ns, id); err != nil {
		return false, err
	}
	if err := c.deleteMaterial(ctx, ns, id); err != nil {
		return false, err
	}

	if _, found, err := c.getJob(ctx, ns, id.base()); err != nil || found {
		return true, err
	}
	pods, err := c.orphanPods(ctx, ns, id)
	return len(pods) > 0, err
}

func (c *core) deleteMaterial(ctx context.Context, ns string, id ident) error {
	s, err := c.cluster.GetSecret(ctx, ns, id.materialName())
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("kernelrt: reading the step secrets: %w", err)
	case !id.owns(s.Metadata.Labels, s.Metadata.Annotations) || s.Metadata.Labels[labelRole] != roleMaterial:
		return fmt.Errorf("%w: secret %s", ErrForeign, s.Metadata.Name)
	}
	err = c.cluster.DeleteSecret(ctx, ns, s.Metadata.Name, kubernetes.DeleteOptions{UID: s.Metadata.UID})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the step secrets: %w", err)
	}
	return nil
}

// reapJob deletes the Job with exactly this UID, and the Pods that Job controls.
// A different instance under the name is never touched.
func (c *core) reapJob(ctx context.Context, ns string, id ident, uid string) error {
	job, found, err := c.getJob(ctx, ns, id.base())
	if err != nil || !found || job.Metadata.UID != uid {
		return err
	}
	if !id.owns(job.Metadata.Labels, job.Metadata.Annotations) {
		return fmt.Errorf("%w: job %s", ErrForeign, job.Metadata.Name)
	}
	if job.Metadata.DeletionTimestamp == "" {
		if err := c.deleteJob(ctx, ns, job); err != nil {
			return err
		}
	}
	pods, err := c.jobPods(ctx, ns, id, job)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		if err := c.cluster.DeletePod(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Foreground"}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return fmt.Errorf("kernelrt: deleting a Pod: %w", err)
		}
	}
	return nil
}

// reapMaterial deletes the step Secret with exactly this UID.
func (c *core) reapMaterial(ctx context.Context, ns string, id ident, uid string) error {
	s, err := c.cluster.GetSecret(ctx, ns, id.materialName())
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("kernelrt: reading the step secrets: %w", err)
	case s.Metadata.UID != uid || !id.owns(s.Metadata.Labels, s.Metadata.Annotations):
		return nil
	}
	if err := c.cluster.DeleteSecret(ctx, ns, s.Metadata.Name, kubernetes.DeleteOptions{UID: uid}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the step secrets: %w", err)
	}
	return nil
}
