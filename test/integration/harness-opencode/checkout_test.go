package harnessopencode_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// Test_fixture_checkout_binding proves the T-031 checkout provider is bound to
// the fixture's committed scope, workspace and repository, not merely echoing
// whatever request the executor passes to it.
func Test_fixture_checkout_binding(t *testing.T) {
	credential, ca := []byte("fixture:read-token"), []byte("-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n")
	base := kube.CheckoutRequest{
		Scope: executiontest.ScopeA, AttemptID: "att_1", WorkspaceID: "ws_1",
		RepositoryURL: "https://git.example.test/t031-repo.git",
	}
	tests := []struct {
		name string
		req  kube.CheckoutRequest
		cred []byte
		ca   []byte
	}{
		{name: "exact binding", req: base, cred: credential, ca: ca},
		{name: "foreign scope", req: func() kube.CheckoutRequest { r := base; r.Scope = executiontest.ScopeB; return r }(), cred: credential, ca: ca},
		{name: "unknown workspace", req: func() kube.CheckoutRequest { r := base; r.WorkspaceID = "ws_2"; return r }(), cred: credential, ca: ca},
		{name: "empty workspace", req: func() kube.CheckoutRequest { r := base; r.WorkspaceID = ""; return r }(), cred: credential, ca: ca},
		{name: "mismatched repository", req: func() kube.CheckoutRequest {
			r := base
			r.RepositoryURL = "https://git.example.test/other.git"
			return r
		}(), cred: credential, ca: ca},
		{name: "missing credential", req: base, ca: ca},
		{name: "missing CA", req: base, cred: credential},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fixtureCheckout(tt.req, executiontest.ScopeA, "ws_1", base.RepositoryURL, tt.cred, tt.ca)
			if tt.name == "exact binding" {
				if err != nil {
					t.Fatalf("checkout: %v", err)
				}
				if got.Scope != tt.req.Scope || got.WorkspaceID != tt.req.WorkspaceID || got.RepositoryURL != tt.req.RepositoryURL ||
					string(got.Credential) != string(credential) || string(got.GitCA) != string(ca) {
					t.Fatalf("checkout material = %+v", got)
				}
				return
			}
			if !errors.Is(err, kube.ErrCheckoutDenied) {
				t.Fatalf("err = %v, want ErrCheckoutDenied", err)
			}
			if got.Scope != (kube.CheckoutMaterial{}).Scope || got.WorkspaceID != "" || got.RepositoryURL != "" ||
				len(got.Credential) != 0 || len(got.GitCA) != 0 {
				t.Fatalf("denied checkout returned material: %+v", got)
			}
		})
	}
}
