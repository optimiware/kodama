package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// Test_runtime_role_privileges verifies IT-04: the runtime role is neither
// owner, superuser nor BYPASSRLS, cannot change the schema, and cannot alter
// the audit trail. Select with: go test -run runtime_role_privileges
func Test_runtime_role_privileges(t *testing.T) {
	e := newEnv(t)
	pool := e.runtime(t, 1)
	ctx := testCtx(t)

	t.Run("role_attributes", func(t *testing.T) {
		var super, bypass, createDB, createRole bool
		err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = current_user`).
			Scan(&super, &bypass, &createDB, &createRole)
		if err != nil {
			t.Fatal(err)
		}
		if super || bypass || createDB || createRole {
			t.Fatalf("runtime role is privileged: super=%v bypassrls=%v createdb=%v createrole=%v", super, bypass, createDB, createRole)
		}
		var owned int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE r.rolname = current_user AND n.nspname = 'public'`).Scan(&owned); err != nil || owned != 0 {
			t.Fatalf("runtime role owns %d relations (err %v)", owned, err)
		}
		var owner string
		if err := pool.QueryRow(ctx, `SELECT tableowner FROM pg_tables WHERE tablename = 'projects'`).Scan(&owner); err != nil || owner != e.db.MigrationRole {
			t.Fatalf("projects owner = %q (err %v), want %q", owner, err, e.db.MigrationRole)
		}
	})

	t.Run("all_tables_enable_and_force_rls", func(t *testing.T) {
		rows, err := e.mig.Query(ctx, `SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind = 'r' AND relname <> 'schema_migrations'
			ORDER BY relname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var name string
			var enabled, forced bool
			if err := rows.Scan(&name, &enabled, &forced); err != nil {
				t.Fatal(err)
			}
			seen++
			if !enabled || !forced {
				t.Errorf("%s: enable=%v force=%v", name, enabled, forced)
			}
		}
		if seen != 5 {
			t.Errorf("checked %d tables, want tenants, projects, users, memberships, audit", seen)
		}
	})

	t.Run("cannot_change_schema", func(t *testing.T) {
		for _, sql := range []string{
			"CREATE TABLE rogue (id int)",
			"CREATE TABLE public.rogue (id int)",
			"ALTER TABLE projects ADD COLUMN rogue int",
			"ALTER TABLE projects DISABLE ROW LEVEL SECURITY",
			"DROP TABLE projects",
			"TRUNCATE projects",
			"TRUNCATE audit",
			"CREATE POLICY rogue ON projects USING (true)",
			"CREATE FUNCTION rogue() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		} {
			_, err := pool.Exec(ctx, sql)
			requireCode(t, err, "42501", sql)
		}
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); pgCode(err) != "42501" {
			t.Errorf("runtime can read schema_migrations: n=%d err=%v", n, err)
		}
	})

	t.Run("audit_is_append_only", func(t *testing.T) {
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM audit").Scan(&n); err != nil || n != 1 {
				t.Errorf("select own audit: n=%d err=%v", n, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, sql := range []string{
			"UPDATE audit SET reason = 'tampered'",
			"UPDATE audit SET action = 'x' WHERE id = 'au-a'",
			"DELETE FROM audit",
			"DELETE FROM audit WHERE id = 'au-a'",
		} {
			// Each statement gets its own transaction: a failed statement aborts it.
			err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql)
				return err
			})
			requireCode(t, err, "42501", sql)
		}
		var reason *string
		if err := e.mig.QueryRow(ctx, "SELECT reason FROM audit WHERE id = 'au-a'").Scan(&reason); err != nil || reason != nil {
			t.Fatalf("audit row changed: reason=%v err=%v", reason, err)
		}
	})

	t.Run("least_privilege_grants", func(t *testing.T) {
		type grant struct{ table, priv string }
		for _, g := range []grant{
			{"tenants", "INSERT"}, {"tenants", "UPDATE"}, {"tenants", "DELETE"},
			{"users", "INSERT"}, {"users", "UPDATE"}, {"users", "DELETE"},
			{"projects", "DELETE"}, {"audit", "UPDATE"}, {"audit", "DELETE"}, {"audit", "TRUNCATE"},
			{"schema_migrations", "SELECT"},
		} {
			var ok bool
			if err := pool.QueryRow(ctx, "SELECT has_table_privilege(current_user, $1, $2)", g.table, g.priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Errorf("runtime role has %s on %s", g.priv, g.table)
			}
		}
		for _, g := range []grant{
			{"tenants", "SELECT"}, {"projects", "SELECT"}, {"projects", "INSERT"}, {"projects", "UPDATE"}, {"users", "SELECT"},
			{"memberships", "SELECT"}, {"memberships", "INSERT"}, {"memberships", "UPDATE"}, {"memberships", "DELETE"},
			{"audit", "SELECT"}, {"audit", "INSERT"},
		} {
			var ok bool
			if err := pool.QueryRow(ctx, "SELECT has_table_privilege(current_user, $1, $2)", g.table, g.priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("runtime role lacks %s on %s", g.priv, g.table)
			}
		}
	})

	t.Run("users_visible_only_to_tenant_members", func(t *testing.T) {
		ids := func(tenant domain.TenantID) []string {
			var out []string
			err := storage.WithTenant(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, "SELECT id FROM users ORDER BY id")
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					out = append(out, id)
				}
				return rows.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got := ids(tenantA); len(got) != 1 || got[0] != "u1" {
			t.Errorf("tenant A users = %v, want [u1]", got)
		}
		if got := ids(tenantB); len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
			t.Errorf("tenant B users = %v, want [u1 u2]", got)
		}
	})
}

// Test_non_tenant_paths covers the two narrow paths outside a tenant context.
func Test_non_tenant_paths(t *testing.T) {
	e := newEnv(t)
	pool := e.runtime(t, 1)
	ctx := testCtx(t)

	t.Run("discover_memberships_is_principal_filtered", func(t *testing.T) {
		got, err := storage.DiscoverMemberships(ctx, pool, "https://issuer.example", "subject-1")
		if err != nil {
			t.Fatal(err)
		}
		want := []storage.Membership{
			{UserID: "u1", TenantID: "tenant-a", Role: "admin"},
			{UserID: "u1", TenantID: "tenant-a", ProjectID: "pa1", Role: "engineer"},
			{UserID: "u1", TenantID: "tenant-b", Role: "viewer"},
		}
		if len(got) != len(want) {
			t.Fatalf("memberships = %+v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("membership[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
		// Same subject under another issuer is a different principal.
		if got, err := storage.DiscoverMemberships(ctx, pool, "https://other.example", "subject-1"); err != nil || len(got) != 0 {
			t.Errorf("other issuer: %+v, %v", got, err)
		}
		if got, err := storage.DiscoverMemberships(ctx, pool, "https://issuer.example", "unknown"); err != nil || len(got) != 0 {
			t.Errorf("unknown subject: %+v, %v", got, err)
		}
		if _, err := storage.DiscoverMemberships(ctx, pool, "", "subject-1"); err == nil {
			t.Error("empty issuer accepted")
		}
		// SQL metacharacters are data.
		if got, err := storage.DiscoverMemberships(ctx, pool, "x' OR '1'='1", "y"); err != nil || len(got) != 0 {
			t.Errorf("injection: %+v, %v", got, err)
		}
	})

	t.Run("discovery_leaves_no_tenant_context", func(t *testing.T) {
		if r := probe(t, pool); !r.clean() {
			t.Fatalf("context after discovery: %+v", r)
		}
	})

	t.Run("list_tenant_ids_exposes_ids_only", func(t *testing.T) {
		ids, err := storage.ListTenantIDs(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 || ids[0] != tenantA || ids[1] != tenantB {
			t.Fatalf("ids = %v", ids)
		}
		// The direct table stays closed without a context.
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&n); err != nil || n != 0 {
			t.Fatalf("direct tenants read: n=%d err=%v", n, err)
		}
	})

	t.Run("functions_are_not_callable_by_public", func(t *testing.T) {
		var ok bool
		if err := e.mig.QueryRow(ctx, `SELECT has_function_privilege('public', 'kodama_list_tenant_ids()', 'EXECUTE')`).Scan(&ok); err != nil || ok {
			t.Fatalf("PUBLIC may execute kodama_list_tenant_ids: ok=%v err=%v", ok, err)
		}
	})
}
