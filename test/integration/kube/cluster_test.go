package kube_test

// The task's kind cluster fixture: provider/node discovery, the locally built,
// digest-pinned workload and injector images (loaded into the node's containerd
// because the cluster has no registry), and the address Pods use to reach the
// runtime server running in the test process.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

type cluster struct {
	kubeconfig string
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
	// images. Nothing else (another invocation's images, the cluster itself) is
	// touched.
	cleanupMu sync.Mutex
	cleanups  []func()
)

func onCleanup(f func()) {
	cleanupMu.Lock()
	cleanups = append(cleanups, f)
	cleanupMu.Unlock()
}

// TestMain removes this invocation's fixture resources after the tests.
func TestMain(m *testing.M) {
	code := m.Run()
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
	os.Exit(code)
}

// requireCluster returns the shared cluster fixture. A missing cluster skips the
// test unless KODAMA_REQUIRE_KUBERNETES=1; any other setup failure fails.
func requireCluster(t *testing.T) *cluster {
	t.Helper()
	kc := kubetest.Require(t)
	clusterOnce.Do(func() { clusterVal, clusterErr = setupCluster(kc) })
	if clusterErr != nil {
		t.Fatalf("cluster fixture: %v", clusterErr)
	}
	return clusterVal
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
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
	var err error
	if c.client, err = kubetest.NewClient(kc); err != nil {
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
	ctxName, err := run("kubectl", "--kubeconfig", kc, "config", "current-context")
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(ctxName, "kind-")
	c.node = name + "-control-plane"
	if _, err := run(c.provider, "inspect", "--format", "{{.State.Running}}", c.node); err != nil {
		return nil, fmt.Errorf("the kind node container %s is not available through %s: %w", c.node, c.provider, err)
	}
	arch, err := run("kubectl", "--kubeconfig", kc, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	if err != nil {
		return nil, err
	}
	c.arch = arch
	if c.hostIP, err = c.findHostIP(); err != nil {
		return nil, err
	}
	// The embedded Containerfiles must stay on the recorded base pin.
	for _, cf := range [][]byte{workloadContainerfile, injectorContainerfile} {
		if !bytes.Contains(cf, []byte("FROM "+basePin)) {
			return nil, fmt.Errorf("a fixture Containerfile does not use the pinned base image")
		}
	}
	// Every invocation builds in its own directory and tags its images with its own
	// random id: concurrent or leftover runs never share a mutable build context
	// or tag.
	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	c.id = hex.EncodeToString(idb)
	build, err := os.MkdirTemp("", "kodama-t021-images-"+c.id+"-")
	if err != nil {
		return nil, err
	}
	onCleanup(func() { _ = os.RemoveAll(build) })
	bin := filepath.Join(build, "kodama")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/kube/stepbin")
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+c.arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the entrypoint: %w: %s", err, out)
	}
	if c.workload, err = c.loadImage(build, "t021.local/workload", workloadContainerfile); err != nil {
		return nil, err
	}
	if c.injector, err = c.loadImage(build, "t021.local/injector", injectorContainerfile); err != nil {
		return nil, err
	}
	return c, nil
}

// findHostIP returns the address a Pod can use to reach the machine running the
// tests: the host alias the container provider injects into the node, else the
// node's default gateway.
func (c *cluster) findHostIP() (string, error) {
	for _, alias := range []string{"host.containers.internal", "host.docker.internal"} {
		if out, err := run(c.provider, "exec", c.node, "getent", "hosts", alias); err == nil && out != "" {
			return strings.Fields(out)[0], nil
		}
	}
	out, err := run(c.provider, "exec", c.node, "sh", "-c", "ip route show default | awk '{print $3; exit}'")
	if err != nil || out == "" {
		return "", fmt.Errorf("cannot determine the host address from the node: %v", err)
	}
	return out, nil
}

// loadImage builds the image, saves it as an OCI archive, loads it into the
// node's containerd and returns <repo>@sha256:<digest of the archive index>,
// the reference containerd resolves for the imported image.
func (c *cluster) loadImage(dir, repo string, containerfile []byte) (string, error) {
	cf := filepath.Join(dir, filepath.Base(repo)+".Containerfile")
	if err := os.WriteFile(cf, containerfile, 0o644); err != nil {
		return "", err
	}
	tag := repo + ":" + c.id
	onCleanup(func() { _, _ = run(c.provider, "rmi", "-f", tag) })
	if _, err := run(c.provider, "build", "-q", "-t", tag, "-f", cf, dir); err != nil {
		return "", err
	}
	archive := filepath.Join(dir, filepath.Base(repo)+".tar")
	args := []string{"save", "-o", archive, tag}
	if c.provider == "podman" {
		args = []string{"save", "--format", "oci-archive", "-o", archive, tag}
	}
	if _, err := run(c.provider, args...); err != nil {
		return "", err
	}
	index, err := run("tar", "-xOf", archive, "index.json")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(index))
	ref := repo + "@sha256:" + hex.EncodeToString(sum[:])
	// The import registers the digest reference and the tag; remove both names.
	onCleanup(func() { _, _ = run(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "rm", ref, tag) })
	if listing, err := run(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "ls", "-q"); err == nil {
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
	listing, err := run(c.provider, "exec", c.node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil || !strings.Contains(listing, ref) {
		return "", fmt.Errorf("image %s is not resolvable in the node after loading", ref)
	}
	return ref, nil
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
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
