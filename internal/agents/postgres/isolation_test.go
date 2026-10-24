package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
)

func TestProfileIsolation(t *testing.T) {
	e := newEnv(t)
	a := e.mustRegister(scopeA, engineer, e.draft)
	a2 := e.mustRegister(scopeA2, engineer, e.variant(2))
	b := e.mustRegister(scopeB, engineer, e.variant(3))
	ctx := context.Background()

	t.Run("the same name and content exists independently per project and tenant", func(t *testing.T) {
		same := e.mustRegister(scopeA2, engineer, e.draft)
		if !same.Created || same.Profile.ID == a.Profile.ID || same.Profile.Version != 2 || same.Profile.Scope != scopeA2 {
			t.Fatalf("a project owns its own copy and versions: %+v", same.Profile)
		}
		sameB := e.mustRegister(scopeB, engineer, e.draft)
		if !sameB.Created || sameB.Profile.Scope != scopeB || sameB.Profile.Digest != a.Profile.Digest {
			t.Fatalf("deduplication is scoped to the project: %+v", sameB.Profile)
		}
	})

	t.Run("exact reads and lists never cross scopes", func(t *testing.T) {
		got, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range got {
			if p.Scope != scopeA {
				t.Fatalf("a foreign profile leaked into the list: %+v", p.Scope)
			}
			if p.ID == a2.Profile.ID || p.ID == b.Profile.ID {
				t.Fatal("a foreign profile ID leaked")
			}
		}
		if _, err := e.store.Get(ctx, scopeA, "coding", 99); !errors.Is(err, agents.ErrNotFound) {
			t.Fatal(err)
		}
	})

	t.Run("a foreign cursor position discloses nothing", func(t *testing.T) {
		after := &agents.Position{CreatedAt: b.Profile.CreatedAt, ID: b.Profile.ID}
		rows, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 100, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			if p.Scope != scopeA {
				t.Fatalf("a foreign position leaked a row: %+v", p)
			}
		}
	})

	t.Run("row level security hides other tenants from the runtime role", func(t *testing.T) {
		err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_profiles WHERE tenant_id = $1`, tenantB).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("tenant A sees %d rows of tenant B", n)
			}
			// Even without a predicate, only tenant A's rows are visible.
			rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM agent_profiles`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var tid string
				if err := rows.Scan(&tid); err != nil {
					return err
				}
				if tid != tenantA {
					t.Errorf("row of tenant %s visible under tenant A's context", tid)
				}
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a tenant context cannot insert another tenant's profile", func(t *testing.T) {
		err := e.withTenant(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO agent_profiles (tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
				VALUES ($1, $2, 'ap_t030_forged', 'coding', 77, $3, $4, 'user', 'x', now())`,
				tenantB, projectB, string(a.Profile.Digest), string(a.Profile.Content.Canonical()))
			return err
		})
		if err == nil {
			t.Fatal("RLS must refuse a cross-tenant insert")
		}
	})

	t.Run("composite ownership: a project of another tenant is refused by the foreign key", func(t *testing.T) {
		_, err := e.owner.Exec(ctx, `INSERT INTO agent_profiles (tenant_id, project_id, id, name, version, digest, body, created_by_type, created_by_id, created_at)
			VALUES ($1, $2, 'ap_t030_cross', 'coding', 88, $3, $4, 'user', 'x', now())`,
			tenantA, projectB, string(a.Profile.Digest), string(a.Profile.Content.Canonical()))
		if err == nil || !strings.Contains(err.Error(), "foreign key") {
			t.Fatalf("a profile must belong to a project of its own tenant: %v", err)
		}
	})

	t.Run("pooled connections do not retain tenant context", func(t *testing.T) {
		// Hammer a tiny pool so connections are reused across tenants.
		small := newEnvWith(t, envOptions{conns: 2})
		ra := small.mustRegister(scopeA, engineer, small.draft)
		rb := small.mustRegister(scopeB, engineer, small.variant(1))
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scope, want := scopeA, ra.Profile.ID
				if i%2 == 1 {
					scope, want = scopeB, rb.Profile.ID
				}
				rows, err := small.store.List(context.Background(), scope, agents.ListQuery{Limit: 10})
				if err != nil || len(rows) != 1 || rows[0].ID != want {
					t.Errorf("iteration %d: %v %v", i, rows, err)
				}
			}()
		}
		wg.Wait()
		// After the work, a bare pooled connection holds no tenant and sees no row.
		var setting string
		var visible int
		if err := small.rt.QueryRow(context.Background(), `SELECT coalesce(current_setting('kodama.tenant_id', true), ''),
			(SELECT count(*) FROM agent_profiles)`).Scan(&setting, &visible); err != nil || setting != "" || visible != 0 {
			t.Fatalf("tenant context leaked into a pooled session: %q %d %v", setting, visible, err)
		}
	})

	t.Run("invalid scopes are rejected before any query", func(t *testing.T) {
		if _, err := e.store.List(ctx, domain.Scope{}, agents.ListQuery{Limit: 1}); err == nil {
			t.Fatal("an invalid scope must not query")
		}
	})
}
