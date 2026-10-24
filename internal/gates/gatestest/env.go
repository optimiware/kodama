// Package gatestest holds the fixtures of the gate module's tests: the
// execution module's isolated PostgreSQL database migrated with the gate
// schema, the real gate store, service, keyed decision helper, gate-aware
// execution store, identity and artifact adapters, and controllable component
// seams only for deployment facts and the workspace history. The deployment
// module (T-035) is implemented, but this fixture does not exercise the real
// joined path; that qualification belongs to T-073. It contains no production
// behavior.
//
// Resource prefix t034: databases (via KODAMA_RESOURCE_PREFIX), S3 prefixes and
// fixtures carry it. Tenants, projects, work items and workspaces come from the
// execution fixture.
package gatestest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Prefix is the resource prefix of this task's test resources.
const Prefix = "t034"

// Fixture scopes and users (the execution fixture's tenants).
var (
	ScopeA  = executiontest.ScopeA
	ScopeA2 = executiontest.ScopeA2
	ScopeB  = executiontest.ScopeB
)

const (
	// Reviewer holds only the reviewer role.
	Reviewer = "u034_reviewer"
	// Author holds engineer and reviewer: an author who also reviews.
	Author = "u034_author"
	// Admin holds only the admin role (never a reviewer).
	Admin = "u034_admin"
	// Engineer holds only the engineer role.
	Engineer = executiontest.Engineer
	// Viewer holds only the viewer role.
	Viewer = executiontest.Viewer

	CommitA = executiontest.CommitA
	CommitB = executiontest.CommitB
	CommitC = executiontest.CommitC

	// ReportOutput is the declared output name of the conformance report.
	ReportOutput = "conformance"
)

// Options tune the fixture.
type Options struct {
	// S3 stores artifact bytes in the isolated S3 prefix instead of memory.
	S3 bool
	// GateTTL is the gate deadline duration (default one hour).
	GateTTL time.Duration
	// WrapExecEvents inserts fault injection before execution events are
	// appended (claims, run and attempt transitions).
	WrapExecEvents func(gatespg.EventAppender) gatespg.EventAppender
	// WrapEvents inserts fault injection before gate events are appended.
	WrapEvents func(gatespg.EventAppender) gatespg.EventAppender
	// NoDeployments leaves the deployment adapter unset.
	NoDeployments bool
	// NoConformance leaves the conformance adapter unset.
	NoConformance bool
	// WrapObjects wraps the object store the evidence reader uses.
	WrapObjects func(artifacts.ObjectStore) artifacts.ObjectStore
	// WrapEvidence wraps the evidence reader (fault injection).
	WrapEvidence func(gates.Evidence) gates.Evidence
	// WrapPins wraps the pin port (fault injection).
	WrapPins func(gates.Pins) gates.Pins
	// WrapCandidates wraps the candidate verifier.
	WrapCandidates func(gates.Candidates) gates.Candidates
	// WrapAuthority wraps the transaction authority binder.
	WrapAuthority func(gatespg.AuthorityBinder) gatespg.AuthorityBinder
	// Sources replaces the fake workspace sources of both the execution service
	// and the candidate verifier (real Git adapters). It runs once the base
	// fixture exists.
	Sources func(e *Env) execution.Sources
}

// Env is one fully wired gate module over real PostgreSQL.
type Env struct {
	T testing.TB
	X *executiontest.Env

	Repo     *artifactspg.Store
	Objects  artifacts.ObjectStore
	S3       *s3support.Env
	Evidence *adapters.Evidence

	Store    *gatespg.Store
	Keyed    *gatespg.Keyed
	GateExec *gatespg.ExecutionStore
	Authz    *adapters.Authority
	Gates    *gates.Service
	// Svc is the execution service over the gate-aware store: the one a
	// production composition root installs.
	Svc *execution.Service

	Deployments *FakeDeployments
	Mem         *MemObjects
	Opts        Options

	mu          sync.Mutex
	realSources execution.Sources
	baseline    *runtimev1.DesignBaseline
	seq         int
	ttl         time.Duration
}

// New builds the environment. PostgreSQL follows test/support/postgres: the
// test skips when no administrator URL is configured and fails instead when
// KODAMA_REQUIRE_POSTGRES=1.
func New(t testing.TB, o ...Options) *Env {
	t.Helper()
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	xo := executiontest.Options{}
	if opt.WrapExecEvents != nil {
		xo.WrapAppender = func(a execpg.EventAppender) execpg.EventAppender { return opt.WrapExecEvents(a) }
	}
	x := executiontest.New(t, xo)
	e := &Env{T: t, X: x, Opts: opt, Mem: NewMemObjects(), Deployments: NewFakeDeployments(), ttl: opt.GateTTL}
	if e.ttl == 0 {
		e.ttl = time.Hour
	}
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	contribs = append(contribs, gatespg.Migrations()...)
	manifest, err := storage.NewManifest(contribs...)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate gates: %v", err)
	}

	e.Repo, err = artifactspg.NewStore(artifactspg.Config{DB: x.Runtime})
	must(t, err)
	if opt.S3 {
		e.S3 = s3support.New(t)
		store, err := arts3.NewStore(arts3.Config{Client: e.S3.Client, Bucket: e.S3.Bucket(), Prefix: e.S3.Prefix, AllowInsecure: true, Clock: x.Clock.Now})
		must(t, err)
		e.Objects = store
	} else {
		e.Objects = e.Mem
	}
	objects := e.Objects
	if opt.WrapObjects != nil {
		objects = opt.WrapObjects(objects)
	}
	e.Evidence, err = adapters.NewEvidence(e.Repo, objects)
	must(t, err)

	e.build()
	return e
}

// build constructs the stores and services over the current options. Tests that
// change an option rebuild with it (a restarted process).
func (e *Env) build() {
	t, x, opt := e.T, e.X, e.Opts
	var appender gatespg.EventAppender = x.Events
	if opt.WrapEvents != nil {
		appender = opt.WrapEvents(appender)
	}
	var binder gatespg.AuthorityBinder = adapters.BindAuthority
	if opt.WrapAuthority != nil {
		binder = opt.WrapAuthority(binder)
	}
	cfg := gatespg.Config{DB: x.Runtime, Events: appender, Execution: x.Store, Authority: binder, Clock: x.Clock.Now}
	if !opt.NoDeployments {
		cfg.Deployments = func(pgx.Tx) gates.Deployments { return e.Deployments }
	}
	var err error
	e.Store, err = gatespg.NewStore(cfg)
	must(t, err)
	e.Keyed, err = gatespg.NewKeyed(x.Idem, e.Store)
	must(t, err)
	e.GateExec, err = gatespg.NewExecutionStore(x.Runtime, x.Store, x.Store, e.Store)
	must(t, err)

	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(t, err)
	e.Authz, err = adapters.NewAuthority(std, reader)
	must(t, err)

	pins, err := adapters.NewPins(e.Repo)
	must(t, err)
	var sources execution.Sources = x.Sources
	var override execution.Sources
	if opt.Sources != nil {
		if e.realSources == nil {
			e.realSources = opt.Sources(e)
		}
		sources, override = e.realSources, e.realSources
	}
	candidates, err := adapters.NewCandidates(sources)
	must(t, err)
	var evidence gates.Evidence = e.Evidence
	if opt.WrapEvidence != nil {
		evidence = opt.WrapEvidence(evidence)
	}
	var pinPort gates.Pins = pins
	if opt.WrapPins != nil {
		pinPort = opt.WrapPins(pinPort)
	}
	var cand gates.Candidates = candidates
	if opt.WrapCandidates != nil {
		cand = opt.WrapCandidates(cand)
	}
	deps := gates.Dependencies{
		Store: e.Store, Authority: e.Authz, Runs: x.Store, Evidence: evidence, Pins: pinPort, Candidates: cand,
		Tenants: tenantList{ScopeA.TenantID, ScopeB.TenantID}, Clock: x.Clock.Now,
	}
	if !opt.NoDeployments {
		deps.Deployments = e.Deployments
	}
	if !opt.NoConformance {
		deps.Conformance = adapters.ConformanceMapping{ReportOutput: ReportOutput}
	}
	e.Gates, err = gates.NewService(deps, gates.Options{GateTTL: e.ttl, Batch: 10})
	must(t, err)

	e.Svc = x.NewService(executiontest.Options{
		Store: &testStore{Store: e.GateExec, env: e}, Hooks: execution.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent},
		Sources: override,
	})
}

// Restart rebuilds every service over the same durable records, as a new
// process would. Nothing is carried over in memory.
func (e *Env) Restart() { e.build() }

// SetGateTTL changes the deadline duration of gates requested from now on.
func (e *Env) SetGateTTL(d time.Duration) { e.ttl = d; e.build() }

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- identities ----

// GrantAll seeds the fixture users: a reviewer, an author who also reviews, an
// admin who is not a reviewer, an engineer and a viewer.
func (e *Env) GrantAll() {
	for _, s := range []domain.Scope{ScopeA, ScopeA2} {
		e.X.Grant(s, Reviewer, identity.RoleReviewer)
		e.X.Grant(s, Author, identity.RoleEngineer)
		e.X.Grant(s, Author, identity.RoleReviewer)
		e.X.Grant(s, Admin, identity.RoleAdmin)
		e.X.Grant(s, Engineer, identity.RoleEngineer)
		e.X.Grant(s, Viewer, identity.RoleViewer)
	}
	e.X.Grant(ScopeB, "u034_reviewer_b", identity.RoleReviewer)
}

// Caller builds a gate caller for a principal.
func Caller(principal string) gates.Caller {
	return gates.Caller{PrincipalID: principal, RequestID: "req_t034"}
}

// ---- definitions, runs ----

// Gate is the flow mapping of a gate step.
func Gate(id string, needs []string, evidence ...string) string {
	ev := ""
	for i, x := range evidence {
		if i > 0 {
			ev += ", "
		}
		ev += `"` + x + `"`
	}
	n := ""
	for i, x := range needs {
		if i > 0 {
			n += ", "
		}
		n += x
	}
	return fmt.Sprintf("{id: %s, kind: gate, needs: [%s], gate: {evidence: [%s]}}", id, n, ev)
}

// DeploymentGate is a gate bound to a deployment output reference.
func DeploymentGate(id string, needs []string, evidence []string, deployment string) string {
	g := Gate(id, needs, evidence...)
	return g[:len(g)-2] + `, deploymentId: "` + deployment + `"}}`
}

// Agent is the flow mapping of an agent step with a writer workspace.
func Agent(id string, extra ...string) string {
	s := "{id: " + id + ", kind: agent, agent: {profile: coding-v1}, workspace: {access: write, from: run}, timeoutSeconds: 60"
	for _, x := range extra {
		s += ", " + x
	}
	return s + "}"
}

// Define registers a definition from step flow mappings.
func (e *Env) Define(scope domain.Scope, steps ...string) pipeline.Definition {
	e.T.Helper()
	return e.X.Define(scope, pipelinetest.Doc(steps...))
}

// DefineDoc registers a definition from a full document.
func (e *Env) DefineDoc(scope domain.Scope, doc string) pipeline.Definition {
	e.T.Helper()
	return e.X.Define(scope, doc)
}

// Admit admits a work run through the gate-aware execution service.
func (e *Env) Admit(p executiontest.Project, def pipeline.Definition, inputs map[string]runtimev1.Value) (*execution.Run, error) {
	return e.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: CommitA, Inputs: inputs,
	})
}

// AdmitAt admits a work run pinned to the given source commit.
func (e *Env) AdmitAt(p executiontest.Project, def pipeline.Definition, commit string) (*execution.Run, error) {
	return e.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: commit,
	})
}

// MustAdmit admits or fails the test.
func (e *Env) MustAdmit(p executiontest.Project, def pipeline.Definition, inputs map[string]runtimev1.Value) *execution.Run {
	e.T.Helper()
	r, err := e.Admit(p, def, inputs)
	if err != nil {
		e.T.Fatalf("admit: %v", err)
	}
	return r
}

// Get loads the aggregate.
func (e *Env) Get(run *execution.Run) *execution.Aggregate {
	e.T.Helper()
	agg, err := e.X.Store.Get(context.Background(), run.Scope, run.ID)
	must(e.T, err)
	return agg
}

// Claim claims one runnable step through the gate-aware service.
func (e *Env) Claim(tenant domain.TenantID) (*execution.AttemptSpec, error) {
	return e.Svc.ClaimRunnable(context.Background(), tenant)
}

// MustClaim claims or fails the test.
func (e *Env) MustClaim(tenant domain.TenantID) *execution.AttemptSpec {
	e.T.Helper()
	in, err := e.Claim(tenant)
	if err != nil {
		e.T.Fatalf("claim: %v", err)
	}
	return in
}

// Start observes the command start of the attempt.
func (e *Env) Start(in *execution.AttemptSpec) {
	e.T.Helper()
	must(e.T, e.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, execution.Observation{
		Kind: execution.ObservedStarted, WorkloadName: in.WorkloadName, WorkloadUID: "11111111-2222-3333-4444-555555555555",
	}))
}

// Output is one artifact output of a finished step.
type Output struct {
	Name string
	Data []byte
	Role artifacts.Role
	// Provenance overrides the trusted source provenance recorded for the
	// artifact (the producing attempt's own source by default). A non-nil
	// pointer replaces the repository/commit, "" meaning none.
	Provenance *Provenance
}

// Provenance is an explicit source provenance for a fixture artifact.
type Provenance struct {
	Repository string
	Commit     string
}

// Finish starts the attempt, stores its artifact outputs, publishes the commit
// of a writer and commits a successful result. outputs maps an output name to
// the artifact content; scalars map extra outputs. commit is the commit a
// writer published ("" for a reader).
func (e *Env) Finish(in *execution.AttemptSpec, commit string, outputs ...Output) map[string]domain.ArtifactID {
	e.T.Helper()
	e.Start(in)
	ids := map[string]domain.ArtifactID{}
	refs := map[domain.ArtifactID]runtimev1.ArtifactRef{}
	values := map[string]runtimev1.Value{}
	for _, o := range outputs {
		role := o.Role
		if role == "" {
			role = artifacts.RoleOutput
		}
		id := e.artifact(in, o.Name, o.Data, role, o.Provenance)
		ids[o.Name] = id
		refs[id] = runtimev1.ArtifactRef{ID: id, Scope: in.Scope, Attempt: in.AttemptID, Kind: runtimev1.KindFile, Usable: true}
		values[o.Name] = runtimev1.StringValue(string(id))
	}
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, Outputs: values, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	zero := int64(0)
	res.ExitCode = &zero
	if in.Access == execution.AccessWrite {
		c := runtimev1.GitCommit(commit)
		res.SourceCommit = &c
		if commit != executiontest.CommitA {
			e.PublishWriter(in, commit)
		}
	}
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	_, err = e.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res}, Artifacts: refs,
	})
	if err != nil {
		e.T.Fatalf("commit result of %s: %v", in.StepID, err)
	}
	return ids
}

// StartAndSucceed starts the attempt and commits a successful result with the
// given scalar outputs and no artifacts.
func (e *Env) StartAndSucceed(in *execution.AttemptSpec, outputs map[string]runtimev1.Value) {
	e.T.Helper()
	e.Start(in)
	zero := int64(0)
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	if _, err := e.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
	}); err != nil {
		e.T.Fatalf("commit result of %s: %v", in.StepID, err)
	}
}

// PublishWriter records the writer's publication of commit in the workspace, as
// the workspace module's publisher does after a push, so a successful result
// naming the commit is accepted.
func (e *Env) PublishWriter(in *execution.AttemptSpec, commit string) {
	e.T.Helper()
	ctx := context.Background()
	bundle := e.Artifact(in, "bundle", []byte("bundle of "+commit), artifacts.RoleBundle)
	var ws, tip string
	e.X.Scalar(&ws, `SELECT workspace_id FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(in.Scope.TenantID), string(in.Scope.ProjectID), string(in.AttemptID))
	e.X.Scalar(&tip, `SELECT expected_tip FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(in.Scope.TenantID), string(in.Scope.ProjectID), string(in.AttemptID))
	_, err := e.X.Workspaces.Update(ctx, in.Scope, workspace.ID(ws), workspace.Load{Receipt: in.AttemptID}, func(a *workspace.Aggregate) (workspace.Effects, error) {
		now := e.X.Clock.Now()
		pub, _, err := a.Workspace.BeginPublication(workspace.Proposal{
			AttemptID: in.AttemptID, Commit: workspace.Commit(commit), ExpectedTip: workspace.Commit(tip), ArtifactID: bundle, BundleDigest: string(artifacts.DigestOf([]byte("bundle of " + commit))),
		}, nil, now)
		if err != nil {
			return workspace.Effects{}, err
		}
		if err := a.Workspace.ClaimPublisher("drv_t034", now, time.Minute); err != nil {
			return workspace.Effects{}, err
		}
		if _, err := a.Workspace.RecordPublished("drv_t034", pub, now); err != nil {
			return workspace.Effects{}, err
		}
		return workspace.Effects{Receipt: pub}, nil
	})
	must(e.T, err)
}

// Artifact stores a usable file artifact produced by the attempt: its metadata
// row (with the producing attempt's provenance) and its sealed bytes.
func (e *Env) Artifact(in *execution.AttemptSpec, name string, data []byte, role artifacts.Role) domain.ArtifactID {
	e.T.Helper()
	return e.artifact(in, name, data, role, nil)
}

func (e *Env) artifact(in *execution.AttemptSpec, name string, data []byte, role artifacts.Role, prov *Provenance) domain.ArtifactID {
	e.T.Helper()
	e.mu.Lock()
	e.seq++
	id := domain.ArtifactID(fmt.Sprintf("art_t034_%d", e.seq))
	e.mu.Unlock()
	var input, repo *string
	e.X.Scalar(&input, `SELECT input_commit FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(in.Scope.TenantID), string(in.Scope.ProjectID), string(in.AttemptID))
	// The provenance repository is the one of the producing attempt's own
	// workspace (the source it worked from), as the artifact module records it.
	e.X.Scalar(&repo, `SELECT w.repository_url FROM step_attempts a LEFT JOIN workspaces w ON w.tenant_id = a.tenant_id AND w.project_id = a.project_id AND w.id = a.workspace_id
		WHERE a.tenant_id = $1 AND a.project_id = $2 AND a.id = $3`, string(in.Scope.TenantID), string(in.Scope.ProjectID), string(in.AttemptID))
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	nonce := fmt.Sprintf("n%d", e.seq)
	sealed := artifacts.ObjectKey(in.Scope, id, artifacts.SegmentSealed, nonce)
	staging := artifacts.ObjectKey(in.Scope, id, artifacts.SegmentStaging, nonce)
	var url, commit *string
	if input != nil && repo != nil {
		url, commit = repo, input
	}
	if prov != nil {
		url, commit = nil, nil
		if prov.Repository != "" {
			r := prov.Repository
			url = &r
		}
		if prov.Commit != "" {
			c := prov.Commit
			commit = &c
		}
	}
	e.X.OwnerExec(`INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes,
		source_repository_url, source_commit, state, disposition, staging_key, sealed_key, transfer_expires_at, verified_at, verified_size_bytes, created_at, updated_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'file',$8,$9,'text/plain',$10,$11,$12,'usable','active',$13,$14, now(), now(), $10, now(), now(), 2)`,
		string(in.Scope.TenantID), string(in.Scope.ProjectID), string(id), string(in.RunID), string(in.AttemptID), in.StepID, name, string(role), digest, int64(len(data)),
		url, commit, staging, sealed)
	e.PutObject(sealed, data)
	return id
}

// PutObject writes content under a sealed key in the configured object store.
func (e *Env) PutObject(key string, data []byte) {
	e.T.Helper()
	err := e.Objects.Put(context.Background(), artifacts.PutSpec{Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), Digest: artifacts.DigestOf(data), MediaType: "text/plain"})
	must(e.T, err)
}

// SealedKey returns the sealed object key of an artifact.
func (e *Env) SealedKey(id domain.ArtifactID) string {
	var k string
	e.X.Scalar(&k, `SELECT sealed_key FROM artifacts WHERE id = $1`, string(id))
	return k
}

// ---- baseline injection ----

// SetBaseline sets the design baseline pinned into the context of every agent
// attempt claimed from now on, as the agents planner pins the frozen profile's
// baseline. nil pins none.
func (e *Env) SetBaseline(b *runtimev1.DesignBaseline) {
	e.mu.Lock()
	e.baseline = b
	e.mu.Unlock()
}

// Mu returns the lock guarding the injected baseline (test helpers only).
func (e *Env) Mu() *sync.Mutex { return &e.mu }

// Baseline returns the injected baseline; hold Mu.
func (e *Env) Baseline() *runtimev1.DesignBaseline { return e.baseline }

// NewBaseline builds a valid baseline with the required checks.
func NewBaseline(t testing.TB, checks ...string) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline([]string{"SD-01", "DQ-14"}, []string{"C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.test/acme/design.git", Revision: CommitA, Path: "docs/specs/software-design.md"}}, checks)
	must(t, err)
	return b
}

// Report builds a conformance report. Checks all pass unless mutated.
func Report(base runtimev1.DesignBaseline, candidate string, checks ...runtimev1.ConformanceCheck) runtimev1.ConformanceReport {
	r := runtimev1.ConformanceReport{
		BaselineDigest: base.Digest, CandidateCommit: runtimev1.GitCommit(candidate), Requirements: []string{"SD-01"},
		Checks: checks, UnresolvedDeviations: []runtimev1.Deviation{}, Disposition: runtimev1.Conforms,
	}
	for _, c := range checks {
		if c.Outcome != runtimev1.CheckPassed {
			r.Disposition = runtimev1.DispBlocked
		}
	}
	return r
}

// Marshal renders a report as the artifact payload.
func Marshal(t testing.TB, r runtimev1.ConformanceReport) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	must(t, err)
	return b
}

// testStore expands agent steps at admission and pins the design baseline into
// agent attempts, standing in for the agents module's resolver and planner.
type testStore struct {
	execution.Store
	env *Env
}

type fakeResolver struct{}

func (fakeResolver) ResolveAgent(context.Context, domain.Scope, pipeline.Step) (execution.AgentExpansion, error) {
	return execution.AgentExpansion{Image: pipelinetest.Image, Command: []string{"/bin/true"}}, nil
}

func (s *testStore) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	expanded, err := execution.ExpandAgents(ctx, fakeResolver{}, p)
	if err != nil {
		return nil, false, err
	}
	return s.Store.Admit(ctx, expanded)
}

func (s *testStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	return s.Store.Update(ctx, scope, id, opts, func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		fx, err := fn(ctx, env, agg)
		if err != nil {
			return fx, err
		}
		s.env.mu.Lock()
		b := s.env.baseline
		s.env.mu.Unlock()
		for _, at := range agg.Created {
			step := agg.Step(at.StepID)
			if b == nil || step == nil || step.Spec.Kind != pipeline.KindAgent {
				continue
			}
			var c runtimev1.Context
			if err := json.Unmarshal(at.Context, &c); err != nil {
				return execution.Effects{}, err
			}
			base := *b
			c.DesignBaseline = &base
			raw, err := json.Marshal(c)
			if err != nil {
				return execution.Effects{}, err
			}
			canonical, err := runtimev1.Canonicalize(raw)
			if err != nil {
				return execution.Effects{}, err
			}
			sum := sha256.Sum256(canonical)
			at.Context, at.ContextDigest = canonical, "sha256:"+hex.EncodeToString(sum[:])
		}
		return fx, nil
	})
}

// ---- doubles ----

// FakeDeployments is a controlled fixture of the gate-owned deployment port,
// not the real joined path to the implemented deployment module (T-035); that
// binding is qualified by T-073. It answers with the
// scope, candidate and configuration digest a test records for a deployment id,
// without filtering by the requested scope, so tests can exercise the gate
// service's own ownership and candidate checks against a misbehaving adapter.
type FakeDeployments struct {
	mu      sync.Mutex
	entries map[string]gates.DeploymentSnapshot
	err     error
	Calls   int
}

// NewFakeDeployments returns an empty fixture.
func NewFakeDeployments() *FakeDeployments {
	return &FakeDeployments{entries: map[string]gates.DeploymentSnapshot{}}
}

// Set records a deployment of scope that records candidate and has the
// configuration digest.
func (f *FakeDeployments) Set(scope domain.Scope, id, candidate, configDigest string) {
	f.mu.Lock()
	f.entries[id] = gates.DeploymentSnapshot{ID: id, ConfigDigest: configDigest, Scope: scope, Candidate: candidate}
	f.mu.Unlock()
}

// Remove makes the deployment unknown.
func (f *FakeDeployments) Remove(id string) {
	f.mu.Lock()
	delete(f.entries, id)
	f.mu.Unlock()
}

// Fail makes every call fail with err (an outage); nil restores service.
func (f *FakeDeployments) Fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// Snapshot implements gates.Deployments.
func (f *FakeDeployments) Snapshot(_ context.Context, _ domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.err != nil {
		return gates.DeploymentSnapshot{}, f.err
	}
	snap, ok := f.entries[id]
	if !ok {
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	}
	return snap, nil
}

// MemObjects is an in-memory artifacts.ObjectStore (Open, Put, Delete only).
type MemObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemObjects returns an empty store.
func NewMemObjects() *MemObjects { return &MemObjects{data: map[string][]byte{}} }

// PresignUpload implements artifacts.ObjectStore (unused).
func (m *MemObjects) PresignUpload(context.Context, artifacts.UploadSpec) (artifacts.Transfer, error) {
	return artifacts.Transfer{}, fmt.Errorf("not supported")
}

// PresignDownload implements artifacts.ObjectStore (unused).
func (m *MemObjects) PresignDownload(context.Context, artifacts.DownloadSpec) (artifacts.Transfer, error) {
	return artifacts.Transfer{}, fmt.Errorf("not supported")
}

// Open implements artifacts.ObjectStore.
func (m *MemObjects) Open(_ context.Context, key string) (artifacts.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return artifacts.Object{}, artifacts.ErrObjectNotFound
	}
	return artifacts.Object{Body: nopCloser{bytes.NewReader(append([]byte(nil), b...))}, Size: int64(len(b))}, nil
}

// Put implements artifacts.ObjectStore.
func (m *MemObjects) Put(_ context.Context, spec artifacts.PutSpec) error {
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(spec.Body); err != nil {
		return err
	}
	m.mu.Lock()
	m.data[spec.Key] = buf.Bytes()
	m.mu.Unlock()
	return nil
}

// Delete implements artifacts.ObjectStore.
func (m *MemObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
