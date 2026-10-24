package events_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// rawPublish publishes an already encoded payload to the tenant's subject
// without a broker message ID, so the broker cannot suppress duplicates.
func rawPublish(t *testing.T, b *bus, token, eventType string, payload []byte) {
	t.Helper()
	subject, err := events.Subject(b.cfg.Namespace, token, eventType)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jsConn(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(context.Background(), subject, payload); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
}

func payloadOf(t *testing.T, env eventsv1.Envelope) []byte {
	t.Helper()
	p, err := eventsv1.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ackCrashProcessor commits the real local outcome, then reports a failure to
// the transport once: a consumer killed after the local commit and before the
// acknowledgment.
type ackCrashProcessor struct {
	inner   events.Processor
	crashed atomic.Bool
}

func (p *ackCrashProcessor) Process(ctx context.Context, d events.Delivery) (events.Outcome, error) {
	out, err := p.inner.Process(ctx, d)
	if err == nil && out.Kind == events.OutcomeCompleted && p.crashed.CompareAndSwap(false, true) {
		return events.Outcome{}, errors.New("process killed after the local commit, before the acknowledgment")
	}
	return out, err
}

// Test_duplicate_consumer: concurrent deliveries and a crash after the local
// commit but before the acknowledgment produce one local effect and one durable
// inbox result, and a failed effect rolls back. Real PostgreSQL and NATS.
// PX-05, AC-08.
func Test_duplicate_consumer(t *testing.T) {
	var failOnce atomic.Bool
	failing := postgres.Binding{
		Spec: events.ConsumerSpec{Name: "flaky", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			if _, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id) VALUES ($1, 'flaky', $2)`, string(env.TenantID), env.ID); err != nil {
				return err
			}
			if failOnce.CompareAndSwap(false, true) {
				return events.Transient(errors.New("downstream not ready"))
			}
			return nil
		},
	}
	// A long backoff, so that "during the backoff" is not a timing guess.
	b := newBus(t, busOptions{bindings: []postgres.Binding{ledger("audit"), ledger("export"), failing}, mutate: func(o *postgres.Options) {
		o.Retry = events.RetryPolicy{Initial: time.Minute, Max: time.Minute, MaxAttempts: 3}
	}})

	t.Run("concurrent deliveries", func(t *testing.T) {
		env := workItemEvent("evt_concurrent", "wi_1", 1)
		b.append(env)
		d := events.Delivery{Tenant: tenantA, Consumer: "audit", Payload: payloadOf(t, env)}
		const n = 12
		var wg sync.WaitGroup
		kinds := make(chan events.OutcomeKind, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := b.store.Process(context.Background(), d)
				if err != nil {
					t.Errorf("Process: %v", err)
					return
				}
				kinds <- out.Kind
			}()
		}
		wg.Wait()
		close(kinds)
		completed, duplicate := 0, 0
		for k := range kinds {
			switch k {
			case events.OutcomeCompleted:
				completed++
			case events.OutcomeDuplicate:
				duplicate++
			default:
				t.Errorf("unexpected outcome %s", k)
			}
		}
		if completed != 1 || duplicate != n-1 {
			t.Fatalf("completed=%d duplicate=%d", completed, duplicate)
		}
		if got := b.effects("audit", "evt_concurrent"); got != 1 {
			t.Fatalf("effects = %d, want 1", got)
		}
		if got := b.count(`SELECT count(*) FROM event_inbox WHERE event_id = 'evt_concurrent' AND consumer = 'audit' AND state = 'completed'`); got != 1 {
			t.Fatalf("inbox rows = %d", got)
		}
		// A different consumer has its own identity and its own effect.
		if out, err := b.store.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "export", Payload: d.Payload}); err != nil || out.Kind != events.OutcomeCompleted {
			t.Fatalf("second consumer: %+v %v", out, err)
		}
		if got := b.effects("export", "evt_concurrent"); got != 1 {
			t.Fatalf("export effects = %d", got)
		}
	})

	t.Run("duplicates through the broker and a crash before acknowledgment", func(t *testing.T) {
		env := workItemEvent("evt_broker_dup", "wi_2", 1)
		b.append(env)
		token, err := b.store.SubjectToken(context.Background(), tenantA)
		if err != nil {
			t.Fatal(err)
		}
		payload := payloadOf(t, env)
		b.jsPublisher(b.broker.Addr()) // provisions the stream
		for i := 0; i < 3; i++ {       // three copies the broker cannot suppress
			rawPublish(t, b, token, env.Type, payload)
		}
		proc := &ackCrashProcessor{inner: b.store}
		c1 := b.consumer(b.broker.Addr(), proc, "audit")
		// A second consumer instance on the same durable would compete; use one
		// instance, plus a concurrent second process on a separate durable of the
		// same inbox identity to race deliveries.
		c2 := b.consumer(b.broker.Addr(), nil, "audit")
		// The first instance takes the first copy, commits it and "dies" before
		// acknowledging; only then do both instances race for the rest.
		pump(t, c1, 10*time.Second, "the injected crash", func() bool { return proc.crashed.Load() })
		var wg sync.WaitGroup
		for _, c := range []interface {
			RunOnce(context.Context) (int, error)
		}{c1, c2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				deadline := time.Now().Add(8 * time.Second)
				for time.Now().Before(deadline) {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					_, _ = c.RunOnce(ctx)
					cancel()
					if b.count(`SELECT count(*) FROM event_inbox WHERE event_id = 'evt_broker_dup' AND state = 'completed'`) == 1 &&
						proc.crashed.Load() && b.effects("audit", "evt_broker_dup") == 1 {
						// keep draining briefly so the redelivered message is acknowledged
						if c1.Counters().Acked+c2.Counters().Acked >= 3 {
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		if !proc.crashed.Load() {
			t.Fatal("the crash was never injected")
		}
		if got := b.effects("audit", "evt_broker_dup"); got != 1 {
			t.Fatalf("effects = %d, want exactly one", got)
		}
		if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE event_id = 'evt_broker_dup' AND consumer = 'audit' AND outcome = 'completed'`); got != 1 {
			t.Fatalf("completions recorded = %d", got)
		}
		if acked := c1.Counters().Acked + c2.Counters().Acked; acked < 3 {
			t.Fatalf("only %d of 3 broker copies were acknowledged", acked)
		}
	})

	t.Run("failed effect rolls back and a retry succeeds once", func(t *testing.T) {
		env := workItemEvent("evt_flaky", "wi_3", 1)
		b.append(env)
		d := events.Delivery{Tenant: tenantA, Consumer: "flaky", Payload: payloadOf(t, env)}
		out, err := b.store.Process(context.Background(), d)
		if err != nil || out.Kind != events.OutcomeRetry || out.RetryAfter <= 0 {
			t.Fatalf("first attempt: %+v %v", out, err)
		}
		if got := b.effects("flaky", "evt_flaky"); got != 0 {
			t.Fatalf("a failed effect left %d row(s); the handler's writes must roll back", got)
		}
		if got := b.count(`SELECT count(*) FROM event_inbox WHERE event_id = 'evt_flaky' AND state = 'pending' AND attempts = 1 AND last_failure_class = 'transient' AND next_attempt_at IS NOT NULL`); got != 1 {
			t.Fatalf("the failure was not recorded durably with its class and retry time")
		}
		// Duplicate deliveries (broker copies, a second subscription, a repair
		// pass) during the backoff must neither run the handler nor spend the
		// attempt budget: the persisted schedule is authoritative.
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dup, err := b.store.Process(context.Background(), d)
				if err != nil || dup.Kind != events.OutcomeRetry || dup.RetryAfter <= 0 || dup.RetryAfter > time.Minute {
					t.Errorf("duplicate during backoff: %+v %v", dup, err)
				}
			}()
		}
		wg.Wait()
		if got := b.count(`SELECT attempts FROM event_inbox WHERE event_id = 'evt_flaky' AND consumer = 'flaky' AND state = 'pending'`); got != 1 {
			t.Fatalf("attempts after duplicates during the backoff = %d, want 1", got)
		}
		if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE event_id = 'evt_flaky' AND consumer = 'flaky'`); got != 1 {
			t.Fatalf("history rows after duplicates during the backoff = %d, want only the failure", got)
		}
		if got := b.effects("flaky", "evt_flaky"); got != 0 {
			t.Fatalf("a duplicate during the backoff ran the handler: %d effect row(s)", got)
		}
		b.clock.Advance(2 * time.Minute) // past the scheduled retry time
		out, err = b.store.Process(context.Background(), d)
		if err != nil || out.Kind != events.OutcomeCompleted {
			t.Fatalf("retry: %+v %v", out, err)
		}
		if got := b.effects("flaky", "evt_flaky"); got != 1 {
			t.Fatalf("effects = %d after the retry", got)
		}
		if got := b.count(`SELECT count(*) FROM event_inbox_attempts WHERE event_id = 'evt_flaky' AND consumer = 'flaky'`); got != 2 {
			t.Fatalf("history rows = %d, want retry + completed", got)
		}
	})

	t.Run("permanent failure rolls back and quarantines", func(t *testing.T) {
		env := workItemEvent("evt_poison_effect", "wi_4", 1)
		b.append(env)
		panicking := postgres.Binding{
			Spec: events.ConsumerSpec{Name: "panicky", Mode: events.ModeHistorical, Effect: events.EffectLocal},
			Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
				_, _ = tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id) VALUES ($1, 'panicky', $2)`, string(env.TenantID), env.ID)
				return events.Permanent(errors.New("cannot ever apply"))
			},
		}
		s := rebuildWithConsumer(t, b, panicking)
		out, err := s.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "panicky", Payload: payloadOf(t, env)})
		if err != nil || out.Kind != events.OutcomeQuarantined || out.QuarantineID == "" {
			t.Fatalf("outcome: %+v %v", out, err)
		}
		if got := b.effects("panicky", "evt_poison_effect"); got != 0 {
			t.Fatalf("poison left %d effect row(s)", got)
		}
		// Redelivery of the poison message is acknowledged, not reprocessed.
		out, err = s.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "panicky", Payload: payloadOf(t, env)})
		if err != nil || out.Kind != events.OutcomeQuarantined {
			t.Fatalf("redelivery: %+v %v", out, err)
		}
	})
}
