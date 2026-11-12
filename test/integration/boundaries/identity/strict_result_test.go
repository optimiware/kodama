//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// resultTransport answers every invocation with one fixed public result
// envelope so the git client's codec, not the transport, decides acceptance.
func resultTransport(t *testing.T, operation, body string) *client.HTTPTransport {
	t.Helper()
	envelope, err := json.Marshal(api.Result{Operation: operation, Body: []byte(body)})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return &client.HTTPTransport{
		BaseURL: "https://factory.example",
		RoundTripper: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: api.HTTPStatusSuccess,
				Header:     http.Header{"Content-Type": []string{api.MediaType}},
				Body:       io.NopCloser(bytes.NewReader(envelope)),
			}, nil
		}),
		Source: staticCredentialSource{credential: client.Credential{
			Kind: client.KindService, Token: "service-token", ExpiresAt: time.Unix(1_700_000_000, 0).Add(time.Hour),
		}},
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep: func(time.Duration) {},
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

// TestVerificationClientRefusesUnknownResultBodyMembers proves the git
// verification client decodes result bodies with the declared operation codec,
// so an unknown member at the envelope or nested body level is refused instead
// of silently accepted.
func TestVerificationClientRefusesUnknownResultBodyMembers(t *testing.T) {
	validVerify := `{"principal":{"id":"oidc-0123456789abcdef0123456789abcdef","kind":"human"},"verification_id":"verification-0001"}`
	validReport := `{"verification_id":"verification-0001","event_id":"event-0001"}`

	cases := []struct {
		name      string
		operation string
		body      string
		invoke    func(*gitapp.VerificationClient) error
	}{
		{
			name:      "verify unknown top-level member",
			operation: "actor.verify",
			body:      `{"principal":{"id":"oidc-0123456789abcdef0123456789abcdef","kind":"human"},"verification_id":"verification-0001","unexpected_member":true}`,
			invoke: func(verification *gitapp.VerificationClient) error {
				_, err := verification.Verify(context.Background(), validVerifyRequest("actor-token"))
				return err
			},
		},
		{
			name:      "verify unknown nested member",
			operation: "actor.verify",
			body:      `{"principal":{"id":"oidc-0123456789abcdef0123456789abcdef","kind":"human","unexpected_member":true},"verification_id":"verification-0001"}`,
			invoke: func(verification *gitapp.VerificationClient) error {
				_, err := verification.Verify(context.Background(), validVerifyRequest("actor-token"))
				return err
			},
		},
		{
			name:      "report unknown top-level member",
			operation: "resource.report",
			body:      `{"verification_id":"verification-0001","event_id":"event-0001","unexpected_member":true}`,
			invoke: func(verification *gitapp.VerificationClient) error {
				_, err := verification.Report(context.Background(), validReportRequest("verification-0001"))
				return err
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verification, err := gitapp.NewVerificationClient(resultTransport(t, testCase.operation, testCase.body))
			if err != nil {
				t.Fatalf("NewVerificationClient: %v", err)
			}
			err = testCase.invoke(verification)
			requireFaultClass(t, err, fault.DependencyUnavailable)
		})
	}

	// Positive control: the same client accepts the exact valid bodies, so the
	// refusals above are strict decoding rather than a client that refuses
	// every response.
	for _, testCase := range []struct {
		operation string
		body      string
		invoke    func(*gitapp.VerificationClient) error
	}{
		{"actor.verify", validVerify, func(verification *gitapp.VerificationClient) error {
			_, err := verification.Verify(context.Background(), validVerifyRequest("actor-token"))
			return err
		}},
		{"resource.report", validReport, func(verification *gitapp.VerificationClient) error {
			_, err := verification.Report(context.Background(), validReportRequest("verification-0001"))
			return err
		}},
	} {
		verification, err := gitapp.NewVerificationClient(resultTransport(t, testCase.operation, testCase.body))
		if err != nil {
			t.Fatalf("NewVerificationClient: %v", err)
		}
		if err := testCase.invoke(verification); err != nil {
			t.Fatalf("%s valid body was refused: %v", testCase.operation, err)
		}
	}
}
