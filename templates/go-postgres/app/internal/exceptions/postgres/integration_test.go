package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.invalid/inventory/internal/exceptions"
	expg "example.invalid/inventory/internal/exceptions/postgres"
	"example.invalid/inventory/internal/tenancy"
	tpg "example.invalid/inventory/internal/tenancy/postgres"
	"example.invalid/inventory/migrations"
	"example.invalid/inventory/test/support/pgtest"
)

const (
	tenantA = tenancy.TenantID("11111111-1111-4111-8111-111111111111")
	tenantB = tenancy.TenantID("22222222-2222-4222-8222-222222222222")
)

type env struct {
	db      *pgtest.Database
	owner   *pgxpool.Pool
	runtime *pgxpool.Pool
	store   *expg.Store
	svc     *exceptions.Service
}

func setup(t *testing.T, runtimeConns int32) env {
	t.Helper()
	ctx := context.Background()
	db := pgtest.New(t)
	conn, err := pgx.Connect(ctx, db.MigrationURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	owner := db.OwnerPool(ctx, 4)
	prov, _ := tpg.NewProvisioner(owner)
	for tenant, name := range map[tenancy.TenantID]string{tenantA: "Acme", tenantB: "Globex"} {
		if err := prov.Seed(ctx, tenant, name, []tpg.Member{{Subject: "rita", Role: tenancy.RoleReviewer}, {Subject: "worker-1", Role: tenancy.RoleWorker}}); err != nil {
			t.Fatal(err)
		}
	}
	rt := db.RuntimePool(ctx, runtimeConns)
	store, err := expg.NewStore(rt)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := exceptions.NewService(exceptions.Config{Store: store, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	return env{db: db, owner: owner, runtime: rt, store: store, svc: svc}
}

func acc(t *testing.T, tenant tenancy.TenantID, subject string, role tenancy.Role) tenancy.Access {
	t.Helper()
	a, err := tenancy.NewAccess(tenant, subject, role)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e env) create(t *testing.T, tenant tenancy.TenantID, ref string) exceptions.Exception {
	t.Helper()
	x, err := e.svc.Create(context.Background(), acc(t, tenant, "rita", tenancy.RoleReviewer), exceptions.CreateInput{ERPRef: ref, ERPQuantity: 10, CountedQuantity: 7})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func count(t *testing.T, pool *pgxpool.Pool, tenant tenancy.TenantID, table string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if tenant != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", string(tenant)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTenantTransactions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("runtime role is neither owner nor bypasses row-level security", func(t *testing.T) {
		e := setup(t, 2)
		var super, bypass bool
		if err := e.runtime.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&super, &bypass); err != nil {
			t.Fatal(err)
		}
		if super || bypass {
			t.Fatalf("runtime role must be NOSUPERUSER NOBYPASSRLS: %v %v", super, bypass)
		}
		var unprotected int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> 'schema_migrations' AND NOT (c.relrowsecurity AND c.relforcerowsecurity)`).Scan(&unprotected); err != nil {
			t.Fatal(err)
		}
		if unprotected != 0 {
			t.Fatalf("%d tables lack forced RLS", unprotected)
		}
	})

	t.Run("direct queries see only the tenant context, and never without one", func(t *testing.T) {
		e := setup(t, 2)
		e.create(t, tenantA, "A/1")
		e.create(t, tenantB, "B/1")
		e.create(t, tenantB, "B/2")
		if n := count(t, e.runtime, "", "exceptions"); n != 0 {
			t.Fatalf("no tenant context must expose nothing, saw %d", n)
		}
		if n := count(t, e.runtime, tenantA, "exceptions"); n != 1 {
			t.Fatalf("tenant A sees %d", n)
		}
		if n := count(t, e.runtime, tenantB, "exceptions"); n != 2 {
			t.Fatalf("tenant B sees %d", n)
		}
		// Writing a row for another tenant is refused by the policy itself.
		tx, _ := e.runtime.Begin(ctx)
		defer tx.Rollback(ctx)
		_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", string(tenantA))
		if _, err := tx.Exec(ctx, `INSERT INTO exceptions (tenant_id, id, erp_ref, erp_quantity, counted_quantity, state, version, created_by, created_at, updated_at)
			VALUES ($1, $2, 'x', 1, 2, 'open', 1, 'a', now(), now())`, string(tenantB), string(exceptions.NewID())); err == nil {
			t.Fatal("a cross-tenant insert must violate row-level security")
		}
	})

	t.Run("service paths cannot read or write across tenants", func(t *testing.T) {
		e := setup(t, 2)
		x := e.create(t, tenantB, "B/1")
		a := acc(t, tenantA, "rita", tenancy.RoleAdmin)
		if _, err := e.svc.Get(ctx, a, x.ID); !errors.Is(err, exceptions.ErrNotFound) {
			t.Fatalf("cross-tenant read: %v", err)
		}
		if _, err := e.svc.Resolve(ctx, a, x.ID, 7, "r", "key-00000001", 1); !errors.Is(err, exceptions.ErrNotFound) {
			t.Fatalf("cross-tenant write: %v", err)
		}
		if _, err := e.svc.History(ctx, a, x.ID); !errors.Is(err, exceptions.ErrNotFound) {
			t.Fatalf("cross-tenant history: %v", err)
		}
		page, err := e.svc.List(ctx, a, exceptions.ListQuery{})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("cross-tenant list: %v %d", err, len(page.Items))
		}
	})

	t.Run("composite keys refuse foreign references", func(t *testing.T) {
		e := setup(t, 2)
		x := e.create(t, tenantB, "B/1")
		tx, _ := e.owner.Begin(ctx)
		defer tx.Rollback(ctx)
		_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", string(tenantA))
		_, err := tx.Exec(ctx, `INSERT INTO resolution_intents (tenant_id, id, exception_id, operation_key, payload_hash, erp_ref, adjusted_quantity, reason, requested_by, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'op', 'h', 'r', 1, 'x', 'a', 'queued', now(), now())`, string(tenantA), string(exceptions.NewID()), string(x.ID))
		if err == nil {
			t.Fatal("an intent in tenant A must not reference tenant B's exception")
		}
	})

	t.Run("pooled connections keep no tenant context between callers", func(t *testing.T) {
		e := setup(t, 1) // one connection forces reuse
		e.create(t, tenantA, "A/1")
		var v *string
		if err := e.runtime.QueryRow(ctx, "SELECT nullif(current_setting('app.tenant_id', true), '')").Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("tenant context leaked to the next pooled caller: %q", *v)
		}
		if n := count(t, e.runtime, "", "exceptions"); n != 0 {
			t.Fatalf("leaked rows: %d", n)
		}
		// A failing transaction also leaves nothing behind.
		bad := acc(t, tenantB, "rita", tenancy.RoleReviewer)
		_, _ = e.svc.Get(ctx, bad, exceptions.NewID())
		if err := e.runtime.QueryRow(ctx, "SELECT nullif(current_setting('app.tenant_id', true), '')").Scan(&v); err != nil || v != nil {
			t.Fatalf("context after a failed call: %v %v", v, err)
		}
	})

	t.Run("concurrent resolutions are serialized to one intent and one effect", func(t *testing.T) {
		e := setup(t, 8)
		x := e.create(t, tenantA, "A/1")
		a := acc(t, tenantA, "rita", tenancy.RoleReviewer)
		var accepted, replayed, rejected atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := "key-same-0001"
				if i%3 == 0 {
					key = "key-other-" + string(rune('a'+i))
					key += "0000"
				}
				r, err := e.svc.Resolve(ctx, a, x.ID, 7, "recount", key, 1)
				switch {
				case err == nil && r.Replayed:
					replayed.Add(1)
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, exceptions.ErrInvalidState), errors.Is(err, exceptions.ErrVersionConflict):
					rejected.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}(i)
		}
		wg.Wait()
		if accepted.Load() != 1 {
			t.Fatalf("accepted=%d replayed=%d rejected=%d", accepted.Load(), replayed.Load(), rejected.Load())
		}
		if n := count(t, e.runtime, tenantA, "resolution_intents"); n != 1 {
			t.Fatalf("exactly one durable intent, got %d", n)
		}
		got, _ := e.svc.Get(ctx, a, x.ID)
		if got.State != exceptions.StatePending || got.Version != 2 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("an audit failure rolls back state, intent and idempotency record", func(t *testing.T) {
		e := setup(t, 4)
		x := e.create(t, tenantA, "A/1")
		a := acc(t, tenantA, "rita", tenancy.RoleReviewer)
		for _, q := range []string{
			`CREATE FUNCTION fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`,
			`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'resolution_requested') EXECUTE FUNCTION fail_audit()`,
		} {
			if _, err := e.owner.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.svc.Resolve(ctx, a, x.ID, 7, "recount", "key-00000001", 1); err == nil {
			t.Fatal("expected the audit failure to fail the command")
		}
		got, _ := e.svc.Get(ctx, a, x.ID)
		if got.State != exceptions.StateOpen || got.Version != 1 || got.Resolution != nil {
			t.Fatalf("state leaked past the failed audit: %+v", got)
		}
		if n := count(t, e.runtime, tenantA, "resolution_intents") + count(t, e.runtime, tenantA, "idempotency_records"); n != 0 {
			t.Fatalf("intent/idempotency leaked: %d", n)
		}
		if _, err := e.owner.Exec(ctx, "DROP TRIGGER fail_audit ON audit_events"); err != nil {
			t.Fatal(err)
		}
		r, err := e.svc.Resolve(ctx, a, x.ID, 7, "recount", "key-00000001", 1)
		if err != nil || r.Replayed || r.Exception.State != exceptions.StatePending {
			t.Fatalf("the key must not have been consumed by the failed attempt: %v %+v", err, r)
		}
		hist, _ := e.svc.History(ctx, a, x.ID)
		if len(hist) != 2 || hist[1].Action != "resolution_requested" || hist[1].Actor != "rita" {
			t.Fatalf("history: %+v", hist)
		}
	})

	t.Run("the audit history is append-only", func(t *testing.T) {
		e := setup(t, 2)
		e.create(t, tenantA, "A/1")
		tx, _ := e.runtime.Begin(ctx)
		defer tx.Rollback(ctx)
		_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", string(tenantA))
		if _, err := tx.Exec(ctx, "DELETE FROM audit_events"); err == nil {
			t.Fatal("the runtime role must not delete history")
		}
		tx2, _ := e.owner.Begin(ctx)
		defer tx2.Rollback(ctx)
		_, _ = tx2.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", string(tenantA))
		if _, err := tx2.Exec(ctx, "UPDATE audit_events SET actor = 'x'"); err == nil {
			t.Fatal("even the owner cannot rewrite history")
		}
	})

	t.Run("only one worker claims an intent and observations are fenced by the lease", func(t *testing.T) {
		e := setup(t, 8)
		x := e.create(t, tenantA, "A/1")
		human := acc(t, tenantA, "rita", tenancy.RoleReviewer)
		worker := acc(t, tenantA, "worker-1", tenancy.RoleWorker)
		if _, err := e.svc.Resolve(ctx, human, x.ID, 7, "recount", "key-00000001", 1); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		var claims atomic.Int32
		var winner atomic.Value
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				owner := string(rune('a' + i))
				c, err := e.store.Claim(ctx, worker, owner, time.Minute, now, 3)
				if err != nil {
					t.Error(err)
				}
				if c != nil {
					claims.Add(1)
					winner.Store(c)
				}
			}(i)
		}
		wg.Wait()
		if claims.Load() != 1 {
			t.Fatalf("exactly one claim, got %d", claims.Load())
		}
		c := winner.Load().(*exceptions.Claim)
		if c.Reconcile {
			t.Fatal("a first claim is not a reconciliation")
		}
		// A stranger cannot report an uncertain outcome for a lease it does not hold.
		_, err := e.store.Observe(ctx, worker, exceptions.Observation{IntentID: c.Intent.ID, OperationKey: c.Intent.OperationKey, Owner: "stranger", Kind: exceptions.ObservedUnknown, Detail: "x", At: now})
		if !errors.Is(err, exceptions.ErrLeaseLost) {
			t.Fatalf("want ErrLeaseLost, got %v", err)
		}
		// A wrong operation key is refused.
		_, err = e.store.Observe(ctx, worker, exceptions.Observation{IntentID: c.Intent.ID, OperationKey: "other", Owner: c.Owner, Kind: exceptions.ObservedAcknowledged, Reference: "r", At: now})
		if !errors.Is(err, exceptions.ErrOperationMismatch) {
			t.Fatalf("want ErrOperationMismatch, got %v", err)
		}
		// Unknown, then reclaimed for reconciliation once due, then acknowledged.
		if _, err := e.store.Observe(ctx, worker, exceptions.Observation{IntentID: c.Intent.ID, OperationKey: c.Intent.OperationKey, Owner: c.Owner, Kind: exceptions.ObservedUnknown, Detail: "timeout", RetryAt: now.Add(time.Minute), At: now}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.svc.Get(ctx, human, x.ID)
		if got.State != exceptions.StateUnknown {
			t.Fatalf("unknown expected, got %s", got.State)
		}
		if c2, _ := e.store.Claim(ctx, worker, "w2", time.Minute, now, 3); c2 != nil {
			t.Fatal("not due yet")
		}
		c2, err := e.store.Claim(ctx, worker, "w2", time.Minute, now.Add(2*time.Minute), 3)
		if err != nil || c2 == nil || !c2.Reconcile || c2.Intent.Attempts != 2 {
			t.Fatalf("reconcile claim: %v %+v", err, c2)
		}
		ack := exceptions.Observation{IntentID: c2.Intent.ID, OperationKey: c2.Intent.OperationKey, Owner: "w2", Kind: exceptions.ObservedAcknowledged, Reference: "erp-1", At: now}
		done, err := e.store.Observe(ctx, worker, ack)
		if err != nil || done.State != exceptions.StateResolved {
			t.Fatalf("ack: %v %+v", err, done)
		}
		// Replaying the same fact is harmless; a contradicting terminal fact is refused.
		if _, err := e.store.Observe(ctx, worker, ack); err != nil {
			t.Fatalf("replayed ack: %v", err)
		}
		rej := ack
		rej.Kind, rej.Detail = exceptions.ObservedRejected, "no"
		if _, err := e.store.Observe(ctx, worker, rej); !errors.Is(err, exceptions.ErrInvalidState) {
			t.Fatalf("contradicting terminal fact: %v", err)
		}
		// The first dead claimant, observing late, cannot downgrade the settled intent.
		_, err = e.store.Observe(ctx, worker, exceptions.Observation{IntentID: c.Intent.ID, OperationKey: c.Intent.OperationKey, Owner: c.Owner, Kind: exceptions.ObservedUnknown, Detail: "late", At: now})
		if !errors.Is(err, exceptions.ErrInvalidState) && !errors.Is(err, exceptions.ErrLeaseLost) {
			t.Fatalf("a stale claimant must not change a settled intent: %v", err)
		}
		final, _ := e.svc.Get(ctx, human, x.ID)
		if final.State != exceptions.StateResolved {
			t.Fatalf("final state %s", final.State)
		}
	})

	t.Run("pagination is bounded and bound to the tenant", func(t *testing.T) {
		e := setup(t, 2)
		for i := 0; i < 5; i++ {
			e.create(t, tenantA, "A/"+string(rune('1'+i)))
		}
		e.create(t, tenantB, "B/1")
		a := acc(t, tenantA, "rita", tenancy.RoleViewer)
		var seen []exceptions.ID
		var after *exceptions.Position
		for {
			p, err := e.svc.List(ctx, a, exceptions.ListQuery{Limit: 2, After: after})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) > 2 {
				t.Fatalf("page exceeds the limit: %d", len(p.Items))
			}
			for _, it := range p.Items {
				if it.Tenant != tenantA {
					t.Fatalf("foreign row on a page: %+v", it)
				}
				seen = append(seen, it.ID)
			}
			if p.Next == nil {
				break
			}
			after = p.Next
		}
		if len(seen) != 5 {
			t.Fatalf("saw %d rows", len(seen))
		}
	})
}
