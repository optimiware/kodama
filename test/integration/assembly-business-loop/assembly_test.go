package assemblybusinessloop_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestBusinessLoopAssembly (business_loop_assembly) is the join of the
// business-loop modules (dependency graph, ChatOps, QA mapping, flow summaries)
// on the assembled core over real PostgreSQL and JetStream, not a wiring double:
//
//   - what the installation statement selects is what runs: the ChatOps worker
//     is one of the process's loops, its consumer exists on the broker and
//     advances, the dependency and ChatOps routes answer, the QA mapping exists
//     only with its principal, and what is deliberately not wired is named;
//   - without the statement nothing is notified and nothing is mapped (fail
//     closed, not allow-all), while the dependency graph is still selected;
//   - a malformed statement stops startup instead of being guessed;
//   - the notification intent is durable BEFORE any external call (a receiver
//     that never answers still leaves a visible delivery) and a send that
//     outlives its attempt deadline is retried with the same key;
//   - one committed work mutation yields exactly one event, one intent per
//     destination, and nothing for a mutation that was refused.
//
// Covers WM-01/WM-05/WM-12, AC-08/AC-09/AC-12, DQ-06/DQ-10 and SD-11.
func TestBusinessLoopAssembly(t *testing.T) {
	t.Run("the statement selects the slice", func(t *testing.T) {
		e := kit.New(t, kit.Options{})
		slice := e.Loop()
		for _, r := range []string{"work-dependencies", "chatops"} {
			if !slices.Contains(slice.Routes, r) {
				t.Fatalf("routes %v lack %s", slice.Routes, r)
			}
		}
		if slice.QA == nil || slice.Dependencies == nil || slice.Flow == nil || slice.ChatOps == nil {
			t.Fatalf("services: qa %v dependencies %v flow %v chatops %v", slice.QA != nil, slice.Dependencies != nil, slice.Flow != nil, slice.ChatOps != nil)
		}
		if !slices.Contains(e.Server().Core().Workers(), "chatops-delivery") {
			t.Fatalf("workers %v lack the ChatOps delivery loop", e.Server().Core().Workers())
		}
		if len(slice.Consumers) != 1 || slice.Consumers[0].Durable == "" {
			t.Fatalf("consumers %+v", slice.Consumers)
		}
		// Named, not silent: what the business loop leaves unwired.
		for _, want := range []string{"flow-summary-list", "exploratory-qa"} {
			if !slices.Contains(slice.Disabled, want) {
				t.Fatalf("disabled %v lacks %s", slice.Disabled, want)
			}
		}
		if slices.Contains(slice.Disabled, "chatops") {
			t.Fatal("ChatOps is wired and still listed as disabled")
		}
		// The routes answer with real authority: only a settings administrator configures, nobody unauthenticated.
		path := e.ProjectPath() + "/chatops-destinations"
		if resp, _ := e.Raw("GET", path, "", nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated destination list: %d", resp.StatusCode)
		}
		for _, who := range []string{"engineer", "reviewer", "viewer"} {
			if code, _ := e.Do(who, "GET", path, nil, nil); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Fatalf("%s listed destinations: %d", who, code)
			}
		}
		if code, b := e.Do("admin", "GET", path, nil, nil); code != http.StatusOK {
			t.Fatalf("the administrator's destination list: %d %s", code, b)
		}
		// The consumer is a real broker consumer and it advances with real events.
		d := e.Destination("assembly")
		item := e.NewBareItem("Assembly", "# assembly")
		e.Ready(item)
		e.WaitReceived("work_item.transitioned", item.Id, time.Minute)
		m2.WaitFor(t, "the ChatOps consumer to acknowledge what it was delivered", 30*time.Second, func() bool {
			st := e.Consumer(slice.Consumers[0].Durable)
			return st.Delivered > 0 && st.AckPending == 0 && st.Pending == 0
		})
		if ds := e.Deliveries("admin", "destinationId="+d.Id); len(ds) == 0 {
			t.Fatal("no delivery for the destination")
		}
	})

	t.Run("without the statement nothing is notified or mapped", func(t *testing.T) {
		e := kit.New(t, kit.Options{NoQA: true, NoChatOps: true})
		slice := e.Loop()
		if slice.QA != nil || slice.ChatOps != nil || len(slice.Consumers) != 0 {
			t.Fatalf("the empty statement selected qa %v chatops %v consumers %v", slice.QA != nil, slice.ChatOps != nil, slice.Consumers)
		}
		if slices.Contains(e.Server().Core().Workers(), "chatops-delivery") {
			t.Fatal("a notification loop runs without a destination policy")
		}
		if slices.Contains(slice.Routes, "chatops") || !slices.Contains(slice.Routes, "work-dependencies") || slice.Dependencies == nil || slice.Flow == nil {
			t.Fatalf("routes %v", slice.Routes)
		}
		if code, _ := e.Do("admin", "GET", e.ProjectPath()+"/chatops-destinations", nil, nil); code == http.StatusOK {
			t.Fatal("destinations can be configured with no destination policy")
		}
		// The dependency graph needs no statement.
		a, b := e.NewBareItem("A", "# a"), e.NewBareItem("B", "# b")
		if code, body := e.Depend("engineer", a, b.Id); code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("link: %d %s", code, body)
		}
	})

	t.Run("a malformed statement stops startup", func(t *testing.T) {
		e := kit.New(t, kit.Options{})
		cases := map[string]func(*app.CoreConfig){
			"a plain http origin":          func(c *app.CoreConfig) { c.BusinessLoop.ChatOps.Origin = "http://api.example.invalid" },
			"an origin with a path":        func(c *app.CoreConfig) { c.BusinessLoop.ChatOps.Origin = "https://api.example.invalid/api" },
			"an unscoped private endpoint": func(c *app.CoreConfig) { c.BusinessLoop.ChatOps.PrivateEndpoints[0].Address = "not-an-address" },
			"a QA mapping with no report":  func(c *app.CoreConfig) { c.BusinessLoop.QA.ReportOutput = "" },
		}
		for name, mutate := range cases {
			cfg := e.Config()
			cfg.Process.HTTPAddr = "127.0.0.1:0"
			mutate(&cfg)
			if srv, err := e.TryStart(cfg); err == nil {
				_ = srv.Shutdown(t.Context())
				t.Fatalf("%s: the process started", name)
			}
		}
	})

	t.Run("the intent is durable before the call and a late send is retried with the same key", func(t *testing.T) {
		e := kit.New(t, kit.Options{NoQA: true, ReceiverBehavior: chatopstest.Hang()})
		e.Destination("hang", "work_item.transitioned")
		item := e.NewBareItem("Durable intent", "# durable intent")
		e.Ready(item)
		var first kit.Payload
		m2.WaitFor(t, "the receiver to be called", time.Minute, func() bool {
			got := e.Received()
			if len(got) == 0 {
				return false
			}
			first = got[0]
			return true
		})
		// While the receiver has not answered the delivery exists, is visible, and is not delivered.
		d, ok := e.DeliveryByID(first.NotificationID)
		if !ok || d.State == "delivered" || d.Attempts < 1 {
			t.Fatalf("delivery while the receiver hangs: %+v (found %v)", d, ok)
		}
		e.Receiver.SetBehavior(chatopstest.Accept("rcpt-late"))
		done := waitDelivered(t, e, first.NotificationID)
		if done.Attempts < 2 || done.Receipt == nil || *done.Receipt != "rcpt-late" {
			t.Fatalf("delivery after the hang: %+v", done)
		}
		h := e.DeliveryDetail("viewer", done.Id).AttemptHistory
		if len(h) < 2 || !h[0].Uncertain {
			t.Fatalf("the unanswered attempt must be recorded as uncertain: %+v", h)
		}
		keys := map[string]int{}
		for _, r := range e.Receiver.Requests() {
			if r.Header.Get("Idempotency-Key") == done.Id {
				keys[string(r.Body)]++
			}
		}
		if len(keys) != 1 {
			t.Fatalf("the retry changed the payload: %d variants", len(keys))
		}
		if !e.Receiver.EffectOf(done.Id) {
			t.Fatal("no effect")
		}
	})

	t.Run("one mutation, one event, one intent per destination, nothing for a refusal", func(t *testing.T) {
		e := kit.New(t, kit.Options{NoQA: true})
		d1 := e.Destination("one", "work_item.transitioned")
		d2 := e.Destination("two", "work_item.transitioned")
		d3 := e.Destination("filtered", "gate.requested")
		item := e.NewBareItem("Intent count", "# intent count")
		ready := e.Ready(item)
		active := e.MustTransition("engineer", ready, "active", nil)
		// Refusals: a stale version, a forbidden edge and a missing reason write no event and no intent.
		if code, _ := e.Transition("engineer", ready, "active", nil); code != http.StatusPreconditionFailed {
			t.Fatalf("a stale transition: %d", code)
		}
		if code, _ := e.Transition("engineer", active, "ready", nil); code != http.StatusUnprocessableEntity {
			t.Fatalf("an active item to ready without a reason: %d", code)
		}
		if code, _ := e.Transition("viewer", active, "cancelled", map[string]any{"reason": "x"}); code != http.StatusForbidden {
			t.Fatalf("a viewer cancelled: %d", code)
		}
		if events := e.WorkEvents("work_item.transitioned", item.Id); events != 2 {
			t.Fatalf("%d transition events for two committed transitions", events)
		}
		m2.WaitFor(t, "two intents per event", time.Minute, func() bool {
			return len(e.Deliveries("admin", "destinationId="+d1.Id)) == 2 && len(e.Deliveries("admin", "destinationId="+d2.Id)) == 2
		})
		time.Sleep(time.Second)
		if n := len(e.Deliveries("admin", "destinationId="+d1.Id)) + len(e.Deliveries("admin", "destinationId="+d2.Id)); n != 4 {
			t.Fatalf("%d intents for 2 events x 2 destinations", n)
		}
		if n := len(e.Deliveries("admin", "destinationId="+d3.Id)); n != 0 {
			t.Fatalf("%d intents for a destination that selects none of these events", n)
		}
		// Two destinations are two notifications with two keys; the versions announced are the committed ones.
		e.WaitSettled(time.Minute)
		keys := map[string]bool{}
		versions := map[int64]bool{}
		for _, p := range e.ReceivedFor("work_item.transitioned", item.Id) {
			keys[p.NotificationID] = true
			versions[p.Resource.Version] = true
		}
		if len(keys) != 4 || !versions[int64(ready.Version)] || !versions[int64(active.Version)] || len(versions) != 2 {
			t.Fatalf("notifications %v versions %v", keys, versions)
		}
	})
}

func waitDelivered(t *testing.T, e *kit.Env, id string) (d client.Delivery) {
	t.Helper()
	m2.WaitFor(t, "delivery "+id+" to be delivered", time.Minute, func() bool {
		got, ok := e.DeliveryByID(id)
		d = got
		return ok && got.State == "delivered"
	})
	return d
}
