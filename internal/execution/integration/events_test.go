package integration_test

import (
	"context"
	"encoding/json"
	natsjs "github.com/nats-io/nats.go/jetstream"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// cancelEvent is the work module's committed work_item.transitioned to cancelled.
func cancelEvent(id string, scope domain.Scope, item domain.WorkItemID, version int64, at time.Time) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.WorkItemTransitioned{From: "active", To: "cancelled", TransitionID: "tr_" + id})
	return eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeWorkItemTransitioned, OccurredAt: at.UTC().Truncate(time.Microsecond),
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, Resource: eventsv1.Resource{Type: "work_item", ID: string(item), Version: version},
		Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_engineer"}, CorrelationID: "req_" + id, Data: data,
	}
}

// bus is the real event bus (PostgreSQL outbox/inbox, NATS JetStream) wired with
// the execution cancellation consumer of the work module's binding.
type bus struct {
	t      testing.TB
	e      *executiontest.Env
	store  *eventspg.Store
	broker *eventstest.Broker
	cfg    jetstream.Config
	proxy  *eventstest.Proxy
	pub    *events.Publisher
	cons   *jetstream.Consumer
}

func newBus(t testing.TB, e *executiontest.Env) *bus {
	t.Helper()
	broker := eventstest.NewBroker(t)
	binding, err := workpg.CancellationBinding(workpg.CancellationConsumerName, adapters.TxCancellationRequester(e.Store))
	if err != nil {
		t.Fatal(err)
	}
	opts := eventstest.DefaultOptions()
	opts.Clock = e.Clock.Now
	b := &bus{t: t, e: e, broker: broker, cfg: broker.StreamConfig(t, time.Minute)}
	b.store = e.Base.NewStore(t, opts, binding)
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
		Retry: events.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3}, Clock: e.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	b.cons, err = jetstream.NewConsumer(broker.Connect(t, broker.Addr()), b.cfg, jetstream.ConsumerConfig{
		Consumer: workpg.CancellationConsumerName, Durable: "t020_cancel_" + eventstest.RandomHex(t, 3),
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

// commitCancellation is the work module's transaction: the cancelled state and its
// event commit together, whatever the broker is doing.
func (b *bus) commitCancellation(scope domain.Scope, item domain.WorkItemID, eventID string) (int64, eventsv1.Envelope) {
	b.t.Helper()
	var version int64
	var committed eventsv1.Envelope
	err := storage.WithTenant(context.Background(), b.e.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE work_items SET state = 'cancelled', version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3 RETURNING version`,
			string(scope.TenantID), string(scope.ProjectID), string(item)).Scan(&version); err != nil {
			return err
		}
		committed = cancelEvent(eventID, scope, item, version, b.e.Clock.Now())
		_, err := b.store.Append(ctx, tx, committed)
		return err
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return version, committed
}

func (b *bus) drain(timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, _ = b.pub.RunOnce(context.Background())
		if b.e.CountTenant(executiontest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE state <> 'published'`) == 0 {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("outbox not drained within %s", timeout)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (b *bus) pump(timeout time.Duration, what string, cond func() bool) {
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

func cancelRequests(e *executiontest.Env, item domain.WorkItemID) int {
	return e.Count(`SELECT count(*) FROM run_history h JOIN runs r ON r.id = h.run_id WHERE r.work_item_id = $1 AND h.reason = 'cancel_requested'`, string(item))
}

// Test_event_recovery: PX-05, PX-12, PX-A03. Duplicate and reordered delivery, a
// broker outage and scan recovery converge without duplicate attempts or a lost
// cancellation. Real PostgreSQL and NATS.
func Test_event_recovery(t *testing.T) {
	ctx := context.Background()
	scope := executiontest.ScopeA

	setup := func(t *testing.T) (*executiontest.Env, *bus, executiontest.Project, executiontest.Project, *execution.Run, *execution.Run, *execution.Run, *execution.AttemptSpec) {
		e := executiontest.New(t)
		e.GrantAll()
		b := newBus(t, e)
		p := e.NewProject(scope)
		other := e.NewProject(scope)
		def := e.Define(scope, pipelinetest.Doc(pipelinetest.Container("a", "workspace: {access: read, from: run}"), pipelinetest.Container("b", "needs: [a]", "workspace: {access: read, from: run}")))
		running := e.MustAdmit(p, def, nil)
		in := e.MustClaim(scope.TenantID)
		e.Start(in)
		queued := e.MustAdmit(p, def, nil)
		bystander := e.MustAdmit(other, def, nil)
		return e, b, p, other, running, queued, bystander, in
	}

	t.Run("a broker outage loses no cancellation: scan recovery, then late delivery converges", func(t *testing.T) {
		e, b, p, _, running, queued, bystander, in := setup(t)
		b.proxy.Stop() // the broker is unreachable for the whole transition
		version, _ := b.commitCancellation(scope, p.Item, "evt_cancel_1")
		_, _ = b.pub.RunOnce(ctx) // fails: the broker is unreachable; the event stays in the outbox
		if n := e.CountTenant(scope.TenantID, `SELECT count(*) FROM outbox WHERE event_id = 'evt_cancel_1' AND state <> 'published'`); n != 1 {
			t.Fatalf("the cancellation event was not retained in the outbox: %d", n)
		}
		// Committed cancelled state is the durable intent; the periodic scan finds it.
		if n, err := e.Svc.ReconcileCancellations(ctx, scope.TenantID); err != nil || n != 1 {
			t.Fatalf("scan: %d %v", n, err)
		}
		for _, r := range []*execution.Run{running, queued} {
			got := e.Get(r).Run
			if got.CancelRequestedAt == nil || got.CancelReason != execution.ReasonWorkCancelled {
				t.Fatalf("run %s: cancel %v/%s", r.ID, got.CancelRequestedAt, got.CancelReason)
			}
		}
		if e.Get(queued).Run.Status != execution.RunCancelled {
			t.Fatal("the queued run with nothing admitted was not cancelled")
		}
		if agg := e.Get(running); agg.Control(in.AttemptID).Live() || agg.Attempt(in.AttemptID).Status != execution.AttemptCancelled {
			t.Fatal("the running attempt kept its authority")
		}
		if e.Get(bystander).Run.CancelRequestedAt != nil {
			t.Fatal("another work item's run was cancelled")
		}
		if _, err := e.Claim(scope.TenantID); err != nil {
			// The bystander's run is the only runnable work.
			t.Logf("bystander claim: %v", err)
		}
		before := cancelRequests(e, p.Item)
		// The broker recovers; the late event arrives and changes nothing more.
		b.proxy.Start()
		b.drain(10 * time.Second)
		b.pump(10*time.Second, "the cancellation consumer", func() bool {
			return e.CountTenant(scope.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = $1`, workpg.CancellationConsumerName) >= 1
		})
		if got := cancelRequests(e, p.Item); got != before {
			t.Fatalf("the late event cancelled again: %d -> %d", before, got)
		}
		if n := e.Count(`SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1 AND version = $2`, string(p.Item), version); n != 1 {
			t.Fatalf("%d intent rows at version %d", n, version)
		}
	})

	t.Run("duplicate and reordered deliveries converge", func(t *testing.T) {
		e, b, p, _, running, queued, _, in := setup(t)
		version, committed := b.commitCancellation(scope, p.Item, "evt_cancel_2")
		b.drain(10 * time.Second)
		// An older cancellation of the same item, produced earlier and delivered
		// late (reordered): it has an origin like every real event.
		stale := cancelEvent("evt_cancel_0", scope, p.Item, version-1, e.Clock.Now().Add(time.Second))
		if err := storage.WithTenant(ctx, e.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := b.store.Append(ctx, tx, stale)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		b.drain(10 * time.Second)
		// The same cancellation again and an older one, published without broker
		// message IDs so the broker cannot hide the duplicates.
		for _, env := range []eventsv1.Envelope{committed, stale} {
			payload, err := eventsv1.Encode(env)
			if err != nil {
				t.Fatal(err)
			}
			token, err := b.store.SubjectToken(ctx, scope.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			subject, err := events.Subject(b.cfg.Namespace, token, env.Type)
			if err != nil {
				t.Fatal(err)
			}
			js, err := natsJS(b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := js.Publish(ctx, subject, payload); err != nil {
				t.Fatal(err)
			}
		}
		// Meanwhile dispatch keeps racing the consumer.
		var wg sync.WaitGroup
		for range 6 {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = e.Claim(scope.TenantID) }()
		}
		defer func() {
			if t.Failed() {
				_ = e.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
					rows, err := tx.Query(ctx, `SELECT event_id, consumer, state FROM event_inbox`)
					if err != nil {
						t.Logf("inbox: %v", err)
						return nil
					}
					defer rows.Close()
					for rows.Next() {
						var a, c, st string
						_ = rows.Scan(&a, &c, &st)
						t.Logf("inbox %s %s %s", a, c, st)
					}
					qr, err := tx.Query(ctx, `SELECT coalesce(event_id, ''), reason_code, diagnostic FROM event_quarantine`)
					if err != nil {
						return nil
					}
					defer qr.Close()
					for qr.Next() {
						var a, c, d string
						_ = qr.Scan(&a, &c, &d)
						t.Logf("quarantine %s %s %s", a, c, d)
					}
					return nil
				})
			}
		}()
		b.pump(15*time.Second, "all deliveries", func() bool {
			return e.CountTenant(scope.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = $1`, workpg.CancellationConsumerName) >= 2
		})
		wg.Wait()
		for _, r := range []*execution.Run{running, queued} {
			if got := e.Get(r).Run.CancelRequestedAt; got == nil {
				t.Fatalf("run %s was not cancelled", r.ID)
			}
		}
		if n := cancelRequests(e, p.Item); n != 2 {
			t.Fatalf("%d cancellation requests recorded for 2 runs: duplicates were applied again", n)
		}
		if n := attemptCountOf(e, running.ID); n != 1 {
			t.Fatalf("%d attempts for the running run", n)
		}
		if n := attemptCountOf(e, queued.ID); n != 0 {
			t.Fatalf("%d attempts were created for a cancelled run", n)
		}
		if agg := e.Get(running); agg.Control(in.AttemptID).Live() {
			t.Fatal("the attempt is live")
		}
		if n := e.Count(`SELECT count(*) FROM work_cancellation_requests WHERE work_item_id = $1`, string(p.Item)); n != 1 {
			t.Fatalf("%d intent rows", n)
		}
	})
}

func attemptCountOf(e *executiontest.Env, run domain.RunID) int {
	return e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run))
}

func natsJS(b *bus) (natsjs.JetStream, error) {
	return natsjs.New(b.broker.Connect(b.t, b.broker.Addr()))
}
