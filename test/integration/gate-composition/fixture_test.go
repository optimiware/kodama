// Package gatecomposition_test is the independent composition fixture of T-073:
// real PostgreSQL carrying the execution, workspace, secret, artifact,
// runtime-grant and gate schemas (migrations applied in the merged order, the
// runtime authority after execution and the gate contribution after both)
// under separate owner and non-owner runtime roles; the runtime-enabled
// execution store (execpg.NewRuntimeStore); the real gate store, keyed
// decision helper, gate-aware execution store and gate service over it, with
// the gate store's execution binding, transaction-bound authority and event
// appender all joined to the caller's transaction. Fault injection wraps the
// existing seams (commit, peer bindings, authority binder, deployment binder
// and event appender), never production code.
//
// Controlled doubles are limited to seams outside the gate/execution SQL join:
// the workspace history and run source verification (fake sources, consulted
// before and outside any transaction), the in-memory object store behind
// evidence reads, the runtime executor, and the source of deployment
// configuration. Deployment configuration is a tenant-scoped SQL fixture table
// (SQLDeployments) read through the transaction the gate store hands to its
// binder, so the binder's read is a real transactional read; it is not a join to
// the delivery module (T-035), which no merged code provides; see README.md.
//
// Resource prefix t073: databases (via KODAMA_RESOURCE_PREFIX) and every
// fixture identifier created here carry it.
package gatecomposition_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

// Prefix is the task's resource prefix.
const Prefix = "t073"

// Fixture scopes and users come from the gate and execution fixtures.
var (
	ScopeA  = gatestest.ScopeA
	ScopeA2 = gatestest.ScopeA2
	ScopeB  = gatestest.ScopeB
)

const (
	readRun  = "workspace: {access: read, from: run}"
	writeRun = "workspace: {access: write, from: run}"

	reportOutput = "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"
)

// Options select what a test needs, so a test never claims coverage of a
// dependency it did not use.
type Options struct {
	// SingleConn gives the composition (execution store, gate store, guard
	// decorators, keyed helper) a runtime pool of exactly one connection, so any
	// peer that opened a second transaction while its caller's was open would
	// deadlock, and tenant context is reused across tenants by construction.
	SingleConn bool
	// S3 stores artifact bytes in LocalStack S3 instead of memory.
	S3 bool
	// NoDeploymentBinder leaves the gate store's transaction-bound deployment
	// binder unset (the state of the merged code: no real binder exists).
	NoDeploymentBinder bool
	// GateTTL is the gate deadline duration (default one hour).
	GateTTL time.Duration
}

// Env is one integrated environment.
type Env struct {
	*gatestest.Env

	// Pool is the runtime pool the composition's stores use.
	Pool *pgxpool.Pool
	// F is the fault and trace layer around the seams.
	F *Faults
	// RT is the runtime-enabled execution store the gate store and the gate-aware
	// execution store both join.
	RT *execpg.Store
	// Deploy is the SQL deployment configuration fixture (see SQLDeployments).
	Deploy *SQLDeployments

	// Keep makes the scenario builders leave earlier runs of the environment
	// active (a test that races several live scenarios); by default each builder
	// cancels them first, so a claim only meets its own scenario's steps.
	Keep bool

	opts     Options
	mu       sync.Mutex
	baseline *runtimev1.DesignBaseline
	ttl      time.Duration
}

// New builds the environment.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	g := gatestest.New(t, gatestest.Options{S3: o.S3, GateTTL: o.GateTTL})
	e := &Env{Env: g, opts: o, ttl: o.GateTTL, Pool: g.X.Runtime}
	if e.ttl == 0 {
		e.ttl = time.Hour
	}
	// The execution fixture already applied the baseline, which includes the gate
	// tables and the runtime grants; re-applying it is a no-op.
	manifest, err := storage.NewManifest(storage.BaseContribution(), execpg.RunResourceMigrations())
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = storage.Migrate(ctx, g.X.Migration, manifest, storage.MigrateOptions{RuntimeRole: g.X.Base.DB.RuntimeRole})
	must(t, err)

	// Tests keep several attempts alive; the fixture's quota is not what they exercise.
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 200, 100
	g.X.Limits.Set(lim)

	if o.SingleConn {
		e.Pool = g.X.Base.DB.RuntimePool(ctx, 1)
	}
	e.F = NewFaults(e.Pool)
	e.wire()
	return e
}

// Restart rebuilds every store and service over the same durable records, as a
// new process would. Nothing is carried over in memory but the test doubles of
// seams outside the composition.
func (e *Env) Restart() { e.wire() }

// wire builds the composition: the exact production shape of the integration
// owner's checklist (internal/gates/README.md, "Integration owner").
func (e *Env) wire() {
	t, x := e.T, e.X
	rt, err := execpg.NewRuntimeStore(e.F.DB(), e.F.Appender(x.Events), execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: e.F.Workspaces(x.Workspaces), Secrets: e.F.Secrets(x.Secrets),
	}, e.F.Revoker(execadapters.RevokeAttemptGrants), execpg.Options{Clock: x.Clock.Now})
	must(t, err)
	e.RT = rt

	if e.Deploy == nil {
		e.Deploy = newSQLDeployments(e, e.X.Base.DB.RuntimeRole)
	}
	e.Deploy.f, e.Deploy.pool = e.F, e.F.DB()
	cfg := gatespg.Config{
		DB: e.F.DB(), Events: e.F.Appender(x.Events), Execution: rt,
		Authority: e.F.Authority(adapters.BindAuthority), Clock: x.Clock.Now,
	}
	if !e.opts.NoDeploymentBinder {
		cfg.Deployments = e.Deploy.Bind
	}
	store, err := gatespg.NewStore(cfg)
	must(t, err)
	keyed, err := gatespg.NewKeyed(x.Idem, store)
	must(t, err)
	gateExec, err := gatespg.NewExecutionStore(e.F.DB(), rt, rt, store)
	must(t, err)

	pins, err := adapters.NewPins(e.Repo)
	must(t, err)
	candidates, err := adapters.NewCandidates(x.Sources)
	must(t, err)
	svc, err := gates.NewService(gates.Dependencies{
		Store: store, Authority: e.Authz, Runs: rt, Evidence: e.Evidence, Pins: pins, Candidates: candidates,
		Tenants: tenantList{ScopeA.TenantID, ScopeB.TenantID}, Deployments: e.Deploy,
		Conformance: adapters.ConformanceMapping{ReportOutput: gatestest.ReportOutput}, Clock: x.Clock.Now,
	}, gates.Options{GateTTL: e.ttl, Batch: 10})
	must(t, err)

	e.Store, e.Keyed, e.GateExec, e.Gates = store, keyed, gateExec, svc
	// The execution service over the gate-aware store: the one a production
	// composition root installs for dispatch, claims and cancellation.
	e.Svc = x.NewService(executiontest.Options{
		Store: &agentStore{Store: gateExec, env: e}, Hooks: execution.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent},
	})
}

// SetGateTTL changes the deadline duration of gates requested from now on.
func (e *Env) SetGateTTL(d time.Duration) { e.ttl = d; e.wire() }

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// SetBaseline sets the design baseline pinned into the context of every agent
// attempt claimed from now on (nil pins none), as the agents planner pins the
// frozen profile's baseline.
func (e *Env) SetBaseline(b *runtimev1.DesignBaseline) {
	e.mu.Lock()
	e.baseline = b
	e.mu.Unlock()
}

func (e *Env) pinned() *runtimev1.DesignBaseline {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.baseline
}

// ---- agent expansion over the gate-aware store ----

// agentStore expands agent steps at admission and pins the design baseline
// into agent attempts, standing in for the agents module's resolver and
// planner (outside the gate/execution join).
type agentStore struct {
	execution.Store
	env *Env
}

type fakeResolver struct{}

func (fakeResolver) ResolveAgent(context.Context, domain.Scope, pipeline.Step) (execution.AgentExpansion, error) {
	return execution.AgentExpansion{Image: pipelinetest.Image, Command: []string{"/bin/true"}}, nil
}

func (s *agentStore) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	expanded, err := execution.ExpandAgents(ctx, fakeResolver{}, p)
	if err != nil {
		return nil, false, err
	}
	return s.Store.Admit(ctx, expanded)
}

func (s *agentStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	return s.Store.Update(ctx, scope, id, opts, func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		fx, err := fn(ctx, env, agg)
		if err != nil {
			return fx, err
		}
		b := s.env.pinned()
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

// ---- deployment configuration fixture ----

// SQLDeployments is a tenant-scoped SQL fixture of the gate-owned deployment
// port: the table t073_deployments (forced row-level security by tenant, owned
// by the owner role, readable and writable by the non-owner runtime role). It
// stands in for the deployment configuration the delivery module would answer
// with. It is NOT a join to the delivery module (T-035), which no merged code
// provides; it exists so the gate store's transaction-bound binder performs a
// real SQL read through the transaction it was handed:
//
//   - Bind(tx) reads the row through that very pgx.Tx with one statement that
//     also returns txid_current(), recorded as a Hit, so equal identifiers with
//     the other participants of the decision, consumption or claim prove the
//     read ran in their transaction, and a row written but not committed by the
//     same transaction is visible to it and to nobody else;
//   - the unbound Snapshot (the gate service port, used before a gate exists)
//     reads in its own read-only tenant transaction, as the gate owner specifies.
//
// Fail makes the binder fail as an outage after a successful read (a double of
// the outage of a deployment dependency; the SQL read itself is real).
type SQLDeployments struct {
	f    *Faults
	pool storage.TxBeginner
	exec func(sql string, args ...any)
}

const deploymentsDDL = `
CREATE TABLE t073_deployments (
    tenant_id     kodama_id NOT NULL,
    project_id    kodama_id NOT NULL,
    id            kodama_id NOT NULL,
    candidate     text NOT NULL,
    config_digest text NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id)
);
ALTER TABLE t073_deployments ENABLE ROW LEVEL SECURITY;
ALTER TABLE t073_deployments FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON t073_deployments TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON t073_deployments
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
`

func newSQLDeployments(e *Env, role string) *SQLDeployments {
	e.X.OwnerExec(deploymentsDDL)
	e.X.OwnerExec(`GRANT SELECT, INSERT, UPDATE ON t073_deployments TO ` + pgx.Identifier{role}.Sanitize())
	return &SQLDeployments{f: e.F, pool: e.F.DB(), exec: e.X.OwnerExec}
}

// Set records a deployment of scope that records candidate and has the
// configuration digest (as the owner role, committed).
func (d *SQLDeployments) Set(scope domain.Scope, id, candidate, configDigest string) {
	d.exec(`INSERT INTO t073_deployments (tenant_id, project_id, id, candidate, config_digest) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, project_id, id) DO UPDATE SET candidate = EXCLUDED.candidate, config_digest = EXCLUDED.config_digest`,
		string(scope.TenantID), string(scope.ProjectID), id, candidate, configDigest)
}

// Remove makes the deployment unknown in every scope.
func (d *SQLDeployments) Remove(id string) { d.exec(`DELETE FROM t073_deployments WHERE id = $1`, id) }

func readDeployment(ctx context.Context, tx pgx.Tx, scope domain.Scope, id string) (snap gates.DeploymentSnapshot, xid int64, err error) {
	var candidate, digest string
	err = tx.QueryRow(ctx, `SELECT candidate, config_digest, txid_current() FROM t073_deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), id).Scan(&candidate, &digest, &xid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return snap, 0, gates.ErrNotFound
	case err != nil:
		return snap, 0, err
	}
	return gates.DeploymentSnapshot{ID: id, ConfigDigest: digest, Scope: scope, Candidate: candidate}, xid, nil
}

// Snapshot is the unbound port the gate service uses before a gate exists.
func (d *SQLDeployments) Snapshot(ctx context.Context, scope domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	var snap gates.DeploymentSnapshot
	err := storage.WithTenantOptions(ctx, d.pool, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, _, err = readDeployment(ctx, tx, scope, id)
		return err
	})
	return snap, err
}

// Bind is the gate store's DeploymentBinder.
func (d *SQLDeployments) Bind(tx pgx.Tx) gates.Deployments { return &boundDeployments{d: d, tx: tx} }

type boundDeployments struct {
	d  *SQLDeployments
	tx pgx.Tx
}

func (b *boundDeployments) Snapshot(ctx context.Context, scope domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	snap, xid, err := readDeployment(ctx, b.tx, scope, id)
	if err != nil {
		return snap, err
	}
	b.d.f.record(PointDeployment, xid)
	return snap, b.d.f.failure(PointDeployment)
}

// ---- scenarios ----

// Scenario is a project with a run and its gate.
type Scenario struct {
	P    executiontest.Project
	Run  *execution.Run
	Gate *gates.Gate
	// Pre is the step that produced the gate's subject ("test" by default); its
	// attempts are not leaks.
	Pre string
	// Dep is the deployment of a deployment-bearing gate.
	Dep string
}

func (e *Env) cancelAtEnd(run *execution.Run) {
	e.T.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer, RequestID: "req_t073"}, run.Scope, run.ID)
	})
}

func (e *Env) quiesceUnlessKept() {
	if !e.Keep {
		e.Quiesce()
	}
}

// Quiesce cancels every run that can still progress, so a claim in a later
// subtest of the same environment can only meet its own scenario's steps.
func (e *Env) Quiesce() {
	e.T.Helper()
	rows, err := e.X.Migration.Query(context.Background(), `SELECT tenant_id, project_id, id FROM runs WHERE status NOT IN ('succeeded', 'failed', 'cancelled')`)
	must(e.T, err)
	var runs []domain.Scope
	var ids []domain.RunID
	for rows.Next() {
		var tn, pj, id string
		must(e.T, rows.Scan(&tn, &pj, &id))
		runs = append(runs, domain.Scope{TenantID: domain.TenantID(tn), ProjectID: domain.ProjectID(pj)})
		ids = append(ids, domain.RunID(id))
	}
	rows.Close()
	for i, sc := range runs {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer, RequestID: "req_t073"}, sc, ids[i])
	}
}

// ReadyRun is test -> review(gate over the test report) -> deploy(writer) with
// the test step finished: the gate step is ready and has no gate yet. The
// writer successor makes a leaked attempt, ordinal, reservation or writer slot
// observable.
func (e *Env) ReadyRun(scope domain.Scope) Scenario {
	e.T.Helper()
	e.quiesceUnlessKept()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, reportOutput),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", writeRun))
	run := e.MustAdmit(p, def, nil)
	e.cancelAtEnd(run)
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	return Scenario{P: p, Run: run, Pre: "test"}
}

// Reviewed is ReadyRun up to a requested gate.
func (e *Env) Reviewed(scope domain.Scope) Scenario {
	e.T.Helper()
	s := e.ReadyRun(scope)
	s.Gate = e.RequestGate(s.Run, "review")
	return s
}

// DeploymentGated is build(deployment output) -> review(gate bound to the
// deployment) -> promote, with the deployment recorded for the candidate and the
// configuration digest, up to a requested gate. name must be unique per
// definition.
func (e *Env) DeploymentGated(scope domain.Scope, name, depID, config string) Scenario {
	e.T.Helper()
	e.quiesceUnlessKept()
	p := e.X.NewProject(scope)
	def := e.DefineDoc(scope, pipelinetest.DocNamed(name, "{}",
		pipelinetest.Container("build", readRun, "outputs: {deployment: {type: string}}"),
		gatestest.DeploymentGate("review", []string{"build"}, nil, "${{ steps.build.outputs.deployment }}"),
		pipelinetest.Container("promote", "needs: [review]", writeRun)))
	e.Deploy.Set(scope, depID, gatestest.CommitA, digestOf(config))
	run := e.MustAdmit(p, def, nil)
	e.cancelAtEnd(run)
	e.StartAndSucceed(e.MustClaim(scope.TenantID), map[string]runtimev1.Value{"deployment": runtimev1.StringValue(depID)})
	return Scenario{P: p, Run: run, Pre: "build", Dep: depID, Gate: e.RequestGate(run, "review")}
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FinalGate is test -> review(gate) with nothing behind the gate. Completing
// that gate step settles the run, so the transition emits a run event: the
// execution event a gate completion can produce.
func (e *Env) FinalGate(scope domain.Scope) Scenario {
	e.T.Helper()
	e.quiesceUnlessKept()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, reportOutput),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"))
	run := e.MustAdmit(p, def, nil)
	e.cancelAtEnd(run)
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	return Scenario{P: p, Run: run, Pre: "test", Gate: e.RequestGate(run, "review")}
}

// RequestGate lets reconciliation request the ready gate of the step.
func (e *Env) RequestGate(run *execution.Run, step string) *gates.Gate {
	e.T.Helper()
	if rep, err := e.Gates.Reconcile(context.Background(), run.Scope.TenantID); err != nil || rep.Requested < 1 {
		e.T.Fatalf("reconcile: %+v %v", rep, err)
	}
	return e.GateOf(run, step)
}

// GateOf loads the gate of a step.
func (e *Env) GateOf(run *execution.Run, step string) *gates.Gate {
	e.T.Helper()
	g, err := e.Store.ByStep(context.Background(), run.Scope, run.ID, step)
	must(e.T, err)
	return g
}

// Current reloads a gate.
func (e *Env) Current(g *gates.Gate) *gates.Gate {
	e.T.Helper()
	cur, err := e.Store.Get(context.Background(), g.Scope, g.ID)
	must(e.T, err)
	return cur
}

// Reconcile runs one reconciliation pass of the tenant.
func (e *Env) Reconcile(tenant domain.TenantID) gates.Report {
	e.T.Helper()
	rep, err := e.Gates.Reconcile(context.Background(), tenant)
	if err != nil {
		e.T.Fatalf("reconcile: %v", err)
	}
	return rep
}

// DecideCmd is a decision command bound to the gate's current version and subject.
func DecideCmd(g *gates.Gate, d gates.Decision) gates.DecideCommand {
	reason := "Reviewed the identified source and test evidence."
	if d == gates.DecisionReject {
		reason = "The identified evidence does not support delivery."
	}
	return gates.DecideCommand{Decision: d, Reason: reason, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest()}
}

// Decide decides through the gate service as the principal.
func (e *Env) Decide(g *gates.Gate, principal string, d gates.Decision) (*gates.Gate, error) {
	return e.Gates.Decide(context.Background(), gatestest.Caller(principal), g.Scope, g.ID, DecideCmd(g, d))
}

// Approve approves as the fixture reviewer.
func (e *Env) Approve(g *gates.Gate) (*gates.Gate, error) {
	return e.Decide(g, gatestest.Reviewer, gates.DecisionApprove)
}

// MustApprove approves as the fixture reviewer or fails the test.
func (e *Env) MustApprove(g *gates.Gate) *gates.Gate {
	e.T.Helper()
	got, err := e.Approve(g)
	must(e.T, err)
	return got
}

// Consume completes the step of an approved gate through the gate store.
func (e *Env) Consume(g *gates.Gate) (gates.StepOutcome, error) {
	return e.Store.CompleteStep(context.Background(), g.Scope, g.ID)
}

// Claim claims the next runnable step through the gate-aware service.
func (e *Env) ClaimErr(tenant domain.TenantID) (*execution.AttemptSpec, error) {
	return e.Svc.ClaimRunnable(context.Background(), tenant)
}

// NothingRunnable asserts that nothing can be claimed.
func (e *Env) NothingRunnable(tenant domain.TenantID) error {
	_, err := e.ClaimErr(tenant)
	if !errors.Is(err, execution.ErrNothingRunnable) {
		return errors.New("claim did not report nothing runnable: " + errString(err))
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// ---- inspection (owner role, and the application's view) ----

// Snap is every durable effect a gate/execution transition could leave for one
// run, plus the compute a gate must never create.
type Snap struct {
	StepStatus, RunStatus string
	Started               int // attempts_started of the gate-protected successor
	History               int
	ExecOutbox            int // run and attempt events about the run (attempts found by the payload's run id)
	ExecAudit             int // run, step and attempt audit rows (attempts found by the metadata's run id)
	GateRows              int
	GateEvidence          int
	GateBound             int // steps carrying a gate reference
	GateVersion           int
	GateStatus            string
	GateAudit             int // audit rows of the stable gate identity, with or without a gate row
	GateOutbox            int // events of the stable gate identity, with or without a gate row
	Pins                  int // artifact pins owned by gates of the run
	Attempts              int
	Reservations          int
	WriterSlots           int
	Invalidations         int
}

// Snapshot reads the run's state. gateStep is the gate step; project the run's
// work item workspace.
func (e *Env) Snapshot(s Scenario, gateStep string) Snap {
	e.T.Helper()
	run, tenant := string(s.Run.ID), s.Run.Scope.TenantID
	var n Snap
	e.X.Scalar(&n.StepStatus, `SELECT status FROM step_runs WHERE run_id = $1 AND step_id = $2`, run, gateStep)
	e.X.Scalar(&n.RunStatus, `SELECT status FROM runs WHERE id = $1`, run)
	e.X.Scalar(&n.Started, `SELECT coalesce(sum(attempts_started), 0)::int FROM step_runs WHERE run_id = $1 AND step_id <> $2 AND step_id <> $3`, run, gateStep, s.Pre)
	n.History = e.X.Count(`SELECT count(*) FROM run_history WHERE run_id = $1`, run)
	gate := string(gates.GateIDFor(s.Run.Scope, s.Run.ID, gateStep))
	n.ExecAudit = e.X.Count(`SELECT count(*) FROM audit WHERE resource_type <> 'gate' AND (resource_id = $1 OR metadata->>'runId' = $1)`, run)
	n.ExecOutbox = e.X.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $1
		OR convert_from(payload, 'UTF8')::jsonb->'data'->>'runId' = $1`, run)
	n.GateRows = e.X.Count(`SELECT count(*) FROM gates WHERE run_id = $1`, run)
	n.GateEvidence = e.X.Count(`SELECT count(*) FROM gate_evidence WHERE gate_id IN (SELECT id FROM gates WHERE run_id = $1)`, run)
	n.GateBound = e.X.Count(`SELECT count(*) FROM step_runs WHERE run_id = $1 AND gate_ref IS NOT NULL`, run)
	n.GateAudit = e.X.Count(`SELECT count(*) FROM audit WHERE resource_type = 'gate' AND (resource_id = $1 OR metadata->>'runId' = $2)`, gate, run)
	n.GateOutbox = e.X.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE convert_from(payload, 'UTF8')::jsonb->'resource'->>'type' = 'gate'
		AND convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $1`, gate)
	n.Pins = e.X.Count(`SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id IN (SELECT id FROM gates WHERE run_id = $1)`, run)
	n.Attempts = e.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id <> $2`, run, s.Pre)
	n.Reservations = e.X.Count(`SELECT count(*) FROM execution_reservations WHERE run_id = $1`, run)
	n.WriterSlots = e.X.Count(`SELECT count(*) FROM workspaces WHERE id = $1 AND writer_attempt_id IS NOT NULL`, string(s.P.Workspace))
	n.Invalidations = e.X.Count(`SELECT count(*) FROM gates WHERE run_id = $1 AND invalidated_at IS NOT NULL`, run)
	if n.GateRows > 0 {
		var v int64
		e.X.Scalar(&v, `SELECT max(version) FROM gates WHERE run_id = $1`, run)
		n.GateVersion = int(v)
		e.X.Scalar(&n.GateStatus, `SELECT max(status) FROM gates WHERE run_id = $1`, run)
	}
	return n
}

// Transitions counts the recorded transitions of a step out of pending.
func (e *Env) Transitions(run *execution.Run, step string) int {
	return e.X.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = $2 AND to_status <> 'pending'`, string(run.ID), step)
}

// PoolContext reads the tenant setting and the visible gates through the pool
// outside any transaction of ours, as the next borrower of the connection
// would.
func (e *Env) PoolContext() (setting string, visible int) {
	e.T.Helper()
	ctx := context.Background()
	must(e.T, e.Pool.QueryRow(ctx, `SELECT coalesce(current_setting('`+storage.TenantSetting+`', true), '')`).Scan(&setting))
	must(e.T, e.Pool.QueryRow(ctx, `SELECT count(*) FROM gates`).Scan(&visible))
	return setting, visible
}

// Hold runs a locking statement in its own owner-pool transaction (a decision,
// an invalidation, a revocation or a competing claim in flight) and returns the
// transaction's identifier and a release function that rolls it back.
func (e *Env) Hold(sql string, args ...any) (xid string, release func()) {
	e.T.Helper()
	ready, stop, finished := make(chan string, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		tx, err := e.X.Migration.Begin(context.Background())
		if err != nil {
			e.T.Errorf("hold: %v", err)
			ready <- ""
			return
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
			e.T.Errorf("hold: %v", err)
			ready <- ""
			return
		}
		var id string
		if err := tx.QueryRow(context.Background(), `SELECT pg_current_xact_id()::xid::text`).Scan(&id); err != nil {
			e.T.Errorf("hold: %v", err)
		}
		ready <- id
		<-stop
	}()
	id := <-ready
	if id == "" {
		e.T.Fatal("the lock was not taken")
	}
	return id, func() { close(stop); <-finished }
}

// HoldRevocation revokes the user in its own owner-pool transaction exactly as
// identity's revocation does (authorization revision, then bindings) and keeps
// the transaction open: the revision row stays locked and the revocation
// invisible. The release function commits it, so a waiter sees the revocation
// only after it has waited on the authorization revision.
func (e *Env) HoldRevocation(scope domain.Scope, user string) (xid string, release func()) {
	e.T.Helper()
	ready, stop, finished := make(chan string, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ctx := context.Background()
		tx, err := e.X.Migration.Begin(ctx)
		if err != nil {
			e.T.Errorf("hold revocation: %v", err)
			ready <- ""
			return
		}
		defer tx.Rollback(ctx)
		var id string
		for _, q := range []string{
			`UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`,
			`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		} {
			if _, err := tx.Exec(ctx, q, string(scope.TenantID), user); err != nil {
				e.T.Errorf("hold revocation: %v", err)
				ready <- ""
				return
			}
		}
		if err := tx.QueryRow(ctx, `SELECT pg_current_xact_id()::xid::text`).Scan(&id); err != nil {
			e.T.Errorf("hold revocation: %v", err)
		}
		ready <- id
		<-stop
		if err := tx.Commit(ctx); err != nil {
			e.T.Errorf("commit revocation: %v", err)
		}
	}()
	id := <-ready
	if id == "" {
		e.T.Fatal("the revocation was not started")
	}
	return id, func() { close(stop); <-finished }
}

// HoldGate locks the gate row FOR UPDATE.
func (e *Env) HoldGate(g *gates.Gate) (xid string, release func()) {
	e.T.Helper()
	return e.Hold(`SELECT 1 FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
		string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.ID))
}

// WaitBlocked waits until some backend waits for the transaction xid, which
// holds the lock under test. Other roles' activity rows are not readable by the
// test connection, so the wait is observed through pg_locks.
func (e *Env) WaitBlocked(xid string) {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.X.Count(`SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype = 'transactionid' AND transactionid::text = $1`, xid) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.T.Fatalf("no backend ever waited for the lock held by transaction %s", xid)
}
