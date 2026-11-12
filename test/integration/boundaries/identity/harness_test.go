//go:build integration

// Package identity_test exercises the identity and public-boundary
// fixtures. The contract server is a fixture of Shell verification/report
// facts, not the Shell composition, and no test here claims full approval
// proof.
package identity_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

const (
	fixtureServiceID    = "service-1"
	fixtureServiceTok   = "kodsvc.service-1.fixture-token"
	fixtureExecutionID  = "execution-1"
	fixtureExecutionTok = "kodex.execution-1.fixture-token"
	fixtureResourceID   = "repo-1"
	fixtureTenant       = "tenant-a"
	fixtureProject      = "project-1"
)

func testClock() *idfix.Clock {
	return idfix.NewClock(idfix.DefaultClockTime())
}

func newOIDC(t *testing.T, options idfix.OIDCOptions) *idfix.Provider {
	t.Helper()
	if options.Clock == nil {
		options.Clock = testClock()
	}
	provider, err := idfix.NewProvider(options)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(provider.Close)
	return provider
}

func newContract(t *testing.T, provider *idfix.Provider, options idfix.ContractOptions) *idfix.ContractServer {
	t.Helper()
	if options.Clock == nil {
		options.Clock = provider.Clock()
	}
	options.OIDC = provider
	server, err := idfix.NewContractServer(options)
	if err != nil {
		t.Fatalf("NewContractServer: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func defaultContractOptions() idfix.ContractOptions {
	return idfix.ContractOptions{
		Services: []idfix.ServiceAccount{{
			ID:        fixtureServiceID,
			Token:     fixtureServiceTok,
			TenantID:  fixtureTenant,
			ProjectID: fixtureProject,
			Enabled:   true,
			Roles:     []string{"engineer"},
		}},
		Executions: []idfix.ExecutionAccount{{
			ID:        fixtureExecutionID,
			Token:     fixtureExecutionTok,
			TenantID:  fixtureTenant,
			ProjectID: fixtureProject,
			Enabled:   true,
		}},
		Bindings: []idfix.Binding{{
			ID:               "binding-1",
			ServiceID:        fixtureServiceID,
			TenantID:         fixtureTenant,
			ProjectID:        fixtureProject,
			ResourceID:       fixtureResourceID,
			Operations:       []string{string(api.ResourceOperationGitRefUpdate)},
			ReportableEvents: []string{"git.ref.updated"},
		}},
		ActorRoles: map[string][]string{},
	}
}

func gitSubject(ref, base, candidate string) api.Subject {
	return api.Subject{Fields: []api.SubjectField{
		{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
		{Name: "resource", Kind: "string", Value: fixtureResourceID},
		{Name: "ref", Kind: "string", Value: ref},
		{Name: "expected_base", Kind: "string", Value: base},
		{Name: "candidate", Kind: "string", Value: candidate},
	}}
}

func validVerifyRequest(actorToken string) api.VerifyRequest {
	return api.VerifyRequest{
		ActorCredential: actorToken,
		ResourceID:      fixtureResourceID,
		Operation:       string(api.ResourceOperationGitRefUpdate),
		Subject:         gitSubject("refs/heads/main", "1111111", "2222222"),
	}
}

func validReportRequest(verificationID string) api.ReportRequest {
	return api.ReportRequest{
		ResourceID:     fixtureResourceID,
		SourceEventID:  "source-event-1",
		EventType:      "git.ref.updated",
		Operation:      string(api.ResourceOperationGitRefUpdate),
		VerificationID: verificationID,
		Subject:        gitSubject("refs/heads/main", "1111111", "2222222"),
	}
}

type staticCredentialSource struct {
	credential client.Credential
	err        error
}

func (source staticCredentialSource) Credential(context.Context) (client.Credential, error) {
	return source.credential, source.err
}

func serviceSource() client.CredentialSource {
	return tokenSource(client.KindService, fixtureServiceTok)
}

func tokenSource(kind client.CredentialKind, token string) client.CredentialSource {
	return staticCredentialSource{credential: client.Credential{
		Kind:      kind,
		Token:     token,
		ExpiresAt: idfix.DefaultClockTime().Add(24 * time.Hour),
	}}
}

func newTransport(t *testing.T, server *idfix.ContractServer, source client.CredentialSource, attempts int) *client.HTTPTransport {
	t.Helper()
	if source == nil {
		source = serviceSource()
	}
	if attempts == 0 {
		attempts = 1
	}
	return &client.HTTPTransport{
		BaseURL:      server.URL(),
		RoundTripper: server.Client().Transport,
		Source:       source,
		Retry:        client.RetryPolicy{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep:        func(time.Duration) {},
		Now:          func() time.Time { return idfix.DefaultClockTime() },
	}
}

func containsSecret(haystack, needle string) bool {
	return needle != "" && strings.Contains(haystack, needle)
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func readBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func decodeJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode JSON: %v (body %s)", err, data)
	}
}

func postRaw(t *testing.T, server *idfix.ContractServer, path, contentType string, body []byte) *http.Response {
	t.Helper()
	return postRawWithAuth(t, server, path, fixtureServiceTok, contentType, body)
}

func postRawWithAuth(t *testing.T, server *idfix.ContractServer, path, token, contentType string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL()+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return response
}
