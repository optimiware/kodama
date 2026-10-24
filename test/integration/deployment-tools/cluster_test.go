package deploymenttools_test

// Real Kubernetes: the task's kind cluster, per-test namespaces under restricted
// Pod Security, and a namespace-scoped ServiceAccount credential (the project's
// "kubeconfig secret"). The test process holds the cluster-admin credential to
// create and inspect; the scripts only ever see the scoped one.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/test/support/layer"
)

type cluster struct {
	admin    string // admin kubeconfig path
	server   string
	caData   string // base64 PEM, as in a kubeconfig
	provider string
	node     string
}

var (
	clusterOnce sync.Once
	clusterVal  *cluster
	clusterErr  error
)

// requireCluster returns the cluster fixture. A missing cluster FAILS: a skipped
// check is not acceptance evidence.
func requireCluster(t *testing.T) *cluster {
	t.Helper()
	layer.SkipService(t, "Kubernetes")
	path := kubetest.KubeconfigPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a kind cluster is required but %s is missing (run make dev-up for this scope, or set KODAMA_TEST_KUBECONFIG): %v", path, err)
	}
	clusterOnce.Do(func() { clusterVal, clusterErr = discoverCluster(path) })
	if clusterErr != nil {
		t.Fatalf("cluster fixture: %v", clusterErr)
	}
	return clusterVal
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

func discoverCluster(kubeconfig string) (*cluster, error) {
	c := &cluster{admin: kubeconfig, provider: os.Getenv("DEV_PROVIDER")}
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
		return nil, fmt.Errorf("the API server %s is not on loopback; these tests reach it from the host network namespace", c.server)
	}
	c.node = strings.TrimPrefix(doc.CurrentContext, "kind-") + "-control-plane"
	if _, err := shell(c.provider, "inspect", "--format", "{{.State.Running}}", c.node); err != nil {
		return nil, fmt.Errorf("the kind node %s is not running under %s: %w", c.node, c.provider, err)
	}
	return c, nil
}

func (c *cluster) kubectl(t testing.TB, args ...string) string {
	t.Helper()
	out, err := kubetest.Kubectl(c.admin, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return strings.TrimSpace(string(out))
}

func (c *cluster) kubectlTry(args ...string) (string, error) {
	out, err := kubetest.Kubectl(c.admin, args...)
	return strings.TrimSpace(string(out)), err
}

// namespace is one test namespace with its scoped credential.
type namespace struct {
	c          *cluster
	name       string
	kubeconfig string // the scoped kubeconfig written for the script
	token      string // the scoped credential (tests assert it never reaches a log)
}

// newNamespace creates <prefix>-<random> with restricted Pod Security enforcement
// and a ServiceAccount whose Role covers only the namespaced kinds the example
// manifests use. Cleanup deletes exactly this namespace.
func (c *cluster) newNamespace(t *testing.T, dir, label string) *namespace {
	t.Helper()
	name := fmt.Sprintf("t065-%s-%s", label, randHex(3))
	c.kubectl(t, "create", "namespace", name)
	t.Cleanup(func() { _, _ = c.kubectlTry("delete", "namespace", name, "--wait=false", "--ignore-not-found") })
	c.kubectl(t, "label", "namespace", name, "pod-security.kubernetes.io/enforce=restricted", "pod-security.kubernetes.io/enforce-version=latest", "kodama.invalid/task=t065")
	c.kubectl(t, "-n", name, "create", "serviceaccount", "deployer")
	c.kubectl(t, "-n", name, "create", "role", "deployer",
		"--verb=get,list,watch,create,update,patch,delete",
		"--resource=configmaps,services,deployments.apps,statefulsets.apps,jobs.batch")
	c.kubectl(t, "-n", name, "create", "rolebinding", "deployer", "--role=deployer", "--serviceaccount="+name+":deployer")
	token := c.kubectl(t, "-n", name, "create", "token", "deployer", "--duration=1h")
	ns := &namespace{c: c, name: name, token: token, kubeconfig: filepath.Join(dir, "kubeconfig-"+name)}
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
	if err := os.WriteFile(ns.kubeconfig, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	return ns
}

// admin runs kubectl with the test's cluster-admin credential in this namespace.
func (n *namespace) admin(t testing.TB, args ...string) string {
	t.Helper()
	return n.c.kubectl(t, append([]string{"-n", n.name}, args...)...)
}

func (n *namespace) adminTry(args ...string) (string, error) {
	return n.c.kubectlTry(append([]string{"-n", n.name}, args...)...)
}

// scoped runs kubectl with the namespace-scoped credential.
func (n *namespace) scoped(args ...string) (string, error) {
	out, err := kubetest.Kubectl(n.kubeconfig, args...)
	return strings.TrimSpace(string(out)), err
}

// object is the live object as JSON, or nil when absent.
func (n *namespace) object(t testing.TB, kind, name string) map[string]any {
	t.Helper()
	out, err := n.adminTry("get", kind, name, "--ignore-not-found", "-o", "json")
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

func uidOf(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	s, _ := md["uid"].(string)
	return s
}

func annotationsOf(obj map[string]any) map[string]string {
	md, _ := obj["metadata"].(map[string]any)
	raw, _ := md["annotations"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		out[k], _ = v.(string)
	}
	return out
}

// ---- the image inside the kind node ---------------------------------------------

var (
	loadMu     sync.Mutex
	loadedRefs = map[string]string{}
)

// loadIntoNode imports the locally built image into the kind node's containerd
// (the cluster has no registry the node can pull from) and returns
// <repo>@sha256:<digest> as containerd resolves it. The digest is that of the OCI
// archive's index, which is the reference the import registers.
func (c *cluster) loadIntoNode(t *testing.T, localTag, dir string) (ref, digest string) {
	t.Helper()
	loadMu.Lock()
	defer loadMu.Unlock()
	if ref, ok := loadedRefs[localTag]; ok {
		return ref, ref[strings.Index(ref, "@")+1:]
	}
	repo := "t065.local/tools"
	nodeTag := repo + ":" + randHex(4)
	if _, err := shell(c.provider, "tag", localTag, nodeTag); err != nil {
		t.Fatal(err)
	}
	onCleanup(func() { _, _ = shell(c.provider, "rmi", nodeTag) })
	archive := filepath.Join(dir, "tools.tar")
	saveArgs := []string{"save", "-o", archive, nodeTag}
	if c.provider == "podman" {
		saveArgs = []string{"save", "--format", "oci-archive", "-o", archive, nodeTag}
	}
	if _, err := shell(c.provider, saveArgs...); err != nil {
		t.Fatal(err)
	}
	index, err := shell("tar", "-xOf", archive, "index.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex([]byte(index))
	digest = "sha256:" + sum
	ref = repo + "@" + digest
	onCleanup(func() { _, _ = shell(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "rm", ref, nodeTag) })
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(c.provider, "exec", "-i", c.node, "ctr", "-n", "k8s.io", "images", "import", "--base-name", repo, "--digests", "-")
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("loading the image into the node: %v: %s", err, out)
	}
	listing, err := shell(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil || !strings.Contains(listing, ref) {
		t.Fatalf("image %s is not resolvable in the node after loading", ref)
	}
	loadedRefs[localTag] = ref
	return ref, digest
}

func eventually(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
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
