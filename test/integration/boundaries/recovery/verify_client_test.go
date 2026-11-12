//go:build integration

package recovery_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// fixedResultTransport answers every invocation with one fixed public result.
type fixedResultTransport struct {
	operation string
	body      []byte
}

func (transport fixedResultTransport) Invoke(context.Context, api.Invocation) (api.Result, error) {
	return api.Result{Operation: transport.operation, Body: transport.body}, nil
}

// TestVerificationClientRefusesMismatchedOperationWithValidBody proves the
// result-envelope operation check is not masked by body decoding: a body that
// is valid for actor.verify but named resource.report must still be refused.
func TestVerificationClientRefusesMismatchedOperationWithValidBody(t *testing.T) {
	body, err := json.Marshal(api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "oidc-0123456789abcdef0123456789abcdef", Kind: "human"},
		VerificationID: "verification-0001",
	})
	if err != nil {
		t.Fatalf("marshal verify response: %v", err)
	}
	client, err := gitapp.NewVerificationClient(fixedResultTransport{operation: "resource.report", body: body})
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	if _, err := client.Verify(context.Background(), api.VerifyRequest{
		ActorCredential: "actor-token",
		ResourceID:      "res-shop-web",
		Operation:       string(api.ResourceOperationGitRefUpdate),
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
			{Name: "resource", Kind: "string", Value: "shop/web"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
			{Name: "expected_base", Kind: "string", Value: ""},
			{Name: "candidate", Kind: "string", Value: "0123456789abcdef0123456789abcdef01234567"},
		}},
	}); err == nil {
		t.Fatal("Verify accepted a result named for a different operation with a valid body")
	}
}
