// Package serve assembles the simplified kernel: PostgreSQL store, one executor,
// the HTTP API and the scheduler loop. It is the Phase 3 cutover target for
// `kodama server --assembly kernel`.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/bus"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/store"
	"github.com/optimiware/kodama/internal/storage"
	taskpg "github.com/optimiware/kodama/internal/tasks/postgres"
	secretkeys "github.com/optimiware/kodama/pkg/secretkeys"
)

type Config struct {
	Addr           string
	DatabaseURL    string
	InstallationID string
	Env            string
	Executor       string // podman | process | kubernetes
	LogDir         string
	StateDir       string
	Keyfile        string
	// KMSFile is the installation's strict key-provider document. It is used
	// instead of the development keyfile when set.
	KMSFile string
	// Kube is the Kubernetes execution configuration. Required when Executor is
	// kubernetes; refused otherwise.
	Kube *runner.KubeConfig

	// Development-only static credential. Refused in production.
	DevToken  string
	DevUserID string
	DevName   string

	OIDCIssuer   string
	OIDCClientID string

	// Git hosting (O-36). PublicURL renders clone URLs. WorkerURL addresses the
	// tenants' workers ({tenantId} is replaced); empty hosts one worker per tenant
	// in this process, in development only. The signing keys and the control
	// token are required in production. GitInProcess is the explicit stage-only
	// embedding of the workers in the server (a persistent state directory); it
	// is refused in every other production profile.
	PublicURL            string
	GitWorkerURL         string
	GitControlToken      string
	GitForwardingKeyFile string
	GitIdentityKeyFile   string
	GitPollInterval      time.Duration
	GitInProcess         bool
	GitInProcessAck      string

	// Events (T-168). NATSURL selects JetStream for the outbox publisher and the
	// dispatcher; empty delivers in process (the outbox rows stay the durable
	// state). EventInterval is the outbox poll period. Bus replaces the broker
	// (tests).
	NATSURL       string
	NATSToken     string
	EventInterval time.Duration
	Bus           bus.Bus
}

// Run blocks until ctx is cancelled or the listener fails.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.DatabaseURL == "" {
		return errors.New("kernel: a database runtime URL is required")
	}
	if cfg.InstallationID == "" {
		return errors.New("kernel: an installation id is required")
	}
	pool, err := storage.Open(ctx, storage.Config{URL: cfg.DatabaseURL, ApplicationName: "kodama-kernel"})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := verifyRuntimeRole(ctx, pool); err != nil {
		return err
	}
	st := store.New(pool.Pool)

	keys, closeKeys, err := keyProvider(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeKeys()

	auth, err := authenticator(ctx, cfg)
	if err != nil {
		return err
	}

	backend, err := chooseBackend(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o750); err != nil {
		return fmt.Errorf("kernel: log directory: %w", err)
	}
	if cfg.StateDir != "" {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, "secret-tmp"), 0o750); err != nil {
			return fmt.Errorf("kernel: state directory: %w", err)
		}
		// Run Git credentials: private to this process, never inside a workspace.
		// Files left by a process that died are stale: their runs are over.
		_ = os.RemoveAll(filepath.Join(cfg.StateDir, "git-credentials"))
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, "git-credentials"), 0o700); err != nil {
			return fmt.Errorf("kernel: state directory: %w", err)
		}
	}

	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	broker, closeBroker, err := connectBus(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeBroker()
	// The publisher exists before the Git routes so that a committed receive can
	// wake it; the dispatcher needs the workers and is attached after them.
	kick := make(chan struct{}, 1)
	git, workers, publicURL, err := buildGit(cfg, st, auth, log, func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer workers.Close()
	r, err := runner.New(runner.Config{
		Store: st, Backend: backend, Keys: keys, InstallationID: cfg.InstallationID,
		LogDir: filepath.Join(cfg.LogDir, "runs"), Logger: log,
		SecretDir: filepath.Join(cfg.StateDir, "secret-tmp"),
		Git:       runGitSources(backend, st, git, publicURL, cfg.StateDir, log),
	})
	if err != nil {
		return err
	}
	events, err := NewEvents(ctx, EventsConfig{Store: st, Files: workers, Bus: broker, Interval: cfg.EventInterval, Wake: notify, Logger: log})
	if err != nil {
		return err
	}
	defer events.Close()
	srv, err := api.New(api.Config{
		Store: st, Auth: auth, Tasks: taskpg.Store{DB: pool.Pool}, Keys: keys,
		InstallationID: cfg.InstallationID, Backend: backend.Name(), Wake: notify, Logger: log,
		Git: git,
		Ready: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("database: %w", err)
			}
			if workers != nil {
				if err := workers.Ready(); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	var wg sync.WaitGroup
	loopCtx, stopLoop := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		loop(loopCtx, st, r, log, wake)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		reconcileLoop(loopCtx, workers, st.TenantIDs, func(ctx context.Context, tenant domain.TenantID) bool {
			if repos, err := st.ProvisioningRepositories(ctx, tenant, 1); err == nil && len(repos) > 0 {
				return true
			}
			repos, err := st.ReconcilableRepositories(ctx, tenant, "", 1)
			return err == nil && len(repos) > 0
		}, cfg.GitPollInterval, log)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-kick:
				events.Publisher.Kick()
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		events.Run(loopCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("kernel listening", "address", cfg.Addr, "executor", backend.Name())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("kernel listener stopped", "error", err)
			stopLoop()
		}
	}()

	<-loopCtx.Done()
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	stopLoop()
	wg.Wait()
	return nil
}

// loop claims one queued run per tenant at a time, so a single process never
// runs two steps of different runs concurrently by accident.
func loop(ctx context.Context, st *store.Store, r *runner.Runner, log *slog.Logger, wake <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		tenants, err := st.TenantIDs(ctx)
		if err == nil {
			for _, tenant := range tenants {
				if ctx.Err() != nil {
					return
				}
				// A run claimed by a previous process is adopted before new work:
				// its named Jobs and workspace are the durable state.
				running, err := st.RunningRuns(ctx, tenant)
				if err == nil {
					for _, q := range running {
						if ctx.Err() != nil {
							return
						}
						if err := r.Resume(ctx, q); err != nil && !errors.Is(err, store.ErrConflict) {
							log.Warn("run resume failed", "tenant", tenant, "run", q.RunID, "error", err)
						}
					}
				}
				queued, err := st.QueuedRuns(ctx, tenant)
				if err != nil || len(queued) == 0 {
					continue
				}
				if err := r.Execute(ctx, queued[0]); err != nil && !errors.Is(err, store.ErrConflict) {
					log.Warn("run execution failed", "tenant", tenant, "run", queued[0].RunID, "error", err)
				}
			}
		}
		timer.Reset(time.Second)
	}
}

func chooseBackend(cfg Config) (runner.Backend, error) {
	workspaceRoot := filepath.Join(cfg.StateDir, "workspaces")
	switch cfg.Executor {
	case "process":
		return runner.Process{Root: workspaceRoot}, nil
	case "podman", "":
		return runner.Podman{Root: workspaceRoot}, nil
	case "kubernetes":
		if cfg.Kube == nil {
			return nil, errors.New("kernel: the kubernetes executor needs its installation configuration")
		}
		return runner.NewKube(*cfg.Kube)
	default:
		return nil, fmt.Errorf("kernel: unknown executor %q", cfg.Executor)
	}
}

func keyProvider(ctx context.Context, cfg Config) (secretkeys.Provider, func(), error) {
	if cfg.KMSFile != "" {
		if cfg.Keyfile != "" {
			return nil, nil, errors.New("kernel: KMSFile and Keyfile are mutually exclusive")
		}
		kms, err := loadKMSConfig(cfg.KMSFile)
		if err != nil {
			return nil, nil, err
		}
		return newKMSProvider(ctx, kms, cfg.InstallationID)
	}
	if cfg.Keyfile != "" {
		k, err := secretkeys.NewKeyfile(cfg.Keyfile, cfg.Env == "production")
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: keyfile: %w", err)
		}
		return k, func() {}, nil
	}
	if cfg.Env == "production" {
		return nil, nil, errors.New("kernel: production requires a KMS/keyfile key provider")
	}
	k, err := secretkeys.NewEphemeral()
	if err != nil {
		return nil, nil, err
	}
	return k, func() {}, nil
}

func authenticator(ctx context.Context, cfg Config) (api.Authenticator, error) {
	if cfg.DevToken != "" {
		if cfg.Env == "production" {
			return nil, errors.New("kernel: a static development token is refused in production")
		}
		return api.StaticToken{Token: cfg.DevToken, UserID: cfg.DevUserID, Name: cfg.DevName}, nil
	}
	if cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" {
		return nil, errors.New("kernel: configure OIDC (issuer and client id) or an explicit development token")
	}
	return api.NewOIDC(ctx, cfg.OIDCIssuer, "", cfg.OIDCClientID)
}

func verifyRuntimeRole(ctx context.Context, pool *storage.Pool) error {
	var super, bypass bool
	err := pool.QueryRow(ctx, `SELECT r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &bypass)
	if err != nil {
		return fmt.Errorf("kernel: inspect runtime role: %w", err)
	}
	if super || bypass {
		return errors.New("kernel: the runtime role must not be a superuser or bypass RLS")
	}
	return nil
}

// buildGit assembles the Git hosting routes and the tenants' workers. Outside
// development without a worker address the in-process adapter is refused: the
// routes are served with workers that fail closed, and the refusal is logged.
func buildGit(cfg Config, st *store.Store, auth api.Authenticator, log *slog.Logger, onEvent func()) (*api.GitServer, *GitWorkers, string, error) {
	signer, err := NewGitSigner(cfg.Env, cfg.GitForwardingKeyFile, cfg.GitIdentityKeyFile)
	if err != nil {
		return nil, nil, "", err
	}
	workers, err := NewGitWorkers(GitWorkersConfig{Env: cfg.Env, InProcess: cfg.GitInProcess, InProcessAck: cfg.GitInProcessAck,
		StateDir: cfg.StateDir, WorkerURL: cfg.GitWorkerURL,
		ControlToken: cfg.GitControlToken, Verifier: signer.Verifier(), Logger: log})
	if err != nil {
		if !errors.Is(err, ErrInProcessInProduction) {
			return nil, nil, "", err
		}
		log.Error("Git hosting is unavailable", "error", err)
	}
	publicURL := cfg.PublicURL
	if publicURL == "" {
		publicURL = "http://localhost" + cfg.Addr
		if !strings.HasPrefix(cfg.Addr, ":") {
			publicURL = "http://" + cfg.Addr
		}
	}
	git, err := api.NewGit(api.GitConfig{Store: st, Auth: auth, Workers: workers, Signer: signer,
		Identities: signer.Verifier(), PublicURL: publicURL, Logger: log, OnEvent: onEvent})
	if err != nil {
		return nil, nil, "", err
	}
	workers.BindControl(git.Control())
	return git, workers, publicURL, nil
}
