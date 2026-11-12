// Package kubesupport runs pinned rootless kind clusters for the
// Kubernetes component suite.
//
// It is test support code under test/, never product code. Every kind and
// kubectl interaction goes through os/exec with an explicit, non-nil
// environment and no shell, so arguments cannot be re-interpreted. The
// harness.Runner seam lets unit tests exercise argument construction, version
// checking, download verification, cluster naming and dead-owner cleanup
// without a real container runtime or cluster.
//
// Scope rules:
//
//   - the kind binary is pinned to KindVersion and verified before use; a
//     missing binary is downloaded from the pinned release URL and accepted only
//     when its SHA-256 matches KindSHA256;
//   - the provider is always rootless Podman (KIND_EXPERIMENTAL_PROVIDER);
//   - every cluster name carries the ClusterPrefix, the owner pid and a random
//     suffix, so a next-run sweep can delete only dead-owner clusters;
//   - the existing cluster named ProtectedCluster is never created, selected or
//     deleted, and neither is any name outside the strict generated pattern;
//   - each cluster gets its own kubeconfig file, so the user's ~/.kube/config
//     and any concurrent shard are never touched.
package kubesupport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// Pinned kind release and node image. These are the single source of truth for
// the Kubernetes component suite; no test may invent a different tag.
const (
	// KindVersion is the pinned kind release.
	KindVersion = "0.33.0"
	// KindSHA256 is the official SHA-256 of the pinned kind-linux-amd64
	// release asset.
	KindSHA256 = "aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d"
	// KindDownloadURL is the pinned official release asset.
	KindDownloadURL = "https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64"

	// NodeImage is the pinned kind node image tag.
	NodeImage = "docker.io/kindest/node:v1.37.0"
	// NodeImageDigest is the pinned node image manifest digest (the linux/amd64
	// manifest, not the multi-platform index digest).
	NodeImageDigest = "sha256:1aac8018c42eb5d48fae5be507caaee789fc6d9b9b3f560359dd4157ac00d4f6"
	// NodeImageReference is the digest-pinned reference passed to kind.
	NodeImageReference = NodeImage + "@" + NodeImageDigest

	// ClusterPrefix scopes every cluster this suite creates.
	ClusterPrefix = "kodama-t-"
	// ProtectedCluster is an existing cluster that must never be touched.
	ProtectedCluster = "kodama-probe"

	// ProviderEnv selects the rootless Podman provider.
	ProviderEnv = "KIND_EXPERIMENTAL_PROVIDER=podman"

	// KindBinaryEnv overrides the kind binary path.
	KindBinaryEnv = "KODAMA_KIND_BIN"
	// KindCacheEnv overrides the download cache directory.
	KindCacheEnv = "KODAMA_KIND_CACHE"

	// StorageClassName is the StorageClass the pinned kind image provisions.
	StorageClassName = "standard"

	// DefaultClusterTimeout bounds one cluster's lifetime.
	DefaultClusterTimeout = 5 * time.Minute
	// DefaultReadyTimeout bounds the readiness barrier.
	DefaultReadyTimeout = 3 * time.Minute
	// DefaultOperationTimeout bounds one kind or kubectl call.
	DefaultOperationTimeout = 2 * time.Minute
	// DefaultDeleteTimeout bounds one cluster deletion.
	DefaultDeleteTimeout = 2 * time.Minute

	// deleteAttempts bounds one Cluster.Close deletion. A transient kind or
	// Podman failure is retried a small number of times; if every attempt
	// fails the cluster stays registered for the post-run sweep.
	deleteAttempts = 3
	// deleteRetryBackoff is the pause between deletion attempts.
	deleteRetryBackoff = 500 * time.Millisecond
)

// ErrKindUnavailable distinguishes a missing or unusable kind binary from a
// failed cluster operation.
var ErrKindUnavailable = errors.New("kubesupport: kind is unavailable")

// errKindBinaryAbsent marks the one cleanup case that may be skipped silently:
// no kind binary exists anywhere, so this process cannot have created a
// cluster. Every other resolution failure (a corrupted cache, a version
// mismatch) is surfaced.
var errKindBinaryAbsent = errors.New("kubesupport: no kind binary available")

// clusterNamePattern is the strict generated-name pattern. A sweep or delete
// only ever acts on a name matching it, so a foreign cluster such as
// ProtectedCluster can never be selected by accident.
var clusterNamePattern = regexp.MustCompile(`^` + regexp.QuoteMeta(ClusterPrefix) + `([0-9]+)-([0-9a-f]{8})$`)

// kindVersionPattern extracts the semantic version from `kind version`.
var kindVersionPattern = regexp.MustCompile(`\bkind v([0-9]+\.[0-9]+\.[0-9]+)`)

// Kind is the pinned kind client. All operations are bounded by a context and
// run through harness.Runner with an explicit environment.
type Kind struct {
	binary         string
	kubectlBinary  string
	runner         harness.Runner
	env            []string
	clock          harness.Clock
	logf           func(format string, args ...any)
	cacheDir       string
	fetcher        func(ctx context.Context, url string) ([]byte, error)
	processAlive   func(pid int) bool
	waitOptions    harness.WaitOptions
	operationLimit time.Duration
	noDownload     bool
	expectedDigest string
}

// Option configures a Kind client.
type Option func(*Kind)

// WithRunner replaces the process runner (unit tests use a fake).
func WithRunner(runner harness.Runner) Option {
	return func(k *Kind) {
		if runner != nil {
			k.runner = runner
		}
	}
}

// WithBinary replaces the kind executable path and disables PATH resolution.
func WithBinary(path string) Option {
	return func(k *Kind) {
		if path != "" {
			k.binary = path
		}
	}
}

// WithEnv replaces the explicit command environment. A nil slice restores the
// default environment so the environment is never nil.
func WithEnv(env []string) Option {
	return func(k *Kind) {
		if env == nil {
			k.env = kindEnv()
			return
		}
		k.env = append([]string(nil), env...)
	}
}

// WithClock replaces the clock used by readiness barriers.
func WithClock(clock harness.Clock) Option {
	return func(k *Kind) {
		if clock != nil {
			k.clock = clock
		}
	}
}

// WithLogger receives human-readable harness records.
func WithLogger(f func(format string, args ...any)) Option {
	return func(k *Kind) {
		if f != nil {
			k.logf = f
		}
	}
}

// WithCacheDir replaces the kind download cache directory.
func WithCacheDir(dir string) Option {
	return func(k *Kind) {
		if dir != "" {
			k.cacheDir = dir
		}
	}
}

// WithFetcher replaces the release downloader (unit tests never use the
// network).
func WithFetcher(fetcher func(ctx context.Context, url string) ([]byte, error)) Option {
	return func(k *Kind) {
		if fetcher != nil {
			k.fetcher = fetcher
		}
	}
}

// WithProcessAlive replaces the liveness check used by dead-owner cleanup.
func WithProcessAlive(check func(pid int) bool) Option {
	return func(k *Kind) {
		if check != nil {
			k.processAlive = check
		}
	}
}

// WithWaitOptions replaces the readiness barrier configuration.
func WithWaitOptions(options harness.WaitOptions) Option {
	return func(k *Kind) { k.waitOptions = options }
}

// WithoutDownload forbids a network download. Cleanup and signal paths use it so
// they never fetch anything.
func WithoutDownload() Option {
	return func(k *Kind) { k.noDownload = true }
}

// withExpectedDigest overrides the digest verified for a cached binary. It is
// unexported because it exists only for unit tests, which cannot ship the real
// pinned release; production always verifies KindSHA256.
func withExpectedDigest(digest string) Option {
	return func(k *Kind) {
		if digest != "" {
			k.expectedDigest = digest
		}
	}
}

// NewKind resolves and verifies the pinned kind binary. A missing binary is
// downloaded into the ignored cache only when downloading is allowed; a
// version mismatch is always refused.
func NewKind(options ...Option) (*Kind, error) {
	kind := &Kind{
		kubectlBinary:  "kubectl",
		runner:         harness.ExecRunner{},
		env:            kindEnv(),
		clock:          harness.RealClock{},
		logf:           func(string, ...any) {},
		cacheDir:       defaultCacheDir(),
		fetcher:        httpFetcher,
		processAlive:   harness.ProcessAlive,
		waitOptions:    harness.DefaultWaitOptions(),
		operationLimit: DefaultOperationTimeout,
		expectedDigest: KindSHA256,
	}
	for _, option := range options {
		if option != nil {
			option(kind)
		}
	}
	if kind.binary == "" {
		kind.binary = os.Getenv(KindBinaryEnv)
	}
	if kind.binary == "" {
		if found, err := exec.LookPath("kind"); err == nil {
			kind.binary = found
		}
	}
	if kind.binary == "" && kind.noDownload {
		// Cleanup and signal paths must never download, but a cluster can only
		// exist if some binary created it. Reuse the already verified pinned
		// cache file so the sweep runs on the default download-verified path.
		cached, err := kind.resolveCachedBinary()
		if err != nil {
			return nil, err
		}
		kind.binary = cached
	}
	if kind.binary == "" {
		if kind.noDownload {
			return nil, fmt.Errorf("%w: %w: no kind binary and downloading is disabled", ErrKindUnavailable, errKindBinaryAbsent)
		}
		downloaded, err := kind.download(context.Background())
		if err != nil {
			return nil, err
		}
		kind.binary = downloaded
	}
	if err := kind.checkVersion(context.Background()); err != nil {
		return nil, err
	}
	kind.logf("kubesupport: kind %s at %s (node image %s)", KindVersion, kind.binary, NodeImageReference)
	return kind, nil
}

// Binary returns the resolved kind executable path.
func (k *Kind) Binary() string { return k.binary }

// Version returns the pinned kind version.
func (k *Kind) Version() string { return KindVersion }

// NodeImage returns the pinned digest reference.
func (k *Kind) NodeImage() string { return NodeImageReference }

// kindEnv is the explicit environment every kind and kubectl command receives.
func kindEnv() []string {
	env := append(harness.BaseEnv(), ProviderEnv)
	return env
}

// checkVersion proves the resolved binary is the pinned release.
func (k *Kind) checkVersion(ctx context.Context) error {
	result, err := k.run(ctx, "version")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKindUnavailable, err)
	}
	version := parseKindVersion(string(result.Stdout))
	if version != KindVersion {
		return fmt.Errorf("%w: kind version %q; want %q", ErrKindUnavailable, version, KindVersion)
	}
	return nil
}

// parseKindVersion extracts the release version from `kind version` output. An
// unrecognized line yields the empty string.
func parseKindVersion(output string) string {
	match := kindVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return ""
	}
	return match[1]
}

// run executes one kind command with the explicit environment.
func (k *Kind) run(ctx context.Context, args ...string) (harness.Result, error) {
	return k.runBinary(ctx, k.binary, args...)
}

// runKubectl executes one kubectl command with the explicit environment.
func (k *Kind) runKubectl(ctx context.Context, args ...string) (harness.Result, error) {
	return k.runBinary(ctx, k.kubectlBinary, args...)
}

func (k *Kind) runBinary(ctx context.Context, binary string, args ...string) (harness.Result, error) {
	if strings.TrimSpace(binary) == "" {
		return harness.Result{}, errors.New("kubesupport: empty executable name")
	}
	if k.operationLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, k.operationLimit)
		defer cancel()
	}
	env := append([]string(nil), k.env...)
	if env == nil {
		env = kindEnv()
	}
	return k.runner.Run(ctx, harness.Command{Name: binary, Args: append([]string(nil), args...), Env: env})
}

// ClusterOptions configures one cluster creation.
type ClusterOptions struct {
	// Name overrides the generated name. It must match the strict generated
	// pattern; ProtectedCluster and any foreign name are refused.
	Name string
	// Dir holds the cluster config, kubeconfig and evidence. Empty creates a
	// temporary directory owned by the cluster.
	Dir string
	// ReadyTimeout overrides DefaultReadyTimeout.
	ReadyTimeout time.Duration
}

// CreateCluster creates one uniquely named, rootless kind cluster. It returns
// once the API objects exist; WaitReady is the readiness barrier.
func (k *Kind) CreateCluster(ctx context.Context, options ClusterOptions) (*Cluster, error) {
	name := options.Name
	if name == "" {
		name = NewClusterName()
	}
	if err := validateClusterName(name); err != nil {
		return nil, err
	}
	dir := options.Dir
	ownedDir := false
	if dir == "" {
		created, err := os.MkdirTemp("", "kodama-kind-"+name+"-")
		if err != nil {
			return nil, fmt.Errorf("kubesupport: create cluster directory: %w", err)
		}
		dir = created
		ownedDir = true
	}
	configPath := filepath.Join(dir, "kind-config.yaml")
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(configPath, []byte(clusterConfig(NodeImageReference)), 0o600); err != nil {
		return nil, fmt.Errorf("kubesupport: write kind config: %w", err)
	}
	readyTimeout := options.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = DefaultReadyTimeout
	}
	_, err := k.run(ctx,
		"create", "cluster",
		"--name", name,
		"--config", configPath,
		"--kubeconfig", kubeconfigPath,
		"--wait", "0s",
	)
	if err != nil {
		// A failed create can still have created some of the cluster's
		// containers (a timeout or a killed process). The cluster was never
		// registered, so delete that exact generated name now; the sweep could
		// not find it later. The private kubeconfig path is always named so the
		// cleanup delete cannot rewrite the ambient host kubeconfig.
		if deleteErr := k.deleteWithRetry(context.Background(), name, kubeconfigPath, 1); deleteErr != nil {
			k.logf("kubesupport: best-effort delete after failed create of %s: %v", name, deleteErr)
		}
		if ownedDir {
			_ = os.RemoveAll(dir)
		}
		return nil, fmt.Errorf("kubesupport: create cluster %s: %w", name, err)
	}
	cluster := &Cluster{
		Name:           name,
		Dir:            dir,
		KubeconfigPath: kubeconfigPath,
		ConfigPath:     configPath,
		ReadyTimeout:   readyTimeout,
		kind:           k,
		ownedDir:       ownedDir,
		createdAt:      k.clock.Now().UTC(),
	}
	registerCluster(cluster)
	k.logf("kubesupport: created cluster %s (node image %s, digest %s)", name, NodeImageReference, NodeImageDigest)
	return cluster, nil
}

// ListClusters returns every kind cluster name visible to the provider.
func (k *Kind) ListClusters(ctx context.Context) ([]string, error) {
	result, err := k.run(ctx, "get", "clusters")
	if err != nil {
		return nil, fmt.Errorf("kubesupport: list clusters: %w", err)
	}
	names := make([]string, 0)
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// clusterGone reports whether name is no longer listed by the provider. It is
// the confirmation check used when a delete command failed but the cluster may
// already be gone.
func (k *Kind) clusterGone(ctx context.Context, name string) (bool, error) {
	names, err := k.ListClusters(ctx)
	if err != nil {
		return false, err
	}
	if slices.Contains(names, name) {
		return false, nil
	}
	return true, nil
}

// DeleteCluster deletes exactly one generated cluster. A foreign or protected
// name is refused before any process starts.
//
// The delete command always names an explicit --kubeconfig. kind removes the
// cluster's context entry from the selected kubeconfig, so without the flag it
// would rewrite $KUBECONFIG or $HOME/.kube/config, the ambient kubeconfig
// shared with every other kind run on the host. When the caller has no cluster
// directory (an orphan sweep), a private one-shot path is allocated.
func (k *Kind) DeleteCluster(ctx context.Context, name string) error {
	return k.deleteClusterWithKubeconfig(ctx, name, "")
}

// deleteClusterWithKubeconfig deletes one generated cluster using an explicit
// kubeconfig path. An empty path allocates a private one-shot file so kind can
// never fall back to the ambient host kubeconfig.
func (k *Kind) deleteClusterWithKubeconfig(ctx context.Context, name, kubeconfigPath string) error {
	if err := validateClusterName(name); err != nil {
		return err
	}
	if kubeconfigPath == "" {
		dir, err := os.MkdirTemp("", "kodama-kind-kubeconfig-")
		if err != nil {
			return fmt.Errorf("kubesupport: create private delete kubeconfig for %s: %w", name, err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		kubeconfigPath = filepath.Join(dir, "kubeconfig")
	}
	if _, err := k.run(ctx, "delete", "cluster", "--name", name, "--kubeconfig", kubeconfigPath); err != nil {
		return fmt.Errorf("kubesupport: delete cluster %s: %w", name, err)
	}
	k.logf("kubesupport: deleted cluster %s", name)
	return nil
}

// deleteWithRetry deletes one cluster with a bounded number of attempts. The
// clock is the injected one, so unit tests never wait in wall-clock time.
func (k *Kind) deleteWithRetry(ctx context.Context, name, kubeconfigPath string, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := k.deleteClusterWithKubeconfig(ctx, name, kubeconfigPath); err == nil {
			return nil
		} else {
			last = err
		}
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		if err := k.clock.Sleep(ctx, deleteRetryBackoff); err != nil {
			break
		}
	}
	return last
}

// SweepDeadOwners deletes generated clusters whose owner pid is no longer
// alive. A live owner, ProtectedCluster and every name outside the strict
// pattern are left untouched.
func (k *Kind) SweepDeadOwners(ctx context.Context) ([]string, error) {
	return k.SweepOrphans(ctx, 0)
}

// sweepPass is the result of one sweep listing: the names deleted, the delete
// failures for clusters owned by ownPID, and the failures for dead-owner
// clusters that belong to another process.
type sweepPass struct {
	deleted      []string
	ownFailures  map[string]error
	foreignFails []error
}

// sweepPassOnce deletes every cluster a sweep is allowed to touch.
func (k *Kind) sweepPassOnce(ctx context.Context, ownPID int) (sweepPass, error) {
	names, err := k.ListClusters(ctx)
	if err != nil {
		return sweepPass{}, err
	}
	pass := sweepPass{ownFailures: map[string]error{}}
	for _, name := range names {
		owner, ok := clusterOwner(name)
		if !ok {
			continue
		}
		if owner != ownPID && k.processAlive(owner) {
			continue
		}
		if err := k.DeleteCluster(ctx, name); err != nil {
			if ownPID > 0 && owner == ownPID {
				pass.ownFailures[name] = err
			} else {
				pass.foreignFails = append(pass.foreignFails, err)
			}
			continue
		}
		pass.deleted = append(pass.deleted, name)
	}
	return pass, nil
}

// SweepOrphans deletes generated clusters whose owner pid is dead. When ownPID
// is positive it also deletes clusters owned by ownPID: the post-run sweep runs
// after every registered cluster has been closed, so a remaining own-pid
// cluster is an orphan. A live other pid, ProtectedCluster and every name
// outside the strict pattern are always left untouched.
func (k *Kind) SweepOrphans(ctx context.Context, ownPID int) ([]string, error) {
	pass, err := k.sweepPassOnce(ctx, ownPID)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, failure := range pass.ownFailures {
		errs = append(errs, failure)
	}
	errs = append(errs, pass.foreignFails...)
	return pass.deleted, errors.Join(errs...)
}

// sweepOwnOrphans is the post-run sweep. It deletes own-pid and dead-owner
// clusters, tolerates a foreign dead-owner cluster that another lane removed
// between the list and the delete, and reports an error only when this
// process's cleanup cannot be verified or one of its own clusters remains.
func (k *Kind) sweepOwnOrphans(ctx context.Context, ownPID int) error {
	pass, err := k.sweepPassOnce(ctx, ownPID)
	if err != nil {
		return err
	}
	if len(pass.foreignFails) > 0 {
		k.logf("kubesupport: ignoring %d foreign dead-owner sweep error(s): %v",
			len(pass.foreignFails), errors.Join(pass.foreignFails...))
	}
	remaining, err := k.ListClusters(ctx)
	if err != nil {
		return err
	}
	confirmed := make(map[string]struct{}, len(pass.deleted))
	for _, name := range pass.deleted {
		confirmed[name] = struct{}{}
	}
	var leaked []string
	for _, name := range ownClusterNames(remaining, ownPID) {
		if _, gone := confirmed[name]; !gone {
			leaked = append(leaked, name)
		}
	}
	if len(leaked) == 0 {
		return nil
	}
	errs := []error{fmt.Errorf(
		"kubesupport: %d cluster(s) owned by pid %d remain after cleanup: %s",
		len(leaked), ownPID, strings.Join(leaked, ", "),
	)}
	for _, name := range leaked {
		if failure, ok := pass.ownFailures[name]; ok {
			errs = append(errs, fmt.Errorf("kubesupport: delete cluster %s: %w", name, failure))
		}
	}
	return errors.Join(errs...)
}

// NewClusterName returns a unique generated name carrying the owner pid.
func NewClusterName() string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		suffix = []byte{byte(os.Getpid()), 0, 0, 0}
	}
	return fmt.Sprintf("%s%d-%s", ClusterPrefix, os.Getpid(), hex.EncodeToString(suffix))
}

// validateClusterName refuses any name this suite does not own.
func validateClusterName(name string) error {
	if name == ProtectedCluster {
		return fmt.Errorf("kubesupport: refusing to touch the protected cluster %q", ProtectedCluster)
	}
	if _, ok := clusterOwner(name); !ok {
		return fmt.Errorf("kubesupport: cluster name %q is not a generated %s<pid>-<random> name", name, ClusterPrefix)
	}
	return nil
}

// clusterOwner returns the owner pid encoded in a generated cluster name.
func clusterOwner(name string) (int, bool) {
	match := clusterNamePattern.FindStringSubmatch(name)
	if match == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// defaultCacheDir is the ignored download cache.
func defaultCacheDir() string {
	if dir := strings.TrimSpace(os.Getenv(KindCacheEnv)); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "kodama-kind-cache")
}

// cachedBinaryPath is the pinned cache file for the current KindVersion.
func (k *Kind) cachedBinaryPath() string {
	return filepath.Join(k.cacheDir, "kind-"+KindVersion+"-linux-amd64")
}

// resolveCachedBinary returns the pinned cache file only when it already exists
// and its SHA-256 matches the expected digest. A missing file returns "", nil;
// a mismatched or unreadable file is an error and is never used. It never
// downloads.
func (k *Kind) resolveCachedBinary() (string, error) {
	path := k.cachedBinaryPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("%w: read cached kind %s: %v", ErrKindUnavailable, path, err)
	}
	if err := verifyKindDigest(data, k.expectedDigest); err != nil {
		return "", err
	}
	return path, nil
}

// download fetches the pinned release asset, verifies its SHA-256 and installs
// it in the cache. A digest mismatch never installs anything.
func (k *Kind) download(ctx context.Context) (string, error) {
	destination := k.cachedBinaryPath()
	cached, err := k.resolveCachedBinary()
	if err != nil {
		return "", err
	}
	if cached != "" {
		return cached, nil
	}
	if err := os.MkdirAll(k.cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("kubesupport: create kind cache: %w", err)
	}
	data, err := k.fetcher(ctx, KindDownloadURL)
	if err != nil {
		return "", fmt.Errorf("%w: download kind %s: %v", ErrKindUnavailable, KindVersion, err)
	}
	if err := verifyKindDigest(data, k.expectedDigest); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(k.cacheDir, "kind-download-*")
	if err != nil {
		return "", fmt.Errorf("kubesupport: stage kind download: %w", err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("kubesupport: write kind download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("kubesupport: close kind download: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("kubesupport: mark kind executable: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("kubesupport: install kind: %w", err)
	}
	k.logf("kubesupport: downloaded kind %s to %s (sha256 %s)", KindVersion, destination, KindSHA256)
	return destination, nil
}

// verifyKindDigest proves downloaded or cached bytes are the pinned release.
func verifyKindDigest(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		return fmt.Errorf("%w: kind download sha256 %s; want %s", ErrKindUnavailable, got, expected)
	}
	return nil
}

// httpFetcher is the production downloader. It is replaced in unit tests.
func httpFetcher(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubesupport: GET %s returned %d", url, response.StatusCode)
	}
	// The asset is about 11 MiB; 64 MiB is a hard upper bound.
	return io.ReadAll(io.LimitReader(response.Body, 64<<20))
}
