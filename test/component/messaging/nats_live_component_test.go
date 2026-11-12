//go:build component

package messaging_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

func liveScope(project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID(project)}
}

func invalidationHandler(deliveries chan<- token.Invalidation) broker.InvalidationHandler {
	return func(_ context.Context, invalidation token.Invalidation) error {
		deliveries <- invalidation
		return nil
	}
}

// TestLiveCredentialRevocation proves a revoked credential stops delivery and
// closes the existing subscription within a bounded server-observed barrier.
func TestLiveCredentialRevocation(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("revoke")
	directory, credential, invalidation := serviceIdentity("service-live-revoke", scope)
	adapter := newBroker(t, instance, directory, "conn-live-revoke")
	ctx := instance.Shard.Context()

	subject, err := broker.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("invalidation subject: %v", err)
	}
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{
		Credential: credential,
		Scope:      scope,
		Handler:    invalidationHandler(deliveries),
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-live-revoke", string(subject))

	directory.Revoke(credential)
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish revocation: %v", err)
	}
	instance.WaitForNoSubscription(t, "conn-live-revoke", string(subject))
	requireNoReceive(t, deliveries, 500*time.Millisecond, "delivery to a revoked credential")
	support.Note(t, "revoked_subscription_closed", "true")
}

// TestLiveCredentialRotation proves rotation invalidates the old credential's
// open subscription while the new credential can subscribe.
func TestLiveCredentialRotation(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("rotate")
	directory, credential, invalidation := serviceIdentity("service-live-rotate", scope, token.InvalidationRotated)
	if invalidation.Reason != token.InvalidationRotated {
		t.Fatalf("rotation invalidation reason = %q; want %q", invalidation.Reason, token.InvalidationRotated)
	}
	adapter := newBroker(t, instance, directory, "conn-live-rotate")
	ctx := instance.Shard.Context()

	subject, _ := broker.InvalidationSubject(scope)
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe old credential: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-live-rotate", string(subject))

	directory.Rotate(credential, "credential-rotated")
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish rotation: %v", err)
	}
	instance.WaitForNoSubscription(t, "conn-live-rotate", string(subject))
	requireNoReceive(t, deliveries, 500*time.Millisecond, "delivery to a rotated credential")

	rotatedDeliveries := make(chan token.Invalidation, 8)
	rotated, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: "credential-rotated", Scope: scope, Handler: invalidationHandler(rotatedDeliveries)})
	if err != nil {
		t.Fatalf("subscribe rotated credential: %v", err)
	}
	instance.WaitForSubscription(t, "conn-live-rotate", string(subject))
	if err := rotated.Close(ctx); err != nil {
		t.Fatalf("close rotated subscription: %v", err)
	}
	support.Note(t, "rotated_old_closed_new_accepted", "true")
}

// TestLivePrincipalDisablement proves disabling a principal closes its open
// subscription on a disabled invalidation and refuses a new subscription.
func TestLivePrincipalDisablement(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("disable")
	directory, credential, invalidation := serviceIdentity("service-live-disable", scope, token.InvalidationDisabled)
	if invalidation.Reason != token.InvalidationDisabled {
		t.Fatalf("disablement invalidation reason = %q; want %q", invalidation.Reason, token.InvalidationDisabled)
	}
	adapter := newBroker(t, instance, directory, "conn-live-disable")
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
	instance.WaitForSubscription(t, "conn-live-disable", string(subject))

	directory.SetEnabled(credential, false)
	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish disablement: %v", err)
	}
	instance.WaitForNoSubscription(t, "conn-live-disable", string(subject))
	requireNoReceive(t, deliveries, 500*time.Millisecond, "delivery to a disabled principal")

	if _, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)}); err == nil {
		t.Fatal("disabled principal opened a subscription; want refusal")
	} else {
		requireClass(t, err, fault.Unauthenticated)
	}
	support.Note(t, "disabled_principal_subscription_closed", "true")
}

// TestLiveCredentialExpiry proves an expired credential loses its open
// subscription through periodic idle validation without publishing a Revoked
// invalidation message, and cannot open a new subscription.
func TestLiveCredentialExpiry(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("expire")
	directory, credential, _ := serviceIdentity("service-live-expire", scope)
	adapter := newBrokerWithIdleValidation(t, instance, directory, "conn-live-expire", 50*time.Millisecond)
	ctx := instance.Shard.Context()

	subject, _ := broker.InvalidationSubject(scope)
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-live-expire", string(subject))

	directory.Expire(credential)
	instance.WaitForNoSubscription(t, "conn-live-expire", string(subject))
	requireNoReceive(t, deliveries, 200*time.Millisecond, "delivery to an expired credential")

	if _, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)}); err == nil {
		t.Fatal("expired credential opened a subscription; want refusal")
	} else {
		requireClass(t, err, fault.Unauthenticated)
	}
	support.Note(t, "expired_credential_refused", "true")
}

// TestLiveSelfInvalidationDeliveredOnce proves an invalidation naming the
// subscriber's own principal is delivered once and then closes the
// subscription.
func TestLiveSelfInvalidationDeliveredOnce(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("self")
	directory, credential, invalidation := serviceIdentity("service-live-self", scope)
	adapter := newBroker(t, instance, directory, "conn-live-self")
	ctx := instance.Shard.Context()

	subject, _ := broker.InvalidationSubject(scope)
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-live-self", string(subject))

	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish self invalidation: %v", err)
	}
	delivered := receive(t, deliveries, 10*time.Second, "self invalidation")
	if delivered.PrincipalID != invalidation.PrincipalID {
		t.Fatalf("delivered principal = %q; want %q", delivered.PrincipalID, invalidation.PrincipalID)
	}
	instance.WaitForNoSubscription(t, "conn-live-self", string(subject))
	requireNoReceive(t, deliveries, 500*time.Millisecond, "second delivery of a self invalidation")
}

// TestLiveCrossScopeInvalidationRejected proves a message naming another scope
// is refused without closing the subscription, while a valid self
// invalidation still closes it.
func TestLiveCrossScopeInvalidationRejected(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("cross")
	directory, credential, invalidation := serviceIdentity("service-live-cross", scope)
	adapter := newBroker(t, instance, directory, "conn-live-cross")
	ctx := instance.Shard.Context()

	subject, _ := broker.InvalidationSubject(scope)
	deliveries := make(chan token.Invalidation, 8)
	subscription, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(deliveries)})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	instance.WaitForSubscription(t, "conn-live-cross", string(subject))

	foreign := support.ServiceInvalidation(liveScope("cross-foreign"), id.PrincipalID("service-foreign"), "service-foreign", token.InvalidationRevoked, testNow())
	payload, err := json.Marshal(foreign)
	if err != nil {
		t.Fatalf("encode foreign invalidation: %v", err)
	}
	raw := instance.Raw(t)
	if err := raw.Publish(string(subject), payload); err != nil {
		t.Fatalf("publish foreign invalidation: %v", err)
	}
	if err := raw.Flush(); err != nil {
		t.Fatalf("flush foreign invalidation: %v", err)
	}
	requireNoReceive(t, deliveries, 500*time.Millisecond, "cross-scope invalidation delivery")
	instance.WaitForSubscription(t, "conn-live-cross", string(subject))

	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish valid invalidation: %v", err)
	}
	receive(t, deliveries, 10*time.Second, "valid invalidation after cross-scope refusal")
	instance.WaitForNoSubscription(t, "conn-live-cross", string(subject))
	support.Note(t, "cross_scope_invalidation_refused", "true")
}

// TestSubscribeRequiresCurrentAuthority proves subscribe-time authority is
// rechecked: a removed grant and a mismatched scope are refused.
func TestSubscribeRequiresCurrentAuthority(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("authority")
	directory, credential, _ := serviceIdentity("service-live-authority", scope)
	adapter := newBroker(t, instance, directory, "conn-live-authority")
	ctx := instance.Shard.Context()

	directory.Grant(id.PrincipalID("service-live-authority"), broker.OperationEventSubscribe, false)
	if _, err := adapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: credential, Scope: scope, Handler: invalidationHandler(make(chan token.Invalidation, 1))}); err == nil {
		t.Fatal("subscription without event.subscribe accepted; want refusal")
	} else {
		requireClass(t, err, fault.Forbidden)
	}

	otherDirectory, otherCredential, _ := serviceIdentity("service-live-authority-other", liveScope("authority-other"))
	otherAdapter := newBroker(t, instance, otherDirectory, "conn-live-authority-other")
	if _, err := otherAdapter.Subscribe(ctx, broker.SubscriptionRequest{Credential: otherCredential, Scope: scope, Handler: invalidationHandler(make(chan token.Invalidation, 1))}); err == nil {
		t.Fatal("credential for another scope opened a subscription; want refusal")
	} else {
		requireClass(t, err, fault.Forbidden)
	}
}

// TestLiveLogSubscriptionScopeAndInvalidation proves the Core NATS live log
// stream is scope-qualified and fails closed on an observed invalidation.
func TestLiveLogSubscriptionScopeAndInvalidation(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATS(t)
	instance.EnsureStreams(t)
	scope := liveScope("logs")
	otherScope := liveScope("logs-other")
	directory, _, invalidation := serviceIdentity("service-live-logs", scope)
	adapter := newBroker(t, instance, directory, "conn-live-logs")
	ctx := instance.Shard.Context()

	live, err := adapter.SubscribeLive(ctx, scope, id.ExecutionID("exec-1"))
	if err != nil {
		t.Fatalf("subscribe live: %v", err)
	}
	t.Cleanup(func() { _ = live.Close(context.Background()) })

	logSubject, err := broker.LogSubject(scope, id.ExecutionID("exec-1"))
	if err != nil {
		t.Fatalf("log subject: %v", err)
	}
	if want := "kodama.v1.tenant-a.logs.logs.exec-1"; string(logSubject) != want {
		t.Fatalf("log subject = %q; want %q", logSubject, want)
	}
	raw := instance.Raw(t)
	rawChunks := make(chan *natsgo.Msg, 8)
	rawSubscription, err := raw.ChanSubscribe(string(logSubject), rawChunks)
	if err != nil {
		t.Fatalf("raw subscribe to log subject: %v", err)
	}
	defer func() { _ = rawSubscription.Unsubscribe() }()
	if err := raw.Flush(); err != nil {
		t.Fatalf("flush raw log subscription: %v", err)
	}

	chunk := log.Chunk{
		ExecutionID: id.ExecutionID("exec-1"),
		ActionID:    id.ActionID("action-1"),
		Stream:      log.StreamStdout,
		Offset:      0,
		Sequence:    1,
		Data:        []byte("hello"),
	}
	if err := adapter.PublishLive(ctx, scope, chunk); err != nil {
		t.Fatalf("publish live chunk: %v", err)
	}
	rawChunk := receive(t, rawChunks, 10*time.Second, "raw live log chunk")
	if rawChunk.Subject != string(logSubject) {
		t.Fatalf("live log subject on the wire = %q; want %q", rawChunk.Subject, logSubject)
	}
	nextCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	delivered, err := live.Next(nextCtx)
	cancel()
	if err != nil {
		t.Fatalf("next live chunk: %v", err)
	}
	if string(delivered.Data) != "hello" {
		t.Fatalf("live chunk data = %q; want hello", delivered.Data)
	}

	if err := adapter.PublishLive(ctx, otherScope, chunk); err != nil {
		t.Fatalf("publish other-scope chunk: %v", err)
	}
	quietCtx, quietCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	_, err = live.Next(quietCtx)
	quietCancel()
	if err == nil {
		t.Fatal("live subscription received another scope's chunk")
	}

	if err := adapter.Invalidate(ctx, invalidation); err != nil {
		t.Fatalf("publish invalidation: %v", err)
	}
	closedCtx, closedCancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = live.Next(closedCtx)
	closedCancel()
	if err == nil {
		t.Fatal("live subscription stayed open after an invalidation")
	} else {
		requireClass(t, err, fault.DependencyUnavailable)
	}
	if err := adapter.PublishLive(ctx, scope, chunk); err != nil {
		t.Fatalf("publish after close: %v", err)
	}
	afterCtx, afterCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	if _, err := live.Next(afterCtx); err == nil {
		t.Fatal("closed live subscription delivered another chunk")
	}
	afterCancel()

	if err := adapter.PublishLive(ctx, tenancy.Scope{}, chunk); err == nil {
		t.Fatal("scope-less live publication accepted; want refusal")
	}
	support.Note(t, "live_log_scope_and_invalidation", "true")
}
