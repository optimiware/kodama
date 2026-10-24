package harnessopencode_test

// The kind cluster, registry and image fixtures of the T-031 acceptance
// composition. Nothing is skipped: a missing cluster, registry, container
// provider, OpenCode binary or image build fails the test.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
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

// debianPin is the digest-pinned base of the injector image. It is the same
// pinned base the wrapper image builds on (images/harness-opencode/Containerfile).
const debianPin = "docker.io/library/debian@sha256:5ae3c39ebd15e229dcedd5cee596b2497182493d41ff162e824ba13fc1b2b867"

type cluster struct {
	kubeconfig string
	client     *kubernetes.Client
	provider   string
	node       string
	hostIP     string
	registry   string // host:port the kubelet and the host both address, e.g. localhost:33067
	scope      string // registry namespace, e.g. t-batch
	injector   string // digest-pinned references Pods can pull
	wrapper    string
	build      buildRecord
	id         string
}

// buildRecord is what images/harness-opencode/build.sh recorded.
type buildRecord struct {
	OpenCodeVersion string `json:"opencodeVersion"`
	OpenCodeSHA256  string `json:"opencodeSha256"`
	WrapperSHA256   string `json:"wrapperSha256"`
	LocalImageID    string `json:"localImageId"`
	RegistryRef     string `json:"registryRef"`
	PushedDigest    string `json:"pushedDigest"`
	Packages        string `json:"packages"`
}

var (
	clusterOnce sync.Once
	logOnce     sync.Once
	clusterVal  *cluster
	clusterErr  error

	cleanupMu sync.Mutex
	cleanups  []func()
)

func onCleanup(f func()) {
	cleanupMu.Lock()
	cleanups = append(cleanups, f)
	cleanupMu.Unlock()
}

func requireCluster(t *testing.T) *cluster {
	t.Helper()
	kc := kubetest.Require(t)
	clusterOnce.Do(func() { clusterVal, clusterErr = setupCluster(kc) })
	if clusterErr != nil {
		t.Fatalf("cluster fixture: %v", clusterErr)
	}
	logOnce.Do(func() {
		t.Logf("wrapper image %s (opencode %s sha256 %s, wrapper sha256 %s); injector %s; hostIP %s; node %s",
			clusterVal.wrapper, clusterVal.build.OpenCodeVersion, clusterVal.build.OpenCodeSHA256, clusterVal.build.WrapperSHA256, clusterVal.injector, clusterVal.hostIP, clusterVal.node)
	})
	return clusterVal
}

func run(name string, args ...string) (string, error) {
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

func mustEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required (source .harness/test-env.sh)", name)
	}
	return v, nil
}

func setupCluster(kc string) (*cluster, error) {
	c := &cluster{kubeconfig: kc}
	var err error
	if c.client, err = kubetest.NewClient(kc); err != nil {
		return nil, err
	}
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
	c.node = strings.TrimPrefix(ctxName, "kind-") + "-control-plane"
	if _, err := run(c.provider, "inspect", "--format", "{{.State.Running}}", c.node); err != nil {
		return nil, fmt.Errorf("the kind node %s is not available through %s: %w", c.node, c.provider, err)
	}
	if c.hostIP, err = c.findHostIP(); err != nil {
		return nil, err
	}
	reg, err := mustEnv("KODAMA_REGISTRY_URL")
	if err != nil {
		return nil, err
	}
	src, err := mustEnv("KODAMA_REGISTRY_SOURCE")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(reg)
	if err != nil || u.Port() == "" {
		return nil, fmt.Errorf("KODAMA_REGISTRY_URL %q has no port", reg)
	}
	c.registry = "localhost:" + u.Port()
	c.scope, _, _ = strings.Cut(src, "/")
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	c.id = hex.EncodeToString(idb)

	build, err := os.MkdirTemp("", "kodama-t031-images-"+c.id+"-")
	if err != nil {
		return nil, err
	}
	onCleanup(func() { _ = os.RemoveAll(build) })

	// The wrapper image: the real build script, which verifies the OpenCode
	// binary's checksum, builds with podman and pushes to the registry.
	repo := fmt.Sprintf("%s/%s/t031/harness-opencode", c.registry, c.scope)
	cmd := exec.Command(filepath.Join(repoRoot(), "images", "harness-opencode", "build.sh"), "--push", repo, "--out", build)
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the wrapper image: %w\n%s", err, tail(out, 3000))
	}
	raw, err := os.ReadFile(filepath.Join(build, "build.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &c.build); err != nil || c.build.RegistryRef == "" || !strings.Contains(c.build.RegistryRef, "@sha256:") {
		return nil, fmt.Errorf("build.json: %v", err)
	}
	c.wrapper = c.build.RegistryRef

	// The entrypoint injector: the actual `kodama step` binary on the same pinned base.
	arch, err := run("kubectl", "--kubeconfig", kc, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(build, "kodama")
	gb := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/harness-opencode/stepbin")
	gb.Dir = repoRoot()
	gb.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := gb.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the entrypoint: %w: %s", err, out)
	}
	cf := filepath.Join(build, "injector.Containerfile")
	if err := os.WriteFile(cf, []byte("FROM "+debianPin+"\nCOPY kodama /usr/local/bin/kodama\n"), 0o644); err != nil {
		return nil, err
	}
	tag := "localhost/kodama-t031-injector:" + c.id
	onCleanup(func() { _, _ = run(c.provider, "rmi", "-f", tag) })
	if _, err := run(c.provider, "build", "-q", "-t", tag, "-f", cf, build); err != nil {
		return nil, err
	}
	irepo := fmt.Sprintf("%s/%s/t031/injector", c.registry, c.scope)
	df := filepath.Join(build, "injector.digest")
	if _, err := run(c.provider, "push", "--tls-verify=false", "--digestfile", df, tag, "docker://"+irepo+":"+c.id); err != nil {
		return nil, err
	}
	dg, err := os.ReadFile(df)
	if err != nil {
		return nil, err
	}
	c.injector = irepo + "@" + strings.TrimSpace(string(dg))
	return c, nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// findHostIP returns the address a Pod can use to reach the machine running the
// tests.
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
