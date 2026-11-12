package opsapp_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// recordingActorActions is the consumer-owned actor-action seam. It records
// every credential the handler forwarded and returns the configured action (or
// refusal), so the test can prove the handler uses the server-side resolution
// rather than a request value or a constant.
type recordingActorActions struct {
	actions     map[string]id.ActionID
	err         error
	credentials []string
}

func (resolver *recordingActorActions) ResolveActorAction(_ context.Context, credential string) (id.ActionID, error) {
	resolver.credentials = append(resolver.credentials, credential)
	if resolver.err != nil {
		return "", resolver.err
	}
	return resolver.actions[credential], nil
}

// capturingVerifier records the exact attempt the handler builds. A refused
// resolution must never reach it, so a count proves no verification record was
// requested.
type capturingVerifier struct {
	attempts []verify.Attempt
}

func (verifier *capturingVerifier) Verify(_ context.Context, attempt verify.Attempt) (verify.Record, error) {
	verifier.attempts = append(verifier.attempts, attempt)
	return verify.Record{
		ID: verify.VerificationID("verification-action-test"),
		Actor: identity.Principal{
			ID:          "actor-captured",
			Kind:        identity.KindExecution,
			Scope:       tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
			Enabled:     true,
			Version:     1,
			ExecutionID: "execution-captured",
		},
		Approved: false,
	}, nil
}

// setUpVerifyBinding prepares the calling service and its resource binding for
// actor.verify. It mirrors the real setup path of TestVerifyAndReportFamily.
func setUpVerifyBinding(t *testing.T, h *harness) {
	t.Helper()
	h.seedResource(t, tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, "resource-1", 1)
	if _, err := h.invoke(t, testToken, "service.create", `{"service_id":"service-1","roles":["engineer"]}`, map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create: %v", err)
	}
	h.principals.principals["token-service"] = identity.Principal{
		ID:      "service-1",
		Kind:    identity.KindService,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	if _, err := h.invoke(t, testToken, "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "binding-1",
		"resource_id":       "resource-1",
		"enforcing_service": "service-1",
		"trusted_origins":   []string{"https://example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
	}), map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register: %v", err)
	}
}

// verifyRequest builds one actor.verify body with the given actor credential.
func verifyRequest(t *testing.T, actorCredential string) string {
	t.Helper()
	return jsonBody(t, map[string]any{
		"actor_credential": actorCredential,
		"resource_id":      "resource-1",
		"operation":        "git.ref.update",
		"subject": map[string]any{"fields": []map[string]string{
			{"name": "operation", "kind": "string", "value": "git.ref.update"},
			{"name": "resource", "kind": "string", "value": "resource-1"},
		}},
	})
}

// TestActorVerifyPassesResolvedExecutionAction proves the handler forwards the
// action identity resolved from the actor credential, never an empty action, a
// request value or another execution's action.
func TestActorVerifyPassesResolvedExecutionAction(t *testing.T) {
	h := newHarness(t)
	setUpVerifyBinding(t, h)

	actions := &recordingActorActions{actions: map[string]id.ActionID{
		"actor-execution": "action-current",
		"actor-other":     "action-other-execution",
	}}
	verifier := &capturingVerifier{}
	h.actorActions = actions
	h.verifierSeam = verifier
	h.buildRouter(t)

	if _, err := h.invoke(t, "token-service", "actor.verify", verifyRequest(t, "actor-execution"), nil); err != nil {
		t.Fatalf("actor.verify: %v", err)
	}

	if len(actions.credentials) != 1 || actions.credentials[0] != "actor-execution" {
		t.Fatalf("resolver credentials = %q; want exactly [actor-execution]", actions.credentials)
	}
	if len(verifier.attempts) != 1 {
		t.Fatalf("verifier attempts = %d; want 1", len(verifier.attempts))
	}
	attempt := verifier.attempts[0]
	if attempt.ActionID != "action-current" {
		t.Fatalf("attempt action = %q; want the resolved current action %q", attempt.ActionID, "action-current")
	}
	if attempt.ActionID == "action-other-execution" {
		t.Fatalf("attempt action = %q; want never another execution's action", attempt.ActionID)
	}
	if attempt.Credential != "actor-execution" {
		t.Fatalf("attempt credential = %q; want the forwarded actor credential", attempt.Credential)
	}
	if attempt.Service.ID != "service-1" {
		t.Fatalf("attempt service = %q; want the authenticated calling service", attempt.Service.ID)
	}
	if attempt.Scope != (tenancy.Scope{TenantID: testTenant, ProjectID: testProject}) {
		t.Fatalf("attempt scope = %+v; want the resolved project scope", attempt.Scope)
	}
}

// TestActorVerifyRefusesUnresolvableExecutionAction proves a credential whose
// execution has no running container action is refused before any verification
// record is requested.
func TestActorVerifyRefusesUnresolvableExecutionAction(t *testing.T) {
	h := newHarness(t)
	setUpVerifyBinding(t, h)

	resolutionError := fault.New(fault.DependencyUnavailable, "execution has no running container workload")
	actions := &recordingActorActions{err: resolutionError}
	verifier := &capturingVerifier{}
	h.actorActions = actions
	h.verifierSeam = verifier
	h.buildRouter(t)

	_, err := h.invoke(t, "token-service", "actor.verify", verifyRequest(t, "actor-execution"), nil)
	if err == nil {
		t.Fatal("actor.verify accepted a credential with no running container action")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("no-action refusal class = %v; want dependency_unavailable (%v)", class, err)
	}
	if len(actions.credentials) != 1 {
		t.Fatalf("resolver credentials = %q; want the forwarded credential", actions.credentials)
	}
	if len(verifier.attempts) != 0 {
		t.Fatalf("verifier attempts = %d; want none: no verification record for an unresolvable action", len(verifier.attempts))
	}
}

// TestActorVerifyHumanAndServiceNameNoAction proves non-execution actors still
// verify through the handler while carrying no action identity.
func TestActorVerifyHumanAndServiceNameNoAction(t *testing.T) {
	for _, actorCredential := range []string{"actor-human", "actor-service"} {
		t.Run(actorCredential, func(t *testing.T) {
			h := newHarness(t)
			setUpVerifyBinding(t, h)

			actions := &recordingActorActions{}
			verifier := &capturingVerifier{}
			h.actorActions = actions
			h.verifierSeam = verifier
			h.buildRouter(t)

			if _, err := h.invoke(t, "token-service", "actor.verify", verifyRequest(t, actorCredential), nil); err != nil {
				t.Fatalf("actor.verify: %v", err)
			}
			if len(verifier.attempts) != 1 {
				t.Fatalf("verifier attempts = %d; want 1", len(verifier.attempts))
			}
			if attempt := verifier.attempts[0]; attempt.ActionID != "" {
				t.Fatalf("%s attempt action = %q; want no action", actorCredential, attempt.ActionID)
			}
		})
	}
}

// TestActorVerifyNeverAcceptsClientActionID proves the public request cannot
// inject the action identity: an action_id field is refused by the strict codec
// before the handler resolves or verifies anything.
func TestActorVerifyNeverAcceptsClientActionID(t *testing.T) {
	h := newHarness(t)
	setUpVerifyBinding(t, h)

	actions := &recordingActorActions{actions: map[string]id.ActionID{"actor-execution": "action-current"}}
	verifier := &capturingVerifier{}
	h.actorActions = actions
	h.verifierSeam = verifier
	h.buildRouter(t)

	body := jsonBody(t, map[string]any{
		"actor_credential": "actor-execution",
		"resource_id":      "resource-1",
		"operation":        "git.ref.update",
		"action_id":        "action-client-supplied",
		"subject": map[string]any{"fields": []map[string]string{
			{"name": "operation", "kind": "string", "value": "git.ref.update"},
			{"name": "resource", "kind": "string", "value": "resource-1"},
		}},
	})
	if _, err := h.invoke(t, "token-service", "actor.verify", body, nil); err == nil {
		t.Fatal("actor.verify accepted a client-supplied action_id")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("client action_id refusal class = %v; want invalid_input (%v)", class, err)
	}
	if len(actions.credentials) != 0 || len(verifier.attempts) != 0 {
		t.Fatalf("client action_id reached resolution (%d) or verification (%d)",
			len(actions.credentials), len(verifier.attempts))
	}
}
