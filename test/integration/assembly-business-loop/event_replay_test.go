package assemblybusinessloop_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestBusinessLoopEventReplay (business_loop_event_replay) is SD-11 for the
// notification path, through the real outbox, JetStream, inbox and delivery
// worker rather than a replay double:
//
//   - the broker is unreachable while work is committed and a review is
//     requested, decided and its run completed: nothing is lost (the outbox
//     keeps every event, business progress does not wait for the bus) and the
//     event module's repair, which reads the committed outbox itself, still
//     makes exactly one notification per event; when the broker returns every
//     event is published and the consumer meets events it has already
//     processed: nothing is notified a second time, and the announced
//     versions are in causal order;
//   - a second copy of every event under a fresh broker identity (what a
//     republish or a broker redelivery looks like) and a restart of the whole
//     process create no second intent and send nothing;
//   - the consumer ends with nothing pending and nothing unacknowledged, and a
//     later event is still delivered exactly once.
//
// Covers AC-08, AC-12, WM-08, SD-04/SD-10/SD-11 and DQ-04 (the decision event
// is the one the committed gate recorded).
func TestBusinessLoopEventReplay(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true})
	def := e.GatePipeline("replay")
	kinds := []string{"work_item.transitioned", "gate.requested", "gate.decided"}
	d := e.Destination("replay", kinds...)
	durable := e.Loop().Consumers[0].Durable

	supported := func() int {
		return e.TenantCount(m2.TenantA, `SELECT count(*) FROM outbox WHERE event_type IN ('work_item.transitioned', 'gate.requested', 'gate.decided')`)
	}

	// ---- the broker goes away; the business keeps committing ----
	item := e.NewItem("Replay subject", "# replay subject")
	e.NATS.Stop()
	baseline := e.Receiver.Count()
	ready := e.Ready(item)
	active := e.MustTransition("engineer", ready, "active", nil)
	run := e.StartRun("replay-run", item, def.Id, nil)
	g := e.WaitGate(run.Id, 3*time.Minute)
	if g.Status != "pending" {
		t.Fatalf("gate %+v", g)
	}
	if code, b := e.Decide("reviewer", g, "approve", "decided while the broker is down", "replay-approve"); code != http.StatusOK {
		t.Fatalf("approve during the broker outage: %d %s", code, b)
	}
	if f := e.World.WaitRun(run.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s): the business flow must not wait for the bus", f.Status, f.Reason)
	}
	if got := e.GetItem(item.Id); got.State != "active" || got.Version != active.Version {
		t.Fatalf("item %+v", got)
	}
	pending := e.PendingEvents(m2.TenantA)
	if pending == 0 {
		t.Fatal("every event reached the broker while it was unreachable")
	}
	want := supported()
	if want < 4 { // ready, active, gate requested, gate decided
		t.Fatalf("%d notifiable events committed", want)
	}
	// The event module's repair reads the committed outbox itself, so the notifications do not wait
	// for the broker either: one delivery per event, sent once, while it is still down.
	m2.WaitFor(t, "one delivery per event during the outage", time.Minute, func() bool { return len(e.Deliveries("admin", "destinationId="+d.Id)) == want })
	ds := e.WaitSettled(time.Minute)
	if len(ds) != want {
		t.Fatalf("%d deliveries for %d events", len(ds), want)
	}
	if still := e.PendingEvents(m2.TenantA); still == 0 {
		t.Fatal("the outbox was drained with no broker")
	}
	events := map[string]bool{}
	for _, dl := range ds {
		if dl.State != "delivered" || dl.Attempts != 1 || dl.Replays != 0 || events[dl.EventId] {
			t.Fatalf("delivery %+v (event seen before: %v)", dl, events[dl.EventId])
		}
		events[dl.EventId] = true
	}
	if e.Receiver.Effects() != want || e.Receiver.Count()-baseline != want {
		t.Fatalf("receiver: %d effects, %d requests, want %d each", e.Receiver.Effects(), e.Receiver.Count()-baseline, want)
	}

	// ---- the broker returns: the outbox publishes what was already notified, and nothing is notified twice ----
	e.NATS.Start()
	m2.WaitFor(t, "the outbox to drain", time.Minute, func() bool { return e.PendingEvents(m2.TenantA) == 0 })
	m2.WaitFor(t, "the ChatOps consumer to receive the published events", 30*time.Second, func() bool {
		st := e.Consumer(durable)
		return st.Delivered > 0 && st.Pending == 0 && st.AckPending == 0
	})
	time.Sleep(2 * time.Second)
	if got := len(e.Deliveries("admin", "destinationId="+d.Id)); got != want || e.Receiver.Count()-baseline != want || e.Receiver.Effects() != want {
		t.Fatalf("publishing %d already-notified events created %d deliveries, %d requests, %d effects", pending, got, e.Receiver.Count()-baseline, e.Receiver.Effects())
	}
	// Causal order per resource: a gate is announced as requested before it is announced as decided, at increasing versions.
	req := e.ReceivedFor("gate.requested", g.Id)
	dec := e.ReceivedFor("gate.decided", g.Id)
	if len(req) != 1 || len(dec) != 1 || dec[0].Resource.Version <= req[0].Resource.Version || dec[0].Event.OccurredAt < req[0].Event.OccurredAt {
		t.Fatalf("gate notifications requested %+v decided %+v", req, dec)
	}
	decided := e.GetGate("reviewer", g.Id)
	if dec[0].Resource.Version != int64(decided.Version) || req[0].Attributes["subjectDigest"] != string(g.SubjectDigest) {
		t.Fatalf("the announced gate version %d / digest %s are not the committed ones (%d / %s)", dec[0].Resource.Version, req[0].Attributes["subjectDigest"], decided.Version, g.SubjectDigest)
	}
	tr := e.ReceivedFor("work_item.transitioned", item.Id)
	// Arrival order across a worker pool is not a promise; the versions announced are the committed ones.
	if len(tr) != 2 || !sameVersions(tr, int64(ready.Version), int64(active.Version)) {
		t.Fatalf("work transitions announced %+v, want versions %d and %d", tr, ready.Version, active.Version)
	}
	settle := func() {
		m2.WaitFor(t, "the ChatOps consumer to settle", 30*time.Second, func() bool {
			st := e.Consumer(durable)
			return st.Pending == 0 && st.AckPending == 0 && st.Delivered > 0
		})
	}
	settle()

	// ---- duplicate copies under fresh broker identities: no second intent, no second send ----
	n := e.RepublishEvents(item.Id) + e.RepublishEvents(g.Id)
	if n < 4 {
		t.Fatalf("republished %d messages", n)
	}
	time.Sleep(3 * time.Second)
	settle()
	if got := len(e.Deliveries("admin", "destinationId="+d.Id)); got != want {
		t.Fatalf("%d deliveries after %d duplicate messages, want %d", got, n, want)
	}
	if e.Receiver.Count()-baseline != want || e.Receiver.Effects() != want {
		t.Fatalf("a duplicate message was notified again: %d requests, %d effects", e.Receiver.Count()-baseline, e.Receiver.Effects())
	}

	// ---- a restart of the whole process neither loses nor repeats anything ----
	e.Restart()
	time.Sleep(2 * time.Second)
	settle()
	if got := len(e.Deliveries("admin", "destinationId="+d.Id)); got != want || e.Receiver.Count()-baseline != want {
		t.Fatalf("after the restart: %d deliveries, %d requests, want %d", got, e.Receiver.Count()-baseline, want)
	}
	// ...and the pipeline is alive: the next event is delivered exactly once.
	cancelled := e.MustTransition("engineer", e.GetItem(item.Id), "cancelled", map[string]any{"reason": "replay scenario done"})
	m2.WaitFor(t, "the next event to be delivered", time.Minute, func() bool { return len(e.Deliveries("admin", "destinationId="+d.Id)) == want+1 })
	e.WaitSettled(time.Minute)
	time.Sleep(time.Second)
	last := e.ReceivedFor("work_item.transitioned", item.Id)
	if len(last) != 3 || !sameVersions(last, int64(ready.Version), int64(active.Version), int64(cancelled.Version)) || e.Receiver.Count()-baseline != want+1 || e.Receiver.Effects() != want+1 {
		t.Fatalf("the event after the restart: %+v, %d requests, %d effects", last, e.Receiver.Count()-baseline, e.Receiver.Effects())
	}
}

func sameVersions(ps []kit.Payload, want ...int64) bool {
	got := map[int64]bool{}
	for _, p := range ps {
		got[p.Resource.Version] = true
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return len(got) == len(want)
}
