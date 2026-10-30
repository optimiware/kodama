package rtest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// MemResources is the in-memory stand-in for the execution owner's committed
// resource state together with the resource grant store, for the run_shared
// resource subjects (T-113). It serializes every transaction behind one mutex
// and stages writes on a copy, so a transaction that fails commits nothing, the
// way a database transaction would. It is a test fixture and development-only:
// it proves the module's behavior, not execution's or Kubernetes'.
type MemResources struct {
	mu     sync.Mutex
	states map[runtimeauth.ResourceRef]runtimeauth.ResourceState
	grants map[runtimeauth.ResourceRef]runtimeauth.ResourceGrant

	Pods
	// hook, when set, runs after LockResource returned and before the transaction
	// continues (tests order a stop against an admission with it).
	hook func()
}

var (
	_ runtimeauth.ResourceStore     = (*MemResources)(nil)
	_ runtimeauth.ResourceObserver  = (*MemResources)(nil)
	_ runtimeauth.ResourceWorkloads = (*MemResources)(nil)
)

// NewMemResources returns an empty fixture.
func NewMemResources() *MemResources {
	return &MemResources{
		states: map[runtimeauth.ResourceRef]runtimeauth.ResourceState{},
		grants: map[runtimeauth.ResourceRef]runtimeauth.ResourceGrant{},
	}
}

// Pods is the controlled backend answer of runtimeauth.ResourceWorkloads:
// workload UID → the Pod names/UIDs that belong to it.
type Pods struct {
	podMu  sync.Mutex
	pods   map[string]map[string]string
	podErr error
}

// BindPod declares that a Pod belongs to the workload UID in the backend.
func (p *Pods) BindPod(workloadUID, podName, podUID string) {
	p.podMu.Lock()
	defer p.podMu.Unlock()
	if p.pods == nil {
		p.pods = map[string]map[string]string{}
	}
	if p.pods[workloadUID] == nil {
		p.pods[workloadUID] = map[string]string{}
	}
	p.pods[workloadUID][podName] = podUID
}

// SetPodError makes pod verification inconclusive.
func (p *Pods) SetPodError(err error) { p.podMu.Lock(); p.podErr = err; p.podMu.Unlock() }

// VerifyPod implements runtimeauth.ResourceWorkloads.
func (p *Pods) VerifyPod(_ context.Context, w runtimeauth.ResourceWorkload, podName, podUID string) error {
	p.podMu.Lock()
	defer p.podMu.Unlock()
	if p.podErr != nil {
		return p.podErr
	}
	if uid, ok := p.pods[w.UID][podName]; ok && uid == podUID {
		return nil
	}
	return runtimeauth.ErrNotObserved
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it.
func (*MemResources) DevelopmentOnly() bool { return true }

// Put stores or replaces the committed state of a resource.
func (m *MemResources) Put(st runtimeauth.ResourceState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[runtimeauth.ResourceRef{Scope: st.Scope, RunID: st.RunID, Key: st.Key}] = st
}

// Modify changes a committed state as the execution owner would.
func (m *MemResources) Modify(ref runtimeauth.ResourceRef, fn func(*runtimeauth.ResourceState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[ref]
	if !ok {
		panic("rtest: no such resource")
	}
	fn(&st)
	m.states[ref] = st
}

// Stop is the owner's stop intent: the resource ends and, when revoke is true,
// its grant is revoked in the same step. With revoke false it models an owner
// that only changed state, proving the currentness checks do not depend on
// revocation having run.
func (m *MemResources) Stop(ref runtimeauth.ResourceRef, revoke bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.states[ref]
	st.Phase = runtimeauth.ResourceEnded
	m.states[ref] = st
	if g, ok := m.grants[ref]; ok && revoke {
		g.Revoke(now)
		m.grants[ref] = g
	}
}

// ModifyLocked is Modify for a hook: the fixture's mutex is already held there.
func (m *MemResources) ModifyLocked(ref runtimeauth.ResourceRef, fn func(*runtimeauth.ResourceState)) {
	st, ok := m.states[ref]
	if !ok {
		panic("rtest: no such resource")
	}
	fn(&st)
	m.states[ref] = st
}

// SetHook sets the callback run inside each resource lock (with the fixture's
// mutex held: change state only through ModifyLocked).
func (m *MemResources) SetHook(fn func()) { m.mu.Lock(); m.hook = fn; m.mu.Unlock() }

// Grant returns the stored grant of a resource.
func (m *MemResources) Grant(ref runtimeauth.ResourceRef) (runtimeauth.ResourceGrant, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[ref]
	return g, ok
}

// ObserveResource implements runtimeauth.ResourceObserver.
func (m *MemResources) ObserveResource(_ context.Context, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[ref]
	if !ok {
		return runtimeauth.ResourceState{}, runtimeauth.ErrNotFound
	}
	return st, nil
}

// LocateResource implements runtimeauth.ResourceStore.
func (m *MemResources) LocateResource(_ context.Context, d runtimeauth.Digest) (runtimeauth.ResourceLocator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ref, g := range m.grants {
		if g.CredentialDigest.Equal(d) {
			return runtimeauth.ResourceLocator{Ref: ref}, nil
		}
	}
	return runtimeauth.ResourceLocator{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no such credential"))
}

// DoResource implements runtimeauth.ResourceStore.
func (m *MemResources) DoResource(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx runtimeauth.ResourceTx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memResourceTx{m: m, tenant: tenant, grants: make(map[runtimeauth.ResourceRef]runtimeauth.ResourceGrant, len(m.grants))}
	for k, v := range m.grants {
		tx.grants[k] = cloneResourceGrant(v)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	m.grants = tx.grants
	return nil
}

func cloneResourceGrant(g runtimeauth.ResourceGrant) runtimeauth.ResourceGrant {
	if g.Consumption != nil {
		c := *g.Consumption
		g.Consumption = &c
	}
	if g.RevokedAt != nil {
		r := *g.RevokedAt
		g.RevokedAt = &r
	}
	return g
}

type memResourceTx struct {
	m      *MemResources
	tenant domain.TenantID
	grants map[runtimeauth.ResourceRef]runtimeauth.ResourceGrant
}

var _ runtimeauth.ResourceTx = (*memResourceTx)(nil)

func (t *memResourceTx) LockResource(_ context.Context, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	if ref.Scope.TenantID != t.tenant {
		return runtimeauth.ResourceState{}, errors.Join(runtimeauth.ErrNotFound, errors.New("another tenant"))
	}
	if t.m.hook != nil {
		t.m.hook()
	}
	st, ok := t.m.states[ref]
	if !ok {
		return runtimeauth.ResourceState{}, errors.Join(runtimeauth.ErrNotFound, errors.New("no such resource"))
	}
	return st, nil
}

func (t *memResourceTx) ResourceGrantByCredential(_ context.Context, d runtimeauth.Digest, _ runtimeauth.LockMode) (runtimeauth.ResourceGrant, error) {
	for _, g := range t.grants {
		if g.Scope.TenantID == t.tenant && g.CredentialDigest.Equal(d) {
			return cloneResourceGrant(g), nil
		}
	}
	return runtimeauth.ResourceGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("no such grant"))
}

func (t *memResourceTx) ResourceGrantBySession(_ context.Context, s runtimeauth.SessionID, _ runtimeauth.LockMode) (runtimeauth.ResourceGrant, error) {
	if err := s.Validate(); err != nil {
		return runtimeauth.ResourceGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("malformed session"))
	}
	for _, g := range t.grants {
		if g.Scope.TenantID == t.tenant && g.Consumption != nil && g.Consumption.SessionID == s {
			return cloneResourceGrant(g), nil
		}
	}
	return runtimeauth.ResourceGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("no such grant"))
}

func (t *memResourceTx) InsertResourceGrant(_ context.Context, g runtimeauth.ResourceGrant) (runtimeauth.ResourceGrant, bool, error) {
	if g.Scope.TenantID != t.tenant {
		return runtimeauth.ResourceGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	if existing, ok := t.grants[g.Ref()]; ok {
		return cloneResourceGrant(existing), false, nil
	}
	for ref, other := range t.m.grants {
		if other.CredentialDigest.Equal(g.CredentialDigest) && ref != g.Ref() {
			return runtimeauth.ResourceGrant{}, false, errors.Join(runtimeauth.ErrConflict, errors.New("credential already registered elsewhere"))
		}
	}
	t.grants[g.Ref()] = cloneResourceGrant(g)
	return g, true, nil
}

func (t *memResourceTx) SaveResourceConsumption(_ context.Context, g runtimeauth.ResourceGrant) error {
	r, ok := t.grants[g.Ref()]
	if g.Consumption == nil || !ok || r.Consumption != nil || r.RevokedAt != nil {
		return errors.Join(runtimeauth.ErrConflict, errors.New("the grant is not consumable"))
	}
	c := *g.Consumption
	r.Consumption = &c
	t.grants[g.Ref()] = r
	return nil
}

func (t *memResourceTx) SaveResourceRevocation(_ context.Context, g runtimeauth.ResourceGrant) error {
	r, ok := t.grants[g.Ref()]
	if g.RevokedAt == nil || !ok {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no revocation to save"))
	}
	if r.RevokedAt == nil {
		v := *g.RevokedAt
		r.RevokedAt = &v
		t.grants[g.Ref()] = r
	}
	return nil
}

func (t *memResourceTx) RevokeResource(_ context.Context, ref runtimeauth.ResourceRef, now time.Time) (int, error) {
	r, ok := t.grants[ref]
	if !ok || r.RevokedAt != nil {
		return 0, nil
	}
	r.Revoke(now)
	t.grants[ref] = r
	return 1, nil
}

func (t *memResourceTx) RevokeRun(_ context.Context, scope domain.Scope, run domain.RunID, now time.Time) (int, error) {
	n := 0
	for ref, r := range t.grants {
		if ref.Scope == scope && ref.RunID == run && r.RevokedAt == nil {
			r.Revoke(now)
			t.grants[ref] = r
			n++
		}
	}
	return n, nil
}

// ---- builders of committed states --------------------------------------

// ResourceFixture names one resource of the test run.
type ResourceFixture struct {
	Scope       domain.Scope
	RunID       domain.RunID
	Key         string
	Fingerprint string
	WorkloadUID string
	DeadlineAt  time.Time
}

// Fingerprint is a fixed, well-formed resource fingerprint.
const Fingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testPolicyPin = runtimev2.PolicyPin{ID: "run-v1", Digest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}

func ts(t time.Time) runtimev2.Timestamp {
	return runtimev2.Timestamp(t.UTC().Format(time.RFC3339Nano))
}

// PreparationState builds the committed state of a run's preparation.
func PreparationState(f ResourceFixture) runtimeauth.ResourceState {
	return runtimeauth.ResourceState{
		Scope: f.Scope, RunID: f.RunID, Key: "preparation", Kind: runtimev2.ResourcePreparation, Phase: runtimeauth.ResourceActive,
		Fingerprint: fp(f.Fingerprint), WorkloadName: "kr-0123456789abcdef01234567", WorkloadUID: f.WorkloadUID, DeadlineAt: f.DeadlineAt,
		Context: runtimev2.ResourceContext{
			ContextVersion: runtimev2.ContextVersion, ResourceKind: runtimev2.ResourcePreparation,
			TenantID: f.Scope.TenantID, ProjectID: f.Scope.ProjectID, RunID: f.RunID, ResourceKey: "preparation",
			Preparation: &runtimev2.PreparationContext{
				RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: "1111111111111111111111111111111111111111",
				MountPath: "/workspace", TimeoutSeconds: 600, MaxBytes: 1 << 30, MaxAttempts: 2, DeadlineAt: ts(f.DeadlineAt),
			},
			Policy: testPolicyPin,
		},
	}
}

// ServiceState builds the committed state of the service NAME with the secret
// bindings it pins.
func ServiceState(f ResourceFixture, name string, secrets []runtimev1.SecretBinding) runtimeauth.ResourceState {
	if secrets == nil {
		secrets = []runtimev1.SecretBinding{}
	}
	return runtimeauth.ResourceState{
		Scope: f.Scope, RunID: f.RunID, Key: "service." + name, Kind: runtimev2.ResourceService, Phase: runtimeauth.ResourceActive,
		Fingerprint: fp(f.Fingerprint), WorkloadName: "s-ab12cd34ef567890-" + name, WorkloadUID: f.WorkloadUID, DeadlineAt: f.DeadlineAt,
		Context: runtimev2.ResourceContext{
			ContextVersion: runtimev2.ContextVersion, ResourceKind: runtimev2.ResourceService,
			TenantID: f.Scope.TenantID, ProjectID: f.Scope.ProjectID, RunID: f.RunID, ResourceKey: "service." + name,
			Service: &runtimev2.ServiceContext{
				Name: name, Role: runtimev2.RoleGeneral, DeadlineAt: ts(f.DeadlineAt), Secrets: secrets,
				Command: runtimev2.ServiceCommand{
					Image:   "registry.example.invalid/db@sha256:4444444444444444444444444444444444444444444444444444444444444444",
					Command: []string{"/usr/local/bin/docker-entrypoint.sh", "postgres"}, Args: []string{}, Env: map[string]string{"POSTGRES_DB": "app"},
				},
			},
			Policy: testPolicyPin,
		},
	}
}

func fp(s string) string {
	if s == "" {
		return Fingerprint
	}
	return s
}

// ---- run_shared attempts and frozen v2 scenarios -------------------------

// V2Step is one exchange of a frozen runtime v2 scenario.
type V2Step struct {
	API           string          `json:"api"`
	Op            string          `json:"op"`
	ResourceKind  string          `json:"resourceKind"`
	TokenAudience string          `json:"tokenAudience"`
	Request       json.RawMessage `json:"request"`
	Status        int             `json:"status"`
	Body          json.RawMessage `json:"body"`
}

// V2Scenario is a frozen scenario of contracts/runtime/v2/fixtures.
type V2Scenario struct {
	Name  string   `json:"name"`
	Steps []V2Step `json:"steps"`
}

// LoadV2Scenarios reads every scenario of the runtime v2 fixtures.
func LoadV2Scenarios(t testing.TB) []V2Scenario {
	t.Helper()
	_, file, _, _ := goruntime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "runtime", "v2", "fixtures", "scenarios.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []V2Scenario
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	return all
}

// V2Scenario returns one scenario by name.
func LoadV2Scenario(t testing.TB, name string) V2Scenario {
	t.Helper()
	for _, s := range LoadV2Scenarios(t) {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("scenario %q not found", name)
	return V2Scenario{}
}

// ContextV2Of decodes the context of a scenario step's admit response.
func ContextV2Of(t testing.TB, step V2Step) runtimev2.ContextV2 {
	t.Helper()
	var resp struct {
		Context json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(step.Body, &resp); err != nil {
		t.Fatal(err)
	}
	c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](resp.Context)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// SeedAttemptV2 creates an active run_shared attempt with the given pinned
// ContextV2 in the memory authority and returns it as an Attempt whose
// Workload/AdmitRequest helpers install and describe its Job.
func (m *MemStack) SeedAttemptV2(c runtimev2.ContextV2, actions runtimeauth.ActionSet) Attempt {
	m.T.Helper()
	scope := domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}
	digest, _, err := runtimeauth.PinContextV2(c)
	if err != nil {
		m.T.Fatal(err)
	}
	job := runtimeauth.ExpectedJob{Namespace: Namespace, Name: JobName(c.AttemptID)}
	m.Env.SeedAttempt(m.T, scope, c.RunID, c.AttemptID, job, digest, actions)
	return Attempt{Scope: scope, Context: runtimev1.Context{AttemptID: c.AttemptID, RunID: c.RunID}, Job: job, Actions: actions}
}

// RegisterV2With registers an existing credential for a run_shared attempt.
func (s *Stack) RegisterV2With(a Attempt, c runtimev2.ContextV2, cred runtimeauth.Credential, actions runtimeauth.ActionSet) (runtimeauth.Registration, error) {
	return s.Service.RegisterV2(context.Background(), runtimeauth.RegisterCommandV2{Scope: a.Scope, AttemptID: c.AttemptID, Credential: cred, Actions: actions, Context: c})
}
