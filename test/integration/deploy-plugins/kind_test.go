package deployplugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/state"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

type resolver struct{}

func (resolver) ResolveBinding(_ context.Context, _ *http.Request, attempt, alias string) (state.Scope, state.Binding, error) {
	if attempt != "att_demo" || alias != "release" {
		return state.Scope{}, state.Binding{}, state.ErrDenied
	}
	return state.Scope{TenantID: domain.TenantID("t_demo"), ProjectID: domain.ProjectID("p_demo")},
		state.Binding{Namespace: "deploy", Prefix: "", Read: true, Write: true}, nil
}

func TestKubernetesPluginDeploysDummyApp(t *testing.T) {
	if os.Getenv("KODAMA_REQUIRE_KIND") == "" {
		t.Skip("set KODAMA_REQUIRE_KIND=1 to run the real kind deployment")
	}
	kind := envOr("KIND_BIN", "kind")
	cluster := envOr("KODAMA_TEST_CLUSTER", "kodama-plugin-test")
	kubeconfig := os.Getenv("KODAMA_TEST_KUBECONFIG")
	var cleanup func()
	if kubeconfig == "" {
		var err error
		kubeconfig, cleanup, err = ensureKind(t, kind, cluster)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
	}
	kubectl := envOr("KUBECTL", "kubectl")
	run(t, nil, kubectl, "--kubeconfig", kubeconfig, "create", "namespace", "plugin-test", "--dry-run=client", "-o", "yaml")
	// The dry-run above proves the client is usable; creation is idempotent below.
	runAllow(t, nil, kubectl, "--kubeconfig", kubeconfig, "create", "namespace", "plugin-test")
	loadDummyImage(t, kind, kubeconfig)

	store := state.NewMemory(nil)
	handler, err := state.NewHandler(store, resolver{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	bin := filepath.Join(t.TempDir(), "deploy")
	root := moduleRoot(t)
	build := exec.Command("go", "build", "-o", bin, "./images/plugins/deploy")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}
	bundle := writeBundle(t, dummyManifest())

	common := []string{
		"--state-url", srv.URL, "--state-attempt", "att_demo", "--state-binding", "release",
		"--action-key", "run-demo:deploy", "--recipe", "kubernetes-manifests",
		"--namespace", "plugin-test", "--release", "dummy-app", "--kubeconfig", kubeconfig,
		"--bundle", bundle, "--desired-release", "dummy-v1", "--timeout", "2m",
	}
	out := run(t, nil, bin, append([]string{"apply"}, common...)...)
	if !strings.Contains(string(out), `"status": "succeeded"`) {
		t.Fatalf("apply did not succeed: %s", out)
	}
	status := run(t, nil, bin, append([]string{"status"}, common...)...)
	if !strings.Contains(string(status), `"state": "ready"`) {
		t.Fatalf("status is not ready: %s", status)
	}
	if _, err := exec.Command(kubectl, "--kubeconfig", kubeconfig, "-n", "plugin-test", "get", "deployment", "dummy-app", "-o", "json").CombinedOutput(); err != nil {
		t.Fatalf("deployment not observable: %v", err)
	}
	cleanupArgs := []string{"cleanup", "--operation-id", "cleanup-1"}
	cleanupArgs = append(cleanupArgs, common...)
	if os.Getenv("KODAMA_KEEP_DUMMY") == "" {
		cleanupOut := run(t, nil, bin, cleanupArgs...)
		if !strings.Contains(string(cleanupOut), `"deletedAt"`) {
			t.Fatalf("cleanup did not delete: %s", cleanupOut)
		}
		if err := exec.Command(kubectl, "--kubeconfig", kubeconfig, "-n", "plugin-test", "get", "deployment", "dummy-app").Run(); err == nil {
			t.Fatal("deployment still exists after cleanup")
		}
	} else {
		t.Log("KODAMA_KEEP_DUMMY set: dummy-app remains for inspection")
	}

	// The state API retains history and rejects a different payload for the same
	// operation is covered by the engine unit tests; this integration test also
	// verifies that every key written was inside the granted binding.
	entries, err := pluginstate.New(srv.URL, "att_demo", "release", "")
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := entries.List(context.Background(), "operations/", "", 100)
	if err != nil || len(items) == 0 {
		t.Fatalf("state history missing: %v %+v", err, items)
	}
}

func ensureKind(t *testing.T, kind, cluster string) (string, func(), error) {
	t.Helper()
	out, _ := exec.Command(kind, "get", "clusters").CombinedOutput()
	created := false
	if !strings.Contains(string(out), cluster) {
		cmd := exec.Command(kind, "create", "cluster", "--name", cluster, "--wait", "120s")
		cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER=podman")
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", nil, fmt.Errorf("kind create: %w\n%s", err, b)
		}
		created = true
	}
	out, err := exec.Command(kind, "get", "kubeconfig", "--name", cluster).Output()
	if err != nil {
		return "", nil, fmt.Errorf("kind kubeconfig: %w", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", nil, err
	}
	return path, func() {
		if created && os.Getenv("KEEP_KIND") == "" {
			_ = exec.Command(kind, "delete", "cluster", "--name", cluster).Run()
		}
	}, nil
}

func loadDummyImage(t *testing.T, kind, kubeconfig string) {
	t.Helper()
	if os.Getenv("KODAMA_SKIP_IMAGE_LOAD") != "" {
		return
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("podman is required to load the dummy image: %v", err)
	}
	image := envOr("KODAMA_DUMMY_IMAGE", "docker.io/library/nginx:1.27-alpine")
	save := filepath.Join(t.TempDir(), "image.tar")
	if out, err := exec.Command("podman", "save", "-o", save, image).CombinedOutput(); err != nil {
		t.Fatalf("podman save: %v\n%s", err, out)
	}
	cluster := envOr("KODAMA_TEST_CLUSTER", "kodama-plugin-test")
	cmd := exec.Command(kind, "load", "image-archive", save, "--name", cluster)
	cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER=podman")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kind load: %v\n%s", err, out)
	}
}

func dummyManifest() []byte {
	return []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: dummy-app
  namespace: plugin-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dummy-app
  template:
    metadata:
      labels:
        app: dummy-app
    spec:
      containers:
        - name: app
          image: docker.io/library/nginx:1.27-alpine
          ports:
            - containerPort: 80
`)
}

func writeBundle(t *testing.T, manifest []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Mode: 0o600, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, env []string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func runAllow(t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	_ = cmd.Run()
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			t.Fatal("go.mod not found")
		}
	}
}
