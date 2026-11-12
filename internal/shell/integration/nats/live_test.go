package nats_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

var _ log.LiveSubscriber = (*nats.Broker)(nil)

// validLiveChunk is one well-formed live log chunk for the given execution.
func validLiveChunk(executionID id.ExecutionID) log.Chunk {
	return log.Chunk{
		ExecutionID: executionID,
		ActionID:    id.ActionID("action-1"),
		Stream:      log.StreamStdout,
		Offset:      0,
		Sequence:    1,
		Data:        []byte("line\n"),
	}
}

// subscriptionWithSubject returns the fake client subscription opened on the
// exact subject.
func subscriptionWithSubject(t *testing.T, client *fakeClient, subject nats.Subject) *fakeSubscription {
	t.Helper()
	for _, subscription := range client.subscriptions() {
		if subscription.subject == subject {
			return subscription
		}
	}
	t.Fatalf("fake client has no subscription on %q", subject)
	return nil
}

// TestSubscribeLiveDeliversChunksForItsScopeOnly pins the Core NATS live log
// subscription: the subject derives only from the supplied scope and
// execution, and a subscription never receives another scope's chunks.
func TestSubscribeLiveDeliversChunksForItsScopeOnly(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-live")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)

	chunk := validLiveChunk(executionID)
	if err := logSubscription.deliver(context.Background(), mustJSON(t, chunk)); err != nil {
		t.Fatalf("live delivery: %v", err)
	}
	nextCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received, err := live.Next(nextCtx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if received.ExecutionID != chunk.ExecutionID || received.ActionID != chunk.ActionID ||
		received.Stream != chunk.Stream || received.Offset != chunk.Offset ||
		received.Sequence != chunk.Sequence || !bytes.Equal(received.Data, chunk.Data) {
		t.Fatalf("Next returned %+v; want %+v", received, chunk)
	}

	// A second execution in another scope has a different subject; delivering
	// there never reaches the first subscription.
	other := otherScope()
	otherLive, err := env.broker.SubscribeLive(context.Background(), other, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive other scope: %v", err)
	}
	otherSubject, err := nats.LogSubject(other, executionID)
	if err != nil {
		t.Fatalf("LogSubject other scope: %v", err)
	}
	if otherSubject == logSubject {
		t.Fatalf("both scopes derived subject %q", logSubject)
	}
	otherSubscription := subscriptionWithSubject(t, client, otherSubject)
	otherChunk := validLiveChunk(executionID)
	otherChunk.Data = []byte("other\n")
	if err := otherSubscription.deliver(context.Background(), mustJSON(t, otherChunk)); err != nil {
		t.Fatalf("other-scope live delivery: %v", err)
	}
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := live.Next(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first subscription Next error = %v; want deadline_exceeded (cross-scope leak)", err)
	}
	otherCtx, cancelOther := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelOther()
	if _, err := otherLive.Next(otherCtx); err != nil {
		t.Fatalf("other subscription Next: %v", err)
	}
}

// TestSubscribeLiveRevocationClosesSubscription pins fail-closed live
// authorization: an observed invalidation on the scope closes the live
// subscription, no later chunk is delivered and Next reports closure.
func TestSubscribeLiveRevocationClosesSubscription(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-revoked")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	invalidationSubject, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)
	watcher := subscriptionWithSubject(t, client, invalidationSubject)

	// A chunk already buffered when the invalidation is observed must never be
	// handed to the consumer.
	if err := logSubscription.deliver(context.Background(), mustJSON(t, validLiveChunk(executionID))); err != nil {
		t.Fatalf("pre-invalidation delivery: %v", err)
	}

	invalidation := token.Invalidation{
		Scope:       scope,
		PrincipalID: id.PrincipalID("execution-principal-1"),
		ExecutionID: executionID,
		Reason:      token.InvalidationRevoked,
		At:          testNow(),
	}
	if err := watcher.deliver(context.Background(), mustJSON(t, invalidation)); err != nil {
		t.Fatalf("invalidation delivery: %v", err)
	}
	logSubscription.waitClosed(t)
	watcher.waitClosed(t)
	if err := logSubscription.deliver(context.Background(), mustJSON(t, validLiveChunk(executionID))); err == nil {
		t.Fatal("chunk delivered after the subscription was invalidated")
	}
	nextCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := live.Next(nextCtx); err == nil {
		t.Fatal("Next succeeded after the subscription was invalidated")
	}
}

// TestSubscribeLiveRefusesCrossScopeInvalidation pins that an invalidation
// naming another scope is refused without closing the live subscription.
func TestSubscribeLiveRefusesCrossScopeInvalidation(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-cross")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	invalidationSubject, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	watcher := subscriptionWithSubject(t, client, invalidationSubject)
	crossScope := token.Invalidation{
		Scope:       otherScope(),
		PrincipalID: id.PrincipalID("other-human"),
		ServiceID:   service.ServiceID("other-service"),
		Reason:      token.InvalidationRevoked,
		At:          testNow(),
	}
	if err := watcher.deliver(context.Background(), mustJSON(t, crossScope)); err == nil {
		t.Fatal("cross-scope invalidation accepted")
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)
	if logSubscription.isClosed() {
		t.Fatal("cross-scope invalidation closed the live subscription")
	}
	if err := logSubscription.deliver(context.Background(), mustJSON(t, validLiveChunk(executionID))); err != nil {
		t.Fatalf("live delivery after refused cross-scope invalidation: %v", err)
	}
	nextCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := live.Next(nextCtx); err != nil {
		t.Fatalf("Next after refused cross-scope invalidation: %v", err)
	}
}

// TestSubscribeLiveRefusesInvalidScopeAndExecution pins that wildcard, foreign
// or scope-less subscriptions are refused before any dial.
func TestSubscribeLiveRefusesInvalidScopeAndExecution(t *testing.T) {
	client := newFakeClient()
	connector := &fakeConnector{clients: []nats.Client{client}}
	env := newBrokerEnv(t, brokerOptions{connector: connector})
	cases := []struct {
		name        string
		scope       tenancy.Scope
		executionID id.ExecutionID
	}{
		{"scope-less", tenancy.Scope{}, id.ExecutionID("exec-1")},
		{"tenant-wide placeholder project", tenancy.Scope{TenantID: "tenant-a", ProjectID: "_"}, id.ExecutionID("exec-1")},
		{"wildcard project", tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-*"}, id.ExecutionID("exec-1")},
		{"invalid tenant", tenancy.Scope{TenantID: "tenant a"}, id.ExecutionID("exec-1")},
		{"empty execution", testScope(), id.ExecutionID("")},
		{"invalid execution", testScope(), id.ExecutionID("exec 1")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := env.broker.SubscribeLive(context.Background(), testCase.scope, testCase.executionID)
			requireClass(t, err, fault.InvalidInput)
		})
	}
	if got := connector.callCount(); got != 0 {
		t.Fatalf("Connect calls = %d; want 0 for refused subscriptions", got)
	}
	if got := len(client.subscriptions()); got != 0 {
		t.Fatalf("client subscriptions = %d; want 0", got)
	}
}

// TestSubscribeLiveCloseJoinsAndIsIdempotent pins that Close releases the
// client subscriptions, makes Next report closure and can be repeated.
func TestSubscribeLiveCloseJoinsAndIsIdempotent(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-close")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	invalidationSubject, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)
	watcher := subscriptionWithSubject(t, client, invalidationSubject)

	if err := live.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	logSubscription.waitClosed(t)
	watcher.waitClosed(t)
	if err := live.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := live.Next(context.Background()); err == nil {
		t.Fatal("Next succeeded after Close")
	}

	// Broker.Close also releases live subscriptions.
	secondLive, err := env.broker.SubscribeLive(context.Background(), scope, id.ExecutionID("exec-broker-close"))
	if err != nil {
		t.Fatalf("SubscribeLive second: %v", err)
	}
	secondSubject, err := nats.LogSubject(scope, id.ExecutionID("exec-broker-close"))
	if err != nil {
		t.Fatalf("LogSubject second: %v", err)
	}
	secondLog := subscriptionWithSubject(t, client, secondSubject)
	if err := env.broker.Close(context.Background()); err != nil {
		t.Fatalf("Broker.Close: %v", err)
	}
	secondLog.waitClosed(t)
	if _, err := secondLive.Next(context.Background()); err == nil {
		t.Fatal("Next succeeded after Broker.Close")
	}
}

// TestSubscribeLiveSignalsOverflow pins bounded buffering: a consumer that
// stops reading never grows memory without bound and observes an explicit
// overflow error instead of a silent drop.
func TestSubscribeLiveSignalsOverflow(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-overflow")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)
	payload := mustJSON(t, validLiveChunk(executionID))
	for index := range 4096 {
		if err := logSubscription.deliver(context.Background(), payload); err != nil {
			t.Fatalf("delivery %d: %v", index, err)
		}
	}
	nextCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := live.Next(nextCtx); !errors.Is(err, nats.ErrLiveOverflow) {
		t.Fatalf("Next error = %v; want ErrLiveOverflow", err)
	}
}

// TestSubscribeLiveReestablishesOnReconnect pins that a live subscription
// follows the broker onto a fresh client with the same scope-qualified subject.
func TestSubscribeLiveReestablishesOnReconnect(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-reconnect")
	first := newFakeClient()
	second := newFakeClient()
	connector := &fakeConnector{clients: []nats.Client{first, second}}
	env := newBrokerEnv(t, brokerOptions{connector: connector, maxAttempts: 2})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	oldLog := subscriptionWithSubject(t, first, logSubject)
	first.failJetStream(errBrokerDown)

	if err := env.broker.PublishEvent(context.Background(), validEnvelope(scope, id.EventID("event-live-reconnect"))); err != nil {
		t.Fatalf("PublishEvent after reconnect: %v", err)
	}
	oldLog.waitClosed(t)
	newLog := subscriptionWithSubject(t, second, logSubject)
	chunk := validLiveChunk(executionID)
	if err := newLog.deliver(context.Background(), mustJSON(t, chunk)); err != nil {
		t.Fatalf("delivery on re-established live subscription: %v", err)
	}
	nextCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received, err := live.Next(nextCtx)
	if err != nil {
		t.Fatalf("Next after reconnect: %v", err)
	}
	if !bytes.Equal(received.Data, chunk.Data) {
		t.Fatalf("received data = %q; want %q", received.Data, chunk.Data)
	}
}

// TestSubscribeLiveConcurrentDeliveryAndClose is a race check: deliveries,
// Next calls and Close may overlap.
func TestSubscribeLiveConcurrentDeliveryAndClose(t *testing.T) {
	scope := testScope()
	executionID := id.ExecutionID("exec-race")
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})

	live, err := env.broker.SubscribeLive(context.Background(), scope, executionID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	logSubject, err := nats.LogSubject(scope, executionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	logSubscription := subscriptionWithSubject(t, client, logSubject)
	payload := mustJSON(t, validLiveChunk(executionID))

	var deliveries sync.WaitGroup
	for range 8 {
		deliveries.Go(func() {
			for range 100 {
				_ = logSubscription.deliver(context.Background(), payload)
			}
		})
	}
	readerCtx, cancelReader := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelReader()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			if _, err := live.Next(readerCtx); err != nil {
				return
			}
		}
	}()
	deliveries.Wait()
	if err := live.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not stop after Close")
	}
}
