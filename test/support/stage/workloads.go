package stage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/kube"
)

// Job is one workload Job as the cluster reports it.
type Job struct {
	Name        string
	UID         string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Active      int
	Succeeded   int
	Failed      int
	Created     time.Time
}

// Attempt is the attempt the Job carries.
func (j Job) Attempt() string { return j.Annotations[kube.AnnAttempt] }

// Tenant is the tenant the Job carries.
func (j Job) Tenant() string { return j.Annotations[kube.AnnTenant] }

type jobList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			UID               string            `json:"uid"`
			Labels            map[string]string `json:"labels"`
			Annotations       map[string]string `json:"annotations"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Active    int `json:"active"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"status"`
	} `json:"items"`
}

// Jobs lists the Jobs of a namespace.
func (i *Installation) Jobs(ctx context.Context, namespace string) ([]Job, error) {
	var l jobList
	if err := i.Kube.JSON(ctx, &l, "-n", namespace, "get", "jobs"); err != nil {
		return nil, err
	}
	var out []Job
	for _, it := range l.Items {
		out = append(out, Job{Name: it.Metadata.Name, UID: it.Metadata.UID, Namespace: namespace, Labels: it.Metadata.Labels, Annotations: it.Metadata.Annotations,
			Active: it.Status.Active, Succeeded: it.Status.Succeeded, Failed: it.Status.Failed, Created: it.Metadata.CreationTimestamp})
	}
	return out, nil
}

// JobsOfAttempt lists the Jobs of a namespace that carry the attempt.
func (i *Installation) JobsOfAttempt(namespace, attempt string) []Job {
	jobs, err := i.Jobs(context.Background(), namespace)
	if err != nil {
		return nil
	}
	var out []Job
	for _, j := range jobs {
		if j.Attempt() == attempt {
			out = append(out, j)
		}
	}
	return out
}

// WaitJob waits until a Job of the attempt exists (and, with running, has an active Pod).
func (i *Installation) WaitJob(namespace, attempt string, running bool, within time.Duration) Job {
	i.T.Helper()
	var found Job
	WaitForEvery(i.T, "the Job of "+attempt, within, time.Second, func() bool {
		for _, j := range i.JobsOfAttempt(namespace, attempt) {
			if !running || j.Active > 0 {
				found = j
				return true
			}
		}
		return false
	})
	return found
}

// Pod is a workload Pod.
type Pod struct {
	Name  string
	UID   string
	Phase string
	Image string
}

// PodsOfJob lists the Pods the Job's controller created.
func (i *Installation) PodsOfJob(ctx context.Context, namespace, job string) ([]Pod, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := i.Kube.JSON(ctx, &l, "-n", namespace, "get", "pods", "-l", "batch.kubernetes.io/job-name="+job); err != nil {
		return nil, err
	}
	var out []Pod
	for _, it := range l.Items {
		p := Pod{Name: it.Metadata.Name, UID: it.Metadata.UID, Phase: it.Status.Phase}
		for _, c := range it.Spec.Containers {
			if c.Name == kube.PrimaryContainer {
				p.Image = c.Image
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// WaitRemoved waits until no Job of the attempt exists and the removal is durably recorded.
func (w *World) WaitRemoved(attempt string) {
	t := w.I.T
	t.Helper()
	ns := w.Namespace(w.TenantA)
	WaitForEvery(t, "the workload of "+attempt+" to be removed", 3*time.Minute, time.Second, func() bool {
		if len(w.I.JobsOfAttempt(ns, attempt)) != 0 {
			return false
		}
		n, err := w.I.TryOwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND workload_removed_at IS NOT NULL`, attempt)
		return err == nil && n == 1
	})
}

// Namespace is the execution namespace of a tenant of the session.
func (w *World) Namespace(tenant string) string {
	for _, t := range w.I.S.Tenants {
		if t.TenantID == tenant {
			return t.Namespace
		}
	}
	w.I.T.Fatalf("tenant %s is not part of the session", tenant)
	return ""
}

// RuntimeToken reads the bearer token a running step entrypoint holds, the way only an operator with access to the
// Pod can. The token is the workload's own authority; using it from outside is how the result guard is exercised.
func (i *Installation) RuntimeToken(namespace, pod string) string {
	i.T.Helper()
	var tok string
	WaitForEvery(i.T, "the entrypoint's runtime token", 2*time.Minute, 2*time.Second, func() bool {
		out, err := i.Kube.Run(context.Background(), "-n", namespace, "exec", pod, "-c", kube.PrimaryContainer, "--", "cat", "/kodama/identity/token")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		tok = strings.TrimSpace(out)
		return true
	})
	return tok
}

// WaitLaunched waits until the entrypoint in the Pod admitted itself and launched the step's command.
func (i *Installation) WaitLaunched(namespace, pod string) {
	i.T.Helper()
	WaitForEvery(i.T, "the entrypoint to launch the command", 3*time.Minute, 2*time.Second, func() bool {
		out, err := i.Kube.Run(context.Background(), "-n", namespace, "logs", pod, "-c", kube.PrimaryContainer)
		return err == nil && strings.Contains(out, "launching command")
	})
}

// WaitPod waits for a running Pod of the Job and returns it.
func (i *Installation) WaitPod(namespace, job string, within time.Duration) Pod {
	i.T.Helper()
	var found Pod
	WaitForEvery(i.T, "a running Pod of "+job, within, 2*time.Second, func() bool {
		pods, err := i.PodsOfJob(context.Background(), namespace, job)
		if err != nil {
			return false
		}
		for _, p := range pods {
			if p.Phase == "Running" {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// ManagedSecrets lists the Secrets the control plane manages in a tenant namespace (the per-attempt admission
// credentials the Job mounts).
func (i *Installation) ManagedSecrets(ctx context.Context, namespace string) ([]string, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := i.Kube.JSON(ctx, &l, "-n", namespace, "get", "secrets", "-l", kube.LabelManagedBy); err != nil {
		return nil, err
	}
	var names []string
	for _, it := range l.Items {
		names = append(names, it.Metadata.Name)
	}
	return names, nil
}

var _ = json.Marshal
var _ = fmt.Sprintf
