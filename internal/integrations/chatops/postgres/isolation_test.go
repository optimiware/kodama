package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// TestTenantIsolationAndPoolReuse: non-owner RLS, composite ownership, foreign
// references, scoped listings and pooled-connection context isolation. Real
// PostgreSQL.
func TestTenantIsolationAndPoolReuse(t *testing.T) {
	ctx := context.Background()
	e := chatopstest.New(t, chatopstest.Options{})
	scopeB, scopeA2 := chatopstest.ScopeB, chatopstest.ScopeA2
	e.SeedProject(scopeA)
	e.SeedProject(scopeA2)
	e.SeedProject(scopeB)
	a, a2, b := e.Actors(scopeA, "a"), e.Actors(scopeA2, "a2"), e.Actors(scopeB, "b")

	dA, idsA := seed(t, e, a, "alpha", 2)
	dA2 := e.Destination(a2.Admin, scopeA2, "alpha", "https://hooks.t041.example/a2") // the same name in another project is fine
	dB, idsB := func() (chatops.Destination, []chatops.DeliveryID) {
		d := e.Destination(b.Admin, scopeB, "beta", "https://hooks.t041.example/b")
		var ids []chatops.DeliveryID
		for i, evID := range []string{"evt_b_1", "evt_b_2", "evt_b_3"} {
			ev := chatopstest.WorkItem(scopeB, evID, "wi_b", int64(i+1))
			e.Append(ev)
			process(t, e, ev)
			ids = append(ids, chatops.DeliveryIDFor(scopeB, d.ID, evID))
		}
		return d, ids
	}()

	t.Run("row level security shows each tenant only its own rows to the runtime role", func(t *testing.T) {
		count := func(tenant domain.TenantID, sql string) int {
			var n int
			err := storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql).Scan(&n) })
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		for table, want := range map[string][2]int{
			"chatops_destinations":      {2, 1}, // tenant A has two destinations (two projects), B one
			"chatops_deliveries":        {2, 3},
			"chatops_delivery_attempts": {0, 0},
		} {
			if got := count(scopeA.TenantID, "SELECT count(*) FROM "+table); got != want[0] {
				t.Errorf("%s as tenant A = %d, want %d", table, got, want[0])
			}
			if got := count(scopeB.TenantID, "SELECT count(*) FROM "+table); got != want[1] {
				t.Errorf("%s as tenant B = %d, want %d", table, got, want[1])
			}
		}
		// Without a tenant context the runtime role sees nothing (or fails closed).
		var n int
		err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM chatops_deliveries`).Scan(&n)
		if err == nil && n != 0 {
			t.Fatalf("a connection without tenant context saw %d deliveries", n)
		}
		// A write for another tenant is rejected by the policy.
		err = storage.WithTenant(ctx, e.Runtime, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO chatops_destinations (tenant_id, project_id, id, name, endpoint, events, credential, enabled, binding_revision, version, created_at, updated_at)
VALUES ($1, $2, 'cdst_forged', 'forged', 'https://x.example/h', ARRAY['gate.requested'], '', true, 1, 1, now(), now())`, string(scopeB.TenantID), string(scopeB.ProjectID))
			return err
		})
		if err == nil {
			t.Fatal("tenant A's context wrote a tenant B destination")
		}
	})

	t.Run("composite ownership rejects cross-tenant and cross-project references", func(t *testing.T) {
		exec := func(tenant domain.TenantID, sql string, args ...any) error {
			return storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err })
		}
		const insert = `INSERT INTO chatops_deliveries (tenant_id, project_id, id, destination_id, event_id, event_type, binding_revision, payload, payload_digest, state,
next_attempt_at, version, created_at, updated_at) VALUES ($1, $2, 'cdl_forged', $3, 'evt_x', 'gate.requested', 1, '{}'::bytea, 'sha256:' || repeat('a', 64), 'pending', now(), 1, now(), now())`
		// Tenant A's delivery naming tenant B's destination, and a same-tenant destination of another project.
		if err := exec(scopeA.TenantID, insert, string(scopeA.TenantID), string(scopeA.ProjectID), string(dB.ID)); err == nil {
			t.Error("a delivery referenced another tenant's destination")
		}
		if err := exec(scopeA.TenantID, insert, string(scopeA.TenantID), string(scopeA.ProjectID), string(dA2.ID)); err == nil {
			t.Error("a delivery referenced another project's destination")
		}
		// A destination whose project belongs to another tenant.
		err := exec(scopeA.TenantID, `INSERT INTO chatops_destinations (tenant_id, project_id, id, name, endpoint, events, credential, enabled, binding_revision, version, created_at, updated_at)
VALUES ($1, $2, 'cdst_cross', 'cross', 'https://x.example/h', ARRAY['gate.requested'], '', true, 1, 1, now(), now())`, string(scopeA.TenantID), string(scopeB.ProjectID))
		if err == nil {
			t.Error("a destination was attached to another tenant's project")
		}
		// An attempt row for a foreign delivery.
		err = exec(scopeA.TenantID, `INSERT INTO chatops_delivery_attempts (tenant_id, project_id, delivery_id, attempt, claim_token, started_at) VALUES ($1, $2, $3, 1, 1, now())`,
			string(scopeA.TenantID), string(scopeA.ProjectID), string(idsB[0]))
		if err == nil {
			t.Error("an attempt was recorded for another tenant's delivery")
		}
	})

	t.Run("foreign and same-tenant cross-project references are indistinguishable from absent ones", func(t *testing.T) {
		missing := chatops.DeliveryID("cdl_does_not_exist")
		probes := map[string]error{}
		_, probes["B reads A's delivery in A's scope"] = e.Svc.GetDelivery(ctx, b.Viewer, scopeA, idsA[0])
		_, probes["B reads A's delivery in B's scope"] = e.Svc.GetDelivery(ctx, b.Viewer, scopeB, idsA[0])
		_, probes["A reads B's delivery in A's scope"] = e.Svc.GetDelivery(ctx, a.Viewer, scopeA, idsB[0])
		_, probes["A reads a delivery of project 2"] = e.Svc.GetDelivery(ctx, a.Viewer, scopeA2, idsA[0])
		_, probes["A2 admin reads A's destination"] = e.Svc.GetDestination(ctx, a2.Admin, scopeA, dA.ID)
		_, probes["A admin reads A2's destination in A"] = e.Svc.GetDestination(ctx, a.Admin, scopeA, dA2.ID)
		_, probes["A admin reads A2's destination in A2"] = e.Svc.GetDestination(ctx, a.Admin, scopeA2, dA2.ID)
		_, probes["A admin reads B's destination"] = e.Svc.GetDestination(ctx, a.Admin, scopeA, dB.ID)
		_, probes["B admin reads A's destination in B's scope"] = e.Svc.GetDestination(ctx, b.Admin, scopeB, dA.ID)
		_, probes["A viewer lists project 2's deliveries"] = e.Svc.ListDeliveries(ctx, a.Viewer, scopeA2, chatops.DeliveryFilter{}, chatops.DeliveryPosition{}, 10)
		_, probes["A reads a missing delivery"] = e.Svc.GetDelivery(ctx, a.Viewer, scopeA, missing)
		_, probes["replay across tenants"] = e.Svc.ReplayDelivery(ctx, b.Admin, scopeA, idsA[0], 1, "x")
		_, probes["update across projects"] = e.Svc.UpdateDestination(ctx, a.Admin, scopeA2, dA2.ID, 1, chatops.DestinationEdit{Name: ptr("renamed")})
		for name, err := range probes {
			if !errors.Is(err, chatops.ErrNotFound) {
				t.Errorf("%s: %v, want not found", name, err)
			}
		}
		// The messages are the same fixed text whatever the cause.
		first := probes["A reads a missing delivery"]
		for name, err := range probes {
			if err.Error() != first.Error() && !errors.Is(err, chatops.ErrDenied) {
				t.Errorf("%s: %q differs from the missing-delivery answer %q", name, err, first)
			}
		}
	})

	t.Run("listings are scoped to the project and keyset-paged", func(t *testing.T) {
		all, err := e.Svc.ListDeliveries(ctx, a.Viewer, scopeA, chatops.DeliveryFilter{}, chatops.DeliveryPosition{}, 10)
		if err != nil || len(all) != 2 {
			t.Fatalf("list = %d %v", len(all), err)
		}
		var seen []chatops.DeliveryID
		var pos chatops.DeliveryPosition
		for {
			page, err := e.Svc.ListDeliveries(ctx, a.Viewer, scopeA, chatops.DeliveryFilter{}, pos, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].ID)
			pos = chatops.DeliveryPosition{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
		}
		if len(seen) != 2 || seen[0] == seen[1] {
			t.Fatalf("paged = %v", seen)
		}
		// A position taken in one project applied to another returns only the latter's rows.
		other, err := e.Svc.ListDeliveries(ctx, b.Viewer, scopeB, chatops.DeliveryFilter{}, pos, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range other {
			if d.Scope != scopeB {
				t.Fatalf("a foreign row leaked through a cursor position: %+v", d)
			}
		}
		// Filters.
		pend, _ := e.Svc.ListDeliveries(ctx, a.Viewer, scopeA, chatops.DeliveryFilter{State: chatops.StateFailed}, chatops.DeliveryPosition{}, 10)
		if len(pend) != 0 {
			t.Fatalf("failed filter returned %d", len(pend))
		}
		byDest, _ := e.Svc.ListDeliveries(ctx, a.Viewer, scopeA, chatops.DeliveryFilter{Destination: dA.ID}, chatops.DeliveryPosition{}, 10)
		if len(byDest) != 2 {
			t.Fatalf("destination filter returned %d", len(byDest))
		}
		none, _ := e.Svc.ListDeliveries(ctx, a.Viewer, scopeA, chatops.DeliveryFilter{Destination: dA2.ID}, chatops.DeliveryPosition{}, 10)
		if len(none) != 0 {
			t.Fatalf("a foreign destination filter returned %d", len(none))
		}
	})

	t.Run("worker claims and finalization stay inside the tenant", func(t *testing.T) {
		batch, err := e.Store.ClaimDue(ctx, scopeB.TenantID, now(), time.Minute, 50, policy)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Claims) != 3 {
			t.Fatalf("tenant B claimed %d", len(batch.Claims))
		}
		for _, c := range batch.Claims {
			if c.Delivery.Scope.TenantID != scopeB.TenantID || c.Destination.Scope != scopeB {
				t.Fatalf("a foreign claim: %+v", c)
			}
		}
		// A claim of tenant B cannot finalize a delivery of tenant A even when forged.
		forged := batch.Claims[0]
		forged.Delivery.ID = idsA[0]
		if _, err := e.Store.Finish(ctx, forged, chatops.Outcome{Delivered: true}, now(), policy); !errors.Is(err, chatops.ErrNotFound) {
			t.Fatalf("forged finish = %v", err)
		}
		if d := e.Delivery(scopeA, idsA[0]); d.State != chatops.StatePending {
			t.Fatalf("tenant A's delivery moved: %+v", d)
		}
	})

	t.Run("pooled connection reuse never leaks the previous tenant context", func(t *testing.T) {
		pool := e.DB.RuntimePool(ctx, 1) // a single connection forces reuse across tenants
		s, err := chatopspg.NewStore(chatopspg.Config{DB: pool, Authorizer: chatopspg.IdentityAuthorizer{}})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 6; i++ {
			scope, who, want := scopeA, a.Viewer, 2
			if i%2 == 1 {
				scope, who, want = scopeB, b.Viewer, 3
			}
			got, err := s.ListDeliveries(ctx, who, scope, chatops.DeliveryFilter{}, chatops.DeliveryPosition{}, 20)
			if err != nil || len(got) != want {
				t.Fatalf("round %d: %d rows, want %d (%v)", i, len(got), want, err)
			}
			// Between transactions the connection carries no tenant.
			var n int
			qerr := pool.QueryRow(ctx, `SELECT count(*) FROM chatops_deliveries`).Scan(&n)
			if qerr == nil && n != 0 {
				t.Fatalf("round %d: the reused connection saw %d rows without a tenant context", i, n)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }
