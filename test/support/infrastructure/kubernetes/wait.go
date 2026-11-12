// Readiness barriers for one kind cluster. Every probe is an observation, never
// a fixed sleep: harness.WaitReady retries with bounded exponential backoff and
// the injected clock, and unit tests use a clock that never sleeps.
package kubesupport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// nodeList is the subset of `kubectl get nodes -o json` the barrier reads.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// WaitReady blocks until the cluster is usable: every node reports Ready, the
// default ServiceAccount exists and the standard StorageClass is present. Each
// attempt checks all three so the error names the first missing piece.
func (c *Cluster) WaitReady(ctx context.Context) error {
	options := c.kind.waitOptions
	if c.ReadyTimeout > 0 {
		options.Timeout = c.ReadyTimeout
	}
	probe := func(ctx context.Context) error {
		if err := c.nodeReady(ctx); err != nil {
			return fmt.Errorf("node readiness: %w", err)
		}
		if err := c.defaultServiceAccount(ctx); err != nil {
			return fmt.Errorf("default serviceaccount: %w", err)
		}
		if err := c.storageClass(ctx); err != nil {
			return fmt.Errorf("storageclass %s: %w", StorageClassName, err)
		}
		return nil
	}
	return harness.WaitReady(ctx, c.kind.clock, "kind cluster "+c.Name, probe, options)
}

// nodeReady requires at least one node and every node Ready=True.
func (c *Cluster) nodeReady(ctx context.Context) error {
	result, err := c.Kubectl(ctx, "get", "nodes", "-o", "json")
	if err != nil {
		return err
	}
	var list nodeList
	if err := json.Unmarshal(result.Stdout, &list); err != nil {
		return fmt.Errorf("decode nodes: %w", err)
	}
	if len(list.Items) == 0 {
		return errors.New("no nodes are registered")
	}
	for _, node := range list.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("node %q is not Ready", node.Metadata.Name)
		}
	}
	return nil
}

// defaultServiceAccount requires the namespace default ServiceAccount, which
// every workload pod relies on even when it mounts no token.
func (c *Cluster) defaultServiceAccount(ctx context.Context) error {
	document, err := c.GetJSON(ctx, "get", "serviceaccounts", "default")
	if err != nil {
		return err
	}
	metadata, _ := document["metadata"].(map[string]any)
	if name, _ := metadata["name"].(string); name != "default" {
		return fmt.Errorf("default ServiceAccount is absent or misnamed (%q)", name)
	}
	return nil
}

// storageClass requires the pinned StorageClass used by retained workspaces.
func (c *Cluster) storageClass(ctx context.Context) error {
	document, err := c.GetJSON(ctx, "get", "storageclass", StorageClassName)
	if err != nil {
		return err
	}
	metadata, _ := document["metadata"].(map[string]any)
	if name, _ := metadata["name"].(string); name != StorageClassName {
		return fmt.Errorf("StorageClass %q is absent or misnamed (%q)", StorageClassName, name)
	}
	if provisioner, _ := document["provisioner"].(string); provisioner == "" {
		return fmt.Errorf("StorageClass %q has no provisioner", StorageClassName)
	}
	return nil
}
