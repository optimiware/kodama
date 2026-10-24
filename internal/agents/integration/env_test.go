package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// env is the execution module's real PostgreSQL fixture extended with the agent
// profile tables, the agent admission chain and a second, agent-capable
// execution service. Only the external seams are doubles (executor, effect
// reconciler, Git observation, as in executiontest).
type env struct {
	*executiontest.Env
	Agents    *agents.Service
	AgentRepo *agentspg.Store
	Approvals *agentstest.Approvals
	Draft     agents.Draft
	Chain     adapters.Chain
	Resolver  adapters.Resolver
	Lookup    adapters.Lookup
	Validator *pipeline.Validator
	Svc       *execution.Service // shadows Env.Svc: this one expands and plans agents
	Router    *httpapi.Router
	Attempts  adapters.AttemptSource
}

type envOptions struct {
	// Catalogue overrides the compatibility catalogue (default: qualifies Draft).
	Catalogue agents.Compatibility
}

func newEnv(t *testing.T, o ...envOptions) *env {
	t.Helper()
	var opt envOptions
	if len(o) > 0 {
		opt = o[0]
	}
	x := executiontest.New(t)
	x.GrantAll()
	e := &env{Env: x, Approvals: agentstest.NewApprovals(), Draft: agentstest.Draft(t, "coding")}

	// The agents contribution is appended after the contributions executiontest
	// applied, in the same order, so the applied history is a prefix.
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	manifest, err := storage.NewManifest(append(append(contribs, execpg.Migrations()...), agentspg.Migrations())...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate agents: %v", err)
	}

	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(t, err)
	agentAuth, err := adapters.NewAuthority(std, reader)
	must(t, err)
	e.AgentRepo, err = agentspg.NewStore(x.Runtime, agentspg.Options{})
	must(t, err)
	var compat agents.Compatibility = agentstest.Catalogue(t, e.Draft)
	if opt.Catalogue != nil {
		compat = opt.Catalogue
	}
	e.Agents, err = agents.NewService(agents.ServiceConfig{
		Repository: e.AgentRepo, Authority: agentAuth, Compatibility: compat, Approvals: e.Approvals,
		Clock: x.Clock.Now, NewID: func() (string, error) { return x.ID("ap"), nil },
	})
	must(t, err)

	e.Resolver, err = adapters.NewResolver(e.Agents)
	must(t, err)
	e.Lookup, err = adapters.NewLookup(e.Agents)
	must(t, err)
	e.Chain, err = adapters.NewChain(adapters.ChainConfig{Resolver: e.Resolver, Store: x.Store, Creator: x.Keyed})
	must(t, err)
	e.Attempts, err = adapters.NewAttemptSource(x.Store)
	must(t, err)
	e.Validator, err = pipeline.NewValidator(pipeline.Config{
		Capabilities: e.Chain.Capabilities(pipeline.KindGate), Profiles: e.Lookup,
		References: pipelinetest.References{Secrets: map[string]bool{"token": true}},
	})
	must(t, err)

	e.Svc = e.newService(t)

	// The HTTP surface of run creation, through the agent creator.
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-engineer": executiontest.Engineer}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	must(t, err)
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	must(t, err)
	h, err := executionhttp.NewHandler(executionhttp.Config{Service: e.Svc, Creator: e.Chain.Creator, Cursors: codec})
	must(t, err)
	must(t, h.RegisterRoutes(rt))
	e.Router = rt
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type tokens map[string]string

func (tk tokens) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := tk[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// workContent reads work items directly, as executiontest does.
type workContent struct{ e *executiontest.Env }

func (w workContent) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	var kind, state, spec string
	var rev int64
	err := w.e.Migration.QueryRow(ctx, `SELECT kind, state, spec, content_revision FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&kind, &state, &spec, &rev)
	if err != nil {
		return execution.WorkSnapshot{}, execution.ErrNotFound
	}
	return execution.WorkSnapshot{
		ID: id, Actionable: kind != "epic", Cancelled: state == "cancelled", ContentRevision: domain.ContentRevision(rev), Spec: spec,
		Criteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}},
	}, nil
}

// newService builds another agent-capable execution service over the same store
// and ports, as a second control-plane process (or a restart) would.
func (e *env) newService(t testing.TB) *execution.Service { return e.newServiceFor(t, e.Chain) }

// newServiceFor builds an execution service over another agent chain.
func (e *env) newServiceFor(t testing.TB, chain adapters.Chain) *execution.Service {
	t.Helper()
	x := e.Env
	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(t, err)
	authority, err := execadapters.NewAuthority(std, reader)
	must(t, err)
	resolver, err := secrets.NewConfigurationResolver(x.Secrets, x.Secrets, nil)
	must(t, err)
	cfg, err := execadapters.NewConfiguration(resolver)
	must(t, err)
	defs, err := execadapters.NewDefinitions(x.Pipeline)
	must(t, err)
	svc, err := execution.NewService(execution.Dependencies{
		Store: chain.Store, Authority: authority, Definitions: defs, Capabilities: chain.Capabilities(pipeline.KindGate),
		Work: workContent{x}, Configuration: cfg, Sources: x.Sources, Limits: x.Limits, Planner: chain.Planner, Hooks: chain.Hooks,
		Tenants: tenantList{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID}, Clock: x.Clock.Now,
		Runtime: x.Workloads, EffectReconciler: x.Effects,
	}, execution.Options{ClaimLease: 2 * time.Second, Batch: 10})
	must(t, err)
	return svc
}

// ---- profiles and definitions ----

// registerAs registers a draft through the real service and identity authority.
func (e *env) registerAs(principal string, scope domain.Scope, d agents.Draft) (agents.RegisterResult, error) {
	return e.Agents.Register(context.Background(), agents.RegisterCommand{
		Scope: scope, Caller: agents.Caller{PrincipalID: principal, RequestID: "req_t030"}, Draft: d,
	})
}

func (e *env) register(scope domain.Scope, d agents.Draft) agents.ProfileVersion {
	e.T.Helper()
	res, err := e.registerAs(executiontest.Engineer, scope, d)
	if err != nil {
		e.T.Fatalf("register profile: %v", err)
	}
	return res.Profile
}

// variant returns the default draft with distinct instructions.
func (e *env) variant(n int) agents.Draft {
	d := e.Draft
	d.Instructions = fmt.Sprintf("%s\nVariant %d.", d.Instructions, n)
	return d
}

// define validates a YAML pipeline with agent steps enabled and registers it.
func (e *env) define(scope domain.Scope, yaml string) pipeline.Definition {
	e.T.Helper()
	actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
	v, err := e.Validator.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		e.T.Fatalf("validate definition: %v", err)
	}
	def, _, err := e.Pipeline.RegisterOrGet(context.Background(), pipeline.Candidate{
		Scope: scope, ID: domain.DefinitionID(e.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: e.Clock.Now(),
	})
	must(e.T, err)
	return def
}

// agentDoc is a one-step pipeline running the profile ref read-only.
func agentDoc(name, ref string, extra string) doc {
	return doc(pipelinetest.DocNamed(name, "{}", "{id: implement, kind: agent, agent: {profile: "+ref+"}, timeoutSeconds: 600, workspace: {access: read, from: run}"+extra+"}"))
}

// ---- running ----

func (e *env) caller() execution.Caller {
	return execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t030"}
}

func (e *env) admit(p executiontest.Project, def pipeline.Definition) (*execution.Run, error) {
	return e.Svc.Admit(context.Background(), e.caller(), execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
}

func (e *env) mustAdmit(p executiontest.Project, def pipeline.Definition) *execution.Run {
	e.T.Helper()
	run, err := e.admit(p, def)
	if err != nil {
		e.T.Fatalf("admit: %v", err)
	}
	return run
}

func (e *env) claim(svc *execution.Service, tenant domain.TenantID) *execution.AttemptSpec {
	e.T.Helper()
	in, err := svc.ClaimRunnable(context.Background(), tenant)
	if err != nil {
		e.T.Fatalf("claim: %v", err)
	}
	return in
}

func (e *env) start(in *execution.AttemptSpec) {
	e.T.Helper()
	must(e.T, e.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, execution.Observation{
		Kind: execution.ObservedStarted, WorkloadName: in.WorkloadName, WorkloadUID: "11111111-2222-3333-4444-555555555555"}))
}

func (e *env) commit(in *execution.AttemptSpec, res runtimev1.Result, artifacts map[domain.ArtifactID]runtimev1.ArtifactRef) (execution.ResultAck, error) {
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	return e.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res}, Artifacts: artifacts,
	})
}

// attemptContext decodes the pinned context of an attempt from the database.
func (e *env) attemptContext(in *execution.AttemptSpec) runtimev1.Context {
	e.T.Helper()
	var raw string
	e.Scalar(&raw, `SELECT context FROM step_attempts WHERE tenant_id = $1 AND id = $2`, string(in.Scope.TenantID), string(in.AttemptID))
	c, err := runtimev1.DecodeRequest[runtimev1.Context]([]byte(raw))
	if err != nil {
		e.T.Fatalf("decode context: %v", err)
	}
	return c
}

func (e *env) payload(c runtimev1.Context) agents.WrapperConfig {
	e.T.Helper()
	cfg, present, err := agents.DecodeArgs(c.Command.Args)
	if err != nil || !present {
		e.T.Fatalf("payload: %v %v", present, err)
	}
	return cfg
}

// httpCreate starts a run through the keyed HTTP route.
func (e *env) httpCreate(p executiontest.Project, def pipeline.Definition, key string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"workItemId":%q,"definitionId":%q,"contentRevision":%d,"sourceCommit":%q}`,
		p.Item, def.ID, e.ContentRevision(p), executiontest.CommitA)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/"+string(p.Scope.TenantID)+"/projects/"+string(p.Scope.ProjectID)+"/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-engineer")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	e.Router.ServeHTTP(rec, req)
	return rec
}

func runID(t testing.TB, rec *httptest.ResponseRecorder) domain.RunID {
	t.Helper()
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.ID == "" {
		t.Fatalf("not a run: %v %s", err, rec.Body.String())
	}
	return domain.RunID(run.ID)
}

func (e *env) runRef(scope domain.Scope, id domain.RunID) *execution.Run {
	return &execution.Run{Scope: scope, ID: id}
}

func ptr[T any](v T) *T { return &v }

const agentsImageFragment = "t030-agent@sha256"

func contains(b []byte, s string) bool { return strings.Contains(string(b), s) }

// replaceAccess turns the generated read-only agent document into a writer.
type doc string

func (d doc) replaceAccess() string {
	return strings.Replace(string(d), "workspace: {access: read, from: run}", "workspace: {access: write, from: run}", 1)
}

func pipelineWithContainer(ref string) string {
	return pipelinetest.DocNamed("mixed", "{}",
		"{id: build, kind: container, image: "+pipelinetest.Image+", command: [/bin/sh, -ec], args: [make], timeoutSeconds: 60, workspace: {access: read, from: run}}",
		"{id: implement, kind: agent, needs: [build], agent: {profile: "+ref+"}, timeoutSeconds: 600, workspace: {access: read, from: run}}")
}
