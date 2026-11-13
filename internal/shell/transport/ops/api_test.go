// Package ops_test pins the operation-router API and the behavior these tests
// require.
//
// # Exported API
//
//	func NewRouter(Config) (*Router, error)
//	func (*Router) Invoke(context.Context, Invocation) (api.Result, error)
//
//	type Config struct {
//	    Principals authorize.CurrentPrincipal
//	    Scopes     ScopeResolver
//	    Executions ExecutionAuthority
//	    Bindings   []Binding
//	}
//	type Binding struct {
//	    Operation authorize.OperationName
//	    Input     Codec
//	    Result    Codec
//	    Handler   Handler
//	}
//	type Codec interface{ Validate(json.RawMessage) error }
//	type Handler interface{ Handle(context.Context, AuthorizedCall) (api.Result, error) }
//	type AuthorizedCall struct {
//	    Principal authorize.Principal
//	    Operation authorize.Operation
//	    Scope     tenancy.Scope
//	    Targets   map[string]string
//	    Request   api.Request
//	}
//	type Call struct {
//	    Operation authorize.OperationName
//	    Request   api.Request
//	    Params    map[string]string
//	}
//	type Invocation struct {
//	    Operation  authorize.OperationName
//	    Credential string
//	    Request    api.Request
//	    Params     map[string]string
//	}
//	type Resolution struct {
//	    Scope   tenancy.Scope
//	    Targets map[string]string
//	}
//	type ScopeResolver interface {
//	    Resolve(context.Context, authorize.Principal, Call) (Resolution, error)
//	}
//	type ExecutionAuthority interface {
//	    Admit(context.Context, authorize.Principal) (authorize.AdmittedExecution, error)
//	}
//
// # Pinned router behavior
//
// The registry is immutable and complete: NewRouter accepts exactly one binding
// for every one of the 58 authorize registry operations and refuses a duplicate,
// missing, empty or unknown binding, a nil codec/handler and a nil dependency.
// It copies the caller's binding slice so later caller mutation cannot change a
// running router.
//
// Invoke refuses an unknown operation structurally, authenticates through the
// injected authorize.CurrentPrincipal boundary before any resource lookup,
// resolves scope/targets through the injected ScopeResolver, authorizes through
// authorize.NewMatrix for human/service principals and through
// authorize.AuthorizeExecution for execution principals, enforces the registry
// receipt policy, validates the request envelope and the binding's input codec,
// refuses a REST path/body disagreement, and only then calls the handler. The
// handler receives the verified principal, the registry operation, the resolved
// scope/targets and the unchanged request. The binding result codec validates
// the handler result. A handler fault keeps its public class/code and message;
// a non-fault handler error, an invalid result or a result-codec failure becomes
// internal_failure with a sanitized message that never renders a private cause.
//
// The router owns no receipt store. A capability handler owns the atomic
// effect+receipt commit; the router preserves the request bytes, expected
// version and idempotency key so an identical retry returns the handler's
// original result (including Replayed) and a changed retry is refused by the
// capability with changed_idempotency_content. Every invocation, including a
// retry, re-authenticates before any handler can expose a cached result.
package ops_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

const (
	testTenant    = id.TenantID("tenant-a")
	testProject   = id.ProjectID("project-a")
	testHuman     = id.PrincipalID("human-1")
	testService   = id.PrincipalID("service-1")
	testExecution = id.ExecutionID("exec-1")
	testAction    = id.ActionID("action-1")
	testToken     = "bearer-token-1"
	testVersion   = uint64(7)
)

var testDeadline = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant}
}

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func scopeFor(operation authorize.Operation) tenancy.Scope {
	if operation.Scope == authorize.ScopeTenant {
		return tenantScope()
	}
	return projectScope()
}

func humanPrincipal(scope tenancy.Scope, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      testHuman,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 3,
	}
}

func servicePrincipal(scope tenancy.Scope, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      testService,
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 4,
	}
}

func executionPrincipal(scope tenancy.Scope) authorize.Principal {
	return authorize.Principal{
		ID:          testHuman,
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     5,
		ExecutionID: testExecution,
	}
}

// orderRecorder is a deterministic call-order log shared by the fakes.
type orderRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (recorder *orderRecorder) add(step string) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.steps = append(recorder.steps, step)
}

func (recorder *orderRecorder) snapshot() []string {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.steps...)
}

type fakePrincipals struct {
	mu        sync.Mutex
	principal authorize.Principal
	err       error
	calls     int
	tokens    []string
	order     *orderRecorder
}

func (fake *fakePrincipals) Resolve(_ context.Context, token string) (authorize.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.tokens = append(fake.tokens, token)
	fake.order.add("authenticate")
	return fake.principal, fake.err
}

func (fake *fakePrincipals) set(principal authorize.Principal, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.principal = principal
	fake.err = err
}

func (fake *fakePrincipals) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakePrincipals) lastToken() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokens) == 0 {
		return ""
	}
	return fake.tokens[len(fake.tokens)-1]
}

type fakeScopes struct {
	mu         sync.Mutex
	resolution ops.Resolution
	err        error
	calls      int
	last       ops.Call
	order      *orderRecorder
}

func (fake *fakeScopes) Resolve(_ context.Context, _ authorize.Principal, call ops.Call) (ops.Resolution, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.last = call
	fake.order.add("resolve")
	return fake.resolution, fake.err
}

func (fake *fakeScopes) set(resolution ops.Resolution, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.resolution = resolution
	fake.err = err
}

func (fake *fakeScopes) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeScopes) lastCall() ops.Call {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.last
}

type fakeExecutions struct {
	mu       sync.Mutex
	admitted authorize.AdmittedExecution
	err      error
	calls    int
	last     authorize.Principal
	order    *orderRecorder
}

func (fake *fakeExecutions) Admit(_ context.Context, principal authorize.Principal) (authorize.AdmittedExecution, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.last = principal
	fake.order.add("admit")
	return fake.admitted, fake.err
}

func (fake *fakeExecutions) set(admitted authorize.AdmittedExecution, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.admitted = admitted
	fake.err = err
}

func (fake *fakeExecutions) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

type recordingCodec struct {
	mu    sync.Mutex
	err   error
	calls int
	last  json.RawMessage
	step  string
	order *orderRecorder
}

func (codec *recordingCodec) Validate(raw json.RawMessage) error {
	codec.mu.Lock()
	defer codec.mu.Unlock()
	codec.calls++
	codec.last = append(json.RawMessage(nil), raw...)
	codec.order.add(codec.step)
	return codec.err
}

func (codec *recordingCodec) setError(err error) {
	codec.mu.Lock()
	defer codec.mu.Unlock()
	codec.err = err
}

func (codec *recordingCodec) callCount() int {
	codec.mu.Lock()
	defer codec.mu.Unlock()
	return codec.calls
}

type recordingHandler struct {
	mu    sync.Mutex
	fn    func(context.Context, ops.AuthorizedCall) (api.Result, error)
	calls int
	last  ops.AuthorizedCall
	step  string
	order *orderRecorder
}

func (handler *recordingHandler) Handle(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
	handler.mu.Lock()
	handler.calls++
	handler.last = call
	handler.order.add(handler.step)
	fn := handler.fn
	handler.mu.Unlock()
	if fn != nil {
		return fn(ctx, call)
	}
	return api.Result{Operation: string(call.Operation.Name), Body: json.RawMessage(`{"ok":true}`)}, nil
}

func (handler *recordingHandler) setFunc(fn func(context.Context, ops.AuthorizedCall) (api.Result, error)) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.fn = fn
}

func (handler *recordingHandler) callCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.calls
}

func (handler *recordingHandler) lastCall() ops.AuthorizedCall {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.last
}

type probe struct {
	input   *recordingCodec
	result  *recordingCodec
	handler *recordingHandler
}

type harness struct {
	t          *testing.T
	order      *orderRecorder
	principals *fakePrincipals
	scopes     *fakeScopes
	executions *fakeExecutions
	probes     map[authorize.OperationName]*probe
	bindings   []ops.Binding
	router     *ops.Router
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		order:  &orderRecorder{},
		probes: make(map[authorize.OperationName]*probe, len(authorize.Operations())),
	}
	h.principals = &fakePrincipals{
		principal: humanPrincipal(projectScope(), tenancy.RoleEngineer),
		order:     h.order,
	}
	h.scopes = &fakeScopes{
		resolution: ops.Resolution{Scope: projectScope(), Targets: map[string]string{}},
		order:      h.order,
	}
	h.executions = &fakeExecutions{order: h.order}

	for _, operation := range authorize.Operations() {
		entry := &probe{
			input:   &recordingCodec{step: "input", order: h.order},
			result:  &recordingCodec{step: "result", order: h.order},
			handler: &recordingHandler{step: "handle", order: h.order},
		}
		h.probes[operation.Name] = entry
		h.bindings = append(h.bindings, ops.Binding{
			Operation: operation.Name,
			Input:     entry.input,
			Result:    entry.result,
			Handler:   entry.handler,
		})
	}

	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.scopes,
		Executions: h.executions,
		Bindings:   h.bindings,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	h.router = router
	return h
}

// prepare selects a principal and scope the real authorize matrix grants for
// one registry operation. It keeps the 58-operation parity and receipt-policy
// tables on the production grant matrix instead of an allow-all fake.
func (h *harness) prepare(operation authorize.Operation) {
	h.t.Helper()
	scope := scopeFor(operation)
	switch {
	case len(operation.Requires.HumanRoles) > 0:
		h.principals.set(humanPrincipal(scope, operation.Requires.HumanRoles[0]), nil)
		h.executions.set(authorize.AdmittedExecution{}, nil)
	case len(operation.Requires.ServiceRoles) > 0:
		h.principals.set(servicePrincipal(scope, operation.Requires.ServiceRoles[0]), nil)
		h.executions.set(authorize.AdmittedExecution{}, nil)
	default:
		principal := executionPrincipal(scope)
		h.principals.set(principal, nil)
		h.executions.set(authorize.AdmittedExecution{
			Principal:   principal,
			ExecutionID: testExecution,
			ActionID:    testAction,
			Permission:  operation.Requires.Execution,
			Deadline:    testDeadline,
		}, nil)
	}
	h.scopes.set(ops.Resolution{Scope: scope, Targets: map[string]string{}}, nil)
}

func (h *harness) probeFor(name authorize.OperationName) *probe {
	h.t.Helper()
	entry, ok := h.probes[name]
	if !ok {
		h.t.Fatalf("no probe for operation %q", name)
	}
	return entry
}

func (h *harness) invoke(name authorize.OperationName, request api.Request, params map[string]string) (api.Result, error) {
	h.t.Helper()
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  name,
		Credential: testToken,
		Request:    request,
		Params:     params,
	})
}

// acceptingCodec and fixedHandler are the minimal complete bindings used by
// constructor tests that do not need recording probes.
type acceptingCodec struct{}

func (acceptingCodec) Validate(json.RawMessage) error { return nil }

type fixedHandler struct {
	operation authorize.OperationName
}

func (handler fixedHandler) Handle(_ context.Context, call ops.AuthorizedCall) (api.Result, error) {
	return api.Result{Operation: string(call.Operation.Name), Body: json.RawMessage(`{"ok":true}`)}, nil
}

func buildBindings() []ops.Binding {
	operations := authorize.Operations()
	bindings := make([]ops.Binding, 0, len(operations))
	for _, operation := range operations {
		bindings = append(bindings, ops.Binding{
			Operation: operation.Name,
			Input:     acceptingCodec{},
			Result:    acceptingCodec{},
			Handler:   fixedHandler{operation: operation.Name},
		})
	}
	return bindings
}

func mustCode(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error is nil; want class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want class %q", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %q; want %q (error %v)", class, want, err)
	}
}
