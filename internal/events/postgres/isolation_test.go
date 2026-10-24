package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/jetstream"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// TestTenantIsolation: non-owner runtime access, absent or pooled context,
// cross-owner project references, foreign replay requests, and forged subject or
// envelope scope all fail closed. Subject checks run against real NATS.
func TestTenantIsolation(t *testing.T) {
	ledger := postgres.Binding{
		Spec:   events.ConsumerSpec{Name: "audit", Mode: events.ModeHistorical, Effect: events.EffectLocal},
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1Envelope) error { return nil },
	}
	f := newFixture(t, nil, ledger)
	ctx := context.Background()
	f.append(eventA("iso_a", 1))
	f.append(eventB("iso_b", 1))

	t.Run("each tenant sees only its own rows", func(t *testing.T) {
		for _, table := range []string{"outbox", "event_observations", "event_scopes", "event_backlog", "event_tenant_subjects"} {
			a := f.count(tenantA, `SELECT count(*) FROM `+table)
			b := f.count(tenantB, `SELECT count(*) FROM `+table)
			if a != 1 || b != 1 {
				t.Errorf("%s: tenant A sees %d, tenant B sees %d, want 1 each", table, a, b)
			}
			if leaked := f.count(tenantA, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenantB); leaked != 0 {
				t.Errorf("%s leaked %d row(s) of another tenant under an explicit predicate", table, leaked)
			}
		}
	})

	t.Run("absent context sees and writes nothing", func(t *testing.T) {
		var n int
		if err := f.env.Runtime.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("no-context read: %d %v", n, err)
		}
		_, err := f.env.Runtime.Exec(ctx, `INSERT INTO event_backlog (tenant_id) VALUES ($1)`, tenantA)
		if pgCode(err) != "42501" {
			t.Fatalf("no-context write: %v", err)
		}
		// Append refuses a transaction without the tenant's context.
		tx, err := f.env.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := f.store.Append(ctx, tx, eventA("iso_nocontext", 1)); !errors.Is(err, events.ErrScopeMismatch) {
			t.Fatalf("append without context: %v", err)
		}
	})

	t.Run("a tenant transaction cannot append another tenant's event", func(t *testing.T) {
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.store.Append(ctx, tx, eventB("iso_cross", 1))
			return err
		})
		if !errors.Is(err, events.ErrScopeMismatch) {
			t.Fatalf("cross-tenant append: %v", err)
		}
		// Nor can an event reference another tenant's project.
		wrongProject := eventA("iso_wrongproject", 1)
		wrongProject.ProjectID = projectB
		err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.store.Append(ctx, tx, wrongProject)
			return err
		})
		if !errors.Is(err, events.ErrScopeMismatch) {
			t.Fatalf("cross-owner project: %v", err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox`) + f.count(tenantB, `SELECT count(*) FROM outbox`); got != 2 {
			t.Fatalf("refused appends left rows: %d", got)
		}
	})

	t.Run("pooled connection carries no tenant context", func(t *testing.T) {
		one := f.env.DB.RuntimePool(ctx, 1)
		for i := 0; i < 3; i++ {
			if err := storage.WithTenant(ctx, one, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				var n int
				return tx.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
			}); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := one.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("reused connection kept tenant context: %d %v", n, err)
			}
		}
	})

	t.Run("replay requests are scoped and foreign ones refused", func(t *testing.T) {
		// An authorizer that grants the operator tenant A only.
		onlyA := authorizerFunc(func(_ events.Operator, scope events.Scope) error {
			if scope.TenantID != tenantA {
				return errors.New("operator has no access to this tenant")
			}
			return nil
		})
		s := f.env.NewStore(t, withAuthorizer(onlyA), ledger)
		req := events.ReplayRequest{Scope: events.Scope{TenantID: tenantB, ProjectID: projectB}, Consumer: "audit", FromSeq: 1, ToSeq: 1, Reason: "r", Operator: events.Operator{ID: "ops_1"}}
		if _, err := s.RequestReplay(ctx, req); !errors.Is(err, events.ErrUnauthorized) {
			t.Fatalf("foreign replay request: %v", err)
		}
		if _, err := s.DryRun(ctx, req); !errors.Is(err, events.ErrUnauthorized) {
			t.Fatalf("foreign dry-run: %v", err)
		}
		// A request that names tenant A's scope cannot reach tenant B's events.
		own := events.ReplayRequest{Scope: events.Scope{TenantID: tenantA, ProjectID: projectA}, Consumer: "audit", FromSeq: 1, ToSeq: 1, Reason: "r", Operator: events.Operator{ID: "ops_1"}}
		rec, err := s.RequestReplay(ctx, own)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetReplay(ctx, tenantB, rec.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("tenant B read tenant A's replay request: %v", err)
		}
		if _, err := s.RunReplay(ctx, tenantB, rec.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("tenant B ran tenant A's replay request: %v", err)
		}
		if got := f.count(tenantB, `SELECT count(*) FROM event_inbox`); got != 0 {
			t.Fatalf("tenant B inbox changed: %d", got)
		}
	})

	t.Run("subject tokens are server-assigned, unique and resolvable", func(t *testing.T) {
		ta, err := f.store.SubjectToken(ctx, tenantA)
		if err != nil {
			t.Fatal(err)
		}
		tb, _ := f.store.SubjectToken(ctx, tenantB)
		if ta == tb || events.ValidateSubjectToken(ta) != nil || strings.Contains(ta, tenantA) {
			t.Fatalf("tokens %q %q", ta, tb)
		}
		again, _ := f.store.SubjectToken(ctx, tenantA)
		if again != ta {
			t.Fatal("token changed between calls")
		}
		if got, err := f.store.ResolveSubjectToken(ctx, tb); err != nil || got != tenantB {
			t.Fatalf("resolve: %q %v", got, err)
		}
		for _, bad := range []string{tenantA, "unmappedtokenunmappedtoken", "a.b", "*", ">", ""} {
			if _, err := f.store.ResolveSubjectToken(ctx, bad); !errors.Is(err, events.ErrUnknownSubject) {
				t.Errorf("token %q: %v", bad, err)
			}
		}
		// The token cannot be rewritten by the runtime role.
		err = f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE event_tenant_subjects SET subject_token = 'abcdefghijklmnopqrstuvwxyz' WHERE tenant_id = $1`, tenantA)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("token rewrite: %v", err)
		}
	})

	t.Run("forged subject and envelope scope fail closed on real NATS", func(t *testing.T) {
		broker := eventstest.NewBroker(t)
		cfg := broker.StreamConfig(t, time.Minute)
		nc := broker.Connect(t, broker.Addr())
		pub, err := jetstream.NewPublisher(nc, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := pub.EnsureStream(ctx); err != nil {
			t.Fatal(err)
		}
		consumer, err := jetstream.NewConsumer(nc, cfg, jetstream.ConsumerConfig{
			Consumer: "audit", Durable: "t005_iso_" + eventstest.RandomHex(t, 3), AckWait: 5 * time.Second, MaxAckPending: 20, FetchBatch: 10,
			FetchWait: 300 * time.Millisecond, RetryDelay: 100 * time.Millisecond,
		}, f.store, f.store, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := consumer.Provision(ctx); err != nil {
			t.Fatal(err)
		}
		tokenA, _ := f.store.SubjectToken(ctx, tenantA)
		tokenB, _ := f.store.SubjectToken(ctx, tenantB)
		publishRaw := func(token string, payload []byte) {
			if err := pub.Publish(ctx, events.Message{ID: eventstest.RandomHex(t, 6), SubjectToken: token, EventType: "work_item.transitioned", Data: payload}); err != nil {
				t.Fatal(err)
			}
		}
		// 1. Tenant A's envelope sent on tenant B's subject.
		forged, _ := json.Marshal(eventA("iso_forged", 1))
		publishRaw(tokenB, forged)
		// 2. A subject token no tenant owns.
		publishRaw("unmappedtokenunmappedtoken", forged)
		// 3. A subject that is not a valid token at all is refused by the publisher itself.
		if err := pub.Publish(ctx, events.Message{ID: "x", SubjectToken: "t_acme.*", EventType: "work_item.transitioned", Data: forged}); err == nil || events.ClassOf(err) != events.ClassPermanent {
			t.Fatalf("untrusted token reached the broker: %v", err)
		}
		// 4. A legitimate delivery for tenant A.
		f.append(eventA("iso_good", 2)) // produced through Append, as every real event is
		good, _ := json.Marshal(eventA("iso_good", 2))
		publishRaw(tokenA, good)

		deadline := time.Now().Add(15 * time.Second)
		for consumer.Counters().Acked+consumer.Counters().Terminated < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("deliveries not settled: %+v", consumer.Counters())
			}
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, _ = consumer.RunOnce(c)
			cancel()
		}
		if c := consumer.Counters(); c.Terminated != 1 || c.Acked != 2 {
			t.Fatalf("counters: %+v", c)
		}
		// The forged event was quarantined under the trusted (subject) tenant B,
		// never applied to tenant A, and did not consult its claimed tenant.
		if got := f.count(tenantB, `SELECT count(*) FROM event_quarantine WHERE reason_code = 'scope_mismatch' AND delivery_key = 'iso_forged'`); got != 1 {
			t.Fatalf("tenant B quarantine rows = %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_quarantine`); got != 0 {
			t.Fatalf("tenant A quarantine rows = %d", got)
		}
		if got := f.count(tenantB, `SELECT count(*) FROM event_inbox`); got != 0 {
			t.Fatalf("forged event reached tenant B's inbox: %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE event_id = 'iso_good' AND state = 'completed'`); got != 1 {
			t.Fatalf("legitimate delivery was not processed: %d", got)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE event_id = 'iso_forged'`); got != 0 {
			t.Fatalf("forged event reached tenant A's inbox")
		}
	})
}

type authorizerFunc func(events.Operator, events.Scope) error

func (f authorizerFunc) AuthorizeReplay(_ context.Context, op events.Operator, scope events.Scope, _ string) error {
	return f(op, scope)
}

func withAuthorizer(a events.ReplayAuthorizer) postgres.Options {
	o := eventstest.DefaultOptions()
	o.Authorizer = a
	return o
}
