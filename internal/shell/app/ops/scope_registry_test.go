package opsapp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// spyHandler counts handler invocations so a scope refusal can be proven to
// happen before any capability effect.
type spyHandler struct {
	inner ops.Handler
	calls *int32
}

func (spy spyHandler) Handle(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
	atomic.AddInt32(spy.calls, 1)
	return spy.inner.Handle(ctx, call)
}

// newSpyRouter rebuilds the complete binding table over the harness services
// with counting handlers.
func newSpyRouter(t *testing.T, h *harness) (*ops.Router, *int32) {
	t.Helper()
	bindings, err := opsapp.Bindings(h.deps)
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	calls := new(int32)
	wrapped := make([]ops.Binding, len(bindings))
	for index, binding := range bindings {
		wrapped[index] = ops.Binding{
			Operation: binding.Operation,
			Input:     binding.Input,
			Result:    binding.Result,
			Handler:   spyHandler{inner: binding.Handler, calls: calls},
		}
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.ownership,
		Executions: h.ownership,
		Bindings:   wrapped,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}
	return router, calls
}

// scopeCarriage renders the identity-scope route fields of one operation for
// both edges: REST params and the equivalent MCP input object.
func scopeCarriage(codec api.OperationCodec, values map[string]string) (map[string]string, json.RawMessage, bool) {
	params := make(map[string]string)
	fields := make(map[string]string)
	has := false
	for _, param := range codec.RouteParams {
		if param.Name != "tenant" && param.Name != "project" {
			continue
		}
		value, ok := values[param.Name]
		if !ok {
			continue
		}
		has = true
		params[param.Name] = value
		fields[param.Field] = value
	}
	if !has {
		return nil, json.RawMessage(`{}`), false
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return params, encoded, true
}

// scopeEffectState is the observable committed state a scope refusal must leave
// untouched.
type scopeEffectState struct {
	events []int
	audit  []int
	keys   int
}

func snapshotScopeEffects(h *harness, scopes []tenancy.Scope) scopeEffectState {
	state := scopeEffectState{keys: h.keys.callsMade()}
	for _, scope := range scopes {
		state.events = append(state.events, len(h.store.Events(scope)))
		state.audit = append(state.audit, len(h.store.Audit(scope)))
	}
	return state
}

// requireScopeRefusal asserts the uniform not_found of a foreign scope identity.
func requireScopeRefusal(t *testing.T, label string, err error) {
	t.Helper()
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("%s = %v; want not_found", label, err)
	}
}

// TestScopeIdentityRegistrySweep walks all 51 registry operations. For every
// operation it pins that the principal's own tenant/project is accepted by the
// scope resolver, that a foreign tenant (REST route or MCP input) and a
// project-scoped principal naming another project are uniformly not_found, and
// that the refusal happens before any handler, row, receipt, key or event.
func TestScopeIdentityRegistrySweep(t *testing.T) {
	h := newHarness(t)
	spy, handlerCalls := newSpyRouter(t, h)
	engineer, err := h.principals.Resolve(context.Background(), testToken)
	if err != nil {
		t.Fatalf("resolve engineer: %v", err)
	}
	admin, err := h.principals.Resolve(context.Background(), "token-admin")
	if err != nil {
		t.Fatalf("resolve admin: %v", err)
	}
	scopes := []tenancy.Scope{
		{TenantID: testTenant},
		{TenantID: testTenant, ProjectID: testProject},
		{TenantID: "tenant-b"},
		{TenantID: "tenant-b", ProjectID: "project-b"},
	}
	baseline := snapshotScopeEffects(h, scopes)

	for _, operation := range authorize.Operations() {
		name := string(operation.Name)
		codec, ok := codecForOperation(name)
		if !ok {
			t.Fatalf("%s has no public codec", name)
		}
		// The principal that would normally hold the operation's grant: a
		// tenant-wide admin for tenant administration, the project engineer for
		// everything else.
		credential, principal := testToken, engineer
		if operation.Scope == authorize.ScopeTenant {
			credential, principal = "token-admin", admin
		}

		// Own tenant/project is never refused by the resolver, on either edge.
		ownParams, ownInput, _ := scopeCarriage(codec, map[string]string{
			"tenant":  testTenant,
			"project": testProject,
		})
		if _, err := h.ownership.Resolve(context.Background(), principal, ops.Call{
			Operation: operation.Name,
			Request:   api.Request{Input: ownInput},
		}); err != nil {
			t.Fatalf("%s MCP own scope refused: %v", name, err)
		}
		if _, err := h.ownership.Resolve(context.Background(), principal, ops.Call{
			Operation: operation.Name,
			Request:   api.Request{Input: json.RawMessage(`{}`)},
			Params:    ownParams,
		}); err != nil {
			t.Fatalf("%s REST own scope refused: %v", name, err)
		}

		// A route-named tenant is read from the REST parameter or the MCP input
		// field the route table names, and a foreign one is not_found.
		tenantParams, tenantInput, hasTenant := scopeCarriage(codec, map[string]string{"tenant": "tenant-b"})
		if hasTenant {
			for _, invocation := range []ops.Invocation{
				{
					Operation:  operation.Name,
					Credential: credential,
					Request:    api.Request{Input: tenantInput, IdempotencyKey: "sweep-mcp-tenant-" + name},
				},
				{
					Operation:  operation.Name,
					Credential: credential,
					Request:    api.Request{Input: json.RawMessage(`{}`), IdempotencyKey: "sweep-rest-tenant-" + name},
					Params:     tenantParams,
				},
			} {
				before := atomic.LoadInt32(handlerCalls)
				_, err := spy.Invoke(executionRequest(), invocation)
				requireScopeRefusal(t, name+" foreign tenant", err)
				if atomic.LoadInt32(handlerCalls) != before {
					t.Fatalf("%s foreign tenant reached the handler", name)
				}
			}
		}

		// A project-scoped principal naming another project is not_found on
		// either edge, and the handler never runs.
		projectParams, projectInput, hasProject := scopeCarriage(codec, map[string]string{"project": "project-b"})
		if hasProject {
			for _, invocation := range []ops.Invocation{
				{
					Operation:  operation.Name,
					Credential: testToken,
					Request:    api.Request{Input: projectInput, IdempotencyKey: "sweep-mcp-project-" + name},
				},
				{
					Operation:  operation.Name,
					Credential: testToken,
					Request:    api.Request{Input: json.RawMessage(`{}`), IdempotencyKey: "sweep-rest-project-" + name},
					Params:     projectParams,
				},
			} {
				before := atomic.LoadInt32(handlerCalls)
				_, err := spy.Invoke(executionRequest(), invocation)
				requireScopeRefusal(t, name+" foreign project", err)
				if atomic.LoadInt32(handlerCalls) != before {
					t.Fatalf("%s foreign project reached the handler", name)
				}
				for _, scope := range []tenancy.Scope{
					{TenantID: testTenant, ProjectID: testProject},
					{TenantID: "tenant-b", ProjectID: "project-b"},
				} {
					if _, found := h.store.Receipt(receipt.Key{
						TenantID:    scope.TenantID,
						ProjectID:   scope.ProjectID,
						PrincipalID: engineer.ID,
						Operation:   name,
						CallerKey:   invocation.Request.IdempotencyKey,
					}); found {
						t.Fatalf("%s foreign project committed a receipt", name)
					}
				}
			}

			// A tenant-wide principal naming a project resolves inside its own
			// tenant, never inside the project's (unprovable) tenant.
			resolution, err := h.ownership.Resolve(context.Background(), admin, ops.Call{
				Operation: operation.Name,
				Request:   api.Request{Input: projectInput},
			})
			if err != nil {
				t.Fatalf("%s tenant-wide project resolution: %v", name, err)
			}
			if resolution.Scope.TenantID != testTenant {
				t.Fatalf("%s resolved tenant = %q; want %q", name, resolution.Scope.TenantID, testTenant)
			}
			if operation.Scope == authorize.ScopeProject && resolution.Scope.ProjectID != "project-b" {
				t.Fatalf("%s resolved project = %q; want project-b", name, resolution.Scope.ProjectID)
			}
		}
	}

	if got := snapshotScopeEffects(h, scopes); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("scope refusals changed committed state: %+v -> %+v", baseline, got)
	}
}

// TestMCPTenantIdentityIsConfined pins the tenant identity carried only in the
// MCP input: a foreign tenant_id is uniformly not_found before the handler,
// while the principal's own tenant still works.
func TestMCPTenantIdentityIsConfined(t *testing.T) {
	h := newHarness(t)
	spy, handlerCalls := newSpyRouter(t, h)
	keysBefore := h.keys.callsMade()

	before := atomic.LoadInt32(handlerCalls)
	_, err := spy.Invoke(executionRequest(), ops.Invocation{
		Operation:  authorize.OperationName("project.create"),
		Credential: "token-admin",
		Request: api.Request{Input: json.RawMessage(`{"tenant_id":"tenant-b","project_id":"project-x","name":"Foreign"}`),
			IdempotencyKey: "mcp-project-create-foreign"},
	})
	requireScopeRefusal(t, "MCP project.create foreign tenant", err)
	if atomic.LoadInt32(handlerCalls) != before {
		t.Fatal("MCP project.create foreign tenant reached the handler")
	}
	if _, found := h.store.Snapshot(tenancy.Scope{TenantID: "tenant-b"}, "projects", "project-x"); found {
		t.Fatal("MCP project.create created a project in the foreign tenant")
	}
	if _, found := h.store.Snapshot(tenancy.Scope{TenantID: testTenant}, "projects", "project-x"); found {
		t.Fatal("MCP project.create created a project in the caller tenant")
	}

	before = atomic.LoadInt32(handlerCalls)
	_, err = spy.Invoke(executionRequest(), ops.Invocation{
		Operation:  authorize.OperationName("tenant.rotate_key"),
		Credential: "token-admin",
		Request: api.Request{Input: json.RawMessage(`{"tenant_id":"tenant-b","name":"Tenant B"}`),
			ExpectedVersion: new(uint64(1)), IdempotencyKey: "mcp-rotate-foreign"},
	})
	requireScopeRefusal(t, "MCP tenant.rotate_key foreign tenant", err)
	if atomic.LoadInt32(handlerCalls) != before {
		t.Fatal("MCP tenant.rotate_key foreign tenant reached the handler")
	}
	if h.keys.callsMade() != keysBefore {
		t.Fatalf("foreign tenant minted %d tenant keys", h.keys.callsMade()-keysBefore)
	}

	// The same MCP carriage naming the principal's own tenant is accepted.
	created, err := h.invoke(t, "token-admin", "project.create", `{"tenant_id":"tenant-a","project_id":"project-own","name":"Own"}`, nil)
	if err != nil {
		t.Fatalf("MCP project.create own tenant: %v", err)
	}
	var project api.ProjectCreateResult
	decodeResult(t, created, &project)
	if project.ProjectID != "project-own" {
		t.Fatalf("MCP project.create own tenant = %+v", project)
	}

	// REST route carriage stays uniformly not_found.
	_, err = h.invoke(t, "token-admin", "project.create", `{"project_id":"project-route","name":"Route"}`, map[string]string{"tenant": "tenant-b"})
	requireScopeRefusal(t, "REST project.create foreign tenant", err)
	_, err = h.invokeVersion(t, "token-admin", "tenant.rotate_key", `{"name":"Tenant B"}`, 1, map[string]string{"tenant": "tenant-b"})
	requireScopeRefusal(t, "REST tenant.rotate_key foreign tenant", err)
	if h.keys.callsMade() != keysBefore {
		t.Fatalf("tenant keys minted = %d; want none", h.keys.callsMade())
	}
}

// TestTenantWideProjectStaysInCallerTenant pins that a tenant-wide principal
// naming a project that exists in another tenant reads inside its own tenant:
// the foreign row is uniformly not_found and is never exposed.
func TestTenantWideProjectStaysInCallerTenant(t *testing.T) {
	h := newHarness(t)
	foreign := tenancy.Scope{TenantID: "tenant-b", ProjectID: "project-b"}
	if _, err := h.store.Execute(context.Background(), foreign, store.Unit{
		Writes: []store.Write{{
			Table:  "secrets",
			ID:     "secret-foreign",
			Insert: true,
			Values: map[string]any{
				"name":        "FOREIGN",
				"ciphertext":  []byte("ciphertext"),
				"wrapped_key": []byte("wrapped"),
				"wrap_nonce":  []byte("wrap-nonce"),
				"key_ref":     "kms/foreign",
				"key_version": int64(1),
				"nonce":       make([]byte, 12),
				"version":     int64(1),
				"created_at":  fixedNow,
				"updated_at":  fixedNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed foreign secret: %v", err)
	}

	_, err := h.invoke(t, "token-admin", "secret.metadata.read", `{}`, map[string]string{
		"project": "project-b",
		"secret":  "secret-foreign",
	})
	requireScopeRefusal(t, "tenant-wide foreign project read", err)
}
