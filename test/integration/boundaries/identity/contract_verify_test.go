//go:build integration

package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

func TestContractVerifyHumanRoundTripAndRedaction(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	principalID, err := shelloidc.PrincipalIDFor(provider.Issuer(), "auth0|human-1")
	if err != nil {
		t.Fatalf("PrincipalIDFor: %v", err)
	}
	options := defaultContractOptions()
	options.ActorRoles = map[string][]string{string(principalID): {"engineer"}}
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}

	verification, err := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Verify(context.Background(), validVerifyRequest(human.Value))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if response.Principal.ID != string(principalID) || response.Principal.Kind != "human" {
		t.Fatalf("principal = %+v; want human %q", response.Principal, principalID)
	}
	if response.VerificationID != "verification-0001" {
		t.Fatalf("verification id = %q; want the deterministic first id", response.VerificationID)
	}
	if len(response.Roles) != 1 || response.Roles[0] != "engineer" {
		t.Fatalf("roles = %v; want [engineer]", response.Roles)
	}
	if response.Approval {
		t.Fatal("a non-approval verification reported approval")
	}
	if server.VerificationCount() != 1 {
		t.Fatalf("verification count = %d; want 1", server.VerificationCount())
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal response: %v", err)
	}
	for _, secret := range []string{human.Value, fixtureServiceTok} {
		if containsSecret(string(encoded), secret) {
			t.Fatal("the verification response echoed credential material")
		}
		if containsSecret(server.RecordedJSON(), secret) {
			t.Fatal("the recorded request contains credential material")
		}
		if containsSecret(server.LogText(), secret) {
			t.Fatal("the fixture log contains credential material")
		}
	}
}

func TestContractStrictDecoding(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	server := newContract(t, provider, defaultContractOptions())
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	verifyInput, _ := json.Marshal(validVerifyRequest(human.Value))
	reportInput, _ := json.Marshal(validReportRequest("verification-0001"))

	cases := []struct {
		name        string
		path        string
		contentType string
		body        string
	}{
		{
			name: "unknown envelope member", path: "/api/v1/verify", contentType: "application/json",
			body: `{"input":` + string(verifyInput) + `,"unexpected_member":true}`,
		},
		{
			name: "unknown input member", path: "/api/v1/verify", contentType: "application/json",
			body: `{"input":{"actor_credential":"token","resource_id":"repo-1","operation":"git.ref.update","subject":{"fields":[]},"unexpected_member":true}}`,
		},
		{
			name: "non-object body", path: "/api/v1/verify", contentType: "application/json", body: `[]`,
		},
		{
			name: "missing content type", path: "/api/v1/verify", contentType: "", body: `{"input":` + string(verifyInput) + `}`,
		},
		{
			name: "report missing idempotency key", path: "/api/v1/reports", contentType: "application/json",
			body: `{"input":` + string(reportInput) + `}`,
		},
		{
			name: "report unknown input member", path: "/api/v1/reports", contentType: "application/json",
			body: `{"idempotency_key":"source-event-1","input":{"resource_id":"repo-1","source_event_id":"source-event-1","event_type":"git.ref.updated","operation":"git.ref.update","verification_id":"verification-0001","subject":{"fields":[]},"unexpected_member":true}}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := postRaw(t, server, testCase.path, testCase.contentType, []byte(testCase.body))
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body %s)", response.StatusCode, readBody(t, response))
			}
			var public api.Error
			decodeJSON(t, readBody(t, response), &public)
			if public.Code != api.CodeInvalidInput {
				t.Fatalf("code = %q; want invalid_input", public.Code)
			}
		})
	}
}

func TestContractServiceActorSeparation(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	principalID, _ := shelloidc.PrincipalIDFor(provider.Issuer(), "auth0|human-1")
	options := defaultContractOptions()
	options.ActorRoles = map[string][]string{string(principalID): {"engineer"}}
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}

	t.Run("human cannot call verification as a service", func(t *testing.T) {
		body, _ := json.Marshal(api.Request{Input: mustJSON(t, validVerifyRequest(human.Value))})
		response := postRawWithAuth(t, server, "/api/v1/verify", human.Value, "application/json", body)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", response.StatusCode)
		}
	})

	t.Run("execution cannot call verification as a service", func(t *testing.T) {
		body, _ := json.Marshal(api.Request{Input: mustJSON(t, validVerifyRequest(human.Value))})
		response := postRawWithAuth(t, server, "/api/v1/verify", fixtureExecutionTok, "application/json", body)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", response.StatusCode)
		}
	})

	t.Run("unknown caller is unauthenticated", func(t *testing.T) {
		body, _ := json.Marshal(api.Request{Input: mustJSON(t, validVerifyRequest(human.Value))})
		response := postRawWithAuth(t, server, "/api/v1/verify", "unknown-token", "application/json", body)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401", response.StatusCode)
		}
	})

	t.Run("service credential is not an actor credential", func(t *testing.T) {
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), validVerifyRequest(fixtureServiceTok))
		requireFaultClass(t, err, fault.Unauthenticated)
	})

	t.Run("service caller with a human actor verifies", func(t *testing.T) {
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		response, err := verification.Verify(context.Background(), validVerifyRequest(human.Value))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if response.Principal.Kind != "human" {
			t.Fatalf("principal kind = %q; want human", response.Principal.Kind)
		}
	})
}

func TestContractApprovalRequiresExecutionEvidence(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	options := defaultContractOptions()
	options.Approvals = []idfix.ApprovalFact{{
		ExecutionID: fixtureExecutionID,
		Assignment:  "role/reviewer",
		ResourceID:  fixtureResourceID,
		Operation:   string(api.ResourceOperationGitRefUpdate),
		Subject:     gitSubject("refs/heads/main", "1111111", "2222222"),
		Decision:    "pass",
	}}
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}

	t.Run("matching execution evidence is approved", func(t *testing.T) {
		request := validVerifyRequest(fixtureExecutionTok)
		request.Approval = "role/reviewer"
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		response, err := verification.Verify(context.Background(), request)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !response.Approval || response.Principal.Kind != "execution" {
			t.Fatalf("response = %+v; want execution approval", response)
		}
	})

	t.Run("human actor cannot carry approval", func(t *testing.T) {
		request := validVerifyRequest(human.Value)
		request.Approval = "role/reviewer"
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), request)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("changed effect subject refuses approval", func(t *testing.T) {
		request := validVerifyRequest(fixtureExecutionTok)
		request.Approval = "role/reviewer"
		request.Subject = gitSubject("refs/heads/main", "1111111", "3333333")
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), request)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("wrong assignment refuses approval", func(t *testing.T) {
		request := validVerifyRequest(fixtureExecutionTok)
		request.Approval = "role/operator"
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), request)
		requireFaultClass(t, err, fault.Forbidden)
	})
}

func TestContractBindingAndScopeRefusals(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	server := newContract(t, provider, defaultContractOptions())
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))

	// Positive baseline: the request used by the refusal cases below is
	// otherwise valid, so each refusal is attributable to the mutated field.
	t.Run("baseline request verifies", func(t *testing.T) {
		response, err := verification.Verify(context.Background(), validVerifyRequest(human.Value))
		if err != nil {
			t.Fatalf("baseline Verify: %v", err)
		}
		if response.VerificationID == "" {
			t.Fatal("baseline verification returned no id")
		}
	})

	t.Run("unknown resource is refused", func(t *testing.T) {
		request := validVerifyRequest(human.Value)
		request.ResourceID = "repo-2"
		_, err := verification.Verify(context.Background(), request)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("actor outside scope is refused", func(t *testing.T) {
		other, err := provider.MintAccessToken("auth0|human-2", "tenant-b", fixtureProject)
		if err != nil {
			t.Fatalf("MintAccessToken: %v", err)
		}
		_, err = verification.Verify(context.Background(), validVerifyRequest(other.Value))
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("disabled service is refused", func(t *testing.T) {
		disabled := defaultContractOptions()
		disabled.Services[0].Enabled = false
		disabledServer := newContract(t, provider, disabled)
		disabledVerification, _ := gitapp.NewVerificationClient(newTransport(t, disabledServer, serviceSource(), 1))
		_, err := disabledVerification.Verify(context.Background(), validVerifyRequest(human.Value))
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("disabled execution actor is refused", func(t *testing.T) {
		disabled := defaultContractOptions()
		disabled.Executions[0].Enabled = false
		disabledServer := newContract(t, provider, disabled)
		disabledVerification, _ := gitapp.NewVerificationClient(newTransport(t, disabledServer, serviceSource(), 1))
		_, err := disabledVerification.Verify(context.Background(), validVerifyRequest(fixtureExecutionTok))
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("binding in a different project does not authorize", func(t *testing.T) {
		// The caller and actor are both project-1; a binding that names a
		// different project must not match even though service, tenant,
		// resource and operation all agree.
		mismatched := defaultContractOptions()
		mismatched.Bindings[0].ProjectID = "project-2"
		mismatchedServer := newContract(t, provider, mismatched)
		mismatchedVerification, _ := gitapp.NewVerificationClient(newTransport(t, mismatchedServer, serviceSource(), 1))
		_, err := mismatchedVerification.Verify(context.Background(), validVerifyRequest(human.Value))
		requireFaultClass(t, err, fault.Forbidden)
	})
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}
