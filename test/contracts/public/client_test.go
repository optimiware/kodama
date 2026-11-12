package publiccontract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// TestCredentialValidationAndRedaction pins the credential value: a bounded
// token, a valid source kind, a non-zero expiry, and redacted formatting.
func TestCredentialValidationAndRedaction(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	credential := client.Credential{
		Kind:      client.KindExecution,
		Token:     "kodex.exec-1.signature",
		ExpiresAt: now.Add(time.Hour),
	}
	if err := credential.Validate(); err != nil {
		t.Fatalf("valid credential refused: %v", err)
	}
	if !credential.Usable(now) {
		t.Fatal("Usable(now) = false for a current credential")
	}
	if credential.Usable(credential.ExpiresAt) {
		t.Fatal("Usable(expiry) = true; expiry at the authoritative time is expired")
	}
	if credential.Usable(credential.ExpiresAt.Add(time.Second)) {
		t.Fatal("Usable(after expiry) = true")
	}

	rendered := fmt.Sprintf("%v|%+v|%#v|%s", credential, credential, credential, credential.String())
	if strings.Contains(rendered, credential.Token) {
		t.Fatalf("credential formatting leaks the token: %s", rendered)
	}
	if !strings.Contains(strings.ToLower(rendered), "execution") {
		t.Fatalf("credential formatting dropped the source kind: %s", rendered)
	}

	invalid := []client.Credential{
		{Kind: "robot", Token: "x", ExpiresAt: now},
		{Kind: client.KindSession, Token: "", ExpiresAt: now},
		{Kind: client.KindSession, Token: strings.Repeat("x", api.MaxCredentialBytes+1), ExpiresAt: now},
		{Kind: client.KindService, Token: "x"},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("Credential.Validate accepted %+v", candidate)
		}
	}
	if (client.Credential{Kind: client.KindService, Token: "x"}).Usable(now) {
		t.Error("Usable accepted a credential with a zero expiry")
	}
	for _, kind := range []client.CredentialKind{client.KindSession, client.KindExecution, client.KindService} {
		if !kind.Valid() {
			t.Errorf("credential kind %q is not valid", kind)
		}
	}
	if client.CredentialKind("robot").Valid() {
		t.Error("CredentialKind(robot).Valid() = true")
	}
}

// rotatingSource is a fake credential source. It proves the client reads the
// source on every authentication so renewal takes effect.
type rotatingSource struct {
	tokens []string
	calls  int
	err    error
}

func (source *rotatingSource) Credential(context.Context) (client.Credential, error) {
	if source.err != nil {
		return client.Credential{}, source.err
	}
	if source.calls >= len(source.tokens) {
		return client.Credential{}, fault.New(fault.Unauthenticated, "no credential")
	}
	token := source.tokens[source.calls]
	source.calls++
	return client.Credential{
		Kind:      client.KindExecution,
		Token:     token,
		ExpiresAt: time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC),
	}, nil
}

var _ client.CredentialSource = (*rotatingSource)(nil)

// TestCredentialSourcingPerAttempt pins that each authentication reads the
// source again (no cached token), so a rotated mounted token is used without a
// restart, and that an unusable credential is refused.
func TestCredentialSourcingPerAttempt(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	source := &rotatingSource{tokens: []string{"kodex.exec-1.first", "kodex.exec-1.second"}}
	first, err := client.Bearer(context.Background(), source, now)
	if err != nil {
		t.Fatalf("Bearer(first): %v", err)
	}
	second, err := client.Bearer(context.Background(), source, now)
	if err != nil {
		t.Fatalf("Bearer(second): %v", err)
	}
	if first != "Bearer kodex.exec-1.first" || second != "Bearer kodex.exec-1.second" {
		t.Fatalf("bearer values = %q, %q; want each attempt to read the current token", first, second)
	}
	if source.calls != 2 {
		t.Fatalf("credential source calls = %d; want one per attempt", source.calls)
	}
	if strings.Contains(first, "kodex.exec-1.first") && strings.Count(first, "kodex.exec-1.first") != 1 {
		t.Fatalf("bearer value repeats the token: %q", first)
	}

	expired := &rotatingSource{tokens: []string{"kodex.exec-1.expired"}}
	expiredAt := now.Add(-time.Second)
	sourceCredential := client.Credential{Kind: client.KindExecution, Token: "kodex.exec-1.expired", ExpiresAt: expiredAt}
	if sourceCredential.Usable(now) {
		t.Fatal("expired credential reported usable")
	}
	header, err := client.Bearer(context.Background(), expired, now.Add(2*time.Hour))
	if err == nil {
		t.Fatalf("Bearer accepted an expired credential: %q", header)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("expired credential class = %v; want unauthenticated", class)
	}

	failing := &rotatingSource{err: fault.New(fault.Unauthenticated, "session ended")}
	if header, err := client.Bearer(context.Background(), failing, now); err == nil {
		t.Fatalf("Bearer accepted a failing source: %q", header)
	}
}

// TestRetryPolicyAndAmbiguousMutationRules pins the retry contract: reads retry
// only transient dependency/internal failures; mutations retry only with the
// caller's idempotency key and never retry an ambiguous external effect or a
// stale version.
func TestRetryPolicyAndAmbiguousMutationRules(t *testing.T) {
	valid := client.RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid retry policy refused: %v", err)
	}
	for _, invalid := range []client.RetryPolicy{
		{},
		{MaxAttempts: 0, BaseDelay: time.Millisecond, MaxDelay: time.Second},
		{MaxAttempts: 9, BaseDelay: time.Millisecond, MaxDelay: time.Second},
		{MaxAttempts: 2, BaseDelay: 0, MaxDelay: time.Second},
		{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: time.Millisecond},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("RetryPolicy.Validate accepted %+v", invalid)
		}
	}

	cases := []struct {
		name          string
		code          fault.Code
		mutation      bool
		hasKey        bool
		wantRetryable bool
	}{
		{"read dependency", fault.CodeDependencyUnavailable, false, false, true},
		{"read internal", fault.CodeInternalFailure, false, false, true},
		{"read not found", fault.CodeNotFound, false, false, false},
		{"read invalid input", fault.CodeInvalidInput, false, false, false},
		{"read forbidden", fault.CodeForbidden, false, false, false},
		{"read version conflict", fault.CodeVersionConflict, false, false, false},
		{"mutation dependency with key", fault.CodeDependencyUnavailable, true, true, true},
		{"mutation dependency without key", fault.CodeDependencyUnavailable, true, false, false},
		{"mutation internal with key", fault.CodeInternalFailure, true, true, false},
		{"mutation version conflict", fault.CodeVersionConflict, true, true, false},
		{"mutation changed content", fault.CodeChangedIdempotencyContent, true, true, false},
		{"mutation uncertain effect", fault.CodeUncertainExternalEffect, true, true, false},
		{"mutation deadline", fault.CodeDeadlineExpired, true, true, false},
	}
	for _, testCase := range cases {
		if got := client.RetryAllowed(testCase.code, testCase.mutation, testCase.hasKey); got != testCase.wantRetryable {
			t.Errorf("%s: RetryAllowed = %v; want %v", testCase.name, got, testCase.wantRetryable)
		}
	}

	version := uint64(7)
	original := api.Request{
		Input:           json.RawMessage(`{"ref":"refs/heads/main"}`),
		ExpectedVersion: &version,
		IdempotencyKey:  "caller-key-1",
	}
	retried := client.RetryRequest(original)
	if retried.IdempotencyKey != original.IdempotencyKey {
		t.Fatalf("retry idempotency key = %q; want the same key %q", retried.IdempotencyKey, original.IdempotencyKey)
	}
	if retried.ExpectedVersion == nil || *retried.ExpectedVersion != version {
		t.Fatalf("retry expected version = %v; want %d", retried.ExpectedVersion, version)
	}
	if string(retried.Input) != string(original.Input) {
		t.Fatalf("retry input = %s; want %s", retried.Input, original.Input)
	}
}

// recordingTransport is the compile-only fake REST transport.
type recordingTransport struct {
	invocations []api.Invocation
}

func (transport *recordingTransport) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	transport.invocations = append(transport.invocations, invocation)
	return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
}

var _ client.Transport = (*recordingTransport)(nil)

// TestTransportConsumerCompiles pins the client transport boundary used by the
// CLI, helper and Git report client.
func TestTransportConsumerCompiles(t *testing.T) {
	transport := &recordingTransport{}
	invocation := api.Invocation{Operation: "task.read", Request: api.Request{Input: json.RawMessage(`{"id":"task-1"}`)}}
	result, err := transport.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("fake transport: %v", err)
	}
	if result.Operation != "task.read" {
		t.Fatalf("result operation = %q", result.Operation)
	}
	if len(transport.invocations) != 1 || transport.invocations[0].Operation != "task.read" {
		t.Fatalf("transport invocations = %+v", transport.invocations)
	}
}
