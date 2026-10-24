package logs_test

// The task's kind cluster fixture: kubeconfig-authenticated REST access (no
// kubectl is needed), provider/node discovery, the locally built, digest-pinned
// workload and injector images (loaded into the node's containerd because the
// cluster has no registry) and the address Pods use to reach the runtime server
// running in the test process. Everything created outside the cluster's
// task-owned namespaces (build directory, local image tags, imported images) is
// removed by TestMain; nothing else, and never a node, is touched.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

//go:embed fixtures/Containerfile.workload
var workloadContainerfile []byte

//go:embed fixtures/Containerfile.injector
var injectorContainerfile []byte

// basePin is the digest-pinned base image of both fixture images (recorded in
// fixtures/pins.json).
const basePin = "docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

// imageRepo is the repository prefix of the locally built fixture images.
const imageRepo = "t064.local"

// kapi is a minimal authenticated REST client for the objects the fixture
// itself provisions (namespaces, policies) and for raw log reads.
type kapi struct {
	origin string
	hc     *http.Client
}

type kubeconfigDoc struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			CertData string `yaml:"client-certificate-data"`
			KeyData  string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

func newKapi(kubeconfig string) (*kapi, string, error) {
	raw, err := os.ReadFile(kubeconfig)
	if err != nil {
		return nil, "", err
	}
	var kc kubeconfigDoc
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, "", err
	}
	var cluster, user string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			cluster, user = c.Context.Cluster, c.Context.User
		}
	}
	var server, caData, certData, keyData string
	for _, c := range kc.Clusters {
		if c.Name == cluster {
			server, caData = c.Cluster.Server, c.Cluster.CAData
		}
	}
	for _, u := range kc.Users {
		if u.Name == user {
			certData, keyData = u.User.CertData, u.User.KeyData
		}
	}
	if server == "" || caData == "" || certData == "" || keyData == "" {
		return nil, "", fmt.Errorf("kubeconfig %s: the current context needs a server, a CA and a client certificate", kubeconfig)
	}
	dec := base64.StdEncoding.DecodeString
	ca, err := dec(caData)
	if err != nil {
		return nil, "", err
	}
	crt, err := dec(certData)
	if err != nil {
		return nil, "", err
	}
	key, err := dec(keyData)
	if err != nil {
		return nil, "", err
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return nil, "", err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}}
	return &kapi{origin: strings.TrimRight(server, "/"), hc: hc}, strings.TrimPrefix(kc.CurrentContext, "kind-"), nil
}

// do sends one request; a non-nil body is JSON encoded.
func (k *kapi) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.origin+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, err
}

// create posts an object and tolerates an existing one.
func (k *kapi) create(ctx context.Context, collection string, obj map[string]any) error {
	code, b, err := k.do(ctx, http.MethodPost, collection, obj)
	if err != nil {
		return err
	}
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusConflict {
		return fmt.Errorf("create in %s: %d: %s", collection, code, truncate(string(b), 300))
	}
	return nil
}

// rawLog reads the container log of a Pod through the API directly (not
// through the log module): the evidence of what really reached Kubernetes.
func (k *kapi) rawLog(ctx context.Context, ns, pod, container string) (string, error) {
	code, b, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+pod+"/log?container="+container, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("pod log %s/%s: %d: %s", ns, pod, code, truncate(string(b), 200))
	}
	return string(b), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type cluster struct {
	kubeconfig string
	api        *kapi
	client     *kubernetes.Client
	provider   string
	node       string
	arch       string
	hostIP     string
	workload   string // digest-pinned image references usable by Pods
	injector   string
	id         string // this invocation's random id: image tags and build directory
}

var (
	clusterOnce sync.Once
	clusterVal  *cluster
	clusterErr  error

	// cleanups undo what this test invocation created outside the cluster's
	// namespaces: its build directory, local image tags and the node's imported
	// images.
	cleanupMu sync.Mutex
	cleanups  []func()
)

func onCleanup(f func()) {
	cleanupMu.Lock()
	cleanups = append(cleanups, f)
	cleanupMu.Unlock()
}

func runCleanups() {
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// requireCluster returns the shared cluster fixture. A missing cluster fails
// (KODAMA_REQUIRE_KUBERNETES=1, set by TestMain); the unit layer skips.
func requireCluster(t testing.TB) *cluster {
	t.Helper()
	kc := kubetest.Require(t)
	clusterOnce.Do(func() { clusterVal, clusterErr = setupCluster(kc) })
	if clusterErr != nil {
		t.Fatalf("cluster fixture: %v", clusterErr)
	}
	return clusterVal
}

func sh(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func repoRoot() string {
	_, file, _, _ := goruntime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func setupCluster(kc string) (*cluster, error) {
	c := &cluster{kubeconfig: kc}
	var name string
	var err error
	if c.api, name, err = newKapi(kc); err != nil {
		return nil, err
	}
	c.client, err = kubernetes.NewClient(kubernetes.Config{Origin: c.api.origin, HTTP: c.api.hc, RequestTimeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	// DEV_PROVIDER overrides the local Podman default on every OS.
	c.provider = os.Getenv("DEV_PROVIDER")
	if c.provider == "" {
		c.provider = "podman"
	}
	if c.provider != "podman" && c.provider != "docker" {
		return nil, fmt.Errorf("DEV_PROVIDER must be podman or docker (got %q)", c.provider)
	}
	c.node = name + "-control-plane"
	if _, err := sh(c.provider, "inspect", "--format", "{{.State.Running}}", c.node); err != nil {
		return nil, fmt.Errorf("the kind node container %s is not available through %s: %w", c.node, c.provider, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, b, err := c.api.do(ctx, http.MethodGet, "/api/v1/nodes", nil)
	if err != nil || code != http.StatusOK {
		return nil, fmt.Errorf("listing nodes: %d %v", code, err)
	}
	var nodes struct {
		Items []struct {
			Status struct {
				NodeInfo struct {
					Architecture string `json:"architecture"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &nodes); err != nil || len(nodes.Items) == 0 {
		return nil, fmt.Errorf("the cluster has no nodes: %v", err)
	}
	c.arch = nodes.Items[0].Status.NodeInfo.Architecture
	if c.hostIP, err = c.findHostIP(); err != nil {
		return nil, err
	}
	for _, cf := range [][]byte{workloadContainerfile, injectorContainerfile} {
		if !bytes.Contains(cf, []byte("FROM "+basePin)) {
			return nil, fmt.Errorf("a fixture Containerfile does not use the pinned base image")
		}
	}
	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	c.id = hex.EncodeToString(idb)
	build, err := os.MkdirTemp("", "kodama-t064-images-"+c.id+"-")
	if err != nil {
		return nil, err
	}
	onCleanup(func() { _ = os.RemoveAll(build) })
	bin := filepath.Join(build, "kodama")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/logs/stepbin")
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+c.arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the entrypoint: %w: %s", err, out)
	}
	if c.workload, err = c.loadImage(build, imageRepo+"/workload", workloadContainerfile); err != nil {
		return nil, err
	}
	if c.injector, err = c.loadImage(build, imageRepo+"/injector", injectorContainerfile); err != nil {
		return nil, err
	}
	return c, nil
}

// findHostIP returns the address a Pod can use to reach the machine running the
// tests: the host alias the container provider injects into the node, else the
// node's default gateway.
func (c *cluster) findHostIP() (string, error) {
	for _, alias := range []string{"host.containers.internal", "host.docker.internal"} {
		if out, err := sh(c.provider, "exec", c.node, "getent", "hosts", alias); err == nil && out != "" {
			return strings.Fields(out)[0], nil
		}
	}
	out, err := sh(c.provider, "exec", c.node, "sh", "-c", "ip route show default | awk '{print $3; exit}'")
	if err != nil || out == "" {
		return "", fmt.Errorf("cannot determine the host address from the node: %v", err)
	}
	return out, nil
}

// loadImage builds the image, saves it as an OCI archive, loads it into the
// node's containerd and returns <repo>@sha256:<digest of the archive index>.
func (c *cluster) loadImage(dir, repo string, containerfile []byte) (string, error) {
	cf := filepath.Join(dir, filepath.Base(repo)+".Containerfile")
	if err := os.WriteFile(cf, containerfile, 0o644); err != nil {
		return "", err
	}
	tag := repo + ":" + c.id
	onCleanup(func() { _, _ = sh(c.provider, "rmi", "-f", tag) })
	if _, err := sh(c.provider, "build", "-q", "-t", tag, "-f", cf, dir); err != nil {
		return "", err
	}
	archive := filepath.Join(dir, filepath.Base(repo)+".tar")
	args := []string{"save", "-o", archive, tag}
	if c.provider == "podman" {
		args = []string{"save", "--format", "oci-archive", "-o", archive, tag}
	}
	if _, err := sh(c.provider, args...); err != nil {
		return "", err
	}
	index, err := sh("tar", "-xOf", archive, "index.json")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(index))
	ref := repo + "@sha256:" + hex.EncodeToString(sum[:])
	onCleanup(func() { _, _ = sh(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "rm", ref, tag) })
	if listing, err := sh(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "ls", "-q"); err == nil {
		for _, l := range strings.Fields(listing) {
			if l == ref {
				return ref, nil // already loaded
			}
		}
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := exec.Command(c.provider, "exec", "-i", c.node, "ctr", "-n", "k8s.io", "images", "import", "--base-name", repo, "--digests", "-")
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("loading %s: %w: %s", repo, err, out)
	}
	listing, err := sh(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil || !strings.Contains(listing, ref) {
		return "", fmt.Errorf("image %s is not resolvable in the node after loading", ref)
	}
	return ref, nil
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

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
