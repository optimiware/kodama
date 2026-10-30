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
// runtime.ErrNotReached.
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
	if _, err := r.claim(ctx, t.Namespace, id, b.Fence); err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	job, found, err := r.getJob(ctx, t.Namespace, id.base())
	if err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	if found {
		if err := r.verifyJob(job, id); err != nil {
			return runtime.WorkloadObservation{}, notReached(err)
		}
		r.clearInflight(ctx, t.Namespace, id)
		return r.observe(ctx, t.Namespace, b)
	}

	material := ""
	if len(req.Secrets.Refs) > 0 {
		material, err = r.ensureMaterial(ctx, t.Namespace, id, req.Secrets)
		if err != nil {
			return runtime.WorkloadObservation{}, notReached(err)
		}
	}
	var svcs []serviceRef
	for _, s := range req.Services {
		svcs = append(svcs, serviceRef{name: s.Name, base: handleIdent(s, r.cfg.Backend).base()})
	}
	j := r.stepJob(id, req, t, ws.base(), material, serviceHostEnv(t.Namespace, svcs))

	// The in-flight marker precedes the create: a create whose answer is lost
	// must never later be mistaken for absence.
	if err := r.markInflight(ctx, t.Namespace, id); err != nil {
		return runtime.WorkloadObservation{}, notReached(err)
	}
	_, err = r.cluster.CreateJob(ctx, t.Namespace, j)
	switch {
	case err == nil:
		r.clearInflight(ctx, t.Namespace, id)
	case errors.Is(err, kubernetes.ErrAlreadyExists):
		existing, ok, rerr := r.getJob(ctx, t.Namespace, id.base())
		if rerr != nil || !ok {
			return runtime.WorkloadObservation{}, uncertain(errors.Join(err, rerr))
		}
		if verr := r.verifyJob(existing, id); verr != nil {
			r.clearInflight(ctx, t.Namespace, id)
			return runtime.WorkloadObservation{}, notReached(verr)
		}
		r.clearInflight(ctx, t.Namespace, id)
	case kubernetes.Definite(err):
		r.clearInflight(ctx, t.Namespace, id)
		return runtime.WorkloadObservation{}, notReached(fmt.Errorf("the cluster refused the Job: %w", err))
	default:
		return runtime.WorkloadObservation{}, uncertain(err)
	}
	return r.observe(ctx, t.Namespace, b)
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
	return nil
}

func pvcCondition(p kubernetes.PersistentVolumeClaim) string {
	if p.Metadata.DeletionTimestamp != "" {
		return "terminating"
	}
	return "lost"
}

// ensureMaterial creates, or reuses, the immutable Secret holding the step's
// declared secret files.
func (c *core) ensureMaterial(ctx context.Context, ns string, id ident, m runtime.Materialization) (string, error) {
	name := id.materialName()
	files, err := c.secrets.Files(ctx, m)
	if err != nil || len(files) == 0 {
		return "", ErrSecretUnavailable
	}
	s := kubernetes.Secret{
		Metadata:  kubernetes.ObjectMeta{Name: name, Labels: id.labels(roleMaterial), Annotations: id.annotations()},
		Type:      "Opaque",
		Immutable: ptr(true),
		Data:      files,
	}
	_, err = c.cluster.CreateSecret(ctx, ns, s)
	switch {
	case err == nil:
		return name, nil
	case errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
		got, gerr := c.cluster.GetSecret(ctx, ns, name)
		if gerr != nil {
			return "", fmt.Errorf("kernelrt: reading the step secrets: %w", gerr)
		}
		if !id.owns(got.Metadata.Labels, got.Metadata.Annotations) || got.Metadata.Labels[labelRole] != roleMaterial {
			return "", fmt.Errorf("%w: secret %s", ErrForeign, name)
		}
		return name, nil
	}
	return "", fmt.Errorf("kernelrt: creating the step secrets: %w", err)
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
	return r.observe(ctx, t.Namespace, b)
}

func (c *core) obs(b runtime.RuntimeBinding, phase runtime.WorkloadPhase, res runtime.Result) runtime.WorkloadObservation {
	return runtime.WorkloadObservation{Binding: b, Phase: phase, Result: res, ObservedAt: c.opts.Clock()}
}

// observe maps the Job, its Pods and the identity's record to a phase. Absent is
// reported only when nothing exists and no create can still be in flight.
func (c *core) observe(ctx context.Context, ns string, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
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
	if err := c.verifyJob(job, id); err != nil {
		return runtime.WorkloadObservation{}, err
	}
	pods, err := c.jobPods(ctx, ns, id, job)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	phase, res := jobPhase(job, pods)
	return c.obs(b, phase, res), nil
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
	if !found {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "the workload has no Job"}
	}
	if !id.owns(job.Metadata.Labels, job.Metadata.Annotations) {
		return nil, fmt.Errorf("%w: job %s", ErrForeign, job.Metadata.Name)
	}
	pods, err := r.jobPods(ctx, t.Namespace, id, job)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "the workload has no Pod yet"}
	}
	data, err := r.cluster.PodLog(ctx, t.Namespace, pods[0].Metadata.Name, containerStep)
	if err != nil {
		return nil, fmt.Errorf("kernelrt: reading the log: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Stop fences the identity (a stale writer is refused and nothing changes),
// then deletes a Job that has not exited. A Job that already exited is kept so
// its result stays observable until Remove.
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
	}
	return r.observe(ctx, t.Namespace, b)
}

// deleteJob deletes the Job the caller just read, under its UID.
func (c *core) deleteJob(ctx context.Context, ns string, job kubernetes.Job) error {
	err := c.cluster.DeleteJob(ctx, ns, job.Metadata.Name, kubernetes.DeleteOptions{UID: job.Metadata.UID, Propagation: "Foreground"})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
		return fmt.Errorf("kernelrt: deleting the Job: %w", err)
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
	rec, err := c.settle(ctx, ns, id, b.Fence, recStopped)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	remaining, err := c.deleteWorkloadObjects(ctx, ns, id)
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	if remaining {
		return c.obs(b, runtime.PhaseRunning, runtime.Result{}), nil
	}
	if !rec.quiet(c.opts.Clock(), c.opts.CreateQuiescence) {
		return c.obs(b, runtime.PhaseUnknown, runtime.Result{}), nil
	}
	if _, err := c.settle(ctx, ns, id, b.Fence, recRemoved); err != nil {
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
	pods, err := c.cluster.ListPods(ctx, ns, labelObject+"="+id.base())
	if err != nil {
		return false, fmt.Errorf("kernelrt: listing Pods: %w", err)
	}
	for _, p := range ownedPods(pods, id, id.base(), "") {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		if err := c.cluster.DeletePod(ctx, ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Foreground"}); err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
			return false, fmt.Errorf("kernelrt: deleting a Pod: %w", err)
		}
	}
	if err := c.deleteMaterial(ctx, ns, id); err != nil {
		return false, err
	}

	if _, found, err := c.getJob(ctx, ns, id.base()); err != nil || found {
		return true, err
	}
	pods, err = c.cluster.ListPods(ctx, ns, labelObject+"="+id.base())
	if err != nil {
		return false, fmt.Errorf("kernelrt: listing Pods: %w", err)
	}
	return len(ownedPods(pods, id, id.base(), "")) > 0, nil
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
