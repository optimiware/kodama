//go:build component

// Regression test for subscription resume after a broker outage
// (internal/shell/integration/nats).
//
// A publication that fails while the context is still alive (for example a
// bounded reconnect budget expired during a broker outage) discards the
// suspect client and tries to dial a replacement immediately. When that dial
// also fails, the live subscriptions must not be left on the dead client: a
// later successful ensureClient re-establishes them. This test keeps the
// bounded reconnect configuration that exposes a missing resume, proves the
// subscription resumes once the broker returns, and proves a revocation
// published after the resume is delivered exactly once and then closes the
// stale subscription.

package messaging_test

import (
	"context"
	"testing"
	"time"

	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/token"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

func TestOutageSubscriberResumeAfterFailedPublication(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("outage-resume-failed-publish")
	directory, credential, invalidation := serviceIdentity("service-outage-resume-failed", scope)
	driverConfig := instance.DriverConfig("conn-outage-resume-failed")
	// A bounded reconnect budget expires during the outage, so the client
	// fails the publication promptly instead of waiting for the broker. This
	// is the configuration that exposes a missing resume.
	driverConfig.MaxReconnects = 1
	driverConfig.ReconnectWait = 50 * time.Millisecond
	adapter, err := broker.NewBroker(broker.BrokerConfig{
		Connect: func(ctx context.Context) (broker.Client, error) {
			return natsdriver.Dial(ctx, driverConfig)
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
		t.Fatalf("build broker: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	ctx := instance.Shard.Context()

	subject, err := broker.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("invalidation subject: %v", err)
	}
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-outage-resume-failed", string(subject))

	instance.Stop(t)
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = adapter.PublishEvent(publishCtx, eventEnvelope(t, scope, "evt-outage-defect"))
	cancel()
	if err == nil {
		t.Fatal("publish during the outage reported success; want a bounded failure")
	}

	instance.Start(t)
	if err := adapter.PublishEvent(ctx, eventEnvelope(t, scope, "evt-after-outage")); err != nil {
		t.Fatalf("publish after restart: %v", err)
	}
	// The successful ensureClient after the outage must have resumed the
	// subscription that was left on the discarded client.
	instance.WaitForSubscription(t, "conn-outage-resume-failed", string(subject))

	// A revocation published after the resume is delivered once, then closes
	// the now-stale subscription; no second delivery is accepted.
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish revocation after restart: %v", err)
	}
	delivered := receive(t, deliveries, 15*time.Second, "revocation after restart")
	if delivered.PrincipalID != invalidation.PrincipalID || delivered.Reason != token.InvalidationRevoked {
		t.Fatalf("delivered invalidation = %+v; want principal %q reason revoked", delivered, invalidation.PrincipalID)
	}
	instance.WaitForNoSubscription(t, "conn-outage-resume-failed", string(subject))
	requireNoReceive(t, deliveries, 500*time.Millisecond, "second delivery after the stale subscription closed")
	support.Note(t, "resubscribe_after_failed_outage_publication", "true")
}
