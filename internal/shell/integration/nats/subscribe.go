package nats

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

const (
	// OperationEventSubscribe is the registry operation a live invalidation
	// subscriber must currently hold.
	OperationEventSubscribe authorize.OperationName = "event.subscribe"
	// OperationEventPublish is the registry operation an ingress observation
	// must currently hold in the authenticated scope.
	OperationEventPublish authorize.OperationName = "event.publish"
)

// Subscribe opens one scope-qualified invalidation subscription. The presented
// credential is authenticated, its principal must be current and enabled, its
// scope must equal the requested scope and the authorizer must currently allow
// event.subscribe there. Every delivery, idle tick and reconnect rechecks the
// same facts and fails closed.
func (broker *Broker) Subscribe(ctx context.Context, request SubscriptionRequest) (Subscription, error) {
	if request.Handler == nil {
		return nil, fault.New(fault.InvalidInput, "invalidation handler is nil")
	}
	if err := request.Scope.Validate(); err != nil {
		return nil, err
	}
	principal, err := broker.resolveCurrent(ctx, request.Credential)
	if err != nil {
		return nil, err
	}
	if principal.Scope != request.Scope {
		return nil, fault.New(fault.Forbidden, "credential scope does not cover the subscription scope")
	}
	if err := broker.authorize(ctx, principal, OperationEventSubscribe, request.Scope); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DeadlineExpired, "subscription was canceled", err)
	}
	subject, err := InvalidationSubject(request.Scope)
	if err != nil {
		return nil, err
	}
	client, err := broker.ensureClient(ctx)
	if err != nil {
		return nil, dependencyError("broker subscription failed", err)
	}

	workerCtx, cancel := context.WithCancel(broker.baseCtx)
	subscription := &subscription{
		broker:     broker,
		scope:      request.Scope,
		credential: request.Credential,
		handler:    request.Handler,
		principal:  principal,
		ctx:        workerCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	clientSubscription, err := client.Subscribe(ctx, subject, subscription.handleDelivery)
	if err != nil {
		cancel()
		return nil, fault.Wrap(fault.DependencyUnavailable, "broker subscription failed", err)
	}

	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		cancel()
		_ = clientSubscription.Close(ctx)
		return nil, fault.New(fault.DependencyUnavailable, "broker adapter is closed")
	}
	subscription.mu.Lock()
	subscription.clientSubscription = clientSubscription
	subscription.started = true
	subscription.mu.Unlock()
	broker.subscriptions[subscription] = struct{}{}
	broker.mu.Unlock()

	go subscription.idleLoop()
	return subscription, nil
}

// subscription is one live invalidation subscription. Its immutable authority
// snapshot is only used to recognise a self-invalidation; every delivery and
// idle tick re-reads current authority.
type subscription struct {
	broker     *Broker
	scope      tenancy.Scope
	credential string
	handler    InvalidationHandler
	principal  identity.Principal

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu                 sync.Mutex
	clientSubscription Subscription
	started            bool
	closed             bool
}

// Close closes the subscription, cancels and joins the idle worker. It is
// idempotent.
func (subscription *subscription) Close(context.Context) error {
	subscription.shutdown()
	subscription.wait()
	return nil
}

// handleDelivery revalidates current authority, validates the invalidation,
// refuses a cross-scope message without closing, delivers at-least-once and
// closes on a self-invalidation.
func (subscription *subscription) handleDelivery(ctx context.Context, payload []byte) error {
	if subscription.isClosed() {
		return fault.New(fault.DependencyUnavailable, "subscription is closed")
	}
	if err := subscription.revalidate(ctx); err != nil {
		subscription.shutdown()
		return err
	}
	var invalidation token.Invalidation
	if err := json.Unmarshal(payload, &invalidation); err != nil {
		return fault.Wrap(fault.InvalidInput, "invalidation payload is not JSON", err)
	}
	if err := invalidation.Validate(); err != nil {
		return err
	}
	if invalidation.Scope != subscription.scope {
		return fault.New(fault.InvalidInput, "invalidation names another scope")
	}
	if err := subscription.handler(ctx, invalidation); err != nil {
		return err
	}
	if invalidation.PrincipalID == subscription.principal.ID {
		subscription.shutdown()
	}
	return nil
}

// idleLoop revalidates current authority on every idle tick. A clock whose
// Sleep returns without advancing time has no tick to validate and would
// busy-loop, so the loop stops; delivery and reconnect still revalidate.
func (subscription *subscription) idleLoop() {
	defer close(subscription.done)
	last := subscription.broker.clock.Now()
	for {
		if err := subscription.broker.clock.Sleep(subscription.ctx, subscription.broker.idleValidation); err != nil {
			return
		}
		now := subscription.broker.clock.Now()
		if !now.After(last) {
			return
		}
		last = now
		if subscription.isClosed() {
			return
		}
		if err := subscription.revalidate(subscription.ctx); err != nil {
			subscription.shutdown()
			return
		}
	}
}

// revalidate re-reads the presented credential and the event.subscribe grant.
func (subscription *subscription) revalidate(ctx context.Context) error {
	principal, err := subscription.broker.principal.Resolve(ctx, subscription.credential)
	if err != nil {
		return dependencyError("credential could not be verified", err)
	}
	if !principal.Enabled {
		return fault.New(fault.Unauthenticated, "principal is not current")
	}
	if principal.Scope != subscription.scope {
		return fault.New(fault.Forbidden, "credential scope changed")
	}
	decision, err := subscription.broker.authorizer.Authorize(ctx, principal, OperationEventSubscribe, subscription.scope)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "authorization is unavailable", err)
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, "subscription grant was removed")
	}
	return nil
}

// reestablish moves one still-authorized subscription to a fresh client with
// the same subject; a subscription that no longer revalidates is closed.
func (subscription *subscription) reestablish(ctx context.Context, client Client) {
	if subscription.isClosed() {
		return
	}
	if err := subscription.revalidate(ctx); err != nil {
		subscription.shutdown()
		return
	}
	subject, err := InvalidationSubject(subscription.scope)
	if err != nil {
		subscription.shutdown()
		return
	}
	clientSubscription, err := client.Subscribe(ctx, subject, subscription.handleDelivery)
	if err != nil {
		subscription.shutdown()
		return
	}
	subscription.mu.Lock()
	if subscription.closed {
		subscription.mu.Unlock()
		_ = clientSubscription.Close(context.Background())
		return
	}
	previous := subscription.clientSubscription
	subscription.clientSubscription = clientSubscription
	subscription.mu.Unlock()
	if previous != nil {
		_ = previous.Close(context.Background())
	}
}

// shutdown closes the client subscription and stops the idle worker. It is
// idempotent and never waits for the worker.
func (subscription *subscription) shutdown() {
	subscription.mu.Lock()
	if subscription.closed {
		subscription.mu.Unlock()
		return
	}
	subscription.closed = true
	clientSubscription := subscription.clientSubscription
	subscription.clientSubscription = nil
	subscription.mu.Unlock()

	subscription.cancel()
	subscription.broker.unregister(subscription)
	if clientSubscription != nil {
		_ = clientSubscription.Close(context.Background())
	}
}

// wait joins the idle worker when it was started.
func (subscription *subscription) wait() {
	subscription.mu.Lock()
	started := subscription.started
	subscription.mu.Unlock()
	if started {
		<-subscription.done
	}
}

// isClosed reports whether shutdown has run.
func (subscription *subscription) isClosed() bool {
	subscription.mu.Lock()
	defer subscription.mu.Unlock()
	return subscription.closed
}
