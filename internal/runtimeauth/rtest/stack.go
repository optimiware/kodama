// Package rtest is the shared fixture of the runtimeauth tests: one isolated,
// migrated PostgreSQL database; a transaction-aware execution fixture standing
// in for T-020's attempt authority; a controlled Job ownership observer
// standing in for T-021's Kubernetes adapter; fault injectors; and the frozen
// runtime scenarios. It is imported only by tests. These controlled adapters
// prove this module's behavior, not Kubernetes or execution-module behavior.
// Resource identifiers carry the task prefix t019.
package rtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Prefix is this task's resource prefix.
const (
	Prefix    = "t019"
	Issuer    = "https://runtime.t019.invalid"
	Namespace = "t019-runs"
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)}
}

func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestPolicy is the lifetime policy of the fixtures.
var TestPolicy = runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}

// Stack is one migrated database with runtimeauth wired over it.
type Stack struct {
	T         testing.TB
	DB        *pgtest.Database
	Owner     *pgxpool.Pool
	Runtime   *pgxpool.Pool
	Clock     *Clock
	Ring      *jwt.KeyRing
	Signer    *jwt.Signer
	Verifier  *jwt.Verifier
	Authority *Authority
	Jobs      *Jobs
	Faults    *Faults
	Store     *rtpg.Store
	Service   *runtimeauth.Service
	Handler   http.Handler
	Logs      *SafeBuffer
	// ResourceConfig, when set before NewService, enables the run_shared resource
	// subjects of every service built by this stack.
	ResourceConfig *runtimeauth.ResourceConfig
	// GitConfig, when set before NewService, enables the runtime Git grants of the
	// source-set wire of every service built by this stack.
	GitConfig *runtimeauth.GitConfig
	// Resources is the committed-resource fixture and ResourceStore the grant
	// store over it (PostgreSQL stacks only).
	Resources     *ResourceAuthority
	ResourceStore *rtpg.ResourceStore
}

// Options tune the stack.
type Options struct {
	Conns  int32
	Policy *runtimeauth.Policy
	// AttemptGit also applies migration 014_attempt_git.sql (the runtime-issued Git
	// grant records of the source-set wire).
	AttemptGit bool
}

// New provisions the database, migrations, services and HTTP handler.
func New(t testing.TB, o Options) *Stack {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conns := o.Conns
	if conns == 0 {
		conns = 16
	}
	s := &Stack{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, conns), Clock: NewClock(), Faults: &Faults{}, Logs: &SafeBuffer{}}
	contributions := []storage.Contribution{storage.BaseContribution(), rtpg.ResourceMigrations()}
	if o.AttemptGit {
		contributions = append(contributions, rtpg.AttemptGitMigrations())
	}
	manifest, err := storage.NewManifest(contributions...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, s.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s.Authority = newAuthority(t, s)
	s.Jobs = &Jobs{}
	if s.Store, err = rtpg.NewStore(s.Runtime, s.Authority); err != nil {
		t.Fatal(err)
	}
	key, err := jwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if s.Ring, err = jwt.NewKeyRing(1, key); err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = jwt.NewSigner(s.Ring, Issuer); err != nil {
		t.Fatal(err)
	}
	pol := TestPolicy
	if o.Policy != nil {
		pol = *o.Policy
	}
	if s.Verifier, err = jwt.NewVerifier(jwt.VerifierConfig{Ring: s.Ring, Issuer: Issuer, MaxLifetime: pol.MaxSession, Clock: s.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	s.Resources = newResourceAuthority(t, s)
	if s.ResourceStore, err = rtpg.NewResourceStore(s.Runtime, s.Resources); err != nil {
		t.Fatal(err)
	}
	s.ResourceConfig = &runtimeauth.ResourceConfig{Store: s.ResourceStore, State: s.Resources, Workloads: s.Resources, Tokens: s.Signer, Verifier: s.Verifier}
	s.Service = s.NewService(pol, s.Store, s.Authority, s.Jobs, s.Signer)
	if s.Handler, err = runtimehttp.NewHandler(runtimehttp.Config{Service: s.Service, Logger: newLogger(s.Logs)}); err != nil {
		t.Fatal(err)
	}
	return s
}

// NewService builds another service over the same database and key ring: a
// "restart" in tests. Faults apply to every service built here.
func (s *Stack) NewService(pol runtimeauth.Policy, store runtimeauth.Store, attempts runtimeauth.AttemptObserver, jobs runtimeauth.JobObserver, issuer runtimeauth.TokenIssuer) *runtimeauth.Service {
	svc, err := runtimeauth.NewService(runtimeauth.Config{
		Store: &faultStore{inner: store, f: s.Faults}, Attempts: &faultAttempts{inner: attempts, f: s.Faults}, Jobs: &faultJobs{inner: jobs, f: s.Faults},
		Tokens: &faultIssuer{inner: issuer, f: s.Faults}, Verifier: s.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}},
		Policy:  pol, Clock: s.Clock.Now, Logger: newLogger(s.Logs), Resources: s.ResourceConfig, Git: s.GitConfig,
	})
	if err != nil {
		s.T.Fatal(err)
	}
	return svc
}

// OwnerExec runs SQL as the migration (owner) role.
func (s *Stack) OwnerExec(sql string, args ...any) {
	s.T.Helper()
	if _, err := s.Owner.Exec(context.Background(), sql, args...); err != nil {
		s.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// SeedProject creates a tenant and project.
func (s *Stack) SeedProject(scope domain.Scope) {
	s.T.Helper()
	s.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	s.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(scope.TenantID), string(scope.ProjectID))
}

// Attempt is one seeded attempt.
type Attempt struct {
	Scope   domain.Scope
	Context runtimev1.Context
	Job     runtimeauth.ExpectedJob
	Actions runtimeauth.ActionSet
}

// AllActions is a typical worker ceiling.
func AllActions() runtimeauth.ActionSet {
	as, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionSecretResolve,
		runtimeauth.ActionArtifactReserve, runtimeauth.ActionArtifactComplete, runtimeauth.ActionSourcePropose, runtimeauth.ActionResultSubmit)
	if err != nil {
		panic(err)
	}
	return as
}

// SeedAttempt creates the project (if needed) and an active attempt with the
// given pinned context, and sets the cluster's observable workload to match
// the context's job identity from the frozen fixture.
func (s *Stack) SeedAttempt(c runtimev1.Context, actions runtimeauth.ActionSet) Attempt {
	s.T.Helper()
	scope := c.Scope()
	var exists bool
	if err := s.Owner.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2)`, string(scope.TenantID), string(scope.ProjectID)).Scan(&exists); err != nil {
		s.T.Fatal(err)
	}
	if !exists {
		s.SeedProject(scope)
	}
	digest, _, err := runtimeauth.PinContext(c)
	if err != nil {
		s.T.Fatal(err)
	}
	job := runtimeauth.ExpectedJob{Namespace: Namespace, Name: JobName(c.AttemptID)}
	s.Authority.Seed(scope, c.RunID, c.AttemptID, job, digest, actions)
	return Attempt{Scope: scope, Context: c, Job: job, Actions: actions}
}

// JobName is the deterministic Job name of an attempt in the fixtures.
func JobName(attempt domain.AttemptID) string {
	return "kodama-" + strings.ReplaceAll(string(attempt), "_", "-")
}

// Workload installs the cluster state of an attempt's Job and Pod and returns
// the identity its entrypoint would claim.
func (s *Stack) Workload(a Attempt) runtimev1.JobIdentity {
	s.T.Helper()
	sum := sha256.Sum256([]byte(a.Context.AttemptID))
	h := hex.EncodeToString(sum[:])
	uid := func(off int) string {
		return h[off:off+8] + "-" + h[off+8:off+12] + "-4" + h[off+13:off+16] + "-8" + h[off+17:off+20] + "-" + h[off+20:off+32]
	}
	id := runtimev1.JobIdentity{Name: a.Job.Name, UID: uid(0), PodName: a.Job.Name + "-x7k2p", PodUID: uid(16)}
	s.Jobs.SetCluster(runtimeauth.JobEvidence{
		Namespace: Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: Namespace, PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID,
	})
	return id
}

// AdmitRequest builds the exchange an entrypoint of version 1.4.0 would send.
func (s *Stack) AdmitRequest(a Attempt, cred runtimeauth.Credential, nonce string) runtimev1.AdmitRequest {
	return runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: nonce, ClientVersion: "1.4.0", Job: s.Workload(a)}
}

// ContextFor derives another attempt's context from a base context.
func ContextFor(base runtimev1.Context, attempt domain.AttemptID) runtimev1.Context {
	c := base
	c.AttemptID = attempt
	return c
}

// MintToken signs an arbitrary token with the ring's current key, for tests
// that need a correctly signed token with wrong claims.
func (s *Stack) MintToken(c MintClaims) string {
	s.T.Helper()
	return MintWith(s.T, s.Ring.Current(), c)
}

// Register generates a credential and registers it for the attempt.
func (s *Stack) Register(a Attempt, actions runtimeauth.ActionSet) (runtimeauth.Credential, runtimeauth.Registration, error) {
	s.T.Helper()
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		s.T.Fatal(err)
	}
	reg, err := s.RegisterWith(a, cred, actions)
	return cred, reg, err
}

// RegisterWith registers an existing credential (a retry).
func (s *Stack) RegisterWith(a Attempt, cred runtimeauth.Credential, actions runtimeauth.ActionSet) (runtimeauth.Registration, error) {
	return s.Service.Register(context.Background(), runtimeauth.RegisterCommand{Scope: a.Scope, AttemptID: a.Context.AttemptID, Credential: cred, Actions: actions, Context: a.Context})
}

// Scenario loads a frozen scenario of the runtime contract.
type Scenario struct {
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
}

// Step is one exchange of a scenario.
type Step struct {
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request"`
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body"`
}

// LoadScenario reads contracts/runtime/v1/fixtures/scenarios.json.
func LoadScenario(t testing.TB, name string) Scenario {
	t.Helper()
	_, file, _, _ := goruntime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "runtime", "v1", "fixtures", "scenarios.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []Scenario
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("scenario %q not found", name)
	return Scenario{}
}

// HappyContext is the frozen context of the happy-path scenario, and the
// cluster identity of its admit request is installed as the observable Job.
func (s *Stack) HappyContext() (runtimev1.Context, runtimev1.AdmitRequest) {
	s.T.Helper()
	sc := LoadScenario(s.T, "happy_path")
	req, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](sc.Steps[0].Request)
	if err != nil {
		s.T.Fatal(err)
	}
	var resp runtimev1.AdmitResponse
	if err := json.Unmarshal(sc.Steps[0].Body, &resp); err != nil {
		s.T.Fatal(err)
	}
	s.Jobs.SetCluster(runtimeauth.JobEvidence{
		Namespace: Namespace, JobName: req.Job.Name, JobUID: req.Job.UID,
		PodNamespace: Namespace, PodName: req.Job.PodName, PodUID: req.Job.PodUID, PodOwnerUID: req.Job.UID,
	})
	return resp.Context, req
}

// ---- execution fixture -----------------------------------------------------

// Authority is a transaction-aware stand-in for T-020's attempt authority,
// backed by a real table so locks, cancellation and admission serialize in
// PostgreSQL exactly as the documented lock order requires.
type Authority struct {
	s     *Stack
	mu    sync.Mutex
	pause func()
}

// SetCancelPause runs fn inside every Cancel transaction after its writes and
// before it commits, holding the attempt lock; tests use it to order a
// cancellation against a concurrent admission deterministically.
func (a *Authority) SetCancelPause(fn func()) { a.mu.Lock(); a.pause = fn; a.mu.Unlock() }

func newAuthority(t testing.TB, s *Stack) *Authority {
	t.Helper()
	s.OwnerExec(`CREATE TABLE t019_attempts (
		tenant_id text NOT NULL, project_id text NOT NULL, id text NOT NULL, run_id text NOT NULL,
		phase text NOT NULL, revision bigint NOT NULL, job_namespace text NOT NULL, job_name text NOT NULL,
		context_digest bytea NOT NULL, actions text[] NOT NULL, PRIMARY KEY (tenant_id, id))`)
	s.OwnerExec(`ALTER TABLE t019_attempts ENABLE ROW LEVEL SECURITY`)
	s.OwnerExec(`CREATE POLICY tenant_isolation ON t019_attempts USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())`)
	s.OwnerExec(`GRANT SELECT, INSERT, UPDATE ON t019_attempts TO ` + pgx.Identifier{s.DB.RuntimeRole}.Sanitize())
	return &Authority{s: s}
}

// Seed inserts an active attempt at revision 1.
func (a *Authority) Seed(scope domain.Scope, run domain.RunID, attempt domain.AttemptID, job runtimeauth.ExpectedJob, digest runtimeauth.Digest, actions runtimeauth.ActionSet) {
	a.s.OwnerExec(`INSERT INTO t019_attempts (tenant_id, project_id, id, run_id, phase, revision, job_namespace, job_name, context_digest, actions)
		VALUES ($1, $2, $3, $4, 'active', 1, $5, $6, $7, $8)`, string(scope.TenantID), string(scope.ProjectID), string(attempt), string(run), job.Namespace, job.Name, digest[:], actions.Strings())
}

const attemptColumns = `tenant_id, project_id, id, run_id, phase, revision, job_namespace, job_name, context_digest, actions`

func scanState(row pgx.Row) (runtimeauth.AttemptState, error) {
	var (
		st                                        runtimeauth.AttemptState
		tenant, project, id, run, phase, ns, name string
		digest                                    []byte
		actions                                   []string
	)
	if err := row.Scan(&tenant, &project, &id, &run, &phase, &st.Revision, &ns, &name, &digest, &actions); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return st, runtimeauth.ErrNotFound
		}
		return st, err
	}
	var err error
	st.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	st.AttemptID, st.RunID, st.Phase, st.Job = domain.AttemptID(id), domain.RunID(run), runtimeauth.AttemptPhase(phase), runtimeauth.ExpectedJob{Namespace: ns, Name: name}
	if st.ContextDigest, err = runtimeauth.DigestFromBytes(digest); err != nil {
		return st, err
	}
	st.Actions, err = runtimeauth.ParseActionSet(actions)
	return st, err
}

// ObserveAttempt implements runtimeauth.AttemptObserver.
func (a *Authority) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	var st runtimeauth.AttemptState
	err := storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = scanState(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM t019_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(scope.TenantID), string(scope.ProjectID), string(attempt)))
		return err
	})
	return st, err
}

// LockAttempt implements postgres.AttemptAuthority: FOR SHARE conflicts with
// Cancel's FOR UPDATE.
func (a *Authority) LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	if err := a.s.Faults.lockErr(); err != nil {
		return runtimeauth.AttemptState{}, err
	}
	st, err := scanState(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM t019_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`, string(scope.TenantID), string(scope.ProjectID), string(attempt)))
	if err == nil {
		a.s.Faults.runAfterLock()
	}
	return st, err
}

// Cancel is the execution owner's cancellation transaction: lock the attempt
// (FOR UPDATE), mark it cancelled, and, when revoke is true, revoke the
// attempt's grants through the module's transaction seam. With revoke false it
// models an owner that only changed state, proving the currentness checks do
// not depend on revocation having run.
func (a *Authority) Cancel(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, revoke bool) error {
	return storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := scanState(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM t019_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), string(attempt))); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE t019_attempts SET phase = 'cancelled' WHERE tenant_id = $1 AND id = $2`, string(scope.TenantID), string(attempt)); err != nil {
			return err
		}
		if revoke {
			if _, err := rtpg.RevokeAttempt(ctx, tx, scope, attempt, a.s.Clock.Now()); err != nil {
				return err
			}
		}
		a.mu.Lock()
		pause := a.pause
		a.mu.Unlock()
		if pause != nil {
			pause()
		}
		return nil
	})
}

// Update runs an arbitrary owner-side UPDATE of the attempt row.
func (a *Authority) Update(set string, args ...any) {
	a.s.OwnerExec(`UPDATE t019_attempts SET `+set, args...)
}

// ---- Kubernetes fixture ----------------------------------------------------

// Jobs is a controlled JobObserver reporting a settable cluster state.
type Jobs struct {
	mu      sync.Mutex
	cluster map[string]runtimeauth.JobEvidence // by Job name
	hook    func(runtimeauth.JobObservation)
	err     error
	calls   int
}

// SetCluster sets the evidence observations of the evidence's Job return.
func (j *Jobs) SetCluster(e runtimeauth.JobEvidence) {
	j.mu.Lock()
	if j.cluster == nil {
		j.cluster = map[string]runtimeauth.JobEvidence{}
	}
	j.cluster[e.JobName] = e
	j.mu.Unlock()
}

// Cluster returns the current evidence of a Job.
func (j *Jobs) Cluster(job string) runtimeauth.JobEvidence {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cluster[job]
}

// SetHook runs fn after an observation is taken and before it returns, which is
// where tests cancel an attempt "after an ownership observation".
func (j *Jobs) SetHook(fn func(runtimeauth.JobObservation)) { j.mu.Lock(); j.hook = fn; j.mu.Unlock() }

// SetError makes observations fail inconclusively.
func (j *Jobs) SetError(err error) { j.mu.Lock(); j.err = err; j.mu.Unlock() }

// Calls reports the number of observations.
func (j *Jobs) Calls() int { j.mu.Lock(); defer j.mu.Unlock(); return j.calls }

// ObserveJob implements runtimeauth.JobObserver.
func (j *Jobs) ObserveJob(_ context.Context, obs runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	j.mu.Lock()
	j.calls++
	ev, found := j.cluster[obs.Expected.Name]
	hook, err := j.hook, j.err
	j.mu.Unlock()
	if err != nil {
		return runtimeauth.JobEvidence{}, err
	}
	if !found {
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	}
	if hook != nil {
		hook(obs)
	}
	return ev, nil
}
