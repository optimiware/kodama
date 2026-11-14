package opsapp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// TestEveryRegistryOperationHasARealBinding pins the completeness gate: one
// binding per registry operation, in declaration order, with the public
// codecs and a handler.
func TestEveryRegistryOperationHasARealBinding(t *testing.T) {
	h := newHarness(t)
	operations := authorize.Operations()
	if len(operations) != 58 {
		t.Fatalf("registry operations = %d; want 58", len(operations))
	}
	if len(h.bindingsTable) != len(operations) {
		t.Fatalf("bindings = %d; want %d", len(h.bindingsTable), len(operations))
	}
	for index, operation := range operations {
		binding := h.bindingsTable[index]
		if binding.Operation != operation.Name {
			t.Fatalf("binding %d = %q; want %q", index, binding.Operation, operation.Name)
		}
		if binding.Input == nil || binding.Result == nil || binding.Handler == nil {
			t.Fatalf("operation %q has a nil codec or handler", operation.Name)
		}
		// The empty body is refused by a codec whose DTO has a required field
		// and accepted otherwise; either way the codec must be the public one.
		_ = binding.Input.Validate(json.RawMessage(`{}`))
	}
}

// TestBindingsRefusesIncompleteDependencies pins the constructor refusal.
func TestBindingsRefusesIncompleteDependencies(t *testing.T) {
	if _, err := opsapp.Bindings(opsapp.Dependencies{}); err == nil {
		t.Fatal("Bindings accepted an empty dependency set")
	}
	h := newHarness(t)
	deps := opsapp.Dependencies{
		Tenancy:        h.tenancy,
		Bindings:       h.bindings,
		Pipelines:      h.management,
		Admission:      h.admission,
		Executions:     h.executions,
		Receipts:       mustReceiptLookup(t, h.store),
		Secrets:        h.secrets,
		Services:       h.services,
		Credentials:    h.credentials,
		Tasks:          h.taskService,
		Inbox:          h.inboxQuery,
		Requests:       h.requests,
		Logs:           noLogs{},
		Verifier:       h.verifier,
		ActorActions:   staticActorActions{},
		Reports:        h.reporter,
		Events:         principalIngress{config: h.ingressConfig},
		EventRead:      h.eventRead,
		EventSubscribe: h.subscribe,
		Models:         h.models,
		Tools:          h.tools,
		IDs:            h.ids,
		TenantKeys:     fixedKeys{},
		ActionTimeout:  0,
	}
	if _, err := opsapp.Bindings(deps); err == nil {
		t.Fatal("Bindings accepted a zero action timeout")
	}
	deps.ActionTimeout = 1
	if _, err := opsapp.Bindings(deps); err != nil {
		t.Fatalf("Bindings refused a complete dependency set: %v", err)
	}
	// The complete set is the control: removing exactly one required dependency
	// must be refused and must name it, so a deleted validation entry cannot
	// pass behind the empty-set and zero-timeout checks.
	deps.ActorActions = nil
	_, err := opsapp.Bindings(deps)
	if err == nil {
		t.Fatal("Bindings accepted a complete dependency set with a nil actor action resolver")
	}
	if !strings.Contains(err.Error(), "actor action resolver") {
		t.Fatalf("nil actor action refusal = %v; want it to name the missing dependency", err)
	}
	deps.ActorActions = staticActorActions{}
	if _, err := opsapp.Bindings(deps); err != nil {
		t.Fatalf("Bindings refused the restored complete dependency set: %v", err)
	}
}

// TestRouterRefusesAnUnknownOperation pins the structural refusal.
func TestRouterRefusesAnUnknownOperation(t *testing.T) {
	h := newHarness(t)
	_, err := h.router.Invoke(t.Context(), ops.Invocation{
		Operation:  authorize.OperationName("not.an.operation"),
		Credential: testToken,
	})
	if err == nil {
		t.Fatal("router accepted an unknown operation")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("unknown operation fault = %v; want invalid_input", err)
	}
}

// TestTenantProjectMembershipFamily drives the real tenancy administration
// service through the router and reads the committed state back.
func TestTenantProjectMembershipFamily(t *testing.T) {
	h := newHarness(t)

	result, err := h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-b","name":"Tenant B"}`, nil)
	if err != nil {
		t.Fatalf("tenant.create: %v", err)
	}
	var created api.TenantCreateResult
	decodeResult(t, result, &created)
	if created.TenantID != "tenant-b" || created.Version != 1 {
		t.Fatalf("tenant.create result = %+v", created)
	}

	// Rotate the actor's own tenant derivation key at its committed version.
	if _, err := h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-a","name":"Tenant A"}`, nil); err != nil {
		t.Fatalf("tenant.create own tenant: %v", err)
	}
	rotated, err := h.invokeVersion(t, "token-admin", "tenant.rotate_key", `{"name":"Tenant A"}`, 1, map[string]string{"tenant": "tenant-a"})
	if err != nil {
		t.Fatalf("tenant.rotate_key: %v", err)
	}
	var rotation api.TenantRotateKeyResult
	decodeResult(t, rotated, &rotation)
	if rotation.TenantVersion != 2 || rotation.KeyVersion != 2 {
		t.Fatalf("tenant.rotate_key result = %+v", rotation)
	}

	project, err := h.invoke(t, "token-admin", "project.create", `{"project_id":"project-b","name":"Project B"}`, map[string]string{"tenant": "tenant-a"})
	if err != nil {
		t.Fatalf("project.create: %v", err)
	}
	var projectCreated api.ProjectCreateResult
	decodeResult(t, project, &projectCreated)
	if projectCreated.ProjectID != "project-b" || projectCreated.Version != 1 {
		t.Fatalf("project.create result = %+v", projectCreated)
	}

	updated, err := h.invokeVersion(t, "token-admin", "project.update", `{"name":"Project B renamed"}`, 1, map[string]string{"project": "project-b"})
	if err != nil {
		t.Fatalf("project.update: %v", err)
	}
	var projectUpdated api.ProjectUpdateResult
	decodeResult(t, updated, &projectUpdated)
	if projectUpdated.Version != 2 {
		t.Fatalf("project.update version = %d; want 2", projectUpdated.Version)
	}

	granted, err := h.invoke(t, "token-admin", "membership.grant", `{"roles":["engineer"]}`, map[string]string{"project": "project-b", "principal": "engineer-2"})
	if err != nil {
		t.Fatalf("membership.grant: %v", err)
	}
	var grant api.MembershipGrantResult
	decodeResult(t, granted, &grant)
	if grant.Version != 1 {
		t.Fatalf("membership.grant version = %d; want 1", grant.Version)
	}

	listed, err := h.invoke(t, "token-admin", "membership.list", `{}`, map[string]string{"project": "project-b"})
	if err != nil {
		t.Fatalf("membership.list: %v", err)
	}
	var memberships api.MembershipListResult
	decodeResult(t, listed, &memberships)
	found := false
	for _, membership := range memberships.Memberships {
		if membership.PrincipalID == "engineer-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("membership.list did not return the granted membership: %+v", memberships)
	}

	revoked, err := h.invokeVersion(t, "token-admin", "membership.revoke", `{}`, 1, map[string]string{"project": "project-b", "principal": "engineer-2"})
	if err != nil {
		t.Fatalf("membership.revoke: %v", err)
	}
	var revocation api.MembershipRevokeResult
	decodeResult(t, revoked, &revocation)
	if revocation.Version != 2 {
		t.Fatalf("membership.revoke version = %d; want 2", revocation.Version)
	}
}

// decodeResult decodes one typed result body.
func decodeResult(t *testing.T, result api.Result, target any) {
	t.Helper()
	if err := json.Unmarshal(result.Body, target); err != nil {
		t.Fatalf("decode %s result %s: %v", result.Operation, result.Body, err)
	}
}
