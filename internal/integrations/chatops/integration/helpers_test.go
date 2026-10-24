// Package integration_test holds the ChatOps acceptance tests over real
// PostgreSQL and NATS JetStream: the real outbox -> broker -> inbox path, the
// real stores and migrations, the real outbound client and an isolated local
// HTTPS receiver. No S3 or Git is used. They prove the component's durable
// delivery, retry and deduplication behavior; they do not qualify production
// wiring (T-075), a real gate target (T-046) or restore procedures.
//
// Resource prefix t041.
package integration_test

import (
	"context"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
)

var scopeA = chatopstest.ScopeA

// stack is one environment with the bus, an administrator, a viewer, a
// receiver and a destination pointing at it.
type stack struct {
	*chatopstest.Env
	Bus    *chatopstest.Bus
	Actors chatopstest.Callers
	RC     *chatopstest.Receiver
	Dest   chatops.Destination
}

func newStack(t *testing.T, behavior chatopstest.Behavior) *stack {
	t.Helper()
	return newStackWith(t, behavior, chatopstest.Options{})
}

// newStackWith is newStack with fixture options (for example event retention).
func newStackWith(t *testing.T, behavior chatopstest.Behavior, o chatopstest.Options) *stack {
	t.Helper()
	e := chatopstest.New(t, o)
	s := &stack{Env: e, Bus: e.NewBus(t)}
	e.SeedProject(scopeA)
	s.Actors = e.Actors(scopeA, "int")
	s.RC = chatopstest.NewReceiver(t, behavior)
	e.Allow(s.RC.AddrPort())
	s.Dest = e.Destination(s.Actors.Admin, scopeA, "ops", s.RC.URL())
	return s
}

// attempts renders the attempt history of a delivery: number:result:class:uncertain:status.
func (s *stack) attempts(id chatops.DeliveryID) []chatops.Attempt {
	s.T.Helper()
	view, err := s.Svc.GetDelivery(context.Background(), s.Actors.Viewer, scopeA, id)
	if err != nil {
		s.T.Fatalf("get delivery: %v", err)
	}
	return view.Attempts
}

func (s *stack) deliveryID(eventID string) chatops.DeliveryID {
	return chatops.DeliveryIDFor(scopeA, s.Dest.ID, eventID)
}

// drain runs the worker until cond holds.
func drain(t *testing.T, w *chatops.Worker, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("worker pass: %v", err)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func process(t testing.TB, s *stack, env eventsv1.Envelope) events.Outcome {
	t.Helper()
	payload, err := eventsv1.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Events.Process(context.Background(), events.Delivery{Tenant: env.TenantID, Consumer: chatops.ConsumerName, Payload: payload})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	return out
}
