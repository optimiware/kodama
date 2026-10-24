// Package composition_test is the independent composition fixture of T-062: real
// PostgreSQL carrying the execution, workspace, secret, artifact and
// runtime-grant schemas (migrations applied in the merged order, runtimeauth
// after execution, under separate owner and non-owner runtime roles); the
// runtime-enabled execution store (execpg.NewRuntimeStore) whose grant
// revocation is bound to runtimeauth/postgres; the guarded result decorators
// and the real results HTTP handler; optionally real S3 (LocalStack) for
// artifact bytes and a real HTTPS Git server for publication. Fault injection
// wraps the existing seams (transaction commit, peer transaction bindings and
// event append), never production code. Only the Kubernetes Job observation and
// the hosted identity are controlled doubles, because their production adapters
// belong to T-021 and T-010/T-027.
//
// Resource prefix t062: tenants, projects, work items, workspaces, the S3
// subprefix, the Git repository and every fixture identifier carry it.
package composition_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/results/adapters"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Prefix is the task's resource prefix.
const (
	Prefix    = "t062"
	Namespace = "t062-runs"
	Issuer    = "https://runtime.t062.invalid"
)

var (
	ScopeA = domain.Scope{TenantID: "t062_acme", ProjectID: "p062_inventory"}
	ScopeB = domain.Scope{TenantID: "t062_globex", ProjectID: "p062_ops"}
)

// Options select the heavy dependencies a test really needs, so a test never
// claims coverage of a dependency it did not use.
type Options struct {
	// Artifacts provisions S3 and the artifact service.
	Artifacts bool
	// Git provisions the HTTPS Git server, the workspace service and publication.
	Git bool
	// WrapPublisher inserts fault injection around the real publisher.
	WrapPublisher func(workspace.RefPublisher) workspace.RefPublisher
	// WrapWorkspaceStore inserts fault injection under the guarded workspace store.
	WrapWorkspaceStore func(workspace.Store) workspace.Store
	// SingleConn gives the execution, grant and guard stores a runtime pool of
	// exactly one connection, so any peer that opened a second transaction on
	// the pool while its caller's transaction was open would deadlock.
	SingleConn bool
	// Evidence replaces the artifact evidence port (an outage double).
	Evidence func(results.Evidence) results.Evidence
}

// Env is one integrated environment.
type Env struct {
	T testing.TB
	X *executiontest.Env

	Pool *pgxpool.Pool
	// Faults is the controllable fault and trace layer around the seams.
	Faults *Faults
	// External records whether external calls (executor, Git) ran while a SQL
	// transaction was open.
	External *External
	// RT is the runtime-enabled execution store shared by every service.
	RT *execpg.Store

	// runtime authority
	Ring     *rtjwt.KeyRing
	Verifier *rtjwt.Verifier
	Jobs     *rtest.Jobs
	Auth     *runtimeauth.Service
	Attempts *rpg.AttemptAuthority
	Guard    *rpg.Guard

	// execution over the guarded store
	Gate      *Gate
	ExecPlain *execution.Service // built over the plain store: trusted paths (cancel, dispatch, reconcile)
	ExecSvc   *execution.Service // built over the guarded store: the result path
	Guarded   *rpg.ExecutionStore
	Commits   *CountingCommitter

	// artifacts
	S3       *s3support.Env
	ArtStore *artifactspg.Store
	Objects  *arts3.Store
	ArtSvc   *artifacts.Service
	ArtReg   *artifactstest.Registry
	ArtRead  *artifactstest.Reader

	// git and workspace
	Git       *gitfx.Server
	Repo      *gitfx.Repo
	WS        *workspace.Service
	WSStore   *rpg.WorkspaceStore
	WSPlain   *workspace.Service // reconciliation and trusted paths over the plain store
	Publisher *wst.FaultPublisher
	Secrets   *wst.Secrets
	Authz     *wst.Authz
	Items     *wst.Items
	Repos     *wst.Repos

	// WSHooks run inside the workspace store decorator chain, above the guard.
	WSHooks WorkspaceHooks

	Results   *results.Service
	Handler   http.Handler
	Authority *adapters.Authority
	Ctx       *CtxAuthority

	plainDeps workspace.Dependencies
	wsOpts    workspace.Options

	opts Options
	seq  atomic.Int64
	http *http.Client
}

// ID returns a fresh valid identifier carrying the task prefix.
func (e *Env) ID(prefix string) string { return prefix + "_t062_" + itoa(e.seq.Add(1)) }

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = digits[n%10]
	}
	return string(b[i:])
}

type creds struct {
	read, write string
}

func (c creds) Resolve(_ context.Context, s domain.Scope, ref string) (gitad.Credential, error) {
	if s != ScopeA && s != ScopeB {
		return gitad.Credential{}, errors.New("foreign scope")
	}
	switch ref {
	case "secret://t062/checkout":
		return gitad.Credential{Username: gitfx.Username, Password: c.read}, nil
	case "secret://t062/publisher":
		return gitad.Credential{Username: gitfx.Username, Password: c.write}, nil
	}
	return gitad.Credential{}, errors.New("unknown secret reference")
}

// New builds the environment.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	x := executiontest.New(t)
	e := &Env{T: t, X: x, Pool: x.Runtime, opts: o, http: &http.Client{Timeout: 30 * time.Second}}
	ctx := context.Background()

	// Tests keep many attempts alive at once; the fixture's quota is not what they exercise.
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 200, 100
	x.Limits.Set(lim)
	for _, s := range []domain.Scope{ScopeA, ScopeB} {
		x.Base.SeedTenant(t, string(s.TenantID), string(s.ProjectID))
		x.Grant(s, executiontest.Engineer, identity.RoleEngineer)
	}

	// The execution fixture migrated everything but the runtime grants; the
	// same manifest plus T-019's contribution is a pure append.
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	contribs = append(contribs, rtpg.Migrations()) // appended after execution, as the composition root lists it
	manifest, err := storage.NewManifest(contribs...)
	must(t, err)
	mctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err = storage.Migrate(mctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole})
	must(t, err)

	// ---- the runtime-enabled execution store over fault-injectable seams ----
	pool := x.Runtime
	if o.SingleConn {
		pool = x.Base.DB.RuntimePool(ctx, 1)
		e.Pool = pool
	}
	e.Faults = NewFaults(pool)
	e.External = &External{pool: x.Runtime}
	e.RT, err = execpg.NewRuntimeStore(e.Faults.DB(), e.Faults.Appender(x.Events), execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: e.Faults.Workspaces(x.Workspaces), Secrets: e.Faults.Secrets(x.Secrets),
	}, e.Faults.Revoker(execadapters.RevokeAttemptGrants), execpg.Options{Clock: x.Clock.Now})
	must(t, err)

	// ---- runtime authority over execution's rows ----
	e.Attempts, err = rpg.NewAttemptAuthority(e.RT, func(domain.Scope) (string, error) { return Namespace, nil }, nil)
	must(t, err)
	grants, err := rtpg.NewStore(pool, e.Attempts)
	must(t, err)
	key, err := rtjwt.GenerateKey()
	must(t, err)
	e.Ring, err = rtjwt.NewKeyRing(1, key)
	must(t, err)
	signer, err := rtjwt.NewSigner(e.Ring, Issuer)
	must(t, err)
	pol := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	e.Verifier, err = rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: e.Ring, Issuer: Issuer, MaxLifetime: pol.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	e.Jobs = &rtest.Jobs{}
	e.Auth, err = runtimeauth.NewService(runtimeauth.Config{
		Store: grants, Attempts: e.Attempts, Jobs: e.Jobs, Tokens: signer, Verifier: e.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}, Policy: pol, Clock: x.Clock.Now,
	})
	must(t, err)
	e.Guard, err = rpg.NewGuard(e.Attempts, e.Ring.Generation, x.Clock.Now)
	must(t, err)
	e.Authority, err = adapters.NewAuthority(e.Verifier, e.Ring, grants, x.Clock.Now)
	must(t, err)

	// ---- execution: plain (trusted) and guarded (result path) ----
	e.Guarded, err = rpg.NewExecutionStore(e.RT, e.Faults.DB(), e.Guard)
	must(t, err)
	e.Gate = &Gate{}

	// ---- artifacts ----
	if o.Artifacts {
		e.S3 = s3support.New(t)
		e.ArtStore, err = artifactspg.NewStore(artifactspg.Config{DB: pool})
		must(t, err)
		e.Objects, err = arts3.NewStore(arts3.Config{Client: e.S3.Client, Bucket: e.S3.Bucket(), Prefix: e.S3.Prefix, AllowInsecure: true, Clock: x.Clock.Now})
		must(t, err)
		e.ArtReg, e.ArtRead = artifactstest.NewRegistry(), artifactstest.NewReader()
		artAuth, err := adapters.NewArtifactAuthority(grants, e.Ring, x.Clock.Now)
		must(t, err)
		e.ArtSvc, err = artifacts.NewService(artifacts.ServiceConfig{
			Repository: e.ArtStore, Authority: artAuth, Reader: e.ArtRead, Objects: e.Objects, Registry: e.ArtReg,
			Clock: x.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
		})
		must(t, err)
	}

	// ---- git and workspace ----
	var sources execution.Sources = x.Sources
	if o.Git {
		e.Git = gitfx.NewServer(t)
		e.Repo = e.Git.NewRepo(t, "t062-repo")
		gopts := gitad.Options{
			CAFile: e.Git.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
			ScratchRoot: filepath.Join(t.TempDir(), "scratch"),
		}
		cr := creds{read: e.Git.ReadToken, write: e.Git.WriteToken}
		reader, err := gitad.NewReader(gopts, cr)
		must(t, err)
		pub, err := gitad.NewPublisher(gopts, cr)
		must(t, err)
		t.Cleanup(reader.Close)
		t.Cleanup(pub.Close)
		e.Publisher = &wst.FaultPublisher{Inner: pub}
		var wpub workspace.RefPublisher = &observedPublisher{RefPublisher: e.Publisher, ext: e.External}
		if o.WrapPublisher != nil {
			wpub = o.WrapPublisher(wpub)
		}
		e.Authz, e.Items, e.Repos, e.Secrets = wst.NewAuthz(), wst.NewItems(), wst.NewRepos(), wst.NewSecrets()
		for _, s := range []domain.Scope{ScopeA, ScopeB} {
			e.Authz.Grant(wst.Engineer.ID, s)
			e.Authz.Grant("execution", s)
			e.Repos.Set(s, workspace.RepositoryConfig{ID: "repo_t062", URL: e.Repo.URL, CheckoutSecretRef: "secret://t062/checkout", PublisherSecretRef: "secret://t062/publisher", BaseRef: e.Repo.BaseRef})
		}
		attempts, err := execpg.NewAttemptAuthority(e.RT)
		must(t, err)
		bundles, err := adapters.NewBundles(e.ArtStore, e.Objects)
		if o.Artifacts {
			must(t, err)
		}
		wsOpts := wst.DefaultOptions()
		e.WSStore, err = rpg.NewWorkspaceStore(x.Workspaces, pool, e.Guard)
		must(t, err)
		var store workspace.Store = &hookStore{Store: e.WSStore, hooks: &e.WSHooks}
		if o.WrapWorkspaceStore != nil {
			store = o.WrapWorkspaceStore(store)
		}
		deps := workspace.Dependencies{
			Store: store, Tenants: tenants{ScopeA.TenantID, ScopeB.TenantID}, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos,
			Attempts: attempts, Bundles: bundles, Secrets: e.Secrets, Refs: reader, Source: reader, Publisher: wpub,
			Clock: x.Clock.Now,
		}
		if !o.Artifacts {
			deps.Bundles = noBundles{}
		}
		e.WS, err = workspace.NewService(deps, wsOpts)
		must(t, err)
		plain := deps
		plain.Store = x.Workspaces
		plain.Publisher = e.Publisher
		e.plainDeps, e.wsOpts = plain, wsOpts
		e.WSPlain, err = workspace.NewService(plain, wsOpts)
		must(t, err)
		sources, err = execadapters.NewSources(x.Workspaces, reader, e.WSPlain)
		must(t, err)
	}

	// The runtime confirms every workload stopped once asked, with the
	// evidence that the command ran: tests that need an outage replace this.
	x.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
	x.Workloads.Before(runtimetest.OpStop, func(context.Context, runtimetest.Call) error {
		e.External.Observe("runtime.stop")
		return nil
	})
	e.ExecPlain = x.NewService(executiontest.Options{Sources: sources, Store: e.RT})
	e.ExecSvc = x.NewService(executiontest.Options{Sources: sources, Store: &gatedStore{Store: e.Guarded, gate: e.Gate}})
	e.Commits = &CountingCommitter{inner: e.ExecSvc}
	// The ordinary service is the trusted one; keep x.Svc consistent with it.
	x.Svc = e.ExecPlain

	execAd, err := adapters.NewExecution(e.Commits, e.RT)
	must(t, err)
	ev := results.Evidence(noEvidence{})
	var srcPort results.Source = noSource{}
	if o.Artifacts {
		ev, err = adapters.NewArtifacts(e.ArtSvc)
		must(t, err)
	}
	if o.Git {
		ws, err := wsruntime.NewSource(e.WS)
		must(t, err)
		srcPort, err = adapters.NewSource(ws)
		must(t, err)
	}
	if o.Evidence != nil {
		ev = o.Evidence(ev)
	}
	e.Ctx = &CtxAuthority{Authority: e.Authority}
	e.Results, err = results.NewService(results.Dependencies{Authority: e.Ctx, Contexts: execAd, Evidence: ev, Finalizer: execAd, Source: srcPort}, results.Options{})
	must(t, err)
	e.Handler, err = resultshttp.NewHandler(resultshttp.Config{Service: e.Results})
	must(t, err)
	return e
}

type tenants []domain.TenantID

func (t tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

type noBundles struct{}

func (noBundles) Open(context.Context, domain.Scope, domain.ArtifactID) (workspace.Bundle, error) {
	return workspace.Bundle{}, workspace.ErrNotFound
}
func (noBundles) Settled(context.Context, domain.Scope, domain.ArtifactID) error { return nil }

var errNotProvisioned = errors.New("the test did not provision this dependency")

type noEvidence struct{}

func (noEvidence) References(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	return map[domain.ArtifactID]results.ArtifactEvidence{}, nil
}
func (noEvidence) Protect(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) error {
	return nil
}

type noSource struct{}

func (noSource) Propose(context.Context, domain.Scope, domain.AttemptID, runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	return results.SourceOutcome{}, errNotProvisioned
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- controlled interleavings ----------------------------------------------

// Gate pauses a guarded mutation after its work and before its commit, with every
// lock the transaction holds still held, so a test orders a competing
// transaction deterministically.
type Gate struct {
	mu      sync.Mutex
	after   func()
	entered chan struct{}
	release chan struct{}
}

// Arm makes the next guarded execution mutation pause after its mutation ran.
// The returned function waits until it paused; release lets it commit.
func (g *Gate) Arm() (waitPaused func(), release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entered, rel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.entered, g.release = entered, rel
	g.after = func() {
		g.mu.Lock()
		g.after = nil
		g.mu.Unlock()
		close(entered)
		<-rel
	}
	return func() {
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				panic("the guarded mutation never reached the gate")
			}
		}, func() {
			once.Do(func() { close(rel) })
		}
}

func (g *Gate) fire() {
	g.mu.Lock()
	fn := g.after
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type gatedStore struct {
	execution.Store
	gate *Gate
}

func (s *gatedStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	return s.Store.Update(ctx, scope, id, opts, func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		fx, err := fn(ctx, env, agg)
		if err == nil {
			s.gate.fire()
		}
		return fx, err
	})
}

// CountingCommitter counts the commits that reached execution.
type CountingCommitter struct {
	inner *execution.Service
	n     atomic.Int64
}

func (c *CountingCommitter) CommitResult(ctx context.Context, in execution.ResultCommit) (execution.ResultAck, error) {
	c.n.Add(1)
	return c.inner.CommitResult(ctx, in)
}

// Count is the number of commits attempted.
func (c *CountingCommitter) Count() int { return int(c.n.Load()) }

// CtxAuthority wraps the real authority with test hooks.
type CtxAuthority struct {
	Authority results.Authority
	mu        sync.Mutex
	After     func() // runs after a successful Authenticate, before Bind
}

func (c *CtxAuthority) Authenticate(ctx context.Context, req results.AuthRequest) (results.Principal, error) {
	p, err := c.Authority.Authenticate(ctx, req)
	c.mu.Lock()
	hook := c.After
	c.mu.Unlock()
	if err == nil && hook != nil {
		hook()
	}
	return p, err
}

func (c *CtxAuthority) Bind(ctx context.Context, p results.Principal) (context.Context, error) {
	return c.Authority.Bind(ctx, p)
}

// SetAfter installs or clears the hook.
func (c *CtxAuthority) SetAfter(fn func()) { c.mu.Lock(); c.After = fn; c.mu.Unlock() }

// ---- seeding ---------------------------------------------------------------

const (
	readRun  = "workspace: {access: read, from: run}"
	writeRun = "workspace: {access: write, from: run}"
)

// Job builds a container step.
func Job(id string, extra ...string) string {
	s := "{id: " + id + ", kind: container, image: " + pipelinetest.Image + ", command: [/bin/sh, -ec]"
	timeout := true
	for _, x := range extra {
		timeout = timeout && !strings.HasPrefix(x, "timeoutSeconds")
		s += ", " + x
	}
	if timeout {
		s += ", timeoutSeconds: 600"
	}
	return s + "}"
}

// Project is a work item with its workspace.
type Project struct {
	Scope     domain.Scope
	Item      domain.WorkItemID
	Workspace workspace.ID
}

// NewProject seeds a work item and a workspace. With Git, the workspace is
// provisioned through the real service (the branch exists on the remote).
func (e *Env) NewProject(scope domain.Scope) Project {
	e.T.Helper()
	if !e.opts.Git {
		p := e.X.NewProject(scope)
		return Project{Scope: scope, Item: p.Item, Workspace: p.Workspace}
	}
	item := e.X.WorkItem(scope, e.ID("wi"), "# Plan\nDo the thing.")
	e.Items.Add(scope, item, "story")
	w, _, err := e.WS.Provision(context.Background(), wst.Engineer, scope, item)
	must(e.T, err)
	if w.State != workspace.StateAvailable {
		e.T.Fatalf("workspace %s (%s)", w.State, w.LastError)
	}
	return Project{Scope: scope, Item: item, Workspace: w.ID}
}

// Source is the commit runs start from.
func (e *Env) Source() string {
	if e.opts.Git {
		return e.Repo.Initial
	}
	return executiontest.CommitA
}

// Attempt is one started attempt with runtime authority.
type Attempt struct {
	Run     *execution.Run
	Intent  *execution.AttemptSpec
	Context runtimev1.Context
	Token   string
	Session runtimeauth.SessionID
	Scope   domain.Scope
	Project Project
	Cred    runtimeauth.Credential
}

// ID is the attempt identifier.
func (a *Attempt) ID() domain.AttemptID { return a.Intent.AttemptID }

// Run admits a definition, claims and starts its first step, registers the
// attempt's grant and exchanges the admission credential for a runtime token,
// as the executor and the entrypoint would.
func (e *Env) Run(p Project, steps ...string) *Attempt {
	e.T.Helper()
	def := e.X.Define(p.Scope, pipelinetest.Doc(steps...))
	run, err := e.ExecPlain.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t062"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(executiontest.Project{Scope: p.Scope, Item: p.Item}),
		SourceCommit: e.Source(),
	})
	must(e.T, err)
	return e.start(p, run)
}

func (e *Env) start(p Project, run *execution.Run) *Attempt {
	e.T.Helper()
	a := e.startUnadmitted(p, run, true)
	e.admit(a)
	return a
}

// startUnadmitted claims and starts the run's first step without registering a
// runtime grant, as the executor does before the workload exchanges its
// admission credential.
func (e *Env) startUnadmitted(p Project, run *execution.Run, started bool) *Attempt {
	e.T.Helper()
	in, err := e.ExecPlain.ClaimRunnable(context.Background(), p.Scope.TenantID)
	must(e.T, err)
	if started {
		e.X.Start(in)
	}
	agg, err := e.X.Store.Get(context.Background(), p.Scope, run.ID)
	must(e.T, err)
	at := agg.Attempt(in.AttemptID)
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(e.T, err)
	return &Attempt{Run: run, Intent: in, Context: pinned, Scope: p.Scope, Project: p}
}

// admit registers the grant and exchanges the credential.
func (e *Env) admit(a *Attempt) {
	e.T.Helper()
	ctx := context.Background()
	cred, err := runtimeauth.GenerateCredential(nil)
	must(e.T, err)
	actions, err := rpg.DefaultActions(a.Intent.Access)
	must(e.T, err)
	_, err = e.Auth.Register(ctx, runtimeauth.RegisterCommand{Scope: a.Scope, AttemptID: a.ID(), Credential: cred, Actions: actions, Context: a.Context})
	must(e.T, err)
	id := e.workload(a)
	resp, err := e.Auth.Admit(ctx, a.ID(), runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Job: id})
	must(e.T, err)
	a.Token, a.Cred = resp.Token.Token, cred
	claims, err := e.Verifier.Verify(ctx, a.Token)
	must(e.T, err)
	a.Session = claims.Session
}

func (e *Env) workload(a *Attempt) runtimev1.JobIdentity {
	sum := sha256.Sum256([]byte(a.ID()))
	h := hex.EncodeToString(sum[:])
	uid := func(off int) string {
		return h[off:off+8] + "-" + h[off+8:off+12] + "-4" + h[off+13:off+16] + "-8" + h[off+17:off+20] + "-" + h[off+20:off+32]
	}
	name := a.Intent.WorkloadName
	id := runtimev1.JobIdentity{Name: name, UID: uid(0), PodName: name + "-x7k2p", PodUID: uid(16)}
	e.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: Namespace, PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID})
	return id
}

// Principal is the router's runtime principal for the attempt's session.
func (a *Attempt) Principal() artifacts.Caller {
	return artifacts.Caller{Subject: adapters.RuntimePrincipal(a.Scope.TenantID, a.Session)}
}

// ---- results ---------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

// Completed builds a completed result.
func Completed(outputs map[string]runtimev1.Value, diag ...domain.ArtifactID) runtimev1.Result {
	if outputs == nil {
		outputs = map[string]runtimev1.Value{}
	}
	if diag == nil {
		diag = []domain.ArtifactID{}
	}
	return runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: outputs, DiagnosticArtifactIDs: diag}
}

// Request wraps a result with its canonical hash.
func Request(t testing.TB, res runtimev1.Result) runtimev1.ResultRequest {
	t.Helper()
	h, err := runtimev1.ResultHash(res)
	must(t, err)
	return runtimev1.ResultRequest{ResultHash: h, Result: res}
}

// Submit submits a result through the service with the attempt's token.
func (e *Env) Submit(a *Attempt, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	return e.Results.Submit(context.Background(), results.SubmitCommand{Attempt: a.ID(), Bearer: a.Token, Request: req})
}

// SubmitHTTP submits through the HTTP handler and returns status and body.
func (e *Env) SubmitHTTP(a *Attempt, req runtimev1.ResultRequest) (int, []byte) {
	return e.do(a, "result", mustJSON(e.T, req))
}

func (e *Env) do(a *Attempt, route string, body []byte) (int, []byte) {
	return e.doToken(a.ID(), a.Token, route, body)
}

func (e *Env) doToken(attempt domain.AttemptID, token, route string, body []byte) (int, []byte) {
	req, err := http.NewRequest(http.MethodPost, "http://runtime/runtime/v1/attempts/"+string(attempt)+"/"+route, bytes.NewReader(body))
	must(e.T, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := &recorder{header: http.Header{}, code: 200}
	e.Handler.ServeHTTP(rec, req)
	return rec.code, rec.body.Bytes()
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// ---- artifacts -------------------------------------------------------------

// Upload reserves, uploads and completes a file artifact for the attempt.
func (e *Env) Upload(a *Attempt, name string, role artifacts.Role, data []byte) domain.ArtifactID {
	e.T.Helper()
	id := e.Reserve(a, name, role, data)
	e.PutAndComplete(a, id, data)
	return id
}

// Reserve reserves a file artifact and uploads the bytes without completing.
func (e *Env) Reserve(a *Attempt, name string, role artifacts.Role, data []byte) domain.ArtifactID {
	e.T.Helper()
	mt, size := "text/plain", int64(len(data))
	out, err := e.ArtSvc.Reserve(context.Background(), a.Principal(), a.ID(), artifacts.ReserveInput{
		Name: name, Kind: artifacts.KindFile, Role: role, Digest: artifacts.DigestOf(data), MediaType: &mt, Size: &size,
	}, "req_t062")
	must(e.T, err)
	if out.Upload == nil {
		e.T.Fatal("no upload instruction")
	}
	up := *out.Upload
	req, err := http.NewRequest(up.Method, up.URL, bytes.NewReader(data))
	must(e.T, err)
	req.ContentLength = int64(len(data))
	for k, v := range up.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := e.http.Do(req)
	must(e.T, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		e.T.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	return out.ID
}

// PutAndComplete completes an uploaded artifact.
func (e *Env) PutAndComplete(a *Attempt, id domain.ArtifactID, data []byte) {
	e.T.Helper()
	_, err := e.ArtSvc.Complete(context.Background(), a.Principal(), a.ID(), id, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: int64(len(data))}, "req_t062")
	must(e.T, err)
}

// ---- inspection (owner role) -----------------------------------------------

// Count runs a count query as the owner role.
func (e *Env) Count(sql string, args ...any) int { return e.X.Count(sql, args...) }

// Attempt returns the persisted attempt.
func (e *Env) Persisted(a *Attempt) *execution.Attempt {
	e.T.Helper()
	agg, err := e.X.Store.Get(context.Background(), a.Scope, a.Run.ID)
	must(e.T, err)
	return agg.Attempt(a.ID())
}

// Aggregate returns the persisted run aggregate.
func (e *Env) Aggregate(a *Attempt) *execution.Aggregate {
	e.T.Helper()
	agg, err := e.X.Store.Get(context.Background(), a.Scope, a.Run.ID)
	must(e.T, err)
	return agg
}

// Outbox counts the run's state events and audits in the tenant, as the app sees them.
func (e *Env) HistoryRows(a *Attempt) int {
	return e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1`, string(a.Run.ID))
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

func (e *Env) TenantTx(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.Pool, tenant, fn)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ArtStoreDigest is the digest of content, for leak assertions.
func (e *Env) ArtStoreDigest(data []byte) string { return string(artifacts.DigestOf(data)) }

// ---- writers and source ------------------------------------------------------

// Writer starts a writer attempt (the single workspace writer slot reserved).
func (e *Env) Writer(p Project, extra ...string) *Attempt {
	e.T.Helper()
	return e.Run(p, Job("build", append([]string{writeRun}, extra...)...))
}

// Bundle builds what the entrypoint would upload for the attempt on top of the
// attempt's expected tip and uploads it as a completed bundle artifact.
func (e *Env) Bundle(a *Attempt, msg string, files ...gitfx.File) (string, domain.ArtifactID) {
	e.T.Helper()
	src := a.Context.Workspace.Source
	if src == nil || src.ExpectedTip == nil {
		e.T.Fatal("not a writer attempt")
	}
	commit, bundle, _ := e.Repo.Bundle(e.T, string(*src.ExpectedTip), msg, files...)
	return commit, e.Upload(a, e.ID("bundle"), artifacts.RoleBundle, bundle)
}

// Proposal is the source proposal of a commit and its bundle.
func (e *Env) Proposal(a *Attempt, commit string, art domain.ArtifactID) runtimev1.SourceProposalRequest {
	return runtimev1.SourceProposalRequest{Commit: runtimev1.GitCommit(commit), ExpectedTip: *a.Context.Workspace.Source.ExpectedTip, BundleArtifactID: art}
}

// ProposeHTTP posts a source proposal through the handler.
func (e *Env) ProposeHTTP(a *Attempt, req runtimev1.SourceProposalRequest) (int, []byte) {
	return e.do(a, "source", mustJSON(e.T, req))
}

// ReceivePacks counts the pushes that reached the Git server.
func (e *Env) ReceivePacks() int {
	return e.Git.CountRequests(func(r gitfx.Request) bool { return r.Service == "git-receive-pack" && r.Method == "POST" })
}

// Branch is the workspace's work branch.
func (e *Env) Branch(a *Attempt) string { return *a.Context.Workspace.Source.Branch }

// Cancel cancels the attempt's run through execution's own (trusted) path.
func (e *Env) Cancel(a *Attempt) {
	e.T.Helper()
	_, err := e.ExecPlain.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t062"}, a.Scope, a.Run.ID)
	must(e.T, err)
}

// Publications counts the publication receipts of the workspace.
func (e *Env) Publications(ws workspace.ID) int {
	return e.Count(`SELECT count(*) FROM workspace_publications WHERE workspace_id = $1`, string(ws))
}

// CheckpointEvents counts the checkpoint events of the tenant.
func (e *Env) CheckpointEvents(tenant domain.TenantID) int {
	return e.X.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE event_type = 'workspace.checkpoint_created'`)
}

// Workspace returns the persisted workspace.
func (e *Env) Workspace(p Project) *workspace.Workspace {
	e.T.Helper()
	w, err := e.X.Workspaces.Get(context.Background(), p.Scope, p.Workspace)
	must(e.T, err)
	return w
}

// WorkspaceHooks are test interleavings around workspace mutations.
type WorkspaceHooks struct {
	mu sync.Mutex
	// Before runs before every guarded-store Update of an attempt's receipt, once
	// per call, outside any transaction.
	before func(load workspace.Load)
}

// SetBefore installs or clears the hook.
func (h *WorkspaceHooks) SetBefore(fn func(load workspace.Load)) {
	h.mu.Lock()
	h.before = fn
	h.mu.Unlock()
}

type hookStore struct {
	workspace.Store
	hooks *WorkspaceHooks
}

func (s *hookStore) Update(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	s.hooks.mu.Lock()
	before := s.hooks.before
	s.hooks.mu.Unlock()
	if before != nil {
		before(load)
	}
	return s.Store.Update(ctx, scope, id, load, fn)
}

var wsEngineer = wst.Engineer

// WorkspaceRestart builds another workspace service over the same database,
// Git server and ports, as a restarted control-plane process would be.
func (e *Env) WorkspaceRestart() *workspace.Service {
	e.T.Helper()
	svc, err := workspace.NewService(e.plainDeps, e.wsOpts)
	must(e.T, err)
	return svc
}

// rpgSession attaches the attempt's verified session to ctx, as the authority
// does for a request.
func rpgSession(ctx context.Context, a *Attempt) context.Context {
	return rpg.WithSession(ctx, rpg.Session{Tenant: a.Scope.TenantID, ID: a.Session, Generation: 1, Route: a.ID(), Action: runtimeauth.ActionResultSubmit})
}
