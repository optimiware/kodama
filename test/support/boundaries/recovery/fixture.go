// Package recoveryfix is test support for the Git reporting-recovery
// suite.
//
// It is fixture code, not product code and not Shell composition. The contract
// verification/report server, the OIDC provider, the deterministic clock and
// the smart-HTTP admission verifier are labelled fixtures of Shell facts. The
// real boundaries under test are the host git binary, the composed Git app
// (`git http-backend`, product-installed hooks, the private Unix-socket hook
// channel), the durable filesystem receive journal and the identity contract
// fixture.
//
// Unlike the native Git fixture, this harness can stop and restart the Git app
// over the same storage root, journal directory and provisioned admin
// repository, which is what the recovery scenarios require.
package recoveryfix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	actorapp "github.com/optimiware/kodama/internal/userland/git/app/actors"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// Fixture identities. The tokens are opaque fixture values authenticated by the
// contract fixture; they never enter the hook environment or a journal.
const (
	TenantID id.TenantID = "tenant-i03d"

	ProjectID = "project-i03d"

	ServiceID    = "service-i03d"
	ServiceToken = "kodsvcI03dFixtureToken"

	ExecutionID    = "execution-i03d"
	ExecutionToken = "kodexI03dFixtureToken"

	HumanSubject = "auth0|human-i03d"

	ResourceID = "res-i03d"
	Repository = "shop/web"
)

// Policy is the fixture tenant git.conf. Order matters: the protected deny
// precedes the later permissive rule so first-match-wins is observable. The
// admin repository keeps its write rule so provisioning can validate the
// accepted policy.
const Policy = `
repo admin
    RW = @admin

repo shop/web
    R = @all
    - refs/heads/protected = @all
    RW refs/heads/protected = @all
    RW refs/heads/ = @execution
    RW refs/heads/ = @engineer
    RW+ refs/heads/rewind = @engineer
    RW refs/heads/guarded/ = @execution
    RW refs/heads/guarded/ = @engineer
option approve.refs/heads/guarded/ = reviewer

repo shop/linked
    R = @all
`

// FixtureTime is the deterministic fixture clock.
func FixtureTime() time.Time {
	return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
}

// TB is the minimal testing surface the fixture needs. It avoids importing
// testing from a product-build package.
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Options configures one composed recovery harness.
type Options struct {
	// BinaryPath is the built kodama binary the installed hooks exec.
	BinaryPath string
	// SourceEvents overrides the per-receive-pack push identity generator. A
	// nil value uses the production crypto/rand generator.
	SourceEvents func() (string, error)
	// HookTimeout overrides the hook callback timeout.
	HookTimeout time.Duration
	// Logger overrides the app logger. The default records into the harness's
	// safe buffer so the secret scan covers server logs.
	Logger *slog.Logger
}

// Harness is one composed Git app over real host git, a temp bare repository,
// the private hook channel, a durable journal directory and the identity
// contract fixture. It can be restarted over the same durable state.
type Harness struct {
	TB TB

	Tenant      id.TenantID
	Root        string
	StorageRoot string
	JournalDir  string
	SocketBase  string
	ClientHome  string
	BaseURL     string
	BinaryPath  string

	Provider     *idfix.Provider
	Contract     *idfix.ContractServer
	Transport    *client.HTTPTransport
	HumanToken   string
	SourceEvents func() (string, error)

	options Options

	mu        sync.Mutex
	server    *gitapp.Server
	registry  *actorapp.Registry
	admission *AdmissionVerifier
	listener  *trackingListener
	cancel    context.CancelFunc
	done      chan error
	envProbe  string

	logs       *safeBuffer
	transcript *safeBuffer
}

// NewHarness builds and starts one composed native Git service.
func NewHarness(tb TB, options Options) (*Harness, error) {
	tb.Helper()
	if strings.TrimSpace(options.BinaryPath) == "" {
		return nil, fmt.Errorf("fixture kodama binary path is empty")
	}
	root, err := os.MkdirTemp("", "kodama-i03d-")
	if err != nil {
		return nil, fmt.Errorf("create fixture root: %w", err)
	}
	socketBase, err := os.MkdirTemp("", "k-")
	if err != nil {
		os.RemoveAll(root)
		return nil, fmt.Errorf("create socket base: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(socketBase)
	}
	fail := func(err error) (*Harness, error) {
		cleanup()
		return nil, err
	}

	storageRoot := filepath.Join(root, "repos")
	journalDir := filepath.Join(root, "journal")
	clientHome := filepath.Join(root, "home")
	for _, directory := range []string{storageRoot, clientHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fail(fmt.Errorf("create %q: %w", directory, err))
		}
	}
	// The runtime reads only the committed policy of the provisioned admin
	// repository. Provision once through the product path, then install the
	// fixture policy as an accepted commit; no policy file is read.
	adminPath, err := recoveryProvisionTenant(storageRoot, journalDir, options.BinaryPath)
	if err != nil {
		return fail(err)
	}
	if _, err := recoverySeedCommittedPolicy(root, adminPath, Policy); err != nil {
		return fail(fmt.Errorf("install fixture admin policy: %w", err))
	}

	provider, err := idfix.NewProvider(idfix.OIDCOptions{Clock: idfix.NewClock(FixtureTime())})
	if err != nil {
		return fail(fmt.Errorf("NewProvider: %w", err))
	}
	human, err := provider.Mint(idfix.TokenSpec{
		Subject: HumanSubject,
		Tenant:  string(TenantID),
		Project: ProjectID,
		Roles:   []string{"engineer"},
	})
	if err != nil {
		provider.Close()
		return fail(fmt.Errorf("mint human token: %w", err))
	}
	humanPrincipal, err := shelloidc.PrincipalIDFor(provider.Issuer(), HumanSubject)
	if err != nil {
		provider.Close()
		return fail(fmt.Errorf("human principal id: %w", err))
	}
	contract, err := idfix.NewContractServer(idfix.ContractOptions{
		Clock: provider.Clock(),
		OIDC:  provider,
		Services: []idfix.ServiceAccount{{
			ID:        ServiceID,
			Token:     ServiceToken,
			TenantID:  string(TenantID),
			ProjectID: ProjectID,
			Enabled:   true,
		}},
		Executions: []idfix.ExecutionAccount{{
			ID:        ExecutionID,
			Token:     ExecutionToken,
			TenantID:  string(TenantID),
			ProjectID: ProjectID,
			Enabled:   true,
		}},
		Bindings: []idfix.Binding{{
			ID:               "binding-i03d",
			ServiceID:        ServiceID,
			TenantID:         string(TenantID),
			ProjectID:        ProjectID,
			ResourceID:       ResourceID,
			Operations:       []string{string(api.ResourceOperationGitRefUpdate)},
			ReportableEvents: []string{"git.ref.updated"},
		}},
		ActorRoles: map[string][]string{string(humanPrincipal): {"engineer"}},
	})
	if err != nil {
		provider.Close()
		return fail(fmt.Errorf("NewContractServer: %w", err))
	}
	transport := &client.HTTPTransport{
		BaseURL:      contract.URL(),
		RoundTripper: contract.Client().Transport,
		Source: staticCredentialSource{credential: client.Credential{
			Kind:      client.KindService,
			Token:     ServiceToken,
			ExpiresAt: FixtureTime().Add(24 * time.Hour),
		}},
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep: func(time.Duration) {},
		Now:   FixtureTime,
	}

	sourceEvents := options.SourceEvents
	if sourceEvents == nil {
		sourceEvents = git.NewSourceEventID()
	}
	harness := &Harness{
		TB:           tb,
		Tenant:       TenantID,
		Root:         root,
		StorageRoot:  storageRoot,
		JournalDir:   journalDir,
		SocketBase:   socketBase,
		ClientHome:   clientHome,
		BinaryPath:   options.BinaryPath,
		Provider:     provider,
		Contract:     contract,
		Transport:    transport,
		HumanToken:   human.Value,
		SourceEvents: sourceEvents,
		options:      options,
		logs:         &safeBuffer{},
		transcript:   &safeBuffer{},
	}
	if err := harness.start(); err != nil {
		contract.Close()
		provider.Close()
		return fail(fmt.Errorf("start Git app: %w", err))
	}
	tb.Cleanup(func() {
		_ = harness.stop()
		contract.Close()
		provider.Close()
		cleanup()
	})
	return harness, nil
}

// recoveryProvisionTenant creates this tenant's admin repository through the
// real product provisioning boundary, exactly as the operator command does.
func recoveryProvisionTenant(storageRoot, journalDirectory, binaryPath string) (string, error) {
	result, err := gitapp.ProvisionTenant(context.Background(), gitapp.ProvisionConfig{
		TenantID:         TenantID,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDirectory,
		HookBinary:       binaryPath,
	})
	if err != nil {
		return "", fmt.Errorf("provision tenant %q: %w", TenantID, err)
	}
	return result.RepositoryPath, nil
}

// recoverySeedCommittedPolicy installs one committed conf/git.conf on top of
// the provisioned trusted commit. It is fixture state setup, not a policy
// source: the app still reads only the committed blob, and the write is
// plumbing on the bare repository so no running hook channel is required.
func recoverySeedCommittedPolicy(root, repositoryPath, policy string) (string, error) {
	parent, err := recoverySeedGit(root, "--git-dir="+repositoryPath, "rev-parse", "--verify", native.CommittedPolicyRef)
	if err != nil {
		return "", fmt.Errorf("resolve provisioned trusted ref: %w", err)
	}
	parent = strings.TrimSpace(parent)
	policyFile, err := os.CreateTemp(root, "fixture-policy-")
	if err != nil {
		return "", fmt.Errorf("create fixture policy file: %w", err)
	}
	policyPath := policyFile.Name()
	if _, err := policyFile.WriteString(policy); err != nil {
		policyFile.Close()
		return "", fmt.Errorf("write fixture policy file: %w", err)
	}
	if err := policyFile.Close(); err != nil {
		return "", fmt.Errorf("close fixture policy file: %w", err)
	}
	defer os.Remove(policyPath)
	blob, err := recoverySeedGit(root, "--git-dir="+repositoryPath, "hash-object", "-w", "--no-filters", policyPath)
	if err != nil {
		return "", fmt.Errorf("store fixture policy blob: %w", err)
	}
	blob = strings.TrimSpace(blob)
	indexPath := policyPath + ".index"
	defer os.Remove(indexPath)
	indexEnvironment := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := recoverySeedGitEnv(root, indexEnvironment, "--git-dir="+repositoryPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+","+native.CommittedPolicyFile); err != nil {
		return "", fmt.Errorf("index fixture policy: %w", err)
	}
	tree, err := recoverySeedGitEnv(root, indexEnvironment, "--git-dir="+repositoryPath, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write fixture policy tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	commit, err := recoverySeedGit(root, "--git-dir="+repositoryPath,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit-tree", tree, "-p", parent, "-m", "fixture committed policy")
	if err != nil {
		return "", fmt.Errorf("commit fixture policy: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if _, err := recoverySeedGit(root, "--git-dir="+repositoryPath, "update-ref", native.CommittedPolicyRef, commit); err != nil {
		return "", fmt.Errorf("point the trusted ref at the fixture policy: %w", err)
	}
	committed, err := recoverySeedGit(root, "--git-dir="+repositoryPath, "cat-file", "blob", native.CommittedPolicyRef+":"+native.CommittedPolicyFile)
	if err != nil {
		return "", fmt.Errorf("read back fixture policy: %w", err)
	}
	if committed != policy {
		return "", fmt.Errorf("committed fixture policy differs from the requested policy")
	}
	return commit, nil
}

// recoverySeedGit runs one deterministic host-git fixture command.
func recoverySeedGit(root string, args ...string) (string, error) {
	return recoverySeedGitEnv(root, nil, args...)
}

func recoverySeedGitEnv(root string, environment []string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	seedHome := filepath.Join(root, "seed-home")
	if err := os.MkdirAll(seedHome, 0o700); err != nil {
		return "", fmt.Errorf("create seed home: %w", err)
	}
	command.Env = append([]string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + seedHome,
		"PATH=" + os.Getenv("PATH"),
	}, environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// start builds one Git app instance over the existing durable directories and
// serves it on a fresh loopback listener.
func (harness *Harness) start() error {
	registry, err := actorapp.NewRegistry(actorapp.Config{
		TenantID:   TenantID,
		Clock:      fixedClock{now: FixtureTime()},
		MaxHandles: 128,
		TTL:        5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("NewRegistry: %w", err)
	}
	admission := NewAdmissionVerifier()
	admission.Allow(ExecutionToken, git.Identity{Execution: true})
	admission.Allow(harness.HumanToken, git.Identity{Roles: []string{"engineer"}})

	hookTimeout := harness.options.HookTimeout
	if hookTimeout <= 0 {
		hookTimeout = 2 * time.Second
	}
	logger := harness.options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(harness.logs, nil))
	}
	server, err := gitapp.New(gitapp.Config{
		TenantID:            TenantID,
		StorageRoot:         harness.StorageRoot,
		JournalDirectory:    harness.JournalDir,
		ResourceIDs:         map[string]string{Repository: ResourceID, "shop/linked": ResourceID},
		Transport:           harness.Transport,
		Actors:              admission,
		ActorHandles:        registry,
		HookActors:          registry,
		HookBinary:          harness.BinaryPath,
		HookSocketDirectory: harness.SocketBase,
		HookTimeout:         hookTimeout,
		SourceEvent:         harness.SourceEvents,
		Logger:              logger,
	})
	if err != nil {
		_ = registry.Close()
		return fmt.Errorf("git app New: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = server.Close()
		_ = registry.Close()
		return fmt.Errorf("listen: %w", err)
	}
	tracked := &trackingListener{Listener: listener, events: make(chan struct{}, 128)}
	ctx, cancel := context.WithCancel(context.Background())
	// Serve starts the private hook channel itself, as the production Run path
	// does. Wait for the bound socket instead of starting it a second time, so
	// this fixture drives the same lifecycle.
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, tracked) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(server.HookSocketPath()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			_ = listener.Close()
			_ = server.Close()
			_ = registry.Close()
			return fmt.Errorf("git app Serve did not bind the hook channel")
		}
		time.Sleep(5 * time.Millisecond)
	}

	harness.mu.Lock()
	harness.server = server
	harness.registry = registry
	harness.admission = admission
	harness.listener = tracked
	harness.cancel = cancel
	harness.done = done
	harness.BaseURL = "http://" + listener.Addr().String()
	harness.mu.Unlock()
	return nil
}

// stop shuts the current Git app instance down and joins it.
func (harness *Harness) stop() error {
	harness.mu.Lock()
	cancel := harness.cancel
	done := harness.done
	server := harness.server
	registry := harness.registry
	listener := harness.listener
	harness.cancel = nil
	harness.done = nil
	harness.server = nil
	harness.registry = nil
	harness.listener = nil
	harness.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("git app did not join after cancellation")
		}
	}
	var stopErr error
	if server != nil {
		if err := server.Close(); err != nil {
			stopErr = err
		}
	}
	if registry != nil {
		if err := registry.Close(); err != nil && stopErr == nil {
			stopErr = err
		}
	}
	if listener != nil {
		_ = listener.Close()
	}
	return stopErr
}

// Restart stops the Git app and starts a new instance over the same storage
// root, journal directory and provisioned admin repository. It is the recovery
// "process restart" boundary: no in-memory journal or handle state survives and
// the committed policy is read again.
func (harness *Harness) Restart(tb TB) error {
	tb.Helper()
	if err := harness.stop(); err != nil {
		return err
	}
	return harness.start()
}

// Server returns the current composed Git app server.
func (harness *Harness) Server() *gitapp.Server {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.server
}

// Service returns the current tenant-bound Git service.
func (harness *Harness) Service() *git.Service { return harness.Server().Service() }

// Registry returns the current actor-handle registry.
func (harness *Harness) Registry() *actorapp.Registry {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.registry
}

// Address returns the current loopback listener address.
func (harness *Harness) Address() string {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.listener == nil {
		return ""
	}
	return harness.listener.Addr().String()
}

// ConnectionClosed reports one server-side TCP connection close. It is the
// barrier the cancellation scenario uses to prove the aborted request finished.
func (harness *Harness) ConnectionClosed() <-chan struct{} {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.listener == nil {
		return nil
	}
	return harness.listener.events
}

// DrainConnectionCloses discards every close event recorded so far.
func (harness *Harness) DrainConnectionCloses() {
	harness.mu.Lock()
	listener := harness.listener
	harness.mu.Unlock()
	if listener == nil {
		return
	}
	for {
		select {
		case <-listener.events:
		default:
			return
		}
	}
}

// LogText returns the app's safe log output.
func (harness *Harness) LogText() string { return harness.logs.String() }

// Transcript returns every captured git command stdout/stderr. Command
// arguments (which carry a Basic header for the fixture) are never recorded.
func (harness *Harness) Transcript() string { return harness.transcript.String() }

// RepositoryPath returns the tenant-scoped bare repository path.
func (harness *Harness) RepositoryPath(repository string) string {
	return harness.TenantRepositoryPath(harness.Tenant, repository)
}

// TenantRepositoryPath returns one tenant's bare repository path.
func (harness *Harness) TenantRepositoryPath(tenant id.TenantID, repository string) string {
	return filepath.Join(harness.StorageRoot, string(tenant), filepath.FromSlash(repository))
}

// InitBareRepository creates one bare repository for the fixture tenant.
func (harness *Harness) InitBareRepository(tb TB, repository string) string {
	tb.Helper()
	path := harness.RepositoryPath(repository)
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{Path: path}); err != nil {
		tb.Fatalf("InitBareRepository(%q): %v", repository, err)
	}
	return path
}

// InstallProductHooks installs the two safe product hooks.
func (harness *Harness) InstallProductHooks(tb TB, repository string) {
	tb.Helper()
	if err := harness.Server().InstallHooks(repository); err != nil {
		tb.Fatalf("InstallHooks(%q): %v", repository, err)
	}
}

// RestoreProductHooks removes test-only hook overrides and reinstalls the
// product hooks.
func (harness *Harness) RestoreProductHooks(tb TB, repository string) {
	tb.Helper()
	hooksDirectory := filepath.Join(harness.RepositoryPath(repository), "hooks")
	for _, phase := range []string{"pre-receive", "post-receive"} {
		if err := os.Remove(filepath.Join(hooksDirectory, phase)); err != nil && !os.IsNotExist(err) {
			tb.Fatalf("remove test hook %q: %v", phase, err)
		}
	}
	harness.InstallProductHooks(tb, repository)
}

// WriteHook writes one test-only hook script.
func (harness *Harness) WriteHook(tb TB, repository, phase, content string) {
	tb.Helper()
	path := filepath.Join(harness.RepositoryPath(repository), "hooks", phase)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		tb.Fatalf("WriteHook(%q): %v", phase, err)
	}
}

// WriteEnvProbeHook writes a pre-receive wrapper that appends the hook process
// environment to probePath and then runs the real product pre-receive hook.
// The probe is a test-only hook/audit log; the fixture scans it for
// credentials.
func (harness *Harness) WriteEnvProbeHook(tb TB, repository, probePath string) {
	tb.Helper()
	harness.mu.Lock()
	harness.envProbe = probePath
	harness.mu.Unlock()
	script := "#!/bin/sh\n" +
		"set -u\n" +
		"{ printf 'env-start\\n'; env; printf 'env-end\\n'; } >> " + shellQuote(probePath) + "\n" +
		"exec " + shellQuote(harness.BinaryPath) + " git hook pre-receive\n"
	harness.WriteHook(tb, repository, "pre-receive", script)
}

// EnvProbePath returns the configured environment probe file, if any.
func (harness *Harness) EnvProbePath() string {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.envProbe
}

// RecordedHandles returns every opaque actor handle observed in the hook
// environment probe. It never returns a credential.
func (harness *Harness) RecordedHandles() []string {
	path := harness.EnvProbePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var handles []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, git.EnvActorRef+"="); ok && value != "" {
			handles = append(handles, value)
		}
	}
	return handles
}

// Environment returns the deterministic child environment for git clients.
func (harness *Harness) Environment() []string {
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + harness.ClientHome,
		"PATH=" + os.Getenv("PATH"),
	}
}

// Git runs one git command outside a repository.
func (harness *Harness) Git(tb TB, args ...string) (string, string, int) {
	tb.Helper()
	return harness.gitIn(tb, "", args...)
}

// GitIn runs one git command inside directory.
func (harness *Harness) GitIn(tb TB, directory string, args ...string) (string, string, int) {
	tb.Helper()
	return harness.gitIn(tb, directory, args...)
}

func (harness *Harness) gitIn(tb TB, directory string, args ...string) (string, string, int) {
	tb.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = harness.Environment()
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	harness.recordOutput(stdout.String(), stderr.String())
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	tb.Fatalf("git %v: %v", args, err)
	return "", "", -1
}

func (harness *Harness) recordOutput(stdout, stderr string) {
	if stdout != "" {
		_, _ = harness.transcript.Write([]byte(stdout))
	}
	if stderr != "" {
		_, _ = harness.transcript.Write([]byte(stderr))
	}
}

// Clone clones one repository with a Basic credential passed on the command
// line (never persisted in the clone config).
func (harness *Harness) Clone(tb TB, token, repository, directory string) (string, int) {
	tb.Helper()
	_, stderr, code := harness.gitIn(tb, "", "-c", "http.extraHeader="+BasicHeader(token),
		"clone", harness.BaseURL+"/"+repository+".git", directory)
	return stderr, code
}

// Push pushes the current branch with a Basic credential.
func (harness *Harness) Push(tb TB, directory, token string, extra ...string) (string, int) {
	tb.Helper()
	args := []string{"-C", directory, "-c", "http.extraHeader=" + BasicHeader(token), "push"}
	args = append(args, extra...)
	_, stderr, code := harness.gitIn(tb, "", args...)
	return stderr, code
}

// SetOrigin points an existing clone at the current loopback base URL, which
// changes across a restart.
func (harness *Harness) SetOrigin(tb TB, directory, repository string) {
	tb.Helper()
	if _, stderr, code := harness.GitIn(tb, directory, "remote", "set-url", "origin", harness.BaseURL+"/"+repository+".git"); code != 0 {
		tb.Fatalf("remote set-url: %s", stderr)
	}
}

// Commit creates one commit on the checked-out branch.
func (harness *Harness) Commit(tb TB, directory, message string) string {
	tb.Helper()
	file := filepath.Join(directory, "content-"+strings.ReplaceAll(message, " ", "-")+".txt")
	if err := os.WriteFile(file, []byte(message+"\n"), 0o600); err != nil {
		tb.Fatalf("write commit file: %v", err)
	}
	if _, stderr, code := harness.GitIn(tb, directory, "add", "."); code != 0 {
		tb.Fatalf("git add: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(tb, directory, "-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid", "commit", "-m", message); code != 0 {
		tb.Fatalf("git commit: %s", stderr)
	}
	stdout, stderr, code := harness.GitIn(tb, directory, "rev-parse", "HEAD")
	if code != 0 {
		tb.Fatalf("git rev-parse HEAD: %s", stderr)
	}
	return strings.TrimSpace(stdout)
}

// RemoteRef returns the commit currently stored in the bare repository ref.
func (harness *Harness) RemoteRef(tb TB, repository, ref string) string {
	tb.Helper()
	path := harness.RepositoryPath(repository)
	stdout, _, code := harness.Git(tb, "-C", path, "rev-parse", "--verify", "-q", ref)
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(stdout)
}

// SetRemoteRef sets one bare repository ref to commit.
func (harness *Harness) SetRemoteRef(tb TB, repository, ref, commit string) {
	tb.Helper()
	if _, stderr, code := harness.Git(tb, "-C", harness.RepositoryPath(repository), "update-ref", ref, commit); code != 0 {
		tb.Fatalf("update-ref %s: %s", ref, stderr)
	}
}

// DeleteRemoteRef removes one bare repository ref.
func (harness *Harness) DeleteRemoteRef(tb TB, repository, ref string) {
	tb.Helper()
	if _, stderr, code := harness.Git(tb, "-C", harness.RepositoryPath(repository), "update-ref", "-d", ref); code != 0 {
		tb.Fatalf("update-ref -d %s: %s", ref, stderr)
	}
}

// CreateCommitOnTop creates one new commit object in the bare repository whose
// parent is parent and whose tree is parent's tree. It is used to produce a
// third observed ref state that is neither the journal base nor candidate.
func (harness *Harness) CreateCommitOnTop(tb TB, repository, parent, message string) string {
	tb.Helper()
	path := harness.RepositoryPath(repository)
	stdout, stderr, code := harness.Git(tb, "-C", path, "rev-parse", parent+"^{tree}")
	if code != 0 {
		tb.Fatalf("rev-parse tree: %s", stderr)
	}
	tree := strings.TrimSpace(stdout)
	stdout, stderr, code = harness.Git(tb, "-C", path,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit-tree", tree, "-p", parent, "-m", message)
	if code != 0 {
		tb.Fatalf("commit-tree: %s", stderr)
	}
	return strings.TrimSpace(stdout)
}

// Refs returns the exact `for-each-ref` snapshot of one bare repository.
func (harness *Harness) Refs(tb TB, repository string) string {
	tb.Helper()
	stdout, stderr, code := harness.Git(tb, "-C", harness.RepositoryPath(repository), "for-each-ref", "--format=%(refname) %(objectname)")
	if code != 0 {
		tb.Fatalf("for-each-ref: %s", stderr)
	}
	return stdout
}

// ObjectFiles returns the sorted relative paths of every file under the bare
// repository object store.
func (harness *Harness) ObjectFiles(tb TB, repository string) []string {
	tb.Helper()
	root := filepath.Join(harness.RepositoryPath(repository), "objects")
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		tb.Fatalf("walk object store: %v", err)
	}
	sort.Strings(files)
	return files
}

// JournalEntries returns every durable journal entry.
func (harness *Harness) JournalEntries(tb TB) []git.JournalEntry {
	tb.Helper()
	directory, err := os.ReadDir(harness.JournalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		tb.Fatalf("read journal directory: %v", err)
	}
	var entries []git.JournalEntry
	for _, item := range directory {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(harness.JournalDir, item.Name()))
		if err != nil {
			tb.Fatalf("read journal entry: %v", err)
		}
		var entry git.JournalEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			tb.Fatalf("decode journal entry %q: %v", item.Name(), err)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].SourceEventID < entries[right].SourceEventID })
	return entries
}

// JournalRaw returns the exact bytes of every durable journal file keyed by
// file name. It is the secret-scan input, independent of the struct decode.
func (harness *Harness) JournalRaw(tb TB) map[string]string {
	tb.Helper()
	directory, err := os.ReadDir(harness.JournalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		tb.Fatalf("read journal directory: %v", err)
	}
	raw := make(map[string]string)
	for _, item := range directory {
		if item.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(harness.JournalDir, item.Name()))
		if err != nil {
			tb.Fatalf("read journal file: %v", err)
		}
		raw[item.Name()] = string(data)
	}
	return raw
}

// JournalEntriesFor returns the journal entries for one ref.
func (harness *Harness) JournalEntriesFor(tb TB, ref string) []git.JournalEntry {
	tb.Helper()
	var matching []git.JournalEntry
	for _, entry := range harness.JournalEntries(tb) {
		if entry.Ref == ref {
			matching = append(matching, entry)
		}
	}
	return matching
}

// CountReports returns the number of resource.report requests the contract
// fixture observed, including a dropped response.
func (harness *Harness) CountReports() int {
	reports := 0
	for _, record := range harness.Contract.Recorded() {
		if record.Operation == "resource.report" {
			reports++
		}
	}
	return reports
}

// BasicHeader returns the Basic authorization value for the kodama username.
func BasicHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"+token))
}

// AdmissionVerifier is the smart-HTTP admission actor-verification fixture. It
// stands in for the current-principal identity boundary; the hook's per-update
// verification still goes through the real public client and the identity
// contract fixture.
type AdmissionVerifier struct {
	mutex      sync.Mutex
	identities map[string]git.Identity
}

// NewAdmissionVerifier returns an empty fixture.
func NewAdmissionVerifier() *AdmissionVerifier {
	return &AdmissionVerifier{identities: map[string]git.Identity{}}
}

// Allow records one credential-to-identity fact.
func (verifier *AdmissionVerifier) Allow(credential string, identity git.Identity) {
	verifier.mutex.Lock()
	defer verifier.mutex.Unlock()
	verifier.identities[credential] = identity
}

// VerifyReadActor implements git.ReadActorVerifier.
func (verifier *AdmissionVerifier) VerifyReadActor(ctx context.Context, _, credential string) (git.Identity, error) {
	if err := ctx.Err(); err != nil {
		return git.Identity{}, err
	}
	verifier.mutex.Lock()
	defer verifier.mutex.Unlock()
	identity, ok := verifier.identities[credential]
	if !ok {
		return git.Identity{}, fault.New(fault.Unauthenticated, "fixture admission: unknown actor credential")
	}
	return identity, nil
}

type staticCredentialSource struct {
	credential client.Credential
}

func (source staticCredentialSource) Credential(context.Context) (client.Credential, error) {
	return source.credential, nil
}

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

// safeBuffer is a concurrency-safe writer used for server logs and captured
// command output.
type safeBuffer struct {
	mutex  sync.Mutex
	buffer strings.Builder
}

func (buffer *safeBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *safeBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.buffer.String()
}

// trackingListener records server-side connection closes. The cancellation
// scenario uses it as a barrier instead of sleeping.
type trackingListener struct {
	net.Listener
	events chan struct{}
}

func (listener *trackingListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: connection, events: listener.events}, nil
}

type trackedConn struct {
	net.Conn
	once   sync.Once
	events chan struct{}
}

func (connection *trackedConn) Close() error {
	connection.once.Do(func() {
		select {
		case connection.events <- struct{}{}:
		default:
		}
	})
	return connection.Conn.Close()
}

// RunningPush is one git push process the test can kill mid-flight.
type RunningPush struct {
	harness *Harness
	command *exec.Cmd
	stdout  *strings.Builder
	stderr  *strings.Builder
}

// StartPush starts one push without waiting for it.
func (harness *Harness) StartPush(tb TB, directory, token string, extra ...string) *RunningPush {
	tb.Helper()
	args := []string{"-C", directory, "-c", "http.extraHeader=" + BasicHeader(token), "push"}
	args = append(args, extra...)
	command := exec.Command("git", args...)
	command.Env = harness.Environment()
	// Put the client in its own process group so killing a mid-push client
	// also terminates git-remote-http, which inherits the stdout/stderr pipes.
	// Killing only the top-level git process would leave Wait blocked on those
	// pipes.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	running := &RunningPush{harness: harness, command: command, stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	command.Stdout = running.stdout
	command.Stderr = running.stderr
	if err := command.Start(); err != nil {
		tb.Fatalf("start push: %v", err)
	}
	return running
}

// Kill terminates the push client process group.
func (push *RunningPush) Kill() error {
	if push.command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-push.command.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return push.command.Process.Kill()
}

// Wait waits for the push and returns its captured stderr and exit code.
func (push *RunningPush) Wait() (string, int) {
	err := push.command.Wait()
	push.harness.recordOutput(push.stdout.String(), push.stderr.String())
	if err == nil {
		return push.stderr.String(), 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return push.stderr.String(), exit.ExitCode()
	}
	return push.stderr.String(), -1
}

// shellQuote single-quotes one fixed path for /bin/sh.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// Fifo helpers used by the cancellation scenario. They are explicit barriers:
// the test never sleeps to guess that a state was reached.

// MakeFifo creates one private named pipe.
func MakeFifo(tb TB, path string) {
	tb.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		tb.Fatalf("Mkfifo %q: %v", path, err)
	}
}

// ReleaseFifo writes value to a named pipe if a reader is currently waiting.
// It returns false when no reader exists (the hook process is gone).
func ReleaseFifo(tb TB, path, value string, timeout time.Duration) bool {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, writeErr := writer.WriteString(value)
			writer.Close()
			if writeErr != nil {
				tb.Fatalf("write fifo %q: %v", path, writeErr)
			}
			return true
		}
		if !errors.Is(err, syscall.ENXIO) {
			tb.Fatalf("open fifo %q: %v", path, err)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitForFile waits until path exists, using a bounded condition poll rather
// than a fixed sleep.
func WaitForFile(tb TB, path string, timeout time.Duration) bool {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ScanTree returns every regular file under root containing needle.
func ScanTree(root, needle string) ([]string, error) {
	var hits []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	return hits, err
}

// HideRepository renames one bare repository out of the way so the real ref
// observer's `git for-each-ref` fails. The returned function restores it.
func (harness *Harness) HideRepository(tb TB, repository string) func() {
	tb.Helper()
	path := harness.RepositoryPath(repository)
	hidden := path + ".unobservable"
	if err := os.Rename(path, hidden); err != nil {
		tb.Fatalf("hide repository %q: %v", repository, err)
	}
	return func() {
		if err := os.Rename(hidden, path); err != nil {
			tb.Fatalf("restore repository %q: %v", repository, err)
		}
	}
}

// MakeJournalUnwritable removes write permission from the durable journal
// directory so the next Record fails. The returned function restores it.
func (harness *Harness) MakeJournalUnwritable(tb TB) func() {
	tb.Helper()
	if err := os.Chmod(harness.JournalDir, 0o500); err != nil {
		tb.Fatalf("make journal unwritable: %v", err)
	}
	return func() {
		if err := os.Chmod(harness.JournalDir, 0o750); err != nil {
			tb.Fatalf("restore journal mode: %v", err)
		}
	}
}

// CorruptJournalEntry replaces one durable journal file with unreadable bytes so
// the next Load fails. The returned function restores the original bytes.
func (harness *Harness) CorruptJournalEntry(tb TB, sourceEventID string) func() {
	tb.Helper()
	path := filepath.Join(harness.JournalDir, sourceEventID+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read journal entry %q: %v", sourceEventID, err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		tb.Fatalf("corrupt journal entry %q: %v", sourceEventID, err)
	}
	return func() {
		if err := os.WriteFile(path, original, 0o600); err != nil {
			tb.Fatalf("restore journal entry %q: %v", sourceEventID, err)
		}
	}
}
