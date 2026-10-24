package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsjs "github.com/nats-io/nats.go/jetstream"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// failFinish stands in for a worker process that dies after the receiver
// accepted a request and before the outcome was recorded.
type failFinish struct {
	chatops.WorkStore
	armed atomic.Bool
	calls atomic.Int64
}

func (f *failFinish) Finish(ctx context.Context, c chatops.Claim, o chatops.Outcome, now time.Time, p chatops.RetryPolicy) (chatops.Delivery, error) {
	if f.armed.Load() {
		f.calls.Add(1)
		return chatops.Delivery{}, errors.New("process killed before the outcome was recorded")
	}
	return f.WorkStore.Finish(ctx, c, o, now, p)
}

// Test_duplicate_delivery: concurrent redelivery and repair create one intent per
// destination and event; acceptance followed by a lost response or a process
// failure retries the same key and payload, and a deduplicating receiver applies
// one effect. Real PostgreSQL and NATS; HTTP receiver; no S3 or Git. AC-08.
func Test_duplicate_delivery(t *testing.T) {
	ctx := context.Background()

	t.Run("concurrent redelivery, broker duplicates and repair create one intent per destination and event", func(t *testing.T) {
		s := newStack(t, nil)
		second := s.Destination(s.Actors.Admin, scopeA, "second", s.RC.URL())
		ev := chatopstest.GateRequested(scopeA, "evt_dup_1", "gate_1", 1, 'a')
		payload, err := eventsv1.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		s.Append(ev)
		// A producer's own retry of the same event is recognized, not duplicated.
		s.Append(ev)

		// Duplicate copies on the broker without a message ID, so the broker
		// cannot suppress them: the inbox is the correctness boundary.
		token, err := s.Events.SubjectToken(ctx, scopeA.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		subject, err := events.Subject(s.Bus.Config.Namespace, token, ev.Type)
		if err != nil {
			t.Fatal(err)
		}
		js, err := natsjs.New(s.Bus.Conn)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := js.Publish(ctx, subject, payload); err != nil {
				t.Fatalf("raw publish: %v", err)
			}
		}

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ { // concurrent redeliveries of the same delivery
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Events.Process(ctx, events.Delivery{Tenant: scopeA.TenantID, Consumer: chatops.ConsumerName, Payload: payload}); err != nil {
					t.Errorf("process: %v", err)
				}
			}()
		}
		for i := 0; i < 4; i++ { // concurrent repair passes
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Events.Repair(ctx, scopeA.TenantID, chatops.ConsumerName, 0, 10); err != nil {
					t.Errorf("repair: %v", err)
				}
			}()
		}
		wg.Wait()
		s.Bus.Pump(t, 15*time.Second, "broker copies consumed", func() bool {
			return s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0 && s.Bus.Consumer.Counters().Acked >= 4
		})
		for _, d := range []chatops.Destination{s.Dest, second} {
			if n := s.Deliveries(d.ID); n != 1 {
				t.Fatalf("destination %s has %d intents, want exactly 1", d.Name, n)
			}
		}
		if n := s.Count(`SELECT count(*) FROM chatops_deliveries`); n != 2 {
			t.Fatalf("intents = %d, want 2 (one per destination)", n)
		}
		if n := s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_dup_1' AND state = 'completed'`); n != 1 {
			t.Fatalf("inbox entries = %d", n)
		}
		// Sending both: each destination's receiver gets one request and one effect.
		w := s.Worker(s.Client(s.RC))
		drain(t, w, 10*time.Second, "both delivered", func() bool {
			return s.Count(`SELECT count(*) FROM chatops_deliveries WHERE state = 'delivered'`) == 2
		})
		if s.RC.Count() != 2 || s.RC.Effects() != 2 {
			t.Fatalf("receiver requests=%d effects=%d, want 2/2", s.RC.Count(), s.RC.Effects())
		}
	})

	t.Run("acceptance followed by a lost response retries the same key and payload and the receiver applies one effect", func(t *testing.T) {
		s := newStack(t, chatopstest.Then(1, chatopstest.LoseResponse(), chatopstest.Accept("rcpt-2")))
		ev := chatopstest.GateRequested(scopeA, "evt_lost_1", "gate_1", 1, 'a')
		s.Append(ev)
		s.Bus.Pump(t, 15*time.Second, "the intent", func() bool { return s.Deliveries(s.Dest.ID) == 1 })
		id := s.deliveryID("evt_lost_1")
		w := s.Worker(s.Client(s.RC))
		drain(t, w, 10*time.Second, "delivery", func() bool { return s.Delivery(scopeA, id).State == chatops.StateDelivered })

		if s.RC.Count() != 2 || s.RC.Effects() != 1 {
			t.Fatalf("requests=%d effects=%d, want 2 requests applying 1 effect", s.RC.Count(), s.RC.Effects())
		}
		reqs := s.RC.Requests()
		if reqs[0].Header.Get("Idempotency-Key") != string(id) || reqs[1].Header.Get("Idempotency-Key") != string(id) || string(reqs[0].Body) != string(reqs[1].Body) {
			t.Fatal("the retry changed the key or the payload")
		}
		if reqs[0].Header.Get("Kodama-Attempt") != "1" || reqs[1].Header.Get("Kodama-Attempt") != "2" {
			t.Fatalf("attempt numbers = %s, %s", reqs[0].Header.Get("Kodama-Attempt"), reqs[1].Header.Get("Kodama-Attempt"))
		}
		hist := s.attempts(id)
		if len(hist) != 2 || hist[0].Result != chatops.ResultRetry || hist[0].Class != chatops.ClassConnectionLost || !hist[0].Uncertain ||
			hist[1].Result != chatops.ResultDelivered || hist[1].Uncertain || hist[1].Receipt != "rcpt-2" {
			t.Fatalf("history = %+v", hist)
		}
		if d := s.Delivery(scopeA, id); d.State != chatops.StateDelivered || d.LastUncertain {
			t.Fatalf("delivery = %+v", d)
		}
	})

	t.Run("a process failure after receiver acceptance resends the same key after the lease and the receiver applies one effect", func(t *testing.T) {
		s := newStack(t, nil)
		ev := chatopstest.GateRequested(scopeA, "evt_crash_1", "gate_1", 1, 'a')
		s.Append(ev)
		s.Bus.Pump(t, 15*time.Second, "the intent", func() bool { return s.Deliveries(s.Dest.ID) == 1 })
		id := s.deliveryID("evt_crash_1")

		dying := &failFinish{WorkStore: s.Store}
		dying.armed.Store(true)
		cfg := s.WorkerConfig(s.Client(s.RC))
		cfg.Work = dying
		w1, err := chatops.NewWorker(cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := w1.RunOnce(ctx)
		if err != nil || res.Claimed != 1 || res.Unrecorded != 1 {
			t.Fatalf("first pass = %+v %v", res, err)
		}
		if s.RC.Count() != 1 || s.RC.Effects() != 1 {
			t.Fatalf("the receiver accepted %d/%d before the crash", s.RC.Count(), s.RC.Effects())
		}
		if d := s.Delivery(scopeA, id); d.State != chatops.StateClaimed || d.Attempts != 1 {
			t.Fatalf("after the crash the delivery must still be claimed with its attempt on record: %+v", d)
		}
		// Nothing else may send it while the lease is live.
		w2 := s.Worker(s.Client(s.RC))
		if res, err := w2.RunOnce(ctx); err != nil || res.Claimed != 0 {
			t.Fatalf("a live lease was taken over: %+v %v", res, err)
		}
		// The lease expires; a healthy worker resends the identical request.
		s.Clock.Advance(time.Minute)
		drain(t, w2, 10*time.Second, "delivery after the lease", func() bool { return s.Delivery(scopeA, id).State == chatops.StateDelivered })

		if s.RC.Count() != 2 || s.RC.Effects() != 1 {
			t.Fatalf("requests=%d effects=%d, want 2 requests applying 1 effect", s.RC.Count(), s.RC.Effects())
		}
		reqs := s.RC.Requests()
		if reqs[0].Header.Get("Idempotency-Key") != reqs[1].Header.Get("Idempotency-Key") || string(reqs[0].Body) != string(reqs[1].Body) {
			t.Fatal("the resend changed the key or the payload")
		}
		hist := s.attempts(id)
		if len(hist) != 2 || hist[0].Result != chatops.ResultAbandoned || hist[0].Class != chatops.ClassLeaseExpired || !hist[0].Uncertain || hist[1].Result != chatops.ResultDelivered {
			t.Fatalf("history = %+v", hist)
		}
	})

	t.Run("two workers racing over the same due work send each notification once", func(t *testing.T) {
		s := newStack(t, nil)
		const n = 20
		for i := 0; i < n; i++ {
			s.Append(chatopstest.RunState(scopeA, fmt.Sprintf("evt_race_%02d", i), fmt.Sprintf("run_%d", i), 1))
		}
		s.Bus.Pump(t, 30*time.Second, "all intents", func() bool { return s.Deliveries(s.Dest.ID) == n })
		client := s.Client(s.RC)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			w := s.Worker(client)
			go func() {
				defer wg.Done()
				deadline := time.Now().Add(20 * time.Second)
				for s.Count(`SELECT count(*) FROM chatops_deliveries WHERE state = 'delivered'`) < n && time.Now().Before(deadline) {
					if _, err := w.RunOnce(ctx); err != nil {
						t.Errorf("pass: %v", err)
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
		}
		wg.Wait()
		if s.RC.Count() != n || s.RC.Effects() != n {
			t.Fatalf("receiver requests=%d effects=%d, want %d each", s.RC.Count(), s.RC.Effects(), n)
		}
		if got := s.Count(`SELECT count(*) FROM chatops_deliveries WHERE attempts = 1 AND state = 'delivered'`); got != n {
			t.Fatalf("%d deliveries have exactly one attempt, want %d", got, n)
		}
	})

	t.Run("distinct events at one resource version are distinct notifications and repeats are not", func(t *testing.T) {
		s := newStack(t, nil)
		a := chatopstest.WorkItem(scopeA, "evt_same_1", "wi_1", 4)
		b := chatopstest.WorkItem(scopeA, "evt_same_2", "wi_1", 4) // distinct event, same version
		for _, ev := range []eventsv1.Envelope{a, b, a, b} {
			s.Append(ev)
			process(t, s, ev)
		}
		if n := s.Deliveries(s.Dest.ID); n != 2 {
			t.Fatalf("intents = %d, want 2 distinct notifications", n)
		}
	})
}

// restartedEvents builds the event store a restarted process would run: the
// same database and ChatOps binding, with a longer inbox and observation policy.
// Raising a policy cannot restore evidence an earlier prune deleted; T-005's
// durable deduplication boundary decides, not this process's options.
func restartedEvents(t *testing.T, s *stack, retention func(*events.RetentionPolicy)) *eventspg.Store {
	t.Helper()
	binding, err := chatopspg.NewBinding(s.Planner)
	if err != nil {
		t.Fatal(err)
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = s.Clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	retention(&opts.Retention)
	store, err := eventspg.NewStore(s.Runtime, opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// publishRawCopy puts one more copy of the event on the broker with no message
// ID, as a late publisher or a client buffer could.
func publishRawCopy(t *testing.T, s *stack, ev eventsv1.Envelope) {
	t.Helper()
	ctx := context.Background()
	payload, err := eventsv1.Encode(ev)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Events.SubjectToken(ctx, ev.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := events.Subject(s.Bus.Config.Namespace, token, ev.Type)
	if err != nil {
		t.Fatal(err)
	}
	js, err := natsjs.New(s.Bus.Conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, subject, payload); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
}

// pruneAll advances the clock beyond every retention lifetime and runs T-005's
// public retention pass, returning its result.
func pruneAll(t *testing.T, s *stack, by time.Duration) eventspg.PruneResult {
	t.Helper()
	s.Clock.Advance(by)
	pr, err := s.Events.Prune(context.Background(), scopeA.TenantID, s.Clock.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	return pr
}

// Test_duplicate_delivery_after_retention pins the current T-005 recovery
// boundary: once a notification is complete and its event evidence has been
// pruned, no late broker copy, repair or external-intent replay can create a new
// ChatOps intent or receiver effect. ChatOps keeps its own delivery records and
// stable keys, and an authorized replay of a delivery ChatOps already holds
// stays possible. It does not change T-005's retention or replay horizons.
// Real PostgreSQL and NATS; local HTTPS receiver; no S3 or Git. AC-08.
func Test_duplicate_delivery_after_retention(t *testing.T) {
	ctx := context.Background()

	t.Run("a late broker copy after pruned evidence is quarantined, even after a restart with longer retention", func(t *testing.T) {
		s := newStack(t, nil)
		ev := chatopstest.GateRequested(scopeA, "evt_late_1", "gate_1", 1, 'a')
		s.Append(ev)
		s.Bus.Pump(t, 15*time.Second, "the intent and its publication", func() bool {
			return s.Deliveries(s.Dest.ID) == 1 && s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0
		})
		id := s.deliveryID("evt_late_1")
		w := s.Worker(s.Client(s.RC))
		drain(t, w, 10*time.Second, "delivery", func() bool { return s.Delivery(scopeA, id).State == chatops.StateDelivered })
		if s.RC.Count() != 1 || s.RC.Effects() != 1 {
			t.Fatalf("receiver requests=%d effects=%d, want 1/1", s.RC.Count(), s.RC.Effects())
		}
		before := s.Delivery(scopeA, id)
		historyBefore := s.attempts(id)

		pr := pruneAll(t, s, 60*time.Hour)
		if pr.Inbox != 1 || pr.Outbox != 1 || pr.Observations != 1 {
			t.Fatalf("prune = %+v, want every kind of event evidence removed", pr)
		}
		// ChatOps retains its own record, key and history; nothing of it is event evidence.
		if after := s.Delivery(scopeA, id); after.ID != before.ID || after.State != chatops.StateDelivered || after.Version != before.Version || string(after.Payload) != string(before.Payload) {
			t.Fatalf("pruning event evidence changed the delivery: %+v -> %+v", before, after)
		}
		if got := s.attempts(id); len(got) != len(historyBefore) {
			t.Fatalf("attempt history changed: %+v -> %+v", historyBefore, got)
		}

		// A late copy through the real broker consumer is quarantined durably.
		publishRawCopy(t, s, ev)
		s.Bus.Pump(t, 15*time.Second, "the late copy acknowledged", func() bool {
			return s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_quarantine WHERE consumer = 'chatops' AND event_id = 'evt_late_1' AND reason_code = 'beyond_horizon' AND state = 'quarantined'`) == 1
		})

		// A restart with a longer retention policy must not lower the boundary.
		restarted := restartedEvents(t, s, func(r *events.RetentionPolicy) { r.Inbox, r.Observation = 96*time.Hour, 96*time.Hour })
		payload, err := eventsv1.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		out, err := restarted.Process(ctx, events.Delivery{Tenant: scopeA.TenantID, Consumer: chatops.ConsumerName, Payload: payload})
		if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != eventspg.ReasonBeyondHorizon {
			t.Fatalf("late copy after restart = %+v %v", out, err)
		}
		if res, err := restarted.Repair(ctx, scopeA.TenantID, chatops.ConsumerName, 0, 10); err != nil || res.Refused != 0 {
			t.Fatalf("repair after restart = %+v %v (no retained record should be selected)", res, err)
		}
		// The producer's own retry of the pruned event is refused too.
		err = storage.WithTenant(ctx, s.Runtime, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := restarted.Append(ctx, tx, ev)
			return err
		})
		if !errors.Is(err, events.ErrStaleAppend) {
			t.Fatalf("re-append of a pruned event = %v, want ErrStaleAppend", err)
		}

		// No new intent, receiver request or effect, and the delivery is unchanged.
		if n := s.Count(`SELECT count(*) FROM chatops_deliveries`); n != 1 {
			t.Fatalf("intents = %d, want 1", n)
		}
		drainOnce(t, w)
		if s.RC.Count() != 1 || s.RC.Effects() != 1 {
			t.Fatalf("a late copy reached the receiver: requests=%d effects=%d", s.RC.Count(), s.RC.Effects())
		}
		if after := s.Delivery(scopeA, id); after.State != chatops.StateDelivered || after.Attempts != before.Attempts || after.Version != before.Version {
			t.Fatalf("delivery after late copies = %+v", after)
		}
	})

	t.Run("external-intent replay beyond the evidence horizon is refused while ChatOps replay of a held failure still works", func(t *testing.T) {
		s := newStackWith(t, chatopstest.Accept("rcpt-ok"), chatopstest.Options{Events: func(o *eventspg.Options) {
			o.Retention.Observation = 200 * time.Hour // observations outlive inbox evidence, so replay is classified, not merely expired
		}})
		w := s.Worker(s.Client(s.RC))
		delivered := chatopstest.GateRequested(scopeA, "evt_hz_ok", "gate_1", 1, 'a')
		s.Append(delivered)
		s.Bus.Pump(t, 15*time.Second, "the first intent", func() bool { return s.Deliveries(s.Dest.ID) == 1 })
		okID := s.deliveryID("evt_hz_ok")
		drain(t, w, 10*time.Second, "first delivery", func() bool { return s.Delivery(scopeA, okID).State == chatops.StateDelivered })

		s.RC.SetBehavior(chatopstest.Status(503))
		failing := chatopstest.GateRequested(scopeA, "evt_hz_fail", "gate_2", 1, 'b')
		s.Append(failing)
		s.Bus.Pump(t, 15*time.Second, "the second intent and publication", func() bool {
			return s.Deliveries(s.Dest.ID) == 2 && s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0
		})
		failID := s.deliveryID("evt_hz_fail")
		drain(t, w, 15*time.Second, "exhaustion", func() bool { return s.Delivery(scopeA, failID).State == chatops.StateFailed })
		failedAttempts := s.Delivery(scopeA, failID).Attempts
		failedBody := string(s.Delivery(scopeA, failID).Payload)
		requestsBefore := s.RC.Count()
		// Effects counts distinct idempotency keys seen, including the one the failing
		// receiver saw (503) before it exhausted: the late copy must add none.
		effectsBefore := s.RC.Effects()
		if effectsBefore != 2 {
			t.Fatalf("effects before the late copy = %d, want 2 (delivered key and failing key)", effectsBefore)
		}

		pr := pruneAll(t, s, 60*time.Hour)
		if pr.Inbox != 2 || pr.Observations != 0 {
			t.Fatalf("prune = %+v, want inbox evidence removed and observations retained", pr)
		}
		// Pending and failed ChatOps records and their keys survive pruning.
		if s.Delivery(scopeA, okID).State != chatops.StateDelivered || s.Delivery(scopeA, failID).State != chatops.StateFailed {
			t.Fatal("pruning event evidence changed ChatOps delivery records")
		}

		// The external-intent consumer is not replayable beyond the horizon, with or
		// without the acknowledgement that only helps consumers declared idempotent.
		for _, allow := range []bool{false, true} {
			req := events.ReplayRequest{Scope: events.Scope{TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID}, Consumer: chatops.ConsumerName,
				FromSeq: 1, ToSeq: 2, Reason: "re-send everything", Operator: events.Operator{ID: "ops_1"}, AllowBeyondHorizon: allow}
			class, err := s.Events.DryRun(ctx, req)
			if !errors.Is(err, events.ErrReplayBeyondHorizon) || class.BeyondHorizon != 2 {
				t.Fatalf("dry run (allow=%v) = %+v %v", allow, class, err)
			}
			if _, err := s.Events.RequestReplay(ctx, req); !errors.Is(err, events.ErrReplayBeyondHorizon) {
				t.Fatalf("replay beyond the horizon (allow=%v) = %v, want ErrReplayBeyondHorizon", allow, err)
			}
		}
		if n := s.Count(`SELECT count(*) FROM event_replay_requests`); n != 0 {
			t.Fatalf("a refused replay was persisted: %d", n)
		}
		if n := s.Count(`SELECT count(*) FROM chatops_deliveries`); n != 2 {
			t.Fatalf("intents = %d, want 2", n)
		}

		// A late copy of the delivered event is refused, with no receiver effect.
		publishRawCopy(t, s, delivered)
		s.Bus.Pump(t, 15*time.Second, "the late copy quarantined", func() bool {
			return s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_quarantine WHERE consumer = 'chatops' AND event_id = 'evt_hz_ok' AND reason_code = 'beyond_horizon'`) == 1
		})
		drainOnce(t, w)
		if s.RC.Count() != requestsBefore || s.RC.Effects() != effectsBefore {
			t.Fatalf("late copy reached the receiver: requests=%d (was %d) effects=%d (was %d)", s.RC.Count(), requestsBefore, s.RC.Effects(), effectsBefore)
		}

		// The failed delivery is held by ChatOps, so an authorized replay of it
		// remains possible and reuses the stable key and the exact payload.
		s.RC.SetBehavior(chatopstest.Accept("rcpt-replay"))
		if _, err := s.Svc.ReplayDelivery(ctx, s.Actors.Admin, scopeA, failID, s.Delivery(scopeA, failID).Version, "receiver is back"); err != nil {
			t.Fatalf("authorized replay of a held failure: %v", err)
		}
		drain(t, w, 10*time.Second, "the replayed delivery", func() bool { return s.Delivery(scopeA, failID).State == chatops.StateDelivered })
		reqs := s.RC.Requests()
		last := reqs[len(reqs)-1]
		if last.Header.Get("Idempotency-Key") != string(failID) || string(last.Body) != failedBody {
			t.Fatalf("the replay changed the key or payload: %q", last.Header.Get("Idempotency-Key"))
		}
		if got := s.Delivery(scopeA, failID); got.Attempts != failedAttempts+1 || got.Replays != 1 {
			t.Fatalf("replayed delivery = %+v", got)
		}
		if s.RC.Effects() != 2 {
			t.Fatalf("effects = %d, want 2 (one per notification)", s.RC.Effects())
		}
	})
}

// drainOnce runs one worker pass, so work a late copy wrongly created would be sent.
func drainOnce(t *testing.T, w *chatops.Worker) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("worker pass: %v", err)
		}
	}
}
