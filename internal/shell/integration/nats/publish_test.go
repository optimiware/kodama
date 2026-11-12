package nats_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestNewBrokerValidatesConfig pins constructor validation: every dependency
// and bound is explicit, and construction never dials the broker.
func TestNewBrokerValidatesConfig(t *testing.T) {
	connector := &fakeConnector{clients: []nats.Client{newFakeClient()}}
	valid := func() nats.BrokerConfig {
		return nats.BrokerConfig{
			Connect:             connector.Connect,
			Clock:               newAutoClock(),
			Principal:           &fakePrincipalResolver{},
			Authorizer:          &fakeAuthorizer{decision: authorize.DecisionAllow},
			Sink:                &fakeSink{},
			MaxReconnectBackoff: time.Second,
			IdleValidation:      time.Second,
			MaxPublishAttempts:  1,
		}
	}

	broker, err := nats.NewBroker(valid())
	if err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if broker == nil {
		t.Fatal("valid config returned a nil broker")
	}
	if connector.callCount() != 0 {
		t.Fatalf("NewBroker dialed the broker %d times; want lazy connection", connector.callCount())
	}
	if err := broker.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	invalid := []struct {
		name   string
		mutate func(*nats.BrokerConfig)
	}{
		{"nil connect", func(config *nats.BrokerConfig) { config.Connect = nil }},
		{"nil clock", func(config *nats.BrokerConfig) { config.Clock = nil }},
		{"nil principal", func(config *nats.BrokerConfig) { config.Principal = nil }},
		{"nil authorizer", func(config *nats.BrokerConfig) { config.Authorizer = nil }},
		{"nil sink", func(config *nats.BrokerConfig) { config.Sink = nil }},
		{"zero backoff", func(config *nats.BrokerConfig) { config.MaxReconnectBackoff = 0 }},
		{"negative backoff", func(config *nats.BrokerConfig) { config.MaxReconnectBackoff = -time.Second }},
		{"zero idle validation", func(config *nats.BrokerConfig) { config.IdleValidation = 0 }},
		{"zero attempts", func(config *nats.BrokerConfig) { config.MaxPublishAttempts = 0 }},
		{"negative attempts", func(config *nats.BrokerConfig) { config.MaxPublishAttempts = -1 }},
	}
	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			config := valid()
			testCase.mutate(&config)
			broker, err := nats.NewBroker(config)
			if err == nil {
				_ = broker.Close(context.Background())
				t.Fatal("invalid config accepted")
			}
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

// TestPublishSatisfiesEventPublisherSeam pins the event.Publisher method used
// by the post-commit publication reader: Publish is the same publication as
// PublishEvent, with the stable id as the JetStream identity.
func TestPublishSatisfiesEventPublisherSeam(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})
	envelope := validEnvelope(scope, id.EventID("event-seam"))

	if err := env.broker.Publish(context.Background(), envelope); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	calls := client.jetstreamCalls()
	if len(calls) != 1 {
		t.Fatalf("JetStream publications = %d; want 1", len(calls))
	}
	if calls[0].eventID != envelope.ID {
		t.Fatalf("publication event id = %q; want %q", calls[0].eventID, envelope.ID)
	}
	wantSubject, err := nats.EventSubject(scope)
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	if calls[0].subject != wantSubject {
		t.Fatalf("publication subject = %q; want %q", calls[0].subject, wantSubject)
	}
	wantPayload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !bytes.Equal(calls[0].payload, wantPayload) {
		t.Fatalf("publication payload = %s; want %s", calls[0].payload, wantPayload)
	}
}

// TestPublishEventCanonicalJSONAndJetStreamIdentity pins committed-event
// publication: canonical public event JSON, the event id as the JetStream
// publication identity, and the scope-derived subject.
func TestPublishEventCanonicalJSONAndJetStreamIdentity(t *testing.T) {
	scope := testScope()
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})
	envelope := validEnvelope(scope, id.EventID("event-1"))

	if err := env.broker.PublishEvent(context.Background(), envelope); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	calls := client.jetstreamCalls()
	if len(calls) != 1 {
		t.Fatalf("JetStream publications = %d; want 1", len(calls))
	}
	wantSubject, err := nats.EventSubject(scope)
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	if calls[0].subject != wantSubject {
		t.Fatalf("publication subject = %q; want %q", calls[0].subject, wantSubject)
	}
	if calls[0].eventID != envelope.ID {
		t.Fatalf("publication event id = %q; want %q", calls[0].eventID, envelope.ID)
	}
	wantPayload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(envelope): %v", err)
	}
	if !bytes.Equal(calls[0].payload, wantPayload) {
		t.Fatalf("publication payload = %s; want canonical envelope JSON %s", calls[0].payload, wantPayload)
	}

	// The payload is the canonical public event JSON: it decodes to api.Event
	// and validates under the public envelope rules.
	var public api.Event
	if err := json.Unmarshal(calls[0].payload, &public); err != nil {
		t.Fatalf("payload is not public event JSON: %v", err)
	}
	if err := public.Validate(); err != nil {
		t.Fatalf("payload is not a valid public event: %v", err)
	}
	if public.ID != string(envelope.ID) || public.Type != string(envelope.Type) {
		t.Fatalf("public event identity = (%q,%q); want (%q,%q)", public.ID, public.Type, envelope.ID, envelope.Type)
	}
	if len(client.coreCalls()) != 0 {
		t.Fatalf("committed event used Core NATS %d times; want JetStream only", len(client.coreCalls()))
	}
}

// TestPublishEventRetryAfterLostAcknowledgementKeepsIdentity pins the lost
// acknowledgement case: the broker stored the event but the caller saw an
// error, so the retry must carry the same id and the same payload, and the
// broker deduplicates instead of creating a second event.
func TestPublishEventRetryAfterLostAcknowledgementKeepsIdentity(t *testing.T) {
	store := &fakeEventStore{}
	first := newFakeClient()
	first.onJetStream = func(call brokerCall) error {
		if err := store.publish(call.eventID, call.payload); err != nil {
			return err
		}
		return errBrokerDown // stored, but the acknowledgement is lost
	}
	second := newFakeClient()
	second.onJetStream = func(call brokerCall) error {
		return store.publish(call.eventID, call.payload)
	}
	connector := &fakeConnector{clients: []nats.Client{first, second}}
	env := newBrokerEnv(t, brokerOptions{connector: connector, maxAttempts: 2})
	envelope := validEnvelope(testScope(), id.EventID("event-lost-ack"))

	if err := env.broker.PublishEvent(context.Background(), envelope); err != nil {
		t.Fatalf("PublishEvent after lost acknowledgement: %v", err)
	}
	if connector.callCount() != 2 {
		t.Fatalf("Connect calls = %d; want 2 (a failed publication reconnects)", connector.callCount())
	}
	firstCalls := first.jetstreamCalls()
	secondCalls := second.jetstreamCalls()
	if len(firstCalls) != 1 || len(secondCalls) != 1 {
		t.Fatalf("publication attempts = %d/%d; want 1/1", len(firstCalls), len(secondCalls))
	}
	if firstCalls[0].eventID != secondCalls[0].eventID {
		t.Fatalf("retry event id = %q; want unchanged %q", secondCalls[0].eventID, firstCalls[0].eventID)
	}
	if !bytes.Equal(firstCalls[0].payload, secondCalls[0].payload) {
		t.Fatal("retry payload changed; a lost acknowledgement must not rewrite the event")
	}
	if store.count() != 1 {
		t.Fatalf("durable events = %d; want 1 after deduplicated retry", store.count())
	}
	if store.conflictCount() != 0 {
		t.Fatalf("same event id carried %d changed payloads", store.conflictCount())
	}
	sleeps := env.clock.Sleeps()
	if len(sleeps) != 1 {
		t.Fatalf("backoff sleeps = %d; want 1", len(sleeps))
	}
	if sleeps[0] <= 0 || sleeps[0] > 50*time.Millisecond {
		t.Fatalf("backoff = %v; want > 0 and <= 50ms", sleeps[0])
	}
}

// TestPublishEventBrokerOutageIsBounded pins bounded publication failure: the
// caller sees dependency_unavailable after a finite number of attempts and the
// backoff uses the injected clock, never wall time.
func TestPublishEventBrokerOutageIsBounded(t *testing.T) {
	connector := alwaysFailingConnector(8)
	env := newBrokerEnv(t, brokerOptions{connector: connector, maxAttempts: 3})

	err := env.broker.PublishEvent(context.Background(), validEnvelope(testScope(), id.EventID("event-outage")))
	requireClass(t, err, fault.DependencyUnavailable)

	if got := connector.callCount(); got != 3 {
		t.Fatalf("Connect calls = %d; want 3 bounded attempts", got)
	}
	sleeps := env.clock.Sleeps()
	if len(sleeps) != 2 {
		t.Fatalf("backoff sleeps = %d; want 2 between 3 attempts", len(sleeps))
	}
	for index, duration := range sleeps {
		if duration <= 0 || duration > 50*time.Millisecond {
			t.Fatalf("backoff[%d] = %v; want > 0 and <= 50ms", index, duration)
		}
	}
}

// TestPublishEventCancellationStopsBackoff pins cancellation: a canceled
// context stops the bounded retry loop, returns deadline_expired wrapping the
// context error and leaves no sleeper behind.
func TestPublishEventCancellationStopsBackoff(t *testing.T) {
	connector := alwaysFailingConnector(100)
	clock := newManualClock()
	env := newBrokerEnv(t, brokerOptions{connector: connector, clock: clock, maxAttempts: 100})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- env.broker.PublishEvent(ctx, validEnvelope(testScope(), id.EventID("event-cancel")))
	}()

	clock.WaitForSleep(t)
	cancel()

	select {
	case err := <-done:
		requireClass(t, err, fault.DeadlineExpired)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error does not wrap context.Canceled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishEvent did not return after cancellation")
	}
	if got := connector.callCount(); got != 1 {
		t.Fatalf("Connect calls after cancellation = %d; want 1", got)
	}
	if got := clock.Active(); got != 0 {
		t.Fatalf("active injected sleepers = %d; want 0", got)
	}
}

// TestPublishEventRefusesInvalidEnvelopeBeforeDial pins local validation before
// any broker interaction, including the injective scope encoding.
func TestPublishEventRefusesInvalidEnvelopeBeforeDial(t *testing.T) {
	client := newFakeClient()
	connector := &fakeConnector{clients: []nats.Client{client}}
	env := newBrokerEnv(t, brokerOptions{connector: connector})

	invalid := validEnvelope(testScope(), id.EventID("event-invalid"))
	invalid.ID = ""
	requireClass(t, env.broker.PublishEvent(context.Background(), invalid), fault.InvalidInput)

	placeholderScope := validEnvelope(tenancy.Scope{TenantID: "tenant-a", ProjectID: "_"}, id.EventID("event-placeholder"))
	requireClass(t, env.broker.PublishEvent(context.Background(), placeholderScope), fault.InvalidInput)

	if got := connector.callCount(); got != 0 {
		t.Fatalf("Connect calls = %d; want 0 before local validation", got)
	}
	if got := len(client.jetstreamCalls()); got != 0 {
		t.Fatalf("publications = %d; want 0", got)
	}
}

// TestPublishLiveUsesCoreOnlyAndScopeSubject pins live logs as Core-only,
// scope-derived publications; a chunk cannot widen or reinterpret the scope.
func TestPublishLiveUsesCoreOnlyAndScopeSubject(t *testing.T) {
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})
	scope := testScope()
	chunk := log.Chunk{
		ExecutionID: id.ExecutionID("exec-1"),
		ActionID:    id.ActionID("action-1"),
		Stream:      log.StreamStdout,
		Offset:      7,
		Sequence:    2,
		Data:        []byte("hello world"),
	}

	if err := env.broker.PublishLive(context.Background(), scope, chunk); err != nil {
		t.Fatalf("PublishLive: %v", err)
	}
	calls := client.coreCalls()
	if len(calls) != 1 {
		t.Fatalf("Core publications = %d; want 1", len(calls))
	}
	wantSubject, err := nats.LogSubject(scope, chunk.ExecutionID)
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	if calls[0].subject != wantSubject {
		t.Fatalf("live subject = %q; want %q", calls[0].subject, wantSubject)
	}
	wantPayload, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("json.Marshal(chunk): %v", err)
	}
	if !bytes.Equal(calls[0].payload, wantPayload) {
		t.Fatalf("live payload = %s; want %s", calls[0].payload, wantPayload)
	}
	if len(client.jetstreamCalls()) != 0 {
		t.Fatal("live log used JetStream; live progress is Core-only")
	}

	// The subject derives only from the explicit scope: the same chunk in
	// another scope is a different subject and never leaks the first scope.
	if err := env.broker.PublishLive(context.Background(), otherScope(), chunk); err != nil {
		t.Fatalf("PublishLive(other scope): %v", err)
	}
	all := client.coreCalls()
	if len(all) != 2 {
		t.Fatalf("Core publications = %d; want 2", len(all))
	}
	if all[1].subject == all[0].subject {
		t.Fatalf("both scopes published to %q; want distinct scoped subjects", all[0].subject)
	}
	if bytes.Contains(all[1].payload, []byte("tenant-a")) {
		t.Fatal("live payload leaks the other scope")
	}

	// Invalid scope and execution identity are refused before any call.
	before := len(client.coreCalls())
	requireClass(t, env.broker.PublishLive(context.Background(), tenancy.Scope{}, chunk), fault.InvalidInput)
	badChunk := chunk
	badChunk.ExecutionID = id.ExecutionID("bad/exec")
	requireClass(t, env.broker.PublishLive(context.Background(), scope, badChunk), fault.InvalidInput)
	if got := len(client.coreCalls()); got != before {
		t.Fatalf("Core publications after refusals = %d; want %d", got, before)
	}
}

// TestPublishLiveBrokerFailureIsReturned pins that a live publication failure
// is reported to the caller rather than silently swallowed or retried into
// domain progress.
func TestPublishLiveBrokerFailureIsReturned(t *testing.T) {
	client := newFakeClient()
	client.coreErr = errBrokerDown
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})
	chunk := log.Chunk{ExecutionID: id.ExecutionID("exec-1"), ActionID: id.ActionID("action-1"), Stream: log.StreamStderr, Data: []byte("x")}
	requireClass(t, env.broker.PublishLive(context.Background(), testScope(), chunk), fault.DependencyUnavailable)
}

// TestBrokerConcurrentPublication exercises the shared connection and retry
// state under -race: concurrent committed publications must all be recorded
// with distinct ids.
func TestBrokerConcurrentPublication(t *testing.T) {
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{connector: &fakeConnector{clients: []nats.Client{client}}})
	const count = 32

	var wait sync.WaitGroup
	errs := make(chan error, count)
	for index := range count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			envelope := validEnvelope(testScope(), id.EventID(fmt.Sprintf("event-%02d", index)))
			errs <- env.broker.PublishEvent(context.Background(), envelope)
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PublishEvent: %v", err)
		}
	}

	calls := client.jetstreamCalls()
	if len(calls) != count {
		t.Fatalf("JetStream publications = %d; want %d", len(calls), count)
	}
	seen := make(map[id.EventID]bool, count)
	for _, call := range calls {
		if seen[call.eventID] {
			t.Fatalf("duplicate event id %q", call.eventID)
		}
		seen[call.eventID] = true
	}
}

// TestPublishEventFailureDoesNotMutateEnvelope pins that publication is an
// effect over an already-committed value: a broker failure changes neither the
// envelope nor the caller's payload.
func TestPublishEventFailureDoesNotMutateEnvelope(t *testing.T) {
	connector := alwaysFailingConnector(4)
	env := newBrokerEnv(t, brokerOptions{connector: connector, maxAttempts: 2})
	envelope := validEnvelope(testScope(), id.EventID("event-immutable"))
	before, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	_ = env.broker.PublishEvent(context.Background(), envelope)
	after, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("broker failure mutated the committed envelope")
	}
}
