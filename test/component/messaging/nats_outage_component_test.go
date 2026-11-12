//go:build component

package messaging_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/token"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestOutagePublicationBacklogCatchUp proves a broker outage turns publication
// into a bounded failure, and the drained backlog keeps the original event ids
// so the broker stores no duplicate.
func TestOutagePublicationBacklogCatchUp(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	directory, _, _ := serviceIdentity("service-outage", projectScope())
	adapter := newBroker(t, instance, directory, "conn-outage-publisher")
	ctx := instance.Shard.Context()

	if err := adapter.PublishEvent(ctx, eventEnvelope(t, projectScope(), "evt-1")); err != nil {
		t.Fatalf("publish evt-1: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 1)

	instance.Stop(t)
	for _, eventID := range []id.EventID{"evt-2", "evt-3"} {
		publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := adapter.PublishEvent(publishCtx, eventEnvelope(t, projectScope(), eventID))
		cancel()
		if err == nil {
			t.Fatalf("publish %s during the outage reported success; want a bounded failure", eventID)
		}
		class, ok := fault.ClassOf(err)
		if !ok || (class != fault.DependencyUnavailable && class != fault.DeadlineExpired) {
			t.Fatalf("outage publish %s class = %v (ok=%v); want dependency_unavailable or deadline_expired (err=%v)", eventID, class, ok, err)
		}
	}

	instance.Start(t)
	for _, eventID := range []id.EventID{"evt-2", "evt-3"} {
		if err := adapter.PublishEvent(ctx, eventEnvelope(t, projectScope(), eventID)); err != nil {
			t.Fatalf("backlog publish %s after restart: %v", eventID, err)
		}
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 3)

	js := instance.JetStream(t)
	messages := fetch(t, js, support.EventsStream, jetstream.ConsumerConfig{
		Durable:       "inspect-outage",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, 4, 5*time.Second)
	if len(messages) != 3 {
		t.Fatalf("stored messages after catch-up = %d; want 3", len(messages))
	}
	seen := map[id.EventID]int{}
	for _, message := range messages {
		seen[decodeEvent(t, message.Data).ID]++
		_ = message.ack()
	}
	for _, eventID := range []id.EventID{"evt-1", "evt-2", "evt-3"} {
		if seen[eventID] != 1 {
			t.Fatalf("event %s stored %d times; want exactly once", eventID, seen[eventID])
		}
	}
	support.Note(t, "outage_backlog_original_ids", "true")
}

// TestOutageSubscriberResume proves a live invalidation subscription resumes
// after the broker restarts without a failed publication discarding its
// client.
func TestOutageSubscriberResume(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("outage-resume")
	directory, credential, invalidation := serviceIdentity("service-outage-resume", scope)
	adapter := newBroker(t, instance, directory, "conn-outage-resume")
	ctx := instance.Shard.Context()

	subject, _ := broker.InvalidationSubject(scope)
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-outage-resume", string(subject))

	instance.Restart(t)
	instance.WaitForSubscription(t, "conn-outage-resume", string(subject))
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish invalidation after restart: %v", err)
	}
	delivered := receive(t, deliveries, 15*time.Second, "invalidation after broker restart")
	if delivered.PrincipalID != invalidation.PrincipalID {
		t.Fatalf("delivered principal = %q; want %q", delivered.PrincipalID, invalidation.PrincipalID)
	}
	support.Note(t, "subscriber_resumed_after_restart", "true")
}
