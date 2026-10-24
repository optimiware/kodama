// Package app is the composition root: it constructs the concrete adapters and
// wires them into the use cases. No other package chooses an adapter.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.invalid/inventory/internal/config"
	"example.invalid/inventory/internal/erp"
	"example.invalid/inventory/internal/exceptions"
	exhttp "example.invalid/inventory/internal/exceptions/http"
	expg "example.invalid/inventory/internal/exceptions/postgres"
	"example.invalid/inventory/internal/identity"
	"example.invalid/inventory/internal/tenancy"
	tpg "example.invalid/inventory/internal/tenancy/postgres"
	"example.invalid/inventory/migrations"
)

func openPool(ctx context.Context, d config.Database) (*pgxpool.Pool, error) {
	url, err := config.ReadSecret(d.URLFile)
	if err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("database URL is not a valid PostgreSQL URI")
	}
	pc.MaxConns = d.MaxConns
	pc.ConnConfig.RuntimeParams["application_name"] = "inventory"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	return pool, nil
}

// API is the HTTP server with its resources.
type API struct {
	Server *http.Server
	ln     net.Listener
	pool   *pgxpool.Pool
	log    *slog.Logger
	grace  time.Duration
}

// NewAPI builds the API process from validated configuration. A missing
// application dependency (database, secrets) fails here; an unreachable
// identity provider is reported by readiness while requests fail closed.
func NewAPI(ctx context.Context, cfg config.API, log *slog.Logger) (*API, error) {
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*API, error) { pool.Close(); return nil, err }
	key, err := config.ReadSecret(cfg.CursorKeyFile)
	if err != nil {
		return fail(err)
	}
	if len(key) < 32 {
		return fail(errors.New("the cursor key must be at least 32 bytes"))
	}
	ver, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: cfg.OIDC.Issuer, Audience: cfg.OIDC.Audience, AllowInsecureIssuer: cfg.OIDC.AllowInsecureIssuer})
	if err != nil {
		return fail(err)
	}
	store, err := expg.NewStore(pool)
	if err != nil {
		return fail(err)
	}
	svc, err := exceptions.NewService(exceptions.Config{Store: store}) // the API holds no ERP adapter or credential
	if err != nil {
		return fail(err)
	}
	mem, err := tpg.NewMemberships(pool)
	if err != nil {
		return fail(err)
	}
	resolver, err := tenancy.NewResolver(mem)
	if err != nil {
		return fail(err)
	}
	latest, err := migrations.Latest()
	if err != nil {
		return fail(err)
	}
	ready := func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		applied, err := migrations.Current(ctx, pool)
		if err != nil || len(applied) == 0 || applied[len(applied)-1] != latest {
			return fmt.Errorf("database schema is not at version %s", latest)
		}
		return ver.Ready(ctx)
	}
	h, err := exhttp.New(exhttp.Config{Auth: ver, Tenants: resolver, Service: svc, CursorKey: []byte(key), Ready: ready, Logger: log, Build: buildInfo()})
	if err != nil {
		return fail(err)
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fail(fmt.Errorf("listen: %w", err))
	}
	return &API{
		ln:     ln,
		Server: &http.Server{Addr: cfg.HTTPAddr, Handler: h.Routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second},
		pool:   pool, log: log, grace: cfg.ShutdownTimeout.Std(),
	}, nil
}

// Addr is the bound listen address (useful with port 0).
func (a *API) Addr() string { return a.ln.Addr().String() }

// Run serves until ctx ends, then drains requests.
func (a *API) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- a.Server.Serve(a.ln) }()
	select {
	case err := <-errc:
		a.pool.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), a.grace)
	defer cancel()
	err := a.Server.Shutdown(sctx)
	a.pool.Close()
	return err
}

// Worker executes ERP intents for exactly one application tenant under its own
// service identity. It uses the same runtime role and row-level security as the
// API: it neither bypasses isolation nor impersonates a human user.
type Worker struct {
	svc      *exceptions.Service
	resolver *tenancy.Resolver
	pool     *pgxpool.Pool
	tenant   string
	service  string
	poll     time.Duration
	log      *slog.Logger
}

// NewWorker builds the worker process.
func NewWorker(ctx context.Context, cfg config.Worker, log *slog.Logger) (*Worker, error) {
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Worker, error) { pool.Close(); return nil, err }
	files := map[tenancy.TenantID]string{}
	for t, f := range cfg.ERP.CredentialFiles {
		files[tenancy.TenantID(t)] = f
	}
	creds, err := erp.NewFileCredentials(files, os.ReadFile)
	if err != nil {
		return fail(err)
	}
	if _, err := creds.Token(tenancy.TenantID(cfg.Tenant)); err != nil {
		return fail(fmt.Errorf("the worker's ERP credential is unusable: %w", err))
	}
	adapter, err := erp.NewHTTPAdapter(erp.Config{BaseURL: cfg.ERP.BaseURL, Credentials: creds, Timeout: time.Duration(cfg.ERP.TimeoutSeconds) * time.Second, AllowInsecure: cfg.ERP.AllowInsecure})
	if err != nil {
		return fail(err)
	}
	store, err := expg.NewStore(pool)
	if err != nil {
		return fail(err)
	}
	svc, err := exceptions.NewService(exceptions.Config{Store: store, ERP: adapter, Lease: cfg.Lease.Std(), MaxAttempts: cfg.MaxAttempts, RetryBase: cfg.RetryBase.Std(), RetryMax: cfg.RetryMax.Std()})
	if err != nil {
		return fail(err)
	}
	mem, err := tpg.NewMemberships(pool)
	if err != nil {
		return fail(err)
	}
	resolver, err := tenancy.NewResolver(mem)
	if err != nil {
		return fail(err)
	}
	w := &Worker{svc: svc, resolver: resolver, pool: pool, tenant: cfg.Tenant, service: cfg.ServiceID, poll: cfg.PollInterval.Std(), log: log}
	if _, err := resolver.ResolveWorker(ctx, w.service, w.tenant); err != nil {
		pool.Close()
		return nil, fmt.Errorf("the service identity %q holds no worker membership in tenant %s: %w", w.service, w.tenant, err)
	}
	return w, nil
}

// AwaitWorker builds the worker, waiting while the environment is not yet
// provisioned. A fresh environment is migrated by the delivery step and
// provisioned afterwards (tenants and memberships are owner-identity operations
// that no runtime process may perform), so a worker started before that either
// has no membership yet or cannot read the schema yet. Both are retried every
// retry interval, logged, for at most wait; any other failure, such as a bad
// configuration or credential, is returned immediately. NewWorker itself stays
// strict: a service identity without a current worker membership never runs.
func AwaitWorker(ctx context.Context, cfg config.Worker, log *slog.Logger, wait, retry time.Duration) (*Worker, error) {
	deadline := time.Now().Add(wait)
	for {
		w, err := NewWorker(ctx, cfg, log)
		if err == nil {
			return w, nil
		}
		if (!errors.Is(err, tenancy.ErrNotMember) && !errors.Is(err, tenancy.ErrUnavailable)) || !time.Now().Add(retry).Before(deadline) {
			return nil, err
		}
		log.Warn("worker waiting for provisioning", "tenant", cfg.Tenant, "service", cfg.ServiceID, "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

// Run processes intents until ctx ends. Membership is re-checked on every
// cycle, so revoking the service identity stops its work.
func (w *Worker) Run(ctx context.Context) error {
	defer w.pool.Close()
	owner := fmt.Sprintf("%s/%d", w.service, os.Getpid())
	for ctx.Err() == nil {
		access, err := w.resolver.ResolveWorker(ctx, w.service, w.tenant)
		busy := false
		if err == nil {
			busy, err = w.svc.Process(ctx, access, owner)
		}
		if err != nil && ctx.Err() == nil {
			w.log.Error("worker cycle failed", "tenant", w.tenant, "error", err)
		}
		if busy && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.poll):
		}
	}
	return nil
}

// Migrate applies the embedded migrations as the owner identity.
func Migrate(ctx context.Context, cfg config.Migrate, log *slog.Logger) error {
	url, err := config.ReadSecret(cfg.Database.URLFile)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return errors.New("migration database unreachable")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	done, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: cfg.RuntimeRole})
	for _, v := range done {
		log.Info("migration applied", "version", v)
	}
	if err == nil && len(done) == 0 {
		log.Info("database is up to date")
	}
	return err
}

// SeedFile is explicit test/preview provisioning data.
type SeedFile struct {
	TenantID string `json:"tenantId"`
	Name     string `json:"name"`
	Members  []struct {
		Subject string `json:"subject"`
		Role    string `json:"role"`
	} `json:"members"`
}

// Seed provisions a tenant and memberships with the owner identity. It is the
// only way preview data is created; runtime processes cannot do this.
func Seed(ctx context.Context, cfg config.Migrate, s SeedFile) error {
	url, err := config.ReadSecret(cfg.Database.URLFile)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return errors.New("seed database unreachable")
	}
	defer pool.Close()
	prov, err := tpg.NewProvisioner(pool)
	if err != nil {
		return err
	}
	tenant, err := tenancy.ParseTenantID(s.TenantID)
	if err != nil {
		return err
	}
	var members []tpg.Member
	for _, m := range s.Members {
		members = append(members, tpg.Member{Subject: m.Subject, Role: tenancy.Role(m.Role)})
	}
	return prov.Seed(ctx, tenant, s.Name, members)
}

// buildInfo reads the nonsecret identity the delivery step injects into the pods.
func buildInfo() map[string]string {
	out := map[string]string{}
	for k, env := range map[string]string{"deploymentId": "INVENTORY_DEPLOYMENT_ID", "configurationDigest": "INVENTORY_CONFIGURATION_DIGEST",
		"imageDigest": "INVENTORY_IMAGE_DIGEST", "sourceCommit": "INVENTORY_SOURCE_COMMIT"} {
		out[k] = os.Getenv(env)
	}
	return out
}
