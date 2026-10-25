package stage

import (
	"context"
	"time"
)

// Diagnose records only allowlisted status metadata, never Pod specs/env, Secret contents,
// event messages, provider responses or arbitrary container logs.
func (i *Installation) Diagnose() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, ns := range append([]Tenant{{Namespace: i.S.Namespace}}, i.S.Tenants...) {
		var pods struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Phase      string `json:"phase"`
					Containers []struct {
						Name     string `json:"name"`
						Restarts int    `json:"restartCount"`
						State    struct {
							Waiting *struct {
								Reason string `json:"reason"`
							} `json:"waiting"`
							Terminated *struct {
								Reason string `json:"reason"`
								Exit   int    `json:"exitCode"`
							} `json:"terminated"`
						} `json:"state"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := i.Kube.JSON(ctx, &pods, "-n", ns.Namespace, "get", "pods"); err != nil {
			i.T.Logf("diagnostic namespace=%s unavailable", ns.Namespace)
			continue
		}
		for _, p := range pods.Items {
			i.T.Logf("diagnostic namespace=%s pod=%s phase=%s", ns.Namespace, p.Metadata.Name, p.Status.Phase)
			for _, c := range p.Status.Containers {
				if c.State.Waiting != nil {
					i.T.Logf("diagnostic container=%s restarts=%d waiting=%s", c.Name, c.Restarts, c.State.Waiting.Reason)
				}
				if c.State.Terminated != nil {
					i.T.Logf("diagnostic container=%s restarts=%d terminated=%s exit=%d", c.Name, c.Restarts, c.State.Terminated.Reason, c.State.Terminated.Exit)
				}
			}
		}
	}
}
