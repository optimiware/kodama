package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsjs "github.com/nats-io/nats.go/jetstream"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

// gbus is the real event bus (PostgreSQL outbox/inbox, NATS JetStream) wired
// with the gate decision consumer.
type gbus struct {
	t      testing.TB
	e      *gatestest.Env
	store  *eventspg.Store
	broker *eventstest.Broker
	cfg    jetstream.Config
	proxy  *eventstest.Proxy
	pub    *events.Publisher
	cons   *jetstream.Consumer
}

func newGBus(t testing.TB, e *gatestest.Env) *gbus {
	t.Helper()
	broker := eventstest.NewBroker(t)
	binding, err := e.Store.DecisionBinding(gatespg.DecisionConsumerName)
	if err != nil {
		t.Fatal(err)
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = e.X.Clock.Now
	b := &gbus{t: t, e: e, broker: broker, cfg: broker.StreamConfig(t, time.Minute)}
	b.store = e.X.Base.NewStore(t, opts, binding)
	b.proxy = eventstest.NewProxy(t, broker.Addr())
	jp, err := jetstream.NewPublisher(broker.Connect(t, b.proxy.Addr()), b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := jp.EnsureStream(ctx); err != nil {
		t.Fatal(err)
	}
	b.pub, err = events.NewPublisher(b.store, b.store, jp, events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: 500 * time.Millisecond, BatchSize: 50, Interval: 50 * time.Millisecond,
		Retry: events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3}, Clock: e.X.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	b.cons, err = jetstream.NewConsumer(broker.Connect(t, broker.Addr()), b.cfg, jetstream.ConsumerConfig{
		Consumer: gatespg.DecisionConsumerName, Durable: "t034_gates_" + eventstest.RandomHex(t, 3),
		AckWait: 5 * time.Second, MaxAckPending: 50, FetchBatch: 10, FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond,
	}, b.store, b.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cons.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *gbus) drain(timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, _ = b.pub.RunOnce(context.Background())
		if b.e.X.CountTenant(gatestest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0 {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("outbox not drained within %s", timeout)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (b *gbus) pump(timeout time.Duration, what string, cond func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = b.cons.RunOnce(ctx)
		cancel()
	}
}

func (b *gbus) inbox(event string) int {
	return b.e.X.CountTenant(gatestest.ScopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = $1 AND state = 'completed'`, gatespg.DecisionConsumerName)
}

func stepHistory(e *gatestest.Env, run *execution.Run, step string) int {
	return count(e, `SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = $2 AND to_status <> 'pending'`, string(run.ID), step)
}

// decidedEvent is a gate.decided notification, as committed by a decision.
func decidedEvent(id string, g *gates.Gate, state string, at time.Time) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.GateDecided{DecidedBy: eventsv1.Actor{Type: eventsv1.ActorUser, ID: gatestest.Reviewer}, Decision: state, State: state, SubjectDigest: g.SubjectDigest()})
	return eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeGateDecided, OccurredAt: at.UTC().Truncate(time.Microsecond),
		TenantID: g.Scope.TenantID, ProjectID: g.Scope.ProjectID, Resource: eventsv1.Resource{Type: "gate", ID: string(g.ID), Version: int64(g.Version) + 1},
		Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: gatestest.Reviewer}, CorrelationID: "req_" + id, Data: data,
	}
}

// flakyRunEvents fails the first n run.state_changed appends of the execution
// store, which is how a gate completion that settles its run fails after the
// step and run mutations were written. Before each append it calls the observer
// with the 1-based attempt number, so a test can inspect what the previous,
// failed attempt left behind.
type flakyRunEvents struct {
	mu       sync.Mutex
	failures int
	hits     int
	observe  func(attempt int)
}

func (f *flakyRunEvents) arm(failures int, observe func(attempt int)) {
	f.mu.Lock()
	f.failures, f.hits, f.observe = failures, 0, observe
	f.mu.Unlock()
}

// disarm stops injecting and returns the number of appends seen while armed.
func (f *flakyRunEvents) disarm() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	hits := f.hits
	f.failures, f.hits, f.observe = 0, 0, nil
	return hits
}

func (f *flakyRunEvents) wrap(inner gatespg.EventAppender) gatespg.EventAppender {
	return &flakyAppender{EventAppender: inner, f: f}
}

type flakyAppender struct {
	gatespg.EventAppender
	f *flakyRunEvents
}

func (a *flakyAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	a.f.mu.Lock()
	armed := a.f.observe != nil && env.Type == eventsv1.TypeRunStateChanged
	var observe func(int)
	var fail bool
	var n int
	if armed {
		a.f.hits++
		n, observe, fail = a.f.hits, a.f.observe, a.f.hits <= a.f.failures
	}
	a.f.mu.Unlock()
	if armed {
		observe(n)
	}
	if fail {
		return eventspg.AppendResult{}, errors.New("injected execution event failure")
	}
	return a.EventAppender.Append(ctx, tx, env)
}

// TestGateRecoveryEvents (PX-05, PX-A03, PX-A08): a gate converges without
// duplicate transitions whatever the broker does. Events only wake
// reconciliation: a broker outage, a lost publication acknowledgement, a
// duplicated or reordered notification and a missing wakeup all end in the same
// durable state, and a notification can never grant an approval. Real
// PostgreSQL and NATS.
func TestGateRecoveryEvents(t *testing.T) {
	flaky := &flakyRunEvents{}
	e := gatestest.New(t, gatestest.Options{WrapExecEvents: flaky.wrap})
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA
	tenant := scope.TenantID

	t.Run("a broker outage is covered by the scan and the late event changes nothing", func(t *testing.T) {
		b := newGBus(t, e)
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		b.proxy.Stop() // the broker is unreachable for the whole decision
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		_, _ = b.pub.RunOnce(ctx)
		if n := e.X.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE event_type = 'gate.decided' AND state <> 'published'`); n < 1 {
			t.Fatalf("the decision event was not retained in the outbox: %d", n)
		}
		// The committed decision is the durable intent; the scan finds it.
		if rep := reconcile(t, e, tenant); rep.Completed != 1 {
			t.Fatalf("scan: %+v", rep)
		}
		history := stepHistory(e, run, "review")
		b.proxy.Start()
		b.drain(10 * time.Second)
		b.pump(10*time.Second, "the decision consumer", func() bool { return b.inbox("") >= 1 })
		if got := stepHistory(e, run, "review"); got != history {
			t.Fatalf("the late event moved the step again: %d -> %d history rows", history, got)
		}
		if n := attemptsOf(e, run, "review"); n != 0 {
			t.Fatalf("%d attempts for the gate", n)
		}
	})

	t.Run("the decision event wakes the step without the scan", func(t *testing.T) {
		b := newGBus(t, e)
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		b.drain(10 * time.Second)
		b.pump(10*time.Second, "the step to progress", func() bool { return stepStatus(t, e, run, "review") == execution.StepSucceeded })
		if rep := reconcile(t, e, tenant); rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("the scan found more to do after the wakeup: %+v", rep)
		}
		if in := e.MustClaim(tenant); in.StepID != "deploy" {
			t.Fatalf("claimed %s", in.StepID)
		}
	})

	t.Run("duplicate, republished and forged notifications converge and grant nothing", func(t *testing.T) {
		b := newGBus(t, e)
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		// A second, undecided gate: a notification claiming it was approved is
		// not a decision.
		p2, run2 := reviewed(t, e)
		_ = p2
		e.X.Sources.Tip = gatestest.CommitA
		in2 := e.MustClaim(tenant)
		e.Finish(in2, "", gatestest.Output{Name: "report", Data: []byte("ok")})
		reconcile(t, e, tenant)
		pending := gateOf(t, e, run2, "review")

		if err := storage.WithTenant(ctx, e.X.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := b.store.Append(ctx, tx, decidedEvent("evt_forged_1", pending, "approved", e.X.Clock.Now()))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		b.drain(10 * time.Second)
		// The same notification again and an older one, republished without
		// broker message IDs (a retried publication after a lost acknowledgement).
		js, err := natsjs.New(b.broker.Connect(t, b.broker.Addr()))
		if err != nil {
			t.Fatal(err)
		}
		token, err := b.store.SubjectToken(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range []eventsv1.Envelope{decidedEvent("evt_forged_1", pending, "approved", e.X.Clock.Now()), decidedEvent("evt_old_1", g, "approved", e.X.Clock.Now().Add(-time.Hour))} {
			payload, err := eventsv1.Encode(env)
			if err != nil {
				t.Fatal(err)
			}
			subject, err := events.Subject(b.cfg.Namespace, token, env.Type)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := js.Publish(ctx, subject, payload); err != nil {
					t.Fatal(err)
				}
			}
		}
		b.pump(15*time.Second, "all deliveries", func() bool { return b.inbox("") >= 3 })
		if got := stepStatus(t, e, run, "review"); got != execution.StepSucceeded {
			t.Fatalf("the decided gate step is %s", got)
		}
		if h := stepHistory(e, run, "review"); h != 1 {
			t.Fatalf("%d step transitions for the gate step, want exactly one (pending to succeeded)", h)
		}
		if got := stepStatus(t, e, run2, "review"); got != execution.StepPending {
			t.Fatalf("a forged notification progressed an undecided gate: %s", got)
		}
		if cur := gateByID(t, e, pending); cur.Status != gates.StatusPending {
			t.Fatalf("a notification changed a gate: %+v", cur)
		}
		if n := attemptsOf(e, run, "review") + attemptsOf(e, run2, "review"); n != 0 {
			t.Fatalf("%d gate attempts", n)
		}
		// Reconciliation agrees with the events: nothing is left to do.
		if rep := reconcile(t, e, tenant); rep.Completed != 0 || rep.Rejected != 0 || rep.Invalidated != 0 {
			t.Fatalf("scan: %+v", rep)
		}
	})

	t.Run("a failed progression rolls back the consumer and the redelivery completes it once", func(t *testing.T) {
		b := newGBus(t, e)
		// The gate is the last step: completing it settles the run, which
		// appends a run event inside the consumer's transaction, after the step
		// and run mutations, and so can fail the whole progression.
		p := e.X.NewProject(scope)
		def := e.Define(scope,
			pipelinetest.Container("test", readRun, reportOutput),
			gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		finishTest(t, e, p)
		reconcile(t, e, tenant)
		g := gateOf(t, e, run, "review")
		if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		var eventID string
		if err := e.X.RuntimeTx(tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT event_id FROM outbox WHERE event_type = 'gate.decided' AND convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $1`, string(g.ID)).Scan(&eventID)
		}); err != nil {
			t.Fatal(err)
		}
		inbox := func(state string) int {
			return e.X.CountTenant(tenant, `SELECT count(*) FROM event_inbox WHERE consumer = $1 AND event_id = $2 AND state = $3`, gatespg.DecisionConsumerName, eventID, state)
		}
		reservationsBefore := reservations(e, run)

		// The first two deliveries fail inside the consumer's transaction, after
		// the step and run mutations. Before each later attempt, the previous
		// one must have left nothing behind.
		var rolledBack []string
		check := func(n int) {
			if n == 1 {
				return
			}
			if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
				rolledBack = append(rolledBack, fmt.Sprintf("attempt %d: the step is %s", n-1, got))
			}
			if h := stepHistory(e, run, "review"); h != 0 {
				rolledBack = append(rolledBack, fmt.Sprintf("attempt %d: %d step transitions survived", n-1, h))
			}
			if inbox("completed") != 0 {
				rolledBack = append(rolledBack, fmt.Sprintf("attempt %d: the inbox receipt completed", n-1))
			}
			if cur := gateByID(t, e, g); cur.Status != gates.StatusApproved || cur.Decision == nil {
				rolledBack = append(rolledBack, fmt.Sprintf("attempt %d: the decision is gone", n-1))
			}
		}
		flaky.arm(2, check)
		b.drain(10 * time.Second)
		b.pump(20*time.Second, "the redelivery to progress the step", func() bool { return inbox("completed") == 1 })
		if hits := flaky.disarm(); hits != 3 {
			t.Fatalf("the progression ran %d times, want 2 failures and one success", hits)
		}
		if len(rolledBack) != 0 {
			t.Fatalf("a failed progression left effects: %v", rolledBack)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepSucceeded {
			t.Fatalf("the redelivery left the step %s", got)
		}
		if got := e.Get(run).Run.Status; got != execution.RunSucceeded {
			t.Fatalf("the run is %s", got)
		}
		if h := stepHistory(e, run, "review"); h != 1 {
			t.Fatalf("%d step transitions, want exactly one", h)
		}
		if n := attemptsOf(e, run, "review"); n != 0 || reservations(e, run) != reservationsBefore {
			t.Fatalf("a gate created compute: %d attempts, reservations %d -> %d", n, reservationsBefore, reservations(e, run))
		}
		if outboxCount(e, "gate.decided", g.ID) != 1 || auditCount(e, "gate.decided", g.ID) != 1 {
			t.Fatalf("the decision was duplicated")
		}
		if rep := reconcile(t, e, tenant); rep.Completed != 0 || rep.Rejected != 0 || rep.Failed != 0 {
			t.Fatalf("reconciliation found work after the redelivery: %+v", rep)
		}
		if h := stepHistory(e, run, "review"); h != 1 {
			t.Fatalf("reconciliation repeated the transition: %d", h)
		}
	})

	t.Run("a missing wakeup strands nothing", func(t *testing.T) {
		// No bus at all: the scan alone requests, and completes, the gate.
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		if rep := reconcile(t, e, tenant); rep.Requested != 1 {
			t.Fatalf("request: %+v", rep)
		}
		g := gateOf(t, e, run, "review")
		if _, err := reject(t, e, g, gatestest.Reviewer); err != nil {
			t.Fatal(err)
		}
		if rep := reconcile(t, e, tenant); rep.Rejected != 1 {
			t.Fatalf("rejection: %+v", rep)
		}
		if got := e.Get(run); got.Step("review").Status != execution.StepFailed || got.Step("review").Reason != execution.ReasonGateRejected {
			t.Fatalf("rejected gate step %+v", got.Step("review"))
		}
		if got := e.Get(run).Run.Status; got != execution.RunFailed {
			t.Fatalf("a rejected gate leaves the run %s", got)
		}
		if rep := reconcile(t, e, tenant); rep.Rejected != 0 {
			t.Fatalf("repeat: %+v", rep)
		}
	})
}
