package stage

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/kube"
)

// ReleaseCommand releases a script's post-checkout barrier. The marker is in /tmp, never source.
// The script creates /tmp/kq-entered and waits for /tmp/kq-release; this proves the command has
// begun, rather than guessing when checkout happened from Job creation or an arbitrary sleep.
func (i *Installation) ReleaseCommand(namespace, pod string) {
	i.T.Helper()
	ctx := i.Context(2 * time.Minute)
	if _, err := i.Kube.Run(ctx, "-n", namespace, "exec", pod, "-c", kube.PrimaryContainer, "--", "touch", "/tmp/kq-release"); err != nil {
		i.T.Fatalf("release command barrier: %v", err)
	}
}

const CommandBarrier = `touch /tmp/kq-entered; while [ ! -e /tmp/kq-release ]; do sleep 1; done; `

func (i *Installation) WaitCommandBarrier(namespace, pod string) {
	i.T.Helper()
	WaitForEvery(i.T, "the command's post-checkout barrier", 2*time.Minute, time.Second, func() bool {
		_, err := i.Kube.Run(context.Background(), "-n", namespace, "exec", pod, "-c", kube.PrimaryContainer, "--", "test", "-e", "/tmp/kq-entered")
		return err == nil
	})
}

// Crash deletes the server Pod without draining. It requires a different Pod UID to become ready:
// rollout status alone can briefly report the old ready Pod after a force-delete.
func (i *Installation) Crash(ctx context.Context) {
	i.T.Helper()
	old, err := i.serverUID(ctx)
	if err != nil {
		i.T.Fatal(err)
	}
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "delete", "pod", "-l", "app.kubernetes.io/component=server", "--force", "--grace-period=0"); err != nil {
		i.T.Fatalf("crash server: %v", err)
	}
	WaitForEvery(i.T, "a replacement ready server Pod", 4*time.Minute, time.Second, func() bool {
		uid, err := i.serverUID(ctx)
		return err == nil && uid != old
	})
	i.WaitReady(ctx)
}

func (i *Installation) serverUID(ctx context.Context) (string, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				UID               string  `json:"uid"`
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := i.Kube.JSON(ctx, &pods, "-n", i.S.Namespace, "get", "pods", "-l", "app.kubernetes.io/component=server"); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				return p.Metadata.UID, nil
			}
		}
	}
	return "", fmt.Errorf("no ready server Pod")
}
