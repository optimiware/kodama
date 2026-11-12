// Cluster lifecycle for one pinned kind cluster.
package kubesupport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// Cluster is one created kind cluster with its own isolated kubeconfig.
type Cluster struct {
	// Name is the generated cluster name.
	Name string
	// Dir holds the kind config, kubeconfig and any applied YAML.
	Dir string
	// KubeconfigPath is the per-cluster kubeconfig file. The user's
	// ~/.kube/config is never read or written.
	KubeconfigPath string
	// ConfigPath is the written kind cluster configuration.
	ConfigPath string
	// ReadyTimeout bounds WaitReady.
	ReadyTimeout time.Duration

	kind      *Kind
	ownedDir  bool
	createdAt time.Time

	closeOnce sync.Once
	closeErr  error
}

// CreatedAt is the wall-clock creation time recorded as evidence.
func (c *Cluster) CreatedAt() time.Time { return c.createdAt }

// KubeconfigJSON returns the isolated kubeconfig as the JSON document the
// driver consumes. It is produced exactly the way the plan requires:
//
//	kubectl --kubeconfig <file> config view --raw --minify -o json
func (c *Cluster) KubeconfigJSON(ctx context.Context) ([]byte, error) {
	result, err := c.kind.runKubectl(ctx,
		"--kubeconfig", c.KubeconfigPath,
		"config", "view", "--raw", "--minify", "-o", "json",
	)
	if err != nil {
		return nil, fmt.Errorf("kubesupport: export kubeconfig for %s: %w", c.Name, err)
	}
	var document map[string]any
	if err := json.Unmarshal(result.Stdout, &document); err != nil {
		return nil, fmt.Errorf("kubesupport: exported kubeconfig for %s is not JSON: %w", c.Name, err)
	}
	if _, ok := document["clusters"]; !ok {
		return nil, fmt.Errorf("kubesupport: exported kubeconfig for %s has no clusters", c.Name)
	}
	return result.Stdout, nil
}

// Kubectl runs one kubectl command against this cluster's kubeconfig. The
// kubeconfig flag is always explicit, so an inherited KUBECONFIG cannot select
// another cluster.
func (c *Cluster) Kubectl(ctx context.Context, args ...string) (harness.Result, error) {
	full := append([]string{"--kubeconfig", c.KubeconfigPath}, args...)
	return c.kind.runKubectl(ctx, full...)
}

// Apply writes yaml to a private file under the cluster directory and applies
// it. The temporary file is removed afterwards.
func (c *Cluster) Apply(ctx context.Context, yaml string) (harness.Result, error) {
	file, err := os.CreateTemp(c.Dir, "apply-*.yaml")
	if err != nil {
		return harness.Result{}, fmt.Errorf("kubesupport: stage apply file: %w", err)
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := file.WriteString(yaml); err != nil {
		_ = file.Close()
		return harness.Result{}, fmt.Errorf("kubesupport: write apply file: %w", err)
	}
	if err := file.Close(); err != nil {
		return harness.Result{}, fmt.Errorf("kubesupport: close apply file: %w", err)
	}
	return c.Kubectl(ctx, "apply", "-f", path)
}

// GetJSON runs kubectl with -o json and returns the decoded document.
func (c *Cluster) GetJSON(ctx context.Context, args ...string) (map[string]any, error) {
	full := append(append([]string(nil), args...), "-o", "json")
	result, err := c.Kubectl(ctx, full...)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(result.Stdout, &document); err != nil {
		return nil, fmt.Errorf("kubesupport: decode kubectl JSON: %w", err)
	}
	return document, nil
}

// Close deletes the cluster, unregisters it and removes the private directory.
// It is idempotent and safe to call from a signal handler.
//
// A transient kind/Podman failure is retried a bounded number of times. If
// every attempt fails the cluster stays registered and Close returns the error:
// the caller must fail the test rather than report green, and the post-run sweep
// retries own-pid clusters that are still listed.
func (c *Cluster) Close() error {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), DefaultDeleteTimeout)
		defer cancel()
		if err := c.kind.deleteWithRetry(ctx, c.Name, c.KubeconfigPath, deleteAttempts); err != nil {
			// The delete command can fail after the provider already removed
			// the cluster. Only a cluster that is still listed is a leak.
			gone, listErr := c.kind.clusterGone(ctx, c.Name)
			if listErr != nil || !gone {
				c.closeErr = err
				// Keep the cluster registered until it is confirmed gone.
				// Removing the directory would also discard the evidence
				// needed to diagnose the leak.
				return
			}
		}
		unregisterCluster(c)
		if c.ownedDir && c.Dir != "" {
			if err := os.RemoveAll(c.Dir); err != nil {
				c.closeErr = fmt.Errorf("kubesupport: remove cluster directory %s: %w", c.Dir, err)
			}
		}
	})
	return c.closeErr
}
