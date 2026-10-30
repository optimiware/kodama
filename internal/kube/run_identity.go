package kube

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/execution"
)

// ErrPodNotOwned: the Pod a workload claims is not provably controlled by the
// resource's committed workload (seam S6). It is a definite answer; every other
// failure of VerifyResourcePod is an outage that concludes nothing.
var ErrPodNotOwned = errors.New("kube: the Pod does not belong to the resource's workload")

// VerifyResourcePod confirms that a Pod a resource workload claims for itself is
// the Pod of the resource's committed workload (seam S6): in the tenant's
// namespace, controlled by the Job whose name and UID execution bound for the
// resource, with the Pod's own UID as claimed. The claimed name and UIDs are
// evidence only; the namespace and the Job come from the committed handle. Only a
// preparation has a Job-owned Pod: a service's workload is its Pod, so a
// service has nothing to confirm and is refused. Absence and mismatch are
// ErrPodNotOwned; an unreadable cluster is ErrUnavailable.
//
// T-116 bridges this to runtimeauth's consumer-owned ResourceWorkloads port.
func (r *RunResources) VerifyResourcePod(ctx context.Context, h execution.ResourceHandle, podName, podUID string) error {
	tgt, err := r.handleTarget(h)
	if err != nil {
		return err
	}
	notOwned := func(why string) error { return wrap(ErrPodNotOwned, "%s", why) }
	if h.Kind != execution.KindPreparation {
		return notOwned("only a Job-owned workload has a Pod to confirm")
	}
	if h.UID == "" || podName == "" || podUID == "" {
		return notOwned("the workload or the Pod is not identified")
	}
	ns := tgt.Namespace
	j, ok, err := r.getJob(ctx, ns, h.Name)
	switch {
	case err != nil:
		return err
	case !ok:
		return notOwned("the resource's Job does not exist")
	}
	if err := checkOwned(j.Metadata, ns, h.Name, ownedOf(h, h.UID)); err != nil {
		return notOwned("the Job is not the resource's bound instance")
	}
	p, err := r.cl.GetPod(ctx, ns, podName)
	switch {
	case isNotFound(err):
		return notOwned("the Pod does not exist")
	case err != nil:
		return unavailable(err, "reading the Pod")
	}
	if p.Metadata.Namespace != ns || p.Metadata.Name != podName || p.Metadata.UID != podUID {
		return notOwned("the Pod is not the claimed instance")
	}
	if !controlledBy(p, h.Name, h.UID) || p.Metadata.Labels[LabelResourceOwner] != ValueResourceOwner ||
		p.Metadata.Labels[LabelRunKey] != RunKey(h.Scope, h.RunID) {
		return notOwned("the Pod is not controlled by the resource's Job")
	}
	return nil
}
