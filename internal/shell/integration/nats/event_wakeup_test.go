package nats_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/integration/nats"
)

func TestBrokerSubscribeEventWakeup(t *testing.T) {
	client := newFakeClient()
	env := newBrokerEnv(t, brokerOptions{
		connector: &fakeConnector{clients: []nats.Client{client}},
	})
	scope := testScope()

	woken := 0
	sub, err := env.broker.SubscribeEventWakeup(context.Background(), scope, func() {
		woken++
	})
	if err != nil {
		t.Fatalf("SubscribeEventWakeup: %v", err)
	}
	defer func() { _ = sub.Close(context.Background()) }()

	wantSubject, err := nats.EventSubject(scope)
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	fakeSub := client.lastSubscription(t)
	if fakeSub.subject != wantSubject {
		t.Fatalf("subscribed subject = %q; want %q", fakeSub.subject, wantSubject)
	}
	if err := fakeSub.deliver(context.Background(), []byte(`{"id":"evt-1"}`)); err != nil {
		t.Fatalf("deliver wakeup: %v", err)
	}
	if woken != 1 {
		t.Fatalf("wakeup count = %d; want 1", woken)
	}

	if _, err := env.broker.SubscribeEventWakeup(context.Background(), scope, nil); err == nil {
		t.Fatal("expected nil wakeup callback to fail")
	} else {
		requireClass(t, err, fault.InvalidInput)
	}
}
