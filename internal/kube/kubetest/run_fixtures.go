package kubetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Fixtures of the run-resource adapter's tests: a retained-policy catalog, a
// resource registrar, a checkout provider, the namespace baseline of the
// run_shared model and canonical resource specs.

// RunScope is the scope of the fixture runs (tenant "a" of Tenant).
func RunScope() domain.Scope {
	id, _ := Tenant("a")
	return domain.Scope{TenantID: id, ProjectID: "prj_t114"}
}

// RunID is the fixture run.
const RunID domain.RunID = "run_t114"

// Catalog is an in-memory kube.PolicyCatalog: the retained, append-only policies.
type Catalog struct {
	mu     sync.Mutex
	bodies map[execution.PolicyPin]execution.ExecutionPolicy
	Err    error
	Calls  int
}

var _ kube.PolicyCatalog = (*Catalog)(nil)

// NewCatalog retains the policies and returns their pins in order.
func NewCatalog(t testing.TB, pols ...execution.ExecutionPolicy) (*Catalog, []execution.PolicyPin) {
	t.Helper()
	c := &Catalog{bodies: map[execution.PolicyPin]execution.ExecutionPolicy{}}
	var pins []execution.PolicyPin
	for _, p := range pols {
		pin, err := p.Pin()
		if err != nil {
			t.Fatal(err)
		}
		c.bodies[pin] = p
		pins = append(pins, pin)
	}
	return c, pins
}

// Policy implements kube.PolicyCatalog.
func (c *Catalog) Policy(_ context.Context, pin execution.PolicyPin) (execution.ExecutionPolicy, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls++
	if c.Err != nil {
		return execution.ExecutionPolicy{}, c.Err
	}
	b, ok := c.bodies[pin]
	if !ok {
		return execution.ExecutionPolicy{}, kube.ErrPolicyUnavailable
	}
	return b, nil
}

// Put retains (or replaces, for a test of a wrong body) the body of a pin.
func (c *Catalog) Put(pin execution.PolicyPin, body execution.ExecutionPolicy) {
	c.mu.Lock()
	c.bodies[pin] = body
	c.mu.Unlock()
}

// ResourceRegistrar emulates runtime admission's resource registration:
// idempotent for the identical credential, conflicting for another.
type ResourceRegistrar struct {
	mu     sync.Mutex
	digest map[string]runtimeauth.Digest
	Calls  []kube.ResourceRegisterCommand
	// Fail returns an error for the n-th call (1-based) before it is applied;
	// FailAfter applies the registration and then fails (a lost response).
	Fail      func(n int) error
	FailAfter func(n int) error
	// Revoked makes every registration refuse as a stopping resource.
	Revoked bool
}

var _ kube.ResourceRegistrar = (*ResourceRegistrar)(nil)

// NewResourceRegistrar builds an empty registrar.
func NewResourceRegistrar() *ResourceRegistrar {
	return &ResourceRegistrar{digest: map[string]runtimeauth.Digest{}}
}

// RegisterResource implements kube.ResourceRegistrar.
func (r *ResourceRegistrar) RegisterResource(_ context.Context, cmd kube.ResourceRegisterCommand) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, cmd)
	n := len(r.Calls)
	if r.Fail != nil {
		if err := r.Fail(n); err != nil {
			return err
		}
	}
	if r.Revoked {
		return kube.ErrRegistrationRevoked
	}
	k := string(cmd.Scope.TenantID) + "/" + string(cmd.RunID) + "/" + cmd.Key
	d := cmd.Credential.Digest()
	if prev, ok := r.digest[k]; ok && !prev.Equal(d) {
		return kube.ErrRegistrationConflict
	}
	r.digest[k] = d
	if r.FailAfter != nil {
		if err := r.FailAfter(n); err != nil {
			return err
		}
	}
	return nil
}

// Registered reports whether a credential is registered for the resource.
func (r *ResourceRegistrar) Registered(scope domain.Scope, run domain.RunID, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.digest[string(scope.TenantID)+"/"+string(run)+"/"+key]
	return ok
}

// Checkout is a kube.CheckoutProvider answering for exactly what it is asked.
type Checkout struct {
	mu    sync.Mutex
	CA    []byte
	Err   error
	Reqs  []kube.CheckoutRequest
	Wrong func(*kube.CheckoutMaterial)
}

var _ kube.CheckoutProvider = (*Checkout)(nil)

// NewCheckout builds a provider with a throwaway Git CA.
func NewCheckout(t testing.TB) *Checkout { return &Checkout{CA: CA(t)} }

// Checkout implements kube.CheckoutProvider.
func (c *Checkout) Checkout(_ context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Reqs = append(c.Reqs, req)
	if c.Err != nil {
		return kube.CheckoutMaterial{}, c.Err
	}
	m := kube.CheckoutMaterial{Scope: req.Scope, WorkspaceID: req.WorkspaceID, RepositoryURL: req.RepositoryURL, Credential: []byte("ro-user:ro-secret-value"), GitCA: c.CA}
	if c.Wrong != nil {
		c.Wrong(&m)
	}
	return m, nil
}

// RunBaseline provisions the fake cluster's namespace for the run_shared
// baseline: Baseline with the pinned Pod Security level and version.
func RunBaseline(f *Fake, p kube.ExecutionPolicy, tenant domain.TenantID, ps execution.PolicyPodSecurity) {
	Baseline(f, p, tenant)
	tgt, _ := p.Tenant(tenant)
	n := f.Namespaces[tgt.Namespace]
	n.Metadata.Labels = map[string]string{"pod-security.kubernetes.io/enforce": ps.Level, "pod-security.kubernetes.io/enforce-version": ps.Version}
	f.Namespaces[tgt.Namespace] = n
}

// Deadline is the fixture resource deadline.
var Deadline = time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)

// RunSpecs builds the canonical specs execution would derive for a run: network,
// workspace, preparation and one service "db", pinned to the body.
type RunSpecs struct {
	Body  execution.ExecutionPolicy
	Pin   execution.PolicyPin
	Scope domain.Scope
	Run   domain.RunID
}

// NewRunSpecs pins the body.
func NewRunSpecs(t testing.TB, body execution.ExecutionPolicy) RunSpecs {
	t.Helper()
	pin, err := body.Pin()
	if err != nil {
		t.Fatal(err)
	}
	return RunSpecs{Body: body, Pin: pin, Scope: RunScope(), Run: RunID}
}

func (s RunSpecs) base(t testing.TB, kind execution.ResourceKind, key, role string) execution.ResourceSpec {
	t.Helper()
	shape, err := s.Body.RoleDigest(role)
	if err != nil {
		t.Fatal(err)
	}
	return execution.ResourceSpec{
		Scope: s.Scope, RunID: s.Run, Backend: kube.Backend, Kind: kind, Key: key, Name: execution.ResourceName(s.Scope, s.Run, kind, key),
		Policy: s.Pin, RoleShape: shape, DeadlineAt: Deadline,
	}
}

// Workspace is the run's scratch spec.
func (s RunSpecs) Workspace(t testing.TB) execution.ResourceSpec {
	sp := s.base(t, execution.KindWorkspace, execution.KeyWorkspace, execution.RoleStep)
	sp.Workspace = &execution.WorkspaceSpec{SizeMiB: 2048, Initial: "git"}
	return sp
}

// Preparation is the run's one preparation spec.
func (s RunSpecs) Preparation(t testing.TB) execution.ResourceSpec {
	sp := s.base(t, execution.KindPreparation, execution.KeyPreparation, execution.RolePreparation)
	sp.Preparation = &execution.PreparationSpec{
		Image: s.Body.Roles[execution.RolePreparation].Image, RepositoryURL: "https://git.example.invalid/t114/repo.git",
		Commit: "1111111111111111111111111111111111111111", TimeoutSecs: 600, MaxBytes: 1 << 30, MaxAttempts: 2,
	}
	return sp
}

// ServiceImage is the fixture service image.
const ServiceImage = "registry.example.invalid/postgres@sha256:2222222222222222222222222222222222222222222222222222222222222222"

// Service is the spec of a service named name. With secrets it runs supervised.
func (s RunSpecs) Service(t testing.TB, name string, secrets bool) execution.ResourceSpec {
	sp := s.base(t, execution.KindService, execution.ServiceKey(name), execution.RoleService)
	svc := &execution.ServiceSpec{
		Name: name, Role: "general", Image: ServiceImage, Env: map[string]string{"POSTGRES_DB": "app"},
		Ports:     map[string]int64{"sql": 5432},
		Resources: execution.ServiceResourcesSpec{CPUMillis: 500, MemoryMiB: 512, EphemeralMiB: 256, ShmMiB: 64},
		Storage:   []execution.StorageSpec{{Path: "/var/lib/postgresql/data", SizeMiB: 1024}},
		Readiness: execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 60, PeriodSecs: 2, ProbeTimeout: 5, LossSecs: 30},
		Host:      execution.EndpointHost(s.Scope, s.Run, name),
	}
	if secrets {
		svc.Command = []string{"/usr/local/bin/docker-entrypoint.sh", "postgres"}
		svc.Secrets = []execution.SecretPin{{Ref: "secret://project/db-password", Version: 1, Name: "POSTGRES_PASSWORD", Delivery: "env"}}
	}
	sp.Service = svc
	return sp
}

// Network is the run's network spec for the services (and edges), under the body's model.
func (s RunSpecs) Network(t testing.TB, services []string, edges []execution.EdgeSpec, workloads map[string][]string) execution.ResourceSpec {
	sp := s.base(t, execution.KindNetwork, execution.KeyNetwork, execution.RoleStep)
	egress := map[string]string{}
	for _, names := range workloads {
		for _, n := range names {
			if d, ok := s.Body.EgressDigest(n); ok {
				egress[n] = d
			}
		}
	}
	sp.Network = &execution.NetworkSpec{
		Services: services, Edges: edges, Egress: egress, Workloads: workloads,
		Isolation: s.Body.Network.Isolation, PublicEgress: s.Body.Network.PublicEgress,
	}
	return sp
}
