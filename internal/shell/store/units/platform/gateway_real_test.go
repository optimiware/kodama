package platform_test

// Real-adapter proof for the model gateway's authority boundary.
//
// The defect these tests close is that the gateway emitted every registry
// authority read with no target, so the real identity adapter rejected an
// execution_running or action_deadline read before any provider call. The tests
// below drive the real gateway.Gateway onto the real store-backed identity
// authority reader over the memory store, the real platform execution reader,
// the real tenancy administration service and the real memory commit units.
// Only the boundaries this package does not own stay faked: the credential
// resolver, the provider transport, the secret custody resolver and the
// execution-permission source. Every successful call must commit its usage row
// (and, when configured, its content row) in the real memory store.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

const (
	gwModel      = "small-model"
	gwUpstream   = "upstream-small"
	gwEndpoint   = "https://models.example.test/v1/chat/completions"
	gwCredential = "kodex.exec-1.credential-material"
	gwSecretName = id.SecretName("MODEL_API_KEY")
	gwSecret     = "provider-secret-value"
	gwServiceID  = id.PrincipalID("svc-model")
)

func gwCompletionJSON() string {
	return `{"id":"cmpl-1","model":"upstream-small","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
}

// gwPrincipals resolves every credential to one configured actor. The real
// authorizer, authority reader and execution reader perform the current-state
// checks themselves; this fake stands only for the credential boundary.
type gwPrincipals struct {
	principal identity.Principal
}

func (fixed gwPrincipals) Resolve(context.Context, string) (identity.Principal, error) {
	return fixed.principal, nil
}

// gwPermissionSource derives the execution permission from trusted
// configuration for every admitted execution read.
type gwPermissionSource struct{}

func (gwPermissionSource) Permission(context.Context, platform.ExecutionFacts) (authorize.ExecutionPermission, error) {
	return authorize.ExecConfiguredModels, nil
}

// gwIDs is the deterministic event/receipt identity source for the real
// tenancy administration service.
type gwIDs struct {
	mu   sync.Mutex
	next int
}

func (source *gwIDs) NewEventID() (id.EventID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *gwIDs) NewReceiptID() (id.ReceiptID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

// gwRecorder delegates every unit to the real memory store and records the
// committed units, so a test can assert the exact rows a call wrote.
type gwRecorder struct {
	inner *memory.Store
	mu    sync.Mutex
	units []store.Unit
}

func (executor *gwRecorder) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.mu.Lock()
	executor.units = append(executor.units, unit)
	executor.mu.Unlock()
	return executor.inner.Execute(ctx, scope, unit)
}

func (executor *gwRecorder) Read(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	return executor.inner.Read(ctx, scope, table, identity)
}

func (executor *gwRecorder) unitsFor(table string) []store.Unit {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	var units []store.Unit
	for _, unit := range executor.units {
		if len(unit.Writes) == 1 && unit.Writes[0].Table == table {
			units = append(units, unit)
		}
	}
	return units
}

// gwSecrets resolves exactly the configured secret name to one deterministic
// value and records the resolve count.
type gwSecrets struct {
	mu    sync.Mutex
	calls int
}

func (resolver *gwSecrets) Resolve(_ context.Context, request secret.ResolveRequest) ([]secret.Resolved, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls++
	resolved := make([]secret.Resolved, 0, len(request.Names))
	for _, name := range request.Names {
		resolved = append(resolved, secret.Resolved{Name: name, Value: gwSecret, Scope: request.Scope})
	}
	return resolved, nil
}

func (resolver *gwSecrets) count() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.calls
}

// gwTransport is the injected provider transport: it records the call count and
// never opens a socket.
type gwTransport struct {
	mu    sync.Mutex
	calls int
}

func (transport *gwTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.calls++
	transport.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(gwCompletionJSON()))),
	}, nil
}

func (transport *gwTransport) count() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.calls
}

// gwUsageIDs allocates deterministic usage row identities.
type gwUsageIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *gwUsageIDs) New() (gateway.UsageID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return gateway.UsageID(fmt.Sprintf("usage-%d", ids.next)), nil
}

// gwWorld is one real gateway composition over one memory store.
type gwWorld struct {
	t         *testing.T
	store     *memory.Store
	executor  *gwRecorder
	tenancy   *tenancyadmin.Service
	secrets   *gwSecrets
	transport *gwTransport
	gateway   *gateway.Gateway
}

func gwModelConfig() gateway.ModelConfig {
	return gateway.ModelConfig{
		Scope:               testScope(),
		Model:               gwModel,
		UpstreamModel:       gwUpstream,
		Endpoint:            gwEndpoint,
		SecretName:          gwSecretName,
		MaxRequestBytes:     4096,
		MaxResponseBytes:    4096,
		MaxCompletionTokens: 64,
		Timeout:             2 * time.Second,
		RecordContent:       true,
	}
}

func gwInvocation() gateway.Invocation {
	return gateway.Invocation{
		Credential: gwCredential,
		ProjectID:  testProject,
		Request: api.ChatCompletionRequest{
			Model:    gwModel,
			Messages: []api.ChatMessage{{Role: "user", Content: "hi"}},
		},
	}
}

func gwHumanEngineer() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-engineer"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

func gwServicePrincipal() identity.Principal {
	return identity.Principal{
		ID:      gwServiceID,
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

func newGWWorld(t *testing.T, principal identity.Principal) *gwWorld {
	t.Helper()
	mem := newStore()
	adapter, err := identityunits.New(identityunits.Config{
		Executor: mem,
		Scanner:  mem,
		Clock:    fixedClock{},
	})
	if err != nil {
		t.Fatalf("identityunits.New: %v", err)
	}
	admin, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             fixedClock{},
		IDs:               &gwIDs{},
		TenantCreate:      adapter,
		TenantKeyRotation: adapter,
		ProjectCreate:     adapter,
		ProjectUpdate:     adapter,
		MembershipGrant:   adapter,
		MembershipRevoke:  adapter,
		MembershipQuery:   adapter,
	})
	if err != nil {
		t.Fatalf("tenancyadmin.NewService: %v", err)
	}
	executions, err := platform.NewExecutionReader(mem.Controller(), gwPermissionSource{})
	if err != nil {
		t.Fatalf("platform.NewExecutionReader: %v", err)
	}
	executor := &gwRecorder{inner: mem}
	secrets := &gwSecrets{}
	transport := &gwTransport{}
	var contentMu sync.Mutex
	contentNext := 0
	content := gateway.StoreContentRecorder{
		Executor: executor,
		NewID: func() (string, error) {
			contentMu.Lock()
			defer contentMu.Unlock()
			contentNext++
			return fmt.Sprintf("content-%d", contentNext), nil
		},
	}
	instance, err := gateway.New(
		gateway.Config{Enabled: true, Models: []gateway.ModelConfig{gwModelConfig()}},
		gateway.Dependencies{
			Principals: gwPrincipals{principal: principal},
			Authorizer: authorize.NewMatrix(),
			Authority:  adapter,
			Secrets:    secrets,
			Store:      executor,
			Content:    content,
			Transport:  transport,
			Clock:      fixedClock{},
			NewID:      (&gwUsageIDs{}).New,
		},
		gateway.WithExecutionReader(executions),
	)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return &gwWorld{
		t:         t,
		store:     mem,
		executor:  executor,
		tenancy:   admin,
		secrets:   secrets,
		transport: transport,
		gateway:   instance,
	}
}

// seedAdmin grants the tenant-wide administration actor used to drive the real
// membership-grant service.
func (world *gwWorld) seedAdmin() {
	world.t.Helper()
	seedMembershipRoles(world.t, world.store, tenancy.Scope{TenantID: testTenant},
		"membership-admin", id.PrincipalID("principal-admin"), []tenancy.Role{tenancy.RoleAdmin})
}

// grantMembership commits one project membership through the real tenancy
// administration service and its real store adapter.
func (world *gwWorld) grantMembership(principal id.PrincipalID, roles ...tenancy.Role) {
	world.t.Helper()
	_, err := world.tenancy.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
		Actor: authorize.Principal{
			ID:      "principal-admin",
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: testTenant},
			Roles:   []tenancy.Role{tenancy.RoleAdmin},
			Enabled: true,
		},
		Membership: tenancy.Membership{
			Scope:       testScope(),
			PrincipalID: principal,
			Roles:       roles,
		},
		ExpectedVersion: 0,
		CallerKey:       "caller-grant-" + string(principal),
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{"grant":"` + string(principal) + `"}`)},
	})
	if err != nil {
		world.t.Fatalf("GrantMembership(%s): %v", principal, err)
	}
}

// requireCommittedUsage asserts exactly one usage row was written and that the
// real memory store holds it with the expected attribution.
func (world *gwWorld) requireCommittedUsage(principal id.PrincipalID, execution id.ExecutionID) {
	world.t.Helper()
	units := world.executor.unitsFor(gateway.UsageTable)
	if len(units) != 1 {
		world.t.Fatalf("usage units = %d; want exactly one", len(units))
	}
	write := units[0].Writes[0]
	if write.Values["principal_id"] != string(principal) {
		world.t.Errorf("usage principal = %v; want %q", write.Values["principal_id"], principal)
	}
	if execution == "" {
		if value := write.Values["execution_id"]; value != nil {
			world.t.Errorf("usage execution = %v; want SQL NULL for a non-execution caller", value)
		}
	} else if write.Values["execution_id"] != string(execution) {
		world.t.Errorf("usage execution = %v; want %q", write.Values["execution_id"], execution)
	}
	if write.Values["outcome"] != string(gateway.UsageSucceeded) || write.Values["usage_known"] != true {
		world.t.Errorf("usage outcome = %v/%v; want succeeded/known", write.Values["outcome"], write.Values["usage_known"])
	}
	if _, found := world.store.Snapshot(testScope(), gateway.UsageTable, write.ID); !found {
		world.t.Fatalf("usage row %q was not committed in the real memory store", write.ID)
	}
}

// requireCommittedContent asserts exactly one content row was written and that
// the real memory store holds it with the expected attribution.
func (world *gwWorld) requireCommittedContent(principal id.PrincipalID, execution id.ExecutionID) {
	world.t.Helper()
	units := world.executor.unitsFor(gateway.ContentTable)
	if len(units) != 1 {
		world.t.Fatalf("content units = %d; want exactly one", len(units))
	}
	write := units[0].Writes[0]
	if write.Values["principal_id"] != string(principal) {
		world.t.Errorf("content principal = %v; want %q", write.Values["principal_id"], principal)
	}
	if execution == "" {
		if value := write.Values["execution_id"]; value != nil {
			world.t.Errorf("content execution = %v; want SQL NULL for a non-execution caller", value)
		}
	} else if write.Values["execution_id"] != string(execution) {
		world.t.Errorf("content execution = %v; want %q", write.Values["execution_id"], execution)
	}
	if _, found := world.store.Snapshot(testScope(), gateway.ContentTable, write.ID); !found {
		world.t.Fatalf("content row %q was not committed in the real memory store", write.ID)
	}
}

// requireNoEffect asserts a refused call made no provider call, resolved no
// secret and committed no usage or content row.
func (world *gwWorld) requireNoEffect() {
	world.t.Helper()
	if got := world.transport.count(); got != 0 {
		world.t.Errorf("upstream calls = %d; want 0", got)
	}
	if got := world.secrets.count(); got != 0 {
		world.t.Errorf("secret resolves = %d; want 0", got)
	}
	if got := len(world.executor.unitsFor(gateway.UsageTable)); got != 0 {
		world.t.Errorf("usage writes = %d; want 0", got)
	}
	if got := len(world.executor.unitsFor(gateway.ContentTable)); got != 0 {
		world.t.Errorf("content writes = %d; want 0", got)
	}
}

// TestRealGatewayHumanCallerCommits proves a human engineer whose committed
// membership satisfies the real authority reader commits usage and content
// rows through the real memory store.
func TestRealGatewayHumanCallerCommits(t *testing.T) {
	principal := gwHumanEngineer()
	world := newGWWorld(t, principal)
	world.seedAdmin()
	world.grantMembership(principal.ID, tenancy.RoleEngineer)

	response, err := world.gateway.Invoke(context.Background(), gwInvocation())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if response.ID != "cmpl-1" {
		t.Errorf("completion id = %q; want cmpl-1", response.ID)
	}
	if got := world.transport.count(); got != 1 {
		t.Errorf("upstream calls = %d; want 1", got)
	}
	world.requireCommittedUsage(principal.ID, "")
	world.requireCommittedContent(principal.ID, "")
}

// TestRealGatewayServiceCallerCommits proves a service caller whose committed
// service row satisfies the real authority reader commits its usage row.
func TestRealGatewayServiceCallerCommits(t *testing.T) {
	principal := gwServicePrincipal()
	world := newGWWorld(t, principal)
	seedService(t, world.store, testScope(), principal.ID, "enabled")

	response, err := world.gateway.Invoke(context.Background(), gwInvocation())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if response.ID != "cmpl-1" {
		t.Errorf("completion id = %q; want cmpl-1", response.ID)
	}
	world.requireCommittedUsage(principal.ID, "")
	world.requireCommittedContent(principal.ID, "")
}

// TestRealGatewayExecutionCallerCommits proves a running execution with a
// running action passes the real authority reader with its execution and action
// targets and commits an execution-attributed usage row.
func TestRealGatewayExecutionCallerCommits(t *testing.T) {
	principal := executionActorPrincipal()
	world := newGWWorld(t, principal)
	seedExecution(t, world.store, testScope(), principal.ExecutionID, principal.ID, "running")
	seedContainerAction(t, world.store, testScope(), testAction, principal.ExecutionID, "running", fixedNow.Add(30*time.Minute))

	response, err := world.gateway.Invoke(context.Background(), gwInvocation())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if response.ID != "cmpl-1" {
		t.Errorf("completion id = %q; want cmpl-1", response.ID)
	}
	world.requireCommittedUsage(principal.ID, principal.ExecutionID)
	world.requireCommittedContent(principal.ID, principal.ExecutionID)
}

// TestRealGatewayRefusesStoppedExecution proves a stopped execution is refused
// by the real admitted-context check with no provider call and no usage row.
func TestRealGatewayRefusesStoppedExecution(t *testing.T) {
	principal := executionActorPrincipal()
	world := newGWWorld(t, principal)
	seedExecution(t, world.store, testScope(), principal.ExecutionID, principal.ID, "succeeded")

	_, err := world.gateway.Invoke(context.Background(), gwInvocation())
	requireClass(t, err, fault.Forbidden)
	world.requireNoEffect()
}

// TestRealGatewayRefusesExpiredDeadline proves an action whose deadline has
// passed is refused with deadline_expired and no provider call.
func TestRealGatewayRefusesExpiredDeadline(t *testing.T) {
	principal := executionActorPrincipal()
	world := newGWWorld(t, principal)
	seedExecution(t, world.store, testScope(), principal.ExecutionID, principal.ID, "running")
	seedContainerAction(t, world.store, testScope(), testAction, principal.ExecutionID, "running", fixedNow.Add(-time.Minute))

	_, err := world.gateway.Invoke(context.Background(), gwInvocation())
	requireClass(t, err, fault.DeadlineExpired)
	world.requireNoEffect()
}

// TestRealGatewayRefusesExecutionWithoutAction proves a running execution with
// no current action is refused before any authority read or effect.
func TestRealGatewayRefusesExecutionWithoutAction(t *testing.T) {
	principal := executionActorPrincipal()
	world := newGWWorld(t, principal)
	seedExecution(t, world.store, testScope(), principal.ExecutionID, principal.ID, "running")

	_, err := world.gateway.Invoke(context.Background(), gwInvocation())
	requireClass(t, err, fault.Forbidden)
	world.requireNoEffect()
}

// TestRealGatewayRefusesUnauthorizedHuman proves the real authority reader
// denies a human whose committed membership does not carry the operation's
// role, even when the resolved principal claims it.
func TestRealGatewayRefusesUnauthorizedHuman(t *testing.T) {
	principal := gwHumanEngineer()
	world := newGWWorld(t, principal)
	world.seedAdmin()
	world.grantMembership(principal.ID, tenancy.RoleViewer)

	_, err := world.gateway.Invoke(context.Background(), gwInvocation())
	requireClass(t, err, fault.Forbidden)
	world.requireNoEffect()
}
