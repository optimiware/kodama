// Package nats_test pins the NATS adapter and live-authorization API.
//
// # Exported API
//
//	// Client is the fakeable broker client seam. PublishJetStream carries the
//	// stable event id as the broker publication identity so a retry with the
//	// same id is deduplicated; PublishCore carries live progress only.
//	type Client interface {
//	    PublishJetStream(ctx context.Context, subject Subject, eventID id.EventID, payload []byte) error
//	    PublishCore(ctx context.Context, subject Subject, payload []byte) error
//	    Subscribe(ctx context.Context, subject Subject, handler func(context.Context, []byte) error) (Subscription, error)
//	    Close(ctx context.Context) error
//	}
//
//	type Connect func(context.Context) (Client, error)
//
//	// Clock is the injected time source. Sleep returns ctx.Err() when the
//	// context ends.
//	type Clock interface {
//	    Now() time.Time
//	    Sleep(ctx context.Context, duration time.Duration) error
//	}
//
//	type BrokerConfig struct {
//	    Connect             Connect
//	    Clock               Clock
//	    Principal           authorize.CurrentPrincipal
//	    Authorizer          authorize.Authorizer
//	    Sink                ObservationSink
//	    MaxReconnectBackoff time.Duration
//	    IdleValidation      time.Duration
//	    MaxPublishAttempts  int
//	}
//
//	func NewBroker(BrokerConfig) (*Broker, error)
//
//	// Publish is the event.Publisher seam; PublishEvent is the scope-qualified
//	// nats.Publisher seam. Both publish the same already-committed envelope
//	// with identical semantics.
//	func (*Broker) Publish(context.Context, event.Envelope) error
//	func (*Broker) PublishEvent(context.Context, event.Envelope) error
//	func (*Broker) PublishLive(context.Context, tenancy.Scope, log.Chunk) error
//	func (*Broker) Subscribe(context.Context, SubscriptionRequest) (Subscription, error)
//	func (*Broker) Observe(context.Context, ObservationRequest) error
//	func (*Broker) Close(context.Context) error
//
//	type SubscriptionRequest struct {
//	    Credential string
//	    Scope      tenancy.Scope
//	    Handler    InvalidationHandler
//	}
//
//	type AuthenticatedSubscriber interface {
//	    Subscribe(context.Context, SubscriptionRequest) (Subscription, error)
//	}
//
//	type ObservationSink interface {
//	    Observe(context.Context, Observation) error
//	}
//
//	type Observation struct {
//	    Scope  tenancy.Scope
//	    Source event.Source
//	    Type   event.Type
//	    Data   []byte
//	}
//
//	type ObservationRequest struct {
//	    Credential     string
//	    Type           event.Type
//	    Data           []byte
//	    DeclaredSource *event.Source
//	    DeclaredScope  *tenancy.Scope
//	}
//
//	// IngressSubject is the separate authenticated observation-ingress
//	// subject: kodama.v1.<tenant>.<project|_>.ingress
//	func IngressSubject(tenancy.Scope) (Subject, error)
//
// # Ports and subjects
//
// The concrete *Broker implements nats.Publisher (PublishEvent + PublishLive)
// and event.Publisher. The credential-less nats.Subscriber port is not
// implemented by *Broker; the explicit-credential shape is the
// AuthenticatedSubscriber interface above. `ingress` is a subject kind, so
// `Subject.Validate` accepts exactly five tokens ending in `ingress` and
// `ParseScope` recovers its scope like the other kinds.
//
// # Behavior
//
// Subjects. Every subject names exactly one scope and never contains a
// wildcard. Project id "_" is refused by every builder (it is the tenant-wide
// placeholder), so the encoding stays injective. Unknown kinds, command-style
// suffixes, empty tokens and missing/extra tokens are refused by
// Subject.Validate.
//
// Publication. PublishEvent validates the envelope first (invalid_input before
// any dial), derives the subject from envelope.Scope only, marshals the
// canonical public event JSON (the encoding/json form of event.Envelope, which
// is byte-for-byte the public api.Event JSON), and calls
// Client.PublishJetStream with envelope.ID as the publication identity. Retries
// use the same event id and the same payload; a failed attempt discards the
// suspect client and the next attempt dials again through Connect. Retries are
// bounded by MaxPublishAttempts and separated by clock.Sleep backoff capped at
// MaxReconnectBackoff. An exhausted publication returns dependency_unavailable;
// a canceled/expired context returns deadline_expired wrapping ctx.Err().
// PublishLive derives the subject from the supplied scope and chunk execution
// only, marshals the canonical chunk JSON, and calls Client.PublishCore; it
// never uses JetStream. Broker failure is returned to the caller and never
// mutates the envelope or blocks domain progress.
//
// Subscriptions. Subscribe validates the request, resolves the presented
// credential through Principal, requires an enabled principal whose scope
// equals the requested scope, and requires Authorizer to allow the
// "event.subscribe" operation in that scope. The client subscription is on
// InvalidationSubject(scope). The credential and authorization are rechecked
// before every delivered invalidation, on every timed idle tick
// (IdleValidation) and after a reconnect; any failure fails closed: the
// subscription is closed and no handler runs. A delivered token.Invalidation
// must validate and name the subscription's scope; a malformed or cross-scope
// message is refused without closing the subscription. Duplicate and reordered
// deliveries are delivered as-is (at-least-once, consumer-ordered); a handler
// error is returned to the client and the subscription stays open. An
// invalidation naming the subscriber's own principal is delivered once and then
// closes the subscription; no later delivery reaches the handler. After a
// publication failure forces a reconnect, every live subscription is
// revalidated: still-authorized subscriptions are re-established on the new
// client with the same subject, and failed ones are closed. Close is
// idempotent, closes client subscriptions and the client, cancels the idle
// workers and joins them before returning. The adapter never renders a
// credential in an error.
//
// Observation ingress. Observe is the handler for the dedicated ingress
// subject. It resolves the presented credential, refuses a human principal, and
// derives the event source from the authenticated principal
// (execution -> execution source, service -> service source) and the scope from
// the authenticated principal, never from the submitted body. The type must be
// observation.custom; a lifecycle or verified-report type is refused
// (forbidden) because the ingress can never impersonate a committed fact. An
// optional DeclaredSource/DeclaredScope that disagrees with authentication is
// refused; a matching declaration is accepted. The authorizer must allow
// "event.publish" in the authenticated scope. The adapter hands the
// authenticated Observation to Sink and never publishes the observation
// directly to a committed-event subject. A sink fault is propagated with its
// class; a plain sink error becomes dependency_unavailable. Data is bounded by
// event.DefaultBounds.MaxDataBytes inclusively.
//
// Fault mapping used by these tests: invalid shape -> invalid_input; unknown,
// disabled or expired credential -> unauthenticated; scope mismatch or denied
// grant -> forbidden; broker/authorizer/sink failure -> dependency_unavailable;
// canceled context -> deadline_expired.
package nats_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Compile-time conformance: the concrete adapter must satisfy the event
// publisher seam, the scope-qualified publisher port and the authenticated
// subscriber port, and the fakes must satisfy the seams they replace.
var (
	_ event.Publisher              = (*nats.Broker)(nil)
	_ nats.Publisher               = (*nats.Broker)(nil)
	_ nats.AuthenticatedSubscriber = (*nats.Broker)(nil)

	_ nats.Client          = (*fakeClient)(nil)
	_ nats.Subscription    = (*fakeSubscription)(nil)
	_ nats.Clock           = (*fakeClock)(nil)
	_ nats.ObservationSink = (*fakeSink)(nil)

	_ authorize.CurrentPrincipal = (*fakePrincipalResolver)(nil)
	_ authorize.Authorizer       = (*fakeAuthorizer)(nil)
)

var (
	errBrokerDown       = errors.New("fake broker is down")
	errSubscriptionGone = errors.New("fake subscription is closed")
	errHandlerFailed    = errors.New("fake handler failed")
)

const (
	testCredential = "credential-1"
	testSentinel   = "credential-sentinel-do-not-leak"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-a"),
		ProjectID: id.ProjectID("project-1"),
	}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-b"),
		ProjectID: id.ProjectID("project-2"),
	}
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}

func humanPrincipal(scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 3,
	}
}

func disabledHumanPrincipal(scope tenancy.Scope) identity.Principal {
	principal := humanPrincipal(scope)
	principal.Enabled = false
	return principal
}

func servicePrincipal(scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("service-1"),
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 2,
	}
}

func executionPrincipal(scope tenancy.Scope, executionID id.ExecutionID) identity.Principal {
	return identity.Principal{
		ID:          id.PrincipalID("execution-principal-1"),
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: executionID,
	}
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", value, err)
	}
	return payload
}

func validEnvelope(scope tenancy.Scope, eventID id.EventID) event.Envelope {
	return event.Envelope{
		ID:             eventID,
		Type:           event.TypeTaskCreated,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID("human-1")},
		At:             testNow(),
		Scope:          scope,
		SubjectID:      "task-1",
		SubjectVersion: 4,
		Data:           []byte(`{"task":"task-1"}`),
	}
}

func validInvalidation(scope tenancy.Scope, principalID id.PrincipalID, serviceID service.ServiceID) token.Invalidation {
	return token.Invalidation{
		Scope:       scope,
		PrincipalID: principalID,
		ServiceID:   serviceID,
		Reason:      token.InvalidationRevoked,
		At:          testNow(),
	}
}

// counter is a mutex-guarded call counter usable from test goroutines.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// brokerCall is one recorded publication through the fake client.
type brokerCall struct {
	subject nats.Subject
	eventID id.EventID
	payload []byte
}

// fakeEventStore deduplicates JetStream publications by event id and detects a
// changed payload under the same id. It models broker-side dedupe.
type fakeEventStore struct {
	mu        sync.Mutex
	byID      map[id.EventID][]byte
	conflicts int
}

func (store *fakeEventStore) publish(eventID id.EventID, payload []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.byID == nil {
		store.byID = make(map[id.EventID][]byte)
	}
	if existing, ok := store.byID[eventID]; ok {
		if !bytes.Equal(existing, payload) {
			store.conflicts++
		}
		return nil
	}
	store.byID[eventID] = append([]byte(nil), payload...)
	return nil
}

func (store *fakeEventStore) count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.byID)
}

func (store *fakeEventStore) conflictCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.conflicts
}

// fakeClient is the in-memory broker client seam. It records every call, keeps
// opened subscriptions and can be switched to a failing state.
type fakeClient struct {
	mu sync.Mutex

	jetstream []brokerCall
	core      []brokerCall
	subs      []*fakeSubscription

	closed     bool
	closeCalls int

	jetstreamErr error
	coreErr      error
	subscribeErr error
	onJetStream  func(brokerCall) error
}

func newFakeClient() *fakeClient {
	return &fakeClient{}
}

func (client *fakeClient) PublishJetStream(_ context.Context, subject nats.Subject, eventID id.EventID, payload []byte) error {
	call := brokerCall{subject: subject, eventID: eventID, payload: append([]byte(nil), payload...)}
	client.mu.Lock()
	client.jetstream = append(client.jetstream, call)
	closed := client.closed
	onJetStream := client.onJetStream
	publishErr := client.jetstreamErr
	client.mu.Unlock()
	if closed {
		return errBrokerDown
	}
	if onJetStream != nil {
		return onJetStream(call)
	}
	return publishErr
}

func (client *fakeClient) PublishCore(_ context.Context, subject nats.Subject, payload []byte) error {
	call := brokerCall{subject: subject, payload: append([]byte(nil), payload...)}
	client.mu.Lock()
	client.core = append(client.core, call)
	closed := client.closed
	publishErr := client.coreErr
	client.mu.Unlock()
	if closed {
		return errBrokerDown
	}
	return publishErr
}

func (client *fakeClient) Subscribe(_ context.Context, subject nats.Subject, handler func(context.Context, []byte) error) (nats.Subscription, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil, errBrokerDown
	}
	if client.subscribeErr != nil {
		return nil, client.subscribeErr
	}
	subscription := &fakeSubscription{
		subject: subject,
		handler: handler,
		closeCh: make(chan struct{}),
	}
	client.subs = append(client.subs, subscription)
	return subscription, nil
}

func (client *fakeClient) Close(context.Context) error {
	client.mu.Lock()
	client.closed = true
	client.closeCalls++
	client.mu.Unlock()
	return nil
}

func (client *fakeClient) jetstreamCalls() []brokerCall {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]brokerCall(nil), client.jetstream...)
}

func (client *fakeClient) coreCalls() []brokerCall {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]brokerCall(nil), client.core...)
}

func (client *fakeClient) subscriptions() []*fakeSubscription {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]*fakeSubscription(nil), client.subs...)
}

func (client *fakeClient) lastSubscription(t *testing.T) *fakeSubscription {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.subs) == 0 {
		t.Fatal("fake client has no subscription")
	}
	return client.subs[len(client.subs)-1]
}

func (client *fakeClient) closeCount() int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.closeCalls
}

func (client *fakeClient) failJetStream(err error) {
	client.mu.Lock()
	client.jetstreamErr = err
	client.mu.Unlock()
}

// fakeSubscription is one subscription opened by the fake client.
type fakeSubscription struct {
	subject nats.Subject
	handler func(context.Context, []byte) error

	mu         sync.Mutex
	closed     bool
	closeCalls int
	closeCh    chan struct{}
}

func (subscription *fakeSubscription) Close(context.Context) error {
	subscription.mu.Lock()
	if !subscription.closed {
		subscription.closed = true
		close(subscription.closeCh)
	}
	subscription.closeCalls++
	subscription.mu.Unlock()
	return nil
}

func (subscription *fakeSubscription) deliver(ctx context.Context, payload []byte) error {
	subscription.mu.Lock()
	closed := subscription.closed
	handler := subscription.handler
	subscription.mu.Unlock()
	if closed {
		return errSubscriptionGone
	}
	return handler(ctx, payload)
}

func (subscription *fakeSubscription) isClosed() bool {
	subscription.mu.Lock()
	defer subscription.mu.Unlock()
	return subscription.closed
}

func (subscription *fakeSubscription) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-subscription.closeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription was not closed")
	}
}

// fakeConnector supplies clients in order, optionally failing an attempt.
type fakeConnector struct {
	mu      sync.Mutex
	clients []nats.Client
	errs    []error
	calls   int
}

func (connector *fakeConnector) Connect(context.Context) (nats.Client, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	index := connector.calls
	connector.calls++
	if index < len(connector.errs) && connector.errs[index] != nil {
		return nil, connector.errs[index]
	}
	if len(connector.clients) == 0 {
		return nil, errBrokerDown
	}
	if index < len(connector.clients) {
		return connector.clients[index], nil
	}
	return connector.clients[len(connector.clients)-1], nil
}

func (connector *fakeConnector) callCount() int {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	return connector.calls
}

func alwaysFailingConnector(attempts int) *fakeConnector {
	errs := make([]error, attempts)
	for index := range errs {
		errs[index] = errBrokerDown
	}
	return &fakeConnector{errs: errs}
}

// fakeClock is a deterministic injected clock. Auto mode returns immediately
// from Sleep; manual mode blocks until Advance or context cancellation.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	auto    bool
	waiters map[*fakeWaiter]struct{}
	sleeps  []time.Duration
	slept   chan struct{}
}

type fakeWaiter struct {
	done chan struct{}
}

func newAutoClock() *fakeClock {
	return &fakeClock{now: testNow(), auto: true, slept: make(chan struct{}, 1)}
}

func newManualClock() *fakeClock {
	return &fakeClock{now: testNow(), waiters: make(map[*fakeWaiter]struct{}), slept: make(chan struct{}, 1)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	clock.sleeps = append(clock.sleeps, duration)
	if clock.auto {
		clock.mu.Unlock()
		return nil
	}
	waiter := &fakeWaiter{done: make(chan struct{})}
	clock.waiters[waiter] = struct{}{}
	clock.mu.Unlock()

	select {
	case clock.slept <- struct{}{}:
	default:
	}

	select {
	case <-waiter.done:
		return nil
	case <-ctx.Done():
		clock.mu.Lock()
		delete(clock.waiters, waiter)
		clock.mu.Unlock()
		return ctx.Err()
	}
}

// Advance moves the clock and wakes every current sleeper.
func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	waiters := make([]*fakeWaiter, 0, len(clock.waiters))
	for waiter := range clock.waiters {
		waiters = append(waiters, waiter)
	}
	clock.waiters = make(map[*fakeWaiter]struct{})
	clock.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter.done)
	}
}

func (clock *fakeClock) Active() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return len(clock.waiters)
}

func (clock *fakeClock) Sleeps() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.sleeps...)
}

func (clock *fakeClock) WaitForSleep(t *testing.T) {
	t.Helper()
	select {
	case <-clock.slept:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not enter an injected Sleep")
	}
}

// fakePrincipalResolver is the authorize.CurrentPrincipal fake. The token map
// models current credential state; a token can be failed at any time to model
// rotation, revocation, disablement, expiry, request entry or terminal state.
type fakePrincipalResolver struct {
	mu        sync.Mutex
	byToken   map[string]identity.Principal
	errs      map[string]error
	onResolve func(string) (identity.Principal, error)
}

func (resolver *fakePrincipalResolver) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	resolver.mu.Lock()
	onResolve := resolver.onResolve
	principal, ok := resolver.byToken[credential]
	err := resolver.errs[credential]
	resolver.mu.Unlock()

	if onResolve != nil {
		return onResolve(credential)
	}
	if err != nil {
		return identity.Principal{}, err
	}
	if !ok {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "credential is not current")
	}
	return principal, nil
}

func (resolver *fakePrincipalResolver) Set(credential string, principal identity.Principal) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.byToken == nil {
		resolver.byToken = make(map[string]identity.Principal)
	}
	if resolver.errs != nil {
		delete(resolver.errs, credential)
	}
	resolver.byToken[credential] = principal
}

func (resolver *fakePrincipalResolver) Fail(credential string, err error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.errs == nil {
		resolver.errs = make(map[string]error)
	}
	resolver.errs[credential] = err
}

// authCall is one recorded authorization check.
type authCall struct {
	principal authorize.Principal
	operation authorize.OperationName
	scope     tenancy.Scope
}

type fakeAuthorizer struct {
	mu       sync.Mutex
	decision authorize.Decision
	err      error
	calls    []authCall
}

func (authorizer *fakeAuthorizer) Authorize(_ context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.calls = append(authorizer.calls, authCall{principal: principal, operation: operation, scope: scope})
	return authorizer.decision, authorizer.err
}

func (authorizer *fakeAuthorizer) SetDecision(decision authorize.Decision) {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.decision = decision
}

func (authorizer *fakeAuthorizer) SetError(err error) {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.err = err
}

func (authorizer *fakeAuthorizer) recorded() []authCall {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]authCall(nil), authorizer.calls...)
}

// fakeSink records authenticated observations handed over by the ingress.
type fakeSink struct {
	mu           sync.Mutex
	observations []nats.Observation
	err          error
	onObserve    func(nats.Observation) error
}

func (sink *fakeSink) Observe(_ context.Context, observation nats.Observation) error {
	sink.mu.Lock()
	sink.observations = append(sink.observations, observation)
	onObserve := sink.onObserve
	err := sink.err
	sink.mu.Unlock()
	if onObserve != nil {
		return onObserve(observation)
	}
	return err
}

func (sink *fakeSink) count() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.observations)
}

func (sink *fakeSink) last(t *testing.T) nats.Observation {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.observations) == 0 {
		t.Fatal("sink received no observation")
	}
	return sink.observations[len(sink.observations)-1]
}

func (sink *fakeSink) SetError(err error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.err = err
}

// brokerOptions configures the deterministic test environment.
type brokerOptions struct {
	connector   *fakeConnector
	clock       *fakeClock
	resolver    *fakePrincipalResolver
	authorizer  *fakeAuthorizer
	sink        *fakeSink
	maxAttempts int
	idle        time.Duration
	backoff     time.Duration
}

type brokerEnv struct {
	broker     *nats.Broker
	connector  *fakeConnector
	clock      *fakeClock
	resolver   *fakePrincipalResolver
	authorizer *fakeAuthorizer
	sink       *fakeSink
	idle       time.Duration
}

func newBrokerEnv(t *testing.T, options brokerOptions) *brokerEnv {
	t.Helper()
	if options.connector == nil {
		options.connector = &fakeConnector{clients: []nats.Client{newFakeClient()}}
	}
	if options.clock == nil {
		options.clock = newAutoClock()
	}
	if options.resolver == nil {
		options.resolver = &fakePrincipalResolver{}
	}
	if options.authorizer == nil {
		options.authorizer = &fakeAuthorizer{decision: authorize.DecisionAllow}
	}
	if options.sink == nil {
		options.sink = &fakeSink{}
	}
	if options.maxAttempts <= 0 {
		options.maxAttempts = 3
	}
	if options.idle <= 0 {
		options.idle = time.Second
	}
	if options.backoff <= 0 {
		options.backoff = 50 * time.Millisecond
	}

	broker, err := nats.NewBroker(nats.BrokerConfig{
		Connect:             options.connector.Connect,
		Clock:               options.clock,
		Principal:           options.resolver,
		Authorizer:          options.authorizer,
		Sink:                options.sink,
		MaxReconnectBackoff: options.backoff,
		IdleValidation:      options.idle,
		MaxPublishAttempts:  options.maxAttempts,
	})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	t.Cleanup(func() {
		_ = broker.Close(context.Background())
	})
	return &brokerEnv{
		broker:     broker,
		connector:  options.connector,
		clock:      options.clock,
		resolver:   options.resolver,
		authorizer: options.authorizer,
		sink:       options.sink,
		idle:       options.idle,
	}
}

func loggingHandler(calls *counter) nats.InvalidationHandler {
	return func(context.Context, token.Invalidation) error {
		calls.inc()
		return nil
	}
}
