package factory

// Host-side Git/Shell composition.
//
// WithGitService selects one optional host-only Git smart-HTTP service beside
// the existing factory composition. The service is composed exactly like the
// production `kodama git serve` process: the public file-credential transport,
// the real verification client, the shared actor-handle registry, the strict
// repository-to-resource mapping and the product Run entry point. It owns real
// tenant storage, a real receive journal, the real native hooks and a private
// hook socket; the Shell remains the only authority.
//
// This file covers the host path; git_workload.go adds container
// consumption: the workload image, the workload relay URL and the
// authored workload commands. The relay keeps the Git listener on loopback and
// advertises it as host.containers.internal, which is the only alias a rootless
// pasta workload can reach.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/actors"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// ErrHookSocketPathTooLong reports a fixture hook socket path beyond the
// product's conservative Unix-socket bound. It is a fixture configuration
// failure with a named cause, not an opaque net.Listen error.
var ErrHookSocketPathTooLong = errors.New("hook socket path exceeds the product bound")

// Exported fixture identities. The integration suite asserts this exact policy
// and these exact repository names, so a silent rename cannot pass.
const (
	// GitAdminRepository is the tenant-relative trusted policy repository.
	GitAdminRepository = native.AdminRepository
	// GitProjectRepository is the ordinary fixture repository.
	GitProjectRepository = "project-i04/web"
	// GitDeployRepository is the delivery deploy repository. Its protected
	// ref is refs/heads/deploy, guarded by a reviewer approval.
	GitDeployRepository = "deploy-i04/release"

	// GitPolicy is the fixture access policy pushed through the served admin
	// repository as one accepted commit. It is never installed as a file and
	// never read from a policy-file option.
	GitPolicy = "repo admin\n" +
		"    RW = @admin\n" +
		"\n" +
		"repo project-i04/web\n" +
		"    R = @all\n" +
		"    RW+ refs/heads/work/ = @engineer @execution\n" +
		"    RW refs/heads/main = @execution\n" +
		"    option approve.refs/heads/main = reviewer\n" +
		"\n" +
		"repo deploy-i04/release\n" +
		"    R = @all\n" +
		"    RW+ refs/heads/work/ = @engineer @execution\n" +
		"    RW refs/heads/deploy = @execution\n" +
		"    option approve.refs/heads/deploy = reviewer\n"
)

const (
	gitServiceID            = "service-i04-git"
	gitServiceRole          = "viewer"
	gitServiceCredentialTTL = time.Hour

	gitAdminBindingID        = "binding-i04-git-admin"
	gitAdminResourceID       = "resource-i04-git-admin"
	gitProjectBindingID      = "binding-i04-git-project-web"
	gitProjectResourceID     = "resource-i04-git-project-web"
	gitDeployBindingID       = "binding-i04-git-deploy"
	gitDeployResourceID      = "resource-i04-git-deploy"
	gitReadOperation         = string(api.ResourceOperationGitRead)
	gitRefUpdateOperation    = string(api.ResourceOperationGitRefUpdate)
	gitRefUpdatedReportType  = "git.ref.updated"
	gitServiceJoinTimeout    = 30 * time.Second
	gitServiceStartupTimeout = 5 * time.Minute
	gitCredentialFileMode    = 0o600
	gitRepositoryDirectory   = 0o700
	gitSocketDirectoryPrefix = "kodama-i04-git-"
)

// WithGitService enables the optional host Git composition. It also selects
// the private-CA TLS edge: registered trusted origins and the served smart-HTTP
// listener are HTTPS. OIDC stays an independent selection.
func WithGitService() Option {
	return func(options *Options) { options.GitService = true }
}

// tlsSelected reports whether the harness serves HTTPS. The Git service requires
// it; WithTLS keeps selecting it on its own.
func tlsSelected(options Options) bool {
	return options.TLS || options.GitService
}

// checkGitRuntimeVariant bounds the Git composition to Podman. A kind
// cluster cannot reach the host-local Git listener through the current workload
// alias, so WithGitService refuses a Kubernetes factory before any
// provisioning or cluster effect instead of advertising an unreachable URL.
func checkGitRuntimeVariant(variant runtimeVariant, gitEnabled bool) error {
	if gitEnabled && variant != runtimeVariantPodman {
		return fmt.Errorf("factory: WithGitService is supported only on the Podman runtime, not the Kubernetes runtime")
	}
	return nil
}

// GitFixture is one served host Git composition plus the identities and paths
// a test needs. It owns no behavior under test: every authority fact comes from
// the served Shell and every ref fact from the real repository.
type GitFixture struct {
	TenantID id.TenantID

	ServiceID      string
	ServiceVersion uint64

	AdminBindingID      string
	AdminBindingVersion uint64
	AdminResourceID     string

	ProjectBindingID      string
	ProjectBindingVersion uint64
	ProjectResourceID     string

	DeployBindingID      string
	DeployBindingVersion uint64
	DeployResourceID     string

	AdminRepository   string
	ProjectRepository string
	DeployRepository  string

	StorageRoot      string
	JournalDirectory string
	SocketPath       string
	BinaryPath       string
	CredentialFile   string

	// HostURL is the HTTPS smart-HTTP origin host clients use.
	HostURL string
	// WorkloadURL is the HTTPS smart-HTTP origin containers use:
	// host.containers.internal through the fixture relay.
	WorkloadURL string
	// Image is the workload image after EnsureWorkloadImage. It is nil for
	// a fixture that never requests container consumption.
	Image *GitWorkloadImage
	// GitVersion is the host Git version the product probe accepted.
	GitVersion string
	// PolicyCommit is the accepted served policy successor commit.
	PolicyCommit string

	h        *Harness
	client   hostGitClient
	service  *gitService
	relay    *edgeForwarder
	workload *gitWorkload

	// transport, resourceIDs and logger are the composition inputs a restart
	// reuses. They are the same objects the first Run received, so a restarted
	// service keeps the test's labelled transport seam and the exact strict
	// repository-to-resource mapping.
	transport   http.RoundTripper
	resourceIDs map[string]string
	logger      *slog.Logger

	workloadMu sync.Mutex
}

// ServiceToken reads the mounted service credential. It is the one value a
// fixture test may need for a leak scan; callers must never render it.
func (fixture *GitFixture) ServiceToken(t testing.TB) (string, error) {
	t.Helper()
	data, err := os.ReadFile(fixture.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("read mounted service credential: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("mounted service credential is empty")
	}
	return token, nil
}

// WriteServiceCredential replaces the mounted credential file atomically with
// the exact 0600 mode. The Git transport reads the file on every attempt, so a
// reissued credential takes effect without restarting the service.
func (fixture *GitFixture) WriteServiceCredential(t testing.TB, token string) {
	t.Helper()
	directory := filepath.Dir(fixture.CredentialFile)
	temporary, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		t.Fatalf("create replacement credential file: %v", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.WriteString(strings.TrimSpace(token) + "\n"); err != nil {
		temporary.Close()
		t.Fatalf("write replacement credential file: %v", err)
	}
	if err := temporary.Chmod(gitCredentialFileMode); err != nil {
		temporary.Close()
		t.Fatalf("chmod replacement credential file: %v", err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatalf("close replacement credential file: %v", err)
	}
	if err := os.Rename(name, fixture.CredentialFile); err != nil {
		t.Fatalf("replace mounted credential file: %v", err)
	}
	if err := os.Chmod(fixture.CredentialFile, gitCredentialFileMode); err != nil {
		t.Fatalf("restore credential file mode: %v", err)
	}
}

// ServingDone returns a channel that is closed only after the Git service Run
// call returned. It is a join barrier, not a scheduler hint.
func (fixture *GitFixture) ServingDone() <-chan struct{} {
	if fixture == nil || fixture.service == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return fixture.service.Done()
}

// RepositoryPath returns the tenant-scoped bare repository path.
func (fixture *GitFixture) RepositoryPath(repository string) string {
	return filepath.Join(fixture.StorageRoot, string(fixture.TenantID), filepath.FromSlash(repository))
}

// gitServicePlanSpec is the complete input of the pure composition planner.
type gitServicePlanSpec struct {
	TenantID         id.TenantID
	BaseURL          string
	CredentialFile   string
	RoundTripper     http.RoundTripper
	ResourceIDs      map[string]string
	StorageRoot      string
	JournalDirectory string
	HookBinary       string
	HookSocketPath   string
	Listener         net.Listener
	// TLS, when set, wraps the listener before serving. The fixture always
	// supplies the private-CA server configuration: the advertised URL is HTTPS.
	TLS    *tls.Config
	Logger *slog.Logger
}

// gitServicePlan is one composed service: the product configuration plus the
// concrete boundary objects the unit guards inspect.
type gitServicePlan struct {
	config          gitapp.Config
	transport       *client.HTTPTransport
	verification    *gitapp.VerificationClient
	readVerifier    *git.ReadVerifier
	registry        *actors.Registry
	resourceID      func(repository string) (string, error)
	socketDirectory string
}

// newGitServicePlan composes the production graph without starting anything.
// It performs no filesystem effect beyond validating the credential file and
// opens no listener.
func newGitServicePlan(spec gitServicePlanSpec) (*gitServicePlan, error) {
	if _, err := id.ParseTenantID(string(spec.TenantID)); err != nil {
		return nil, fmt.Errorf("git service plan tenant: %w", err)
	}
	if strings.TrimSpace(spec.BaseURL) == "" {
		return nil, errors.New("git service plan public base URL is empty")
	}
	if spec.Listener == nil {
		return nil, errors.New("git service plan listener is missing")
	}
	if err := validateGitCredentialFile(spec.CredentialFile); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.StorageRoot) == "" || strings.TrimSpace(spec.JournalDirectory) == "" {
		return nil, errors.New("git service plan storage or journal directory is empty")
	}
	if !filepath.IsAbs(spec.HookBinary) {
		return nil, fmt.Errorf("git service plan hook binary %q is not absolute", spec.HookBinary)
	}
	if !filepath.IsAbs(spec.HookSocketPath) {
		return nil, fmt.Errorf("git service plan hook socket %q is not absolute", spec.HookSocketPath)
	}
	if len(spec.HookSocketPath) > native.MaxSocketPathBytes {
		return nil, fmt.Errorf("%w: git service plan hook socket path %q is %d bytes; at most %d are allowed",
			ErrHookSocketPathTooLong, spec.HookSocketPath, len(spec.HookSocketPath), native.MaxSocketPathBytes)
	}
	if len(spec.ResourceIDs) == 0 {
		return nil, errors.New("git service plan has no registered repositories")
	}
	for repository, resourceID := range spec.ResourceIDs {
		if strings.TrimSpace(repository) == "" || strings.TrimSpace(resourceID) == "" {
			return nil, fmt.Errorf("git service plan repository %q has an empty resource identity", repository)
		}
	}

	transport, err := gitapp.NewTransport(gitapp.TransportConfig{
		BaseURL:        spec.BaseURL,
		CredentialFile: spec.CredentialFile,
		RoundTripper:   spec.RoundTripper,
	})
	if err != nil {
		return nil, fmt.Errorf("git service public transport: %w", err)
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		return nil, fmt.Errorf("git service verification client: %w", err)
	}
	registry, err := actors.NewRegistry(actors.Config{TenantID: spec.TenantID})
	if err != nil {
		return nil, fmt.Errorf("git service actor registry: %w", err)
	}
	resolve := strictResourceID(spec.ResourceIDs)
	readVerifier, err := git.NewReadVerifier(verification, resolve)
	if err != nil {
		_ = registry.Close()
		return nil, fmt.Errorf("git service read verifier: %w", err)
	}

	listener := spec.Listener
	if spec.TLS != nil {
		listener = tls.NewListener(listener, spec.TLS)
	}
	config := gitapp.Config{
		TenantID:         spec.TenantID,
		StorageRoot:      spec.StorageRoot,
		JournalDirectory: spec.JournalDirectory,
		BaseURL:          spec.BaseURL,
		CredentialFile:   spec.CredentialFile,
		ResourceIDs:      copyStringMap(spec.ResourceIDs),
		HookBinary:       spec.HookBinary,
		HookSocketPath:   spec.HookSocketPath,
		Listener:         listener,
		Transport:        transport,
		Verify:           verification,
		Actors:           readVerifier,
		ActorHandles:     registry,
		HookActors:       registry,
		Logger:           spec.Logger,
	}
	return &gitServicePlan{
		config:          config,
		transport:       transport,
		verification:    verification,
		readVerifier:    readVerifier,
		registry:        registry,
		resourceID:      resolve,
		socketDirectory: filepath.Dir(spec.HookSocketPath),
	}, nil
}

// resolveResource maps one tenant-relative repository to its registered
// resource identity. It is exported to the plan's unit guard only.
func (plan *gitServicePlan) resolveResource(repository string) (string, error) {
	return plan.resourceID(repository)
}

// strictResourceID returns the strict repository-to-resource mapping. The
// repository name is never substituted for the registered resource identity,
// and an unregistered or empty repository refuses.
func strictResourceID(resourceIDs map[string]string) func(string) (string, error) {
	mapping := copyStringMap(resourceIDs)
	return func(repository string) (string, error) {
		resourceID, ok := mapping[repository]
		if !ok || strings.TrimSpace(resourceID) == "" {
			// Match production: the unauthenticated class makes the smart-HTTP
			// edge answer a uniform 401, never a 500 that would reveal whether
			// the repository is mapped.
			return "", git.UnregisteredRepositoryError(repository)
		}
		return resourceID, nil
	}
}

// validateGitCredentialFile requires the mounted service credential to be a
// regular file no group or other user can read.
func validateGitCredentialFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git service credential file is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat git service credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("git service credential is not a regular file")
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return fmt.Errorf("git service credential file mode %04o exposes the token", permissions)
	}
	if permissions := info.Mode().Perm(); permissions&0o400 == 0 {
		return errors.New("git service credential file is not owner-readable")
	}
	return nil
}

// copyStringMap returns an independent copy of one string map.
func copyStringMap(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}

// gitRunFunc is the product Run entry point, injectable for lifecycle tests.
type gitRunFunc func(context.Context, gitapp.Config) error

// gitService owns one running Git composition and its fallible resources. The
// registry, listener and private socket directory are closed here; the Shell
// and workload adapter belong to the harness.
type gitService struct {
	plan        *gitServicePlan
	run         gitRunFunc
	shellDone   <-chan struct{}
	joinTimeout time.Duration

	mutex      sync.Mutex
	started    bool
	closed     bool
	cancel     context.CancelFunc
	result     error
	exited     chan struct{}
	closeDone  chan struct{}
	closeErr   error
	shellFirst bool
}

// newGitService builds one not-yet-started service.
func newGitService(plan *gitServicePlan, run gitRunFunc, shellDone <-chan struct{}, joinTimeout time.Duration) *gitService {
	if joinTimeout <= 0 {
		joinTimeout = gitServiceJoinTimeout
	}
	return &gitService{
		plan:        plan,
		run:         run,
		shellDone:   shellDone,
		joinTimeout: joinTimeout,
		exited:      make(chan struct{}),
		closeDone:   make(chan struct{}),
	}
}

// start launches the product Run call in its own goroutine. It returns before
// the service is ready; waitReady supplies the readiness barrier.
func (service *gitService) start() error {
	if service == nil || service.run == nil {
		return errors.New("git service has no Run entry point")
	}
	if service.plan == nil || service.plan.config.Listener == nil {
		return errors.New("git service has no listener")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.mutex.Lock()
	if service.started {
		service.mutex.Unlock()
		cancel()
		return errors.New("git service is already started")
	}
	service.started = true
	service.cancel = cancel
	service.mutex.Unlock()
	go func() {
		err := service.run(ctx, service.plan.config)
		service.mutex.Lock()
		service.result = err
		close(service.exited)
		service.mutex.Unlock()
	}()
	return nil
}

// waitReady polls one authenticated protocol check while watching for a Run
// failure. A Run that exits before readiness fails startup immediately instead
// of waiting for the budget.
func (service *gitService) waitReady(ctx context.Context, ready func(context.Context) error) error {
	if ready == nil {
		return errors.New("git service readiness check is missing")
	}
	var lastErr error
	for {
		select {
		case <-service.exited:
			return fmt.Errorf("git service exited during startup: %w", service.failure())
		default:
		}
		if err := ready(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("git service did not become ready: %w (last check: %v)", ctx.Err(), lastErr)
		case <-service.exited:
			return fmt.Errorf("git service exited during startup: %w", service.failure())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// failure returns the Run result once it has exited.
func (service *gitService) failure() error {
	service.mutex.Lock()
	defer service.mutex.Unlock()
	if service.result != nil {
		return service.result
	}
	return errors.New("git service exited without an error")
}

// Done returns the Run join barrier.
func (service *gitService) Done() <-chan struct{} { return service.exited }

// ShellStoppedFirst reports whether the Shell endpoint was already stopped when
// this service started shutting down. The harness closes Git while the Shell is
// still serving; true is a lifecycle defect.
func (service *gitService) ShellStoppedFirst() bool {
	service.mutex.Lock()
	defer service.mutex.Unlock()
	return service.shellFirst
}

// close cancels and joins Run, then closes the registry, the listener and the
// private socket directory. It is idempotent; a Run that does not join is
// reported as an error and never treated as a successful shutdown.
func (service *gitService) close() error {
	if service == nil {
		return nil
	}
	service.mutex.Lock()
	if service.closed {
		done := service.closeDone
		service.mutex.Unlock()
		<-done
		service.mutex.Lock()
		defer service.mutex.Unlock()
		return service.closeErr
	}
	service.closed = true
	select {
	case <-service.shellDone:
		service.shellFirst = true
	default:
	}
	cancel := service.cancel
	started := service.started
	service.mutex.Unlock()

	if cancel != nil {
		cancel()
	}
	var problems []error
	if started {
		select {
		case <-service.exited:
			if err := service.result; err != nil && !expectedServeStop(err) {
				problems = append(problems, fmt.Errorf("git service run: %w", err))
			}
		case <-time.After(service.joinTimeout):
			problems = append(problems, fmt.Errorf("git service did not join within %s", service.joinTimeout))
		}
	}
	if service.plan != nil {
		if service.plan.registry != nil {
			if err := service.plan.registry.Close(); err != nil {
				problems = append(problems, fmt.Errorf("close git actor registry: %w", err))
			}
		}
		if listener := service.plan.config.Listener; listener != nil {
			_ = listener.Close()
		}
		if service.plan.socketDirectory != "" {
			if err := os.RemoveAll(service.plan.socketDirectory); err != nil {
				problems = append(problems, fmt.Errorf("remove git hook socket directory: %w", err))
			}
		}
	}
	err := errors.Join(problems...)
	service.mutex.Lock()
	service.closeErr = err
	close(service.closeDone)
	service.mutex.Unlock()
	return err
}

// expectedServeStop reports Run results that are normal for a canceled serve.
func expectedServeStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed)
}

// launchGitService starts one composition and closes it again when readiness
// fails, so a failed start owns no registry, listener or socket directory.
func launchGitService(ctx context.Context, plan *gitServicePlan, shellDone <-chan struct{}, run gitRunFunc, ready func(context.Context) error, joinTimeout time.Duration) (*gitService, error) {
	service := newGitService(plan, run, shellDone, joinTimeout)
	if err := service.start(); err != nil {
		_ = service.close()
		return nil, err
	}
	if err := service.waitReady(ctx, ready); err != nil {
		_ = service.close()
		return nil, err
	}
	return service, nil
}

// startGitService composes the optional host Git service for a started harness.
// Every failure is fatal to the test: a half-composed Git edge must never be
// handed to a test as if it were usable.
func startGitService(t *testing.T, h *Harness) *GitFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), gitServiceStartupTimeout)
	defer cancel()
	fixture, err := composeGitFixture(ctx, h)
	if err != nil {
		t.Fatalf("factory: compose the host Git service: %v", err)
	}
	// Harness.Close owns the whole shutdown order: workload support, Git and
	// its relay while the Shell still serves, the Shell, then image names.
	h.Git = fixture
	return fixture
}

// composeGitFixture provisions and serves one host Git composition.
func composeGitFixture(ctx context.Context, h *Harness) (*GitFixture, error) {
	if h.TLS == nil {
		return nil, errors.New("the Git service requires the private-CA TLS fixture")
	}
	root := filepath.Join(h.DataDir, "git")
	storageRoot := filepath.Join(root, "repositories")
	journalDirectory := filepath.Join(root, "journal")
	scratch := filepath.Join(root, "work")
	credentials := filepath.Join(root, "credentials")
	for _, directory := range []string{storageRoot, journalDirectory, scratch, credentials} {
		if err := os.MkdirAll(directory, gitRepositoryDirectory); err != nil {
			return nil, fmt.Errorf("create git fixture directory %q: %w", directory, err)
		}
	}

	binary, err := buildHookBinary(ctx, filepath.Join(root, "bin"))
	if err != nil {
		return nil, err
	}
	if _, err := provisionGitRepositories(ctx, gitRepositorySpec{
		TenantID:         h.TenantID,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDirectory,
		HookBinary:       binary,
		ScratchDirectory: scratch,
	}); err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the Git smart-HTTP edge: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("git listener address %v is not a TCP address", listener.Addr())
	}
	hostURL := fmt.Sprintf("https://127.0.0.1:%d", address.Port)
	client, err := newHostGitClient(root, hostURL, h.TLS.CACertFile())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	// The workload relay is the only listener on the host default-route
	// address. The Git listener stays on loopback; rootless pasta maps
	// host.containers.internal to that routable address, so advertising the
	// loopback listener directly would never reach it.
	relay, relayPort, err := newWorkloadEdge(workloadListenHost(h.T), listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("listen for the workload Git relay: %w", err)
	}
	relayOwned := true
	defer func() {
		if relayOwned {
			relay.Close()
		}
	}()
	workloadURL, err := workloadGitURL(relayPort)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	credentialFile := filepath.Join(credentials, "service-credential")
	registration, err := registerGitService(h, hostURL, credentialFile)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	// Slice A registered only the host origin. Add the container-reachable
	// origin through the versioned public update and read both bindings back.
	if err := addWorkloadTrustedOrigin(h, &registration, hostURL, workloadURL); err != nil {
		_ = listener.Close()
		return nil, err
	}

	socketBase, err := os.MkdirTemp("", gitSocketDirectoryPrefix)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("create private hook socket directory: %w", err)
	}
	socketPath := filepath.Join(socketBase, "hook.sock")
	if len(socketPath) > native.MaxSocketPathBytes {
		_ = os.RemoveAll(socketBase)
		_ = listener.Close()
		return nil, fmt.Errorf("%w: hook socket path %q is %d bytes; at most %d are allowed",
			ErrHookSocketPathTooLong, socketPath, len(socketPath), native.MaxSocketPathBytes)
	}

	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gitTransport := h.roundTripper()
	if h.infra.opts.GitTransport != nil {
		gitTransport = h.infra.opts.GitTransport(gitTransport)
		if gitTransport == nil {
			_ = os.RemoveAll(socketBase)
			_ = listener.Close()
			return nil, errors.New("factory: WithGitTransport returned a nil transport")
		}
	}
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         h.TenantID,
		BaseURL:          h.BaseURL,
		CredentialFile:   credentialFile,
		RoundTripper:     gitTransport,
		ResourceIDs:      registration.resourceIDs,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDirectory,
		HookBinary:       binary,
		HookSocketPath:   socketPath,
		Listener:         listener,
		TLS:              h.TLS.ServerConfig(),
		Logger:           logger,
	})
	if err != nil {
		_ = os.RemoveAll(socketBase)
		_ = listener.Close()
		return nil, err
	}

	ready := func(checkCtx context.Context) error {
		_, stderr, code, runErr := client.run(checkCtx, h.AdminToken, "",
			"ls-remote", "--quiet", client.repositoryURL(GitAdminRepository), "refs/heads/main")
		if runErr != nil {
			return runErr
		}
		if code != 0 {
			return fmt.Errorf("admin smart-HTTP read exited %d: %s", code, strings.TrimSpace(stderr))
		}
		return nil
	}
	service, err := launchGitService(ctx, plan, h.serveDone, gitapp.Run, ready, gitServiceJoinTimeout)
	if err != nil {
		_ = os.RemoveAll(socketBase)
		return nil, err
	}

	policyCommit, err := pushAdminPolicy(ctx, client, scratch, h.AdminToken, GitPolicy)
	if err != nil {
		_ = service.close()
		return nil, err
	}

	// The project repository becomes usable only after the served policy
	// commit declares it. Read it with a real current-role actor as the final
	// readiness step; this is a Git protocol read, not a store probe.
	if _, stderr, code, runErr := client.run(ctx, h.EngineerToken, "",
		"ls-remote", "--quiet", client.repositoryURL(GitProjectRepository), "refs/heads/main"); runErr != nil || code != 0 {
		_ = service.close()
		return nil, fmt.Errorf("project repository is not readable after the served policy commit: %v %s", runErr, strings.TrimSpace(stderr))
	}

	version, err := hostGitVersion(ctx)
	if err != nil {
		_ = service.close()
		return nil, err
	}
	relayOwned = false
	return &GitFixture{
		TenantID:              h.TenantID,
		ServiceID:             gitServiceID,
		ServiceVersion:        registration.serviceVersion,
		AdminBindingID:        gitAdminBindingID,
		AdminBindingVersion:   registration.adminBindingVersion,
		AdminResourceID:       gitAdminResourceID,
		ProjectBindingID:      gitProjectBindingID,
		ProjectBindingVersion: registration.projectBindingVersion,
		ProjectResourceID:     gitProjectResourceID,
		DeployBindingID:       gitDeployBindingID,
		DeployBindingVersion:  registration.deployBindingVersion,
		DeployResourceID:      gitDeployResourceID,
		AdminRepository:       GitAdminRepository,
		ProjectRepository:     GitProjectRepository,
		DeployRepository:      GitDeployRepository,
		StorageRoot:           storageRoot,
		JournalDirectory:      journalDirectory,
		SocketPath:            socketPath,
		BinaryPath:            binary,
		CredentialFile:        credentialFile,
		HostURL:               hostURL,
		WorkloadURL:           workloadURL,
		GitVersion:            version,
		PolicyCommit:          policyCommit,
		h:                     h,
		client:                client,
		service:               service,
		relay:                 relay,
		transport:             gitTransport,
		resourceIDs:           copyStringMap(registration.resourceIDs),
		logger:                logger,
	}, nil
}

// gitServiceRegistration is the served service/binding configuration created
// through public operations.
type gitServiceRegistration struct {
	serviceVersion        uint64
	adminBindingVersion   uint64
	projectBindingVersion uint64
	deployBindingVersion  uint64
	resourceIDs           map[string]string
}

// registerGitService creates the enforcing service, issues its real credential
// into a private file and registers the two separate resource bindings. It
// performs no Git provisioning: registration is Shell metadata only.
func registerGitService(h *Harness, hostURL, credentialFile string) (gitServiceRegistration, error) {
	var created api.ServiceCreateResult
	if err := gitRest(h, h.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: ProjectID,
		ServiceID: gitServiceID,
		Roles:     []string{gitServiceRole},
	}, nil, "create-"+gitServiceID, &created); err != nil {
		return gitServiceRegistration{}, err
	}
	var issued api.ServiceCredentialIssueResult
	if err := gitRest(h, h.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  gitServiceID,
		TTLSeconds: int(gitServiceCredentialTTL / time.Second),
	}, &created.Version, "issue-"+gitServiceID, &issued); err != nil {
		return gitServiceRegistration{}, err
	}
	if issued.Token == nil || strings.TrimSpace(*issued.Token) == "" {
		return gitServiceRegistration{}, errors.New("service.credential.issue returned no one-time token")
	}
	if err := writeGitCredentialFile(credentialFile, *issued.Token); err != nil {
		return gitServiceRegistration{}, err
	}

	operations := []string{gitReadOperation, gitRefUpdateOperation}
	reportable := []string{gitRefUpdatedReportType}
	var adminBinding api.ResourceBindingRegisterResult
	if err := gitRest(h, h.ProjectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        ProjectID,
		BindingID:        gitAdminBindingID,
		ResourceID:       gitAdminResourceID,
		EnforcingService: gitServiceID,
		TrustedOrigins:   []string{hostURL},
		Operations:       operations,
		ReportableEvents: reportable,
		Metadata: map[string]string{
			"repository":     GitAdminRepository,
			"default_branch": "main",
		},
	}, nil, "register-"+gitAdminBindingID, &adminBinding); err != nil {
		return gitServiceRegistration{}, err
	}
	var projectBinding api.ResourceBindingRegisterResult
	if err := gitRest(h, h.ProjectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        ProjectID,
		BindingID:        gitProjectBindingID,
		ResourceID:       gitProjectResourceID,
		EnforcingService: gitServiceID,
		TrustedOrigins:   []string{hostURL},
		Operations:       operations,
		ReportableEvents: reportable,
		Metadata: map[string]string{
			"repository":     GitProjectRepository,
			"default_branch": "main",
		},
	}, nil, "register-"+gitProjectBindingID, &projectBinding); err != nil {
		return gitServiceRegistration{}, err
	}
	var deployBinding api.ResourceBindingRegisterResult
	if err := gitRest(h, h.ProjectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        ProjectID,
		BindingID:        gitDeployBindingID,
		ResourceID:       gitDeployResourceID,
		EnforcingService: gitServiceID,
		TrustedOrigins:   []string{hostURL},
		Operations:       operations,
		ReportableEvents: reportable,
		Metadata: map[string]string{
			"repository":     GitDeployRepository,
			"default_branch": "deploy",
		},
	}, nil, "register-"+gitDeployBindingID, &deployBinding); err != nil {
		return gitServiceRegistration{}, err
	}
	return gitServiceRegistration{
		serviceVersion:        issued.Version,
		adminBindingVersion:   adminBinding.Version,
		projectBindingVersion: projectBinding.Version,
		deployBindingVersion:  deployBinding.Version,
		resourceIDs: map[string]string{
			GitAdminRepository:   gitAdminResourceID,
			GitProjectRepository: gitProjectResourceID,
			GitDeployRepository:  gitDeployResourceID,
		},
	}, nil
}

// gitRest performs one public operation and decodes its result without failing
// the test, so a setup failure can clean up owned resources first.
func gitRest(h *Harness, token, operation string, input any, expected *uint64, key string, out any) error {
	result, publicError := h.REST(token, operation, input, expected, key)
	if publicError.Code != "" {
		return fmt.Errorf("%s refused: %s (%s)", operation, publicError.Code, publicError.Message)
	}
	if out != nil {
		if err := decodeResultBody(result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", operation, err)
		}
	}
	return nil
}

// writeGitCredentialFile writes one fresh credential into a private file. The
// token is never logged, printed or placed in Git configuration.
func writeGitCredentialFile(path, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("git service credential is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, gitRepositoryDirectory); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return fmt.Errorf("create credential file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.WriteString(strings.TrimSpace(token) + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := temporary.Chmod(gitCredentialFileMode); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod credential file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close credential file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish credential file: %w", err)
	}
	if err := os.Chmod(path, gitCredentialFileMode); err != nil {
		return fmt.Errorf("restore credential file mode: %w", err)
	}
	return nil
}

// decodeResultBody decodes one public result body.
func decodeResultBody(result api.Result, target any) error {
	return jsonUnmarshal(result.Body, target)
}

// jsonUnmarshal isolates the one JSON dependency of the setup helpers.
func jsonUnmarshal(data []byte, target any) error {
	return json.Unmarshal(data, target)
}
