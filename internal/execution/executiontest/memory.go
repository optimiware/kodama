package executiontest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/memory"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/workspace"
)

// FakeClock is the controlled clock of the unit layer: it moves only when a
// test advances it.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts at a fixed instant.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// MemOptions tune the memory fixture. Zero values select the M1 wiring.
type MemOptions struct {
	// Hooks defaults to containers only.
	Hooks execution.Hooks
	// NoRuntime leaves the runtime port unset.
	NoRuntime bool
	// Runtime replaces the Manual runtime inside Workloads.
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
	// ServiceOptions override the service options.
	ServiceOptions *execution.Options
	// Kinds are the step kinds admission accepts (default: all three).
	Kinds []pipeline.StepKind
	// CommitFault fails every commit of the kernel after the work is done.
	CommitFault func() error
	// Grants is the optional runtime-grant revoker joined to the store's
	// transactions.
	Grants memory.GrantRevoker
	// GrantFence is the runtime-grant check joined to the transactions that record
	// a begin or a preparation receipt (default: OpenMemFence, the contract
	// suites' permissive fence; pass MemGrants to exercise the real check).
	GrantFence memory.GrantFencer
	// GrantFenceOf builds the fence over the store's own kernel (MemGrants); it
	// takes precedence over GrantFence.
	GrantFenceOf func(db *memdb.Store) memory.GrantFencer
	// NoGrantFence builds the store with no fence: begins and receipts must refuse.
	NoGrantFence bool
	// Shared is the explicit run_shared opt-in (a test-injected fake port and
	// policy); nil keeps new-format admission closed.
	Shared *execution.SharedConfig
	// SourceWrap wraps the Git observation seam (a call counter).
	SourceWrap func(execution.Sources) execution.Sources
}

// Mem is one fully wired execution module over the in-memory store: the memory
// Store and its peers over one memdb kernel, the Process-free runtime double
// (runtimetest.Manual behind runtimetest.Decorator unless Options.Runtime
// replaces it), controlled time and a table authority. It needs no service.
type Mem struct {
	Driver

	DB    *memdb.Store
	World *memory.World
	Auth  *memory.Authority
	Mem   *memory.Store

	Clock     *FakeClock
	Workloads *runtimetest.Decorator
	Manual    *runtimetest.Manual
	Effects   *FakeEffects
	Limits    *MutableLimits
	Defs      *pipelinetest.MemRepo
	Validator *pipeline.Validator

	opt MemOptions
	seq atomic.Int64
}

// NewMem builds the memory fixture.
func NewMem(t testing.TB, o ...MemOptions) *Mem {
	t.Helper()
	var opt MemOptions
	if len(o) > 0 {
		opt = o[0]
	}
	m := &Mem{Clock: NewFakeClock(), Auth: memory.NewAuthority(), Defs: &pipelinetest.MemRepo{}, Effects: &FakeEffects{}, opt: opt}
	var kopts []memdb.Option
	if opt.CommitFault != nil {
		kopts = append(kopts, memdb.WithCommitFault(opt.CommitFault))
	}
	m.DB = memdb.New(kopts...)
	m.World = memory.NewWorld(m.DB)
	var err error
	peers := m.World.Peers(m.Auth)
	peers.Grants = opt.Grants
	peers.GrantFence = opt.GrantFence
	if opt.GrantFenceOf != nil {
		peers.GrantFence = opt.GrantFenceOf(m.DB)
	}
	if peers.GrantFence == nil && !opt.NoGrantFence {
		peers.GrantFence = OpenMemFence{}
	}
	m.Mem, err = memory.NewStore(m.DB, peers, memory.Options{Clock: m.Clock.Now, NewID: m.ID})
	must(t, err)
	m.Validator = pipelinetest.NewValidator(t, pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate)

	m.Manual = runtimetest.NewManual()
	var inner execution.Runtime = m.Manual
	if opt.Runtime != nil {
		inner = opt.Runtime
	}
	m.Workloads = runtimetest.Decorate(inner)
	lim := DefaultLimit()
	if opt.Limits != nil {
		lim = *opt.Limits
	}
	m.Limits = &MutableLimits{lim: lim}
	m.Driver = Driver{T: t, Store: m.Mem, Rev: m.ContentRevision}
	m.Svc = m.NewService(opt)
	return m
}

// NewService builds another Service over the same ports and store, as a second
// control-plane process (or the same one after a restart) would be.
func (m *Mem) NewService(opt MemOptions) *execution.Service {
	m.T.Helper()
	hooks := opt.Hooks
	if hooks == nil {
		hooks = execution.EnabledKinds{pipeline.KindContainer}
	}
	kinds := pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}
	if opt.Kinds != nil {
		kinds = pipeline.EnabledKinds(opt.Kinds)
	}
	deps := execution.Dependencies{
		Store: m.Mem, Authority: m.Auth, Definitions: memDefinitions{m.Defs}, Capabilities: kinds,
		Work: m.World, Configuration: m.World, Sources: m.World, Limits: m.Limits, Hooks: hooks,
		Tenants: tenantList{ScopeA.TenantID, ScopeB.TenantID}, Cleanup: opt.Cleanup, Clock: m.Clock.Now, CancelledWork: m.World,
		Requirements: opt.Requirements, NewID: m.ID, Shared: opt.Shared,
	}
	if opt.SourceWrap != nil {
		deps.Sources = opt.SourceWrap(deps.Sources)
	}
	if !opt.NoRuntime {
		deps.Runtime = m.Workloads
	}
	if !opt.NoEffects {
		deps.EffectReconciler = m.Effects
	}
	so := execution.Options{ClaimLease: 2 * time.Second, Batch: 10}
	if opt.ServiceOptions != nil {
		so = *opt.ServiceOptions
	}
	var svc *execution.Service
	var err error
	if opt.Environments != nil {
		svc, err = execution.NewServiceWithEnvironments(deps, so, opt.Environments)
	} else {
		svc, err = execution.NewService(deps, so)
	}
	must(m.T, err)
	return svc
}

// memDefinitions adapts the in-memory pipeline repository to execution.Definitions.
type memDefinitions struct{ repo *pipelinetest.MemRepo }

func (d memDefinitions) Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	def, err := d.repo.Get(ctx, scope, id)
	if err != nil {
		return pipeline.Definition{}, execution.ErrNotFound
	}
	return def, nil
}

// ID returns a fresh, valid, readable identifier. Attempt and run IDs of the
// service come from here too, so a test run is deterministic.
func (m *Mem) ID(prefix string) string { return fmt.Sprintf("%s_t080_%06d", prefix, m.seq.Add(1)) }

// Advance moves the controlled clock.
func (m *Mem) Advance(d time.Duration) { m.Clock.Advance(d) }

// GrantAll seeds the standard users: engineers manage and read runs in both
// projects of tenant A, viewers only read them, and the other tenant's
// engineer manages its own project.
func (m *Mem) GrantAll() {
	for _, s := range []domain.Scope{ScopeA, ScopeA2} {
		m.Auth.Grant(s, Engineer)
		m.Auth.Grant(s, Viewer, execution.PermRunRead)
	}
	m.Auth.Grant(ScopeB, OtherEng)
}

// NewProject seeds an actionable work item with its workspace in the scope.
func (m *Mem) NewProject(scope domain.Scope) Project {
	m.T.Helper()
	item := domain.WorkItemID(m.ID("wi"))
	must(m.T, m.World.AddWork(context.Background(), scope, item, "# Plan\nDo the thing."))
	ws := m.ID("ws")
	must(m.T, m.World.AddWorkspace(context.Background(), scope, ws, item, CommitA))
	m.World.AcceptCommit(item, CommitB)
	return Project{Scope: scope, Item: item, Workspace: workspace.ID(ws)}
}

// Define registers a validated definition (YAML) and returns it.
func (m *Mem) Define(scope domain.Scope, yaml string) pipeline.Definition {
	m.T.Helper()
	actor := pipeline.Actor{ID: Engineer, Type: pipeline.ActorUser}
	v, err := m.Validator.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		m.T.Fatalf("validate definition: %v", err)
	}
	def, _, err := m.Defs.RegisterOrGet(context.Background(), pipeline.Candidate{
		Scope: scope, ID: domain.DefinitionID(m.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: m.Clock.Now(),
	})
	must(m.T, err)
	return def
}

// CancelWork cancels the item as the work module's cancellation transaction does.
func (m *Mem) CancelWork(scope domain.Scope, id domain.WorkItemID) {
	m.T.Helper()
	must(m.T, m.World.CancelWork(context.Background(), scope, id))
}

// EditSpec changes the item's specification (content revision + 1).
func (m *Mem) EditSpec(scope domain.Scope, id domain.WorkItemID, spec string) {
	m.T.Helper()
	must(m.T, m.World.EditSpec(context.Background(), scope, id, spec))
}

// ContentRevision reads the item's current content revision.
func (m *Mem) ContentRevision(p Project) domain.ContentRevision {
	m.T.Helper()
	snap, err := m.World.Snapshot(context.Background(), p.Scope, p.Item)
	must(m.T, err)
	return snap.ContentRevision
}

// EventCount counts the tenant's outbox events of a type.
func (m *Mem) EventCount(tenant domain.TenantID, typ string) int {
	m.T.Helper()
	evs, err := m.World.Events(context.Background(), tenant)
	must(m.T, err)
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// Subject adapts the fixture to the contract suite.
func (m *Mem) Subject() Subject {
	return Subject{Store: m.Mem, Svc: m.Svc, Seed: m, Drive: &m.Driver, Advance: m.Advance, EventCount: m.EventCount, Decorator: m.Workloads, Limits: m.Limits}
}

// Driver drives a Service through the use cases the way the control plane
// does, failing the test on unexpected errors. It is independent of the store.
type Driver struct {
	T     testing.TB
	Svc   *execution.Service
	Store execution.Store
	// Rev reads the current content revision of the project's item.
	Rev func(Project) domain.ContentRevision
}

// Admit admits a work run of the project as the engineer.
func (d *Driver) Admit(p Project, def pipeline.Definition, inputs map[string]runtimev1.Value) (*execution.Run, error) {
	return d.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t080"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: d.Rev(p), SourceCommit: CommitA, Inputs: inputs,
	})
}

// MustAdmit is Admit failing the test on error.
func (d *Driver) MustAdmit(p Project, def pipeline.Definition, inputs map[string]runtimev1.Value) *execution.Run {
	d.T.Helper()
	r, err := d.Admit(p, def, inputs)
	if err != nil {
		d.T.Fatalf("admit: %v", err)
	}
	return r
}

// Get loads the aggregate (no authorization; fixture inspection).
func (d *Driver) Get(run *execution.Run) *execution.Aggregate {
	d.T.Helper()
	agg, err := d.Store.Get(context.Background(), run.Scope, run.ID)
	must(d.T, err)
	return agg
}

// Claim claims one runnable step of the tenant.
func (d *Driver) Claim(tenant domain.TenantID) (*execution.AttemptSpec, error) {
	return d.Svc.ClaimRunnable(context.Background(), tenant)
}

// MustClaim claims or fails the test.
func (d *Driver) MustClaim(tenant domain.TenantID) *execution.AttemptSpec {
	d.T.Helper()
	in, err := d.Claim(tenant)
	if err != nil {
		d.T.Fatalf("claim: %v", err)
	}
	return in
}

// Observe applies an observation.
func (d *Driver) Observe(in *execution.AttemptSpec, obs execution.Observation) error {
	if obs.WorkloadName == "" {
		obs.WorkloadName = in.WorkloadName
	}
	return d.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, obs)
}

// Start observes the command start of the attempt.
func (d *Driver) Start(in *execution.AttemptSpec) {
	d.T.Helper()
	must(d.T, d.Observe(in, execution.Observation{Kind: execution.ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}))
}

// Succeed submits a completed result for the attempt (declared outputs given).
func (d *Driver) Succeed(in *execution.AttemptSpec, outputs map[string]runtimev1.Value, commit string) (execution.ResultAck, error) {
	zero := int64(0)
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	if outputs == nil {
		res.Outputs = map[string]runtimev1.Value{}
	}
	if commit != "" {
		c := runtimev1.GitCommit(commit)
		res.SourceCommit = &c
	}
	return d.Commit(in, res)
}

// Fail submits a failed result.
func (d *Driver) Fail(in *execution.AttemptSpec) (execution.ResultAck, error) {
	code, reason := int64(1), "command_failed"
	return d.Commit(in, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
}

// Commit submits a result.
func (d *Driver) Commit(in *execution.AttemptSpec, res runtimev1.Result) (execution.ResultAck, error) {
	h, err := runtimev1.ResultHash(res)
	must(d.T, err)
	return d.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
	})
}

// Terminate confirms the workload of the attempt is gone with launch evidence.
func (d *Driver) Terminate(in *execution.AttemptSpec, launch execution.LaunchEvidence) error {
	return d.Observe(in, execution.Observation{Kind: execution.ObservedTerminated, Launch: launch})
}
