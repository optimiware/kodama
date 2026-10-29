package kit

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
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
	gitfx "github.com/optimiware/kodama/test/support/git"
	oidcfx "github.com/optimiware/kodama/test/support/oidc"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Prefix is this task's resource prefix.
const Prefix = "t038"

// TenantA and TenantB are the two bootstrapped tenants. Their identifiers carry
// the prefix and a suffix chosen per test process: workload containers are
// labelled with their tenant, so two test packages running at the same time in
// one scope never see, count or remove each other's containers.
var (
	TenantA = Prefix + "_acme_" + processSuffix
	TenantB = Prefix + "_globex_" + processSuffix
)

var processSuffix = func() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}()

// Stack is one complete installation.
type Stack struct {
	T  testing.TB
	DB *pgtest.Database
	// Owner is the migration (owner) role pool: inspection and fixture damage
	// only. Runtime is the non-owner role the application itself uses.
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	OIDC    *oidcfx.Fixture
	S3      *s3support.Env
	Git     *gitfx.Server
	Repo    *gitfx.Repo

	// PG and NATS sit between the application and its dependencies.
	PG, NATS *eventstest.Proxy

	// Front is the TLS origin workloads call back.
	Front *Front
	// Workloads is the Podman runtime's state directory.
	Workloads string

	natsDirect string
	natsToken  string
	stream     string
	namespace  string
	secrets    string
	entry      string
	podmanNet  string

	tune     func(*app.Tuning)
	signGen  int64
	noDispat bool

	mu     sync.Mutex
	srv    *app.Server
	addr   string
	tokens map[string]string
	logOn  atomic.Bool
}

// StackOptions select what a test needs.
type StackOptions struct {
	// Unmigrated leaves the schema empty (a deployment that skipped `kodama migrate`).
	Unmigrated bool
	// Tune adjusts the operational bounds.
	Tune func(*app.Tuning)
	// NoDispatch keeps the dispatch loop from running (its interval is an hour):
	// tests that never need a container do not start one. A test that does
	// leaves it unset.
	NoDispatch bool
}

// NewStack provisions the isolated dependencies of one installation; Start composes
// and starts the application.
func NewStack(t *testing.T, o StackOptions) *Stack {
	t.Helper()
	s := &Stack{T: t, tokens: map[string]string{}, tune: o.Tune, noDispat: o.NoDispatch}
	s.logOn.Store(true)
	t.Cleanup(func() { s.logOn.Store(false) })
	ctx := context.Background()

	requirePrerequisites(t)
	s.DB = pgtest.New(t)
	s.Owner = s.DB.MigrationPool(ctx, 4)
	s.Runtime = s.DB.RuntimePool(ctx, 8)
	if !o.Unmigrated {
		s.Migrate()
	}

	s.OIDC = oidcfx.Start(t)
	s.S3 = s3support.New(t)
	s.Git = gitfx.NewServerHost(t, CallbackHost())
	s.Repo = s.Git.NewRepo(t, Prefix+"-repo")

	broker := eventstest.NewBroker(t)
	s.natsDirect = broker.Addr()
	if u, err := url.Parse(os.Getenv("KODAMA_NATS_URL")); err == nil && u.User != nil {
		s.natsToken = u.User.Username()
	} else if data, err := os.ReadFile(filepath.Join(os.Getenv("KODAMA_SECRETS_DIR"), "nats-token")); err == nil {
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

	s.secrets = t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(s.secrets, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("master-key", randomKey())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write("runtime-signing-key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	// The artifact and log verification copies are written under the scratch
	// root; the assembly does not create those directories (a finding recorded in
	// docs/acceptance/m1.md), so the installation provides them.
	for _, d := range []string{"artifact-spool", "log-spool"} {
		if err := os.MkdirAll(filepath.Join(s.secrets, "scratch", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// The Podman runtime: the scope's workload network, a shared state
	// directory, the real Linux `kodama` and the TLS front of the runtime API.
	s.podmanNet = "host"
	s.Workloads = podmanSharedDir(t)
	s.entry = EntrypointBinary(t)
	s.removeContainers()
	s.recordContainerLogs()
	t.Cleanup(s.removeContainers)
	s.Front = newFront(t, CallbackHost(), s.dial)
	return s
}

// CallbackHost is the host name the workload containers use to reach the test
// process. They share the Podman engine's network, so it is the loopback address
// where the engine runs on this host and KODAMA_TEST_SERVER_CALLBACK_HOST
// (host.containers.internal for a Podman machine) where it runs in a VM.
func CallbackHost() string {
	if host := os.Getenv("KODAMA_TEST_SERVER_CALLBACK_HOST"); host != "" {
		return host
	}
	return "127.0.0.1"
}

// ScopeEnv returns a required environment variable of the scope or fails: the
// acceptance never passes without the services it names.
func ScopeEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(fmt.Sprintf("%s is not set: the M1 acceptance needs the `test` scope (DEV_STACK=services; make dev-preflight)", name))
	}
	return v
}

// requirePrerequisites fails (never skips) when a tool the acceptance
// exercises is missing.
func requirePrerequisites(t testing.TB) {
	t.Helper()
	for _, name := range []string{"KODAMA_TEST_POSTGRES_ADMIN_URL", "KODAMA_NATS_URL", "KODAMA_S3_ENDPOINT", "KODAMA_REGISTRY_URL"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is not set: run the M1 acceptance in the `test` scope (DEV_STACK=services)", name)
		}
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the M1 acceptance requires git: %v", err)
	}
	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")); err != nil {
		t.Fatalf("the M1 acceptance requires git-http-backend: %v", err)
	}
	requirePodman(t)
}

// Migrate applies the declared migrations as the owner role.
func (s *Stack) Migrate() {
	s.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := app.Migrate(ctx, app.MigrateConfig{MigrationURL: s.DB.MigrationURL, RuntimeURL: s.DB.RuntimeURL})
	if err != nil {
		s.T.Fatalf("migrate: %v", err)
	}
	if len(res.Applied) == 0 {
		s.T.Fatal("migrate applied nothing")
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

// CoreConfig builds the complete configuration the real process would load,
// pointing at the proxies.
func (s *Stack) CoreConfig() app.CoreConfig {
	keyOnce.Do(func() { cursor, hashKey = randomKey(), randomKey() })
	pgURL, _ := url.Parse(s.DB.RuntimeURL)
	pgURL.Host = s.PG.Addr()
	tune := app.DefaultTuning()
	tune.ReconcileInterval = 300 * time.Millisecond
	tune.ReconcileBatch = 20
	tune.ReadinessTTL = 100 * time.Millisecond
	tune.ReadinessTimeout = time.Second
	tune.ObservationPollInterval = 50 * time.Millisecond
	tune.DispatchInterval = 200 * time.Millisecond
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
	if s.noDispat {
		tune.DispatchInterval = time.Hour
	}
	if s.tune != nil {
		s.tune(&tune)
	}
	return app.CoreConfig{
		Process:  app.Config{HTTPAddr: s.listenAddr(), ShutdownTimeout: 10 * time.Second, Assembly: app.AssemblyCore},
		Env:      app.Test,
		Database: app.DatabaseConfig{RuntimeURL: pgURL.String(), MaxConns: 8},
		NATS:     app.NATSConfig{URL: "nats://" + s.natsToken + "@" + s.NATS.Addr(), Stream: s.stream, Namespace: s.namespace},
		S3: app.S3Config{
			Endpoint: s.S3.Config.Endpoint, Region: s3support.Region, PathStyle: true, Bucket: s.S3.Config.Bucket, Prefix: s.S3.Prefix,
			AccessKey: s.S3.Config.AccessKey, SecretKey: s.S3.Config.SecretKey, AllowInsecureHTTP: true,
		},
		OIDC: app.OIDCConfig{Issuer: s.OIDC.Issuer, Audience: s.OIDC.Audience, AllowInsecureHTTP: true},
		Git: app.GitConfig{
			CAFile: s.Git.CAFile, ScratchRoot: filepath.Join(s.secrets, "scratch"), CommandTimeout: s.GitDeadline(),
			MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
			Credentials: []app.GitCredential{
				{Ref: "secret://installation/checkout", Username: gitfx.Username, Password: s.Git.ReadToken, URLPrefix: s.Git.URL + "/", Tenants: []string{TenantA}},
				{Ref: "secret://installation/publisher", Username: gitfx.Username, Password: s.Git.WriteToken, URLPrefix: s.Git.URL + "/", Tenants: []string{TenantA}},
			},
		},
		Secrets: app.SecretsConfig{Adapter: "keyfile", Keyfile: &app.KeyfileConfig{Current: "v1", Files: map[string]string{"v1": filepath.Join(s.secrets, "master-key")}}},
		Runtime: app.RuntimeConfig{
			ServerURL: s.Front.URL, CAFile: s.Front.CAFile, StateDir: s.Workloads, EntrypointBinary: s.entry,
			Podman:         app.PodmanConfig{Network: s.podmanNet},
			SigningKeyFile: filepath.Join(s.secrets, "runtime-signing-key"), SigningGeneration: s.generation(),
		},
		InstallationID: Prefix + "-installation",
		CursorKey:      cursor, IdempotencyHashKey: hashKey,
		Tuning: tune,
	}
}

func (s *Stack) listenAddr() string {
	if s.addr != "" {
		return s.addr
	}
	return "127.0.0.1:0"
}

type testLog struct{ s *Stack }

func (l testLog) Write(p []byte) (int, error) {
	if l.s.logOn.Load() {
		l.s.T.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

// Logger is the server's logger: warnings and above reach the test log.
func (s *Stack) Logger() *slog.Logger {
	level := slog.LevelWarn
	if os.Getenv("KODAMA_M2_LOG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(testLog{s}, &slog.HandlerOptions{Level: level}))
}

// Start composes and starts the application; the address is kept across
// restarts so tokens and cursors stay valid.
func (s *Stack) Start() *app.Server {
	s.T.Helper()
	srv, err := s.TryStart(s.CoreConfig())
	if err != nil {
		s.T.Fatalf("start the core assembly: %v", err)
	}
	return srv
}

// StartReplica starts a second process of the installation over the same state
// (one chart replica more): the protocol must tolerate it, and its address is
// its own. Its shutdown is registered with the test.
func (s *Stack) StartReplica() *app.Server {
	s.T.Helper()
	cfg := s.CoreConfig()
	cfg.Process.HTTPAddr = "127.0.0.1:0"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := app.Start(ctx, cfg, s.Logger())
	if err != nil {
		s.T.Fatalf("start a replica: %v", err)
	}
	s.T.Cleanup(func() { s.stopQuietly(srv) })
	return srv
}

// TryStart starts the application over cfg and reports a refusal.
func (s *Stack) TryStart(cfg app.CoreConfig) (*app.Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := app.Start(ctx, cfg, s.Logger())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.srv, s.addr = srv, srv.Addr()
	s.mu.Unlock()
	s.T.Cleanup(func() { s.stopQuietly(srv) })
	return srv, nil
}

func (s *Stack) stopQuietly(srv *app.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// Stop shuts the running server down and requires a clean drain. Workloads
// already started keep running: the next process adopts them.
func (s *Stack) Stop() {
	s.T.Helper()
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		s.T.Fatalf("shutdown: %v", err)
	}
}

// Restart stops the process and starts a new one over the same state.
func (s *Stack) Restart() *app.Server {
	s.T.Helper()
	s.Stop()
	srv := s.Start()
	s.WaitReady()
	return srv
}

// Server is the running application.
func (s *Stack) Server() *app.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// Base is the public origin of the running process.
func (s *Stack) Base() string { return "http://" + s.addr }

// dial is the address the TLS front forwards to (the running process).
func (s *Stack) dial() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// WaitReady waits for readiness.
func (s *Stack) WaitReady() {
	s.T.Helper()
	WaitFor(s.T, "readiness", 30*time.Second, func() bool {
		resp, err := http.Get(s.Base() + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
}

// GitDeadline is the Git command deadline of the test installation: short
// enough that a parked push is a timeout the test can wait for.
func (s *Stack) GitDeadline() time.Duration { return 4 * time.Second }

// ---- identities ---------------------------------------------------------

// Token returns the persona's access token.
func (s *Stack) Token(persona string) string {
	s.T.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.tokens[persona]; ok {
		return tok
	}
	tok := s.OIDC.Login(persona).Access
	s.tokens[persona] = tok
	return tok
}

// API returns a generated client authenticated as the persona.
func (s *Stack) API(persona string) *client.ClientWithResponses {
	s.T.Helper()
	return s.APIWith(func() string { return s.Token(persona) })
}

// APIWith returns a generated client whose bearer token comes from token.
func (s *Stack) APIWith(token func() string) *client.ClientWithResponses {
	s.T.Helper()
	editor := func(_ context.Context, req *http.Request) error {
		if tok := token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}
	c, err := client.NewClientWithResponses(s.Base()+app.APIPrefix, client.WithRequestEditorFn(editor), client.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	if err != nil {
		s.T.Fatal(err)
	}
	return c
}

// Bootstrap provisions a tenant and its first administrator through the
// operator-only service (no route exists for it).
func (s *Stack) Bootstrap(tenant domain.TenantID, key, persona string) {
	s.T.Helper()
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
		AccessToken: s.Token(persona),
	})
	if err != nil || !res.Created {
		s.T.Fatalf("bootstrap %s: %+v %v", tenant, res, err)
	}
}

// ---- inspection ---------------------------------------------------------

// OwnerCount runs a count as the owner role.
func (s *Stack) OwnerCount(sql string, args ...any) int {
	s.T.Helper()
	var n int
	if err := s.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		s.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// TenantCount counts through the runtime role inside the tenant's context, as
// the application sees data (forced row level security applies).
func (s *Stack) TenantCount(tenant string, sql string, args ...any) int {
	s.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), s.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		s.T.Fatalf("tenant count: %v\n%s", err, sql)
	}
	return n
}

// MustScan scans one owner-role row.
func (s *Stack) MustScan(dst any, sql string, args ...any) {
	s.T.Helper()
	if err := s.Owner.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		s.T.Fatalf("query: %v\n%s", err, sql)
	}
}

// OwnerTenantExec runs a statement as the owner role inside the tenant's
// context (the event tables answer nowhere else) and returns the rows changed.
func (s *Stack) OwnerTenantExec(tenant string, sql string, args ...any) int64 {
	s.T.Helper()
	var n int64
	err := storage.WithTenant(context.Background(), s.Owner, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		s.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
	return n
}

// TenantStrings returns the first column of every row, read through the
// runtime role inside the tenant's context.
func (s *Stack) TenantStrings(tenant string, sql string, args ...any) []string {
	s.T.Helper()
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
		s.T.Fatalf("tenant strings: %v\n%s", err, sql)
	}
	return out
}

// PendingEvents counts the tenant's outbox rows not yet published.
func (s *Stack) PendingEvents(tenant string) int {
	return s.TenantCount(tenant, `SELECT count(*) FROM outbox WHERE state <> 'published'`)
}

// StreamMessages is the number of messages in the JetStream stream.
func (s *Stack) StreamMessages() uint64 {
	s.T.Helper()
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if s.natsToken != "" {
		opts = append(opts, nats.Token(s.natsToken))
	}
	nc, err := nats.Connect("nats://"+s.natsDirect, opts...)
	if err != nil {
		s.T.Fatal(err)
	}
	defer nc.Close()
	js, err := natsjs.New(nc)
	if err != nil {
		s.T.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := js.Stream(ctx, s.stream)
	if err != nil {
		s.T.Fatal(err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		s.T.Fatal(err)
	}
	return info.State.Msgs
}

// WaitFor polls cond until it holds or the deadline passes.
func WaitFor(t testing.TB, what string, within time.Duration, cond func() bool) {
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

// Raw issues one HTTP request against the public origin.
func (s *Stack) Raw(method, path, token string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	s.T.Helper()
	req, err := http.NewRequest(method, s.Base()+path, body)
	if err != nil {
		s.T.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		s.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// JSONBody marshals v as a request body.
func JSONBody(t testing.TB, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// ErrorCode is the stable error code of an error envelope.
func ErrorCode(body []byte) string {
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// RepublishEvents publishes a second copy, under a fresh broker message ID, of
// every message of the stream whose body contains marker: a duplicate delivery
// the broker's own de-duplication cannot suppress, which only the consumer's
// inbox can. It returns the number republished.
func (s *Stack) RepublishEvents(marker string) int {
	s.T.Helper()
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if s.natsToken != "" {
		opts = append(opts, nats.Token(s.natsToken))
	}
	nc, err := nats.Connect("nats://"+s.natsDirect, opts...)
	if err != nil {
		s.T.Fatal(err)
	}
	defer nc.Close()
	js, err := natsjs.New(nc)
	if err != nil {
		s.T.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := js.Stream(ctx, s.stream)
	if err != nil {
		s.T.Fatal(err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		s.T.Fatal(err)
	}
	n := 0
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		m, err := st.GetMsg(ctx, seq)
		if err != nil || !strings.Contains(string(m.Data), marker) {
			continue
		}
		msg := &nats.Msg{Subject: m.Subject, Data: m.Data, Header: nats.Header{}}
		msg.Header.Set(nats.MsgIdHdr, fmt.Sprintf("%s-duplicate-%d", Prefix, seq))
		if _, err := js.PublishMsg(ctx, msg); err != nil {
			s.T.Fatalf("republish: %v", err)
		}
		n++
	}
	return n
}

// ConsumerState is the JetStream consumer's progress: messages it has been
// delivered, still has pending and awaits acknowledgment of.
type ConsumerState struct {
	Delivered  uint64 // stream sequence of the last delivered message
	Pending    uint64
	AckPending int
	Last       uint64 // stream sequence of the newest message
}

// Consumer reads a durable consumer's state.
func (s *Stack) Consumer(durable string) ConsumerState {
	s.T.Helper()
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if s.natsToken != "" {
		opts = append(opts, nats.Token(s.natsToken))
	}
	nc, err := nats.Connect("nats://"+s.natsDirect, opts...)
	if err != nil {
		s.T.Fatal(err)
	}
	defer nc.Close()
	js, err := natsjs.New(nc)
	if err != nil {
		s.T.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := js.Stream(ctx, s.stream)
	if err != nil {
		s.T.Fatal(err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		s.T.Fatal(err)
	}
	c, err := st.Consumer(ctx, durable)
	if err != nil {
		s.T.Fatalf("consumer %s: %v", durable, err)
	}
	ci, err := c.Info(ctx)
	if err != nil {
		s.T.Fatal(err)
	}
	return ConsumerState{Delivered: ci.Delivered.Stream, Pending: ci.NumPending, AckPending: ci.NumAckPending, Last: info.State.LastSeq}
}

func (s *Stack) generation() int64 {
	if s.signGen == 0 {
		return 1
	}
	return s.signGen
}

// RotateSigningGeneration makes the next process sign and verify runtime tokens
// under a new generation of the same key: every token of the old one is refused.
func (s *Stack) RotateSigningGeneration(generation int64) { s.signGen = generation }
