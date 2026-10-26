// Package executiontest holds the fixtures of the execution module's tests: an
// isolated PostgreSQL database migrated with every module the execution tables
// depend on, the real PostgreSQL store, secrets, pipeline, identity and
// workspace tables, and controlled doubles only for the external seams
// (runtime, effect reconciler, Git observation). It contains no production
// behavior.
//
// The runtime is a runtimetest.Decorator over runtimetest.Manual: these
// suites exercise persistence and transactions and script each workload's
// observations themselves. Lifecycle tests pass a real runtime instead.
//
// Resource prefix t020: tenants, projects, work items and workspaces carry it.
package executiontest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	identity "github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// Prefix is the resource prefix of this task's test resources.
const Prefix = "t020"

var (
	ScopeA  = domain.Scope{TenantID: "t020_acme", ProjectID: "p020_inventory"}
	ScopeA2 = domain.Scope{TenantID: "t020_acme", ProjectID: "p020_billing"}
	ScopeB  = domain.Scope{TenantID: "t020_globex", ProjectID: "p020_ops"}
)

// Users. Engineers hold run:manage and run:read; viewers only run:read.
const (
	Engineer = "u020_engineer"
	Viewer   = "u020_viewer"
	Stranger = "u020_stranger"
	OtherEng = "u020_other_tenant"
)

// Commits are fixture commit identifiers (no Git repository behind them).
const (
	CommitA = "1111111111111111111111111111111111111111"
	CommitB = "2222222222222222222222222222222222222222"
	CommitC = "3333333333333333333333333333333333333333"
)

// Clock is a controllable clock: real time plus an adjustable offset, so rows
// stamped with the database clock and Go-side decisions stay comparable.
type Clock = eventstest.Clock

// Env is one fully wired execution module over real PostgreSQL.
type Env struct {
	T testing.TB

	Base      *eventstest.Env
	Migration *pgxpool.Pool
	Runtime   *pgxpool.Pool

	Events     *eventspg.Store
	Workspaces *workspacepg.Store
	Secrets    *secretspg.Store
	SecretSvc  *secrets.Service
	Pipeline   *pipelinepg.Store
	Idem       *httppg.Idempotency
	Store      *execpg.Store
	Keyed      *execpg.Keyed
	Svc        *execution.Service

	Clock *Clock
	// Workloads records and faults the runtime calls; Manual is the inner
	// runtime unless Options.Runtime replaced it.
	Workloads *runtimetest.Decorator
	Manual    *runtimetest.Manual
	Effects   *FakeEffects
	Sources   *FakeSources
	Limits    *MutableLimits
	Validator *pipeline.Validator

	keys *keyring
	seq  atomic.Int64
}

// Options tune the fixture. Zero values select the M1 wiring.
type Options struct {
	// Hooks defaults to containers only.
	Hooks execution.Hooks
	// NoRuntime leaves the runtime port unset.
	NoRuntime bool
	// Runtime replaces the Manual runtime inside Workloads in New; in
	// NewService it is the service's runtime, undecorated.
	Runtime execution.Runtime
	// Requirements overrides the admission requirements policy.
	Requirements execution.Requirements
	// NoEffects leaves the effect reconciler unset.
	NoEffects bool
	// Cleanup enables the deployment-bound cleanup authorization.
	Cleanup execution.DeploymentCleanup
	// Environments enables the admission environment-snapshot seam.
	Environments execution.EnvironmentResolver
	// Limits overrides the default policy.
	Limits *execution.Limit
	// Events overrides the event store options.
	Events *eventspg.Options
	// WrapAppender inserts fault injection before events are appended.
	WrapAppender func(execpgAppender) execpgAppender
	// ServiceOptions override the service options.
	ServiceOptions *execution.Options
	// Kinds are the step kinds admission accepts (default: all three; the
	// definition validator always accepts them so tests can register them).
	Kinds []pipeline.StepKind
	// Sources replaces the fake workspace sources (real Git adapters).
	Sources execution.Sources
	// Store replaces the PostgreSQL store of the service (fault injection or a
	// store built over different peers).
	Store execution.Store
	// Shared is the explicit run_shared opt-in (a test-injected fake port and
	// policy); nil keeps new-format admission closed.
	Shared *execution.SharedConfig
	// SourceWrap wraps the Git observation seam (a call counter).
	SourceWrap func(execution.Sources) execution.Sources
	// LegacySeed builds an UPGRADE fixture: the database holds the schema of the
	// release before T112_001 while it runs, then the additive migration applies.
	LegacySeed func(e *Env)
}

type execpgAppender = execpg.EventAppender

// DefaultLimit is a short, explicit test policy, not a production value.
func DefaultLimit() execution.Limit {
	return execution.Limit{
		TenantConcurrency: 10, ProjectConcurrency: 5, StartupBudget: time.Minute,
		MaxOutputBytes: 1 << 20, MaxOutputFiles: 100, MaxDiagnosticBytes: 1 << 20,
		DefaultTimeout: 10 * time.Minute, MaxTimeout: time.Hour,
	}
}

// New builds the environment. PostgreSQL follows test/support/postgres: the test
// skips when no administrator URL is configured and fails instead when
// KODAMA_REQUIRE_POSTGRES=1.
func New(t testing.TB, o ...Options) *Env {
	t.Helper()
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	base := eventstest.NewDB(t, 60)
	e := &Env{T: t, Base: base, Migration: base.Migration, Runtime: base.Runtime, Clock: &Clock{}}
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		if s == ScopeA2 {
			continue
		}
		base.SeedTenant(t, string(s.TenantID), string(s.ProjectID))
	}
	e.ownerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(ScopeA2.TenantID), string(ScopeA2.ProjectID))

	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	execContribs := execpg.Migrations()
	if opt.LegacySeed != nil {
		// Upgrade fixture: the database first holds exactly what a release before the
		// run_shared schema (T112_001) deployed; historical rows are seeded and only
		// then is the additive migration applied.
		execContribs = execContribs[:len(execContribs)-1]
	}
	manifest, err := storage.NewManifest(append(contribs, execContribs...)...)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, base.Migration, manifest, storage.MigrateOptions{RuntimeRole: base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	upgrade := func() {
		if opt.LegacySeed == nil {
			return
		}
		opt.LegacySeed(e)
		full, err := storage.NewManifest(append(contribs, execpg.Migrations()...)...)
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		res, err := storage.Migrate(ctx, base.Migration, full, storage.MigrateOptions{RuntimeRole: base.DB.RuntimeRole})
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != execpg.RunResourcesMigrationID {
			t.Fatalf("the upgrade must apply only the run_shared migration: %+v %v", res, err)
		}
	}

	evOpts := eventstest.DefaultOptions()
	if opt.Events != nil {
		evOpts = *opt.Events
	}
	if evOpts.Clock == nil {
		evOpts.Clock = e.Clock.Now // one controllable clock for state, events and deadlines
	}
	e.Events = base.NewStore(t, evOpts)
	e.Workspaces, err = workspacepg.NewStore(e.Runtime, e.Events, workspacepg.Options{})
	must(t, err)
	e.keys = newKeyring(t)
	e.keys.add("v1")
	provider := e.keys.provider("v1")
	e.Secrets, err = secretspg.NewStore(secretspg.Config{DB: e.Runtime, Events: secretspg.EventsFrom(e.Events), IDs: secrets.RandomIDs{}, Clock: e.Clock.Now})
	must(t, err)
	e.SecretSvc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: "inst-t020", Store: e.Secrets, Wrapper: provider, IDs: secrets.RandomIDs{}})
	must(t, err)
	e.Pipeline, err = pipelinepg.NewStore(e.Runtime, pipelinepg.Options{})
	must(t, err)
	e.Idem, err = httppg.NewIdempotency(httppg.Config{DB: e.Runtime})
	must(t, err)
	e.Validator = pipelinetest.NewValidator(t, pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate)

	var appender execpg.EventAppender = e.Events
	if opt.WrapAppender != nil {
		appender = opt.WrapAppender(appender)
	}
	e.Store, err = execpg.NewStore(e.Runtime, appender, execpg.Peers{
		Authority: adapters.BindAuthority, Workspaces: e.Workspaces, Secrets: e.Secrets,
	}, execpg.Options{Clock: e.Clock.Now})
	must(t, err)
	e.Keyed, err = execpg.NewKeyed(e.Idem, e.Store)
	must(t, err)

	e.Manual, e.Effects, e.Sources = runtimetest.NewManual(), &FakeEffects{}, NewFakeSources(e)
	var inner execution.Runtime = e.Manual
	if opt.Runtime != nil {
		inner = opt.Runtime
	}
	e.Workloads = runtimetest.Decorate(inner)
	lim := DefaultLimit()
	if opt.Limits != nil {
		lim = *opt.Limits
	}
	e.Limits = &MutableLimits{lim: lim}
	svcOpt := opt
	svcOpt.Runtime = nil
	e.Svc = e.NewService(svcOpt)
	upgrade()
	return e
}

// NewService builds another Service over the same ports and store, as a second
// control-plane process (or the same one after a restart) would be.
func (e *Env) NewService(opt Options) *execution.Service {
	e.T.Helper()
	authority, err := e.authority()
	must(e.T, err)
	cfg, err := e.configuration()
	must(e.T, err)
	hooks := opt.Hooks
	if hooks == nil {
		hooks = execution.EnabledKinds{pipeline.KindContainer}
	}
	kinds := pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}
	if opt.Kinds != nil {
		kinds = pipeline.EnabledKinds(opt.Kinds)
	}
	defs, err := adapters.NewDefinitions(e.Pipeline)
	must(e.T, err)
	var sources execution.Sources = e.Sources
	if opt.Sources != nil {
		sources = opt.Sources
	}
	var store execution.Store = e.Store
	if opt.Store != nil {
		store = opt.Store
	}
	if opt.SourceWrap != nil {
		sources = opt.SourceWrap(sources)
	}
	deps := execution.Dependencies{
		Store: store, Authority: authority, Definitions: defs, Capabilities: kinds,
		Work: SQLWork{e}, Configuration: cfg, Sources: sources, Limits: e.Limits, Hooks: hooks,
		Tenants: tenantList{ScopeA.TenantID, ScopeB.TenantID}, Cleanup: opt.Cleanup, Clock: e.Clock.Now, CancelledWork: SQLCancelled{e},
		Requirements: opt.Requirements, Shared: opt.Shared,
	}
	switch {
	case opt.NoRuntime:
	case opt.Runtime != nil:
		deps.Runtime = opt.Runtime
	default:
		deps.Runtime = e.Workloads
	}
	if !opt.NoEffects {
		deps.EffectReconciler = e.Effects
	}
	so := execution.Options{ClaimLease: 2 * time.Second, Batch: 10}
	if opt.ServiceOptions != nil {
		so = *opt.ServiceOptions
	}
	var svc *execution.Service
	if opt.Environments != nil {
		svc, err = execution.NewServiceWithEnvironments(deps, so, opt.Environments)
	} else {
		svc, err = execution.NewService(deps, so)
	}
	must(e.T, err)
	return svc
}

func (e *Env) authority() (execution.Authority, error) {
	reader, err := identitypg.NewStore(e.Runtime, e.Events)
	if err != nil {
		return nil, err
	}
	std, err := identity.NewStandaloneAuthority(reader)
	if err != nil {
		return nil, err
	}
	return adapters.NewAuthority(std, reader)
}

func (e *Env) configuration() (execution.Configuration, error) {
	r, err := secrets.NewConfigurationResolver(e.Secrets, e.Secrets, nil)
	if err != nil {
		return nil, err
	}
	return adapters.NewConfiguration(r)
}

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ID returns a fresh, valid, readable identifier carrying the task prefix.
func (e *Env) ID(prefix string) string { return fmt.Sprintf("%s_t020_%d", prefix, e.seq.Add(1)) }

func (e *Env) ownerExec(sql string, args ...any) {
	e.T.Helper()
	if _, err := e.Migration.Exec(context.Background(), sql, args...); err != nil {
		e.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// OwnerExec runs SQL as the owner role (assertions and fault setup).
func (e *Env) OwnerExec(sql string, args ...any) { e.T.Helper(); e.ownerExec(sql, args...) }

// OwnerExecErr is OwnerExec returning the error.
func (e *Env) OwnerExecErr(sql string, args ...any) error {
	_, err := e.Migration.Exec(context.Background(), sql, args...)
	return err
}

// Count runs a count query as the owner (the owner policies see every tenant).
func (e *Env) Count(sql string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Migration.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Scalar runs a single-value query as the owner.
func (e *Env) Scalar(dst any, sql string, args ...any) {
	e.T.Helper()
	if err := e.Migration.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		e.T.Fatalf("scalar: %v\n%s", err, sql)
	}
}

// RuntimeTx runs fn in a tenant transaction as the application sees data.
func (e *Env) RuntimeTx(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.Runtime, tenant, fn)
}

// ---- seeding ----

// Grant makes the user a member of the project with the role, as a binding row
// plus the authorization revision row every permission-sensitive transaction
// locks. The user is created if needed.
func (e *Env) Grant(scope domain.Scope, user string, role identity.Role) {
	e.T.Helper()
	e.ownerExec(`INSERT INTO users (id, issuer, subject) VALUES ($1::text, 'https://issuer.invalid', $1::text) ON CONFLICT DO NOTHING`, user)
	e.ownerExec(`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ($1::text, $2::text, $3::text, $4::text, $5::text)`,
		string(scope.TenantID), e.ID("mem"), user, string(scope.ProjectID), string(role))
	e.ownerExec(`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ($1::text, $2::text, 1)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET revision = identity_authz_revisions.revision + 1`, string(scope.TenantID), user)
}

// Revoke removes the user's bindings in the tenant and bumps the revision, as
// the identity module's revocation transaction does.
func (e *Env) Revoke(scope domain.Scope, user string) {
	e.T.Helper()
	e.ownerExec(`UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`, string(scope.TenantID), user)
	e.ownerExec(`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, string(scope.TenantID), user)
}

// GrantAll seeds the standard users in every fixture scope.
func (e *Env) GrantAll() {
	for _, s := range []domain.Scope{ScopeA, ScopeA2} {
		e.Grant(s, Engineer, identity.RoleEngineer)
		e.Grant(s, Viewer, identity.RoleViewer)
	}
	e.Grant(ScopeB, OtherEng, identity.RoleEngineer)
}

// WorkItem seeds an actionable story with a specification and criteria.
func (e *Env) WorkItem(scope domain.Scope, id string, spec string) domain.WorkItemID {
	e.T.Helper()
	e.ownerExec(`INSERT INTO work_items (tenant_id, project_id, id, kind, title, spec, acceptance_criteria, state, created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1::text, $2::text, $3::text, 'story', 'Fixture', $4, '[{"id":"AC-1","text":"works"}]', 'active', 'user', 'u020_engineer', now(), now())`,
		string(scope.TenantID), string(scope.ProjectID), id, spec)
	return domain.WorkItemID(id)
}

// EditSpec changes the item's specification (content revision + 1).
func (e *Env) EditSpec(scope domain.Scope, id domain.WorkItemID, spec string) {
	e.T.Helper()
	e.ownerExec(`UPDATE work_items SET spec = $4, content_revision = content_revision + 1, version = version + 1
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(scope.TenantID), string(scope.ProjectID), string(id), spec)
}

// CancelWork cancels the item as the work module's cancellation transaction
// does: its row is updated (taking the lock admission shares) and its state is
// the durable cancellation intent.
func (e *Env) CancelWork(scope domain.Scope, id domain.WorkItemID) {
	e.T.Helper()
	e.ownerExec(`UPDATE work_items SET state = 'cancelled', version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
}

// Workspace seeds an available workspace of the item whose accepted commit is
// CommitA. No Git repository stands behind it.
func (e *Env) Workspace(scope domain.Scope, item domain.WorkItemID) workspace.ID {
	e.T.Helper()
	id := e.ID("ws")
	e.ownerExec(`INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref,
		work_branch, initial_commit, accepted_commit, state, version, fence_epoch, publication_fenced, created_at, updated_at)
		VALUES ($1::text,$2::text,$3::text,$4::text,'repo_t020','https://git.example.invalid/t020.git','secret://t020/checkout','secret://t020/publisher',
		$5, $6, $6, 'available', 1, 1, false, now(), now())`,
		string(scope.TenantID), string(scope.ProjectID), id, string(item), workspace.WorkBranch(item), CommitA)
	return workspace.ID(id)
}

// Define registers a validated definition (YAML) and returns it.
func (e *Env) Define(scope domain.Scope, yaml string) pipeline.Definition {
	e.T.Helper()
	actor := pipeline.Actor{ID: Engineer, Type: pipeline.ActorUser}
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

// Project is a work item with its workspace, ready to run.
type Project struct {
	Scope     domain.Scope
	Item      domain.WorkItemID
	Workspace workspace.ID
}

// NewProject seeds a work item and its workspace in the scope.
func (e *Env) NewProject(scope domain.Scope) Project {
	e.T.Helper()
	item := e.WorkItem(scope, e.ID("wi"), "# Plan\nDo the thing.")
	return Project{Scope: scope, Item: item, Workspace: e.Workspace(scope, item)}
}

// Admit admits a work run of the project as the engineer.
func (e *Env) Admit(p Project, def pipeline.Definition, inputs map[string]runtimev1.Value) (*execution.Run, error) {
	return e.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t020"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(p), SourceCommit: CommitA, Inputs: inputs,
	})
}

// MustAdmit is Admit failing the test on error.
func (e *Env) MustAdmit(p Project, def pipeline.Definition, inputs map[string]runtimev1.Value) *execution.Run {
	e.T.Helper()
	r, err := e.Admit(p, def, inputs)
	if err != nil {
		e.T.Fatalf("admit: %v", err)
	}
	return r
}

// ContentRevision reads the item's current content revision.
func (e *Env) ContentRevision(p Project) domain.ContentRevision {
	var r int64
	e.Scalar(&r, `SELECT content_revision FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(p.Scope.TenantID), string(p.Scope.ProjectID), string(p.Item))
	return domain.ContentRevision(r)
}

// Get loads the aggregate (no authorization; fixture inspection).
func (e *Env) Get(run *execution.Run) *execution.Aggregate {
	e.T.Helper()
	agg, err := e.Store.Get(context.Background(), run.Scope, run.ID)
	must(e.T, err)
	return agg
}

// Claim claims one runnable step of the tenant.
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

// Observe applies an observation.
func (e *Env) Observe(in *execution.AttemptSpec, obs execution.Observation) error {
	if obs.WorkloadName == "" {
		obs.WorkloadName = in.WorkloadName
	}
	return e.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, obs)
}

// Start observes the command start of the attempt.
func (e *Env) Start(in *execution.AttemptSpec) {
	e.T.Helper()
	must(e.T, e.Observe(in, execution.Observation{Kind: execution.ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}))
}

// Succeed submits a completed result for the attempt (declared outputs given).
func (e *Env) Succeed(in *execution.AttemptSpec, outputs map[string]runtimev1.Value, commit string) (execution.ResultAck, error) {
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	if outputs == nil {
		res.Outputs = map[string]runtimev1.Value{}
	}
	if commit != "" {
		c := runtimev1.GitCommit(commit)
		res.SourceCommit = &c
	}
	return e.Commit(in, res)
}

// Fail submits a failed result.
func (e *Env) Fail(in *execution.AttemptSpec) (execution.ResultAck, error) {
	code := int64(1)
	reason := "command_failed"
	return e.Commit(in, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
}

// Commit submits a result.
func (e *Env) Commit(in *execution.AttemptSpec, res runtimev1.Result) (execution.ResultAck, error) {
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	return e.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
	})
}

func ptr[T any](v T) *T { return &v }

// Terminate confirms the workload of the attempt is gone with launch evidence.
func (e *Env) Terminate(in *execution.AttemptSpec, launch execution.LaunchEvidence) error {
	return e.Observe(in, execution.Observation{Kind: execution.ObservedTerminated, Launch: launch})
}

// ---- secrets ----

// Secret creates a secret with its first value and returns it.
func (e *Env) Secret(scope domain.Scope, name, value string) secrets.Secret {
	e.T.Helper()
	ctx := context.Background()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Engineer}, RequestID: "req_t020", Now: e.Clock.Now()}
	p, err := e.SecretSvc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte(value))
	must(e.T, err)
	var out secrets.Secret
	must(e.T, e.SecretSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.SecretSvc.CreateSecret(ctx, w, m, p)
		return err
	}))
	return out
}

// RevokeSecret revokes one secret version.
func (e *Env) RevokeSecret(scope domain.Scope, id string, version int64) error {
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Engineer}, RequestID: "req_t020", Now: e.Clock.Now()}
	return e.SecretSvc.Atomically(context.Background(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := e.SecretSvc.RevokeVersion(ctx, w, m, scope, id, version)
		return err
	})
}

// Variable creates a nonsecret project variable.
func (e *Env) Variable(scope domain.Scope, name, value string) {
	e.T.Helper()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Engineer}, RequestID: "req_t020", Now: e.Clock.Now()}
	must(e.T, e.SecretSvc.Atomically(context.Background(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := e.SecretSvc.CreateVariable(ctx, w, m, secrets.CreateVariableInput{Scope: scope, Name: name, Value: runtimev1.StringValue(value)})
		return err
	}))
}

type keyring struct {
	t     testing.TB
	dir   string
	files map[string]string
}

func newKeyring(t testing.TB) *keyring {
	return &keyring{t: t, dir: t.TempDir(), files: map[string]string{}}
}

func (k *keyring) add(version string) {
	key := make([]byte, keyfile.KeySize)
	if _, err := rand.Read(key); err != nil {
		k.t.Fatal(err)
	}
	p := filepath.Join(k.dir, "master-key-"+version)
	if err := os.WriteFile(p, key, 0o600); err != nil {
		k.t.Fatal(err)
	}
	k.files[version] = p
}

func (k *keyring) provider(current string) *keyfile.Provider {
	p, err := keyfile.New(keyfile.Config{Development: true, Current: current, Files: k.files})
	if err != nil {
		k.t.Fatal(err)
	}
	return p
}

// ---- doubles ----

// MutableLimits is an adjustable execution policy.
type MutableLimits struct {
	mu  sync.Mutex
	lim execution.Limit
	err error
}

// Set replaces the limit.
func (m *MutableLimits) Set(l execution.Limit) { m.mu.Lock(); m.lim = l; m.mu.Unlock() }

// Fail makes the limits unavailable (missing limits deny dispatch).
func (m *MutableLimits) Fail(err error) { m.mu.Lock(); m.err = err; m.mu.Unlock() }

// Limits implements execution.Limits.
func (m *MutableLimits) Limits(context.Context, domain.Scope) (execution.Limit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lim, m.err
}

// FakeEffects is the trusted effect reconciler double.
type FakeEffects struct {
	mu      sync.Mutex
	Verdict execution.Verdict
	Err     error
	Queries []execution.EffectQuery
}

// Set selects the verdict returned from now on.
func (f *FakeEffects) Set(v execution.Verdict, err error) {
	f.mu.Lock()
	f.Verdict, f.Err = v, err
	f.mu.Unlock()
}

// Reconcile implements execution.EffectReconciler.
func (f *FakeEffects) Reconcile(_ context.Context, q execution.EffectQuery) (execution.ReconciliationEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Queries = append(f.Queries, q)
	if f.Err != nil {
		return execution.ReconciliationEvidence{}, f.Err
	}
	v := f.Verdict
	if v == "" {
		v = execution.VerdictStillUnknown
	}
	return execution.ReconciliationEvidence{Verdict: v, Source: "t020-effects"}, nil
}

// FakeSources observes the seeded workspaces without Git. Retention confirms
// the pin through the real workspace store.
type FakeSources struct {
	e  *Env
	mu sync.Mutex
	// History lists commits accepted as workspace history besides the accepted
	// commit.
	History   map[string]bool
	Tip       string
	RetainErr error
	// VerifyErr makes source verification fail (a Git outage).
	VerifyErr error
	Retains   int
}

// NewFakeSources returns sources accepting CommitA and CommitB as history.
func NewFakeSources(e *Env) *FakeSources {
	return &FakeSources{e: e, History: map[string]bool{CommitA: true, CommitB: true}, Tip: CommitA}
}

func (f *FakeSources) info(scope domain.Scope, ws string) (execution.SourceInfo, error) {
	var url, branch string
	var tip *string
	err := f.e.Migration.QueryRow(context.Background(), `SELECT repository_url, work_branch, accepted_commit FROM workspaces WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), ws).Scan(&url, &branch, &tip)
	if err != nil {
		return execution.SourceInfo{}, execution.ErrNotFound
	}
	return execution.SourceInfo{WorkspaceID: ws, RepositoryURL: url, Branch: branch, AcceptedTip: *tip}, nil
}

// Verify implements execution.Sources.
func (f *FakeSources) Verify(_ context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (execution.SourceInfo, error) {
	f.mu.Lock()
	verr := f.VerifyErr
	f.mu.Unlock()
	if verr != nil {
		return execution.SourceInfo{}, verr
	}
	var ws string
	err := f.e.Migration.QueryRow(context.Background(), `SELECT id FROM workspaces WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(item)).Scan(&ws)
	if err != nil {
		return execution.SourceInfo{}, execution.ErrNotFound
	}
	f.mu.Lock()
	ok := f.History[commit]
	f.mu.Unlock()
	if !ok {
		return execution.SourceInfo{}, execution.Refusedf("the source commit is not in the workspace history")
	}
	return f.info(scope, ws)
}

// Describe implements execution.Sources. A refresh reports the tip the test set.
func (f *FakeSources) Describe(_ context.Context, scope domain.Scope, ws string, refresh bool) (execution.SourceInfo, error) {
	info, err := f.info(scope, ws)
	if err != nil {
		return info, err
	}
	if refresh {
		f.mu.Lock()
		info.AcceptedTip = f.Tip
		f.mu.Unlock()
	}
	return info, nil
}

// Retain implements execution.Sources: it confirms the run's pin through the
// workspace entities, as the workspace service's confirmation does.
func (f *FakeSources) Retain(ctx context.Context, scope domain.Scope, ws string, run domain.RunID, commit string) (string, error) {
	f.mu.Lock()
	f.Retains++
	err := f.RetainErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	var pinID string
	_, uerr := f.e.Workspaces.Update(ctx, scope, workspace.ID(ws), workspace.Load{Pins: true}, func(a *workspace.Aggregate) (workspace.Effects, error) {
		for _, p := range a.Pins {
			if p.RunID == run && string(p.Commit) == commit && p.Live() {
				pinID = string(p.ID)
				if p.Status == workspace.PinIntent {
					if err := p.Confirm(f.e.Clock.Now()); err != nil {
						return workspace.Effects{}, err
					}
					return workspace.Effects{Pins: []*workspace.RetainedRef{p}}, nil
				}
				return workspace.Effects{}, nil
			}
		}
		return workspace.Effects{}, workspace.ErrNotFound
	})
	if uerr != nil {
		return "", execution.Unavailablef(uerr, "retention failed")
	}
	return pinID, nil
}

// SQLWork reads work items directly (the work module's repository needs its
// own service wiring, which these tests do not exercise).
type SQLWork struct{ e *Env }

// Snapshot implements execution.WorkContent.
func (w SQLWork) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
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

// CountTenant runs a count query through the runtime role inside a tenant
// transaction, as the application sees the data (forced RLS applies). Event
// tables only admit tenant-scoped reads.
func (e *Env) CountTenant(tenant domain.TenantID, sql string, args ...any) int {
	e.T.Helper()
	var n int
	err := e.RuntimeTx(tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		e.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// OwnerExecRows runs SQL as the owner role and returns the rows it changed.
func (e *Env) OwnerExecRows(sql string, args ...any) (int64, error) {
	tag, err := e.Migration.Exec(context.Background(), sql, args...)
	return tag.RowsAffected(), err
}

// RotateSecret adds a new value version to the secret.
func (e *Env) RotateSecret(scope domain.Scope, id string, value string) {
	e.T.Helper()
	ctx := context.Background()
	cur, err := e.Secrets.GetSecret(ctx, scope, id)
	must(e.T, err)
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Engineer}, RequestID: "req_t020", Now: e.Clock.Now()}
	p, err := e.SecretSvc.PrepareVersion(ctx, m, scope, id, cur.Version(), []byte(value))
	must(e.T, err)
	must(e.T, e.SecretSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := e.SecretSvc.AddVersion(ctx, w, m, p)
		return err
	}))
}

// CancelWorkRestore undoes CancelWork (the fixture's own state flip, not a
// product transition: cancelled has no outgoing edge).
func (e *Env) CancelWorkRestore(scope domain.Scope, id domain.WorkItemID) {
	e.T.Helper()
	e.ownerExec(`UPDATE work_items SET state = 'active', version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
}

// SQLCancelled scans cancelled work items directly, as the work module's
// cancellation source does.
type SQLCancelled struct{ e *Env }

// CancelledItems implements execution.CancelledWork.
func (c SQLCancelled) CancelledItems(ctx context.Context, tenant domain.TenantID, after execution.CancelCursor, limit int) ([]execution.WorkCancellation, error) {
	rows, err := c.e.Migration.Query(ctx, `SELECT project_id, id, version FROM work_items WHERE tenant_id = $1 AND state = 'cancelled' AND (project_id, id) > ($2, $3)
		ORDER BY project_id, id LIMIT $4`, string(tenant), string(after.ProjectID), string(after.ID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []execution.WorkCancellation
	for rows.Next() {
		var project, id string
		var version int64
		if err := rows.Scan(&project, &id, &version); err != nil {
			return nil, err
		}
		out = append(out, execution.WorkCancellation{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, WorkItemID: domain.WorkItemID(id), Version: domain.Version(version)})
	}
	return out, rows.Err()
}
