// Package stack assembles the delivery path T-067 qualifies over real
// PostgreSQL (non-owner runtime role, forced row-level security): the real
// execution, secrets, pipeline, artifact, runtime-authority and delivery
// stores, the real delivery <-> execution adapters of T-077 (environment
// resolver, transaction-bound environment binding, retained cleanup
// configuration, cleanup authorization), the real cleanup-run bindings and the
// real expiry scheduler of T-066.
//
// It is test support, not production wiring: nothing here activates delivery
// in the composition root (T-072). The only controlled doubles are the ones the
// underlying fixtures document: the workload runtime (execution's Manual
// runtime — the tests run the real tools image themselves under the ordinary
// step restrictions and report the attempt's observations), the effect
// reconciler, and the Git source observation.
//
// Resource prefix t067.
package stack

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// Prefix is the resource prefix of this task's test resources.
const Prefix = "t067"

// Options select fault injection and policy.
type Options struct {
	// Delivery are the delivery fixture's own options (fault injection around
	// the delivery store, artifacts, credentials and authority).
	Delivery deliverytest.Options
	// WrapExecAppender inserts fault injection before execution's events are
	// appended inside its transactions.
	WrapExecAppender func(execpg.EventAppender) execpg.EventAppender
	// WrapBinder inserts fault injection into the transaction-bound delivery
	// reader execution's admission calls.
	WrapBinder func(execadapters.TxDelivery) execadapters.TxDelivery
	// Policy is the cleanup scheduler's policy.
	Policy deliverycleanup.Policy
	// Scopes are swept by the scheduler (default: the fixture's A and B).
	Scopes []domain.Scope
}

// Stack is one assembled delivery path.
type Stack struct {
	T    testing.TB
	D    *deliverytest.Env
	X    *executiontest.Env
	Opts Options

	ExecStore     *execpg.Store
	Exec          *execution.Service
	DeliveryStore *deliverypg.Store
	Svc           *delivery.Service
	Scheduler     *deliverycleanup.Scheduler
	Defs          *Definitions

	// Runtime authority and admission over the stores above.
	Attempts  *rpg.AttemptAuthority
	Grants    *rtpg.Store
	Guard     *deliverypg.Guard
	Authority *deliveryadapters.Authority
	Admission *runtimeauth.Service

	// Secrets: the service that sealed every value, and the real resolver the
	// runtime route serves.
	SecretSvc     *secrets.Service
	secretAdapter *secretsruntime.Adapter
	secretWrapper secrets.KeyWrapper

	// commits are the source commits of the projects whose workspace serves a
	// real repository.
	commits map[domain.WorkItemID]string

	pinMu   sync.Mutex
	pinEdit func(runtimev1.Context) (runtimev1.Context, error)

	// Podman runtime wiring (nil until UsePodman).
	pod *podmanWiring
	// Effects decorates the delivery module's effect reconciler for fault injection.
	Effects *DeploymentEffects
}

// Definitions is the scheduler's source of the cleanup pipeline definition of a
// scope: the project's own (T-065's delete script), set by the test.
type Definitions struct {
	m map[domain.Scope]domain.DefinitionID
}

// Set names the scope's cleanup definition.
func (d *Definitions) Set(scope domain.Scope, id domain.DefinitionID) { d.m[scope] = id }

// CleanupDefinition implements deliverycleanup.Definitions.
func (d *Definitions) CleanupDefinition(_ context.Context, scope domain.Scope) (domain.DefinitionID, error) {
	if id, ok := d.m[scope]; ok {
		return id, nil
	}
	return "", deliverycleanup.ErrNoDefinition
}

type scopeList []domain.Scope

func (l scopeList) Scopes(_ context.Context, after *domain.Scope, limit int) ([]domain.Scope, error) {
	all := append([]domain.Scope(nil), l...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].TenantID != all[j].TenantID {
			return all[i].TenantID < all[j].TenantID
		}
		return all[i].ProjectID < all[j].ProjectID
	})
	var out []domain.Scope
	for _, s := range all {
		if after != nil && (s.TenantID < after.TenantID || (s.TenantID == after.TenantID && s.ProjectID <= after.ProjectID)) {
			continue
		}
		out = append(out, s)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// sqlWork reads work items directly, as the execution fixture does.
type sqlWork struct{ x *executiontest.Env }

func (w sqlWork) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	var kind, state, spec string
	var rev int64
	err := w.x.Migration.QueryRow(ctx, `SELECT kind, state, spec, content_revision FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&kind, &state, &spec, &rev)
	if err != nil {
		return execution.WorkSnapshot{}, execution.ErrNotFound
	}
	return execution.WorkSnapshot{
		ID: id, Actionable: kind != "epic", Cancelled: state == "cancelled", ContentRevision: domain.ContentRevision(rev), Spec: spec,
		Criteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}},
	}, nil
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// New builds the fixture's PostgreSQL database and assembles the stack over it.
func New(t testing.TB, o Options) *Stack {
	t.Helper()
	d := deliverytest.New(t, o.Delivery)
	s := &Stack{T: t, D: d, X: d.X, Opts: o, Defs: &Definitions{m: map[domain.Scope]domain.DefinitionID{}}, commits: map[domain.WorkItemID]string{}}
	s.installSecrets()
	if len(s.Opts.Scopes) == 0 {
		s.Opts.Scopes = []domain.Scope{deliverytest.ScopeA, deliverytest.ScopeB}
	}
	s.build()
	return s
}

// Restart replaces every store, service and scheduler with fresh instances over
// the same database, as a process restart does. Nothing is carried in memory.
func (s *Stack) Restart() {
	s.T.Helper()
	s.build()
}

func (s *Stack) build() {
	t := s.T
	d, x := s.D, s.D.X
	var err error

	// ---- execution store with the transaction-bound environment binding ----
	binder := func(tx pgx.Tx) execution.TxEnvironments {
		var r execadapters.TxDelivery = &txReader{tx: tx, deployments: s.DeliveryStore}
		if s.Opts.WrapBinder != nil {
			r = s.Opts.WrapBinder(r)
		}
		te, err := execadapters.NewTxEnvironments(r)
		if err != nil {
			panic(err)
		}
		return te
	}
	var eAppender execpg.EventAppender = x.Events
	if s.Opts.WrapExecAppender != nil {
		eAppender = s.Opts.WrapExecAppender(eAppender)
	}
	s.ExecStore, err = execpg.NewRuntimeStore(x.Runtime, eAppender, execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: x.Workspaces, Secrets: x.Secrets, Environments: binder,
	}, execadapters.RevokeAttemptGrants, execpg.Options{Clock: x.Clock.Now})
	must(t, err)
	x.Store = s.ExecStore

	// ---- runtime authority over execution's rows: the namespace is the Podman
	// runtime's, and deployment.call is granted on its own interface ----
	s.Attempts, err = rpg.NewAttemptAuthority(s.ExecStore, func(domain.Scope) (string, error) { return podman.Namespace, nil }, deploymentActions)
	must(t, err)
	s.Grants, err = rtpg.NewStore(x.Runtime, s.Attempts)
	must(t, err)
	s.Guard, err = deliverypg.NewGuard(s.Attempts, d.Ring.Generation, x.Clock.Now)
	must(t, err)
	s.Authority, err = deliveryadapters.NewAuthority(d.Verifier, d.Ring, s.Grants, x.Clock.Now)
	must(t, err)
	signer, err := rtjwt.NewSigner(d.Ring, deliverytest.Issuer)
	must(t, err)
	s.Admission, err = runtimeauth.NewService(runtimeauth.Config{
		Store: s.Grants, Attempts: s.Attempts, Jobs: jobObserver{s}, Tokens: signer, Verifier: d.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}},
		Policy:  runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}, Clock: x.Clock.Now,
	})
	must(t, err)

	// ---- the Podman runtime of the stack, when one is configured ----
	if s.pod != nil {
		s.pod.build(s)
	}

	// ---- delivery store (committed reads, record writes) ----
	var db storage.TxBeginner = x.Runtime
	if s.Opts.Delivery.WrapDB != nil {
		db = s.Opts.Delivery.WrapDB(db)
	}
	var dAppender deliverypg.EventAppender = deliverypg.EventsFrom(x.Events)
	if s.Opts.Delivery.WrapAppender != nil {
		dAppender = s.Opts.Delivery.WrapAppender(dAppender)
	}
	s.DeliveryStore, err = deliverypg.NewStore(deliverypg.Config{DB: db, Events: dAppender, Guard: s.Guard, Clock: x.Clock.Now})
	must(t, err)

	// ---- delivery service over the real cleanup binding ----
	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(t, err)
	access, err := deliveryadapters.NewAccess(std)
	must(t, err)
	var creds delivery.Credentials
	creds, err = deliveryadapters.NewCredentials(x.Secrets)
	must(t, err)
	if s.Opts.Delivery.WrapCredentials != nil {
		creds = s.Opts.Delivery.WrapCredentials(creds)
	}
	var arts delivery.Artifacts
	arts, err = deliveryadapters.NewArtifacts(d.ArtifactLookup())
	must(t, err)
	if s.Opts.Delivery.WrapArtifacts != nil {
		arts = s.Opts.Delivery.WrapArtifacts(arts)
	}
	var authority delivery.Authority = s.Authority
	if s.Opts.Delivery.WrapAuthority != nil {
		authority = s.Opts.Delivery.WrapAuthority(authority)
	}
	runBindings, err := execution.NewCleanupBindings(s.ExecStore)
	must(t, err)
	bindings, err := deliverycleanup.NewBindings(runBindings)
	must(t, err)
	producers, err := deliveryadapters.NewExecution(s.ExecStore)
	must(t, err)
	s.Svc, err = delivery.NewService(delivery.Config{
		Store: s.DeliveryStore, Access: access, Credentials: creds, Contexts: pinReader{inner: d.Pins, s: s}, Producers: producers, Artifacts: arts,
		Authority: authority, Cleanups: bindings, Clock: x.Clock.Now, DefaultPreviewTTL: s.Opts.Delivery.DefaultPreviewTTL,
	})
	must(t, err)
	d.Svc, d.Store = s.Svc, s.DeliveryStore

	// ---- execution service over the real delivery adapters ----
	resolver, err := execadapters.NewDeliveryEnvironments(s.Svc)
	must(t, err)
	cleanup, err := deliveryadapters.NewCleanup(s.Svc)
	must(t, err)
	retained, err := execadapters.NewCleanupEnvironments(s.Svc)
	must(t, err)
	authz, err := execadapters.NewAuthority(std, reader)
	must(t, err)
	defs, err := execadapters.NewDefinitions(x.Pipeline)
	must(t, err)
	resolverCfg, err := secrets.NewConfigurationResolver(x.Secrets, x.Secrets, nil)
	must(t, err)
	cfg, err := execadapters.NewConfiguration(resolverCfg)
	must(t, err)
	var rt execution.Runtime = x.Workloads
	if s.pod != nil {
		rt = s.pod.rt
	}
	effects, err := deliveryadapters.NewEffects(s.DeliveryStore, bindings)
	must(t, err)
	if s.Effects == nil {
		s.Effects = &DeploymentEffects{}
	}
	s.Effects.bind(effects)
	deps := execution.Dependencies{
		Store: s.ExecStore, Authority: authz, Definitions: defs,
		Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Work:         sqlWork{x}, Configuration: cfg, Sources: x.Sources, Limits: x.Limits,
		Hooks:   execution.EnabledKinds{pipeline.KindContainer},
		Tenants: tenantList{deliverytest.ScopeA.TenantID, deliverytest.ScopeB.TenantID},
		Runtime: rt, Cleanup: cleanup, CleanupEnvironments: retained, EffectReconciler: s.Effects, Clock: x.Clock.Now,
	}
	s.Exec, err = execution.NewServiceWithEnvironments(deps, execution.Options{ClaimLease: 2 * time.Second, Batch: 10}, resolver)
	must(t, err)
	x.Svc = s.Exec

	// ---- secret resolution: the real resolver over the encrypted store ----
	src, err := secretspg.NewVersionSource(x.Secrets, s.secretWrapper, secretsInstallation)
	must(t, err)
	res, err := secrets.NewResolver(attemptGrant{s}, src)
	must(t, err)
	s.secretAdapter, err = secretsruntime.NewAdapter(res)
	must(t, err)

	s.Scheduler = s.NewScheduler()
}

// pinReader is delivery's pinned-context reader: exactly what execution persisted
// for the attempt, except while a test installs an explicit edit (EditPins), the
// identified fault injection of an absent, foreign or mismatched pin.
type pinReader struct {
	inner delivery.PinnedContexts
	s     *Stack
}

func (p pinReader) Pinned(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev1.Context, error) {
	c, err := p.inner.Pinned(ctx, scope, attempt)
	if err != nil {
		return runtimev1.Context{}, err
	}
	p.s.pinMu.Lock()
	fn := p.s.pinEdit
	p.s.pinMu.Unlock()
	if fn == nil {
		return c, nil
	}
	return fn(c)
}

// EditPins makes the pinned-context reader serve fn(persisted context) for every
// attempt until it is cleared with nil: fault injection at the reader boundary.
func (s *Stack) EditPins(fn func(runtimev1.Context) (runtimev1.Context, error)) {
	s.pinMu.Lock()
	s.pinEdit = fn
	s.pinMu.Unlock()
}

// jobObserver is runtime admission's view of the workloads: the Podman runtime
// verifies the container it started; an attempt that was never launched (a
// direct, API-level attempt) is the fixture's.
type jobObserver struct{ s *Stack }

func (j jobObserver) ObserveJob(ctx context.Context, o runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	if ev, err := j.s.D.Jobs.ObserveJob(ctx, o); err == nil {
		return ev, nil
	}
	if j.s.pod != nil {
		return j.s.pod.rt.ObserveJob(ctx, o)
	}
	return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
}

// deploymentActions is the action policy deployment steps need: the entrypoint's
// actions plus deployment.call.
func deploymentActions(access execution.Access) (runtimeauth.ActionSet, error) {
	base, err := rpg.DefaultActions(access)
	if err != nil {
		return runtimeauth.ActionSet{}, err
	}
	return deliverytest.DeploymentActions(base)
}

// Contracts the fixture's own Pins decorates: attempts read their immutable
// pinned context from execution's persisted rows.
var _ delivery.PinnedContexts = (*deliverytest.Pins)(nil)

// NewScheduler builds another scheduler over the same services: a second
// control-plane process sweeping the same records.
func (s *Stack) NewScheduler() *deliverycleanup.Scheduler {
	records, err := deliverycleanup.NewRecords(s.Svc)
	must(s.T, err)
	sch, err := deliverycleanup.New(deliverycleanup.Config{
		Scopes: scopeList(s.Opts.Scopes), Records: records, Runs: s.Exec, Definitions: s.Defs, Policy: s.Opts.Policy, Clock: s.X.Clock.Now,
	})
	must(s.T, err)
	return sch
}

var seq atomic.Int64

// ID returns a fresh valid identifier carrying the task prefix.
func ID(prefix string) string { return fmt.Sprintf("%s_t067_%d", prefix, seq.Add(1)) }

// ---- database inspection (the owner role sees every tenant) -------------------

// Count runs a count query as the owner role.
func (s *Stack) Count(sql string, args ...any) int { s.T.Helper(); return s.X.Count(sql, args...) }

// Deployments counts the scope's deployment rows.
func (s *Stack) Deployments(scope domain.Scope) int {
	s.T.Helper()
	return s.Count(`SELECT count(*) FROM deployments WHERE tenant_id = $1 AND project_id = $2`, string(scope.TenantID), string(scope.ProjectID))
}

// Caller is the human engineer of the fixtures.
func Caller() delivery.Caller { return deliverytest.Caller(deliverytest.Engineer) }

// Scalar runs a single-value query as the owner role.
func (s *Stack) Scalar(dst any, sql string, args ...any) { s.T.Helper(); s.X.Scalar(dst, sql, args...) }
