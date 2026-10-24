package kube

import (
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Reasons the runtime reports for a workload that ended without a result. They
// are content-free ReasonCodes; execution classifies and settles them.
const (
	ReasonWorkloadFailed                    = execution.ReasonWorkloadFailed
	ReasonWorkloadLost                      = execution.ReasonWorkloadLost
	ReasonWorkloadReplaced execution.Reason = "workload_replaced"
)

func containerStatus(list []kubernetes.ContainerStatus, name string) (kubernetes.ContainerStatus, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return kubernetes.ContainerStatus{}, false
}

// primaryStarted reports whether the primary container was ever started. Launch
// is conservative evidence that work may have begun, not proof of any effect:
// it is true for a running container, a terminated one, or one with a previous
// termination.
func primaryStarted(p kubernetes.Pod) bool {
	s, ok := containerStatus(p.Status.ContainerStatuses, PrimaryContainer)
	if !ok {
		return false
	}
	return (s.Started != nil && *s.Started) || s.State.Running != nil || s.State.Terminated != nil || s.LastState.Terminated != nil
}

// podTerminal reports whether the Pod can never start another container: its
// phase is Failed or Succeeded and restarts are disabled (restartPolicy Never).
func podTerminal(p kubernetes.Pod) bool {
	return (p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded") && p.Spec.RestartPolicy == "Never"
}

// initFailed reports whether the injector init container terminated non-zero:
// with restartPolicy Never the Pod then fails permanently and the primary
// container cannot start.
func initFailed(p kubernetes.Pod) bool {
	s, ok := containerStatus(p.Status.InitContainerStatuses, InjectContainer)
	return ok && s.State.Terminated != nil && s.State.Terminated.ExitCode != 0
}

// launchOf derives launch evidence from the Pods controlled by the Job. Started
// is any started primary container; NotStarted only when affirmative: every Pod
// is terminal and none ever started its primary container. Everything else, in
// particular a Pod that is merely pending, unschedulable or failing to pull
// (kubelet keeps retrying), is unknown.
func launchOf(pods []kubernetes.Pod) execution.LaunchEvidence {
	if len(pods) == 0 {
		return execution.LaunchUnknown
	}
	for _, p := range pods {
		if primaryStarted(p) {
			return execution.LaunchStarted
		}
	}
	for _, p := range pods {
		if !podTerminal(p) || !initFailed(p) {
			return execution.LaunchUnknown
		}
	}
	return execution.LaunchNotStarted
}

// Assessment is what one read of a verified Job and its Pods shows.
type Assessment struct {
	Pods []kubernetes.Pod
	// Started: some controlled Pod's primary container started.
	Started bool
	// Anomaly is non-empty when the workload ended or was replaced without a
	// result: ReasonWorkloadFailed/Lost/Replaced. The launch evidence belongs to
	// it.
	Failure execution.Reason
	Launch  execution.LaunchEvidence
	// Complete is true when the Job reports completion. It never establishes
	// success: only an accepted result does.
	Complete bool
}

// assess classifies the controlled Pods of a verified Job. everStarted is the
// started evidence execution or the workload record already holds: a Pod that
// started and is no longer observable (deleted, node lost, replaced) is lost,
// and a second Pod is a replacement, never a retry.
func assess(job kubernetes.Job, pods []kubernetes.Pod, everStarted bool) Assessment {
	a := Assessment{Pods: pods, Launch: launchOf(pods)}
	_, a.Complete = job.Status.Condition("Complete")
	for _, p := range pods {
		if primaryStarted(p) {
			a.Started = true
		}
	}
	switch {
	case len(pods) > 1:
		a.Failure = ReasonWorkloadReplaced
	case jobFailed(job):
		a.Failure = ReasonWorkloadFailed
	case len(pods) == 1 && pods[0].Status.Phase == "Failed":
		a.Failure = ReasonWorkloadFailed
	case everStarted && !a.Started && !a.Complete && job.Metadata.DeletionTimestamp == "":
		a.Failure = ReasonWorkloadLost
	}
	if a.Failure != "" && everStarted && a.Launch == execution.LaunchUnknown {
		a.Launch = execution.LaunchStarted
	}
	return a
}

func jobFailed(j kubernetes.Job) bool {
	_, failed := j.Status.Condition("Failed")
	return failed
}
