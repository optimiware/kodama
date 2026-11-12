package nats_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
)

func observationRequest(credential string, data []byte) nats.ObservationRequest {
	return nats.ObservationRequest{
		Credential: credential,
		Type:       event.TypeCustomObservation,
		Data:       data,
	}
}

// TestObserveDerivesSourceAndScopeFromAuthentication pins the ingress boundary:
// source and scope come from the verified principal, the authorizer sees
// "event.publish" in that scope, and the adapter hands the observation to the
// sink instead of publishing directly to a committed-event subject.
func TestObserveDerivesSourceAndScopeFromAuthentication(t *testing.T) {
	scope := testScope()
	execution := executionPrincipal(scope, id.ExecutionID("exec-1"))
	client := newFakeClient()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: execution}}
	authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
	env := newBrokerEnv(t, brokerOptions{
		connector:  &fakeConnector{clients: []nats.Client{client}},
		resolver:   resolver,
		authorizer: authorizer,
	})

	if err := env.broker.Observe(context.Background(), observationRequest(testCredential, []byte(`{"observed":true}`))); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got := env.sink.count(); got != 1 {
		t.Fatalf("sink observations = %d; want 1", got)
	}
	observation := env.sink.last(t)
	if observation.Scope != scope {
		t.Fatalf("observation scope = %+v; want %+v", observation.Scope, scope)
	}
	wantSource := event.Source{Kind: event.SourceExecution, PrincipalID: execution.ID}
	if observation.Source != wantSource {
		t.Fatalf("observation source = %+v; want %+v", observation.Source, wantSource)
	}
	if observation.Type != event.TypeCustomObservation {
		t.Fatalf("observation type = %q; want %q", observation.Type, event.TypeCustomObservation)
	}
	if !bytes.Equal(observation.Data, []byte(`{"observed":true}`)) {
		t.Fatalf("observation data = %q", observation.Data)
	}
	recorded := authorizer.recorded()
	if len(recorded) != 1 {
		t.Fatalf("authorization calls = %d; want 1", len(recorded))
	}
	if recorded[0].operation != authorize.OperationName("event.publish") {
		t.Fatalf("authorization operation = %q; want event.publish", recorded[0].operation)
	}
	if recorded[0].scope != scope {
		t.Fatalf("authorization scope = %+v; want %+v", recorded[0].scope, scope)
	}

	// The ingress is not a direct committed-event publication.
	if got := env.connector.callCount(); got != 0 {
		t.Fatalf("Observe dialed the broker %d times; want 0", got)
	}
	if got := len(client.jetstreamCalls()); got != 0 {
		t.Fatalf("Observe used JetStream %d times; want 0", got)
	}
	if got := len(client.coreCalls()); got != 0 {
		t.Fatalf("Observe used Core NATS %d times; want 0", got)
	}

	// A service principal derives a service source.
	service := servicePrincipal(scope)
	resolver.Set(testCredential, service)
	if err := env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("service observation"))); err != nil {
		t.Fatalf("service Observe: %v", err)
	}
	serviceObservation := env.sink.last(t)
	wantServiceSource := event.Source{Kind: event.SourceService, PrincipalID: service.ID}
	if serviceObservation.Source != wantServiceSource {
		t.Fatalf("service observation source = %+v; want %+v", serviceObservation.Source, wantServiceSource)
	}
}

// TestObserveRefusesSpoofedSourceAndScope pins that a body cannot widen or
// reinterpret identity: a declared source or scope that disagrees with
// authentication is refused.
func TestObserveRefusesSpoofedSourceAndScope(t *testing.T) {
	scope := testScope()
	execution := executionPrincipal(scope, id.ExecutionID("exec-1"))
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: execution}}
	env := newBrokerEnv(t, brokerOptions{resolver: resolver})

	spoofedSource := event.Source{Kind: event.SourceService, PrincipalID: id.PrincipalID("service-9")}
	request := observationRequest(testCredential, []byte("spoof"))
	request.DeclaredSource = &spoofedSource
	requireClass(t, env.broker.Observe(context.Background(), request), fault.Forbidden)
	if got := env.sink.count(); got != 0 {
		t.Fatalf("sink observations after spoofed source = %d; want 0", got)
	}

	spoofedScope := otherScope()
	request = observationRequest(testCredential, []byte("spoof"))
	request.DeclaredScope = &spoofedScope
	requireClass(t, env.broker.Observe(context.Background(), request), fault.Forbidden)
	if got := env.sink.count(); got != 0 {
		t.Fatalf("sink observations after spoofed scope = %d; want 0", got)
	}

	// A lifecycle source cannot be claimed either.
	shellSource := event.Source{Kind: event.SourceShell, PrincipalID: execution.ID}
	request = observationRequest(testCredential, []byte("spoof"))
	request.DeclaredSource = &shellSource
	requireClass(t, env.broker.Observe(context.Background(), request), fault.Forbidden)

	// A declaration that agrees with authentication is accepted.
	matchingSource := event.Source{Kind: event.SourceExecution, PrincipalID: execution.ID}
	request = observationRequest(testCredential, []byte("matching"))
	request.DeclaredSource = &matchingSource
	matchingScope := scope
	request.DeclaredScope = &matchingScope
	if err := env.broker.Observe(context.Background(), request); err != nil {
		t.Fatalf("matching declaration refused: %v", err)
	}
	if got := env.sink.count(); got != 1 {
		t.Fatalf("sink observations = %d; want 1", got)
	}
}

// TestObserveRefusesLifecycleAndReportTypes pins that the ingress can never
// impersonate a committed lifecycle or decision fact.
func TestObserveRefusesLifecycleAndReportTypes(t *testing.T) {
	scope := testScope()
	execution := executionPrincipal(scope, id.ExecutionID("exec-1"))
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: execution}}
	env := newBrokerEnv(t, brokerOptions{resolver: resolver})

	for _, eventType := range []event.Type{
		event.TypeTaskCreated,
		event.TypeExecutionQueued,
		event.TypeExecutionSucceeded,
		event.TypeServiceChanged,
		event.TypeVerifiedReport,
	} {
		request := observationRequest(testCredential, []byte("forged"))
		request.Type = eventType
		requireClass(t, env.broker.Observe(context.Background(), request), fault.Forbidden)
	}
	unknown := observationRequest(testCredential, []byte("forged"))
	unknown.Type = event.Type("observation.forged")
	requireClass(t, env.broker.Observe(context.Background(), unknown), fault.InvalidInput)
	if got := env.sink.count(); got != 0 {
		t.Fatalf("sink observations after forged types = %d; want 0", got)
	}
}

// TestObserveRefusesHumanAndDisabledPrincipals pins that ingress is only for
// authenticated execution/service observations and fails closed on any
// non-current credential.
func TestObserveRefusesHumanAndDisabledPrincipals(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	resolver := &fakePrincipalResolver{}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		resolver:  resolver,
	})

	resolver.Set(testCredential, humanPrincipal(scope))
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("human"))), fault.Forbidden)

	disabled := executionPrincipal(scope, id.ExecutionID("exec-1"))
	disabled.Enabled = false
	resolver.Set(testCredential, disabled)
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("disabled"))), fault.Unauthenticated)

	resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential expired"))
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("expired"))), fault.Unauthenticated)

	requireClass(t, env.broker.Observe(context.Background(), observationRequest("", []byte("missing"))), fault.Unauthenticated)

	if got := env.sink.count(); got != 0 {
		t.Fatalf("sink observations after refusals = %d; want 0", got)
	}
	if got := len(client.jetstreamCalls()); got != 0 {
		t.Fatalf("refused observations published %d committed events", got)
	}
}

// TestObserveRechecksCurrentCredentialAndAuthority pins that every ingress
// publish re-reads current state: a rotated/revoked credential or a removed
// grant is refused even after an earlier success.
func TestObserveRechecksCurrentCredentialAndAuthority(t *testing.T) {
	scope := testScope()
	execution := executionPrincipal(scope, id.ExecutionID("exec-1"))
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: execution}}
	authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
	env := newBrokerEnv(t, brokerOptions{resolver: resolver, authorizer: authorizer})

	if err := env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("first"))); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential rotated"))
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("second"))), fault.Unauthenticated)
	if got := env.sink.count(); got != 1 {
		t.Fatalf("sink observations = %d; want 1", got)
	}

	resolver.Set(testCredential, execution)
	authorizer.SetDecision(authorize.DecisionDeny)
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("denied"))), fault.Forbidden)

	authorizer.SetDecision(authorize.DecisionAllow)
	authorizer.SetError(errBrokerDown)
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("unavailable"))), fault.DependencyUnavailable)
	if got := env.sink.count(); got != 1 {
		t.Fatalf("sink observations after refusals = %d; want 1", got)
	}
}

// TestObserveBoundsDataAndPropagatesSinkFailure pins the payload bound and the
// sink error mapping; the credential never appears in an error.
func TestObserveBoundsDataAndPropagatesSinkFailure(t *testing.T) {
	scope := testScope()
	execution := executionPrincipal(scope, id.ExecutionID("exec-1"))
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: execution}}
	env := newBrokerEnv(t, brokerOptions{resolver: resolver})

	atBound := bytes.Repeat([]byte("x"), event.DefaultBounds.MaxDataBytes)
	if err := env.broker.Observe(context.Background(), observationRequest(testCredential, atBound)); err != nil {
		t.Fatalf("observation at the data bound refused: %v", err)
	}
	oversized := bytes.Repeat([]byte("x"), event.DefaultBounds.MaxDataBytes+1)
	requireClass(t, env.broker.Observe(context.Background(), observationRequest(testCredential, oversized)), fault.InvalidInput)
	if got := env.sink.count(); got != 1 {
		t.Fatalf("sink observations = %d; want 1", got)
	}

	// A sink fault keeps its class and never renders the credential.
	env.sink.SetError(fault.New(fault.Forbidden, "observation refused by the sink"))
	err := env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("sink fault")))
	requireClass(t, err, fault.Forbidden)
	if strings.Contains(err.Error(), testCredential) {
		t.Fatalf("sink error leaks the credential: %v", err)
	}

	// A plain sink error becomes dependency_unavailable.
	env.sink.SetError(errBrokerDown)
	err = env.broker.Observe(context.Background(), observationRequest(testCredential, []byte("sink error")))
	requireClass(t, err, fault.DependencyUnavailable)

	// A canceled context is refused before the sink.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := env.sink.count()
	err = env.broker.Observe(ctx, observationRequest(testCredential, []byte("canceled")))
	requireClass(t, err, fault.DeadlineExpired)
	if got := env.sink.count(); got != before {
		t.Fatalf("sink observations after cancellation = %d; want %d", got, before)
	}
}

// TestObserveCredentialNeverLeaksInErrors pins the sentinel rule for the
// ingress path.
func TestObserveCredentialNeverLeaksInErrors(t *testing.T) {
	resolver := &fakePrincipalResolver{}
	env := newBrokerEnv(t, brokerOptions{resolver: resolver})
	resolver.Fail(testSentinel, fault.New(fault.Unauthenticated, "credential rejected"))
	err := env.broker.Observe(context.Background(), observationRequest(testSentinel, []byte("x")))
	if err == nil {
		t.Fatal("unknown credential accepted")
	}
	if strings.Contains(err.Error(), testSentinel) {
		t.Fatalf("ingress error leaks the credential: %v", err)
	}
}
