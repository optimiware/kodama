package devenv

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Up builds the selected stack and writes the endpoint files.
func (e *Environment) Up() error {
	if err := e.SelectStack(); err != nil {
		return err
	}
	if err := e.SelectGitHosting(); err != nil {
		return err
	}
	if err := e.Prepare(); err != nil {
		return err
	}
	if e.State == nil {
		if err := os.MkdirAll(e.Dir, 0o700); err != nil {
			return err
		}
		owner, err := tokenHex(16)
		if err != nil {
			return err
		}
		cluster := "kodama-" + e.Scope + "-" + owner[:8]
		hosting := e.GitMode()
		e.State = &State{
			Root: Root(), Scope: e.Scope, Provider: e.Provider, Owner: owner,
			Stack: e.Stack, Cluster: cluster, Nodes: map[string]string{},
		}
		if hosting != DefaultGitHosting {
			e.State.GitHosting = hosting
		}
		if e.Stack == StackServices {
			e.State.Network = cluster + "-net"
		}
		// Persist intent before creating any external resource.
		if err := e.Save(); err != nil {
			return err
		}
	}
	fixtures, err := e.Fixtures()
	if err != nil {
		return err
	}
	var nodes []string
	if e.Provides("kind-cluster") {
		if nodes, err = e.EnsureCluster(); err != nil {
			return err
		}
	}
	if e.Stack == StackServices {
		if err := e.EnsureNetwork(); err != nil {
			return err
		}
	}
	if err := e.Compose("up", "-d", "registry"); err != nil {
		return err
	}
	port, err := e.HostPort("registry")
	if err != nil {
		return err
	}
	e.State.Port = port
	if err := e.Save(); err != nil {
		return err
	}
	if err := waitFor(fmt.Sprintf("http://127.0.0.1:%d/v2/", port), 90); err != nil {
		return err
	}
	for _, node := range nodes {
		if err := e.configureRegistryMirror(node); err != nil {
			return err
		}
	}
	if err := buildImageFn(e); err != nil {
		return err
	}
	if err := e.Compose("up", "-d"); err != nil {
		return err
	}
	hostEnv, err := e.HostEnv()
	if err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(e.Dir, "host-env.json"), hostEnv); err != nil {
		return err
	}
	if e.Provides("server-deployment") {
		addresses, err := e.Addresses()
		if err != nil {
			return err
		}
		items, err := e.Manifests(fixtures, addresses)
		if err != nil {
			return err
		}
		if err := e.Apply(items); err != nil {
			return err
		}
		if _, err := runCmd(Cmd{Args: []string{e.Kubectl, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
			"--context", "kind-" + e.State.Cluster, "-n", DepsNamespace, "rollout", "status", "deployment/kodama", "--timeout=300s"},
			Timeout: 330 * time.Second}); err != nil {
			return err
		}
	}
	if err := WriteJSON(filepath.Join(e.Dir, "endpoints.json"), e.WorkloadEnv()); err != nil {
		return err
	}
	if e.GitMode() == GitNative {
		fmt.Printf("Git hosting: native. Start a tenant's worker with: make dev-git-worker DEV_SCOPE=%s GIT_TENANT=<tenant-id>\n", e.Scope)
	}
	if e.Stack == StackServices {
		fmt.Printf("Ready: %s (%s), network: %s\n", e.Scope, e.State.Cluster, e.State.Network)
	} else {
		fmt.Printf("Ready: %s (%s), kubeconfig: %s\n", e.Scope, e.State.Cluster, filepath.Join(e.Dir, "kubeconfig"))
	}
	return nil
}

// configureRegistryMirror points the node's containerd at the scope registry,
// so kubelet pulls localhost:<port>/... through the registry container.
func (e *Environment) configureRegistryMirror(node string) error {
	directory := fmt.Sprintf("/etc/containerd/certs.d/localhost:%d", e.State.Port)
	if _, err := runCmd(Cmd{Args: []string{e.Provider, "exec", node, "mkdir", "-p", directory}, Env: e.Env}); err != nil {
		return err
	}
	hosts := fmt.Sprintf("[host.\"http://%s:5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n", e.Container("registry"))
	_, err := runCmd(Cmd{Args: []string{e.Provider, "exec", "-i", node, "cp", "/dev/stdin", directory + "/hosts.toml"},
		Env: e.Env, Stdin: stringReader(hosts)})
	return err
}

// Preflight is a read-only, bounded check: it never provisions, downloads or
// prints credentials.
func (e *Environment) Preflight() error {
	if err := e.SelectStack(); err != nil {
		return err
	}
	remedy := fmt.Sprintf("make dev-up DEV_SCOPE=%s DEV_PROVIDER=%s DEV_STACK=%s", e.Scope, e.Provider, e.Stack)
	hostEnvPath := filepath.Join(e.Dir, "host-env.json")
	if e.State == nil || !exists(hostEnvPath) ||
		(e.Provides("kubeconfig") && !exists(filepath.Join(e.Dir, "kubeconfig"))) {
		return fmt.Errorf("missing dev scope %s: run %s", e.Dir, remedy)
	}
	values, err := readStringMap(hostEnvPath)
	if err != nil {
		return err
	}
	if e.Provides("kubeconfig") {
		kubectl := values["KUBECTL"]
		if kubectl == "" || !executable(kubectl) {
			return fmt.Errorf("fixture unavailable: kubectl for %s; run %s", e.Dir, remedy)
		}
	}
	for _, key := range []string{"KODAMA_TEST_POSTGRES_ADMIN_URL", "KODAMA_NATS_URL", "KODAMA_S3_ENDPOINT", "KODAMA_REGISTRY_URL"} {
		address, err := url.Parse(values[key])
		if err != nil || address.Hostname() == "" || address.Port() == "" {
			return fmt.Errorf("fixture unavailable: %s in %s; run %s", key, e.Dir, remedy)
		}
		if !reachableHostPortFn(address.Hostname(), address.Port(), 2*time.Second) {
			return fmt.Errorf("fixture unavailable: %s in %s; run %s", key, e.Dir, remedy)
		}
	}
	if e.Provides("kubeconfig") {
		if _, err := runCmd(Cmd{Args: []string{values["KUBECTL"], "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"),
			"--context", "kind-" + e.State.Cluster, "get", "--raw=/readyz", "--request-timeout=5s"},
			Capture: true, Timeout: 10 * time.Second}); err != nil {
			return err
		}
	} else {
		item, err := e.NetworkItem()
		if err != nil {
			item = nil
		}
		if item == nil {
			return fmt.Errorf("fixture unavailable: network %s for %s; run %s", e.State.Network, e.Dir, remedy)
		}
	}
	fmt.Printf("PASS development scope %s (stack %s)\n", e.Dir, e.Stack)
	return nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
