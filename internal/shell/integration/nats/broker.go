package nats

import (
	"context"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Broker is the concrete NATS adapter. It satisfies event.Publisher,
// Publisher, AuthenticatedSubscriber, token.InvalidationPort and
// log.LiveSubscriber.
type Broker struct {
	connect             Connect
	clock               Clock
	principal           authorize.CurrentPrincipal
	authorizer          authorize.Authorizer
	sink                ObservationSink
	maxReconnectBackoff time.Duration
	idleValidation      time.Duration
	maxPublishAttempts  int

	baseCtx context.Context
	cancel  context.CancelFunc

	mu                sync.Mutex
	client            Client
	closed            bool
	subscriptions     map[*subscription]struct{}
	liveSubscriptions map[*liveSubscription]struct{}
	// resubscribePending records that the current subscriptions were opened on
	// a client that has since been discarded. The next ensureClient that
	// establishes a client claims it and re-establishes them.
	resubscribePending bool
	// resubscribeMu serializes discard transitions and re-establishment
	// passes, so a subscription is never moved onto a client that was itself
	// discarded meanwhile and concurrent ensureClient callers resume once.
	resubscribeMu sync.Mutex
}

// NewBroker validates the configuration and builds a broker adapter. It never
// dials the broker.
func NewBroker(config BrokerConfig) (*Broker, error) {
	switch {
	case config.Connect == nil:
		return nil, fault.New(fault.InvalidInput, "nats connect function is nil")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "nats clock is nil")
	case config.Principal == nil:
		return nil, fault.New(fault.InvalidInput, "nats principal resolver is nil")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "nats authorizer is nil")
	case config.Sink == nil:
		return nil, fault.New(fault.InvalidInput, "nats observation sink is nil")
	case config.MaxReconnectBackoff <= 0:
		return nil, fault.New(fault.InvalidInput, "nats maximum reconnect backoff must be positive")
	case config.IdleValidation <= 0:
		return nil, fault.New(fault.InvalidInput, "nats idle validation interval must be positive")
	case config.MaxPublishAttempts <= 0:
		return nil, fault.New(fault.InvalidInput, "nats maximum publication attempts must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Broker{
		connect:             config.Connect,
		clock:               config.Clock,
		principal:           config.Principal,
		authorizer:          config.Authorizer,
		sink:                config.Sink,
		maxReconnectBackoff: config.MaxReconnectBackoff,
		idleValidation:      config.IdleValidation,
		maxPublishAttempts:  config.MaxPublishAttempts,
		baseCtx:             ctx,
		cancel:              cancel,
		subscriptions:       make(map[*subscription]struct{}),
		liveSubscriptions:   make(map[*liveSubscription]struct{}),
	}, nil
}

// Close closes every live subscription, cancels and joins the idle workers and
// closes the client. It is idempotent and a closed adapter never dials again.
func (broker *Broker) Close(ctx context.Context) error {
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return nil
	}
	broker.closed = true
	client := broker.client
	broker.client = nil
	subscriptions := make([]*subscription, 0, len(broker.subscriptions))
	for subscription := range broker.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	broker.subscriptions = make(map[*subscription]struct{})
	liveSubscriptions := make([]*liveSubscription, 0, len(broker.liveSubscriptions))
	for subscription := range broker.liveSubscriptions {
		liveSubscriptions = append(liveSubscriptions, subscription)
	}
	broker.liveSubscriptions = make(map[*liveSubscription]struct{})
	broker.mu.Unlock()

	broker.cancel()
	for _, subscription := range subscriptions {
		subscription.shutdown()
	}
	for _, subscription := range subscriptions {
		subscription.wait()
	}
	for _, subscription := range liveSubscriptions {
		subscription.shutdown()
	}
	for _, subscription := range liveSubscriptions {
		subscription.wait()
	}
	if client != nil {
		if err := client.Close(ctx); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "closing the broker client failed", err)
		}
	}
	return nil
}

// isClosed reports whether Close has run.
func (broker *Broker) isClosed() bool {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.closed
}

// ensureClient returns the shared client, dialing once through Connect if no
// client is current. A successful establishment also resumes every subscription
// that was left on a discarded client, so any caller (publication,
// subscription, live stream) repairs the live subscriptions, not only the
// publication that observed the outage.
func (broker *Broker) ensureClient(ctx context.Context) (Client, error) {
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return nil, fault.New(fault.DependencyUnavailable, "broker adapter is closed")
	}
	client := broker.client
	if client == nil {
		if err := ctx.Err(); err != nil {
			broker.mu.Unlock()
			return nil, fault.Wrap(fault.DeadlineExpired, "broker connection was canceled", err)
		}
		dialed, err := broker.connect(ctx)
		if err != nil {
			broker.mu.Unlock()
			return nil, err
		}
		if dialed == nil {
			broker.mu.Unlock()
			return nil, fault.New(fault.DependencyUnavailable, "broker connect returned no client")
		}
		broker.client = dialed
		client = dialed
	}
	pending := broker.resubscribePending
	broker.mu.Unlock()

	if pending {
		broker.resumeSubscriptions(ctx)
	}
	return client, nil
}

// discardClient drops the suspect client after a failed publication and
// records that its subscriptions need re-establishment. The reconnect is
// attempted eagerly, but if the broker is still down the pending marker makes
// the next successful ensureClient resume them instead of orphaning an open
// invalidation or live-log subscription.
func (broker *Broker) discardClient(ctx context.Context, suspect Client) {
	broker.resubscribeMu.Lock()
	broker.mu.Lock()
	if broker.client != suspect {
		broker.mu.Unlock()
		broker.resubscribeMu.Unlock()
		return
	}
	broker.client = nil
	if len(broker.subscriptions) > 0 || len(broker.liveSubscriptions) > 0 {
		broker.resubscribePending = true
	}
	broker.mu.Unlock()
	broker.resubscribeMu.Unlock()

	_ = suspect.Close(ctx)
	// A failed eager reconnect leaves resubscribePending set for the next
	// successful ensureClient.
	_, _ = broker.ensureClient(ctx)
}

// resumeSubscriptions re-establishes every subscription after a client was
// discarded. The pending marker is claimed and the current subscriptions are
// snapshotted under broker.mu, then each is revalidated and moved to the
// current client outside the lock. resubscribeMu makes concurrent
// ensureClient callers resume exactly once, and makes a discard wait for an
// in-flight resume so no subscription is ever moved onto a client that is
// discarded in the meantime.
func (broker *Broker) resumeSubscriptions(ctx context.Context) {
	broker.resubscribeMu.Lock()
	defer broker.resubscribeMu.Unlock()

	broker.mu.Lock()
	if broker.closed || !broker.resubscribePending || broker.client == nil {
		broker.mu.Unlock()
		return
	}
	broker.resubscribePending = false
	client := broker.client
	subscriptions := make([]*subscription, 0, len(broker.subscriptions))
	for subscription := range broker.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	liveSubscriptions := make([]*liveSubscription, 0, len(broker.liveSubscriptions))
	for liveSubscription := range broker.liveSubscriptions {
		liveSubscriptions = append(liveSubscriptions, liveSubscription)
	}
	broker.mu.Unlock()

	for _, subscription := range subscriptions {
		if broker.isClosed() {
			return
		}
		subscription.reestablish(ctx, client)
	}
	for _, liveSubscription := range liveSubscriptions {
		if broker.isClosed() {
			return
		}
		liveSubscription.reestablish(ctx, client)
	}
}

// unregister removes a closed subscription from the live set.
func (broker *Broker) unregister(subscription *subscription) {
	broker.mu.Lock()
	delete(broker.subscriptions, subscription)
	broker.mu.Unlock()
}

// unregisterLive removes a closed live log subscription from the live set.
func (broker *Broker) unregisterLive(subscription *liveSubscription) {
	broker.mu.Lock()
	delete(broker.liveSubscriptions, subscription)
	broker.mu.Unlock()
}

// backoff returns the bounded delay before the given one-based retry. It
// starts below the cap and never exceeds MaxReconnectBackoff.
func (broker *Broker) backoff(retry int) time.Duration {
	delay := broker.maxReconnectBackoff
	if retry > 0 && retry < 4 {
		delay = broker.maxReconnectBackoff >> (4 - retry)
	}
	if delay <= 0 {
		delay = time.Nanosecond
	}
	if delay > broker.maxReconnectBackoff {
		delay = broker.maxReconnectBackoff
	}
	return delay
}

// dependencyError preserves a typed fault and maps a plain dependency error to
// dependency_unavailable.
func dependencyError(message string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	return fault.Wrap(fault.DependencyUnavailable, message, err)
}

// resolveCurrent authenticates one credential and requires a current enabled
// principal. A credential is never rendered in an error.
func (broker *Broker) resolveCurrent(ctx context.Context, credential string) (identity.Principal, error) {
	principal, err := broker.principal.Resolve(ctx, credential)
	if err != nil {
		return identity.Principal{}, dependencyError("credential could not be verified", err)
	}
	if err := principal.Validate(); err != nil {
		return identity.Principal{}, err
	}
	if !principal.Enabled {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "principal is not current")
	}
	return principal, nil
}

// authorize asks the current authorizer for one operation grant. A refusal is
// forbidden and an authorizer failure is dependency_unavailable; neither is
// ever an allow.
func (broker *Broker) authorize(ctx context.Context, principal identity.Principal, operation authorize.OperationName, scope tenancy.Scope) error {
	decision, err := broker.authorizer.Authorize(ctx, principal, operation, scope)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "authorization is unavailable", err)
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, "operation is not granted in the requested scope")
	}
	return nil
}
