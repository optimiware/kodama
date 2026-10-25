package devenv

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Container is the part of `container inspect` the environment decides on.
type Container struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
	NetworkSettings struct {
		Ports    map[string][]PortBinding `json:"Ports"`
		Networks map[string]Network       `json:"Networks"`
	} `json:"NetworkSettings"`
}

// PortBinding is one published host port.
type PortBinding struct {
	HostPort string `json:"HostPort"`
}

// Network is one container network attachment.
type Network struct {
	IPAddress string `json:"IPAddress"`
}

func decodeContainers(data string) ([]Container, error) {
	var containers []Container
	if err := json.Unmarshal([]byte(data), &containers); err != nil {
		return nil, fmt.Errorf("unreadable container inspect output: %w", err)
	}
	return containers, nil
}

// Nodes lists the kind nodes of this scope.
func (e *Environment) Nodes() ([]string, error) {
	stdout, err := outputCmd(e.Env, e.Kind, "get", "nodes", "--name", e.State.Cluster)
	if err != nil {
		return nil, err
	}
	return strings.Fields(stdout), nil
}

// VerifyNodes refuses to continue when a node with our cluster's name is no
// longer the instance the journal recorded.
func (e *Environment) VerifyNodes(nodes []string) error {
	for _, name := range nodes {
		item, err := e.Inspect(name)
		if err != nil {
			return err
		}
		if item == nil || e.State.Nodes[name] != item.ID {
			return fmt.Errorf("node ownership mismatch: %s", name)
		}
		if item.Config.Labels["io.x-k8s.kind.cluster"] != e.State.Cluster {
			return fmt.Errorf("kind cluster label mismatch")
		}
	}
	return nil
}

// EnsureCluster creates the scoped kind cluster if it is absent and exports
// its kubeconfig.
func (e *Environment) EnsureCluster() ([]string, error) {
	nodes, err := e.Nodes()
	if err != nil {
		return nil, err
	}
	if len(nodes) > 0 {
		if err := e.VerifyNodes(nodes); err != nil {
			return nil, err
		}
		if _, err := outputCmd(e.Env, e.Kind, "export", "kubeconfig", "--name", e.State.Cluster,
			"--kubeconfig", filepath.Join(e.Dir, "kubeconfig")); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	lock, err := Versions()
	if err != nil {
		return nil, err
	}
	config := filepath.Join(e.Dir, "kind.json")
	document := map[string]any{
		"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4",
		"nodes": []map[string]any{{"role": "control-plane", "labels": map[string]string{ScopeLabel: e.State.Owner}}},
	}
	if err := WriteJSON(config, document); err != nil {
		return nil, err
	}
	_, createErr := runCmd(Cmd{
		Args: []string{e.Kind, "create", "cluster", "--name", e.State.Cluster, "--config", config,
			"--image", lock.Images.Node, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
			"--wait", "180s", "--retain"},
		Env: e.Env, Timeout: 10 * time.Minute,
	})
	// Journal the nodes even after a partial failure, so teardown can find
	// them.
	nodes, err = e.Nodes()
	if err != nil {
		return nil, err
	}
	for _, name := range nodes {
		if item, inspectErr := e.Inspect(name); inspectErr == nil && item != nil {
			e.State.Nodes[name] = item.ID
		}
	}
	if err := e.Save(); err != nil {
		return nil, err
	}
	if createErr != nil {
		return nil, createErr
	}
	return nodes, nil
}

// NetworkItem inspects the scoped network; it refuses a network this scope
// does not own.
func (e *Environment) NetworkItem() (map[string]any, error) {
	result, err := runCmd(Cmd{Args: []string{e.Provider, "network", "inspect", e.State.Network}, Capture: true})
	if err != nil {
		lower := strings.ToLower(result.Stderr)
		if strings.Contains(lower, "no such") || strings.Contains(lower, "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("network inspection failed: %w", err)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("unreadable network inspect output")
	}
	labels, _ := items[0]["Labels"].(map[string]any)
	if labels == nil {
		if raw, ok := items[0]["labels"].(map[string]any); ok {
			labels = raw
		}
	}
	owner, _ := labels[ScopeLabel].(string)
	if owner != e.State.Owner {
		return nil, fmt.Errorf("%w: network %s", ErrForeignResource, e.State.Network)
	}
	return items[0], nil
}

// EnsureNetwork creates the scoped workload network. Its ownership label gates
// removal.
func (e *Environment) EnsureNetwork() error {
	item, err := e.NetworkItem()
	if err != nil {
		return err
	}
	if item != nil {
		return nil
	}
	if err := execCmd(e.Provider, "network", "create", "--label", ScopeLabel+"="+e.State.Owner, e.State.Network); err != nil {
		return err
	}
	item, err = e.NetworkItem()
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("network %s was not created", e.State.Network)
	}
	return nil
}

// Addresses returns each dependency container's address on kind's network.
func (e *Environment) Addresses() (map[string]string, error) {
	addresses := map[string]string{}
	for service := range ServicePorts {
		item, err := e.Inspect(e.Container(service))
		if err != nil {
			return nil, err
		}
		if item == nil || !item.State.Running {
			return nil, fmt.Errorf("%s is not running; see make dev-diagnostics", service)
		}
		addresses[service] = item.NetworkSettings.Networks["kind"].IPAddress
	}
	return addresses, nil
}

// HostPort reads the dynamically allocated loopback port of a service.
func (e *Environment) HostPort(service string) (int, error) {
	number, ok := ServicePorts[service]
	if !ok {
		return 0, fmt.Errorf("unknown service %q", service)
	}
	item, err := e.Inspect(e.Container(service))
	if err != nil {
		return 0, err
	}
	if item == nil {
		return 0, fmt.Errorf("container %s is missing", e.Container(service))
	}
	bindings := item.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", number)]
	if len(bindings) == 0 {
		return 0, fmt.Errorf("container %s publishes no %s port", e.Container(service), service)
	}
	var port int
	if _, err := fmt.Sscanf(bindings[0].HostPort, "%d", &port); err != nil {
		return 0, fmt.Errorf("container %s has an unreadable port: %w", e.Container(service), err)
	}
	return port, nil
}

// outputCmd runs a command with an explicit environment and captures stdout.
func outputCmd(env []string, name string, args ...string) (string, error) {
	result, err := runCmd(Cmd{Args: append([]string{name}, args...), Env: env, Capture: true})
	return result.Stdout, err
}
