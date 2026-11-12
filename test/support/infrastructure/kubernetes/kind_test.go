package kubesupport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// recordedCommand is one fake process invocation.
type recordedCommand struct {
	Name string
	Args []string
	Env  []string
}

// fakeRunner is the only "kind" and "kubectl" these unit tests touch.
type fakeRunner struct {
	mu       sync.Mutex
	commands []recordedCommand
	handler  func(harness.Command) (harness.Result, error)
}

// Run implements harness.Runner.
func (f *fakeRunner) Run(_ context.Context, command harness.Command) (harness.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, recordedCommand{
		Name: command.Name,
		Args: append([]string(nil), command.Args...),
		Env:  append([]string(nil), command.Env...),
	})
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		return handler(command)
	}
	return harness.Result{}, nil
}

func (f *fakeRunner) snapshot() []recordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCommand(nil), f.commands...)
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

// fakeClock advances only when a barrier sleeps, so WaitReady never waits in
// wall-clock time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
	return nil
}

func fastWaitOptions() harness.WaitOptions {
	return harness.WaitOptions{
		Timeout:        50 * time.Millisecond,
		AttemptTimeout: 50 * time.Millisecond,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
}

const kindVersionOutput = "kind v0.33.0 go1.26.7 linux/amd64\n"

// newTestKind builds a Kind whose runner answers the version check and then
// delegates to handler.
func newTestKind(t *testing.T, handler func(harness.Command) (harness.Result, error)) (*Kind, *fakeRunner) {
	t.Helper()
	return newEnvTestKind(t, []string{"PATH=/usr/bin:/bin", "HOME=/tmp", ProviderEnv}, handler)
}

// newEnvTestKind builds a Kind with an explicit command environment.
func newEnvTestKind(t *testing.T, env []string, handler func(harness.Command) (harness.Result, error)) (*Kind, *fakeRunner) {
	t.Helper()
	runner := &fakeRunner{}
	runner.handler = func(command harness.Command) (harness.Result, error) {
		if command.Name == "/fake/kind" && len(command.Args) == 1 && command.Args[0] == "version" {
			return harness.Result{Stdout: []byte(kindVersionOutput)}, nil
		}
		if handler != nil {
			return handler(command)
		}
		return harness.Result{}, nil
	}
	kind, err := NewKind(
		WithBinary("/fake/kind"),
		WithRunner(runner),
		WithEnv(env),
		WithClock(newFakeClock()),
		WithWaitOptions(fastWaitOptions()),
		WithLogger(t.Logf),
	)
	if err != nil {
		t.Fatalf("NewKind: %v", err)
	}
	return kind, runner
}

// createHandler answers `kind create cluster` and writes the kubeconfig the way
// kind does.
func createHandler(t *testing.T, calls *int) func(harness.Command) (harness.Result, error) {
	t.Helper()
	return func(command harness.Command) (harness.Result, error) {
		if command.Name != "/fake/kind" || len(command.Args) == 0 || command.Args[0] != "create" {
			return harness.Result{}, nil
		}
		if calls != nil {
			*calls++
		}
		for index := 0; index+1 < len(command.Args); index++ {
			if command.Args[index] == "--kubeconfig" {
				if err := os.WriteFile(command.Args[index+1], []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
					return harness.Result{}, err
				}
			}
		}
		return harness.Result{}, nil
	}
}

func TestParseKindVersion(t *testing.T) {
	cases := map[string]string{
		"kind v0.33.0 go1.26.7 linux/amd64\n": "0.33.0",
		"kind v0.29.0 go1.23.0 linux/amd64":   "0.29.0",
		"":                                    "",
		"unexpected":                          "",
	}
	for output, want := range cases {
		if got := parseKindVersion(output); got != want {
			t.Errorf("parseKindVersion(%q) = %q; want %q", output, got, want)
		}
	}
}

// TestNewKindPinsVersionAndEnvironment proves the version check runs with an
// explicit provider environment and that a mismatch is refused.
func TestNewKindPinsVersionAndEnvironment(t *testing.T) {
	kind, runner := newTestKind(t, nil)
	if kind.Binary() != "/fake/kind" {
		t.Fatalf("binary = %q; want /fake/kind", kind.Binary())
	}
	commands := runner.snapshot()
	if len(commands) != 1 || commands[0].Name != "/fake/kind" || len(commands[0].Args) != 1 || commands[0].Args[0] != "version" {
		t.Fatalf("version commands = %+v", commands)
	}
	if !containsString(commands[0].Env, ProviderEnv) {
		t.Fatalf("environment = %v; want %s", commands[0].Env, ProviderEnv)
	}
	for _, entry := range commands[0].Env {
		if strings.HasPrefix(entry, "KUBECONFIG=") {
			t.Fatalf("environment leaks KUBECONFIG: %v", commands[0].Env)
		}
	}

	runner.handler = func(harness.Command) (harness.Result, error) {
		return harness.Result{Stdout: []byte("kind v0.29.0 go1.23.0 linux/amd64\n")}, nil
	}
	if _, err := NewKind(WithBinary("/fake/kind"), WithRunner(runner), WithEnv([]string{"PATH=/usr/bin"}), WithLogger(t.Logf)); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("version mismatch error = %v; want ErrKindUnavailable", err)
	}
}

// TestNewClusterNameIsScopedAndUnique proves generated names carry the prefix
// and owner pid and that protected or foreign names are refused.
func TestNewClusterNameIsScopedAndUnique(t *testing.T) {
	pattern := regexp.MustCompile(`^kodama-t-[0-9]+-[0-9a-f]{8}$`)
	first := NewClusterName()
	second := NewClusterName()
	if !pattern.MatchString(first) {
		t.Fatalf("generated name %q does not match %s", first, pattern)
	}
	if first == second {
		t.Fatalf("generated names collide: %q", first)
	}
	if !strings.HasPrefix(first, ClusterPrefix+strconv.Itoa(os.Getpid())+"-") {
		t.Fatalf("generated name %q does not carry the owner pid", first)
	}
	for _, name := range []string{ProtectedCluster, "kind", "kodama-t-abc", "kodama-t-0-abcdef01", "kodama-t-12-ABCDEF01", ""} {
		if err := validateClusterName(name); err == nil {
			t.Errorf("validateClusterName(%q) = nil; want a refusal", name)
		}
	}
	if err := validateClusterName(first); err != nil {
		t.Fatalf("validateClusterName(%q) = %v", first, err)
	}
}

// TestKindConfigUsesOnlyTheRootlessFeatureGate proves the harness cluster is
// stock with respect to pod logs: the rootless kubelet gate stays, and the alpha
// PodLogsQuerySplitStreams gate is absent so the component suites exercise the
// one combined log real clusters serve.
func TestKindConfigUsesOnlyTheRootlessFeatureGate(t *testing.T) {
	config := clusterConfig("docker.io/kindest/node:pinned")
	if !strings.Contains(config, "KubeletInUserNamespace: true") {
		t.Fatalf("kind config lost the rootless kubelet gate:\n%s", config)
	}
	if strings.Contains(config, "PodLogsQuerySplitStreams") {
		t.Fatalf("kind config still enables the alpha pod-log split-stream gate:\n%s", config)
	}
}

// TestCreateClusterPinsConfigArgumentsAndEnvironment proves the create command,
// the pinned feature gates and node image, and the isolated kubeconfig.
func TestCreateClusterPinsConfigArgumentsAndEnvironment(t *testing.T) {
	createCalls := 0
	kind, runner := newTestKind(t, createHandler(t, &createCalls))
	dir := t.TempDir()
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: dir})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("create calls = %d; want 1", createCalls)
	}
	commands := runner.snapshot()
	create := commands[len(commands)-1]
	wantArgs := []string{
		"create", "cluster",
		"--name", cluster.Name,
		"--config", filepath.Join(dir, "kind-config.yaml"),
		"--kubeconfig", filepath.Join(dir, "kubeconfig"),
		"--wait", "0s",
	}
	if strings.Join(create.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("create args = %v; want %v", create.Args, wantArgs)
	}
	if !containsString(create.Env, ProviderEnv) {
		t.Fatalf("create environment = %v; want %s", create.Env, ProviderEnv)
	}

	config, err := os.ReadFile(filepath.Join(dir, "kind-config.yaml"))
	if err != nil {
		t.Fatalf("read kind config: %v", err)
	}
	wantConfig := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  KubeletInUserNamespace: true
nodes:
  - role: control-plane
    image: docker.io/kindest/node:v1.37.0@sha256:1aac8018c42eb5d48fae5be507caaee789fc6d9b9b3f560359dd4157ac00d4f6
`
	if string(config) != wantConfig {
		t.Fatalf("kind config =\n%s\nwant:\n%s", config, wantConfig)
	}
	if _, err := os.Stat(cluster.KubeconfigPath); err != nil {
		t.Fatalf("isolated kubeconfig is missing: %v", err)
	}

	if err := cluster.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	commands = runner.snapshot()
	deleteCommand := commands[len(commands)-1]
	wantDelete := []string{"delete", "cluster", "--name", cluster.Name, "--kubeconfig", cluster.KubeconfigPath}
	if strings.Join(deleteCommand.Args, " ") != strings.Join(wantDelete, " ") {
		t.Fatalf("delete args = %v; want %v", deleteCommand.Args, wantDelete)
	}
	if err := cluster.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if count := countArgs(runner.snapshot(), "delete"); count != 1 {
		t.Fatalf("delete ran %d times; want 1", count)
	}
	if len(ActiveClusters()) != 0 {
		t.Fatalf("closed cluster is still registered")
	}
}

// TestCreateClusterRefusesProtectedOrForeignName proves the suite can never
// create or touch a cluster it does not own.
func TestCreateClusterRefusesProtectedOrForeignName(t *testing.T) {
	kind, runner := newTestKind(t, nil)
	for _, name := range []string{ProtectedCluster, "kind", "prod", "kodama-t-1-nothex00"} {
		if _, err := kind.CreateCluster(context.Background(), ClusterOptions{Name: name, Dir: t.TempDir()}); err == nil {
			t.Errorf("CreateCluster(%q) = nil; want a refusal", name)
		}
	}
	if count := countArgs(runner.snapshot(), "create"); count != 0 {
		t.Fatalf("refused names ran %d create commands; want 0", count)
	}
}

// TestKubeconfigJSONUsesIsolatedKubeconfig proves the export command and that a
// malformed export is refused.
func TestKubeconfigJSONUsesIsolatedKubeconfig(t *testing.T) {
	export := `{"apiVersion":"v1","kind":"Config","clusters":[{"name":"test","cluster":{"server":"https://127.0.0.1:6443"}}],"users":[],"contexts":[]}`
	kind, runner := newTestKind(t, func(command harness.Command) (harness.Result, error) {
		if command.Name == "kubectl" {
			return harness.Result{Stdout: []byte(export)}, nil
		}
		return createHandler(t, nil)(command)
	})
	dir := t.TempDir()
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: dir})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Close() })

	document, err := cluster.KubeconfigJSON(context.Background())
	if err != nil {
		t.Fatalf("KubeconfigJSON: %v", err)
	}
	if string(document) != export {
		t.Fatalf("kubeconfig JSON = %q; want the exported document", document)
	}
	commands := runner.snapshot()
	exportCommand := commands[len(commands)-1]
	wantArgs := []string{
		"--kubeconfig", filepath.Join(dir, "kubeconfig"),
		"config", "view", "--raw", "--minify", "-o", "json",
	}
	if strings.Join(exportCommand.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("export args = %v; want %v", exportCommand.Args, wantArgs)
	}

	runner.handler = func(command harness.Command) (harness.Result, error) {
		if command.Name == "kubectl" {
			return harness.Result{Stdout: []byte("not json")}, nil
		}
		return createHandler(t, nil)(command)
	}
	if _, err := cluster.KubeconfigJSON(context.Background()); err == nil {
		t.Fatal("KubeconfigJSON accepted a malformed document")
	}
}

// TestWaitReadyRequiresNodeServiceAccountAndStorageClass proves the barrier
// observes all three prerequisites and names the first missing one.
func TestWaitReadyRequiresNodeServiceAccountAndStorageClass(t *testing.T) {
	nodeReady := false
	serviceAccountReady := false
	storageClassReady := false
	kind, runner := newTestKind(t, func(command harness.Command) (harness.Result, error) {
		if command.Name != "kubectl" {
			return createHandler(t, nil)(command)
		}
		switch {
		case hasArgs(command.Args, "get", "nodes"):
			status := "False"
			if nodeReady {
				status = "True"
			}
			return harness.Result{Stdout: []byte(`{"items":[{"metadata":{"name":"control-plane"},"status":{"conditions":[{"type":"Ready","status":"` + status + `"}]}}]}`)}, nil
		case hasArgs(command.Args, "get", "serviceaccounts", "default"):
			name := ""
			if serviceAccountReady {
				name = "default"
			}
			return harness.Result{Stdout: []byte(`{"metadata":{"name":"` + name + `"}}`)}, nil
		case hasArgs(command.Args, "get", "storageclass", StorageClassName):
			if !storageClassReady {
				return harness.Result{}, errors.New("storageclass not found")
			}
			return harness.Result{Stdout: []byte(`{"metadata":{"name":"standard"},"provisioner":"rancher.io/local-path"}`)}, nil
		default:
			return harness.Result{}, nil
		}
	})
	dir := t.TempDir()
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: dir, ReadyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Close() })

	if err := cluster.WaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "node readiness") {
		t.Fatalf("not-ready barrier error = %v; want a node readiness failure", err)
	}
	nodeReady = true
	if err := cluster.WaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "default serviceaccount") {
		t.Fatalf("missing service account error = %v; want a default serviceaccount failure", err)
	}
	serviceAccountReady = true
	if err := cluster.WaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "storageclass standard") {
		t.Fatalf("missing storage class error = %v; want a storageclass failure", err)
	}
	storageClassReady = true
	if err := cluster.WaitReady(context.Background()); err != nil {
		t.Fatalf("ready cluster barrier = %v", err)
	}

	for _, command := range runner.snapshot() {
		if command.Name != "kubectl" {
			continue
		}
		if len(command.Args) < 2 || command.Args[0] != "--kubeconfig" {
			t.Fatalf("kubectl command without an explicit kubeconfig: %v", command.Args)
		}
		if !containsString(command.Env, ProviderEnv) {
			t.Fatalf("kubectl environment = %v; want the explicit provider environment", command.Env)
		}
	}
}

// TestDeleteClusterUsesPrivateKubeconfig proves every destructive kind command
// names a private --kubeconfig. Without the flag kind falls back to $KUBECONFIG
// or $HOME/.kube/config, so a delete could rewrite the host kubeconfig while
// other kind runs are using it. Normal close, failed-create cleanup and the
// orphan sweep are all covered.
func TestDeleteClusterUsesPrivateKubeconfig(t *testing.T) {
	home := t.TempDir()
	sentinel := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatalf("create sentinel kubeconfig directory: %v", err)
	}
	const sentinelBody = "apiVersion: v1\nkind: Config\ncurrent-context: host-sentinel\nclusters: []\ncontexts: []\nusers: []\n"
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o600); err != nil {
		t.Fatalf("write sentinel kubeconfig: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", sentinel)

	type deleteObservation struct {
		name             string
		kubeconfig       string
		configDirExisted bool
	}
	var observations []deleteObservation
	failCreate := false
	orphan := fmt.Sprintf("%s%d-abcdef01", ClusterPrefix, 999999)
	handler := func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "create", "cluster"):
			if failCreate {
				return harness.Result{}, errors.New("kind: create timed out")
			}
			return createHandler(t, nil)(command)
		case hasArgs(command.Args, "get", "clusters"):
			return harness.Result{Stdout: []byte(orphan + "\n")}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			kubeconfig := argValue(command.Args, "--kubeconfig")
			_, statErr := os.Stat(filepath.Dir(kubeconfig))
			observations = append(observations, deleteObservation{
				name:             argValue(command.Args, "--name"),
				kubeconfig:       kubeconfig,
				configDirExisted: kubeconfig != "" && statErr == nil,
			})
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	}
	kind, _ := newEnvTestKind(t, []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"KUBECONFIG=" + sentinel,
		ProviderEnv,
	}, handler)

	// 1. A normal close deletes with the cluster's own private kubeconfig.
	dir := t.TempDir()
	cluster, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: dir})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if err := cluster.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2. A failed create best-effort deletes with the private path it would
	// have written.
	failCreate = true
	failedDir := t.TempDir()
	if _, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: failedDir}); err == nil {
		t.Fatal("CreateCluster returned nil for a failed create")
	}
	failCreate = false

	// 3. The dead-owner sweep has no cluster directory to reuse, yet it must
	// still name a private kubeconfig.
	removed, err := kind.SweepDeadOwners(context.Background())
	if err != nil {
		t.Fatalf("SweepDeadOwners: %v", err)
	}
	if len(removed) != 1 || removed[0] != orphan {
		t.Fatalf("SweepDeadOwners removed %v; want %q", removed, orphan)
	}

	if len(observations) != 3 {
		t.Fatalf("destructive kind commands = %d; want 3 (%+v)", len(observations), observations)
	}
	failedCreateKubeconfig := filepath.Join(failedDir, "kubeconfig")
	var sawClose, sawFailedCreate, sawOrphan bool
	for _, observation := range observations {
		if observation.kubeconfig == "" {
			t.Fatalf("destructive kind command for %q has no --kubeconfig", observation.name)
		}
		if filepath.Clean(observation.kubeconfig) == filepath.Clean(sentinel) {
			t.Fatalf("destructive kind command for %q uses the host kubeconfig %s", observation.name, sentinel)
		}
		if strings.HasPrefix(observation.kubeconfig, home+string(os.PathSeparator)) {
			t.Fatalf("destructive kind command for %q uses a path under the host HOME: %s", observation.name, observation.kubeconfig)
		}
		if !observation.configDirExisted {
			t.Fatalf("private kubeconfig directory did not exist when deleting %q: %s", observation.name, observation.kubeconfig)
		}
		switch observation.kubeconfig {
		case cluster.KubeconfigPath:
			sawClose = true
		case failedCreateKubeconfig:
			sawFailedCreate = true
		default:
			if observation.name == orphan {
				sawOrphan = true
			}
		}
	}
	if !sawClose {
		t.Fatalf("normal close did not use the cluster kubeconfig %s: %+v", cluster.KubeconfigPath, observations)
	}
	if !sawFailedCreate {
		t.Fatalf("failed-create cleanup did not use %s: %+v", failedCreateKubeconfig, observations)
	}
	if !sawOrphan {
		t.Fatalf("orphan deletion did not use a private kubeconfig: %+v", observations)
	}

	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel kubeconfig: %v", err)
	}
	if string(body) != sentinelBody {
		t.Fatalf("host kubeconfig was modified through a kind delete:\n%s", body)
	}
}

// TestSweepDeadOwnersDeletesOnlyDeadGeneratedClusters proves the next-run sweep
// never touches a live-owner, protected or foreign cluster.
func TestSweepDeadOwnersDeletesOnlyDeadGeneratedClusters(t *testing.T) {
	alive := map[int]bool{123: true, 999999: false}
	var deleted []string
	kind, runner := newTestKind(t, func(command harness.Command) (harness.Result, error) {
		switch {
		case hasArgs(command.Args, "get", "clusters"):
			return harness.Result{Stdout: []byte(strings.Join([]string{
				ProtectedCluster,
				"kodama-t-123-abcdef01",
				"kodama-t-999999-abcdef01",
				"foreign-cluster",
				"kodama-t-777777-ABCDEF01",
			}, "\n") + "\n")}, nil
		case hasArgs(command.Args, "delete", "cluster"):
			deleted = append(deleted, argValue(command.Args, "--name"))
			return harness.Result{}, nil
		default:
			return harness.Result{}, nil
		}
	})
	kind.processAlive = func(pid int) bool { return alive[pid] }

	removed, err := kind.SweepDeadOwners(context.Background())
	if err != nil {
		t.Fatalf("SweepDeadOwners: %v", err)
	}
	if len(removed) != 1 || removed[0] != "kodama-t-999999-abcdef01" {
		t.Fatalf("removed = %v; want only the dead-owner cluster", removed)
	}
	if len(deleted) != 1 || deleted[0] != "kodama-t-999999-abcdef01" {
		t.Fatalf("delete commands = %v; want only the dead-owner cluster", deleted)
	}
	for _, command := range runner.snapshot() {
		if hasArgs(command.Args, "delete", "cluster") {
			for _, forbidden := range []string{ProtectedCluster, "foreign-cluster"} {
				if strings.Contains(strings.Join(command.Args, " "), forbidden) {
					t.Fatalf("sweep touched %q: %v", forbidden, command.Args)
				}
			}
		}
	}
}

// TestDownloadVerifiesDigestBeforeInstalling proves a mismatched download never
// becomes executable and a cached binary is re-verified.
func TestDownloadVerifiesDigestBeforeInstalling(t *testing.T) {
	fixture := []byte("kind-binary-fixture")
	sum := sha256.Sum256(fixture)
	digest := hex.EncodeToString(sum[:])
	cache := t.TempDir()
	fetched := 0
	kind := &Kind{
		cacheDir:       cache,
		expectedDigest: digest,
		logf:           t.Logf,
		fetcher: func(context.Context, string) ([]byte, error) {
			fetched++
			return fixture, nil
		},
	}

	path, err := kind.download(context.Background())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if fetched != 1 {
		t.Fatalf("fetch calls = %d; want 1", fetched)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat downloaded binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("downloaded binary mode = %v; want executable", info.Mode())
	}
	if _, err := kind.download(context.Background()); err != nil {
		t.Fatalf("cached download: %v", err)
	}
	if fetched != 1 {
		t.Fatalf("cached download fetched again (%d); want a verified cache hit", fetched)
	}

	badCache := t.TempDir()
	bad := &Kind{
		cacheDir:       badCache,
		expectedDigest: strings.Repeat("0", 64),
		logf:           t.Logf,
		fetcher: func(context.Context, string) ([]byte, error) {
			return fixture, nil
		},
	}
	if _, err := bad.download(context.Background()); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("mismatched digest error = %v; want ErrKindUnavailable", err)
	}
	entries, err := os.ReadDir(badCache)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("mismatched download left %d files in the cache", len(entries))
	}

	tampered := filepath.Join(badCache, "kind-"+KindVersion+"-linux-amd64")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o755); err != nil {
		t.Fatalf("write tampered cache file: %v", err)
	}
	if _, err := bad.download(context.Background()); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("tampered cache error = %v; want ErrKindUnavailable", err)
	}
}

// TestCleanupActiveClustersDeletesRegisteredClusters proves the signal path
// deletes every active cluster.
func TestCleanupActiveClustersDeletesRegisteredClusters(t *testing.T) {
	kind, runner := newTestKind(t, createHandler(t, nil))
	for range 2 {
		if _, err := kind.CreateCluster(context.Background(), ClusterOptions{Dir: t.TempDir()}); err != nil {
			t.Fatalf("CreateCluster: %v", err)
		}
	}
	if len(ActiveClusters()) != 2 {
		t.Fatalf("active clusters = %d; want 2", len(ActiveClusters()))
	}
	if err := CleanupActiveClusters(context.Background()); err != nil {
		t.Fatalf("CleanupActiveClusters: %v", err)
	}
	if len(ActiveClusters()) != 0 {
		t.Fatalf("active clusters remain after cleanup")
	}
	if count := countArgs(runner.snapshot(), "delete"); count != 2 {
		t.Fatalf("delete commands = %d; want 2", count)
	}
}

// TestKindPinsMatchVersionDigestAndNodeImage pins the release constants that
// the download, cache and cluster-config paths share: the URL names the pinned
// kind release and platform asset, the digest is a full lowercase SHA-256, the
// node image tag carries the Kubernetes version, the image digest is a real
// sha256 reference, and the reference passed to kind is tag@digest.
func TestKindPinsMatchVersionDigestAndNodeImage(t *testing.T) {
	if !strings.Contains(KindDownloadURL, "/v"+KindVersion+"/") {
		t.Errorf("KindDownloadURL %q does not contain the pinned release /v%s/", KindDownloadURL, KindVersion)
	}
	const platformAsset = "kind-linux-amd64"
	if !strings.HasSuffix(KindDownloadURL, "/"+platformAsset) {
		t.Errorf("KindDownloadURL %q does not end with the platform asset %q", KindDownloadURL, platformAsset)
	}

	sha256Pattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !sha256Pattern.MatchString(KindSHA256) {
		t.Errorf("KindSHA256 %q is not 64 lowercase hex characters", KindSHA256)
	}

	colon := strings.LastIndex(NodeImage, ":")
	if colon < 0 || colon == len(NodeImage)-1 {
		t.Fatalf("NodeImage %q has no tag", NodeImage)
	}
	tag := NodeImage[colon+1:]
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(tag) {
		t.Errorf("NodeImage tag %q is not a v-prefixed Kubernetes version", tag)
	}
	if tag != "v1.37.0" {
		t.Errorf("NodeImage tag = %q, want the pinned Kubernetes version v1.37.0", tag)
	}

	digestPattern := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	if !digestPattern.MatchString(NodeImageDigest) {
		t.Errorf("NodeImageDigest %q is not a sha256:<64 lowercase hex> digest", NodeImageDigest)
	}
	if NodeImageReference != NodeImage+"@"+NodeImageDigest {
		t.Errorf("NodeImageReference = %q, want %q", NodeImageReference, NodeImage+"@"+NodeImageDigest)
	}
}

// containsString reports whether values contains want.
func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

// hasArgs reports whether args contains the subsequence want.
func hasArgs(args []string, want ...string) bool {
	for index := 0; index+len(want) <= len(args); index++ {
		matched := true
		for offset, candidate := range want {
			if args[index+offset] != candidate {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// countArgs counts commands whose first argument is name.
func countArgs(commands []recordedCommand, name string) int {
	count := 0
	for _, command := range commands {
		if len(command.Args) > 0 && command.Args[0] == name {
			count++
		}
	}
	return count
}
