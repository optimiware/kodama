package kube

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// JobObserver implements runtimeauth.JobObserver: it answers whether the Job and
// Pod an entrypoint claims are the attempt's, from the authoritative namespace
// and committed attempt data. Claimed names and UIDs are evidence only; the
// namespace and expected Job name come from execution state, and the Pod must be
// controlled by that Job (immutable UIDs).
//
// It returns runtimeauth.ErrNotObserved only for definite absence. A Job that
// exists but is not the attempt's workload (foreign owner, changed spec, forged
// fingerprint) yields evidence that cannot match any claim, so admission is
// forbidden rather than unavailable.
type JobObserver struct{ *core }

var _ runtimeauth.JobObserver = (*JobObserver)(nil)

// NewJobObserver validates the dependencies; no registrar is needed.
func NewJobObserver(d Dependencies, o Options) (*JobObserver, error) {
	c, err := newCore(d, o, false)
	if err != nil {
		return nil, err
	}
	return &JobObserver{c}, nil
}

// ObserveJob implements runtimeauth.JobObserver.
func (o *JobObserver) ObserveJob(ctx context.Context, obs runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	tgt, err := o.policy.Tenant(obs.Scope.TenantID)
	if err != nil {
		return runtimeauth.JobEvidence{}, err
	}
	// The expected namespace must be the tenant's: a mismatch is reported as
	// evidence that cannot match, never looked up elsewhere.
	if obs.Expected.Namespace != tgt.Namespace {
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	}
	rec, err := o.attemptOf(ctx, obs.Scope, obs.AttemptID)
	if err != nil {
		return runtimeauth.JobEvidence{}, err
	}
	if obs.Expected.Name != rec.JobName {
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	}
	job, found, err := o.readJob(ctx, tgt.Namespace, rec.JobName)
	if err != nil {
		return runtimeauth.JobEvidence{}, err
	}
	if !found {
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	}
	exp, err := o.policy.Expect(rec)
	if err != nil {
		return runtimeauth.JobEvidence{}, err
	}
	v, err := VerifyJob(exp, job)
	if err != nil {
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	}
	// A Job whose UID conflicts with a recorded one is a same-name replacement,
	// not the attempt's workload. While no UID has been recorded yet (initial
	// create) the live, verified Job is accepted.
	if rec.ObservedJobUID != "" && rec.ObservedJobUID != v.uid {
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	}
	a, err := o.loadAdmission(ctx, exp.Identity)
	switch {
	case errors.Is(err, ErrForeignWorkload):
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	case err != nil:
		return runtimeauth.JobEvidence{}, err
	}
	if a != nil && a.wl.JobUID != "" && a.wl.JobUID != v.uid {
		return runtimeauth.JobEvidence{Namespace: tgt.Namespace}, nil
	}
	ev := runtimeauth.JobEvidence{Namespace: tgt.Namespace, JobName: rec.JobName, JobUID: v.uid}
	if obs.ClaimedPodName == "" {
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	}
	pod, err := o.cluster.GetPod(ctx, tgt.Namespace, obs.ClaimedPodName)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	case err != nil:
		return runtimeauth.JobEvidence{}, wrapCause(ErrUnavailable, err, "reading the Pod")
	}
	ev.PodNamespace, ev.PodName, ev.PodUID = pod.Metadata.Namespace, pod.Metadata.Name, pod.Metadata.UID
	for _, ref := range pod.Metadata.OwnerReferences {
		if ref.Controlled() {
			ev.PodOwnerUID = ref.UID
		}
	}
	// The Pod must also be the Job controller's Pod for this Job: its labels and
	// container images must match what the Job template declares, so a Pod made
	// by hand with an owner reference is not accepted.
	if !podMatchesTemplate(exp, pod, v.uid) {
		ev.PodOwnerUID = ""
	}
	return ev, nil
}

// podMatchesTemplate checks a Pod against the expected template: controlling
// owner, controller labels, ownership labels and the exact container images.
func podMatchesTemplate(exp Expectation, pod kubernetes.Pod, jobUID string) bool {
	if !controlledBy(pod, exp.Identity.JobName, jobUID) {
		return false
	}
	if pod.Metadata.Labels[PodLabelJobUID] != jobUID || pod.Metadata.Labels[PodLabelJobName] != exp.Identity.JobName {
		return false
	}
	for k, v := range exp.Identity.Labels() {
		if pod.Metadata.Labels[k] != v {
			return false
		}
	}
	tpl := exp.Job.Spec.Template.Spec
	if len(pod.Spec.Containers) != len(tpl.Containers) || len(pod.Spec.InitContainers) != len(tpl.InitContainers) {
		return false
	}
	for i, c := range tpl.Containers {
		if pod.Spec.Containers[i].Image != c.Image || pod.Spec.Containers[i].Name != c.Name {
			return false
		}
	}
	for i, c := range tpl.InitContainers {
		if pod.Spec.InitContainers[i].Image != c.Image || pod.Spec.InitContainers[i].Name != c.Name {
			return false
		}
	}
	return pod.Spec.ServiceAccountName == tpl.ServiceAccountName && !pod.Spec.HostNetwork && !pod.Spec.HostPID && !pod.Spec.HostIPC
}
