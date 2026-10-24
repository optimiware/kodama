package app

// Fixtures of the manual agent/review/delivery assembly (T-072). Everything is
// a deterministic double behind the consumer-owned ports: the memory gate
// module, the profile repository of agentstest, a small in-memory deployment
// record store and scripted cleanup records. None of it is evidence of SQL,
// transaction or cluster behavior (T-073, T-067, T-038) and none of it reaches
// production: the doubles flag themselves development-only, so `production`
// refuses them at construction.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	"github.com/optimiware/kodama/internal/httpapi"
)

// ---- delivery -----------------------------------------------------------------

// dvAccess grants every permission to every user: the authorization seam is not
// under test here (delivery's own suites cover it).
type dvAccess struct{}

func (dvAccess) DevelopmentOnly() bool { return true }
func (dvAccess) Authorize(context.Context, delivery.Actor, domain.Scope, delivery.Permission) error {
	return nil
}

// dvCredentials resolves no credential.
type dvCredentials struct{}

func (dvCredentials) Current(context.Context, domain.Scope, string) (delivery.CredentialInfo, error) {
	return delivery.CredentialInfo{}, delivery.Fail(delivery.ErrCredentialUnavailable, nil, "no credential in the fixture")
}

// dvStore is a minimal in-memory delivery.Store: environments and the deployment
// records a test seeds. Registration is not modelled (T-035 owns it).
type dvStore struct {
	mu   sync.Mutex
	envs map[string]delivery.Environment
	deps map[string]delivery.Deployment
}

func newDVStore() *dvStore {
	return &dvStore{envs: map[string]delivery.Environment{}, deps: map[string]delivery.Deployment{}}
}

func (*dvStore) DevelopmentOnly() bool { return true }

func (s *dvStore) unsupported() error {
	return delivery.Fail(delivery.ErrUnavailable, nil, "not modelled by the fixture")
}

func (s *dvStore) CreateEnvironment(_ context.Context, _ delivery.Mutation, env delivery.Environment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs[string(env.Record().Scope.ProjectID)+"/"+string(env.Record().ID)] = env
	return nil
}

func (s *dvStore) GetEnvironment(_ context.Context, scope domain.Scope, id domain.EnvironmentID) (delivery.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, ok := s.envs[string(scope.ProjectID)+"/"+string(id)]
	if !ok || env.Record().Scope != scope {
		return delivery.Environment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return env, nil
}

func (s *dvStore) ListEnvironments(context.Context, domain.Scope, string, int) ([]delivery.Environment, error) {
	return nil, nil
}

func (s *dvStore) UpdateEnvironment(context.Context, delivery.Mutation, domain.Scope, domain.EnvironmentID, domain.Version, delivery.EnvironmentEdit) (delivery.Environment, error) {
	return delivery.Environment{}, s.unsupported()
}

func (s *dvStore) RetainSnapshot(context.Context, delivery.ConfigurationSnapshot) (delivery.ConfigurationSnapshot, error) {
	return delivery.ConfigurationSnapshot{}, s.unsupported()
}

func (s *dvStore) GetSnapshot(context.Context, domain.Scope, domain.EnvironmentID, runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func (s *dvStore) Register(context.Context, delivery.RegisterCommand) (delivery.RegisterResult, error) {
	return delivery.RegisterResult{}, s.unsupported()
}

func (s *dvStore) GetDeployment(_ context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deps[string(id)]
	if !ok || d.Scope() != scope {
		return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return d, nil
}

func (s *dvStore) FindByAction(_ context.Context, scope domain.Scope, action string) (delivery.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deps {
		if d.Scope() == scope && d.ActionKey() == action {
			return d, nil
		}
	}
	return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func (s *dvStore) ListDeployments(context.Context, domain.Scope, delivery.DeploymentFilter, delivery.DeploymentID, int) ([]delivery.Deployment, error) {
	return nil, nil
}

func (s *dvStore) Update(context.Context, domain.Scope, delivery.DeploymentID, delivery.Mutation, func(*delivery.Deployment) (delivery.Transition, error)) (delivery.Deployment, delivery.Transition, error) {
	return delivery.Deployment{}, delivery.Transition{}, s.unsupported()
}

func (s *dvStore) Expired(context.Context, domain.Scope, time.Time, int) ([]delivery.Deployment, error) {
	return nil, nil
}

func (s *dvStore) History(context.Context, domain.Scope, delivery.DeploymentID) ([]delivery.HistoryEntry, error) {
	return nil, nil
}

func (s *dvStore) put(d delivery.Deployment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps[string(d.ID())] = d
}

func newDVService(t testing.TB, store delivery.Store) *delivery.Service {
	t.Helper()
	svc, err := delivery.NewService(delivery.Config{Store: store, Access: dvAccess{}, Credentials: dvCredentials{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// dvRegistrar is the keyed registrar of the HTTP foundation: registration
// answers unavailable (no transaction is modelled).
type dvRegistrar struct{}

var _ deliveryhttp.Registrar = dvRegistrar{}

func (dvRegistrar) DevelopmentOnly() bool { return true }
func (dvRegistrar) RegisterKeyed(context.Context, httpapi.RequestIdentity, func(context.Context, httpapi.Reference) error,
	delivery.RegisterCommand, func(delivery.RegisterResult) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, delivery.RegisterResult, error) {
	return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, httpapi.UnavailableError(time.Second, errors.New("not modelled"))
}

// dvDecider is the keyed gate decision of the HTTP foundation.
type dvDecider struct{}

func (dvDecider) DevelopmentOnly() bool { return true }
func (dvDecider) DecideKeyed(context.Context, httpapi.RequestIdentity, func(context.Context, httpapi.Reference) error,
	*gates.DecisionPlan, func(*gates.Gate) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *gates.Gate, error) {
	return httpapi.StoredResponse{}, false, nil, httpapi.UnavailableError(time.Second, errors.New("not modelled"))
}

// ---- deployment records and cleanup scheduling -------------------------------

// dvRecords is the scheduler's deployment record boundary and execution's
// deployment-bound cleanup authorization over one table, as delivery's adapters
// answer both from one record.
type dvRecords struct {
	mu    sync.Mutex
	clock func() time.Time
	deps  map[string]*deliverycleanup.Candidate
	// scans counts Scan calls; outage fails them while set.
	scans  atomic.Int64
	outage error
}

func newDVRecords(clock func() time.Time) *dvRecords {
	return &dvRecords{clock: clock, deps: map[string]*deliverycleanup.Candidate{}}
}

func (*dvRecords) DevelopmentOnly() bool { return true }

func (r *dvRecords) add(scope domain.Scope, id string, expiresIn time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp := r.clock().Add(expiresIn)
	r.deps[string(scope.TenantID)+"/"+string(scope.ProjectID)+"/"+id] = &deliverycleanup.Candidate{Scope: scope, Deployment: id, ActionKey: "act:" + id, ExpiresAt: &exp}
}

func (r *dvRecords) get(scope domain.Scope, id string) *deliverycleanup.Candidate {
	return r.deps[string(scope.TenantID)+"/"+string(scope.ProjectID)+"/"+id]
}

func (r *dvRecords) Scan(_ context.Context, scope domain.Scope, after string, limit int) ([]deliverycleanup.Candidate, string, error) {
	r.scans.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outage != nil {
		return nil, "", r.outage
	}
	var out []deliverycleanup.Candidate
	for _, c := range r.deps {
		if c.Scope == scope && c.Deployment > after && !c.Deleted {
			out = append(out, *c)
		}
	}
	return out, "", nil
}

func (r *dvRecords) Get(_ context.Context, scope domain.Scope, id string) (deliverycleanup.Candidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(scope, id)
	if c == nil {
		return deliverycleanup.Candidate{}, deliverycleanup.ErrNotFound
	}
	return *c, nil
}

func (r *dvRecords) RequestCleanup(_ context.Context, scope domain.Scope, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(scope, id)
	if c == nil {
		return deliverycleanup.ErrNotFound
	}
	if c.RequestedAt == nil {
		t := r.clock()
		c.RequestedAt, c.State = &t, deliverycleanup.RecordRequested
	}
	return nil
}

// AuthorizeCleanup is execution.DeploymentCleanup: recorded in the scope,
// requested and not deleted.
func (r *dvRecords) AuthorizeCleanup(_ context.Context, scope domain.Scope, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(scope, id)
	if c == nil || c.RequestedAt == nil || c.Deleted {
		return execution.ErrDenied
	}
	return nil
}

// dvScopes lists a fixed set of scopes; hook, when set, runs first (to block or
// fail a sweep).
type dvScopes struct {
	list  []domain.Scope
	hook  func(context.Context) error
	calls atomic.Int64
}

func (*dvScopes) DevelopmentOnly() bool { return true }

func (s *dvScopes) Scopes(ctx context.Context, after *domain.Scope, limit int) ([]domain.Scope, error) {
	s.calls.Add(1)
	if s.hook != nil {
		if err := s.hook(ctx); err != nil {
			return nil, err
		}
	}
	var out []domain.Scope
	for _, sc := range s.list {
		if after != nil && sc.TenantID+"/"+domain.TenantID(sc.ProjectID) <= after.TenantID+"/"+domain.TenantID(after.ProjectID) {
			continue
		}
		out = append(out, sc)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type dvDefinitions map[domain.Scope]domain.DefinitionID

func (d dvDefinitions) CleanupDefinition(_ context.Context, scope domain.Scope) (domain.DefinitionID, error) {
	id, ok := d[scope]
	if !ok {
		return "", deliverycleanup.ErrNoDefinition
	}
	return id, nil
}

// ---- the assembled delivery slice ------------------------------------------------

// dvWorld is a delivery slice assembled over the execution fixture.
type dvWorld struct {
	f      *fixture
	in     DeliveryInputs
	slice  *DeliverySlice
	gates  *gatesmemory.Env
	agents *agents.Service
	repo   *agentstest.MemRepo
	rec    *dvRecords
	scopes *dvScopes
	store  *dvStore
}

// newDVInputs builds the slice's inputs over a built (not yet assembled)
// execution fixture. Nothing is assembled: tests mutate the inputs first.
func newDVInputs(t *testing.T, f *fixture) *dvWorld {
	t.Helper()
	w := &dvWorld{f: f, gates: gatesmemory.New(), repo: &agentstest.MemRepo{}, store: newDVStore()}
	auth := agentstest.NewAuthority()
	auth.Grant(fxScope, executiontest.Engineer, agents.ActorUser, agents.PermRegister, agents.PermList)
	draft := agentstest.Draft(t, "coding")
	svc, err := agents.NewService(agents.ServiceConfig{
		Repository: w.repo, Authority: auth, Compatibility: agentstest.Catalogue(t, draft), Approvals: agentstest.NewApprovals(),
		Clock: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.agents = svc
	w.rec = newDVRecords(f.m.Clock.Now)
	w.scopes = &dvScopes{list: []domain.Scope{fxScope}}
	w.in = DeliveryInputs{
		Env: Test, Tuning: fxTuning(), Cursors: fxCursors(t),
		Profiles: svc, Gates: w.gates.Svc, GateDecisions: dvDecider{},
		Delivery: newDVService(t, w.store), Deployments: dvRegistrar{},
		Records: w.rec, Scopes: w.scopes, Definitions: dvDefinitions{},
		Clock: f.m.Clock.Now, CleanupEvery: 5 * time.Millisecond,
	}
	return w
}

// assemble assembles the execution slice of the fixture and the delivery slice.
func (w *dvWorld) assemble(t *testing.T) *DeliverySlice {
	t.Helper()
	if w.f.slice == nil {
		slice, err := AssembleExecution(w.f.in)
		if err != nil {
			t.Fatalf("assemble execution: %v", err)
		}
		w.f.slice = slice
	}
	w.in.Slice = w.f.slice
	s, err := AssembleDelivery(w.in)
	if err != nil {
		t.Fatalf("assemble delivery: %v", err)
	}
	w.slice = s
	return s
}

// start runs the slice's workers in a worker group, as the process does.
func (w *dvWorld) start() *workerGroup {
	g := newWorkerGroup(context.Background(), nil)
	for _, wk := range w.slice.Workers {
		g.Go(wk.Name, wk.Run)
	}
	return g
}

func fxCursors(t testing.TB) *httpapi.CursorCodec {
	t.Helper()
	c, err := httpapi.NewCursorCodec(bytes32('c'))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
