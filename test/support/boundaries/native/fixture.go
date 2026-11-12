// Package nativefix is test support for the native Git wiring suite.
//
// It is fixture code, not product code and not Shell composition. The
// admission actor-verification fact, the deterministic clock and the public
// contract server are fixtures of Shell facts; the real boundaries under test
// are the host git binary, the Git app's smart-HTTP/http-backend/installed
// hooks, the private Unix-socket hook channel and the identity contract
// fixture.
package nativefix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// Fixture identities. Tokens are opaque fixture values authenticated by the
// contract fixture; they never enter the hook environment or a journal.
const (
	TenantID    id.TenantID = "tenant-i03c"
	OtherTenant id.TenantID = "tenant-other"
	ProjectID               = "project-i03c"

	ServiceID    = "service-i03c"
	ServiceToken = "kodsvc.i03c.fixture-token"

	ExecutionID    = "execution-i03c"
	ExecutionToken = "kodex.i03c.fixture-token"

	HumanSubject = "auth0|human-i03c"
	AdminSubject = "auth0|admin-i03c"

	ResourceID = "res-i03c"
	Repository = "shop/web"
	// AdminRepository is the tenant-relative repository that holds the
	// committed access policy at conf/git.conf.
	AdminRepository = "admin"
)

// Policy is the fixture tenant git.conf. Order matters: the protected deny
// precedes the later permissive rule so first-match-wins is observable.
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

// Options configures one composed native Git harness.
type Options struct {
	// BinaryPath is the built kodama binary the installed hooks exec.
	BinaryPath string
	// Verification overrides the real public verification client (used for the
	// slow-service timeout test).
	Verification git.VerificationClient
	// HookTimeout overrides the hook callback timeout.
	HookTimeout time.Duration
	// Logger overrides the discarded fixture logger.
	Logger *slog.Logger
	// AdminPolicy is the committed conf/git.conf the fixture installs in the
	// provisioned admin repository before any test operation. An empty value
	// uses Policy.
	AdminPolicy string
	// SkipProvision leaves provisioning to the test. It is used only by the
	// provisioning tests, which call the product ProvisionTenant exactly once
	// themselves; every other fixture provisions once here.
	SkipProvision bool
}

// Harness is one composed Git app over real host git, a temp bare repository,
// the private hook channel and the identity contract fixture.
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
	Registry     *actorapp.Registry
	Admission    *AdmissionVerifier
	Verification git.VerificationClient
	Transport    *client.HTTPTransport
	Server       *gitapp.Server
	HumanToken   string
	AdminToken   string

	config   gitapp.Config
	cancel   context.CancelFunc
	done     chan error
	listener net.Listener
}

// NewHarness builds and starts one composed native Git service.
func NewHarness(tb TB, options Options) (*Harness, error) {
	tb.Helper()
	if strings.TrimSpace(options.BinaryPath) == "" {
		return nil, fmt.Errorf("fixture kodama binary path is empty")
	}
	root, err := os.MkdirTemp("", "kodama-i03c-")
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
	// repository. The fixture creates that repository through the product
	// provisioning path once, then installs the fixture policy as an accepted
	// commit. No policy file is ever read by the app.
	if !options.SkipProvision {
		adminPath, err := provisionTenant(root, storageRoot, journalDir, TenantID, options.BinaryPath)
		if err != nil {
			return fail(err)
		}
		adminPolicy := options.AdminPolicy
		if adminPolicy == "" {
			adminPolicy = Policy
		}
		if _, err := seedCommittedPolicy(root, adminPath, adminPolicy); err != nil {
			return fail(fmt.Errorf("install fixture admin policy: %w", err))
		}
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
	admin, err := provider.Mint(idfix.TokenSpec{
		Subject: AdminSubject,
		Tenant:  string(TenantID),
		Project: ProjectID,
		Roles:   []string{"admin"},
	})
	if err != nil {
		provider.Close()
		return fail(fmt.Errorf("mint admin token: %w", err))
	}
	adminPrincipal, err := shelloidc.PrincipalIDFor(provider.Issuer(), AdminSubject)
	if err != nil {
		provider.Close()
		return fail(fmt.Errorf("admin principal id: %w", err))
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
			ID:               "binding-i03c",
			ServiceID:        ServiceID,
			TenantID:         string(TenantID),
			ProjectID:        ProjectID,
			ResourceID:       ResourceID,
			Operations:       []string{string(api.ResourceOperationGitRefUpdate)},
			ReportableEvents: []string{"git.ref.updated"},
		}},
		ActorRoles: map[string][]string{
			string(humanPrincipal): {"engineer"},
			string(adminPrincipal): {"admin"},
		},
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
	verification := options.Verification
	if verification == nil {
		verification, err = gitapp.NewVerificationClient(transport)
		if err != nil {
			contract.Close()
			provider.Close()
			return fail(fmt.Errorf("NewVerificationClient: %w", err))
		}
	}
	registry, err := actorapp.NewRegistry(actorapp.Config{
		TenantID:   TenantID,
		Clock:      fixedClock{now: FixtureTime()},
		MaxHandles: 128,
		TTL:        5 * time.Minute,
	})
	if err != nil {
		contract.Close()
		provider.Close()
		return fail(fmt.Errorf("NewRegistry: %w", err))
	}
	admission := NewAdmissionVerifier()
	admission.Allow(ExecutionToken, git.Identity{Execution: true})
	admission.Allow(human.Value, git.Identity{Roles: []string{"engineer"}})
	admission.Allow(admin.Value, git.Identity{Roles: []string{"admin"}})

	hookTimeout := options.HookTimeout
	if hookTimeout <= 0 {
		hookTimeout = 2 * time.Second
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	config := gitapp.Config{
		TenantID:            TenantID,
		StorageRoot:         storageRoot,
		JournalDirectory:    journalDir,
		ResourceIDs:         map[string]string{Repository: ResourceID, "shop/linked": ResourceID, AdminRepository: ResourceID},
		Transport:           transport,
		Verify:              verification,
		Actors:              admission,
		ActorHandles:        registry,
		HookActors:          registry,
		HookBinary:          options.BinaryPath,
		HookSocketDirectory: socketBase,
		HookTimeout:         hookTimeout,
		Logger:              logger,
	}
	server, err := gitapp.New(config)
	if err != nil {
		_ = registry.Close()
		contract.Close()
		provider.Close()
		return fail(fmt.Errorf("git app New: %w", err))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = server.Close()
		_ = registry.Close()
		contract.Close()
		provider.Close()
		return fail(fmt.Errorf("listen: %w", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.StartHooks(ctx); err != nil {
		cancel()
		_ = listener.Close()
		_ = server.Close()
		_ = registry.Close()
		contract.Close()
		provider.Close()
		return fail(fmt.Errorf("StartHooks: %w", err))
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	harness := &Harness{
		TB:           tb,
		Tenant:       TenantID,
		Root:         root,
		StorageRoot:  storageRoot,
		JournalDir:   journalDir,
		SocketBase:   socketBase,
		ClientHome:   clientHome,
		BaseURL:      "http://" + listener.Addr().String(),
		BinaryPath:   options.BinaryPath,
		Provider:     provider,
		Contract:     contract,
		Registry:     registry,
		Admission:    admission,
		Verification: verification,
		Transport:    transport,
		Server:       server,
		HumanToken:   human.Value,
		AdminToken:   admin.Value,
		config:       config,
		cancel:       cancel,
		done:         done,
		listener:     listener,
	}
	tb.Cleanup(func() {
		harness.stop()
		_ = registry.Close()
		contract.Close()
		provider.Close()
		cleanup()
	})
	return harness, nil
}

// stop cancels the serving instance and joins it. It leaves durable state and
// the fixture resources intact so Restart can start a fresh app instance.
func (harness *Harness) stop() {
	if harness.cancel != nil {
		harness.cancel()
		harness.cancel = nil
	}
	if harness.done != nil {
		select {
		case <-harness.done:
		case <-time.After(10 * time.Second):
		}
		harness.done = nil
	}
	if harness.Server != nil {
		_ = harness.Server.Close()
		harness.Server = nil
	}
	if harness.listener != nil {
		_ = harness.listener.Close()
		harness.listener = nil
	}
}

// Restart stops the current app instance and starts a fresh one over the same
// durable storage, journal and fixture boundaries. It models a service restart:
// no in-memory policy or hook state survives; the accepted policy is read from
// the admin repository again.
func (harness *Harness) Restart(tb TB) error {
	tb.Helper()
	harness.stop()
	server, err := gitapp.New(harness.config)
	if err != nil {
		return fmt.Errorf("restart git app New: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = server.Close()
		return fmt.Errorf("restart listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.StartHooks(ctx); err != nil {
		cancel()
		_ = listener.Close()
		_ = server.Close()
		return fmt.Errorf("restart StartHooks: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	harness.Server = server
	harness.listener = listener
	harness.cancel = cancel
	harness.done = done
	harness.BaseURL = "http://" + listener.Addr().String()
	return nil
}

// SeedRepository creates one bare repository with one commit on
// refs/heads/main, bypassing the Git service. It lets a test observe the first
// policy-dependent service operation instead of failing during setup.
func (harness *Harness) SeedRepository(tb TB, repository string) string {
	tb.Helper()
	path := harness.InitBareRepository(tb, repository)
	commit, err := seedBareHistory(harness.Root, path, "seed.txt", "seed "+repository)
	if err != nil {
		tb.Fatalf("seed repository %q: %v", repository, err)
	}
	return commit
}

// SeedAdminPolicy provisions one other tenant's admin repository through the
// product path and installs the given committed conf/git.conf. It is used to
// prove that another tenant's admin repository cannot supply this tenant's
// rules.
func (harness *Harness) SeedAdminPolicy(tb TB, tenant id.TenantID, policy string) string {
	tb.Helper()
	path, err := provisionTenant(harness.Root, harness.StorageRoot, harness.JournalDir, tenant, harness.BinaryPath)
	if err != nil {
		tb.Fatalf("provision admin repository for %q: %v", tenant, err)
	}
	if _, err := seedCommittedPolicy(harness.Root, path, policy); err != nil {
		tb.Fatalf("seed admin policy for %q: %v", tenant, err)
	}
	return path
}

// provisionTenant creates one tenant's admin repository through the real
// product provisioning boundary, exactly as the operator command does.
func provisionTenant(root, storageRoot, journalDirectory string, tenant id.TenantID, binaryPath string) (string, error) {
	result, err := gitapp.ProvisionTenant(context.Background(), gitapp.ProvisionConfig{
		TenantID:         tenant,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDirectory,
		HookBinary:       binaryPath,
	})
	if err != nil {
		return "", fmt.Errorf("provision tenant %q: %w", tenant, err)
	}
	return result.RepositoryPath, nil
}

// seedCommittedPolicy installs one committed conf/git.conf on top of the
// provisioned trusted commit. It is fixture state setup, not a policy source:
// the app still reads only the committed blob, and the write is plumbing on the
// bare repository so no unserved hook callback is required.
func seedCommittedPolicy(root, repositoryPath, policy string) (string, error) {
	parent, err := seedGit(root, "", "--git-dir="+repositoryPath, "rev-parse", "--verify", native.CommittedPolicyRef)
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
	blob, err := seedGit(root, "", "--git-dir="+repositoryPath, "hash-object", "-w", "--no-filters", policyPath)
	if err != nil {
		return "", fmt.Errorf("store fixture policy blob: %w", err)
	}
	blob = strings.TrimSpace(blob)
	indexPath := policyPath + ".index"
	defer os.Remove(indexPath)
	indexEnvironment := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := seedGitEnv(root, indexEnvironment, "--git-dir="+repositoryPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+","+native.CommittedPolicyFile); err != nil {
		return "", fmt.Errorf("index fixture policy: %w", err)
	}
	tree, err := seedGitEnv(root, indexEnvironment, "--git-dir="+repositoryPath, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write fixture policy tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	commit, err := seedGit(root, "", "--git-dir="+repositoryPath,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit-tree", tree, "-p", parent, "-m", "fixture committed policy")
	if err != nil {
		return "", fmt.Errorf("commit fixture policy: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if _, err := seedGit(root, "", "--git-dir="+repositoryPath, "update-ref", native.CommittedPolicyRef, commit); err != nil {
		return "", fmt.Errorf("point the trusted ref at the fixture policy: %w", err)
	}
	committed, err := seedGit(root, "", "--git-dir="+repositoryPath, "cat-file", "blob", native.CommittedPolicyRef+":"+native.CommittedPolicyFile)
	if err != nil {
		return "", fmt.Errorf("read back fixture policy: %w", err)
	}
	if committed != policy {
		return "", fmt.Errorf("committed fixture policy differs from the requested policy")
	}
	return commit, nil
}

// seedBareHistory commits one file to a fresh worktree and pushes it to the
// bare repository's main ref. It returns the committed revision.
func seedBareHistory(root, repositoryPath, name, content string) (string, error) {
	worktree, err := os.MkdirTemp(root, "seed-")
	if err != nil {
		return "", fmt.Errorf("create seed worktree: %w", err)
	}
	defer os.RemoveAll(worktree)
	if _, err := seedGit(root, worktree, "init", "--quiet", "-b", "main"); err != nil {
		return "", err
	}
	file := filepath.Join(worktree, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", fmt.Errorf("create seed directory: %w", err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write seed file: %w", err)
	}
	if _, err := seedGit(root, worktree, "add", "--all"); err != nil {
		return "", err
	}
	if _, err := seedGit(root, worktree,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", "seed"); err != nil {
		return "", err
	}
	if _, err := seedGit(root, worktree, "push", "--quiet", repositoryPath, "HEAD:refs/heads/main"); err != nil {
		return "", err
	}
	output, err := seedGit(root, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// seedGit runs one deterministic host-git command for fixture setup.
func seedGit(root, directory string, args ...string) (string, error) {
	return seedGitWithEnv(root, directory, nil, args...)
}

// seedGitEnv is seedGit with extra environment entries (such as a private
// index file) for fixture setup.
func seedGitEnv(root string, environment []string, args ...string) (string, error) {
	return seedGitWithEnv(root, "", environment, args...)
}

func seedGitWithEnv(root, directory string, environment []string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
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
	return harness.InitBareRepositoryFor(tb, harness.Tenant, repository)
}

// InitBareRepositoryFor creates one bare repository for a tenant.
func (harness *Harness) InitBareRepositoryFor(tb TB, tenant id.TenantID, repository string) string {
	tb.Helper()
	path := harness.TenantRepositoryPath(tenant, repository)
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{Path: path}); err != nil {
		tb.Fatalf("InitBareRepository(%q): %v", repository, err)
	}
	return path
}

// InstallProductHooks installs the two safe product hooks.
func (harness *Harness) InstallProductHooks(tb TB, repository string) {
	tb.Helper()
	if err := harness.Server.InstallHooks(repository); err != nil {
		tb.Fatalf("InstallHooks(%q): %v", repository, err)
	}
}

// WriteHook writes one test-only hook script.
func (harness *Harness) WriteHook(tb TB, repository, phase, content string) {
	tb.Helper()
	path := filepath.Join(harness.RepositoryPath(repository), "hooks", phase)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		tb.Fatalf("WriteHook(%q): %v", phase, err)
	}
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
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	tb.Fatalf("git %v: %v", args, err)
	return "", "", -1
}

// Clone clones one repository with a Basic credential passed on the command
// line (never persisted in the clone config).
func (harness *Harness) Clone(tb TB, token, repository, directory string) (string, int) {
	tb.Helper()
	_, stderr, code := harness.gitIn(tb, "", "-c", "http.extraHeader="+basicHeader(token),
		"clone", harness.BaseURL+"/"+repository+".git", directory)
	return stderr, code
}

// LSRemote returns the ref advertisement the smart-HTTP edge serves for one
// repository. It is the client-visible view, so a hidden ref is absent here.
func (harness *Harness) LSRemote(tb TB, token, repository string) (string, int) {
	tb.Helper()
	stdout, _, code := harness.gitIn(tb, "", "-c", "http.extraHeader="+basicHeader(token),
		"ls-remote", harness.BaseURL+"/"+repository+".git")
	return stdout, code
}

// Push pushes the current branch with a Basic credential.
func (harness *Harness) Push(tb TB, directory, token string, extra ...string) (string, int) {
	tb.Helper()
	args := []string{"-C", directory, "-c", "http.extraHeader=" + basicHeader(token), "push"}
	args = append(args, extra...)
	_, stderr, code := harness.gitIn(tb, "", args...)
	return stderr, code
}

// Fetch fetches origin with a Basic credential.
func (harness *Harness) Fetch(tb TB, directory, token string) (string, int) {
	tb.Helper()
	_, stderr, code := harness.gitIn(tb, "", "-C", directory, "-c", "http.extraHeader="+basicHeader(token), "fetch", "origin")
	return stderr, code
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

// basicHeader returns the Basic authorization value for the kodama username.
func basicHeader(token string) string {
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

// NewHTTPClient returns a loopback HTTP client with a bounded timeout.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
