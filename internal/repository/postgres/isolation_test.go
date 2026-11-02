package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/repository"
	rt "github.com/optimiware/kodama/internal/repository/repositorytest"
	"github.com/optimiware/kodama/internal/storage"
)

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func countTenant(t *testing.T, e *rt.PGEnv, tenant domain.TenantID, sql string, args ...any) int {
	t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Test_tenant_isolation_and_guards: row level security, pooled tenant reset,
// the composite ownership key, the runtime role's least privilege, the
// database-enforced lifecycle invariants and the creation race.
func Test_tenant_isolation_and_guards(t *testing.T) {
	ctx := context.Background()
	e := rt.NewPGEnv(t)
	k := rt.NewKit(t, e)
	a, err := k.Svc.Create(k.Ctx, rt.Engineer, rt.ScopeA, gitv1.CreateRepositoryRequest{Name: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Svc.Create(k.Ctx, rt.Engineer, rt.ScopeB, gitv1.CreateRepositoryRequest{Name: "backend"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("RLS hides every row from other tenants and from no tenant", func(t *testing.T) {
		var n int
		if err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("without tenant context: %d rows, %v", n, err)
		}
		if got := countTenant(t, e, rt.ScopeB.TenantID, `SELECT count(*) FROM repositories WHERE tenant_id = $1`, string(rt.ScopeA.TenantID)); got != 0 {
			t.Fatalf("tenant B sees %d of tenant A's rows", got)
		}
		if got := countTenant(t, e, rt.ScopeA.TenantID, `SELECT count(*) FROM repositories`); got != 1 {
			t.Fatalf("tenant A sees %d rows", got)
		}
		// Writes under the wrong tenant are refused by the policy, not dropped.
		err := storage.WithTenant(ctx, e.Runtime, rt.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, reason, created_by_type, created_by_id, created_at, updated_at)
				VALUES ($1, $2, 'repo_evil', 'evil', 'main', 'internal', 'provisioning', 'provisioning_pending', 'user', 'x', now(), now())`,
				string(rt.ScopeA.TenantID), string(rt.ScopeA.ProjectID))
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v", err)
		}
		if _, err := e.PG.Get(ctx, rt.ScopeB, a.Repository.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the store crossed tenants: %v", err)
		}
		if got, err := e.PG.Provisioning(ctx, rt.ScopeB.TenantID, "", 10); err != nil || len(got) != 0 {
			t.Fatalf("tenant B's provisioning list: %v %v", got, err)
		}
		if _, err := e.PG.Get(ctx, domain.Scope{TenantID: rt.ScopeA.TenantID, ProjectID: rt.ScopeA2.ProjectID}, a.Repository.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the store crossed projects: %v", err)
		}
	})

	t.Run("a pooled connection never carries the previous tenant's context", func(t *testing.T) {
		pool := e.DB.RuntimePool(ctx, 1) // one connection: every call reuses it
		for range 3 {
			for _, c := range []struct {
				tenant domain.TenantID
				want   int
			}{{rt.ScopeA.TenantID, 1}, {rt.ScopeB.TenantID, 1}} {
				var n int
				err := storage.WithTenant(ctx, pool, c.tenant, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM repositories WHERE name = 'backend'`).Scan(&n)
				})
				if err != nil || n != c.want {
					t.Fatalf("%s: %d rows, %v", c.tenant, n, err)
				}
				var leaked int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&leaked); err != nil || leaked != 0 {
					t.Fatalf("after %s: %d rows visible without a tenant, %v", c.tenant, leaked, err)
				}
			}
		}
	})

	t.Run("the runtime role has least privilege", func(t *testing.T) {
		exec := func(sql string, args ...any) error {
			return storage.WithTenant(ctx, e.Runtime, rt.ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, args...)
				return err
			})
		}
		id := string(a.Repository.ID)
		if got := sqlState(exec(`DELETE FROM repositories WHERE tenant_id = $1 AND id = $2`, string(rt.ScopeA.TenantID), id)); got != "42501" {
			t.Fatalf("delete: %q", got)
		}
		for _, col := range []string{"name = 'renamed'", "hosting = 'external'", "project_id = 'p127_other'", "created_by_id = 'x'", "external_url = 'https://x.test/y'"} {
			if got := sqlState(exec(`UPDATE repositories SET `+col+` WHERE tenant_id = $1 AND id = $2`, string(rt.ScopeA.TenantID), id)); got != "42501" {
				t.Fatalf("update of %s: %q", col, got)
			}
		}
		if err := exec(`UPDATE repositories SET direct_push_to_default = true, version = version + 1, updated_at = updated_at + interval '1 second' WHERE tenant_id = $1 AND id = $2`,
			string(rt.ScopeA.TenantID), id); err != nil {
			t.Fatalf("the lifecycle columns are updatable: %v", err)
		}
	})

	owner := func(sql string, args ...any) error {
		_, err := e.Migration.Exec(ctx, sql, args...)
		return err
	}
	id := string(b.Repository.ID)
	tenant := string(rt.ScopeB.TenantID)

	t.Run("the guard refuses illegal transitions even from the owner", func(t *testing.T) {
		for name, sql := range map[string]string{
			"a change without a version bump": `UPDATE repositories SET default_branch = 'trunk' WHERE tenant_id = $1 AND id = $2`,
			"a version jump":                  `UPDATE repositories SET default_branch = 'trunk', version = version + 2 WHERE tenant_id = $1 AND id = $2`,
			"a rename":                        `UPDATE repositories SET name = 'renamed', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			"a hosting switch":                `UPDATE repositories SET hosting = 'external', external_url = 'https://x.test/y', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			"a move to another project":       `UPDATE repositories SET project_id = 'p127_other', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			"ready back to provisioning":      `UPDATE repositories SET status = 'provisioning', reason = 'provisioning_pending', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		} {
			if got := sqlState(owner(sql, tenant, id)); got != "23514" {
				t.Errorf("%s: SQLSTATE %q", name, got)
			}
		}
		if got := sqlState(owner(`UPDATE repositories SET status = 'archived', reason = 'archived', version = version + 1 WHERE tenant_id = $1 AND id = $2`, tenant, id)); got != "" {
			t.Fatalf("ready to archived is legal: %q", got)
		}
		if got := sqlState(owner(`UPDATE repositories SET status = 'ready', reason = NULL, version = version + 1 WHERE tenant_id = $1 AND id = $2`, tenant, id)); got != "" {
			t.Fatalf("archived to ready is legal: %q", got)
		}
	})

	t.Run("an emptiness observation only advances", func(t *testing.T) {
		if got := sqlState(owner(`UPDATE repositories SET empty_observed_at = '2026-10-30T11:00:00Z' WHERE tenant_id = $1 AND id = $2`, tenant, id)); got != "" {
			t.Fatalf("a first observation: %q", got)
		}
		if got := sqlState(owner(`UPDATE repositories SET empty_observed_at = '2026-10-30T11:00:05Z' WHERE tenant_id = $1 AND id = $2`, tenant, id)); got != "" {
			t.Fatalf("a later observation needs no version: %q", got)
		}
		if got := sqlState(owner(`UPDATE repositories SET empty_observed_at = '2026-10-30T11:00:01Z' WHERE tenant_id = $1 AND id = $2`, tenant, id)); got != "23514" {
			t.Fatalf("an observation that moves back: %q", got)
		}
	})

	t.Run("checks refuse malformed rows", func(t *testing.T) {
		insert := func(name, branch, hosting, status string, reason, url *string, empty bool) error {
			return owner(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, reason, empty, external_url, created_by_type, created_by_id, created_at, updated_at)
				VALUES ($1, $2, 'repo_chk_' || $3::text, $3, $4, $5, $6, $7, $8, $9, 'user', 'u', now(), now())`,
				string(rt.ScopeA.TenantID), string(rt.ScopeA.ProjectID), name, branch, hosting, status, reason, empty, url)
		}
		s := func(v string) *string { return &v }
		pending := s("provisioning_pending")
		for name, err := range map[string]error{
			"an upper-case name":               insert("Bad", "main", "internal", "provisioning", pending, nil, false),
			"a reserved branch":                insert("c1", "kodama/work", "internal", "provisioning", pending, nil, false),
			"a ref path as the branch":         insert("c2", "refs/heads/main", "internal", "provisioning", pending, nil, false),
			"a dotted branch":                  insert("c3", "a..b", "internal", "provisioning", pending, nil, false),
			"a ready row with a reason":        insert("c4", "main", "internal", "ready", pending, nil, false),
			"a provisioning row without one":   insert("c5", "main", "internal", "provisioning", nil, nil, false),
			"a provisioning row that is empty": insert("c6", "main", "internal", "provisioning", pending, nil, true),
			"an internal row with a URL":       insert("c7", "main", "internal", "ready", nil, s("https://x.test/y"), false),
			"an external row without a URL":    insert("c8", "main", "external", "ready", nil, nil, false),
			"an external row that is empty":    insert("c9", "main", "external", "ready", nil, s("https://x.test/y"), true),
			"an external row with credentials": insert("c10", "main", "external", "ready", nil, s("https://u:p@x.test/y"), false),
			"an ssh URL":                       insert("c11", "main", "external", "ready", nil, s("ssh://x.test/y"), false),
			"an unknown status":                insert("c12", "main", "internal", "deleted", pending, nil, false),
		} {
			if got := sqlState(err); got != "23514" && got != "23502" {
				t.Errorf("%s: SQLSTATE %q (%v)", name, got, err)
			}
		}
		if got := sqlState(insert("backend", "main", "internal", "provisioning", pending, nil, false)); got != "23505" {
			t.Errorf("a duplicate name: SQLSTATE %q", got)
		}
		// A project that only another tenant has is no home: the key is composite.
		if _, err := e.Migration.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1, 'p127_only_b', 'p127_only_b', 5)`, tenant); err != nil {
			t.Fatal(err)
		}
		err := owner(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, reason, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1, 'p127_only_b', 'repo_chk_fk', 'fk', 'main', 'internal', 'provisioning', 'provisioning_pending', 'user', 'u', now(), now())`, string(rt.ScopeA.TenantID))
		if got := sqlState(err); got != "23503" {
			t.Errorf("a project of another tenant: SQLSTATE %q (%v)", got, err)
		}
		r, err := repository.New(repository.NewParams{ID: "repo_t127_fk", Scope: domain.Scope{TenantID: rt.ScopeA.TenantID, ProjectID: "p127_only_b"}, Name: "fk", CreatedBy: rt.Engineer}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := e.PG.Insert(ctx, r, repository.Effects{}); !errors.Is(err, repository.ErrNotFound) || created {
			t.Errorf("the store inserting into a foreign project: %v %v", created, err)
		}
	})

	t.Run("concurrent reservations of one name leave one row", func(t *testing.T) {
		const n = 16
		var wg sync.WaitGroup
		created := make(chan bool, n)
		ids := make(chan repository.ID, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := repository.New(repository.NewParams{ID: repository.ID("repo_t127_race_" + string(rune('a'+i))), Scope: rt.ScopeA2, Name: "race", CreatedBy: rt.Engineer}, time.Now().UTC())
				if err != nil {
					t.Error(err)
					return
				}
				got, c, err := e.PG.Insert(ctx, r, repository.Effects{Audit: []repository.Audit{{Action: "repository.provisioning_requested", Actor: rt.Engineer}}})
				if err != nil {
					t.Error(err)
					return
				}
				created <- c
				ids <- got.ID
			}()
		}
		wg.Wait()
		close(created)
		close(ids)
		winners := 0
		for c := range created {
			if c {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("%d reservations created the repository", winners)
		}
		seen := map[repository.ID]bool{}
		for id := range ids {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Fatalf("callers saw %d different repositories", len(seen))
		}
		if got := countTenant(t, e, rt.ScopeA.TenantID, `SELECT count(*) FROM repositories WHERE project_id = $1 AND name = 'race'`, string(rt.ScopeA2.ProjectID)); got != 1 {
			t.Fatalf("%d rows", got)
		}
	})

	t.Run("concurrent updates serialize on the row", func(t *testing.T) {
		const n = 20
		start := a.Repository.Version
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.PG.Update(ctx, rt.ScopeA, a.Repository.ID, func(r *repository.Repository) (repository.Effects, error) {
					_, err := r.SetPolicy(gitv1.RepositoryPolicy{DirectPushToDefault: !r.Policy.DirectPushToDefault}, r.UpdatedAt.Add(time.Second))
					return repository.Effects{}, err
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		got, err := e.PG.Get(ctx, rt.ScopeA, a.Repository.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The role-level test above already bumped the version once by SQL.
		if got.Version != start+1+n {
			t.Fatalf("version %d after %d serialized updates from %d", got.Version, n, start+1)
		}
	})
}
