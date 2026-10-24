// Package mcptest is the shared fixture of the T-033 tests: one isolated,
// migrated PostgreSQL database with the real runtime-authority stack (T-019's
// rtest), the real secrets store, service and development key provider, the
// proxy's PostgreSQL store, bridges and SDK adapters, a local TLS MCP server
// (test/support/mcp) and a workload-facing HTTP server. It is imported only by
// tests. The controlled execution fixture inside rtest proves this module's
// behavior, not the execution module's; the pinned-profile policy is supplied by
// a settable fake of the AttemptPolicy port (the real reader has its own tests
// in internal/agents). Resource identifiers carry the task prefix t033.
package mcptest

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/adapters"
	mcppg "github.com/optimiware/kodama/internal/mcpproxy/postgres"
	"github.com/optimiware/kodama/internal/mcpproxy/protocol"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
)

// Prefix is this task's resource prefix.
const (
	Prefix       = "t033"
	Installation = "inst-t033"
	Admin        = "u033_admin"
)

// Config tunes a stack. Zero values give a working default.
type Config struct {
	Fixture mcpfixture.Options
	// Tune adjusts the proxy options before the service is built.
	Tune func(*mcpproxy.Options)
	// StrictPrivate disallows private-network destinations (the fixture then
	// cannot be reached; destination tests use it).
	StrictPrivate bool
	// Recorder wires a recording sink; RecordRaw enables raw recording.
	Recorder  bool
	RecordRaw bool
	// Incarnation names this stack's process (default "inc-t033-a").
	Incarnation string
	// Conns sizes the runtime pool.
	Conns int32
}

// Policies is a settable stand-in for the pinned-profile policy reader.
type Policies struct {
	mu sync.Mutex
	m  map[domain.AttemptID]mcpproxy.ToolPolicy
}

// Set pins the policy of an attempt.
func (p *Policies) Set(a domain.AttemptID, tools []string, max *int64, record bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[domain.AttemptID]mcpproxy.ToolPolicy{}
	}
	p.m[a] = mcpproxy.NewToolPolicy(tools, max, record)
}

// PolicyOf implements mcpproxy.AttemptPolicy.
func (p *Policies) PolicyOf(_ context.Context, c mcpproxy.Caller) (mcpproxy.ToolPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pol, ok := p.m[c.AttemptID]
	if !ok {
		return mcpproxy.ToolPolicy{}, mcpproxy.ErrNotFound
	}
	return pol, nil
}

// Humans is a settable stand-in for the identity authority.
type Humans struct {
	mu     sync.Mutex
	grants map[string]bool
}

// Grant lets a principal use a permission in a scope.
func (h *Humans) Grant(principal string, scope domain.Scope, perms ...mcpproxy.Permission) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants == nil {
		h.grants = map[string]bool{}
	}
	for _, p := range perms {
		h.grants[principal+"|"+scope.String()+"|"+string(p)] = true
	}
}

// Authorize implements mcpproxy.HumanAuthority.
func (h *Humans) Authorize(_ context.Context, principal string, scope domain.Scope, perm mcpproxy.Permission) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[principal+"|"+scope.String()+"|"+string(perm)] {
		return nil
	}
	return mcpproxy.ErrNotFound
}

// Recorder is a recording sink that keeps what it receives.
type Recorder struct {
	mu   sync.Mutex
	Got  []mcpproxy.RecordedCall
	Fail bool
}

// Record implements mcpproxy.ContentRecorder.
func (r *Recorder) Record(_ context.Context, c mcpproxy.RecordedCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail {
		return context.DeadlineExceeded
	}
	r.Got = append(r.Got, c)
	return nil
}

// Calls returns a copy of the recorded calls.
func (r *Recorder) Calls() []mcpproxy.RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mcpproxy.RecordedCall(nil), r.Got...)
}

// Faults are switchable dependency failures of the stack. They never allow
// anything: they only make a dependency answer with an outage.
type Faults struct {
	mu      sync.Mutex
	recheck error
}

// FailRecheck makes the in-transaction runtime-authority recheck of the proxy
// store fail with err (an authority dependency outage); nil restores it.
func (f *Faults) FailRecheck(err error) { f.mu.Lock(); f.recheck = err; f.mu.Unlock() }

func (f *Faults) recheckErr() error { f.mu.Lock(); defer f.mu.Unlock(); return f.recheck }

// Stack is one fully wired environment.
type Stack struct {
	T       testing.TB
	RT      *rtest.Stack
	Fixture *mcpfixture.Server

	SecretsStore *secretspg.Store
	SecretsSvc   *secrets.Service
	Wrapper      *Wrapper
	Store        *mcppg.Store
	Auth         *adapters.RuntimeAuthority
	Policy       *Policies
	Human        *Humans
	Recorder     *Recorder
	Faults       *Faults
	Dest         *protocol.Destinations
	Remote       *protocol.Remote
	Service      *mcpproxy.Service
	Handler      http.Handler
	Server       *httptest.Server
	Opts         mcpproxy.Options
	Cfg          Config

	baseCtx runtimev1.Context
	n       int
}

// Wrapper is the development key provider with a switchable failure.
type Wrapper struct {
	mu     sync.Mutex
	inner  secrets.KeyWrapper
	unwrap error
}

func (w *Wrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	return w.inner.Wrap(ctx, wc, key)
}

func (w *Wrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, k secrets.WrappedKey) ([]byte, error) {
	w.mu.Lock()
	err := w.unwrap
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return w.inner.Unwrap(ctx, wc, k)
}

// FailUnwrap makes key unwrapping fail (a key provider outage).
func (w *Wrapper) FailUnwrap(err error) { w.mu.Lock(); w.unwrap = err; w.mu.Unlock() }

// DefaultOptions are the proxy options of the fixtures.
func DefaultOptions(incarnation string) mcpproxy.Options {
	return mcpproxy.Options{
		Protocol: protocol.ProtocolRevision, Incarnation: incarnation, SessionTTL: time.Hour, CallTimeout: 20 * time.Second,
		RecheckInterval: 200 * time.Millisecond, StaleAfter: time.Minute, MaxArgumentBytes: 64 << 10, MaxResultBytes: 1 << 20, MaxDiscoveryPages: 10,
	}
}

func toolInvokeActions() runtimeauth.ActionSet {
	as, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionSecretResolve,
		runtimeauth.ActionArtifactReserve, runtimeauth.ActionArtifactComplete, runtimeauth.ActionSourcePropose, runtimeauth.ActionResultSubmit,
		runtimeauth.ActionToolInvoke)
	if err != nil {
		panic(err)
	}
	return as
}

// New builds a stack. Everything is released when the test ends.
func New(t testing.TB, cfg Config) *Stack {
	t.Helper()
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		t.Setenv("KODAMA_RESOURCE_PREFIX", Prefix)
	}
	if cfg.Incarnation == "" {
		cfg.Incarnation = "inc-t033-a"
	}
	rt := rtest.New(t, rtest.Options{Conns: cfg.Conns})
	s := &Stack{T: t, RT: rt, Cfg: cfg, Policy: &Policies{}, Human: &Humans{}, Recorder: &Recorder{}, Faults: &Faults{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manifest, err := storage.NewManifest(storage.BaseContribution(), rtpg.Migrations(), secretspg.Migrations(), mcppg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, rt.Owner, manifest, storage.MigrateOptions{RuntimeRole: rt.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Secrets: the real store and service over a development key provider.
	key := make([]byte, keyfile.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "master-key-v1")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	prov, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": keyPath}})
	if err != nil {
		t.Fatal(err)
	}
	s.Wrapper = &Wrapper{inner: prov}
	noEvents := secretspg.EventAppender(func(context.Context, pgx.Tx, eventsv1.Envelope) error { return nil })
	if s.SecretsStore, err = secretspg.NewStore(secretspg.Config{DB: rt.Runtime, Events: noEvents, IDs: secrets.RandomIDs{}}); err != nil {
		t.Fatal(err)
	}
	if s.SecretsSvc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: Installation, Store: s.SecretsStore, Wrapper: s.Wrapper, IDs: secrets.RandomIDs{}}); err != nil {
		t.Fatal(err)
	}
	versions, err := secretspg.NewVersionSource(s.SecretsStore, s.Wrapper, Installation)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy.
	s.Auth, err = adapters.NewRuntimeAuthority(rt.Service, rt.Authority, rt.Signer.Generation)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := adapters.Pins(s.SecretsStore)
	if err != nil {
		t.Fatal(err)
	}
	if s.Store, err = mcppg.NewStore(rt.Runtime, mcppg.Options{Recheck: s.recheck, Pin: pins}); err != nil {
		t.Fatal(err)
	}
	creds, err := adapters.NewCredentials(versions)
	if err != nil {
		t.Fatal(err)
	}
	s.Fixture = mcpfixture.New(t, cfg.Fixture)
	s.Dest, err = protocol.NewDestinations(protocol.DestinationConfig{Allowed: []string{s.Fixture.HostPort()}, AllowPrivate: !cfg.StrictPrivate})
	if err != nil {
		t.Fatal(err)
	}
	s.Remote, err = protocol.NewRemote(protocol.RemoteConfig{Destinations: s.Dest, RootCAs: s.Fixture.Roots(), ConnectTimeout: 5 * time.Second, MaxResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s.Opts = DefaultOptions(cfg.Incarnation)
	s.Opts.RecordRaw = cfg.RecordRaw
	if cfg.Tune != nil {
		cfg.Tune(&s.Opts)
	}
	scfg := mcpproxy.Config{Store: s.Store, Human: s.Human, Runtime: s.Auth, Policy: s.Policy, Credentials: creds, Remote: s.Remote,
		Destinations: s.Dest, Secrets: s.Store, Options: s.Opts, Clock: rt.Clock.Now}
	if cfg.Recorder {
		scfg.Recorder = s.Recorder
	}
	if s.Service, err = mcpproxy.NewService(scfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Service.Shutdown)
	s.Handler, err = protocol.NewHandler(protocol.ServerConfig{Runtime: s.Service, IdleTimeout: time.Hour, MaxRequestBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s.Server = httptest.NewServer(s.Handler)
	t.Cleanup(s.Server.Close)
	s.baseCtx, _ = rt.HappyContext()
	return s
}

// recheck is the runtime-authority recheck of the proxy stores, with the
// switchable outage in front of it.
func (s *Stack) recheck(ctx context.Context, tx pgx.Tx, c mcpproxy.Caller, now time.Time) error {
	if err := s.Faults.recheckErr(); err != nil {
		return err
	}
	return s.Auth.Recheck(ctx, tx, c, now)
}

// Restart builds a second service over the same database, remote and clock: a
// restarted or second process with another incarnation.
func (s *Stack) Restart(incarnation string) (*mcpproxy.Service, *httptest.Server) {
	s.T.Helper()
	opts := s.Opts
	opts.Incarnation = incarnation
	versions, err := secretspg.NewVersionSource(s.SecretsStore, s.Wrapper, Installation)
	if err != nil {
		s.T.Fatal(err)
	}
	creds, err := adapters.NewCredentials(versions)
	if err != nil {
		s.T.Fatal(err)
	}
	pins, _ := adapters.Pins(s.SecretsStore)
	store, err := mcppg.NewStore(s.RT.Runtime, mcppg.Options{Recheck: s.recheck, Pin: pins})
	if err != nil {
		s.T.Fatal(err)
	}
	svc, err := mcpproxy.NewService(mcpproxy.Config{Store: store, Human: s.Human, Runtime: s.Auth, Policy: s.Policy, Credentials: creds, Remote: s.Remote,
		Destinations: s.Dest, Secrets: store, Options: opts, Clock: s.RT.Clock.Now})
	if err != nil {
		s.T.Fatal(err)
	}
	s.T.Cleanup(svc.Shutdown)
	h, err := protocol.NewHandler(protocol.ServerConfig{Runtime: svc, IdleTimeout: time.Hour, MaxRequestBytes: 1 << 20})
	if err != nil {
		s.T.Fatal(err)
	}
	srv := httptest.NewServer(h)
	s.T.Cleanup(srv.Close)
	return svc, srv
}

// ServiceOver builds another service and store over db (the runtime pool, or a
// wrapper that injects faults) with the given incarnation, sharing the remote,
// clock, authority and secrets of the stack. It has its own live session handles.
func (s *Stack) ServiceOver(db storage.TxBeginner, incarnation string) (*mcpproxy.Service, *mcppg.Store) {
	s.T.Helper()
	opts := s.Opts
	opts.Incarnation = incarnation
	versions, err := secretspg.NewVersionSource(s.SecretsStore, s.Wrapper, Installation)
	if err != nil {
		s.T.Fatal(err)
	}
	creds, err := adapters.NewCredentials(versions)
	if err != nil {
		s.T.Fatal(err)
	}
	pins, _ := adapters.Pins(s.SecretsStore)
	store, err := mcppg.NewStore(db, mcppg.Options{Recheck: s.recheck, Pin: pins})
	if err != nil {
		s.T.Fatal(err)
	}
	svc, err := mcpproxy.NewService(mcpproxy.Config{Store: store, Human: s.Human, Runtime: s.Auth, Policy: s.Policy, Credentials: creds, Remote: s.Remote,
		Destinations: s.Dest, Secrets: store, Options: opts, Clock: s.RT.Clock.Now})
	if err != nil {
		s.T.Fatal(err)
	}
	s.T.Cleanup(svc.Shutdown)
	return svc, store
}

// Scope is the project of the frozen happy-path attempt.
func (s *Stack) Scope() domain.Scope { return s.baseCtx.Scope() }

// Workload is one admitted attempt with its runtime token.
type Workload struct {
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	Token   string
	Caller  mcpproxy.Caller
	Seeded  rtest.Attempt
}

// WorkloadSpec overrides the defaults: the happy-path scope and run, and a fresh
// attempt identifier.
type WorkloadSpec struct {
	Scope   *domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
}

// NewWorkload seeds an active attempt (tool.invoke granted), registers its
// admission credential, admits it through the real runtime-authority service
// and returns its token and verified caller.
func (s *Stack) NewWorkload(spec WorkloadSpec) *Workload {
	s.T.Helper()
	s.n++
	c := s.baseCtx
	if spec.Scope != nil {
		c.TenantID, c.ProjectID = spec.Scope.TenantID, spec.Scope.ProjectID
	}
	if spec.Run != "" {
		c.RunID = spec.Run
	}
	c.AttemptID = spec.Attempt
	if c.AttemptID == "" {
		c.AttemptID = domain.AttemptID("att_t033_" + string(rune('a'+s.n)))
	}
	actions := toolInvokeActions()
	a := s.RT.SeedAttempt(c, actions)
	cred, _, err := s.RT.Register(a, actions)
	if err != nil {
		s.T.Fatalf("register grant: %v", err)
	}
	resp, err := s.RT.Service.Admit(context.Background(), c.AttemptID, s.RT.AdmitRequest(a, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
	if err != nil {
		s.T.Fatalf("admit: %v", err)
	}
	caller, err := s.Auth.Authorize(context.Background(), resp.Token.Token, c.AttemptID)
	if err != nil {
		s.T.Fatalf("authorize: %v", err)
	}
	return &Workload{Scope: c.Scope(), Run: c.RunID, Attempt: c.AttemptID, Token: resp.Token.Token, Caller: caller, Seeded: a}
}

// Credential creates an MCP-kind secret holding a recipient-bound bearer and
// returns its exact reference.
func (s *Stack) Credential(scope domain.Scope, name, recipient, token string) mcpproxy.SecretRef {
	s.T.Helper()
	s.RT.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	s.RT.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5) ON CONFLICT DO NOTHING`, string(scope.TenantID), string(scope.ProjectID))
	ctx := context.Background()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Admin}, RequestID: "req_t033", Now: time.Now().UTC()}
	p, err := s.SecretsSvc.PrepareSecret(ctx, m, scope, name, secrets.KindMCP, adapters.EncodeCredential(recipient, token))
	if err != nil {
		s.T.Fatalf("prepare secret: %v", err)
	}
	if err := s.SecretsSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := s.SecretsSvc.CreateSecret(ctx, w, m, p)
		return err
	}); err != nil {
		s.T.Fatalf("create secret: %v", err)
	}
	return mcpproxy.SecretRef{Name: name, Version: 1}
}

// RotateCredential adds a new value version to a secret by name and returns the
// new exact reference.
func (s *Stack) RotateCredential(scope domain.Scope, name, recipient, token string) mcpproxy.SecretRef {
	s.T.Helper()
	ctx := context.Background()
	var id string
	var cur int64
	if err := s.RT.Owner.QueryRow(ctx, `SELECT id, current_version FROM secrets WHERE tenant_id = $1 AND project_id = $2 AND name = $3`,
		string(scope.TenantID), string(scope.ProjectID), name).Scan(&id, &cur); err != nil {
		s.T.Fatal(err)
	}
	sec, err := s.SecretsStore.GetSecret(ctx, scope, id)
	if err != nil {
		s.T.Fatal(err)
	}
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Admin}, RequestID: "req_t033", Now: time.Now().UTC()}
	p, err := s.SecretsSvc.PrepareVersion(ctx, m, scope, id, sec.Version(), adapters.EncodeCredential(recipient, token))
	if err != nil {
		s.T.Fatal(err)
	}
	if err := s.SecretsSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := s.SecretsSvc.AddVersion(ctx, w, m, p)
		return err
	}); err != nil {
		s.T.Fatal(err)
	}
	return mcpproxy.SecretRef{Name: name, Version: cur + 1}
}

// RevokeCredential revokes one version of a secret by name.
func (s *Stack) RevokeCredential(scope domain.Scope, name string, version int64) {
	s.T.Helper()
	ctx := context.Background()
	var id string
	if err := s.RT.Owner.QueryRow(ctx, `SELECT id FROM secrets WHERE tenant_id = $1 AND project_id = $2 AND name = $3`,
		string(scope.TenantID), string(scope.ProjectID), name).Scan(&id); err != nil {
		s.T.Fatal(err)
	}
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Admin}, RequestID: "req_t033", Now: time.Now().UTC()}
	if err := s.SecretsSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := s.SecretsSvc.RevokeVersion(ctx, w, m, scope, id, version)
		return err
	}); err != nil {
		s.T.Fatal(err)
	}
}

// Register creates a registration for the fixture through the real use case.
func (s *Stack) Register(scope domain.Scope, secret mcpproxy.SecretRef, tools ...string) mcpproxy.Registration {
	s.T.Helper()
	return s.RegisterAt(scope, s.Fixture.URL(), secret, tools...)
}

// RegisterAt creates a registration for an arbitrary approved endpoint.
func (s *Stack) RegisterAt(scope domain.Scope, endpoint string, secret mcpproxy.SecretRef, tools ...string) mcpproxy.Registration {
	s.T.Helper()
	s.Human.Grant(Admin, scope, mcpproxy.PermList, mcpproxy.PermMutate)
	r, err := s.Service.Create(context.Background(), scope, mcpproxy.ManageCaller{PrincipalID: Admin, RequestID: "req_t033"},
		mcpproxy.Draft{Endpoint: endpoint, Secret: secret, Tools: tools})
	if err != nil {
		s.T.Fatalf("create registration: %v", err)
	}
	return r
}

// Manage is the verified management caller.
func Manage() mcpproxy.ManageCaller {
	return mcpproxy.ManageCaller{PrincipalID: Admin, RequestID: "req_t033"}
}

// Ready is the common scenario: a workload, a registration and its credential.
type Ready struct {
	W   *Workload
	Reg mcpproxy.Registration
	Sec mcpproxy.SecretRef
}

// Setup creates the common scenario: a registration enabling regTools and a
// profile pinning the listed tools (qualified with the registration here).
func (s *Stack) Setup(regTools []string, pinned []string, max *int64) Ready {
	s.T.Helper()
	w := s.NewWorkload(WorkloadSpec{})
	sec := s.Credential(w.Scope, "remote-token", s.Fixture.Origin(), s.Fixture.Token())
	reg := s.Register(w.Scope, sec, regTools...)
	var q []string
	for _, p := range pinned {
		q = append(q, mcpproxy.Qualify(reg.ID, p))
	}
	s.Policy.Set(w.Attempt, q, max, true)
	return Ready{W: w, Reg: reg, Sec: sec}
}

// Endpoint is the workload-facing URL for an attempt and registration.
func (s *Stack) Endpoint(attempt domain.AttemptID, reg mcpproxy.RegistrationID) string {
	return s.Server.URL + "/internal/mcp/v1/attempts/" + string(attempt) + "/servers/" + string(reg)
}

type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Connect opens an SDK client session to the proxy as the workload.
func (s *Stack) Connect(ctx context.Context, endpoint, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "t033-workload", Version: "1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerRT{token: token, base: http.DefaultTransport}},
		MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
}

// Context returns a bounded test context.
func (s *Stack) Context() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.T.Cleanup(cancel)
	return ctx
}
