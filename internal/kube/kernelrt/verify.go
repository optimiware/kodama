package kernelrt

import (
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func unverified(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnverified, fmt.Sprintf(format, args...))
}

func eq[T comparable](p *T, want T) bool { return p != nil && *p == want }

// verifyContainer checks the restricted shape every container of this adapter
// has: pinned image, bounded resources, non-root, no escalation, no added
// capabilities, RuntimeDefault seccomp.
func (c *core) verifyContainer(ct kubernetes.Container) error {
	if !pinned(ct.Image) {
		return unverified("container %s image is not pinned", ct.Name)
	}
	sc := ct.SecurityContext
	switch {
	case sc == nil:
		return unverified("container %s has no security context", ct.Name)
	case !eq(sc.Privileged, false) || !eq(sc.AllowPrivilegeEscalation, false):
		return unverified("container %s may escalate", ct.Name)
	case !eq(sc.RunAsNonRoot, true) || !eq(sc.RunAsUser, c.cfg.RunAsUser) || !eq(sc.RunAsGroup, c.cfg.RunAsGroup):
		return unverified("container %s is not the configured non-root identity", ct.Name)
	case sc.Capabilities == nil || len(sc.Capabilities.Add) > 0 || !slices.Equal(sc.Capabilities.Drop, []string{"ALL"}):
		return unverified("container %s capabilities are not dropped", ct.Name)
	case sc.SeccompProfile == nil || sc.SeccompProfile.Type != "RuntimeDefault":
		return unverified("container %s seccomp is not RuntimeDefault", ct.Name)
	case sc.ProcMount != "":
		return unverified("container %s sets procMount", ct.Name)
	}
	r := ct.Resources
	if r == nil {
		return unverified("container %s has no resource bounds", ct.Name)
	}
	for _, k := range []string{"cpu", "memory", "ephemeral-storage"} {
		if r.Requests[k] == "" || r.Limits[k] == "" {
			return unverified("container %s lacks a %s request or limit", ct.Name, k)
		}
	}
	return nil
}

func (c *core) verifyPodSpec(s kubernetes.PodSpec, container string) error {
	switch {
	case s.HostNetwork || s.HostPID || s.HostIPC:
		return unverified("the Pod uses a host namespace")
	case !eq(s.AutomountServiceAccountToken, false):
		return unverified("the Pod mounts a service account token")
	case s.RestartPolicy != "Never":
		return unverified("the Pod restarts containers")
	case len(s.InitContainers) != 0 || len(s.Containers) != 1 || s.Containers[0].Name != container:
		return unverified("the Pod has unexpected containers")
	case s.NodeName != "":
		return unverified("the Pod is bound to a node")
	case s.SecurityContext == nil || !eq(s.SecurityContext.RunAsNonRoot, true) || !eq(s.SecurityContext.RunAsUser, c.cfg.RunAsUser):
		return unverified("the Pod is not non-root")
	}
	return c.verifyContainer(s.Containers[0])
}

// verifyJob checks the live Job: owned by id, the single-attempt shape and the
// restricted Pod. Ownership is checked first and reported as ErrForeign.
func (c *core) verifyJob(j kubernetes.Job, id ident) error {
	if !id.owns(j.Metadata.Labels, j.Metadata.Annotations) {
		return fmt.Errorf("%w: job %s", ErrForeign, j.Metadata.Name)
	}
	if !id.digestMatches(j.Metadata.Annotations) {
		return fmt.Errorf("%w: the workload exists with another specification digest", runtime.ErrSpecMismatch)
	}
	sp := j.Spec
	if !eq(sp.Parallelism, 1) || !eq(sp.Completions, 1) || !eq(sp.BackoffLimit, 0) {
		return unverified("the Job is not a single attempt")
	}
	return c.verifyPodSpec(sp.Template.Spec, containerStep)
}

func (c *core) verifyPod(p kubernetes.Pod, id ident) error {
	if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
		return fmt.Errorf("%w: pod %s", ErrForeign, p.Metadata.Name)
	}
	if !id.digestMatches(p.Metadata.Annotations) {
		return fmt.Errorf("%w: the service exists with another specification digest", runtime.ErrSpecMismatch)
	}
	return c.verifyPodSpec(p.Spec, containerSvc)
}

// ownedPods keeps the Pods whose controlling owner is the named Job (any UID
// when jobUID is empty, for Pods whose Job is already gone) and that carry the
// identity's ownership evidence.
func ownedPods(pods []kubernetes.Pod, id ident, jobName, jobUID string) []kubernetes.Pod {
	var out []kubernetes.Pod
	for _, p := range pods {
		if !id.owns(p.Metadata.Labels, p.Metadata.Annotations) {
			continue
		}
		for _, o := range p.Metadata.OwnerReferences {
			if o.Controlled() && o.Kind == "Job" && o.APIVersion == "batch/v1" && o.Name == jobName && (jobUID == "" || o.UID == jobUID) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
