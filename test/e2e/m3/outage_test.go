package m3_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestM3NotificationOutage (m3_notification_outage) is AC-A06 through the real
// pipeline: the review gate of a run is requested while the receiver of the
// notifications is down. The pending review is a committed business record
// that nothing about the outage rolls back or hides; the delivery is retried
// with the identical payload and key until its cycle is used up, then FAILS
// visibly with its complete attempt history; the reviewer decides regardless;
// when the receiver is back only an authorized, reason-bearing replay re-sends,
// with the same idempotency key, and the history of the failed cycle stays. A
// lost response (the receiver applied it, Kodama cannot know) is retried with
// the same key and a deduplicating receiver applies it once.
func TestM3NotificationOutage(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true, ReceiverBehavior: chatopstest.Status(http.StatusServiceUnavailable)})
	def := e.GatePipeline("outage")
	e.Destination("review-channel", "gate.requested", "gate.decided")
	item := e.NewItem("Outage review", "# review during an outage")

	// ---- the gate is requested during the outage ----
	run := e.StartRun("outage-1", item, def.Id, nil)
	g := e.WaitGate(run.Id, 3*time.Minute)
	if g.Status != "pending" {
		t.Fatalf("gate %+v", g)
	}
	req := waitDelivery(t, e, "gate.requested", "failed", time.Minute)
	if req.FailedReason == nil || *req.FailedReason != "exhausted" || req.Attempts != 4 || req.LastFailure == nil || req.LastFailure.Class != "receiver_error" || req.LastFailure.HttpStatus == nil || *req.LastFailure.HttpStatus != 503 {
		t.Fatalf("the failed delivery %+v", req)
	}
	detail := e.DeliveryDetail("viewer", req.Id) // project:read is enough to inspect (AC-08: visible failure)
	if len(detail.AttemptHistory) != 4 {
		t.Fatalf("attempt history %+v", detail.AttemptHistory)
	}
	for i, a := range detail.AttemptHistory {
		want := "retry"
		if i == 3 {
			want = "failed"
		}
		if a.Number != i+1 || string(a.Result) != want || a.Replay != 0 || a.HttpStatus == nil || *a.HttpStatus != 503 || a.FinishedAt == nil {
			t.Fatalf("attempt %d: %+v", i, a)
		}
	}
	// Every attempt carried the identical payload and key.
	seen := map[string]bool{}
	for _, r := range e.Receiver.Requests() {
		if r.Header.Get("Idempotency-Key") == req.Id {
			seen[string(r.Body)] = true
		}
	}
	if len(seen) != 1 {
		t.Fatalf("%d distinct payloads for one notification", len(seen))
	}
	// The outage changed no business state and hid nothing: the review is committed, listed and decidable.
	if got := e.GetGate("reviewer", g.Id); got.Status != "pending" || got.Version != g.Version || got.Applicability.Expired {
		t.Fatalf("the pending review during the outage: %+v", got)
	}
	if e.PendingEvents(m2.TenantA) != 0 {
		t.Logf("%d events not yet published", e.PendingEvents(m2.TenantA))
	}
	if code, b := e.Decide("reviewer", g, "approve", "decided while the receiver is down", "outage-approve"); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	if f := e.World.WaitRun(run.Id, 2*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s): the outage must not stop the business flow", f.Status, f.Reason)
	}
	dec := waitDelivery(t, e, "gate.decided", "failed", time.Minute)

	// ---- the receiver recovers: nothing re-sends by itself, an authorized replay does ----
	e.Receiver.SetBehavior(chatopstest.Accept("rcpt-recovered"))
	before := e.Receiver.Count()
	time.Sleep(3 * time.Second)
	if e.Receiver.Count() != before {
		t.Fatalf("a failed delivery was sent again without a replay (%d -> %d requests)", before, e.Receiver.Count())
	}
	for _, who := range []string{"viewer", "engineer", "reviewer"} {
		if code, b := e.Replay(who, req, "receiver is back"); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("%s replayed a delivery: %d %s", who, code, b)
		}
	}
	if code, b := e.Replay("admin", req, ""); code < 400 {
		t.Fatalf("a replay without a reason: %d %s", code, b)
	}
	if code, b := e.Do("admin", "POST", e.ProjectPath()+"/chatops-deliveries/"+req.Id+"/replay", map[string]any{"reason": "no version"}, nil); code != http.StatusPreconditionRequired {
		t.Fatalf("a replay without If-Match: %d %s", code, b)
	}
	if code, b := e.Replay("admin", req, "the receiver is back"); code != http.StatusOK {
		t.Fatalf("replay: %d %s", code, b)
	}
	replayed := waitDeliveryID(t, e, req.Id, "delivered", time.Minute)
	if replayed.Replays != 1 || replayed.Attempts != 5 || replayed.Receipt == nil || *replayed.Receipt != "rcpt-recovered" {
		t.Fatalf("replayed delivery %+v", replayed)
	}
	detail = e.DeliveryDetail("viewer", req.Id)
	if len(detail.AttemptHistory) != 5 {
		t.Fatalf("history after the replay: %+v", detail.AttemptHistory)
	}
	for i, a := range detail.AttemptHistory {
		wantReplay, wantResult := 0, "retry"
		switch i {
		case 3:
			wantResult = "failed"
		case 4:
			wantReplay, wantResult = 1, "delivered"
		}
		if a.Replay != wantReplay || string(a.Result) != wantResult {
			t.Fatalf("attempt %d after the replay: %+v", i+1, a)
		}
	}
	if !e.Receiver.EffectOf(req.Id) {
		t.Fatal("the replayed notification did not reach the receiver")
	}
	// The same key, the same payload: the receiver can deduplicate.
	bodies := map[string]bool{}
	for _, r := range e.Receiver.Requests() {
		if r.Header.Get("Idempotency-Key") == req.Id {
			bodies[string(r.Body)] = true
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("the replay changed the payload (%d variants)", len(bodies))
	}
	if code, b := e.Replay("admin", replayed, "again"); code < 400 {
		t.Fatalf("a delivered notification was replayed: %d %s", code, b)
	}
	// The decision's notification is still failed until it is replayed too: failures are never silent, never automatic.
	if still := waitDeliveryID(t, e, dec.Id, "failed", 5*time.Second); still.Replays != 0 {
		t.Fatalf("%+v", still)
	}

	// ---- a lost response: the receiver applied it once, Kodama retried with the same key ----
	e.Receiver.SetBehavior(kit.LoseFirstPerKey())
	item2 := e.NewItem("Lost response review", "# lost response")
	run2 := e.StartRun("outage-2", item2, def.Id, nil)
	g2 := e.WaitGate(run2.Id, 3*time.Minute)
	n2 := e.WaitReceived("gate.requested", g2.Id, time.Minute)
	d2 := waitDeliveryID(t, e, n2.NotificationID, "delivered", time.Minute)
	if d2.Attempts != 2 || d2.Replays != 0 {
		t.Fatalf("delivery after a lost response %+v", d2)
	}
	h2 := e.DeliveryDetail("viewer", d2.Id).AttemptHistory
	if len(h2) != 2 || !h2[0].Uncertain || h2[0].Result == "delivered" || h2[1].Result != "delivered" {
		t.Fatalf("history %+v", h2)
	}
	n := 0
	for _, r := range e.Receiver.Requests() {
		if r.Header.Get("Idempotency-Key") == d2.Id {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("the receiver saw %d requests for the notification, want the lost one and its retry", n)
	}
}

func waitDelivery(t *testing.T, e *kit.Env, eventType, state string, within time.Duration) client.Delivery {
	t.Helper()
	var out client.Delivery
	m2.WaitFor(t, "a "+state+" "+eventType+" delivery", within, func() bool {
		for _, d := range e.Deliveries("admin", "") {
			if string(d.EventType) == eventType && string(d.State) == state {
				out = d
				return true
			}
		}
		return false
	})
	return out
}

func waitDeliveryID(t *testing.T, e *kit.Env, id, state string, within time.Duration) client.Delivery {
	t.Helper()
	var out client.Delivery
	m2.WaitFor(t, "delivery "+id+" to be "+state, within, func() bool {
		d, ok := e.DeliveryByID(id)
		out = d
		return ok && string(d.State) == state
	})
	return out
}
