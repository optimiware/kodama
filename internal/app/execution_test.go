package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/results"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/workspace"
)

var fxScope = executiontest.ScopeA

// ---- doubles of the seams that need a service in the real composition ----

type fxTenants []domain.TenantID

func (t fxTenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

type fxDefinitions struct{ m *executiontest.Mem }

func (d fxDefinitions) Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	def, err := d.m.Defs.Get(ctx, scope, id)
	if err != nil {
		return pipeline.Definition{}, execution.ErrNotFound
	}
	return def, nil
}

// fxStore counts the mutations of the store it wraps and, when guard is set,
// refuses a mutation that does not carry a runtime session: the in-memory
// counterpart of results/postgres.ExecutionStore, which repeats the session
// check inside the run's transaction.
type fxStore struct {
	execution.Store
	guard bool

	mu       sync.Mutex
	updates  int
	sessions int
}

func (s *fxStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	_, has := rpg.SessionFrom(ctx)
	s.mu.Lock()
	s.updates++
	if has {
		s.sessions++
	}
	s.mu.Unlock()
	if s.guard && !has {
		return nil, nil, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no runtime session"))
	}
	return s.Store.Update(ctx, scope, id, opts, fn)
}

func (s *fxStore) counts() (updates, sessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates, s.sessions
}

// fxAuthority authenticates the bearer "token-<attempt>" for an attempt of the
// fixture's run and binds the session the guarded store looks for.
type fxAuthority struct {
	runs      execution.Store
	denied    atomic.Bool
	authCalls atomic.Int32
}

func (a *fxAuthority) Authenticate(ctx context.Context, req results.AuthRequest) (results.Principal, error) {
	a.authCalls.Add(1)
	if a.denied.Load() || req.Bearer != "token-"+string(req.Attempt) {
		return results.Principal{}, results.Fail(results.ErrUnauthenticated, nil, "the credential is not valid")
	}
	run, err := a.runs.RunOf(ctx, fxScope, req.Attempt)
	if err != nil {
		return results.Principal{}, results.Fail(results.ErrUnauthenticated, err, "the credential is not valid")
	}
	return results.Principal{
		Scope: fxScope, RunID: run, AttemptID: req.Attempt, Action: req.Action,
		Proof: rpg.Session{Tenant: fxScope.TenantID, ID: "ses_fixture", Route: req.Attempt, Action: runtimeauth.ActionResultSubmit},
	}, nil
}

func (a *fxAuthority) Bind(ctx context.Context, p results.Principal) (context.Context, error) {
	sess, ok := p.Proof.(rpg.Session)
	if !ok {
		return ctx, results.Fail(results.ErrUnauthenticated, nil, "the principal was not issued by this authority")
	}
	return rpg.WithSession(ctx, sess), nil
}

type fxEvidence struct{}

func (fxEvidence) References(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	return map[domain.ArtifactID]results.ArtifactEvidence{}, nil
}
func (fxEvidence) Protect(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) error {
	return nil
}

// fxSource stands for the workspace owner's publication coordination behind
// the guarded workspace store. It asks the same needle provider the real
// service does (attemptNeedles over the execution store) and, like the guarded
// store, publishes only for a context that carries the runtime session of the
// attempt in the route.
type fxSource struct {
	needles  workspace.SecretNeedles
	calls    atomic.Int32
	sessions atomic.Int32
}

func (s *fxSource) Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	s.calls.Add(1)
	sess, ok := rpg.SessionFrom(ctx)
	if !ok || sess.Route != attempt || sess.Tenant != scope.TenantID {
		return results.SourceOutcome{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no runtime session"))
	}
	s.sessions.Add(1)
	needles, err := s.needles.Needles(ctx, scope, attempt)
	if err != nil {
		return results.SourceOutcome{}, results.Fail(results.ErrUnavailable, err, "the secret needles are unavailable")
	}
	if needles == nil {
		return results.SourceOutcome{}, results.Fail(results.ErrUnavailable, nil, "an unknown needle set is not an empty one")
	}
	c := req.Commit
	return results.SourceOutcome{HTTPStatus: 200, Response: runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: c, AcceptedCommit: &c}}, nil
}

type fxArtifacts struct{}

func (fxArtifacts) Reserve(context.Context, artifacts.Caller, domain.AttemptID, artifacts.ReserveInput, string) (artifacts.ReserveOutput, error) {
	return artifacts.ReserveOutput{}, artifacts.ErrAuthorityMissing
}
func (fxArtifacts) Complete(context.Context, artifacts.Caller, domain.AttemptID, domain.ArtifactID, artifacts.CompleteInput, string) (artifacts.Completion, error) {
	return artifacts.Completion{}, artifacts.ErrAuthorityMissing
}

type fxLogs struct{ opened atomic.Int32 }

func (l *fxLogs) Open(context.Context, logs.Request) (*logs.Session, error) {
	l.opened.Add(1)
	return nil, logs.ErrNotFound
}

type fxCapture struct {
	mu       sync.Mutex
	observe  error
	collect  error
	record   *logs.Record
	observed int
	collects int
}

func (c *fxCapture) Observe(context.Context, domain.Scope, domain.AttemptID) (*logs.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed++
	return nil, c.observe
}

func (c *fxCapture) Collect(context.Context, domain.Scope, domain.AttemptID) (*logs.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collects++
	return c.record, c.collect
}

// ---- the fixture ----

type fixture struct {
	t       *testing.T
	m       *executiontest.Mem
	plain   *fxStore
	guarded *fxStore
	jobs    *rtest.Jobs
	late    *LateRegistrar
	logs    *fxLogs
	auth    *fxAuthority
	source  *fxSource
	// limitsDown makes the limits port report an outage.
	limitsDown atomic.Bool
	in         ExecutionInputs
	slice      *ExecutionSlice
}

func newFixture(t *testing.T, env Env, mutate ...func(*fixture)) *fixture {
	t.Helper()
	f := fxBuild(t, env, mutate...)
	slice, err := AssembleExecution(f.in)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	f.slice = slice
	return f
}

// fxBuild prepares the inputs without assembling them.
func fxBuild(t *testing.T, env Env, mutate ...func(*fixture)) *fixture {
	t.Helper()
	return fxBuildOpts(t, env, executiontest.MemOptions{}, mutate...)
}

// fxBuildOpts is fxBuild over a memory fixture with options (T-072: the
// deployment-bound cleanup authorization and the enabled step kinds).
func fxBuildOpts(t *testing.T, env Env, opts executiontest.MemOptions, mutate ...func(*fixture)) *fixture {
	t.Helper()
	m := executiontest.NewMem(t, opts)
	m.GrantAll()
	f := &fixture{t: t, m: m, jobs: &rtest.Jobs{}, late: &LateRegistrar{}, logs: &fxLogs{}}
	f.plain = &fxStore{Store: m.Mem}
	f.guarded = &fxStore{Store: m.Mem, guard: true}
	f.auth = &fxAuthority{runs: m.Mem}
	f.source = &fxSource{needles: attemptNeedles{runs: m.Mem}}

	key, err := rtjwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ring, err := rtjwt.NewKeyRing(1, key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := rtjwt.NewSigner(ring, "https://runtime.example")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: ring, Issuer: "https://runtime.example", MaxLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	db := memdb.New()
	attempts := rtest.NewMemAttempts(db)
	grants, err := rtest.NewMemStore(db, attempts)
	if err != nil {
		t.Fatal(err)
	}

	f.in = ExecutionInputs{
		Env: env, Tuning: fxTuning(),
		Deps: execution.Dependencies{
			Authority: m.Auth, Definitions: fxDefinitions{m}, Capabilities: pipeline.EnabledKinds{pipeline.KindContainer},
			Work: m.World, Configuration: m.World, Sources: m.World, Limits: m.Limits, Hooks: execution.EnabledKinds{pipeline.KindContainer},
			Tenants: fxTenants{fxScope.TenantID}, CancelledWork: m.World, Clock: m.Clock.Now, NewID: m.ID,
		},
		Options: execution.Options{ClaimLease: 2 * time.Second, Batch: 10},
		Plain:   f.plain, Guarded: f.guarded,
		Runtime: RuntimeSet{
			Runtime: m.Workloads, Jobs: f.jobs,
			Authority: func(*execpg.Store) (rtpg.AttemptAuthority, error) { return nil, errors.New("not used") },
		},
		Authority: f.auth, Evidence: fxEvidence{}, Source: f.source,
		Auth: runtimeauth.Config{
			Store: grants, Attempts: attempts, Tokens: signer, Verifier: verifier,
			Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.0.0"}},
			Policy:  runtimeauth.Policy{AdmissionTTL: time.Minute, TokenTTL: time.Minute, MaxSession: time.Hour},
		},
		Late: f.late, Artifacts: fxArtifacts{}, Logs: f.logs,
	}
	for _, mut := range mutate {
		mut(f)
	}
	return f
}

func fxTuning() Tuning {
	tune := DefaultTuning()
	tune.DispatchInterval = 5 * time.Millisecond
	tune.RateLimitPerMinute = 100000
	return tune
}

func (f *fixture) define(steps ...string) pipeline.Definition {
	return f.m.Define(fxScope, pipelinetest.Doc(steps...))
}

func (f *fixture) admit(def pipeline.Definition) (executiontest.Project, *execution.Run) {
	f.t.Helper()
	p := f.m.NewProject(fxScope)
	run, err := f.slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t071"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
	if err != nil {
		f.t.Fatalf("admit: %v", err)
	}
	return p, run
}

// startWorkers runs the slice's workers in a worker group, as the process does.
func (f *fixture) startWorkers() *workerGroup {
	g := newWorkerGroup(context.Background(), nil)
	for _, w := range f.slice.Workers {
		g.Go(w.Name, w.Run)
	}
	return g
}

func (f *fixture) eventually(what string, ok func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fixture) do(method, path, bearer string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.slice.Runtime.ServeHTTP(rec, req)
	return rec
}

// ---- execution_wiring ----

// The slice is built from the selected runtime and the injected adapters:
// constructor selection per environment, the runtime API routes, the public
// log route, the pinned backend and the dispatch of an admitted run.
func TestExecutionWiring(t *testing.T) {
	t.Run("each environment selects its own runtime", func(t *testing.T) {
		dir := t.TempDir()
		reg := &LateRegistrar{}
		base := RuntimeConfig{ServerURL: "http://127.0.0.1:8080", StateDir: dir, EntrypointBinary: "/usr/bin/kodama"}

		dev, err := NewConstructors(Development)
		if err != nil {
			t.Fatal(err)
		}
		d, err := dev.Runtime(RuntimeSettings{Config: base, Registrar: reg})
		if err != nil {
			t.Fatal(err)
		}
		if d.Runtime.Capabilities().Backend != "process" || d.Jobs == nil || d.Authority == nil || d.Live != nil {
			t.Fatalf("development runtime: %+v", d.Runtime.Capabilities())
		}

		test, _ := NewConstructors(Test)
		p, err := test.Runtime(RuntimeSettings{Config: base, Registrar: reg})
		if err != nil {
			t.Fatal(err)
		}
		caps := p.Runtime.Capabilities()
		if caps.Backend != "podman" || caps.Guarantees&execution.OCIImage == 0 || p.Jobs == nil || p.Authority == nil || p.Live != nil {
			t.Fatalf("test runtime: %+v", caps)
		}

		// A runtime without the registrar cannot register admission credentials.
		if _, err := test.Runtime(RuntimeSettings{Config: base}); err == nil {
			t.Fatal("a runtime was built without a registrar")
		}
		if _, err := test.Runtime(RuntimeSettings{Config: RuntimeConfig{}, Registrar: reg}); err == nil {
			t.Fatal("a runtime was built without its settings")
		}
	})

	t.Run("production selects the Kubernetes runtime from its policy and fails closed without it", func(t *testing.T) {
		kc := fxKubeConfig(t)
		prod, _ := NewConstructors(Production)
		m := executiontest.NewMem(t)
		settings := RuntimeSettings{
			Config: RuntimeConfig{ServerURL: "https://192.0.2.10:8443", Kubernetes: kc}, Registrar: &LateRegistrar{}, Store: m.Mem,
			Checkout: checkoutProvider{},
		}
		set, err := prod.Runtime(settings)
		if err != nil {
			t.Fatal(err)
		}
		if set.Runtime.Capabilities().Backend != "kubernetes" || set.Runtime.Capabilities().Guarantees != execution.AllGuarantees {
			t.Fatalf("production runtime: %+v", set.Runtime.Capabilities())
		}
		if set.Planner == nil || set.Placement == nil || set.Live == nil || set.Workers == nil || set.Authority == nil || set.Jobs == nil {
			t.Fatalf("the Kubernetes seams are incomplete: %+v", set)
		}
		ns, container, err := set.Placement.Placement(context.Background(), logs.AttemptInfo{Scope: domain.Scope{TenantID: "t021_acme", ProjectID: "p"}})
		if err != nil || ns != "t021-acme" || container != "workload" {
			t.Fatalf("placement: %q %q %v", ns, container, err)
		}
		if _, _, err := set.Placement.Placement(context.Background(), logs.AttemptInfo{Scope: domain.Scope{TenantID: "t_other", ProjectID: "p"}}); err == nil {
			t.Fatal("a tenant without an execution target was placed")
		}

		// Each missing piece refuses construction.
		for name, mutate := range map[string]func(*RuntimeSettings){
			"no store":       func(s *RuntimeSettings) { s.Store = nil },
			"no policy file": func(s *RuntimeSettings) { s.Config.Kubernetes.PolicyFile = "" },
			"no token":       func(s *RuntimeSettings) { s.Config.Kubernetes.TokenFile = "/nonexistent" },
			"no ca":          func(s *RuntimeSettings) { s.Config.Kubernetes.CAFile = "/nonexistent" },
			"callback differs from the policy": func(s *RuntimeSettings) {
				s.Config.ServerURL = "https://other.example"
			},
		} {
			s := settings
			mutate(&s)
			if _, err := prod.Runtime(s); err == nil {
				t.Errorf("%s: a runtime was built", name)
			}
		}
	})

	t.Run("the runtime API serves the contract routes and nothing else", func(t *testing.T) {
		f := newFixture(t, Test)
		attempt := "/runtime/v1/attempts/att_t071_1"
		for name, tc := range map[string]struct {
			method, path string
			status       int
		}{
			"admit without a body":           {"POST", attempt + "/admit", http.StatusBadRequest},
			"context without a bearer":       {"GET", attempt + "/context", http.StatusUnauthorized},
			"result without a bearer":        {"POST", attempt + "/result", http.StatusUnauthorized},
			"source without a bearer":        {"POST", attempt + "/source", http.StatusUnauthorized},
			"artifacts without a bearer":     {"POST", attempt + "/artifacts", http.StatusUnauthorized},
			"completion without a bearer":    {"POST", attempt + "/artifacts/art_1/complete", http.StatusUnauthorized},
			"secret resolution is not wired": {"POST", attempt + "/secrets/resolve", http.StatusNotFound},
			"an unknown route":               {"GET", attempt + "/anything", http.StatusNotFound},
			"another prefix":                 {"GET", "/runtime/v2/attempts/att_t071_1/context", http.StatusNotFound},
		} {
			rec := f.do(tc.method, tc.path, "", nil)
			if rec.Code != tc.status {
				t.Errorf("%s: %d (want %d): %s", name, rec.Code, tc.status, rec.Body.String())
			}
		}
		// A user token is not a runtime token: the artifact routes verify the
		// bearer against the runtime signing key.
		if rec := f.do("POST", attempt+"/artifacts", "not-a-runtime-token", map[string]any{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("a foreign bearer: %d: %s", rec.Code, rec.Body.String())
		}
		if got := f.slice.Disabled; !contains(got, "runtime-secret-resolution") {
			t.Errorf("the unwired secret route is not declared: %v", got)
		}
		for _, want := range []string{"runtime-admission", "step-result-callbacks", "runtime-artifacts", "logs"} {
			if !contains(f.slice.Routes, want) {
				t.Errorf("route %s is not declared: %v", want, f.slice.Routes)
			}
		}
	})

	t.Run("the log route joins the public router", func(t *testing.T) {
		f := newFixture(t, Test)
		router, err := httpapi.NewRouter(httpapi.RouterConfig{
			Authenticator: fxAuthn{}, Authorizer: fxAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: APIPrefix,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.slice.RegisterPublic(router); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", APIPrefix+"/tenants/"+string(fxScope.TenantID)+"/projects/"+string(fxScope.ProjectID)+"/runs/run_x/logs", nil)
		req.Header.Set("Authorization", "Bearer u")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if f.logs.opened.Load() != 1 {
			t.Fatalf("the log service was not reached: %d %s", rec.Code, rec.Body.String())
		}
		// The action is mapped by the identity rules: an action without a rule is denied.
		if _, ok := coreRules()["run.logs"]; !ok {
			t.Fatal("the log action has no authorization rule")
		}
	})

	t.Run("runs pin the selected backend and the dispatch worker starts them", func(t *testing.T) {
		f := newFixture(t, Test)
		f.m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
		if f.slice.Backend != runtimetest.ManualBackend {
			t.Fatalf("backend %q", f.slice.Backend)
		}
		_, run := f.admit(f.define(pipelinetest.Container("build")))
		agg := f.m.Get(run)
		if agg.Run.Snapshot.Runtime == nil || agg.Run.Snapshot.Runtime.Backend != runtimetest.ManualBackend {
			t.Fatalf("the run is not pinned to the selected runtime: %+v", agg.Run.Snapshot.Runtime)
		}
		names := []string{}
		for _, w := range f.slice.Workers {
			names = append(names, w.Name)
		}
		if len(names) != 1 || names[0] != "dispatch" {
			t.Fatalf("workers %v", names)
		}
		g := f.startWorkers()
		defer func() { _ = g.Stop(context.Background()) }()
		f.eventually("the dispatch of the run", func() bool { return len(f.m.Workloads.Starts()) == 1 })
		got := f.m.Workloads.Starts()[0]
		if got.RunID != run.ID || got.StepID != "build" {
			t.Fatalf("started %+v", got)
		}
		// The start was confirmed with the instance the runtime reported.
		f.eventually("dispatch confirmation", func() bool {
			a := f.m.Get(run).Attempts
			return len(a) == 1 && f.m.Get(run).Control(a[0].ID).DispatchConfirmedAt != nil
		})
		// Nothing more is started for the same attempt.
		time.Sleep(30 * time.Millisecond)
		if n := len(f.m.Workloads.Starts()); n != 1 {
			t.Fatalf("the attempt was started %d times", n)
		}
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("development drops the isolation requirement and nothing else does", func(t *testing.T) {
		if got := requirementsFor(Development, nil)(pipeline.Step{}); got != 0 {
			t.Fatalf("development requires %s", got)
		}
		for _, env := range []Env{Test, Production} {
			if requirementsFor(env, nil) != nil {
				t.Errorf("%s replaced the default requirements", env)
			}
		}
		custom := func(pipeline.Step) execution.Guarantees { return execution.NetworkPolicy }
		if got := requirementsFor(Development, custom)(pipeline.Step{}); got != execution.NetworkPolicy {
			t.Fatalf("a configured policy was replaced: %s", got)
		}
		// A runtime that lacks a required guarantee refuses the run at admission.
		f := fxBuild(t, Test, func(f *fixture) {
			f.m.Manual.SetCapabilities(execution.Capabilities{Backend: "bare"})
		})
		slice, err := AssembleExecution(f.in)
		if err != nil {
			t.Fatal(err)
		}
		f.slice = slice
		p := f.m.NewProject(fxScope)
		def := f.define(pipelinetest.Container("build"))
		_, err = slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer}, execution.AdmitCommand{
			Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
		})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a runtime without isolation admitted a container step: %v", err)
		}
	})

	t.Run("an incomplete assembly is refused rather than defaulted", func(t *testing.T) {
		for name, mutate := range map[string]func(*fixture){
			"no trusted store":         func(f *fixture) { f.in.Plain = nil },
			"no guarded store":         func(f *fixture) { f.in.Guarded = nil },
			"no runtime":               func(f *fixture) { f.in.Runtime = RuntimeSet{} },
			"no observer":              func(f *fixture) { f.in.Runtime.Jobs = nil },
			"no authority":             func(f *fixture) { f.in.Authority = nil },
			"no evidence":              func(f *fixture) { f.in.Evidence = nil },
			"no source":                func(f *fixture) { f.in.Source = nil },
			"no token verifier":        func(f *fixture) { f.in.Auth.Verifier = nil },
			"no grant store":           func(f *fixture) { f.in.Auth.Store = nil },
			"no artifact routes":       func(f *fixture) { f.in.Artifacts = nil },
			"no log service":           func(f *fixture) { f.in.Logs = nil },
			"no limits":                func(f *fixture) { f.in.Deps.Limits = nil },
			"no execution authority":   func(f *fixture) { f.in.Deps.Authority = nil },
			"no tenants":               func(f *fixture) { f.in.Deps.Tenants = nil },
			"no dispatch interval":     func(f *fixture) { f.in.Tuning.DispatchInterval = 0 },
			"unknown environment":      func(f *fixture) { f.in.Env = "" },
			"a pre-bound runtime":      func(f *fixture) { f.in.Deps.Runtime = f.m.Workloads },
			"a pre-bound store":        func(f *fixture) { f.in.Deps.Store = f.m.Mem },
			"a pre-bound planner":      func(f *fixture) { f.in.Deps.Planner = execution.DefaultPlanner{} },
			"a pre-bound job observer": func(f *fixture) { f.in.Auth.Jobs = f.jobs },
		} {
			f := fxBuild(t, Test, mutate)
			slice, err := AssembleExecution(f.in)
			if name == "no dispatch interval" {
				// The loop refuses a zero interval when it starts, not the assembly.
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				if err := slice.Workers[0].Run(context.Background()); err == nil {
					t.Errorf("%s: the dispatch loop ran", name)
				}
				continue
			}
			if err == nil || slice != nil {
				t.Errorf("%s: the slice was assembled", name)
			}
		}
	})
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

type fxAuthn struct{}

func (fxAuthn) Authenticate(context.Context, *http.Request) (httpapi.Principal, error) {
	return httpapi.Principal{ID: "u_fixture"}, nil
}

type fxAuthz struct{}

func (fxAuthz) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

// ---- execution_shutdown ----

// Workers start and stop deliberately: dispatch stops with its context and
// starts nothing afterwards, a restarted process redrives an unconfirmed start
// with the same identity, cancellation reaches the runtime through the repair
// pass and a settled workload is removed only after its log is retained.
func TestExecutionShutdown(t *testing.T) {
	t.Run("stopping the group stops dispatch and starts nothing afterwards", func(t *testing.T) {
		f := newFixture(t, Test)
		_, first := f.admit(f.define(pipelinetest.Container("build")))
		g := f.startWorkers()
		f.eventually("the first start", func() bool { return len(f.m.Workloads.Starts()) == 1 })
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.Stop(stop); err != nil {
			t.Fatalf("workers did not stop: %v", err)
		}
		if err := g.Healthy(); err != nil {
			t.Fatalf("a worker failed during a clean stop: %v", err)
		}
		// Work admitted after the stop is not started by the stopped group.
		_, second := f.admit(f.define(pipelinetest.Container("later")))
		time.Sleep(60 * time.Millisecond)
		if n := len(f.m.Workloads.Starts()); n != 1 {
			t.Fatalf("a stopped dispatch loop started work: %d starts", n)
		}
		// A new process picks the admitted run up from durable state alone.
		g2 := f.startWorkers()
		defer func() { _ = g2.Stop(context.Background()) }()
		f.eventually("the second run", func() bool { return len(f.m.Workloads.Starts()) == 2 })
		started := map[domain.RunID]bool{}
		for _, s := range f.m.Workloads.Starts() {
			started[s.RunID] = true
		}
		if !started[first.ID] || !started[second.ID] {
			t.Fatalf("starts %+v, runs %s %s", started, first.ID, second.ID)
		}
	})

	t.Run("a start blocked in the runtime ends with the stop", func(t *testing.T) {
		f := newFixture(t, Test)
		entered := make(chan struct{})
		var once sync.Once
		f.m.Workloads.Before(runtimetest.OpStart, func(ctx context.Context, _ runtimetest.Call) error {
			once.Do(func() { close(entered) })
			<-ctx.Done() // a runtime honoring its context
			return ctx.Err()
		})
		f.admit(f.define(pipelinetest.Container("build")))
		g := f.startWorkers()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the start never began")
		}
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.Stop(stop); err != nil {
			t.Fatalf("a blocked start held the shutdown: %v", err)
		}
		if err := g.Healthy(); err != nil {
			t.Fatalf("an interrupted start is not a worker failure: %v", err)
		}
	})

	t.Run("an interrupted start is redriven with the same attempt identity", func(t *testing.T) {
		f := newFixture(t, Test)
		var fail atomic.Bool
		fail.Store(true)
		f.m.Workloads.Before(runtimetest.OpStart, func(context.Context, runtimetest.Call) error {
			if fail.Load() {
				return errors.New("the process died before the runtime answered")
			}
			return nil
		})
		_, run := f.admit(f.define(pipelinetest.Container("build")))
		if err := dispatchPass(context.Background(), f.slice.Trusted, fxTenants{fxScope.TenantID}); err != nil {
			t.Fatalf("a lost start is repaired later, not reported as a failed pass: %v", err)
		}
		attempts := f.m.Get(run).Attempts
		if len(attempts) != 1 || f.m.Get(run).Control(attempts[0].ID).DispatchConfirmedAt != nil {
			t.Fatalf("the lost start must leave one unconfirmed attempt: %d", len(attempts))
		}
		fail.Store(false)
		// The claim lease has not lapsed: the repair pass leaves the attempt alone.
		if err := f.slice.ReconcileTenant(context.Background(), fxScope.TenantID); err != nil {
			t.Fatal(err)
		}
		before := len(f.m.Workloads.Starts())
		f.m.Advance(5 * time.Second)
		if err := f.slice.ReconcileTenant(context.Background(), fxScope.TenantID); err != nil {
			t.Fatal(err)
		}
		starts := f.m.Workloads.Starts()
		if len(starts) <= before {
			t.Fatalf("the unconfirmed start was not redriven: %d starts", len(starts))
		}
		last := starts[len(starts)-1]
		if last.AttemptID != attempts[0].ID || last.WorkloadName != attempts[0].WorkloadName || !last.Redrive {
			t.Fatalf("redrive %+v for attempt %+v", last, attempts[0])
		}
		if n := len(f.m.Get(run).Attempts); n != 1 {
			t.Fatalf("redrive created another attempt: %d", n)
		}
	})

	t.Run("cancelled work reaches the runtime through the repair pass", func(t *testing.T) {
		f := newFixture(t, Test)
		f.m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
		p, run := f.admit(f.define(pipelinetest.Container("build")))
		if err := dispatchPass(context.Background(), f.slice.Trusted, fxTenants{fxScope.TenantID}); err != nil {
			t.Fatal(err)
		}
		if len(f.m.Workloads.Starts()) != 1 {
			t.Fatalf("starts: %d", len(f.m.Workloads.Starts()))
		}
		// The work item's cancellation is durable state; no event is delivered.
		f.m.CancelWork(p.Scope, p.Item)
		if err := f.slice.ReconcileTenant(context.Background(), fxScope.TenantID); err != nil {
			t.Fatal(err)
		}
		stops := f.m.Workloads.Calls(runtimetest.OpStop)
		if len(stops) == 0 || stops[0].Workload.AttemptID != f.m.Get(run).Attempts[0].ID {
			t.Fatalf("the cancellation did not reach the runtime: %+v", stops)
		}
		if st := f.m.Get(run).Run.Status; st != execution.RunCancelled {
			t.Fatalf("run status %s", st)
		}
		// Cancellation is the trusted path: it never used the guarded store.
		if _, sessions := f.guarded.counts(); sessions != 0 {
			t.Fatalf("cancellation ran under a runtime session")
		}
		if updates, _ := f.guarded.counts(); updates != 0 {
			t.Fatalf("cancellation touched the guarded store %d times", updates)
		}
		// No later dispatch starts the cancelled work again.
		if err := dispatchPass(context.Background(), f.slice.Trusted, fxTenants{fxScope.TenantID}); err != nil {
			t.Fatal(err)
		}
		if len(f.m.Workloads.Starts()) != 1 {
			t.Fatalf("cancelled work was started again")
		}
	})

	t.Run("a settled workload is removed only after its log is retained", func(t *testing.T) {
		for name, tc := range map[string]struct {
			capture     *fxCapture
			live        bool
			wantRemoved bool
			wantCollect int
		}{
			"the wrapper's log is observed": {capture: &fxCapture{}, wantRemoved: true},
			"no log and no live reader":     {capture: &fxCapture{observe: logs.ErrNoLog}, wantRemoved: true},
			"an unobserved wrapper log is collected live": {
				capture: &fxCapture{observe: logs.ErrNoLog, record: &logs.Record{State: logs.StateRetained}}, live: true, wantRemoved: true, wantCollect: 1,
			},
			"explicit unavailable evidence is the outcome": {
				capture: &fxCapture{observe: logs.ErrNoLog, record: &logs.Record{State: logs.StateUnavailable}}, live: true, wantRemoved: true, wantCollect: 1,
			},
			"a capture still in progress waits": {
				capture: &fxCapture{observe: logs.ErrNoLog, record: &logs.Record{State: logs.StateCollecting}}, live: true, wantCollect: 1,
			},
			"a failing capture waits": {
				capture: &fxCapture{observe: logs.ErrNoLog, collect: errors.New("the pod log was unreachable")}, live: true, wantCollect: 1,
			},
			"a failing observation waits": {capture: &fxCapture{observe: errors.New("the log store is down")}},
		} {
			t.Run(name, func(t *testing.T) {
				c := tc.capture
				f := newFixture(t, Test, func(f *fixture) {
					f.in.Capture = c
					if tc.live {
						f.in.Runtime.Live = noLive{}
					}
				})
				f.m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
				_, run := f.admit(f.define(pipelinetest.Container("build")))
				in := f.m.MustClaim(fxScope.TenantID)
				f.m.Start(in)
				if _, err := f.m.Succeed(in, nil, ""); err != nil {
					t.Fatal(err)
				}
				if err := f.m.Terminate(in, execution.LaunchStarted); err != nil {
					t.Fatal(err)
				}
				if err := f.slice.ReconcileTenant(context.Background(), fxScope.TenantID); err != nil && tc.wantRemoved {
					t.Fatalf("reconcile: %v", err)
				}
				removed := len(f.m.Workloads.Calls(runtimetest.OpRemove))
				if (removed > 0) != tc.wantRemoved {
					t.Fatalf("removals %d, want removed=%v (run %s)", removed, tc.wantRemoved, run.ID)
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.observed == 0 {
					t.Fatal("the log was not observed before the removal")
				}
				if c.collects != tc.wantCollect {
					t.Fatalf("collects %d, want %d", c.collects, tc.wantCollect)
				}
			})
		}
	})
}

// ---- execution_guard_binding ----

// The result and source routes run over the guarded stores and trusted
// background work over the plain ones; a missing limit or authority denies.
func TestExecutionGuardBinding(t *testing.T) {
	t.Run("the guarded and the trusted store must be different", func(t *testing.T) {
		f := fxBuild(t, Test, func(f *fixture) { f.in.Guarded = f.in.Plain })
		if _, err := AssembleExecution(f.in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("the trusted store was accepted as the guarded one: %v", err)
		}
		f = fxBuild(t, Test, func(f *fixture) { f.in.Guarded = nil })
		if _, err := AssembleExecution(f.in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an assembly without a guarded store was accepted: %v", err)
		}
	})

	t.Run("a result commits through the guarded store under its session", func(t *testing.T) {
		f := newFixture(t, Test)
		f.m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
		_, run := f.admit(f.define(pipelinetest.Container("build")))
		in := f.m.MustClaim(fxScope.TenantID)
		f.m.Start(in)
		plainBefore, _ := f.plain.counts()

		zero := int64(0)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		hash, err := runtimev1.ResultHash(res)
		if err != nil {
			t.Fatal(err)
		}
		path := "/runtime/v1/attempts/" + string(in.AttemptID) + "/result"
		req := runtimev1.ResultRequest{ResultHash: hash, Result: res}

		// A wrong bearer reaches no store.
		guardedBefore, _ := f.guarded.counts()
		if rec := f.do("POST", path, "token-"+"att_other", req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a bearer of another attempt: %d %s", rec.Code, rec.Body.String())
		}
		if n, _ := f.guarded.counts(); n != guardedBefore {
			t.Fatal("an unauthenticated result reached the guarded store")
		}

		rec := f.do("POST", path, "token-"+string(in.AttemptID), req)
		if rec.Code != http.StatusOK {
			t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
		}
		updates, sessions := f.guarded.counts()
		if updates == guardedBefore || sessions != updates-guardedBefore {
			t.Fatalf("the commit must run under the runtime session on the guarded store: %d updates, %d sessions", updates, sessions)
		}
		if n, _ := f.plain.counts(); n != plainBefore {
			t.Fatalf("the result path mutated the trusted store %d times", n-plainBefore)
		}
		if st := f.m.Get(run).Attempts[0].Status; st != execution.AttemptSucceeded {
			t.Fatalf("attempt %s", st)
		}

		// The same result again is the original acknowledgment, not a second commit.
		again := f.do("POST", path, "token-"+string(in.AttemptID), req)
		if again.Code != http.StatusOK || again.Body.String() != rec.Body.String() {
			t.Fatalf("replay: %d %s (first %s)", again.Code, again.Body.String(), rec.Body.String())
		}
		// A different result after acceptance is a conflict, not a replacement.
		one, reason := int64(1), "command_failed"
		other := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &one, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		otherHash, _ := runtimev1.ResultHash(other)
		if conflict := f.do("POST", path, "token-"+string(in.AttemptID), runtimev1.ResultRequest{ResultHash: otherHash, Result: other}); conflict.Code != http.StatusConflict {
			t.Fatalf("a different result: %d %s", conflict.Code, conflict.Body.String())
		}
	})

	t.Run("an authorized writer publishes source through the guarded binding", func(t *testing.T) {
		f := newFixture(t, Test)
		f.admit(f.define(pipelinetest.Container("build")))
		in := f.m.MustClaim(fxScope.TenantID)
		f.m.Start(in)
		path := "/runtime/v1/attempts/" + string(in.AttemptID) + "/source"
		commit := runtimev1.GitCommit(executiontest.CommitB)
		req := runtimev1.SourceProposalRequest{Commit: commit, ExpectedTip: executiontest.CommitA, BundleArtifactID: "art_t071_bundle"}

		rec := f.do("POST", path, "token-"+string(in.AttemptID), req)
		if rec.Code != http.StatusOK {
			t.Fatalf("an attempt that declares no secret must be able to publish: %d %s", rec.Code, rec.Body.String())
		}
		var resp runtimev1.SourceProposalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != runtimev1.PublishAccepted || resp.Commit != commit {
			t.Fatalf("%v %s", err, rec.Body.String())
		}
		if f.source.calls.Load() != 1 || f.source.sessions.Load() != 1 {
			t.Fatalf("the proposal must reach the source binding once, under the attempt's session: %d/%d", f.source.calls.Load(), f.source.sessions.Load())
		}

		// A bearer of another attempt, and a revoked authority, never reach it.
		if rec := f.do("POST", path, "token-att_other", req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("another attempt's bearer: %d %s", rec.Code, rec.Body.String())
		}
		f.auth.denied.Store(true)
		if rec := f.do("POST", path, "token-"+string(in.AttemptID), req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a revoked authority: %d %s", rec.Code, rec.Body.String())
		}
		if f.source.calls.Load() != 1 {
			t.Fatal("a denied proposal reached the source binding")
		}
	})

	t.Run("a source proposal is refused while the attempt's injected secrets are unknown", func(t *testing.T) {
		f := newFixture(t, Test)
		ref := f.m.World.CreateSecret(fxScope, "token")
		_ = ref
		_, run := f.admit(f.define(pipelinetest.Container("build", `env: {TOKEN: {secretRef: "secret://project/token"}}`)))
		_ = run
		in := f.m.MustClaim(fxScope.TenantID)
		f.m.Start(in)
		req := runtimev1.SourceProposalRequest{Commit: executiontest.CommitB, ExpectedTip: executiontest.CommitA, BundleArtifactID: "art_t071_bundle"}
		rec := f.do("POST", "/runtime/v1/attempts/"+string(in.AttemptID)+"/source", "token-"+string(in.AttemptID), req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("an attempt with declared secrets must not publish on an empty needle set: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a revoked authority commits nothing", func(t *testing.T) {
		f := newFixture(t, Test)
		_, run := f.admit(f.define(pipelinetest.Container("build")))
		in := f.m.MustClaim(fxScope.TenantID)
		f.m.Start(in)
		f.auth.denied.Store(true)
		zero := int64(0)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		hash, _ := runtimev1.ResultHash(res)
		rec := f.do("POST", "/runtime/v1/attempts/"+string(in.AttemptID)+"/result", "token-"+string(in.AttemptID), runtimev1.ResultRequest{ResultHash: hash, Result: res})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if u, _ := f.guarded.counts(); u != 0 {
			t.Fatal("a denied result reached the guarded store")
		}
		if st := f.m.Get(run).Attempts[0].Status; st != execution.AttemptRunning {
			t.Fatalf("attempt %s", st)
		}
	})

	t.Run("a guarded store refuses a mutation without a session", func(t *testing.T) {
		f := newFixture(t, Test)
		_, run := f.admit(f.define(pipelinetest.Container("build")))
		in := f.m.MustClaim(fxScope.TenantID)
		f.m.Start(in)
		zero := int64(0)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		hash, _ := runtimev1.ResultHash(res)
		// The guarded service is the result path's; called without the session the
		// binding the authority makes, it is refused by the store.
		_, err := f.slice.Guarded.CommitResult(context.Background(), execution.ResultCommit{
			Scope: fxScope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res},
		})
		if !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("an unbound commit was not refused: %v", err)
		}
		if st := f.m.Get(run).Attempts[0].Status; st != execution.AttemptRunning {
			t.Fatalf("attempt %s", st)
		}
		// The trusted service is the one background work uses, with no session.
		if _, err := f.slice.Trusted.CommitResult(context.Background(), execution.ResultCommit{
			Scope: fxScope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res},
		}); err != nil {
			t.Fatalf("the trusted service must commit over the plain store: %v", err)
		}
	})

	t.Run("a missing limit or authority denies dispatch", func(t *testing.T) {
		for name, outage := range map[string]func(f *fixture, down bool){
			// The limits port is the assembly's; the claim's authority is the
			// store's transaction-bound one.
			"limits outage":    func(f *fixture, down bool) { f.limitsDown.Store(down) },
			"authority outage": func(f *fixture, down bool) { f.m.Auth.SetErr(fxOutage(down)) },
		} {
			f := newFixture(t, Test, func(f *fixture) { f.in.Deps.Limits = fxLimits{inner: f.m.Limits, down: &f.limitsDown} })
			f.admit(f.define(pipelinetest.Container("build")))
			outage(f, true)
			// Dispatch must not start anything the policy could not authorize.
			if err := dispatchPass(context.Background(), f.slice.Trusted, fxTenants{fxScope.TenantID}); err == nil {
				t.Errorf("%s: the pass reported no failure", name)
			}
			if n := len(f.m.Workloads.Starts()); n != 0 {
				t.Errorf("%s: %d workloads started without authority or limits", name, n)
			}
			outage(f, false)
			if err := dispatchPass(context.Background(), f.slice.Trusted, fxTenants{fxScope.TenantID}); err != nil {
				t.Errorf("%s: dispatch did not resume with the dependency: %v", name, err)
			}
			if n := len(f.m.Workloads.Starts()); n != 1 {
				t.Errorf("%s: %d starts after recovery", name, n)
			}
		}
	})

	t.Run("production refuses a memory adapter on any path", func(t *testing.T) {
		// The baseline wraps every memory adapter of the fixture in a plain type,
		// which production accepts: each case then hands one back unwrapped.
		wrap := func(f *fixture) {
			f.m.Manual.SetCapabilities(execution.Capabilities{Backend: "kubernetes", Guarantees: execution.AllGuarantees})
			f.in.Auth.Store = fxGrants{f.in.Auth.Store}
			f.in.Auth.Attempts = fxObserver{f.in.Auth.Attempts}
			peers := fxPeers{f.m.World, f.m.World, f.m.World, f.m.World}
			f.in.Deps.Work, f.in.Deps.Configuration, f.in.Deps.Sources, f.in.Deps.CancelledWork = peers, peers, peers, peers
			f.in.Deps.Authority = fxAuthorityWrap{f.m.Auth}
		}
		f := fxBuild(t, Production, wrap)
		if _, err := AssembleExecution(f.in); err != nil {
			t.Fatalf("production refused an assembly of admissible adapters: %v", err)
		}
		grants, attempts := fxBuild(t, Production).in.Auth.Store, fxBuild(t, Production).in.Auth.Attempts
		for name, mutate := range map[string]func(*fixture){
			"trusted store":       func(f *fixture) { f.in.Plain = f.m.Mem },
			"guarded store":       func(f *fixture) { f.in.Guarded = f.m.Mem },
			"grant store":         func(f *fixture) { f.in.Auth.Store = grants },
			"attempt authority":   func(f *fixture) { f.in.Auth.Attempts = attempts },
			"peer world":          func(f *fixture) { f.in.Deps.Sources = f.m.World },
			"work peer":           func(f *fixture) { f.in.Deps.Work = f.m.World },
			"table authority":     func(f *fixture) { f.in.Deps.Authority = f.m.Auth },
			"the Process runtime": func(f *fixture) { f.in.Runtime = fxProcessSet(t) },
		} {
			f := fxBuild(t, Production, wrap, mutate)
			if _, err := AssembleExecution(f.in); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("%s: production assembled a development adapter: %v", name, err)
			}
		}
	})
}

type fxGrants struct{ runtimeauth.Store }
type fxObserver struct{ runtimeauth.AttemptObserver }
type fxAuthorityWrap struct{ execution.Authority }
type fxPeers struct {
	execution.WorkContent
	execution.Configuration
	execution.Sources
	execution.CancelledWork
}

func fxProcessSet(t *testing.T) RuntimeSet {
	rt, err := process.New(process.Config{Program: process.DeclaredCommand{}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return completeRuntimeSet(rt, rt)
}

type fxLimits struct {
	inner execution.Limits
	down  *atomic.Bool
}

func (l fxLimits) Limits(ctx context.Context, scope domain.Scope) (execution.Limit, error) {
	if l.down.Load() {
		return execution.Limit{}, execution.Unavailablef(errors.New("settings are not configured"), "execution limits are not configured")
	}
	return l.inner.Limits(ctx, scope)
}

func fxOutage(down bool) error {
	if down {
		return errors.New("the identity authority is unavailable")
	}
	return nil
}

// fxKubeConfig writes the files the production runtime reads: the service
// account token, the cluster CA and the execution policy.
func fxKubeConfig(t *testing.T) KubernetesConfig {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	policy, err := json.Marshal(kubetest.PolicyConfig(t, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	return KubernetesConfig{
		APIURL: "https://192.0.2.1:6443", TokenFile: write("token", []byte("service-account-token\n")),
		CAFile: write("ca.crt", kubetest.CA(t)), PolicyFile: write("policy.json", policy),
	}
}

// fxRuns serves one fixed aggregate (or error) to the needle provider.
type fxRuns struct {
	run    domain.RunID
	agg    *execution.Aggregate
	runErr error
	getErr error
}

func (r fxRuns) RunOf(context.Context, domain.Scope, domain.AttemptID) (domain.RunID, error) {
	return r.run, r.runErr
}
func (r fxRuns) Get(context.Context, domain.Scope, domain.RunID) (*execution.Aggregate, error) {
	return r.agg, r.getErr
}

func TestAttemptNeedlesAreEstablishedFromTheAttemptRecord(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Test)
	f.admit(f.define(pipelinetest.Container("build")))
	in := f.m.MustClaim(fxScope.TenantID)
	real := attemptNeedles{runs: f.m.Mem}

	got, err := real.Needles(ctx, fxScope, in.AttemptID)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an attempt that declares no secret has an empty set: %v %v", got, err)
	}
	// Foreign and unknown attempts are not empty sets.
	if _, err := real.Needles(ctx, domain.Scope{TenantID: "t020_globex", ProjectID: fxScope.ProjectID}, in.AttemptID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("a foreign scope: %v", err)
	}
	if _, err := real.Needles(ctx, fxScope, "att_unknown"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("an unknown attempt: %v", err)
	}
	if _, err := real.Needles(ctx, domain.Scope{}, in.AttemptID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("an invalid scope: %v", err)
	}

	// Declared secrets: values may have been injected, so the set is unknown.
	f2 := newFixture(t, Test)
	f2.m.World.CreateSecret(fxScope, "token")
	f2.admit(f2.define(pipelinetest.Container("build", `env: {TOKEN: {secretRef: "secret://project/token"}}`)))
	in2 := f2.m.MustClaim(fxScope.TenantID)
	if n, err := (attemptNeedles{runs: f2.m.Mem}).Needles(ctx, fxScope, in2.AttemptID); !errors.Is(err, workspace.ErrUnavailable) || n != nil {
		t.Fatalf("declared secrets must not yield an empty set: %v %v", n, err)
	}

	// An unreadable or foreign context, a missing attempt and outages are refused.
	agg := f.m.Get(&execution.Run{Scope: fxScope, ID: in.RunID})
	at := agg.Attempt(in.AttemptID)
	good := at.Context
	for name, tc := range map[string]struct {
		runs fxRuns
		want error
	}{
		"corrupt context": {fxRuns{run: in.RunID, agg: aggWith(agg, at, func(a *execution.Attempt) { a.Context = []byte("{") })}, workspace.ErrUnavailable},
		"nil aggregate":   {fxRuns{run: in.RunID}, workspace.ErrNotFound},
		"another attempt": {fxRuns{run: in.RunID, agg: aggWith(agg, at, func(a *execution.Attempt) { a.ID = "att_other"; a.Context = good })}, workspace.ErrNotFound},
		"run outage":      {fxRuns{runErr: errors.New("db down")}, workspace.ErrUnavailable},
		"get outage":      {fxRuns{getErr: errors.New("db down")}, workspace.ErrUnavailable},
		"run hidden":      {fxRuns{runErr: execution.ErrHidden}, workspace.ErrNotFound},
	} {
		if n, err := (attemptNeedles{runs: tc.runs}).Needles(ctx, fxScope, in.AttemptID); !errors.Is(err, tc.want) || n != nil {
			t.Errorf("%s: %v %v", name, n, err)
		}
	}
}

func aggWith(base *execution.Aggregate, at *execution.Attempt, edit func(*execution.Attempt)) *execution.Aggregate {
	cp := *at
	edit(&cp)
	out := *base
	out.Attempts = []*execution.Attempt{&cp}
	return &out
}
