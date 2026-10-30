package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Down removes exactly the resources the recorded stack lists. It refuses
// before deleting anything when a resource is no longer bound to this scope.
func (e *Environment) Down() error {
	if e.State == nil {
		fmt.Printf("Scope already absent: %s\n", e.Scope)
		return nil
	}
	if err := e.Prepare(); err != nil {
		return err
	}
	if e.Provides("kind-cluster") {
		nodes, err := e.Nodes()
		if err != nil {
			return err
		}
		if err := e.VerifyNodes(nodes); err != nil {
			return err
		}
	} else if _, err := e.NetworkItem(); err != nil {
		return err
	}
	e.removeGitWorkers()
	_ = e.Compose("down", "-v")
	containers, err := e.OwnedContainers()
	if err != nil {
		return err
	}
	for _, container := range containers {
		if _, err := runCmd(Cmd{Args: []string{e.Provider, "rm", "-f", container}}); err != nil {
			return err
		}
	}
	if e.Provides("kind-cluster") {
		if _, err := runCmd(Cmd{Args: []string{e.Kind, "delete", "cluster", "--name", e.State.Cluster,
			"--kubeconfig", filepath.Join(e.Dir, "kubeconfig")}, Env: e.Env}); err != nil {
			return err
		}
	} else {
		item, err := e.NetworkItem()
		if err != nil {
			return err
		}
		if item != nil {
			if _, err := runCmd(Cmd{Args: []string{e.Provider, "network", "rm", e.State.Network}}); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(e.Dir); err != nil {
		return err
	}
	e.State = nil
	fmt.Printf("Removed scope: %s\n", e.Scope)
	return nil
}

// Diagnostics writes an allow-listed report: dependency logs and, for a
// cluster stack, Pod summaries and logs. Secrets, private keys and PostgreSQL
// startup SQL are never copied.
func (e *Environment) Diagnostics() error {
	if e.State == nil {
		return nil
	}
	directory := filepath.Join(StateDir(), "reports", e.Scope)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for _, service := range []string{"git", "oauthsonas", "nats", "localstack"} {
		result, err := runCmd(Cmd{Args: []string{e.Provider, "logs", e.Container(service)}, Capture: true, Timeout: 30 * time.Second})
		if err != nil && result.Stdout == "" && result.Stderr == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, service+".log"), []byte(result.Stdout+result.Stderr), 0o644); err != nil {
			return err
		}
	}
	for _, tenant := range e.State.GitTenants {
		result, err := runCmd(Cmd{Args: []string{e.Provider, "logs", e.GitWorkerContainer(tenant)}, Capture: true, Timeout: 30 * time.Second})
		if err != nil && result.Stdout == "" && result.Stderr == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, "git-"+tenant+".log"), []byte(result.Stdout+result.Stderr), 0o644); err != nil {
			return err
		}
	}
	if !e.Provides("kubeconfig") || !exists(filepath.Join(e.Dir, "kubeconfig")) {
		return nil
	}
	if e.Kubectl == "" {
		installed, err := installTool("kubectl")
		if err != nil {
			return err
		}
		e.Kubectl = installed
	}
	for name, args := range map[string][]string{
		"pods.txt":   {"get", "pods", "-A", "-o", "wide"},
		"events.txt": {"get", "events", "-A", "--sort-by=.metadata.creationTimestamp"},
	} {
		result, _ := runCmd(Cmd{Args: append([]string{e.Kubectl, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
			"--context", "kind-" + e.State.Cluster}, args...), Capture: true, Timeout: 30 * time.Second})
		if err := os.WriteFile(filepath.Join(directory, name), []byte(result.Stdout+result.Stderr), 0o644); err != nil {
			return err
		}
	}
	for _, namespace := range []string{DepsNamespace, TestsNamespace} {
		result, _ := runCmd(Cmd{Args: []string{e.Kubectl, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
			"--context", "kind-" + e.State.Cluster, "-n", namespace, "get", "pods", "-o", "json"}, Capture: true, Timeout: 30 * time.Second})
		var pods struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(result.Stdout), &pods); err != nil {
			continue
		}
		for _, pod := range pods.Items {
			logsResult, _ := runCmd(Cmd{Args: []string{e.Kubectl, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
				"--context", "kind-" + e.State.Cluster, "-n", namespace, "logs", pod.Metadata.Name}, Capture: true, Timeout: 30 * time.Second})
			name := namespace + "-" + pod.Metadata.Name + ".log"
			if err := os.WriteFile(filepath.Join(directory, name), []byte(logsResult.Stdout+logsResult.Stderr), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
