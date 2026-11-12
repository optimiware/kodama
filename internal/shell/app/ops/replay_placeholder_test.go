package opsapp_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// TestReceiptReplayReturnsTheOriginalResult pins the idempotency mapping: an
// identical retry replays the committed result and a changed retry is refused.
func TestReceiptReplayReturnsTheOriginalResult(t *testing.T) {
	h := newHarness(t)
	first, err := h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-replay","name":"Replay"}`, nil)
	if err != nil {
		t.Fatalf("first tenant.create: %v", err)
	}
	second, err := h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-replay","name":"Replay"}`, nil)
	if err != nil {
		t.Fatalf("replayed tenant.create: %v", err)
	}
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("replay body %s differs from original %s", second.Body, first.Body)
	}

	_, err = h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-replay","name":"Changed"}`, nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("changed replay = %v; want changed_idempotency_content", err)
	}
}

// recordingTenancy records the complete command one handler builds.
type recordingTenancy struct {
	calls []tenancyadmin.TenantCreateCommand
}

func (recorder *recordingTenancy) CreateTenant(_ context.Context, command tenancyadmin.TenantCreateCommand) (tenancyadmin.TenantCreateResult, error) {
	recorder.calls = append(recorder.calls, command)
	return tenancyadmin.TenantCreateResult{TenantID: command.TenantID, Version: 1}, nil
}

func (*recordingTenancy) RotateTenantKey(context.Context, tenancyadmin.TenantKeyRotationCommand) (tenancyadmin.TenantKeyRotationResult, error) {
	return tenancyadmin.TenantKeyRotationResult{}, nil
}

func (*recordingTenancy) CreateProject(context.Context, tenancyadmin.ProjectCreateCommand) (tenancyadmin.ProjectCreateResult, error) {
	return tenancyadmin.ProjectCreateResult{}, nil
}

func (*recordingTenancy) UpdateProject(context.Context, tenancyadmin.ProjectUpdateCommand) (tenancyadmin.ProjectUpdateResult, error) {
	return tenancyadmin.ProjectUpdateResult{}, nil
}

func (*recordingTenancy) GrantMembership(context.Context, tenancyadmin.MembershipGrantCommand) (tenancyadmin.MembershipGrantResult, error) {
	return tenancyadmin.MembershipGrantResult{}, nil
}

func (*recordingTenancy) RevokeMembership(context.Context, tenancyadmin.MembershipRevokeCommand) (tenancyadmin.MembershipRevokeResult, error) {
	return tenancyadmin.MembershipRevokeResult{}, nil
}

func (*recordingTenancy) ListMemberships(context.Context, tenancyadmin.MembershipListCommand) (tenancyadmin.MembershipPage, error) {
	return tenancyadmin.MembershipPage{}, nil
}

// TestHandlerCallsTheCapabilityWithTheCompleteCommand proves a handler is not a
// placeholder: the recorded command carries the authenticated principal, the
// resolved scope, the tenant key, the caller key and the receipt content.
func TestHandlerCallsTheCapabilityWithTheCompleteCommand(t *testing.T) {
	h := newHarness(t)
	recorder := &recordingTenancy{}
	ownership, err := opsapp.NewOwnership(h.facts, opsapp.RegistryPermissions{})
	if err != nil {
		t.Fatalf("NewOwnership: %v", err)
	}
	bindings, err := opsapp.Bindings(opsapp.Dependencies{
		Tenancy:        recorder,
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
		ActionTimeout:  1,
	})
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     ownership,
		Executions: ownership,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}
	result, err := router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName("tenant.create"),
		Credential: "token-admin",
		Request: api.Request{
			Input:          []byte(`{"tenant_id":"tenant-new","name":"New"}`),
			IdempotencyKey: "caller-key-1",
		},
	})
	if err != nil {
		t.Fatalf("tenant.create: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("capability calls = %d; want 1", len(recorder.calls))
	}
	command := recorder.calls[0]
	if command.TenantID != id.TenantID("tenant-new") || command.Name != "New" {
		t.Fatalf("command identity = %+v", command)
	}
	if command.Actor.ID != testAdmin {
		t.Fatalf("command actor = %q; want %q", command.Actor.ID, testAdmin)
	}
	if command.CallerKey != "caller-key-1" {
		t.Fatalf("command caller key = %q", command.CallerKey)
	}
	if command.Key.TenantID != id.TenantID("tenant-new") || command.Key.Version != 1 {
		t.Fatalf("command tenant key = %+v", command.Key)
	}
	if len(command.Content.Bytes) == 0 {
		t.Fatal("command receipt content is empty")
	}
	var decoded api.TenantCreateResult
	decodeResult(t, result, &decoded)
	if decoded.TenantID != "tenant-new" || decoded.Version != 1 {
		t.Fatalf("result = %+v", decoded)
	}
	_ = token.TenantKey{}
}
