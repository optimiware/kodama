package kit

// Real Kubernetes as the preview's deployment target: the scope's kind cluster
// (the `cluster` stack), a namespace per scenario under restricted Pod
// Security, and a namespace-scoped ServiceAccount credential held as the
// project's kubeconfig secret. The test process holds the cluster-admin
// credential to create and inspect; the deployment scripts only ever see the
// scoped one. A scope without a cluster FAILS the scenarios that need it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// Cluster is the scope's kind cluster.
type Cluster struct {
	admin    string
	server   string
	caData   string
	provider string
	node     string
}

var (
	clusterOnce sync.Once
	clusterVal  *Cluster
	clusterErr  error
)

// RequireCluster returns the cluster fixture; a missing cluster fails.
//
// A `cluster` scope provides its kind cluster (KODAMA_TEST_KUBECONFIG). A
// `services` scope has none, and a scope's stack cannot be changed without
// tearing the scope down, which a test must never do: the kit then creates a
// kind cluster that the scope's test processes share, attaches its node to the
// scope's registry network, and deletes it when the last of them ends (see
// provisionCluster).
// Without the kind binary or the scope's registry network that fails too.
func RequireCluster(t testing.TB) *Cluster {
	t.Helper()
	prepareCluster()
	if clusterErr != nil {
		t.Fatalf("cluster fixture: %v", clusterErr)
	}
	return clusterVal
}

// prepareCluster discovers or creates the cluster once per process. Main starts
// it in the background when the whole suite runs, so the minute kind needs
// overlaps the tests that do not use it.
func prepareCluster() {
	clusterOnce.Do(func() {
		path := kubetest.KubeconfigPath()
		if _, err := os.Stat(path); err != nil {
			path, err = provisionCluster()
			if err != nil {
				clusterErr = fmt.Errorf("the preview's deployment target is a kind cluster: the scope has none (%s is missing; DEV_STACK=cluster provides it) and the kit could not create one: %w", kubetest.KubeconfigPath(), err)
				return
			}
		}
		clusterVal, clusterErr = discoverCluster(path)
	})
}

// provisionCluster returns the kubeconfig of the kind cluster the test processes
// of one services scope share. The cluster is created by the first process that
// needs it and deleted by the last one that stops using it: a cluster per
// package process (three run at once) would double the load of a shared host and
// outlast its budget. Its node joins the scope's Podman network so that its
// containerd pulls `localhost:<registry port>/...` from the scope's registry
// container, exactly as the cluster stack wires it. The scope's own resources are
// only read.
//
// Sharing is by advisory file locks in the temporary directory: the creation lock
// serialises create-or-attach and teardown, and every user holds a shared lock on
// the users file for its lifetime (the kernel drops it with a killed process), so
// the deleter is the process that can take it exclusively.
func provisionCluster() (string, error) {
	provider := os.Getenv("DEV_PROVIDER")
	if provider == "" {
		provider = "podman"
	}
	network := os.Getenv("KODAMA_TEST_PODMAN_NETWORK")
	if !strings.HasSuffix(network, "-net") {
		return "", fmt.Errorf("KODAMA_TEST_PODMAN_NETWORK %q does not name the scope's workload network", network)
	}
	registry := strings.TrimSuffix(network, "-net") + "-registry"
	u, err := url.Parse(os.Getenv("KODAMA_REGISTRY_URL"))
	if err != nil || u.Port() == "" {
		return "", fmt.Errorf("KODAMA_REGISTRY_URL is not the scope's registry origin")
	}
	root := repoRoot()
	kind := filepath.Join(root, "dev", ".tools", "bin", "kind")
	if _, err := os.Stat(kind); err != nil {
		return "", fmt.Errorf("the kind binary %s is missing (a cluster scope installs it): %w", kind, err)
	}
	lock, err := os.ReadFile(filepath.Join(root, "dev", "versions.lock.json"))
	if err != nil {
		return "", err
	}
	var versions struct {
		Images struct {
			Node string `json:"node"`
		} `json:"images"`
	}
	if err := json.Unmarshal(lock, &versions); err != nil || versions.Images.Node == "" {
		return "", fmt.Errorf("dev/versions.lock.json names no kind node image")
	}

	name := "t038-kit-" + u.Port()
	dir := filepath.Join(os.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	env := append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER="+provider)
	creation, err := os.OpenFile(filepath.Join(dir, "creation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	users, err := os.OpenFile(filepath.Join(dir, "users.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		creation.Close()
		return "", err
	}
	if err := syscall.Flock(int(creation.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	// Whatever happens next, this process leaves the creation lock and, when it ends,
	// its place among the users.
	OnCleanup(func() {
		users.Close() // drops this process's shared lock
		if err := syscall.Flock(int(creation.Fd()), syscall.LOCK_EX); err != nil {
			return
		}
		defer creation.Close()
		again, err := os.OpenFile(filepath.Join(dir, "users.lock"), os.O_RDWR, 0o600)
		if err != nil {
			return
		}
		defer again.Close()
		if syscall.Flock(int(again.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return // another process still uses the cluster: it deletes it
		}
		cmd := exec.Command(kind, "delete", "cluster", "--name", name)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "m2 kit: deleting the kind cluster %s: %v: %s\n", name, err, out)
			return
		}
		_ = os.RemoveAll(dir)
	})
	defer syscall.Flock(int(creation.Fd()), syscall.LOCK_UN)
	if err := syscall.Flock(int(users.Fd()), syscall.LOCK_SH); err != nil {
		return "", err
	}

	node := name + "-control-plane"
	if running, err := shell(provider, "inspect", "--format", "{{.State.Running}}", node); err == nil && running == "true" {
		cmd := exec.Command(kind, "export", "kubeconfig", "--name", name, "--kubeconfig", kubeconfig)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("kind export kubeconfig: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return kubeconfig, nil
	}

	// A loaded shared host sometimes starts the API server too slowly for kubeadm's
	// bootstrap; kind removes the failed node, so a fresh attempt starts clean.
	// runc inside the node creates a session keyring per container, from the host
	// user's kernel key quota (200 by default) that other scopes and tasks of a
	// shared host can exhaust ("unable to join session keyring ... disk quota
	// exceeded": the pod never starts). The node's containerd is told not to.
	config := filepath.Join(dir, "kind.yaml")
	if err := os.WriteFile(config, []byte(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
    SystemdCgroup = true
    NoNewKeyring = true
`), 0o600); err != nil {
		return "", err
	}
	del := func() {
		cmd := exec.Command(kind, "delete", "cluster", "--name", name)
		cmd.Env = env
		_ = cmd.Run()
	}
	del() // a remnant of a killed run
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		cmd := exec.Command(kind, "create", "cluster", "--name", name, "--image", versions.Images.Node, "--config", config, "--kubeconfig", kubeconfig, "--wait", "180s")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("kind create cluster (attempt %d): %w: %s", attempt, err, strings.TrimSpace(string(out)))
		del()
	}
	if lastErr != nil {
		return "", lastErr
	}
	if _, err := shell(provider, "network", "connect", network, node); err != nil {
		return "", err
	}
	hosts := "/etc/containerd/certs.d/localhost:" + u.Port()
	if _, err := shell(provider, "exec", node, "mkdir", "-p", hosts); err != nil {
		return "", err
	}
	write := exec.Command(provider, "exec", "-i", node, "cp", "/dev/stdin", hosts+"/hosts.toml")
	write.Stdin = strings.NewReader("[host.\"http://" + registry + ":5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n")
	if out, err := write.CombinedOutput(); err != nil {
		return "", fmt.Errorf("registry mirror: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return kubeconfig, nil
}

func shell(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func discoverCluster(kubeconfig string) (*Cluster, error) {
	c := &Cluster{admin: kubeconfig, provider: os.Getenv("DEV_PROVIDER")}
	if c.provider == "" {
		c.provider = "podman"
	}
	raw, err := kubetest.Kubectl(kubeconfig, "config", "view", "--raw", "--minify", "-o", "json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		CurrentContext string `json:"current-context"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Clusters) == 0 || doc.Clusters[0].Cluster.CAData == "" {
		return nil, fmt.Errorf("the kubeconfig has no cluster with embedded certificate authority data")
	}
	c.server, c.caData = doc.Clusters[0].Cluster.Server, doc.Clusters[0].Cluster.CAData
	if !strings.HasPrefix(c.server, "https://127.0.0.1:") && !strings.HasPrefix(c.server, "https://localhost:") {
		return nil, fmt.Errorf("the API server %s is not on loopback; the host-networked step containers reach it through the host", c.server)
	}
	c.node = strings.TrimPrefix(doc.CurrentContext, "kind-") + "-control-plane"
	if _, err := shell(c.provider, "inspect", "--format", "{{.State.Running}}", c.node); err != nil {
		return nil, fmt.Errorf("the kind node %s is not running under %s: %w", c.node, c.provider, err)
	}
	return c, nil
}

// Kubectl runs kubectl with the test's cluster-admin credential.
func (c *Cluster) Kubectl(t testing.TB, args ...string) string {
	t.Helper()
	out, err := kubetest.Kubectl(c.admin, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return strings.TrimSpace(string(out))
}

// KubectlTry is Kubectl reporting the error.
func (c *Cluster) KubectlTry(args ...string) (string, error) {
	out, err := kubetest.Kubectl(c.admin, args...)
	return strings.TrimSpace(string(out)), err
}

// Namespace is one scenario's namespace with its scoped credential.
type Namespace struct {
	C          *Cluster
	Name       string
	Kubeconfig string // the scoped kubeconfig (what becomes the project's secret)
	Token      string // the scoped credential: it must never reach a log or an event
}

// NewNamespace creates t038-<label>-<random> with restricted Pod Security and a
// ServiceAccount whose Role covers only the namespaced kinds the example
// manifests use. Cleanup deletes exactly this namespace.
func (c *Cluster) NewNamespace(t testing.TB, dir, label string) *Namespace {
	t.Helper()
	name := fmt.Sprintf("t038-%s-%s", label, randHex(3))
	c.Kubectl(t, "create", "namespace", name)
	t.Cleanup(func() { _, _ = c.KubectlTry("delete", "namespace", name, "--wait=false", "--ignore-not-found") })
	c.Kubectl(t, "label", "namespace", name, "pod-security.kubernetes.io/enforce=restricted", "pod-security.kubernetes.io/enforce-version=latest", "kodama.invalid/task=t038")
	c.Kubectl(t, "-n", name, "create", "serviceaccount", "deployer")
	c.Kubectl(t, "-n", name, "create", "role", "deployer",
		"--verb=get,list,watch,create,update,patch,delete",
		"--resource=configmaps,services,deployments.apps,statefulsets.apps,jobs.batch")
	c.Kubectl(t, "-n", name, "create", "rolebinding", "deployer", "--role=deployer", "--serviceaccount="+name+":deployer")
	token := c.Kubectl(t, "-n", name, "create", "token", "deployer", "--duration=2h")
	ns := &Namespace{C: c, Name: name, Token: token, Kubeconfig: filepath.Join(dir, "kubeconfig-"+name)}
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: target
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: deployer
  user:
    token: %s
contexts:
- name: target
  context: {cluster: target, user: deployer, namespace: %s}
current-context: target
`, c.server, c.caData, token, name)
	if err := os.WriteFile(ns.Kubeconfig, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	return ns
}

// Admin runs kubectl as cluster-admin in this namespace.
func (n *Namespace) Admin(t testing.TB, args ...string) string {
	t.Helper()
	return n.C.Kubectl(t, append([]string{"-n", n.Name}, args...)...)
}

// AdminTry is Admin reporting the error.
func (n *Namespace) AdminTry(args ...string) (string, error) {
	return n.C.KubectlTry(append([]string{"-n", n.Name}, args...)...)
}

// Object is the live object as JSON, or nil when absent.
func (n *Namespace) Object(t testing.TB, kind, name string) map[string]any {
	t.Helper()
	out, err := n.AdminTry("get", kind, name, "--ignore-not-found", "-o", "json")
	if err != nil {
		t.Fatalf("get %s/%s: %v", kind, name, err)
	}
	if out == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	return m
}

// UID is the object's uid.
func UID(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	s, _ := md["uid"].(string)
	return s
}

// Annotations are the object's annotations.
func Annotations(obj map[string]any) map[string]string {
	md, _ := obj["metadata"].(map[string]any)
	raw, _ := md["annotations"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		out[k], _ = v.(string)
	}
	return out
}

// Eventually polls cond until it holds or the deadline passes.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %s", what, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand_Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
