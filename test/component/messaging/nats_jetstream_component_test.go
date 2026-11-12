//go:build component

package messaging_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestJetStreamDedupeAndSubjectLayout proves the real broker stores one
// message per stable event id and that the subjects follow the adapter
// contract exactly.
func TestJetStreamDedupeAndSubjectLayout(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	directory, _, _ := serviceIdentity("service-publisher", projectScope())
	adapter := newBroker(t, instance, directory, "conn-dedupe")
	ctx := instance.Shard.Context()

	first := eventEnvelope(t, projectScope(), "evt-1")
	if err := adapter.PublishEvent(ctx, first); err != nil {
		t.Fatalf("publish evt-1: %v", err)
	}
	if err := adapter.PublishEvent(ctx, first); err != nil {
		t.Fatalf("republish evt-1: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 1)
	info := instance.StreamInfo(t, support.EventsStream)
	if got, want := info.Config.Subjects, []string{support.EventsStreamSubjects}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("events stream subjects = %v; want %v", got, want)
	}
	support.Note(t, "dedupe_stored_messages", "1")

	second := eventEnvelope(t, tenantScope(), "evt-2")
	if err := adapter.PublishEvent(ctx, second); err != nil {
		t.Fatalf("publish evt-2: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 2)

	invalidation := support.ServiceInvalidation(projectScope(), id.PrincipalID("service-1"), service.ServiceID("service-1"), token.InvalidationRevoked, testNow())
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish invalidation: %v", err)
	}
	instance.WaitForStreamMessages(t, support.InvalidationsStream, 1)
	invalidationInfo := instance.StreamInfo(t, support.InvalidationsStream)
	if got := invalidationInfo.Config.Subjects; len(got) != 1 || got[0] != support.InvalidationsStreamSubjects {
		t.Fatalf("invalidations stream subjects = %v; want %v", got, support.InvalidationsStreamSubjects)
	}

	js := instance.JetStream(t)
	messages := fetch(t, js, support.EventsStream, jetstream.ConsumerConfig{
		Durable:       "inspect-layout",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, 3, 5*time.Second)
	if len(messages) != 2 {
		t.Fatalf("fetched %d event messages; want 2", len(messages))
	}
	wantSubjects := []string{"kodama.v1.tenant-a.project-1.events", "kodama.v1.tenant-a._.events"}
	wantIDs := []id.EventID{"evt-1", "evt-2"}
	for index, message := range messages {
		if message.Subject != wantSubjects[index] {
			t.Fatalf("message %d subject = %q; want %q", index, message.Subject, wantSubjects[index])
		}
		if envelope := decodeEvent(t, message.Data); envelope.ID != wantIDs[index] {
			t.Fatalf("message %d event id = %q; want %q", index, envelope.ID, wantIDs[index])
		}
		if err := message.ack(); err != nil {
			t.Fatalf("ack message %d: %v", index, err)
		}
	}

	// A subscription for one project scope never receives another scope's
	// events, while the matching scope is delivered.
	raw := instance.Raw(t)
	projectChannel := make(chan *natsgo.Msg, 8)
	projectSubject, err := broker.EventSubject(projectScope())
	if err != nil {
		t.Fatalf("project subject: %v", err)
	}
	subscription, err := raw.ChanSubscribe(string(projectSubject), projectChannel)
	if err != nil {
		t.Fatalf("subscribe project events: %v", err)
	}
	defer func() { _ = subscription.Unsubscribe() }()
	if err := raw.Flush(); err != nil {
		t.Fatalf("flush project subscription: %v", err)
	}
	if err := adapter.PublishEvent(ctx, eventEnvelope(t, tenantScope(), "evt-tenant-scoped")); err != nil {
		t.Fatalf("publish tenant event: %v", err)
	}
	requireNoReceive(t, projectChannel, 400*time.Millisecond, "cross-scope event")
	if err := adapter.PublishEvent(ctx, eventEnvelope(t, projectScope(), "evt-project-scoped")); err != nil {
		t.Fatalf("publish project event: %v", err)
	}
	delivered := receive(t, projectChannel, 10*time.Second, "project event")
	if envelope := decodeEvent(t, delivered.Data); envelope.ID != "evt-project-scoped" {
		t.Fatalf("delivered event id = %q; want evt-project-scoped", envelope.ID)
	}

	// Wildcard and mixed-scope subscriptions are refused before any dial.
	if err := broker.Subject("kodama.v1.tenant-a.*.events").Validate(); err == nil {
		t.Fatal("wildcard subject accepted; want refusal")
	}
	client := instance.DialDriver(t, instance.DriverConfig("conn-wildcard"))
	if _, err := client.Subscribe(ctx, broker.Subject("kodama.v1.tenant-a.>.events"), func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("wildcard subscription accepted; want refusal")
	}
	tenantDirectory, tenantCredential, _ := serviceIdentity("service-tenant", tenantScope())
	tenantBroker := newBroker(t, instance, tenantDirectory, "conn-mixed")
	if _, err := tenantBroker.Subscribe(ctx, broker.SubscriptionRequest{
		Credential: tenantCredential,
		Scope:      projectScope(),
		Handler:    func(context.Context, token.Invalidation) error { return nil },
	}); err == nil {
		t.Fatal("credential scope mixed with another subscription scope; want refusal")
	} else {
		requireClass(t, err, fault.Forbidden)
	}
	if _, err := broker.EventSubject(tenancyScopeWithPlaceholderProject()); err == nil {
		t.Fatal("project id \"_\" accepted; want refusal so the encoding stays injective")
	}
}

// TestJetStreamReplayCursorAndAck proves replay from a cursor, durable resume
// without duplicates and at-least-once redelivery of an unacknowledged
// message.
func TestJetStreamReplayCursorAndAck(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	directory, _, _ := serviceIdentity("service-replay", projectScope())
	adapter := newBroker(t, instance, directory, "conn-replay")
	ctx := instance.Shard.Context()
	for _, eventID := range []id.EventID{"evt-1", "evt-2", "evt-3"} {
		if err := adapter.PublishEvent(ctx, eventEnvelope(t, projectScope(), eventID)); err != nil {
			t.Fatalf("publish %s: %v", eventID, err)
		}
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 3)
	js := instance.JetStream(t)

	cursor := fetch(t, js, support.EventsStream, jetstream.ConsumerConfig{
		Durable:       "replay-cursor",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   2,
	}, 2, 5*time.Second)
	if len(cursor) != 2 {
		t.Fatalf("cursor replay returned %d messages; want 2", len(cursor))
	}
	if got := decodeEvent(t, cursor[0].Data).ID; got != "evt-2" {
		t.Fatalf("cursor first event = %q; want evt-2", got)
	}
	if got := decodeEvent(t, cursor[1].Data).ID; got != "evt-3" {
		t.Fatalf("cursor second event = %q; want evt-3", got)
	}
	for _, message := range cursor {
		if err := message.ack(); err != nil {
			t.Fatalf("ack cursor message: %v", err)
		}
	}

	resumeConfig := jetstream.ConsumerConfig{
		Durable:       "durable-resume",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}
	firstBatch := fetch(t, js, support.EventsStream, resumeConfig, 1, 5*time.Second)
	if len(firstBatch) != 1 || decodeEvent(t, firstBatch[0].Data).ID != "evt-1" {
		t.Fatalf("durable first delivery = %+v; want evt-1", firstBatch)
	}
	if err := firstBatch[0].ack(); err != nil {
		t.Fatalf("ack first durable message: %v", err)
	}
	secondBatch := fetch(t, js, support.EventsStream, resumeConfig, 2, 5*time.Second)
	if len(secondBatch) != 2 {
		t.Fatalf("durable resume returned %d messages; want 2", len(secondBatch))
	}
	if decodeEvent(t, secondBatch[0].Data).ID != "evt-2" || decodeEvent(t, secondBatch[1].Data).ID != "evt-3" {
		t.Fatalf("durable resume ids = %q, %q; want evt-2, evt-3",
			decodeEvent(t, secondBatch[0].Data).ID, decodeEvent(t, secondBatch[1].Data).ID)
	}
	for _, message := range secondBatch {
		if err := message.ack(); err != nil {
			t.Fatalf("ack resumed message: %v", err)
		}
	}

	redeliveryConfig := jetstream.ConsumerConfig{
		Durable:       "durable-redelivery",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       500 * time.Millisecond,
		MaxAckPending: 1,
	}
	unacked := fetch(t, js, support.EventsStream, redeliveryConfig, 1, 5*time.Second)
	if len(unacked) != 1 {
		t.Fatalf("redelivery consumer returned %d messages; want 1", len(unacked))
	}
	consumer, err := js.Consumer(ctx, support.EventsStream, "durable-redelivery")
	if err != nil {
		t.Fatalf("consumer handle: %v", err)
	}
	consumerInfo, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if consumerInfo.NumAckPending != 1 {
		t.Fatalf("ack pending after unacked delivery = %d; want 1", consumerInfo.NumAckPending)
	}
	redelivered := fetch(t, js, support.EventsStream, redeliveryConfig, 1, 10*time.Second)
	if len(redelivered) != 1 {
		t.Fatalf("redelivery returned %d messages; want 1", len(redelivered))
	}
	if redelivered[0].Sequence != unacked[0].Sequence {
		t.Fatalf("redelivered sequence = %d; want %d", redelivered[0].Sequence, unacked[0].Sequence)
	}
	if err := redelivered[0].ack(); err != nil {
		t.Fatalf("ack redelivered message: %v", err)
	}
	// The acknowledgement is processed asynchronously by the server, so the
	// settled count is a bounded barrier rather than an immediate read.
	ackSettled := func(ctx context.Context) error {
		info, err := consumer.Info(ctx)
		if err != nil {
			return err
		}
		if info.NumAckPending != 0 {
			return fmt.Errorf("ack pending = %d; want 0", info.NumAckPending)
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "ack settlement", ackSettled, support.FastWaitOptions()); err != nil {
		t.Fatalf("ack settlement barrier: %v", err)
	}
	next := fetch(t, js, support.EventsStream, redeliveryConfig, 1, 5*time.Second)
	if len(next) != 1 || decodeEvent(t, next[0].Data).ID != "evt-2" {
		t.Fatalf("next delivery = %+v; want evt-2", next)
	}
	if err := next[0].ack(); err != nil {
		t.Fatalf("ack next message: %v", err)
	}
}

// TestJetStreamLostAckIsNotReportedPublished proves a publish whose
// acknowledgement never returns is reported as an error, while the message the
// broker already stored is later deduplicated by the original event id.
func TestJetStreamLostAckIsNotReportedPublished(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	directory, _, _ := serviceIdentity("service-lost-ack", projectScope())

	proxy := support.NewDropProxy(t, instance.Host())
	config := instance.DriverConfig("conn-lost-ack")
	config.URL = proxy.URL()
	config.MaxReconnects = 0
	proxyBroker, err := broker.NewBroker(broker.BrokerConfig{
		Connect: func(ctx context.Context) (broker.Client, error) {
			return natsdriver.Dial(ctx, config)
		},
		Clock:               support.RealClock{},
		Principal:           directory,
		Authorizer:          directory,
		Sink:                support.NoopSink{},
		MaxReconnectBackoff: 20 * time.Millisecond,
		IdleValidation:      time.Hour,
		MaxPublishAttempts:  1,
	})
	if err != nil {
		t.Fatalf("build proxy broker: %v", err)
	}
	t.Cleanup(func() { _ = proxyBroker.Close(context.Background()) })
	ctx := instance.Shard.Context()

	if err := proxyBroker.PublishEvent(ctx, eventEnvelope(t, projectScope(), "evt-warmup")); err != nil {
		t.Fatalf("warmup publish: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 1)

	proxy.ArmDropNextServerFrame()
	publishCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	lost := eventEnvelope(t, projectScope(), "evt-lost-ack")
	if err := proxyBroker.PublishEvent(publishCtx, lost); err == nil {
		t.Fatal("publish with a lost acknowledgement reported success; want an error")
	} else {
		class, ok := fault.ClassOf(err)
		if !ok || (class != fault.DependencyUnavailable && class != fault.DeadlineExpired) {
			t.Fatalf("lost-ack error class = %v (ok=%v); want dependency_unavailable or deadline_expired (err=%v)", class, ok, err)
		}
	}
	// The server stored the message even though the client never saw the
	// acknowledgement: the effect is unknown, not proven absent.
	instance.WaitForStreamMessages(t, support.EventsStream, 2)

	direct := newBroker(t, instance, directory, "conn-lost-ack-retry")
	if err := direct.PublishEvent(ctx, lost); err != nil {
		t.Fatalf("retry of the original event id: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 2)

	js := instance.JetStream(t)
	messages := fetch(t, js, support.EventsStream, jetstream.ConsumerConfig{
		Durable:       "inspect-lost-ack",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, 3, 5*time.Second)
	if len(messages) != 2 {
		t.Fatalf("stored messages = %d; want 2 (warmup and one deduplicated lost-ack event)", len(messages))
	}
	ids := map[id.EventID]int{}
	for _, message := range messages {
		ids[decodeEvent(t, message.Data).ID]++
		_ = message.ack()
	}
	if ids["evt-lost-ack"] != 1 {
		t.Fatalf("evt-lost-ack stored %d times; want exactly once", ids["evt-lost-ack"])
	}
	support.Note(t, "lost_ack_retry_deduplicated", "true")
}

func tenancyScopeWithPlaceholderProject() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("_")}
}
