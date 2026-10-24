// Package gatewaytest is the fixture of the model gateway's tests: one isolated
// PostgreSQL database migrated with every module the gateway depends on, the
// real execution, secrets, identity, event and runtime-authority modules over
// it, the gateway's real PostgreSQL store and OpenAI transport, and a
// controlled TLS provider. Doubles exist only for the external seams (the
// provider, Kubernetes Job observation, the content recorder, key-provider and
// storage faults). It is imported only by tests and holds no product behavior.
//
// Resource prefix t032: tenants, projects and identifiers carry it where the
// shared execution fixture allows (its own t020 names remain).
package gatewaytest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/openai"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	identity "github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// Prefix is this task's resource prefix.
const Prefix = "t032"

const (
	// Admin holds project:settings in every fixture project.
	Admin = "u032_admin"
	// Viewer holds only read permissions.
	Viewer = "u032_viewer"

	installation = "inst-t032"
	issuer       = "https://runtime.t032.invalid"
	namespace    = "t032-runs"
	wrapperName  = "1.4.0"

	// SecretName is the project secret the default settings reference.
	SecretName = "model-key"
	// Bearer is the provider key stored in the model secret.
	Bearer = "sk-t032-PROVIDER-KEY-SENTINEL"
)

// Env is the whole wired world.
type Env struct {
	T testing.TB
	X *executiontest.Env

	Provider *Provider
	Jobs     *rtest.Jobs
	Wrapper  *Wrapper
	SecretSv *secrets.Service
	Verifier *jwt.Verifier
	Ring     *jwt.KeyRing
	RT       *runtimeauth.Service
	Policy   *openai.Policy
	Roots    *x509.CertPool
	Attempts *adapters.ExecutionAttempts
	TxAuth   *adapters.TxAuthority

	// Replica is the default gateway replica.
	Replica *Replica

	seq atomic.Int64
}

// Replica is one independently constructed gateway process: its own store,
// provider transport and service over the shared database.
type Replica struct {
	DB    *DB
	Store *gatewaypg.Store
	Svc   *gateway.Service
}

// ReplicaOptions override a replica's collaborators.
type ReplicaOptions struct {
	Recorder         gateway.Recorder
	MaxRecordedBytes int64
	Timeouts         gateway.Timeouts
	Workloads        gateway.Workloads
	Provider         gateway.Provider
	Events           gatewaypg.EventAppender
	Credentials      gateway.Credentials
	Stream           openai.StreamLimits
	NoTenants        bool
	Pins             gatewaypg.PinRegistrar
	// Logger receives the service's diagnostics (tests inspect it for leaks).
	Logger *slog.Logger
}

// New builds the environment.
func New(t testing.TB) *Env {
	t.Helper()
	x := executiontest.New(t)
	e := &Env{T: t, X: x, Provider: NewProvider(), Jobs: &rtest.Jobs{}}
	t.Cleanup(e.Provider.Close)
	x.GrantAll()
	x.Grant(executiontest.ScopeB, executiontest.Engineer, identity.RoleEngineer)
	for _, s := range []domain.Scope{executiontest.ScopeA, executiontest.ScopeA2, executiontest.ScopeB} {
		x.Grant(s, Admin, identity.RoleAdmin)
		x.Grant(s, Viewer, identity.RoleViewer)
	}

	if _, err := storage.Migrate(context.Background(), x.Migration, Manifest(t), storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate gateway: %v", err)
	}

	// Key material for the gateway's own secrets service.
	dir := t.TempDir()
	p := filepath.Join(dir, "master-key-v1")
	if err := os.WriteFile(p, make([]byte, keyfile.KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	prov, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": p}})
	must(t, err)
	e.Wrapper = &Wrapper{Inner: prov}
	e.SecretSv, err = secrets.NewService(secrets.ServiceConfig{InstallationID: installation, Store: x.Secrets, Wrapper: e.Wrapper, IDs: secrets.RandomIDs{}})
	must(t, err)

	// Destination policy and trust for the controlled TLS provider.
	e.Roots = x509.NewCertPool()
	e.Roots.AddCert(e.Provider.Srv.Certificate())
	e.Policy, err = openai.NewPolicy(openai.PolicyConfig{
		Origins:   []openai.ApprovedOrigin{{Origin: e.Provider.Origin(), PathPrefix: "/v1"}},
		Addresses: openai.AllowPrefixes(openai.PublicOnly, netip.MustParsePrefix("127.0.0.0/8")),
	})
	must(t, err)

	// Runtime authority over the real execution attempts.
	actions, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionModelInvoke)
	must(t, err)
	e.Attempts, err = adapters.NewExecutionAttempts(x.Store, namespace, actions)
	must(t, err)
	key, err := jwt.GenerateKey()
	must(t, err)
	ring, err := jwt.NewKeyRing(1, key)
	must(t, err)
	e.Ring = ring
	signer, err := jwt.NewSigner(ring, issuer)
	must(t, err)
	pol := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	e.Verifier, err = jwt.NewVerifier(jwt.VerifierConfig{Ring: ring, Issuer: issuer, MaxLifetime: pol.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	rtStore, err := rtpg.NewStore(x.Runtime, e.Attempts)
	must(t, err)
	e.RT, err = runtimeauth.NewService(runtimeauth.Config{
		Store: rtStore, Attempts: e.Attempts, Jobs: e.Jobs, Tokens: signer, Verifier: e.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{wrapperName}}, Policy: pol, Clock: x.Clock.Now,
	})
	must(t, err)
	e.TxAuth, err = adapters.NewTxAuthority(e.Attempts, signer.Generation)
	must(t, err)

	e.Replica = e.NewReplica(ReplicaOptions{})
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Manifest is every migration the gateway needs, in dependency order.
func Manifest(t testing.TB) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(Contributions()...)
	must(t, err)
	return m
}

// Contributions lists the modules in application order.
func Contributions() []storage.Contribution {
	// The execution fixture's own manifest, extended at the end: applying it again
	// skips what is already applied and adds the runtime-authority and gateway
	// migrations.
	base := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	base = append(base, execpg.Migrations()...)
	return append(base, rtpg.Migrations(), gatewaypg.Migrations())
}

// Estimator is the conservative per-model accounting of the fixtures.
func Estimator(t testing.TB) *openai.Estimator {
	est, err := openai.NewEstimator(map[string]openai.ModelAccounting{
		"gpt-a": openai.ConservativeAccounting, "gpt-b": openai.ConservativeAccounting,
	})
	must(t, err)
	return est
}

// NewReplica builds another gateway process over the same database.
func (e *Env) NewReplica(o ReplicaOptions) *Replica {
	e.T.Helper()
	x := e.X
	db := &DB{Inner: x.Runtime}
	events := o.Events
	if events == nil {
		events = adapters.EventsFrom(x.Events)
	}
	var pins gatewaypg.PinRegistrar = o.Pins
	if pins == nil {
		p, err := adapters.NewPins(x.Secrets)
		must(e.T, err)
		pins = p
	}
	store, err := gatewaypg.NewStore(gatewaypg.Config{
		DB: db, Events: events, Workload: e.TxAuth, Management: adapters.BindManagement, Pins: pins, Clock: x.Clock.Now,
	})
	must(e.T, err)

	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(e.T, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(e.T, err)
	authority, err := adapters.NewAuthority(std)
	must(e.T, err)

	var workloads gateway.Workloads = o.Workloads
	if workloads == nil {
		w, err := adapters.NewRuntimeWorkloads(e.RT)
		must(e.T, err)
		workloads = w
	}
	var provider gateway.Provider = o.Provider
	if provider == nil {
		lim := o.Stream
		if lim == (openai.StreamLimits{}) {
			lim = openai.DefaultStreamLimits()
			lim.IdleTimeout = 5 * time.Second
		}
		client, err := openai.NewClient(openai.Config{
			Policy: e.Policy, TLS: &tls.Config{RootCAs: e.Roots}, Stream: lim,
			Timeouts: openai.Timeouts{Connect: 5 * time.Second, Headers: 10 * time.Second, Total: time.Minute},
		})
		must(e.T, err)
		provider = client
	}
	var creds gateway.Credentials = o.Credentials
	if creds == nil {
		src, err := secretspg.NewVersionSource(x.Secrets, e.Wrapper, installation)
		must(e.T, err)
		c, err := adapters.NewCredentials(src, x.Secrets)
		must(e.T, err)
		creds = c
	}
	to := o.Timeouts
	if to == (gateway.Timeouts{}) {
		to = gateway.Timeouts{Request: 20 * time.Second, Recheck: 200 * time.Millisecond, Finalize: 10 * time.Second, FinalizeAttempts: 4,
			FinalizeBackoff: 20 * time.Millisecond, Record: 5 * time.Second}
	}
	cfg := gateway.Config{
		Store: store, Workloads: workloads, Authority: authority, Credentials: creds, Provider: provider,
		Estimator: Estimator(e.T), Decoder: openai.NewDecoder(), Destinations: e.Policy,
		Recorder: o.Recorder, MaxRecordedBytes: o.MaxRecordedBytes, Timeouts: to, Clock: x.Clock.Now, Logger: o.Logger,
	}
	if o.Recorder != nil && cfg.MaxRecordedBytes == 0 {
		cfg.MaxRecordedBytes = 1 << 20
	}
	if !o.NoTenants {
		cfg.Tenants = tenants{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID}
	}
	svc, err := gateway.NewService(cfg)
	must(e.T, err)
	return &Replica{DB: db, Store: store, Svc: svc}
}

type tenants []domain.TenantID

func (t tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// ---- secrets and settings ------------------------------------------------------

func (e *Env) mutation() secrets.Mutation {
	return secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Admin}, RequestID: "req_t032", Now: e.X.Clock.Now()}
}

// Payload is the model credential payload for the controlled provider.
func (e *Env) Payload(bearer, origin string) []byte {
	raw, err := json.Marshal(adapters.ModelCredentialPayload{Version: 1, Bearer: bearer, Origin: origin})
	must(e.T, err)
	return raw
}

// CreateSecret creates a secret of the kind with its first value.
func (e *Env) CreateSecret(scope domain.Scope, name string, kind secrets.SecretKind, value []byte) secrets.Secret {
	e.T.Helper()
	ctx := context.Background()
	m := e.mutation()
	p, err := e.SecretSv.PrepareSecret(ctx, m, scope, name, kind, value)
	must(e.T, err)
	var out secrets.Secret
	must(e.T, e.SecretSv.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.SecretSv.CreateSecret(ctx, w, m, p)
		return err
	}))
	return out
}

// ModelSecret creates the default model credential for the controlled provider.
func (e *Env) ModelSecret(scope domain.Scope) secrets.Secret {
	return e.CreateSecret(scope, SecretName, secrets.KindModel, e.Payload(Bearer, e.Provider.Origin()))
}

// Rotate adds a new value version.
func (e *Env) Rotate(scope domain.Scope, id string, value []byte) {
	e.T.Helper()
	ctx := context.Background()
	cur, err := e.X.Secrets.GetSecret(ctx, scope, id)
	must(e.T, err)
	m := e.mutation()
	p, err := e.SecretSv.PrepareVersion(ctx, m, scope, id, cur.Version(), value)
	must(e.T, err)
	must(e.T, e.SecretSv.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := e.SecretSv.AddVersion(ctx, w, m, p)
		return err
	}))
}

// Revoke revokes one value version.
func (e *Env) Revoke(scope domain.Scope, id string, version int64) error {
	return e.SecretSv.Atomically(context.Background(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := e.SecretSv.RevokeVersion(ctx, w, e.mutation(), scope, id, version)
		return err
	})
}

// Edit is the default enabled settings against the controlled provider.
func (e *Env) Edit() gateway.SettingsEdit {
	return gateway.SettingsEdit{
		Enabled: true, Endpoint: e.Provider.Endpoint(), Credential: gateway.CredentialRef{Ref: "secret://project/" + SecretName, Version: 1},
		Models: []string{"gpt-a", "gpt-b"},
		Limits: gateway.Limits{MaxRequests: 5, TokenAllowance: 1000, MaxRequestBytes: 64 << 10, MaxInputTokens: 4000, MaxOutputTokens: 256},
	}
}

// Configure stores the settings as the admin on the replica (the current
// revision is read first) and returns them.
func (e *Env) Configure(r *Replica, scope domain.Scope, edit gateway.SettingsEdit) gateway.ModelSettings {
	e.T.Helper()
	res, err := e.TryConfigure(r, scope, edit)
	must(e.T, err)
	return res.Settings
}

// TryConfigure is Configure returning the error.
func (e *Env) TryConfigure(r *Replica, scope domain.Scope, edit gateway.SettingsEdit) (gateway.SettingsResult, error) {
	cur, err := r.Svc.GetSettings(context.Background(), gateway.Actor{ID: Admin}, scope)
	if err != nil {
		return gateway.SettingsResult{}, err
	}
	return r.Svc.ReplaceSettings(context.Background(), gateway.Actor{ID: Admin}, scope, cur.Revision, edit, "req_t032")
}

// ---- attempts ------------------------------------------------------------------

// Attempt is one running attempt with a verified runtime session.
type Attempt struct {
	Scope    domain.Scope
	RunID    domain.RunID
	ID       domain.AttemptID
	Token    string
	Workload gateway.Workload
	Intent   execution.AttemptSpec
}

// NewRun admits a run with n parallel steps and returns one running, admitted
// attempt per step. All attempts share the run.
func (e *Env) NewRun(scope domain.Scope, n int) []Attempt {
	e.T.Helper()
	return e.NewRunWith(scope, n, runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionModelInvoke)
}

// NewRunWith is NewRun with the grant's action ceiling chosen by the test.
func (e *Env) NewRunWith(scope domain.Scope, n int, granted ...runtimeauth.Action) []Attempt {
	e.T.Helper()
	x := e.X
	p := x.NewProject(scope)
	var steps []string
	for i := 0; i < n; i++ {
		steps = append(steps, pipelinetest.Container(fmt.Sprintf("s%d", i), "workspace: {access: read, from: run}"))
	}
	def := x.Define(scope, pipelinetest.Doc(steps...))
	run := x.MustAdmit(p, def, nil)
	var out []Attempt
	for i := 0; i < n; i++ {
		in, err := x.Svc.ClaimRunnable(context.Background(), scope.TenantID)
		must(e.T, err)
		must(e.T, x.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, execution.Observation{
			Kind: execution.ObservedDispatched, WorkloadName: in.WorkloadName, SpecFingerprint: in.SpecFingerprint, ObservedAt: x.Clock.Now(),
		}))
		out = append(out, e.admit(scope, run, *in, granted))
	}
	return out
}

// NewAttempt is NewRun with one attempt.
func (e *Env) NewAttempt(scope domain.Scope) Attempt { return e.NewRun(scope, 1)[0] }

func (e *Env) admit(scope domain.Scope, run *execution.Run, in execution.AttemptSpec, granted []runtimeauth.Action) Attempt {
	e.T.Helper()
	ctx := context.Background()
	agg := e.X.Get(run)
	at := agg.Attempt(in.AttemptID)
	doc, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(e.T, err)
	cred, err := runtimeauth.GenerateCredential(nil)
	must(e.T, err)
	actions, err := runtimeauth.NewActionSet(granted...)
	must(e.T, err)
	_, err = e.RT.Register(ctx, runtimeauth.RegisterCommand{Scope: scope, AttemptID: in.AttemptID, Credential: cred, Actions: actions, Context: doc})
	must(e.T, err)

	n := e.seq.Add(1)
	uid := fmt.Sprintf("00000032-0000-4000-8000-%012d", n)
	podUID := fmt.Sprintf("00000033-0000-4000-8000-%012d", n)
	pod := in.WorkloadName + "-x7k2p"
	e.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: namespace, JobName: in.WorkloadName, JobUID: uid, PodNamespace: namespace, PodName: pod, PodUID: podUID, PodOwnerUID: uid})
	resp, err := e.RT.Admit(ctx, in.AttemptID, runtimev1.AdmitRequest{
		AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: wrapperName,
		Job: runtimev1.JobIdentity{Name: in.WorkloadName, UID: uid, PodName: pod, PodUID: podUID},
	})
	must(e.T, err)
	a := Attempt{Scope: scope, RunID: run.ID, ID: in.AttemptID, Token: resp.Token.Token, Intent: in}
	w, err := adapters.NewRuntimeWorkloads(e.RT)
	must(e.T, err)
	a.Workload, err = w.Authorize(ctx, a.ID, a.Token)
	if err != nil && actions.Contains(runtimeauth.ActionModelInvoke) {
		e.T.Fatal(err)
	}
	return a
}

// CancelRun cancels the run through the execution service, as an engineer.
func (e *Env) CancelRun(a Attempt) {
	e.T.Helper()
	_, err := e.X.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t032"}, a.Scope, a.RunID)
	must(e.T, err)
}

// Body builds a minimal valid streaming request body.
func Body(model, prompt string, maxTokens int64) []byte {
	m := map[string]any{"model": model, "stream": true, "messages": []map[string]any{{"role": "user", "content": prompt}}}
	if maxTokens > 0 {
		m["max_tokens"] = maxTokens
	}
	raw, _ := json.Marshal(m)
	return raw
}

// Invoke runs one request as the attempt on the replica.
func (e *Env) Invoke(r *Replica, a Attempt, body []byte, sink gateway.Sink) (gateway.InvokeResult, error) {
	return r.Svc.Invoke(context.Background(), gateway.InvokeRequest{Attempt: a.ID, Bearer: a.Token, Body: body, Sink: sink})
}

// ---- inspection ----------------------------------------------------------------

// Owner exposes the owner-role helpers of the execution fixture.
func (e *Env) Count(sql string, args ...any) int { return e.X.Count(sql, args...) }

// Scalar runs a single-value query as the owner.
func (e *Env) Scalar(dst any, sql string, args ...any) { e.X.Scalar(dst, sql, args...) }

// Budget reads the run's counters.
func (e *Env) Budget(run domain.RunID) (admitted, charged int64) {
	e.T.Helper()
	if err := e.X.Migration.QueryRow(context.Background(),
		`SELECT requests_admitted, tokens_charged FROM model_run_bindings WHERE run_id = $1`, string(run)).Scan(&admitted, &charged); err != nil {
		e.T.Fatalf("budget: %v", err)
	}
	return admitted, charged
}

// Requests lists the run's requests as (state, code) in admission order.
func (e *Env) Requests(run domain.RunID) [][2]string {
	e.T.Helper()
	rows, err := e.X.Migration.Query(context.Background(),
		`SELECT state, coalesce(outcome_code, '') FROM model_requests WHERE run_id = $1 ORDER BY ordinal`, string(run))
	must(e.T, err)
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var s, c string
		must(e.T, rows.Scan(&s, &c))
		out = append(out, [2]string{s, c})
	}
	must(e.T, rows.Err())
	return out
}

// Dump returns every text the gateway persisted or enqueued for the tenant, so
// tests can assert that a sentinel is absent.
func (e *Env) Dump(tenant domain.TenantID) string {
	e.T.Helper()
	var all string
	for _, q := range []string{
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM model_requests) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM model_usage) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM model_run_bindings) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM model_settings_revisions) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM audit) t`,
	} {
		var s string
		if err := e.X.Migration.QueryRow(context.Background(), q).Scan(&s); err != nil {
			e.T.Fatalf("dump: %v\n%s", err, q)
		}
		all += s + "\n"
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(convert_from(payload, 'UTF8'), E'\n'), '') FROM outbox WHERE tenant_id = $1`,
	} {
		var s string
		err := e.X.RuntimeTx(tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, string(tenant)).Scan(&s) })
		must(e.T, err)
		all += s + "\n"
	}
	return all
}

// Events counts the tenant's outbox events of a type.
func (e *Env) Events(tenant domain.TenantID, typ string) int {
	return e.X.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = $2`, string(tenant), typ)
}

// Pool exposes the execution package's PostgreSQL store for adapter wiring.
func (e *Env) ExecStore() *execpg.Store { return e.X.Store }

// Issuer is the runtime token issuer of the fixture.
const Issuer = issuer

// Mint signs a runtime token for the attempt's session with the ring's key.
// mod may break one rule (audience, issuer, tenant, lifetime...).
func (e *Env) Mint(a Attempt, mod func(*rtest.MintClaims)) string {
	e.T.Helper()
	now := e.X.Clock.Now()
	c := rtest.MintClaims{Issuer: issuer, Subject: a.Workload.Session, Tenant: string(a.Scope.TenantID), IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(4 * time.Minute)}
	if mod != nil {
		mod(&c)
	}
	return rtest.MintWith(e.T, e.Ring.Current(), c)
}

// SecretNamed finds a project secret by name.
func (e *Env) SecretNamed(scope domain.Scope, name string) secrets.Secret {
	e.T.Helper()
	list, err := e.X.Secrets.ListSecrets(context.Background(), scope, "", 100)
	must(e.T, err)
	for _, s := range list {
		if s.Name() == name {
			return s
		}
	}
	e.T.Fatalf("secret %q not found", name)
	return secrets.Secret{}
}

// VersionSource is the standalone exact-version source over the fixture's
// secret store, for tests that exercise the secrets seam directly.
func (e *Env) VersionSource() *secretspg.VersionSource {
	e.T.Helper()
	src, err := secretspg.NewVersionSource(e.X.Secrets, e.Wrapper, installation)
	must(e.T, err)
	return src
}

// AttemptContext returns the attempt's immutable context exactly as the
// execution module stored it, raw and decoded.
func (e *Env) AttemptContext(a Attempt) (raw []byte, doc runtimev1.Context) {
	e.T.Helper()
	agg := e.X.Get(&execution.Run{ID: a.RunID, Scope: a.Scope})
	at := agg.Attempt(a.ID)
	raw = append([]byte(nil), at.Context...)
	d, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(e.T, err)
	return raw, d
}
