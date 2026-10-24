package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
)

var scopeA = chatopstest.ScopeA

func process(t testing.TB, e *chatopstest.Env, env eventsv1.Envelope) events.Outcome {
	t.Helper()
	payload, err := eventsv1.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Events.Process(context.Background(), events.Delivery{Tenant: env.TenantID, Consumer: chatops.ConsumerName, Payload: payload})
	if err != nil {
		t.Fatalf("process %s: %v", env.ID, err)
	}
	return out
}

// TestInboxIntentAtomicity: the delivery intents of one event commit with the
// inbox completion or not at all; reordered and same-version distinct events stay
// eligible; events nobody selected create nothing; redelivery creates nothing.
// Real PostgreSQL (AC-08).
func TestInboxIntentAtomicity(t *testing.T) {
	e := chatopstest.New(t, chatopstest.Options{})
	e.SeedProject(scopeA)
	a := e.Actors(scopeA, "a")

	d1 := e.Destination(a.Admin, scopeA, "alpha", "https://hooks.t041.example/a")
	d2 := e.Destination(a.Admin, scopeA, "bravo", "https://hooks.t041.example/b")
	d3 := e.Destination(a.Admin, scopeA, "charlie", "https://hooks.t041.example/c")
	// A destination that does not select work items, and a disabled one.
	other := e.Destination(a.Admin, scopeA, "gates-only", "https://hooks.t041.example/d", "gate.requested")
	off, err := e.Svc.CreateDestination(context.Background(), a.Admin, scopeA, chatops.DestinationInput{
		Name: "disabled", Endpoint: "https://hooks.t041.example/e", Events: chatopstest.AllEvents(), Enabled: false})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a partial fan-out failure rolls back every intent and the inbox completion", func(t *testing.T) {
		// The third insertion of the event fails inside the handler's savepoint.
		e.OwnerExec(`CREATE FUNCTION t041_inject() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.destination_id = '` + string(d3.ID) + `' THEN RAISE EXCEPTION 'injected insert failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER t041_inject BEFORE INSERT ON chatops_deliveries FOR EACH ROW EXECUTE FUNCTION t041_inject()`)
		ev := chatopstest.WorkItem(scopeA, "evt_atomic", "wi_1", 1)
		e.Append(ev)
		out := process(t, e, ev)
		if out.Kind != events.OutcomeRetry {
			t.Fatalf("outcome = %+v, want a recorded transient retry", out)
		}
		for _, d := range []chatops.Destination{d1, d2, d3} {
			if n := e.Deliveries(d.ID); n != 0 {
				t.Fatalf("destination %s has %d intents after a failed fan-out, want 0", d.Name, n)
			}
		}
		if n := e.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_atomic' AND state = 'completed'`); n != 0 {
			t.Fatalf("inbox completed %d times despite the failed handler", n)
		}
		e.OwnerExec(`DROP TRIGGER t041_inject ON chatops_deliveries; DROP FUNCTION t041_inject()`)
		// Let the recorded backoff elapse, then the retry completes all three at once.
		e.Clock.Advance(time.Minute)
		if out := process(t, e, ev); out.Kind != events.OutcomeCompleted {
			t.Fatalf("retry outcome = %+v", out)
		}
		for _, d := range []chatops.Destination{d1, d2, d3} {
			if n := e.Deliveries(d.ID); n != 1 {
				t.Fatalf("destination %s has %d intents, want 1", d.Name, n)
			}
		}
		if n := e.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_atomic' AND state = 'completed'`); n != 1 {
			t.Fatalf("inbox completed %d times after the retry, want 1", n)
		}
		if n := e.Deliveries(other.ID) + e.Deliveries(off.ID); n != 0 {
			t.Fatalf("an unselecting or disabled destination received %d intents", n)
		}
	})

	t.Run("redelivery and repair create no new intents", func(t *testing.T) {
		ev := chatopstest.WorkItem(scopeA, "evt_atomic", "wi_1", 1)
		for i := 0; i < 3; i++ {
			if out := process(t, e, ev); out.Kind != events.OutcomeDuplicate {
				t.Fatalf("redelivery %d = %+v, want duplicate", i, out)
			}
		}
		if _, err := e.Events.Repair(context.Background(), scopeA.TenantID, chatops.ConsumerName, 0, 10); err != nil {
			t.Fatal(err)
		}
		if n := e.Deliveries(d1.ID); n != 1 {
			t.Fatalf("intents after redelivery = %d, want 1", n)
		}
	})

	t.Run("reordered and same-version distinct events remain eligible", func(t *testing.T) {
		// Newer first, then older, then a distinct event at the same version.
		for _, ev := range []eventsv1.Envelope{
			chatopstest.WorkItem(scopeA, "evt_v5", "wi_2", 5),
			chatopstest.WorkItem(scopeA, "evt_v3", "wi_2", 3),
			chatopstest.WorkItem(scopeA, "evt_v5b", "wi_2", 5),
		} {
			e.Append(ev)
			if out := process(t, e, ev); out.Kind != events.OutcomeCompleted {
				t.Fatalf("%s: %+v", ev.ID, out)
			}
		}
		if n := e.Deliveries(d1.ID); n != 4 {
			t.Fatalf("intents = %d, want 4 (1 + three distinct events)", n)
		}
		// Each carries its own event identity and version.
		if got := e.DeliveryOf(d1.ID, "evt_v3"); got.State != chatops.StatePending || got.EventType != eventsv1.TypeWorkItemTransitioned {
			t.Fatalf("delivery = %+v", got)
		}
	})

	t.Run("tenant-wide and unlisted events create nothing", func(t *testing.T) {
		before := e.Deliveries(d1.ID)
		for _, ev := range []eventsv1.Envelope{chatopstest.MembershipEvent(scopeA.TenantID, "evt_member"), chatopstest.UsageEvent(scopeA, "evt_usage")} {
			e.Append(ev)
			if out := process(t, e, ev); out.Kind != events.OutcomeCompleted {
				t.Fatalf("%s: %+v", ev.ID, out)
			}
		}
		if n := e.Deliveries(d1.ID); n != before {
			t.Fatalf("intents changed from %d to %d for ignored events", before, n)
		}
	})

	t.Run("a destination selects the event types it lists", func(t *testing.T) {
		ev := chatopstest.GateRequested(scopeA, "evt_gate", "gate_1", 1, 'a')
		e.Append(ev)
		if out := process(t, e, ev); out.Kind != events.OutcomeCompleted {
			t.Fatalf("%+v", out)
		}
		if n := e.Deliveries(other.ID); n != 1 {
			t.Fatalf("gate-only destination intents = %d, want 1", n)
		}
	})
}

// TestConsumerSpecIsExternalIntent pins the bus classification: historical,
// external intent, never idempotent beyond the horizon.
func TestConsumerSpecIsExternalIntent(t *testing.T) {
	s := chatops.ConsumerSpec()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Mode != events.ModeHistorical || s.Effect != events.EffectExternalIntent || s.IdempotentBeyondHorizon {
		t.Fatalf("spec = %+v", s)
	}
	for _, typ := range s.Types {
		if !chatops.IsSupportedEvent(typ) {
			t.Fatalf("unsupported type %s", typ)
		}
	}
	_ = domain.Scope{}
}

// TestConflictingRedeliveryPreservesOriginalIntent: valid but changed content
// under an existing event identity is quarantined by T-005 as conflicting_content
// and never creates, replaces or reschedules ChatOps intent. The original is
// completed (or repaired) with its own content only. Real PostgreSQL and the
// real T-005 Append/Process.
func TestConflictingRedeliveryPreservesOriginalIntent(t *testing.T) {
	changed := func(env eventsv1.Envelope) eventsv1.Envelope {
		c := env
		c.Resource.Version += 5
		c.Data = json.RawMessage(`{"from":"ready","to":"closed","transitionId":"tr_other"}`)
		return c
	}
	conflicts := func(e *chatopstest.Env) int {
		return e.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_quarantine WHERE reason_code = $1`, eventspg.ReasonConflictingContent)
	}
	version := func(t *testing.T, d chatops.Delivery) int64 {
		t.Helper()
		var p chatops.Payload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Resource.Version
	}

	t.Run("a pending original after an injected fan-out failure", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "a")
		d1 := e.Destination(a.Admin, scopeA, "alpha", "https://hooks.t041.example/a")
		d2 := e.Destination(a.Admin, scopeA, "bravo", "https://hooks.t041.example/b")

		e.OwnerExec(`CREATE FUNCTION t041_inject() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.destination_id = '` + string(d2.ID) + `' THEN RAISE EXCEPTION 'injected insert failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER t041_inject BEFORE INSERT ON chatops_deliveries FOR EACH ROW EXECUTE FUNCTION t041_inject()`)
		orig := chatopstest.WorkItem(scopeA, "evt_conflict_p", "wi_c", 1)
		e.Append(orig)
		if out := process(t, e, orig); out.Kind != events.OutcomeRetry {
			t.Fatalf("original outcome = %+v, want a recorded transient retry", out)
		}
		attempts := e.CountTenant(scopeA.TenantID, `SELECT attempts FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_conflict_p' AND state = 'pending'`)
		e.OwnerExec(`DROP TRIGGER t041_inject ON chatops_deliveries; DROP FUNCTION t041_inject()`)
		e.Clock.Advance(time.Minute) // the original's backoff has elapsed: a handler would run now

		out := process(t, e, changed(orig))
		if out.Kind != events.OutcomeQuarantined || out.Reason != eventspg.ReasonConflictingContent {
			t.Fatalf("conflicting delivery = %+v", out)
		}
		if conflicts(e) != 1 {
			t.Fatalf("conflicting_content quarantine records = %d, want 1", conflicts(e))
		}
		if n := e.Deliveries(d1.ID) + e.Deliveries(d2.ID); n != 0 {
			t.Fatalf("the conflicting content created %d intents", n)
		}
		if got := e.CountTenant(scopeA.TenantID, `SELECT attempts FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_conflict_p' AND state = 'pending'`); got != attempts {
			t.Fatalf("the original inbox entry changed: attempts %d, was %d", got, attempts)
		}

		// Repair processes the original content only.
		if _, err := e.Events.Repair(context.Background(), scopeA.TenantID, chatops.ConsumerName, 0, 10); err != nil {
			t.Fatal(err)
		}
		for _, d := range []chatops.Destination{d1, d2} {
			if n := e.Deliveries(d.ID); n != 1 {
				t.Fatalf("destination %s has %d intents after repair, want 1", d.Name, n)
			}
			if v := version(t, e.DeliveryOf(d.ID, "evt_conflict_p")); v != 1 {
				t.Fatalf("destination %s carries resource version %d, want the original 1", d.Name, v)
			}
		}
		if out := process(t, e, orig); out.Kind != events.OutcomeDuplicate {
			t.Fatalf("redelivery of the original = %+v", out)
		}
	})

	t.Run("a completed original", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "a")
		d := e.Destination(a.Admin, scopeA, "alpha", "https://hooks.t041.example/a")

		orig := chatopstest.WorkItem(scopeA, "evt_conflict_c", "wi_c", 1)
		e.Append(orig)
		if out := process(t, e, orig); out.Kind != events.OutcomeCompleted {
			t.Fatalf("original outcome = %+v", out)
		}
		before := e.DeliveryOf(d.ID, "evt_conflict_c")

		out := process(t, e, changed(orig))
		if out.Kind != events.OutcomeQuarantined || out.Reason != eventspg.ReasonConflictingContent {
			t.Fatalf("conflicting delivery = %+v", out)
		}
		if conflicts(e) != 1 || e.Deliveries(d.ID) != 1 {
			t.Fatalf("quarantine records = %d, intents = %d; want 1 and 1", conflicts(e), e.Deliveries(d.ID))
		}
		after := e.DeliveryOf(d.ID, "evt_conflict_c")
		if string(after.Payload) != string(before.Payload) || after.Version != before.Version || !after.NextAttemptAt.Equal(before.NextAttemptAt) || after.State != chatops.StatePending {
			t.Fatalf("the original intent changed: %+v -> %+v", before, after)
		}
		if _, err := e.Events.Repair(context.Background(), scopeA.TenantID, chatops.ConsumerName, 0, 10); err != nil {
			t.Fatal(err)
		}
		if e.Deliveries(d.ID) != 1 || version(t, e.DeliveryOf(d.ID, "evt_conflict_c")) != 1 {
			t.Fatal("repair changed the original intent")
		}
		if out := process(t, e, orig); out.Kind != events.OutcomeDuplicate {
			t.Fatalf("redelivery of the original = %+v", out)
		}
	})
}
