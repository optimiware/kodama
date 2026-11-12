package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
)

type fakeTransport struct {
	invocations []api.Invocation
	result      api.Result
	err         error
}

func (transport *fakeTransport) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	transport.invocations = append(transport.invocations, invocation)
	return transport.result, transport.err
}

func verifyResult(t *testing.T) api.Result {
	t.Helper()
	body, err := json.Marshal(api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "principal-1", Kind: "human"},
		VerificationID: "verification-1",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return api.Result{Operation: "actor.verify", Body: body}
}

func validVerifyRequest() api.VerifyRequest {
	return api.VerifyRequest{
		ActorCredential: "actor-secret-token",
		ResourceID:      "resource-1",
		Operation:       string(api.ResourceOperationGitRefUpdate),
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
		}},
	}
}

func validReportRequest() api.ReportRequest {
	return api.ReportRequest{
		ResourceID:     "resource-1",
		SourceEventID:  "event-1",
		EventType:      "git.ref.updated",
		Operation:      string(api.ResourceOperationGitRefUpdate),
		VerificationID: "verification-1",
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
		}},
	}
}

func TestVerificationClientUsesPublicVerifyOperation(t *testing.T) {
	transport := &fakeTransport{result: verifyResult(t)}
	verification, err := NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Verify(context.Background(), validVerifyRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if response.VerificationID != "verification-1" {
		t.Fatalf("response = %+v", response)
	}
	if len(transport.invocations) != 1 || transport.invocations[0].Operation != "actor.verify" {
		t.Fatalf("invocations = %+v", transport.invocations)
	}
}

func TestVerificationClientReportsWithStableIdempotencyKey(t *testing.T) {
	body, err := json.Marshal(api.ReportResponse{VerificationID: "verification-1", EventID: "event-2"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	transport := &fakeTransport{result: api.Result{Operation: "resource.report", Body: body}}
	verification, err := NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Report(context.Background(), validReportRequest())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if response.EventID != "event-2" {
		t.Fatalf("response = %+v", response)
	}
	if len(transport.invocations) != 1 {
		t.Fatalf("invocations = %+v", transport.invocations)
	}
	invocation := transport.invocations[0]
	if invocation.Operation != "resource.report" {
		t.Fatalf("operation = %q", invocation.Operation)
	}
	if invocation.Request.IdempotencyKey != "event-1" {
		t.Fatalf("idempotency key = %q; want the stable source event id", invocation.Request.IdempotencyKey)
	}
}

func TestVerificationClientRefusesMismatchedOperationResult(t *testing.T) {
	transport := &fakeTransport{result: api.Result{Operation: "resource.report", Body: []byte(`{}`)}}
	verification, err := NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	if _, err := verification.Verify(context.Background(), validVerifyRequest()); err == nil {
		t.Fatal("Verify accepted a result for a different operation")
	}
}

func TestVerificationClientRefusesMissingTransport(t *testing.T) {
	if _, err := NewVerificationClient(nil); err == nil {
		t.Fatal("NewVerificationClient accepted a nil transport")
	}
}

func TestRetentionClientRefusesInsteadOfFabricatingEmptyPage(t *testing.T) {
	retention, err := NewRetentionClient(&fakeTransport{})
	if err != nil {
		t.Fatalf("NewRetentionClient: %v", err)
	}
	if _, err := retention.ReferencedCommits(context.Background(), api.Page{}); !errors.Is(err, ErrRetentionUnavailable) {
		t.Fatalf("ReferencedCommits error = %v; want ErrRetentionUnavailable", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestTransportReadsMountedCredentialOnEveryAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token-A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var authorizations []string
	transport, err := NewTransport(TransportConfig{
		BaseURL:        "https://factory.example",
		CredentialFile: path,
		Retry:          client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		RoundTripper: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, request.Header.Get("Authorization"))
			body, _ := json.Marshal(verifyResult(t))
			return &http.Response{
				StatusCode: api.HTTPStatusSuccess,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	verification, err := NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	if _, err := verification.Verify(context.Background(), validVerifyRequest()); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if err := os.WriteFile(path, []byte("token-B\n"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := verification.Verify(context.Background(), validVerifyRequest()); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if len(authorizations) != 2 {
		t.Fatalf("authorizations = %v", authorizations)
	}
	if authorizations[0] != "Bearer token-A" || authorizations[1] != "Bearer token-B" {
		t.Fatalf("authorizations = %v; want the rotated token on the second attempt", authorizations)
	}
}

func TestTransportNeverRendersCredentialInErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("super-secret-token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	transport, err := NewTransport(TransportConfig{
		BaseURL:        "https://factory.example",
		CredentialFile: path,
		Retry:          client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network down")
		}),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	verification, err := NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	_, err = verification.Verify(context.Background(), validVerifyRequest())
	if err == nil {
		t.Fatal("Verify succeeded against a failing transport")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("error rendered the credential: %v", err)
	}
}
