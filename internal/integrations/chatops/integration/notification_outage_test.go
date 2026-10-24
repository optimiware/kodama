package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
)

// Test_notification_outage: the real outbox -> JetStream -> inbox -> delivery path
// survives a receiver outage and a worker restart. The source review-event record
// stays committed, the retry history persists, and recovery drains the due work;
// an exhausted delivery stays visible and is replayed manually. Real PostgreSQL and
// NATS; isolated HTTP receiver; no S3 or Git. AC-08 (AC-A06).
func Test_notification_outage(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, chatopstest.Status(503)) // the receiver is down from the start
	client := s.Client(s.RC)
	w1 := s.Worker(client)

	review := chatopstest.GateRequested(scopeA, "evt_review_1", "gate_1", 1, 'a')
	s.Append(review)
	s.Bus.Pump(t, 15*time.Second, "the review notification intent", func() bool { return s.Deliveries(s.Dest.ID) == 1 })

	t.Run("the review record and its event stay committed whatever the receiver does", func(t *testing.T) {
		if n := s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_id = 'evt_review_1' AND state = 'published'`); n != 1 {
			t.Fatalf("published outbox rows = %d", n)
		}
		if n := s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_observations WHERE event_id = 'evt_review_1'`); n != 1 {
			t.Fatalf("observations = %d", n)
		}
		if n := s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_review_1' AND state = 'completed'`); n != 1 {
			t.Fatalf("completed inbox entries = %d", n)
		}
	})

	id := s.deliveryID("evt_review_1")
	t.Run("a receiver outage retries with a persisted history and never rolls anything back", func(t *testing.T) {
		drain(t, w1, 10*time.Second, "two failed attempts", func() bool { return s.Delivery(scopeA, id).Attempts >= 2 })
		d := s.Delivery(scopeA, id)
		if d.State != chatops.StatePending || d.LastClass != chatops.ClassReceiverError || d.LastStatus != 503 || d.CycleAttempts >= chatopstestRetries() {
			t.Fatalf("delivery during the outage = %+v", d)
		}
		hist := s.attempts(id)
		for i, a := range hist {
			if a.Number != i+1 || a.Result != chatops.ResultRetry || a.Class != chatops.ClassReceiverError || a.HTTPStatus != 503 || a.FinishedAt.IsZero() {
				t.Fatalf("attempt %d = %+v", i+1, a)
			}
		}
		// Business continues during the outage: more events are committed,
		// published, consumed and become durable intents.
		for i, evID := range []string{"evt_during_1", "evt_during_2"} {
			s.Append(chatopstest.RunState(scopeA, evID, "run_1", int64(i+1)))
		}
		s.Bus.Pump(t, 15*time.Second, "intents created during the outage", func() bool { return s.Deliveries(s.Dest.ID) == 3 })
		if got := s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`); got != 0 {
			t.Fatalf("%d events are unpublished", got)
		}
	})

	t.Run("a restarted worker keeps the history and recovery drains the due work", func(t *testing.T) {
		before := s.attempts(id)
		// The first worker process is gone; a new one starts over the same
		// database while the receiver recovers.
		s.RC.SetBehavior(chatopstest.Accept("rcpt-ok"))
		w2 := s.Worker(client)
		drain(t, w2, 15*time.Second, "all three notifications delivered", func() bool {
			return s.Count(`SELECT count(*) FROM chatops_deliveries WHERE state = 'delivered'`) == 3
		})
		hist := s.attempts(id)
		if len(hist) <= len(before) {
			t.Fatalf("no attempt was added after recovery: before %d after %d", len(before), len(hist))
		}
		for i, a := range hist[:len(before)] {
			if a != before[i] {
				t.Fatalf("history entry %d was rewritten: %+v -> %+v", i+1, before[i], a)
			}
		}
		last := hist[len(hist)-1]
		if last.Result != chatops.ResultDelivered || last.HTTPStatus != 200 || last.Receipt != "rcpt-ok" || last.Number != len(hist) {
			t.Fatalf("final attempt = %+v", last)
		}
		d := s.Delivery(scopeA, id)
		if d.State != chatops.StateDelivered || d.Receipt != "rcpt-ok" || d.Attempts != len(hist) || d.LastClass != "" {
			t.Fatalf("delivery = %+v", d)
		}
		// Every attempt carried the identical key and payload; the receiver applied one effect per notification.
		key := string(id)
		var bodies []string
		for _, q := range s.RC.Requests() {
			if q.Header.Get("Idempotency-Key") == key {
				bodies = append(bodies, string(q.Body))
			}
		}
		if len(bodies) != len(hist) {
			t.Fatalf("receiver saw %d requests for the notification, history has %d attempts", len(bodies), len(hist))
		}
		for _, b := range bodies[1:] {
			if b != bodies[0] {
				t.Fatal("a retry changed the payload")
			}
		}
		if s.RC.Effects() != 3 {
			t.Fatalf("effects = %d, want 3", s.RC.Effects())
		}
		// The payload carries the review link and the event identity.
		if !strings.Contains(bodies[0], `"url":"`+chatopstest.Origin+`/api/v1/tenants/t041_acme/projects/p041_inventory/gates/gate_1"`) || !strings.Contains(bodies[0], `"id":"evt_review_1"`) {
			t.Fatalf("payload = %s", bodies[0])
		}
	})

	t.Run("an exhausted delivery is visible and can be replayed manually", func(t *testing.T) {
		down := newStack(t, chatopstest.Status(503))
		w := down.Worker(down.Client(down.RC))
		ev := chatopstest.GateRequested(scopeA, "evt_down_1", "gate_2", 1, 'b')
		down.Append(ev)
		down.Bus.Pump(t, 15*time.Second, "the intent", func() bool { return down.Deliveries(down.Dest.ID) == 1 })
		did := down.deliveryID("evt_down_1")
		drain(t, w, 15*time.Second, "exhaustion", func() bool { return down.Delivery(scopeA, did).State == chatops.StateFailed })

		view, err := down.Svc.GetDelivery(ctx, down.Actors.Viewer, scopeA, did)
		if err != nil {
			t.Fatal(err)
		}
		d := view.Delivery
		if d.State != chatops.StateFailed || d.FailedReason != chatops.FailedExhausted || d.LastClass != chatops.ClassReceiverError || d.Attempts != chatopstestRetries() || len(view.Attempts) != d.Attempts {
			t.Fatalf("visible failure = %+v", d)
		}
		if view.DestinationName != "ops" {
			t.Fatalf("destination name = %q", view.DestinationName)
		}
		// The inspection carries no URL, credential reference, payload or receiver diagnostic.
		if len(d.Payload) != 0 {
			t.Fatal("the sanitized view carries the payload")
		}
		list, err := down.Svc.ListDeliveries(ctx, down.Actors.Viewer, scopeA, chatops.DeliveryFilter{State: chatops.StateFailed}, chatops.DeliveryPosition{}, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("failed list = %d %v", len(list), err)
		}
		// No further automatic attempts happen while it stays failed.
		before := down.RC.Count()
		for i := 0; i < 3; i++ {
			if _, err := w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if down.RC.Count() != before {
			t.Fatal("a failed delivery was retried automatically")
		}
		// The receiver recovers; a reason-bearing replay by an administrator resends it.
		down.RC.SetBehavior(chatopstest.Accept("rcpt-replay"))
		if _, err := down.Svc.ReplayDelivery(ctx, down.Actors.Viewer, scopeA, did, d.Version, "receiver is back"); err == nil {
			t.Fatal("a viewer replayed a delivery")
		}
		if _, err := down.Svc.ReplayDelivery(ctx, down.Actors.Admin, scopeA, did, d.Version, "receiver is back"); err != nil {
			t.Fatal(err)
		}
		drain(t, w, 10*time.Second, "the replayed delivery", func() bool { return down.Delivery(scopeA, did).State == chatops.StateDelivered })
		final := down.attempts(did)
		if len(final) != d.Attempts+1 || final[len(final)-1].Result != chatops.ResultDelivered || final[len(final)-1].Replay != 1 {
			t.Fatalf("history after replay = %+v", final)
		}
		for i, a := range final[:d.Attempts] {
			if a.Replay != 0 || a.Result == chatops.ResultDelivered || a.Number != i+1 {
				t.Fatalf("history was not preserved across the replay: %+v", a)
			}
		}
	})
}

// chatopstestRetries is the fast retry policy's attempts per cycle.
func chatopstestRetries() int { return chatopstest.FastRetry().MaxAttempts }
