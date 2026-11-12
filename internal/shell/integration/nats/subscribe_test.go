package nats_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func subscribeRequest(credential string, scope tenancy.Scope, handler nats.InvalidationHandler) nats.SubscriptionRequest {
	return nats.SubscriptionRequest{Credential: credential, Scope: scope, Handler: handler}
}

// TestSubscribeAuthenticatesAndDeniesCrossScope pins the subscription
// authorization boundary: current principal, exact scope, current grant, and
// the scope-qualified invalidation subject.
func TestSubscribeAuthenticatesAndDeniesCrossScope(t *testing.T) {
	scope := testScope()
	human := humanPrincipal(scope)
	client := newFakeClient()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
	authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
	env := newBrokerEnv(t, brokerOptions{
		connector:  &fakeConnector{clients: []nats.Client{client}},
		resolver:   resolver,
		authorizer: authorizer,
	})
	calls := &counter{}
	handler := loggingHandler(calls)

	subscription, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if subscription == nil {
		t.Fatal("Subscribe returned a nil subscription")
	}
	subs := client.subscriptions()
	if len(subs) != 1 {
		t.Fatalf("client subscriptions = %d; want 1", len(subs))
	}
	wantSubject, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	if subs[0].subject != wantSubject {
		t.Fatalf("subscription subject = %q; want %q", subs[0].subject, wantSubject)
	}
	recorded := authorizer.recorded()
	if len(recorded) != 1 {
		t.Fatalf("authorization calls = %d; want 1", len(recorded))
	}
	if recorded[0].operation != authorize.OperationName("event.subscribe") {
		t.Fatalf("authorization operation = %q; want event.subscribe", recorded[0].operation)
	}
	if recorded[0].scope != scope {
		t.Fatalf("authorization scope = %+v; want %+v", recorded[0].scope, scope)
	}

	if err := subscription.Close(context.Background()); err != nil {
		t.Fatalf("subscription Close: %v", err)
	}
	subs[0].waitClosed(t)

	// A subscriber for one scope never receives another scope's subject.
	before := len(client.subscriptions())
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, otherScope(), handler))
	requireClass(t, err, fault.Forbidden)
	if got := len(client.subscriptions()); got != before {
		t.Fatalf("cross-scope subscribe opened %d client subscriptions; want %d", got, before)
	}

	// A denied grant fails closed.
	authorizer.SetDecision(authorize.DecisionDeny)
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	requireClass(t, err, fault.Forbidden)
	authorizer.SetDecision(authorize.DecisionAllow)

	// An unverifiable credential is unauthenticated.
	resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential expired"))
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	requireClass(t, err, fault.Unauthenticated)

	// A disabled principal is not current authority.
	resolver.Set(testCredential, disabledHumanPrincipal(scope))
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	requireClass(t, err, fault.Unauthenticated)

	// Malformed requests are refused before any dependency is touched.
	resolver.Set(testCredential, human)
	before = len(client.subscriptions())
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest("", scope, handler))
	requireClass(t, err, fault.Unauthenticated)
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, nil))
	requireClass(t, err, fault.InvalidInput)
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, tenancy.Scope{}, handler))
	requireClass(t, err, fault.InvalidInput)
	if got := len(client.subscriptions()); got != before {
		t.Fatalf("refused requests opened %d subscriptions; want %d", got, before)
	}

	// An authorizer failure fails closed.
	authorizer.SetError(errBrokerDown)
	_, err = env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	requireClass(t, err, fault.DependencyUnavailable)
	authorizer.SetError(nil)

	// A broker connection failure is dependency_unavailable.
	failing := newBrokerEnv(t, brokerOptions{connector: alwaysFailingConnector(3), resolver: resolver, authorizer: authorizer})
	_, err = failing.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler))
	requireClass(t, err, fault.DependencyUnavailable)
}

// TestSubscribeRevalidatesCurrentPrincipalOnDelivery pins per-delivery
// revalidation: a credential that stopped being current or a grant that was
// removed closes the subscription and never reaches the handler.
func TestSubscribeRevalidatesCurrentPrincipalOnDelivery(t *testing.T) {
	t.Run("credential revoked", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		client := newFakeClient()
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		env := newBrokerEnv(t, brokerOptions{
			connector: &fakeConnector{clients: []nats.Client{client}},
			resolver:  resolver,
		})
		calls := &counter{}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		fakeSub := client.lastSubscription(t)
		payload := mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service")))

		resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential revoked"))
		if err := fakeSub.deliver(context.Background(), payload); err == nil {
			t.Fatal("delivery succeeded after credential revocation")
		}
		if got := calls.get(); got != 0 {
			t.Fatalf("handler calls = %d; want 0 after revocation", got)
		}
		fakeSub.waitClosed(t)
	})

	t.Run("membership removed", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		client := newFakeClient()
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
		env := newBrokerEnv(t, brokerOptions{
			connector:  &fakeConnector{clients: []nats.Client{client}},
			resolver:   resolver,
			authorizer: authorizer,
		})
		calls := &counter{}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		fakeSub := client.lastSubscription(t)
		payload := mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service")))

		authorizer.SetDecision(authorize.DecisionDeny)
		err := fakeSub.deliver(context.Background(), payload)
		requireClass(t, err, fault.Forbidden)
		if got := calls.get(); got != 0 {
			t.Fatalf("handler calls = %d; want 0 after membership removal", got)
		}
		fakeSub.waitClosed(t)
	})
}

// TestSubscribeClosesOnSelfInvalidation pins the self-target case: the
// subscriber sees its own invalidation once and the subscription is then
// closed, so no later delivery reaches the handler.
func TestSubscribeClosesOnSelfInvalidation(t *testing.T) {
	scope := testScope()
	principal := servicePrincipal(scope)
	client := newFakeClient()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: principal}}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		resolver:  resolver,
	})
	calls := &counter{}
	var received []token.Invalidation
	var mu sync.Mutex
	handler := func(_ context.Context, invalidation token.Invalidation) error {
		calls.inc()
		mu.Lock()
		received = append(received, invalidation)
		mu.Unlock()
		return nil
	}
	if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	invalidation := validInvalidation(scope, principal.ID, service.ServiceID("service-1"))
	payload := mustJSON(t, invalidation)

	if err := fakeSub.deliver(context.Background(), payload); err != nil {
		t.Fatalf("self invalidation delivery: %v", err)
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("handler calls = %d; want 1", got)
	}
	mu.Lock()
	if len(received) != 1 || received[0].Reason != token.InvalidationRevoked {
		t.Fatalf("handler received %+v; want the revoked invalidation", received)
	}
	mu.Unlock()
	fakeSub.waitClosed(t)

	if err := fakeSub.deliver(context.Background(), payload); err == nil {
		t.Fatal("delivery succeeded after self invalidation closed the subscription")
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("handler calls after closure = %d; want 1", got)
	}
}

// TestSubscribeIdleValidationClosesOnCredentialStateChange pins timed idle
// validation: rotation, revocation, disablement, expiry, request entry,
// terminal state and membership removal close an established subscription with
// no message needed.
func TestSubscribeIdleValidationClosesOnCredentialStateChange(t *testing.T) {
	type mutation func(*fakePrincipalResolver, *fakeAuthorizer)
	cases := []struct {
		name   string
		mutate mutation
		want   fault.Class
	}{
		{"rotated", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential rotated"))
		}, fault.Unauthenticated},
		{"revoked", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential revoked"))
		}, fault.Unauthenticated},
		{"disabled", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Set(testCredential, disabledHumanPrincipal(testScope()))
		}, fault.Unauthenticated},
		{"expired", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential expired"))
		}, fault.Unauthenticated},
		{"request entry", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "execution is waiting at a request"))
		}, fault.Unauthenticated},
		{"terminal state", func(resolver *fakePrincipalResolver, _ *fakeAuthorizer) {
			resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "execution is terminal"))
		}, fault.Unauthenticated},
		{"membership removed", func(_ *fakePrincipalResolver, authorizer *fakeAuthorizer) {
			authorizer.SetDecision(authorize.DecisionDeny)
		}, fault.Forbidden},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scope := testScope()
			human := humanPrincipal(scope)
			client := newFakeClient()
			clock := newManualClock()
			resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
			authorizer := &fakeAuthorizer{decision: authorize.DecisionAllow}
			env := newBrokerEnv(t, brokerOptions{
				connector:  &fakeConnector{clients: []nats.Client{client}},
				clock:      clock,
				resolver:   resolver,
				authorizer: authorizer,
				idle:       time.Second,
			})
			calls := &counter{}
			if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			fakeSub := client.lastSubscription(t)
			clock.WaitForSleep(t)
			if sleeps := clock.Sleeps(); len(sleeps) != 1 || sleeps[0] != env.idle {
				t.Fatalf("idle sleeps = %v; want [%v]", sleeps, env.idle)
			}

			testCase.mutate(resolver, authorizer)
			clock.Advance(env.idle)
			fakeSub.waitClosed(t)
			if got := calls.get(); got != 0 {
				t.Fatalf("handler calls = %d; want 0 after idle invalidation", got)
			}
			if got := clock.Active(); got != 0 {
				t.Fatalf("active injected sleepers = %d; want 0", got)
			}

			// The same credential cannot open a fresh subscription.
			before := len(client.subscriptions())
			_, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls)))
			requireClass(t, err, testCase.want)
			if got := len(client.subscriptions()); got != before {
				t.Fatalf("revoked credential opened %d client subscriptions; want %d", got, before)
			}
		})
	}
}

// TestSubscribeInvalidationVersusDeliveryBarrier pins the race between an
// in-flight delivery and a concurrent invalidation: once invalidation is
// observed, no new delivery reaches the handler.
func TestSubscribeInvalidationVersusDeliveryBarrier(t *testing.T) {
	scope := testScope()
	human := humanPrincipal(scope)
	client := newFakeClient()
	clock := newManualClock()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		clock:     clock,
		resolver:  resolver,
		idle:      time.Second,
	})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	calls := &counter{}
	handler := func(context.Context, token.Invalidation) error {
		calls.inc()
		started <- struct{}{}
		<-release
		return nil
	}
	if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	clock.WaitForSleep(t)

	first := mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service")))
	delivered := make(chan error, 1)
	go func() {
		delivered <- fakeSub.deliver(context.Background(), first)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	// Invalidate the credential while the first delivery is still in flight.
	resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential revoked"))
	clock.Advance(env.idle)
	fakeSub.waitClosed(t)
	close(release)
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight delivery did not finish")
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("handler calls after barrier = %d; want 1", got)
	}

	second := mustJSON(t, validInvalidation(scope, id.PrincipalID("another-human"), service.ServiceID("another-service")))
	if err := fakeSub.deliver(context.Background(), second); err == nil {
		t.Fatal("delivery after observed invalidation succeeded")
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("handler calls after observed invalidation = %d; want 1", got)
	}
}

// TestSubscribeReconnectRevalidatesEstablishedSubscriptions pins reconnect
// behavior: a failed publication reconnects and every live subscription is
// revalidated. Still-authorized subscriptions move to the new client with the
// same subject; revoked ones close.
func TestSubscribeReconnectRevalidatesEstablishedSubscriptions(t *testing.T) {
	t.Run("valid authority resubscribes", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		first := newFakeClient()
		second := newFakeClient()
		connector := &fakeConnector{clients: []nats.Client{first, second}}
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		env := newBrokerEnv(t, brokerOptions{
			connector: connector,
			resolver:  resolver,
			// The first publication attempt fails so the adapter reconnects.
			maxAttempts: 2,
		})
		calls := &counter{}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		oldSub := first.lastSubscription(t)
		first.failJetStream(errBrokerDown)

		if err := env.broker.PublishEvent(context.Background(), validEnvelope(scope, id.EventID("event-reconnect"))); err != nil {
			t.Fatalf("PublishEvent after reconnect: %v", err)
		}
		oldSub.waitClosed(t)
		subs := second.subscriptions()
		if len(subs) != 1 {
			t.Fatalf("new client subscriptions = %d; want 1", len(subs))
		}
		wantSubject, err := nats.InvalidationSubject(scope)
		if err != nil {
			t.Fatalf("InvalidationSubject: %v", err)
		}
		if subs[0].subject != wantSubject {
			t.Fatalf("reconnected subject = %q; want %q", subs[0].subject, wantSubject)
		}
		payload := mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service")))
		if err := subs[0].deliver(context.Background(), payload); err != nil {
			t.Fatalf("delivery on reconnected subscription: %v", err)
		}
		if got := calls.get(); got != 1 {
			t.Fatalf("handler calls = %d; want 1", got)
		}
	})

	t.Run("revoked authority closes", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		first := newFakeClient()
		second := newFakeClient()
		connector := &fakeConnector{clients: []nats.Client{first, second}}
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		env := newBrokerEnv(t, brokerOptions{
			connector:   connector,
			resolver:    resolver,
			maxAttempts: 2,
		})
		calls := &counter{}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		oldSub := first.lastSubscription(t)
		first.failJetStream(errBrokerDown)
		resolver.Fail(testCredential, fault.New(fault.Unauthenticated, "credential revoked"))

		if err := env.broker.PublishEvent(context.Background(), validEnvelope(scope, id.EventID("event-reconnect-revoked"))); err != nil {
			t.Fatalf("PublishEvent after reconnect: %v", err)
		}
		oldSub.waitClosed(t)
		if got := len(second.subscriptions()); got != 0 {
			t.Fatalf("new client subscriptions = %d; want 0 after revocation", got)
		}
		if got := calls.get(); got != 0 {
			t.Fatalf("handler calls = %d; want 0", got)
		}

		// The revoked credential cannot open a fresh subscription.
		_, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls)))
		requireClass(t, err, fault.Unauthenticated)
		if got := len(second.subscriptions()); got != 0 {
			t.Fatalf("revoked credential opened %d client subscriptions; want 0", got)
		}
	})
}

// TestSubscribeCloseJoinsWorkers pins Close: it closes the client
// subscriptions and the client, cancels and joins the idle worker, is
// idempotent, and refuses later publication.
func TestSubscribeCloseJoinsWorkers(t *testing.T) {
	scope := testScope()
	human := humanPrincipal(scope)
	client := newFakeClient()
	clock := newManualClock()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		clock:     clock,
		resolver:  resolver,
		idle:      time.Second,
	})
	calls := &counter{}
	if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	clock.WaitForSleep(t)

	if err := env.broker.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fakeSub.waitClosed(t)
	if got := clock.Active(); got != 0 {
		t.Fatalf("active injected sleepers after Close = %d; want 0 (worker not joined)", got)
	}
	if got := client.closeCount(); got != 1 {
		t.Fatalf("client Close calls = %d; want 1", got)
	}

	// Close is idempotent.
	if err := env.broker.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := client.closeCount(); got != 1 {
		t.Fatalf("client Close calls after second Close = %d; want 1", got)
	}

	// A closed adapter never dials again.
	before := env.connector.callCount()
	if err := env.broker.PublishEvent(context.Background(), validEnvelope(scope, id.EventID("event-after-close"))); err == nil {
		t.Fatal("publication succeeded after Close")
	}
	if got := env.connector.callCount(); got != before {
		t.Fatalf("Connect calls after Close = %d; want %d", got, before)
	}
}

// TestSubscribeCredentialNeverLeaks pins that a credential sentinel never
// appears in a subscription error.
func TestSubscribeCredentialNeverLeaks(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	resolver := &fakePrincipalResolver{}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		resolver:  resolver,
	})
	calls := &counter{}

	resolver.Fail(testSentinel, fault.New(fault.Unauthenticated, "credential rejected"))
	_, err := env.broker.Subscribe(context.Background(), subscribeRequest(testSentinel, scope, loggingHandler(calls)))
	if err == nil {
		t.Fatal("unknown credential accepted")
	}
	if strings.Contains(err.Error(), testSentinel) {
		t.Fatalf("subscription error leaks the credential: %v", err)
	}

	resolver.Set(testSentinel, humanPrincipal(scope))
	if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testSentinel, scope, loggingHandler(calls))); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	resolver.Fail(testSentinel, fault.New(fault.Unauthenticated, "credential revoked"))
	deliveryErr := fakeSub.deliver(context.Background(), mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service"))))
	if deliveryErr == nil {
		t.Fatal("delivery after revocation succeeded")
	}
	if strings.Contains(deliveryErr.Error(), testSentinel) {
		t.Fatalf("delivery error leaks the credential: %v", deliveryErr)
	}
}

// TestSubscribeToleratesDuplicateAndReorderedDelivery pins at-least-once
// delivery: duplicates and reordering are delivered in client order, and a
// handler error is returned to the client (retryable) without closing the
// subscription.
func TestSubscribeToleratesDuplicateAndReorderedDelivery(t *testing.T) {
	t.Run("duplicates and reordering", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		client := newFakeClient()
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		env := newBrokerEnv(t, brokerOptions{
			connector: &fakeConnector{clients: []nats.Client{client}},
			resolver:  resolver,
		})
		var mu sync.Mutex
		var seen []time.Time
		handler := func(_ context.Context, invalidation token.Invalidation) error {
			mu.Lock()
			seen = append(seen, invalidation.At)
			mu.Unlock()
			return nil
		}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler)); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		fakeSub := client.lastSubscription(t)
		older := validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service"))
		older.At = testNow().Add(-time.Minute)
		newer := validInvalidation(scope, id.PrincipalID("another-human"), service.ServiceID("another-service"))
		newer.At = testNow()

		payloads := [][]byte{mustJSON(t, older), mustJSON(t, older), mustJSON(t, newer)}
		for index, payload := range payloads {
			if err := fakeSub.deliver(context.Background(), payload); err != nil {
				t.Fatalf("delivery %d: %v", index, err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 3 {
			t.Fatalf("handler calls = %d; want 3 (duplicates are not hidden)", len(seen))
		}
		if !seen[0].Equal(older.At) || !seen[1].Equal(older.At) || !seen[2].Equal(newer.At) {
			t.Fatalf("delivery order = %v; want client order [older, older, newer]", seen)
		}
		if fakeSub.isClosed() {
			t.Fatal("duplicate/reordered delivery closed the subscription")
		}
	})

	t.Run("handler failure is retryable", func(t *testing.T) {
		scope := testScope()
		human := humanPrincipal(scope)
		client := newFakeClient()
		resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
		env := newBrokerEnv(t, brokerOptions{
			connector: &fakeConnector{clients: []nats.Client{client}},
			resolver:  resolver,
		})
		calls := &counter{}
		handler := func(context.Context, token.Invalidation) error {
			calls.inc()
			return errHandlerFailed
		}
		if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, handler)); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		fakeSub := client.lastSubscription(t)
		payload := mustJSON(t, validInvalidation(scope, id.PrincipalID("other-human"), service.ServiceID("other-service")))
		for attempt := 1; attempt <= 2; attempt++ {
			err := fakeSub.deliver(context.Background(), payload)
			if !errors.Is(err, errHandlerFailed) {
				t.Fatalf("delivery %d error = %v; want handler failure", attempt, err)
			}
		}
		if got := calls.get(); got != 2 {
			t.Fatalf("handler calls = %d; want 2 retries", got)
		}
		if fakeSub.isClosed() {
			t.Fatal("handler failure closed the subscription; delivery must stay retryable")
		}
	})
}

// TestSubscribeRefusesCrossScopeInvalidation pins defense in depth: a message
// on a scope-qualified subject that names another scope is refused and does
// not reach the handler.
func TestSubscribeRefusesCrossScopeInvalidation(t *testing.T) {
	scope := testScope()
	human := humanPrincipal(scope)
	client := newFakeClient()
	resolver := &fakePrincipalResolver{byToken: map[string]identity.Principal{testCredential: human}}
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
		resolver:  resolver,
	})
	calls := &counter{}
	if _, err := env.broker.Subscribe(context.Background(), subscribeRequest(testCredential, scope, loggingHandler(calls))); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	crossScope := validInvalidation(otherScope(), id.PrincipalID("other-human"), service.ServiceID("other-service"))
	err := fakeSub.deliver(context.Background(), mustJSON(t, crossScope))
	if err == nil {
		t.Fatal("cross-scope invalidation accepted")
	}
	if got := calls.get(); got != 0 {
		t.Fatalf("handler calls = %d; want 0", got)
	}
	if fakeSub.isClosed() {
		t.Fatal("a cross-scope message closed the subscription instead of being refused")
	}
}
