// Package assembly_test is the independent evidence of T-070: the actual core
// composition (internal/app) started on a real listener over real PostgreSQL
// (declared migrations, non-owner runtime role), real OIDC tokens from an
// isolated oauthsonas issuer, a real HTTPS Git server, real S3 and a real
// JetStream stream, driven only through the generated public client (and, for
// inspection, the owner and runtime database roles). Nothing in the composition
// is replaced by a double: the only test-owned pieces are TCP proxies that sever
// and restore the transport to PostgreSQL, NATS and S3 so dependency outages
// are real.
//
// Resource prefix t070: tenants, projects, work items, the S3 subprefix, the
// stream and namespace and every fixture identifier carry it.
package assembly_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
	gitfx "github.com/optimiware/kodama/test/support/git"
	oidcfx "github.com/optimiware/kodama/test/support/oidc"
	podmantest "github.com/optimiware/kodama/test/support/podman"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

const (
	Prefix = "t070"
	// TenantA and TenantB are the two bootstrapped tenants.
	TenantA = "t070_acme"
	TenantB = "t070_globex"
)

// stack is one complete installation.
type stack struct {
	t  testing.TB
	DB *pgtest.Database
	// Owner is the migration (owner) role pool: inspection and fixture damage
	// only. Runtime is the non-owner role the application itself uses.
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	OIDC    *oidcfx.Fixture
	S3      *s3support.Env
	Git     *gitfx.Server
	Repo    *gitfx.Repo

	// Proxies sit between the application and its dependencies.
	PG, NATS, S3Proxy *eventstest.Proxy

	natsDirect string // host:port of the broker
	natsToken  string
	stream     string
	namespace  string
	secrets    string
	// entry, workloads and podmanNet are the Podman runtime of the `test`
	// environment: the real entrypoint binary, the shared state directory and
	// the scope's workload network. Most of this suite starts no container
	// (dispatch is idle, see coreConfig); core_assembly_current_runtime runs
	// real steps.
	entry     string
	workloads string
	podmanNet string

	tune func(*app.Tuning)

	mu     sync.Mutex
	srv    *app.Server
	addr   string
	tokens map[string]string
	logOn  atomic.Bool
}

// options select what a test needs.
type options struct {
	// Unmigrated leaves the schema empty (a deployment that skipped `kodama migrate`).
	Unmigrated bool
	// Tune adjusts the operational bounds.
	Tune func(*app.Tuning)
}

func newStack(t *testing.T, o options) *stack {
	t.Helper()
	s := &stack{t: t, tokens: map[string]string{}, tune: o.Tune}
	s.logOn.Store(true)
	t.Cleanup(func() { s.logOn.Store(false) })
	ctx := context.Background()

	s.DB = pgtest.New(t)
	s.Owner = s.DB.MigrationPool(ctx, 4)
	s.Runtime = s.DB.RuntimePool(ctx, 8)
	if !o.Unmigrated {
		s.migrate()
	}

	s.OIDC = oidcfx.Start(t)
	s.S3 = s3support.New(t)
	// gitfx.NewServer skips when Git is missing; the acceptance must never pass
	// without exercising it, so the requirement is checked first and fails.
	requireGit(t)
	s.Git = gitfx.NewServer(t)
	s.Repo = s.Git.NewRepo(t, "t070-repo")

	broker := eventstest.NewBroker(t)
	s.natsDirect = broker.Addr()
	if u, err := url.Parse(os.Getenv("KODAMA_NATS_URL")); err == nil && u.User != nil {
		s.natsToken = u.User.Username()
	} else if data, err := os.ReadFile("/secrets/nats-token"); err == nil {
		s.natsToken = strings.TrimSpace(string(data))
	}
	sc := broker.StreamConfig(t, time.Minute) // names carry the prefix and a random suffix; deleted on cleanup
	s.stream, s.namespace = sc.Stream, sc.Namespace

	pgURL, err := url.Parse(s.DB.RuntimeURL)
	if err != nil {
		t.Fatal(err)
	}
	s.PG = eventstest.NewProxy(t, pgURL.Host)
	s.NATS = eventstest.NewProxy(t, s.natsDirect)
	s3URL, err := url.Parse(s.S3.Config.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	s.S3Proxy = eventstest.NewProxy(t, s3URL.Host)

	s.secrets = t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(s.secrets, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	write("master-key", master)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write("runtime-signing-key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	// The `test` environment composes the Podman runtime (ADR 0009): the
	// scope's workload network, a state directory the engine can mount and the
	// real Linux entrypoint. The assembly creates its own spool directories;
	// nothing here prepares them (core_spool_directories).
	podmanTool(t)
	s.podmanNet = kit.Env("KODAMA_TEST_PODMAN_NETWORK")
	s.workloads = podmantest.SharedDir(t)
	s.entry = kit.EntrypointBinary(t)
	return s
}

func (s *stack) migrate() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := app.Migrate(ctx, app.MigrateConfig{MigrationURL: s.DB.MigrationURL, RuntimeURL: s.DB.RuntimeURL})
	if err != nil {
		s.t.Fatalf("migrate: %v", err)
	}
	if len(res.Applied) == 0 {
		s.t.Fatal("migrate applied nothing")
	}
}

func randomKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

var (
	keyOnce sync.Once
	cursor  []byte
	hashKey []byte
)

// coreConfig builds the complete configuration the real process would load,
// pointing at the proxies.
func (s *stack) coreConfig() app.CoreConfig {
	keyOnce.Do(func() { cursor, hashKey = randomKey(), randomKey() })
	pgURL, _ := url.Parse(s.DB.RuntimeURL)
	pgURL.Host = s.PG.Addr()
	tune := app.DefaultTuning()
	tune.ReconcileInterval = 300 * time.Millisecond
	tune.ReconcileBatch = 20
	tune.ReadinessTTL = 100 * time.Millisecond
	tune.ReadinessTimeout = time.Second
	tune.ObservationPollInterval = 50 * time.Millisecond
	// The dispatcher is composed and runs, but its interval outlasts every
	// test here: the coverage of this suite (migration, readiness,
	// authorization, replay, CLI) never needs a container, and a run created by
	// one of its tests stays waiting. Real dispatch, containers and the runtime
	// API are proven by core_assembly_current_runtime.
	tune.DispatchInterval = time.Hour
	tune.Events.PublishEvery = 150 * time.Millisecond
	tune.Events.PublishTimeout = 2 * time.Second
	tune.Events.PublishLease = 5 * time.Second
	tune.Events.FetchWait = 300 * time.Millisecond
	tune.Events.RetryDelay = 300 * time.Millisecond
	tune.Events.AckWait = 5 * time.Second
	tune.Events.RepairGrace = time.Second
	tune.Events.Retry = events.RetryPolicy{Initial: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 30}
	tune.Events.StreamDedup = time.Minute
	tune.Events.StreamMaxAge = 24 * time.Hour
	if s.tune != nil {
		s.tune(&tune)
	}
	return app.CoreConfig{
		Process:  app.Config{HTTPAddr: s.listenAddr(), ShutdownTimeout: 10 * time.Second, Assembly: app.AssemblyCore},
		Env:      app.Test,
		Database: app.DatabaseConfig{RuntimeURL: pgURL.String(), MaxConns: 8},
		NATS:     app.NATSConfig{URL: "nats://" + s.natsToken + "@" + s.NATS.Addr(), Stream: s.stream, Namespace: s.namespace},
		S3: app.S3Config{
			Endpoint: "http://" + s.S3Proxy.Addr(), Region: s3support.Region, PathStyle: true, Bucket: s.S3.Config.Bucket, Prefix: s.S3.Prefix,
			AccessKey: s.S3.Config.AccessKey, SecretKey: s.S3.Config.SecretKey, AllowInsecureHTTP: true,
		},
		OIDC: app.OIDCConfig{Issuer: s.OIDC.Issuer, Audience: s.OIDC.Audience, AllowInsecureHTTP: true},
		Git: app.GitConfig{
			CAFile: s.Git.CAFile, ScratchRoot: filepath.Join(s.secrets, "scratch"), CommandTimeout: s.gitDeadline(),
			MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
			Credentials: []app.GitCredential{
				{Ref: "secret://installation/checkout", Username: gitfx.Username, Password: s.Git.ReadToken, URLPrefix: s.Git.URL + "/", Tenants: []string{TenantA}},
				{Ref: "secret://installation/publisher", Username: gitfx.Username, Password: s.Git.WriteToken, URLPrefix: s.Git.URL + "/", Tenants: []string{TenantA}},
			},
		},
		Secrets: app.SecretsConfig{Adapter: "keyfile", Keyfile: &app.KeyfileConfig{Current: "v1", Files: map[string]string{"v1": filepath.Join(s.secrets, "master-key")}}},
		Runtime: app.RuntimeConfig{
			// Nothing here calls the origin back: no container starts.
			ServerURL: "https://host.containers.internal:8443", StateDir: s.workloads, EntrypointBinary: s.entry,
			Podman:         app.PodmanConfig{Network: s.podmanNet},
			SigningKeyFile: filepath.Join(s.secrets, "runtime-signing-key"), SigningGeneration: 1,
		},
		InstallationID: Prefix + "-installation",
		CursorKey:      cursor, IdempotencyHashKey: hashKey,
		Tuning: tune,
	}
}

func (s *stack) listenAddr() string {
	if s.addr != "" {
		return s.addr
	}
	return "127.0.0.1:0"
}

type testLog struct{ s *stack }

func (l testLog) Write(p []byte) (int, error) {
	if l.s.logOn.Load() {
		l.s.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func (s *stack) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testLog{s}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// start composes and starts the application; the address is kept across
// restarts so tokens and cursors stay valid.
func (s *stack) start() *app.Server {
	s.t.Helper()
	srv, err := s.tryStart(s.coreConfig())
	if err != nil {
		s.t.Fatalf("start the core assembly: %v", err)
	}
	return srv
}

func (s *stack) tryStart(cfg app.CoreConfig) (*app.Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := app.Start(ctx, cfg, s.logger())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.srv, s.addr = srv, srv.Addr()
	s.mu.Unlock()
	s.t.Cleanup(func() { s.stopQuietly(srv) })
	return srv, nil
}

func (s *stack) stopQuietly(srv *app.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// stop shuts the running server down and requires a clean drain.
func (s *stack) stop() {
	s.t.Helper()
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		s.t.Fatalf("shutdown: %v", err)
	}
}

func (s *stack) base() string { return "http://" + s.addr }

// ---- identities ---------------------------------------------------------

func (s *stack) token(persona string) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.tokens[persona]; ok {
		return tok
	}
	tok := s.OIDC.Login(persona).Access
	s.tokens[persona] = tok
	return tok
}

// api returns a generated client authenticated as the persona.
func (s *stack) api(persona string) *client.ClientWithResponses {
	s.t.Helper()
	return s.apiWith(func() string { return s.token(persona) })
}

func (s *stack) apiWith(token func() string) *client.ClientWithResponses {
	s.t.Helper()
	editor := func(_ context.Context, req *http.Request) error {
		if tok := token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}
	c, err := client.NewClientWithResponses(s.base()+app.APIPrefix, client.WithRequestEditorFn(editor), client.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// bootstrap provisions a tenant and its first administrator through the
// operator-only service (no route exists for it).
func (s *stack) bootstrap(tenant domain.TenantID, key, persona string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := app.Bootstrap(ctx, app.BootstrapRequest{
		Env:              app.Test,
		AdminDatabaseURL: s.DB.MigrationURL,
		OIDC:             app.OIDCConfig{Issuer: s.OIDC.Issuer, Audience: s.OIDC.Audience, AllowInsecureHTTP: true},
		Config: identity.BootstrapConfig{
			Profile: identity.ProfileStandalone, Eligible: identity.Principal{Issuer: s.OIDC.Issuer, Subject: oidcfx.Subject(persona)},
			OperatorID: "operator_" + Prefix, TenantID: tenant, TenantKey: key, DisplayName: key,
		},
		AccessToken: s.token(persona),
	})
	if err != nil || !res.Created {
		s.t.Fatalf("bootstrap %s: %+v %v", tenant, res, err)
	}
}

// ---- inspection ---------------------------------------------------------

func (s *stack) ownerCount(sql string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		s.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// tenantCount counts through the runtime role inside the tenant's context, as
// the application sees data (forced row level security applies).
func (s *stack) tenantCount(tenant string, sql string, args ...any) int {
	s.t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), s.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		s.t.Fatalf("tenant count: %v\n%s", err, sql)
	}
	return n
}

func (s *stack) streamMessages() uint64 {
	s.t.Helper()
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if s.natsToken != "" {
		opts = append(opts, nats.Token(s.natsToken))
	}
	nc, err := nats.Connect("nats://"+s.natsDirect, opts...)
	if err != nil {
		s.t.Fatal(err)
	}
	defer nc.Close()
	js, err := natsjs.New(nc)
	if err != nil {
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := js.Stream(ctx, s.stream)
	if err != nil {
		s.t.Fatal(err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return info.State.Msgs
}

func waitFor(t testing.TB, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- raw requests ---------------------------------------------------------

func (s *stack) raw(method, path, token string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base()+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func jsonBody(t testing.TB, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func b64(s string) *[]byte { b := []byte(s); return &b }

var _ = base64.StdEncoding
var _ = fmt.Sprint

func (s *stack) mustScan(dst any, sql string, args ...any) {
	s.t.Helper()
	if err := s.Owner.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		s.t.Fatalf("query: %v\n%s", err, sql)
	}
}

// ownerTenantExec runs a statement as the owner role inside the tenant's
// context (the event tables answer nowhere else) and returns the rows changed.
func (s *stack) ownerTenantExec(tenant string, sql string, args ...any) int64 {
	s.t.Helper()
	var n int64
	err := storage.WithTenant(context.Background(), s.Owner, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		s.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
	return n
}

// tenantStrings returns the first column of every row, read through the runtime
// role inside the tenant's context.
func (s *stack) tenantStrings(tenant string, sql string, args ...any) []string {
	s.t.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), s.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		s.t.Fatalf("tenant strings: %v\n%s", err, sql)
	}
	return out
}

// gitDeadline is the Git command deadline of the test installation: short
// enough that a parked push is a timeout the test can wait for.
func (s *stack) gitDeadline() time.Duration { return 4 * time.Second }

// gitTools abstracts the host lookups of the Git preflight so a missing tool can
// be simulated without touching the machine.
type gitTools struct {
	lookPath func(string) (string, error)
	execPath func(gitBin string) (string, error)
	stat     func(string) (os.FileInfo, error)
}

var hostGitTools = gitTools{
	lookPath: exec.LookPath,
	execPath: func(gitBin string) (string, error) {
		out, err := exec.Command(gitBin, "--exec-path").Output()
		return strings.TrimSpace(string(out)), err
	},
	stat: os.Stat,
}

// preflight returns why the assembly acceptance cannot exercise real Git, or nil.
func (g gitTools) preflight() error {
	gitBin, err := g.lookPath("git")
	if err != nil {
		return fmt.Errorf("git is not installed: %w", err)
	}
	dir, err := g.execPath(gitBin)
	if err != nil {
		return fmt.Errorf("git --exec-path: %w", err)
	}
	backend := filepath.Join(dir, "git-http-backend")
	if _, err := g.stat(backend); err != nil {
		return fmt.Errorf("git-http-backend is not available at %s: %w", backend, err)
	}
	return nil
}

// requireGit fails (never skips) the test when real Git is unavailable: the
// required assembly acceptance covers workspaces and retained refs on a real
// remote.
func requireGit(t testing.TB) {
	t.Helper()
	if err := hostGitTools.preflight(); err != nil {
		t.Fatalf("the assembly acceptance requires Git over HTTP: %v", err)
	}
}

// podmanTool fails (never skips) when the Podman engine of the `test`
// environment is missing: the assembly composes its runtime.
func podmanTool(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("the assembly acceptance requires the Podman runtime of the test environment: %v", err)
	}
	if os.Getenv("KODAMA_TEST_PODMAN_NETWORK") == "" {
		t.Fatal("KODAMA_TEST_PODMAN_NETWORK is not set: run in the `test` scope (DEV_STACK=services)")
	}
}
