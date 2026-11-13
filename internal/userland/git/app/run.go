package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// ErrDisabled reports that `git serve` was asked to start without the
// boundaries the declared ports require. It is a clean refusal, never a partial
// service: a missing actor port would silently skip read authentication or
// update verification. The message names the configuration omission, never an
// internal owner: the production entry wires these ports.
var ErrDisabled = errors.New("git serve is disabled: smart-HTTP actor authentication and actor handle ports are required")

// shutdownTimeout bounds one graceful smart-HTTP shutdown.
const shutdownTimeout = 10 * time.Second

// defaultReadHeaderTimeout bounds how long a client may take to send request
// headers. It is used when a Server is built without an explicit override.
const defaultReadHeaderTimeout = 10 * time.Second

// Config is the complete `git serve` composition input. The production entry
// wires the actor ports; when they are absent the service refuses instead of
// serving an unauthenticated Git edge.
type Config struct {
	TenantID         id.TenantID
	ListenAddress    string
	StorageRoot      string
	JournalDirectory string
	// The access policy is read only from the committed conf/git.conf of the
	// tenant's provisioned admin repository. There is no policy-file option.
	BaseURL          string
	CredentialFile   string
	ResourceIDs      map[string]string
	MaxBodyBytes     int64
	MaxResponseBytes int64
	Timeout          time.Duration
	Logger           *slog.Logger

	// HookBinary is the absolute kodama binary the installed bare-repository
	// hooks exec. A zero value uses the running executable.
	HookBinary string
	// HookSocketPath is the explicit private Unix socket path. A zero value
	// derives a unique path under HookSocketDirectory.
	HookSocketPath string
	// HookSocketDirectory is the parent directory for the generated private
	// socket directory. A zero value uses os.TempDir.
	HookSocketDirectory string
	// HookTimeout bounds one hook callback. A zero value uses the native
	// default.
	HookTimeout time.Duration

	// Listener is an optional injected listener. A nil value listens on
	// ListenAddress.
	Listener net.Listener

	// Injected boundaries. A nil value selects the documented OS/public
	// default where one exists.
	Executor  git.CommandExecutor
	Observer  git.RefObserver
	Verify    git.VerificationClient
	Retention git.RetentionQuery
	Transport client.Transport
	Backend   git.GitBackend
	Clock     git.Clock

	// ActorPorts are the smart-HTTP/hook identity boundaries. The
	// smart-HTTP read verifier performs the repository-level git.read check.
	Actors       git.ReadActorVerifier
	ActorHandles git.ActorHandleIssuer
	HookActors   git.ActorSource
	SourceEvent  func() (string, error)
}

// Server is one constructed smart-HTTP Git service. It owns no goroutine until
// Serve, StartHooks or StartRecovery is called.
type Server struct {
	handler     http.Handler
	hook        *git.Hook
	service     *git.Service
	logger      *slog.Logger
	storage     git.Storage
	storageRoot string
	executor    git.CommandExecutor
	tenant      id.TenantID
	journal     *git.FileJournal
	clock       git.Clock
	// journalDirectory is the canonical receive-journal directory the recovery
	// sweep scans. It is outside every served repository path by construction.
	journalDirectory  string
	hookBinary        string
	socketPath        string
	hooks             *native.SocketServer
	hooksMutex        sync.Mutex
	hooksCancel       context.CancelFunc
	hooksDone         chan error
	hooksLive         bool
	repositories      []string
	staticResourceIDs map[string]string
	provisionMu       sync.Mutex
	provisionLocks    map[string]*sync.Mutex
	dynamicReadyMu    sync.Mutex
	dynamicReady      map[string]bool
	// receiveMutex guards the active in-flight git-receive-pack exchange count
	// per repository, so the recovery sweep never settles a fresh intent while
	// receive-pack may still apply it.
	receiveMutex   sync.Mutex
	activeReceives map[string]int
	// recovery* own the startup and periodic report-recovery sweep. The
	// interval, accepted-entry batch and cheap-skip scan limit are fixed
	// defaults; a zero value selects the documented production bound, and this
	// package's tests override them. recoveryFirstSweep marks the one pass that
	// scans the whole journal directory after startup.
	recoveryMutex      sync.Mutex
	recoveryCancel     context.CancelFunc
	recoveryDone       chan struct{}
	recoveryLive       bool
	recoveryInterval   time.Duration
	recoveryBatch      int
	recoveryScanLimit  int
	recoveryOffset     int
	recoveryFirstSweep bool
	// readHeaderTimeout bounds how long a client may take to send request
	// headers. It is fixed by default and overridable only from this package's
	// tests so the slowloris case runs quickly.
	readHeaderTimeout time.Duration
}

// Handler returns the smart-HTTP handler.
func (server *Server) Handler() http.Handler { return server.handler }

// Hook returns the native update/pre-receive hook adapter.
func (server *Server) Hook() *git.Hook { return server.hook }

// Service returns the tenant-bound Git service.
func (server *Server) Service() *git.Service { return server.service }

// New builds the smart-HTTP service and the native hook from the configured
// boundaries. It starts no listener and no goroutine. It checks the native Git
// version before any filesystem effect; Run supplies the serving context.
func New(config Config) (*Server, error) {
	return newServer(context.Background(), config)
}

// newServer is the context-aware constructor. It declares the native Git
// minimum through the same command executor the service uses, before storage,
// journal, hooks or listeners exist.
func newServer(ctx context.Context, config Config) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return nil, fmt.Errorf("git serve tenant: %w", err)
	}
	if strings.TrimSpace(config.StorageRoot) == "" {
		return nil, fmt.Errorf("git serve storage root is empty")
	}
	if strings.TrimSpace(config.JournalDirectory) == "" {
		return nil, fmt.Errorf("git serve journal directory is empty")
	}
	if config.Actors == nil || config.ActorHandles == nil || config.HookActors == nil {
		return nil, ErrDisabled
	}

	executor := config.Executor
	if executor == nil {
		var err error
		// The default executor also performs every committed-policy read, so a
		// hostile home, global or system Git configuration must never reach it.
		executor, err = NewExecutor(ExecutorConfig{Env: provisionGitEnvironment(nil)})
		if err != nil {
			return nil, err
		}
	}
	// The served service and its hooks run this executable boundary, so the
	// declared minimum is checked here, before storage and journal creation,
	// hook installation or a listener can have an effect.
	if err := native.RequireGitVersion(ctx, executor); err != nil {
		return nil, fmt.Errorf("git serve: %w", err)
	}

	storageRoot, err := filepath.Abs(config.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git serve storage root: %w", err)
	}
	storageRoot = filepath.Clean(storageRoot)
	localStorage, err := git.NewLocalStorage(storageRoot)
	if err != nil {
		return nil, err
	}
	storage, err := native.NewSafeStorage(localStorage, storageRoot, config.TenantID)
	if err != nil {
		return nil, err
	}
	journalDirectory, err := filepath.Abs(config.JournalDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve git serve journal directory: %w", err)
	}
	journalDirectory = filepath.Clean(journalDirectory)
	if err := requireJournalOutsideRepositories(journalDirectory, storage, config.TenantID, config.ResourceIDs); err != nil {
		return nil, err
	}
	journal, err := git.NewFileJournal(journalDirectory)
	if err != nil {
		return nil, err
	}
	adminPath, err := storage.RepositoryPath(config.TenantID, native.AdminRepository)
	if err != nil {
		return nil, err
	}
	policies, err := native.NewCommittedPolicy(native.CommittedPolicyConfig{
		RepositoryPath: adminPath,
		Executor:       executor,
	})
	if err != nil {
		return nil, err
	}
	observer := config.Observer
	if observer == nil {
		observer, err = NewObserver(ObserverConfig{TenantID: config.TenantID, Storage: storage, Executor: executor})
		if err != nil {
			return nil, err
		}
	}
	verify := config.Verify
	if verify == nil {
		if config.Transport == nil {
			return nil, fmt.Errorf("git serve verification client or public transport is required")
		}
		verify, err = NewVerificationClient(config.Transport)
		if err != nil {
			return nil, err
		}
	}
	retention := config.Retention
	if retention == nil {
		if config.Transport != nil {
			retention, err = NewRetentionClient(config.Transport)
			if err != nil {
				return nil, err
			}
		} else {
			retention = &RetentionClient{}
		}
	}
	clock := config.Clock
	if clock == nil {
		clock = SystemClock{}
	}

	service, err := git.NewService(git.ServiceConfig{
		TenantID:    config.TenantID,
		Storage:     storage,
		Policies:    policies,
		Executor:    executor,
		Observer:    observer,
		Journal:     journal,
		Verify:      verify,
		Retention:   retention,
		Clock:       clock,
		ResourceIDs: config.ResourceIDs,
	})
	if err != nil {
		return nil, err
	}

	socketPath := strings.TrimSpace(config.HookSocketPath)
	removeSocketDirectory := false
	if socketPath == "" {
		socketPath, err = native.SocketPath(config.HookSocketDirectory, config.TenantID)
		if err != nil {
			return nil, err
		}
		removeSocketDirectory = true
	}
	hookBinary := strings.TrimSpace(config.HookBinary)
	if hookBinary == "" {
		hookBinary, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve kodama hook binary: %w", err)
		}
	}
	// The trusted admin repository must validate its own policy pushes before
	// receive-pack moves refs. Install the managed hooks when it already
	// exists; provisioning a new repository calls InstallHooks at creation.
	if info, statErr := os.Stat(adminPath); statErr == nil && info.IsDir() {
		if _, err := native.InstallHooks(native.HooksConfig{
			RepositoryPath: adminPath,
			BinaryPath:     hookBinary,
		}); err != nil {
			return nil, fmt.Errorf("install trusted admin policy hooks: %w", err)
		}
	}
	hookHandler, err := native.NewHookHandler(native.HookHandlerConfig{
		Service:          service,
		Actors:           config.HookActors,
		Policy:           policies,
		Logger:           config.Logger,
		PolicyRepository: native.AdminRepository,
		PolicyValidator:  policies,
		RepositoryPath: func(repository string) (string, error) {
			return storage.RepositoryPath(config.TenantID, repository)
		},
	})
	if err != nil {
		return nil, err
	}
	resourceIDs := config.ResourceIDs
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	staticResourceIDs := make(map[string]string, len(resourceIDs))
	repositories := make([]string, 0, len(resourceIDs))
	for repository, resourceID := range resourceIDs {
		repositories = append(repositories, repository)
		if trimmed := strings.TrimSpace(resourceID); trimmed != "" {
			staticResourceIDs[repository] = trimmed
		}
	}
	sort.Strings(repositories)

	server := &Server{
		service:            service,
		logger:             logger,
		storage:            storage,
		storageRoot:        storageRoot,
		executor:           executor,
		tenant:             config.TenantID,
		journal:            journal,
		clock:              clock,
		journalDirectory:   journalDirectory,
		hookBinary:         hookBinary,
		socketPath:         socketPath,
		repositories:       repositories,
		staticResourceIDs:  staticResourceIDs,
		provisionLocks:     make(map[string]*sync.Mutex),
		dynamicReady:       make(map[string]bool),
		activeReceives:     make(map[string]int),
		readHeaderTimeout:  defaultReadHeaderTimeout,
		recoveryInterval:   defaultRecoveryInterval,
		recoveryBatch:      defaultRecoveryBatch,
		recoveryScanLimit:  defaultRecoveryScanLimit,
		recoveryFirstSweep: true,
	}

	hookServer, err := native.NewSocketServer(native.SocketConfig{
		Path:            socketPath,
		Handler:         hookHandler,
		Timeout:         config.HookTimeout,
		RemoveDirectory: removeSocketDirectory,
	})
	if err != nil {
		return nil, err
	}

	backend := config.Backend
	if backend == nil {
		// The backend is the installed-hook enforcement point: isolate it from
		// inherited Git configuration (a hostile global core.hooksPath would
		// otherwise bypass the managed hooks) and keep only the private hook
		// socket it must forward.
		environment := append(provisionGitEnvironment(nil), native.EnvHookSocket+"="+socketPath)
		environment = append(environment, KeepRefHideEnvironment()...)
		backend, err = NewHTTPBackend(BackendConfig{
			Env:              environment,
			MaxBodyBytes:     config.MaxBodyBytes,
			MaxResponseBytes: config.MaxResponseBytes,
			Timeout:          config.Timeout,
		})
		if err != nil {
			return nil, err
		}
	}
	backend = trackingBackend{inner: backend, server: server}

	sourceEvent := config.SourceEvent
	if sourceEvent == nil {
		sourceEvent = git.NewSourceEventID()
	}
	actors := config.Actors
	if readVerifier, ok := actors.(*git.ReadVerifier); ok {
		actors, err = readVerifier.WithResourceResolver(service.ResolveResourceID)
		if err != nil {
			return nil, err
		}
	}
	smartHTTP, err := git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:           service,
		Backend:           backend,
		Actors:            actors,
		ActorHandles:      config.ActorHandles,
		ResourceIDContext: server.prepareServedRepository,
		SourceEvent:       sourceEvent,
		MaxBodyBytes:      config.MaxBodyBytes,
		Timeout:           config.Timeout,
	})
	if err != nil {
		return nil, err
	}
	hook, err := git.NewHook(git.HookConfig{
		Service: service,
		Actors:  config.HookActors,
	})
	if err != nil {
		return nil, err
	}
	server.handler = smartHTTP
	server.hook = hook
	server.hooks = hookServer
	return server, nil
}

// prepareServedRepository resolves the repository's registered resource
// identity and, for repositories dynamically declared in `admin/conf/git.conf`
// via `option resource = <id>` rather than the startup ResourceIDs map,
// idempotently initializes the bare repository and managed hooks on demand
// before the backend runs.
func (server *Server) prepareServedRepository(ctx context.Context, repository string) (string, error) {
	resourceID, err := server.service.ResolveResourceID(ctx, repository)
	if err != nil {
		return "", err
	}
	if _, static := server.staticResourceIDs[repository]; static || repository == native.AdminRepository {
		return resourceID, nil
	}
	if err := server.ensureDynamicRepository(ctx, repository); err != nil {
		return "", err
	}
	return resourceID, nil
}

func (server *Server) repositoryProvisionLock(repository string) *sync.Mutex {
	server.provisionMu.Lock()
	defer server.provisionMu.Unlock()
	if server.provisionLocks == nil {
		server.provisionLocks = make(map[string]*sync.Mutex)
	}
	lock := server.provisionLocks[repository]
	if lock == nil {
		lock = &sync.Mutex{}
		server.provisionLocks[repository] = lock
	}
	return lock
}

func (server *Server) isDynamicReady(repository string) bool {
	server.dynamicReadyMu.Lock()
	defer server.dynamicReadyMu.Unlock()
	return server.dynamicReady[repository]
}

func (server *Server) markDynamicReady(repository string) {
	server.dynamicReadyMu.Lock()
	if server.dynamicReady == nil {
		server.dynamicReady = make(map[string]bool)
	}
	server.dynamicReady[repository] = true
	server.dynamicReadyMu.Unlock()
}

// ensureDynamicRepository idempotently initializes a dynamic repository's bare
// storage and managed pre-receive/post-receive hooks under a per-repository
// mutex and the tenant provisioning lock.
func (server *Server) ensureDynamicRepository(ctx context.Context, repository string) error {
	repositoryPath, err := server.storage.RepositoryPath(server.tenant, repository)
	if err != nil {
		if errors.Is(err, git.ErrInvalidRepository) {
			return fault.Wrap(fault.InvalidInput, "invalid repository path", err)
		}
		return fault.Wrap(fault.DependencyUnavailable, "repository storage is unavailable", err)
	}
	if err := requireJournalOutsidePaths(server.journalDirectory, map[string]string{repository: repositoryPath}); err != nil {
		return fault.Wrap(fault.Forbidden, "repository path conflicts with service journal", err)
	}
	lock := server.repositoryProvisionLock(repository)
	lock.Lock()
	defer lock.Unlock()

	if server.isDynamicReady(repository) && managedBareRepositoryPresent(repositoryPath) {
		return nil
	}
	_, err = native.EnsureBareRepository(ctx, native.BareProvisionConfig{
		TenantID:        server.tenant,
		TenantDirectory: filepath.Join(server.storageRoot, string(server.tenant)),
		RepositoryPath:  repositoryPath,
		HookBinaryPath:  server.hookBinary,
		Executor:        server.executor,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, native.ErrRepositoryProvisionDestination) ||
			errors.Is(err, native.ErrRepositoryProvisionContainment) ||
			errors.Is(err, native.ErrForeignHook) ||
			errors.Is(err, native.ErrHookSymlink) {
			return fault.Wrap(fault.Forbidden, "repository storage cannot be initialized", err)
		}
		return fault.Wrap(fault.DependencyUnavailable, "repository initialization failed", err)
	}
	server.markDynamicReady(repository)
	return nil
}

// managedBareRepositoryPresent checks without subprocesses that repositoryPath
// still has its bare layout and managed hook files as regular, non-symlinked
// entries. Any discrepancy falls back to EnsureBareRepository.
func managedBareRepositoryPresent(repositoryPath string) bool {
	info, err := os.Lstat(repositoryPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	for _, dir := range []string{"objects", "refs", "hooks"} {
		child, err := os.Lstat(filepath.Join(repositoryPath, dir))
		if err != nil || !child.IsDir() || child.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	for _, file := range []string{"HEAD", filepath.Join("hooks", "pre-receive"), filepath.Join("hooks", "post-receive")} {
		child, err := os.Lstat(filepath.Join(repositoryPath, file))
		if err != nil || !child.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// trackingBackend counts active in-flight git-receive-pack exchanges per
// repository. The recovery sweep reads the count so a fresh intent is never
// settled while receive-pack may still apply it.
type trackingBackend struct {
	inner  git.GitBackend
	server *Server
}

func (backend trackingBackend) Serve(ctx context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	if request.Service != "git-receive-pack" {
		return backend.inner.Serve(ctx, request, body, response)
	}
	defer backend.server.beginReceivePack(request.Repository)()
	return backend.inner.Serve(ctx, request, body, response)
}

// KeepRefHideEnvironment pins the managed retained-commit namespace out of
// smart-HTTP advertisement. uploadpack.hideRefs covers the upload-pack
// advertisement and transfer.hideRefs covers the protocol-v2 ref advertisement
// and receive-pack, so a client can neither fetch nor discover a keep ref.
func KeepRefHideEnvironment() []string {
	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=uploadpack.hideRefs",
		"GIT_CONFIG_VALUE_0=" + git.KeepRefNamespace,
		"GIT_CONFIG_KEY_1=transfer.hideRefs",
		"GIT_CONFIG_VALUE_1=" + git.KeepRefNamespace,
	}
}

// HookSocketPath returns the private hook callback socket path.
func (server *Server) HookSocketPath() string { return server.socketPath }

// newServedHTTPServer builds the HTTP server Serve runs. It exists so a test
// can observe the request-header bound Serve actually applies to a Server whose
// field was zeroed; a real installation always builds the standard server.
var newServedHTTPServer = func(handler http.Handler, readHeaderTimeout time.Duration) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
}

// effectiveReadHeaderTimeout applies defaultReadHeaderTimeout when a Server was
// built without one, so a zero (or negative) value can never remove the
// slowloris bound from the served listener.
func (server *Server) effectiveReadHeaderTimeout() time.Duration {
	if server.readHeaderTimeout > 0 {
		return server.readHeaderTimeout
	}
	return defaultReadHeaderTimeout
}

// StartHooks binds the private hook callback socket and serves it until ctx
// ends or CloseHooks is called. It is idempotent and safe for concurrent use.
func (server *Server) StartHooks(ctx context.Context) error {
	server.hooksMutex.Lock()
	defer server.hooksMutex.Unlock()
	if server.hooksLive {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := server.hooks.Listen(); err != nil {
		return err
	}
	hookCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	server.hooksCancel = cancel
	server.hooksDone = done
	server.hooksLive = true
	go func() { done <- server.hooks.Serve(hookCtx) }()
	return nil
}

// CloseHooks stops the private hook callback channel and joins it.
func (server *Server) CloseHooks() error {
	server.hooksMutex.Lock()
	if !server.hooksLive {
		server.hooksMutex.Unlock()
		return nil
	}
	cancel := server.hooksCancel
	done := server.hooksDone
	server.hooksLive = false
	server.hooksCancel = nil
	server.hooksDone = nil
	server.hooksMutex.Unlock()
	if cancel != nil {
		cancel()
	}
	closeErr := server.hooks.Close()
	if done != nil {
		if err := <-done; err != nil {
			closeErr = err
		}
	}
	return closeErr
}

// InstallHooks installs the two safe native hooks into one tenant repository.
func (server *Server) InstallHooks(repository string) error {
	path, err := server.storage.RepositoryPath(server.tenant, repository)
	if err != nil {
		return err
	}
	_, err = native.InstallHooks(native.HooksConfig{
		RepositoryPath: path,
		BinaryPath:     server.hookBinary,
	})
	return err
}

// InstallAllHooks installs hooks into every configured repository that already
// exists. A repository that has not been provisioned yet is skipped; the
// provisioning step calls InstallHooks when it creates the directory.
func (server *Server) InstallAllHooks() error {
	for _, repository := range server.repositories {
		path, err := server.storage.RepositoryPath(server.tenant, repository)
		if err != nil {
			return err
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		if err := server.InstallHooks(repository); err != nil {
			return err
		}
	}
	return nil
}

// Close stops the recovery sweep and the hook channel. It is safe to call
// more than once.
func (server *Server) Close() error {
	recoveryErr := server.CloseRecovery()
	hookErr := server.CloseHooks()
	if recoveryErr != nil {
		return recoveryErr
	}
	return hookErr
}

// Serve serves the smart-HTTP handler until ctx is canceled or the server
// fails. It joins the serving goroutine before returning.
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if listener == nil {
		return fmt.Errorf("git serve requires a listener")
	}
	if err := server.StartHooks(ctx); err != nil {
		return err
	}
	defer func() { _ = server.CloseHooks() }()
	// ReadHeaderTimeout bounds a client that opens a connection and never
	// finishes its request headers (slowloris), so one connection cannot pin a
	// serving goroutine indefinitely. A zero or negative field value can never
	// disable the bound.
	httpServer := newServedHTTPServer(server.handler, server.effectiveReadHeaderTimeout())
	served := make(chan error, 1)
	go func() {
		served <- httpServer.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		<-served
		return nil
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Run builds and serves the smart-HTTP Git service. It refuses cleanly when the
// actor ports are absent and never opens the Shell store.
func Run(ctx context.Context, config Config) error {
	server, err := newServer(ctx, config)
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	if err := server.InstallAllHooks(); err != nil {
		return err
	}
	// Start the startup-and-periodic report recovery before serving. A crash
	// between acceptance and reporting leaves an accepted journal entry; the
	// immediate startup pass scans the whole journal directory, so it reaches
	// and retries that report without waiting for another push or for the
	// settled history ahead of it to age out. The Run defer and CloseRecovery
	// both join the goroutine.
	if err := server.StartRecovery(ctx); err != nil {
		return err
	}
	defer func() { _ = server.CloseRecovery() }()
	listener := config.Listener
	if listener == nil {
		if strings.TrimSpace(config.ListenAddress) == "" {
			return fmt.Errorf("git serve listen address is empty")
		}
		listener, err = net.Listen("tcp", config.ListenAddress)
		if err != nil {
			return err
		}
	}
	return server.Serve(ctx, listener)
}

// SystemClock is the production time boundary.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }
