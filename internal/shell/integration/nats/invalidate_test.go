package nats_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

var _ token.InvalidationPort = (*nats.Broker)(nil)

// TestInvalidatePublishesScopeQualifiedIdentifiedMessage pins the post-commit
// invalidation effect: the message is published on the exact subject the
// invalidation watcher listens to, it carries the canonical invalidation
// payload with no credential material, and the derived message identity is
// stable across retries so a retried effect is deduplicated.
func TestInvalidatePublishesScopeQualifiedIdentifiedMessage(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: humanPrincipal(scope)}}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		resolver:  resolver,
	})
	invalidation := validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service"))

	if err := env.broker.Invalidate(context.Background(), invalidation); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	calls := client.jetstreamCalls()
	if len(calls) != 1 {
		t.Fatalf("publications = %d; want 1", len(calls))
	}
	wantSubject, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	if calls[0].subject != wantSubject {
		t.Fatalf("invalidation subject = %q; want %q", calls[0].subject, wantSubject)
	}
	if _, err := id.ParseEventID(string(calls[0].eventID)); err != nil {
		t.Fatalf("invalidation message id %q is not a stable identity: %v", calls[0].eventID, err)
	}
	var decoded token.Invalidation
	if err := json.Unmarshal(calls[0].payload, &decoded); err != nil {
		t.Fatalf("invalidation payload is not JSON: %v", err)
	}
	if decoded != invalidation {
		t.Fatalf("invalidation payload = %+v; want %+v", decoded, invalidation)
	}
	if bytes.Contains(calls[0].payload, []byte(testCredential)) || bytes.Contains(calls[0].payload, []byte(testSentinel)) {
		t.Fatalf("invalidation payload carries credential material: %s", calls[0].payload)
	}

	// A retried effect keeps the same identity and payload.
	if err := env.broker.Invalidate(context.Background(), invalidation); err != nil {
		t.Fatalf("retried Invalidate: %v", err)
	}
	calls = client.jetstreamCalls()
	if len(calls) != 2 {
		t.Fatalf("publications after retry = %d; want 2", len(calls))
	}
	if calls[0].eventID != calls[1].eventID {
		t.Fatalf("retried message id = %q; want %q", calls[1].eventID, calls[0].eventID)
	}
	if !bytes.Equal(calls[0].payload, calls[1].payload) {
		t.Fatalf("retried payload changed: %s != %s", calls[1].payload, calls[0].payload)
	}

	// The existing invalidation watcher observes the published payload.
	var mu sync.Mutex
	var seen []token.Invalidation
	handler := func(_ context.Context, received token.Invalidation) error {
		mu.Lock()
		seen = append(seen, received)
		mu.Unlock()
		return nil
	}
	subscription, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = subscription.Close(context.Background()) }()
	watcher := subscriptionWithSubject(t, client, wantSubject)
	if err := watcher.deliver(context.Background(), calls[0].payload); err != nil {
		t.Fatalf("watcher delivery: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("watcher deliveries = %d; want 1", len(seen))
	}
	if seen[0] != invalidation {
		t.Fatalf("watcher received %+v; want %+v", seen[0], invalidation)
	}
}

// TestInvalidateScopeQualifiedSubject pins that the invalidation subject
// derives from the invalidation's own scope and never spans scopes.
func TestInvalidateScopeQualifiedSubject(t *testing.T) {
	first := testScope()
	second := otherScope()
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	if err := env.broker.Invalidate(context.Background(), validInvalidation(first, id.PrincipalID("other-human"), service.ServiceID("other-service"))); err != nil {
		t.Fatalf("Invalidate first scope: %v", err)
	}
	if err := env.broker.Invalidate(context.Background(), validInvalidation(second, id.PrincipalID("another-human"), service.ServiceID("another-service"))); err != nil {
		t.Fatalf("Invalidate second scope: %v", err)
	}
	calls := client.jetstreamCalls()
	if len(calls) != 2 {
		t.Fatalf("publications = %d; want 2", len(calls))
	}
	firstSubject, err := nats.InvalidationSubject(first)
	if err != nil {
		t.Fatalf("InvalidationSubject(first): %v", err)
	}
	secondSubject, err := nats.InvalidationSubject(second)
	if err != nil {
		t.Fatalf("InvalidationSubject(second): %v", err)
	}
	if calls[0].subject != firstSubject || calls[1].subject != secondSubject {
		t.Fatalf("subjects = [%q, %q]; want [%q, %q]", calls[0].subject, calls[1].subject, firstSubject, secondSubject)
	}
	if calls[0].subject == calls[1].subject {
		t.Fatal("distinct scopes shared one invalidation subject")
	}
}

// TestInvalidateReturnsBoundedPublishFailure pins that an exhausted
// publication is returned (never swallowed) with bounded attempts and
// injected-clock backoff.
func TestInvalidateReturnsBoundedPublishFailure(t *testing.T) {
	client := newFakeClient()
	client.failJetStream(errBrokerDown)
	env := newBrokerEnv(t, brokerOptions{
		connector:   &fakeConnector{clients: []nats.Client{client}},
		maxAttempts: 3,
	})
	err := env.broker.Invalidate(context.Background(), validInvalidation(testScope(), id.PrincipalID("other-human"), service.ServiceID("other-service")))
	requireClass(t, err, fault.DependencyUnavailable)
	if got := len(client.jetstreamCalls()); got != 3 {
		t.Fatalf("publication attempts = %d; want 3", got)
	}
	if got := len(env.clock.Sleeps()); got != 2 {
		t.Fatalf("backoff sleeps = %d; want 2", got)
	}
}

// TestInvalidateHonorsCancellation pins that ctx cancellation interrupts the
// bounded retry/backoff and is reported as deadline_expired.
func TestInvalidateHonorsCancellation(t *testing.T) {
	client := newFakeClient()
	client.failJetStream(errBrokerDown)
	clock := newManualClock()
	env := newBrokerEnv(t, brokerOptions{
		connector:   &fakeConnector{clients: []nats.Client{client}},
		clock:       clock,
		maxAttempts: 3,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- env.broker.Invalidate(ctx, validInvalidation(testScope(), id.PrincipalID("other-human"), service.ServiceID("other-service")))
	}()
	clock.WaitForSleep(t)
	cancel()
	select {
	case err := <-done:
		requireClass(t, err, fault.DeadlineExpired)
	case <-time.After(5 * time.Second):
		t.Fatal("Invalidate did not honor cancellation")
	}
	if got := clock.Active(); got != 0 {
		t.Fatalf("active injected sleepers = %d; want 0", got)
	}
}

// TestInvalidateRefusesInvalidInputBeforeDial pins that a malformed
// invalidation is refused before any broker interaction.
func TestInvalidateRefusesInvalidInputBeforeDial(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	connector := &fakeConnector{clients: []nats.Client{client}}
	env := newBrokerEnv(t, brokerOptions{connector: connector})

	cases := []struct {
		name         string
		invalidation token.Invalidation
	}{
		{"scope-less", token.Invalidation{PrincipalID: id.PrincipalID("other-human"), ServiceID: service.ServiceID("other-service"), Reason: token.InvalidationRevoked, At: testNow()}},
		{"wildcard project", token.Invalidation{Scope: tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-*")}, PrincipalID: id.PrincipalID("other-human"), ServiceID: service.ServiceID("other-service"), Reason: token.InvalidationRevoked, At: testNow()}},
		{"zero time", token.Invalidation{Scope: scope, PrincipalID: id.PrincipalID("other-human"), ServiceID: service.ServiceID("other-service"), Reason: token.InvalidationRevoked}},
		{"both targets", token.Invalidation{Scope: scope, PrincipalID: id.PrincipalID("other-human"), ExecutionID: id.ExecutionID("exec-1"), ServiceID: service.ServiceID("other-service"), Reason: token.InvalidationRevoked, At: testNow()}},
		{"unknown reason", token.Invalidation{Scope: scope, PrincipalID: id.PrincipalID("other-human"), ServiceID: service.ServiceID("other-service"), Reason: token.InvalidationReason("unknown"), At: testNow()}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requireClass(t, env.broker.Invalidate(context.Background(), testCase.invalidation), fault.InvalidInput)
		})
	}
	if got := connector.callCount(); got != 0 {
		t.Fatalf("Connect calls = %d; want 0 before a valid invalidation", got)
	}
	if got := len(client.jetstreamCalls()); got != 0 {
		t.Fatalf("publications = %d; want 0", got)
	}
}
