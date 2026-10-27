package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Test_dependency_isolation_and_migrations: the migration reapplies without
// effect and installs the intended keys, checks, indexes, forced row level
// security and minimal runtime grants; composite foreign keys keep both
// endpoints in the link's tenant and project; tenant context never leaks
// through the pool. Real PostgreSQL. WM-01, IT-02, IT-04, SD-A01.
func Test_dependency_isolation_and_migrations(t *testing.T) {
	st := depstest.NewStack(t, depstest.Options{Conns: 1}) // one connection: pooled context reuse is certain
	ctx := context.Background()
	a1 := items(st, scopeA, 3, "a")
	a2 := items(st, scopeA2, 1, "a2")
	b := items(st, scopeB, 2, "b")
	st.MustLink(a1[0], a1[1])
	st.MustLink(b[0], b[1])

	t.Run("migration_reapplies_without_effect", func(t *testing.T) {
		res := st.Migrate(ctx)
		if len(res.Applied) != 0 || res.Skipped < 1 {
			t.Fatalf("reapply applied %v skipped %d", res.Applied, res.Skipped)
		}
		if st.Links(tenantA) != 1 {
			t.Fatal("reapplication changed the data")
		}
	})

	t.Run("constraints_indexes_rls_and_grants", func(t *testing.T) {
		var rls, forced int
		if err := st.Owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE relrowsecurity), count(*) FILTER (WHERE relforcerowsecurity)
			FROM pg_class WHERE relname = 'work_item_dependencies'`).Scan(&rls, &forced); err != nil || rls != 1 || forced != 1 {
			t.Fatalf("row level security: %d %d %v", rls, forced, err)
		}
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "DELETE": true, "UPDATE": false, "TRUNCATE": false, "REFERENCES": false, "TRIGGER": false} {
			var has bool
			if err := st.Owner.QueryRow(ctx, `SELECT has_table_privilege($1, 'work_item_dependencies', $2)`, st.DB.RuntimeRole, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s for the runtime role = %v, want %v", priv, has, want)
			}
		}
		for _, name := range []string{"work_item_dependencies_pkey", "work_item_dependencies_not_self", "work_item_dependencies_dependent_fkey", "work_item_dependencies_prerequisite_fkey"} {
			var n int
			if err := st.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, name).Scan(&n); err != nil || n != 1 {
				t.Errorf("constraint %s: %d %v", name, n, err)
			}
		}
		// Both foreign keys are composite over tenant, project and item, with no cascade.
		rows, err := st.Owner.Query(ctx, `SELECT c.conname, array_length(c.conkey, 1), c.confdeltype::text FROM pg_constraint c
			WHERE c.conrelid = 'work_item_dependencies'::regclass AND c.contype = 'f' AND c.conname IN ('work_item_dependencies_dependent_fkey', 'work_item_dependencies_prerequisite_fkey')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var name, del string
			var cols int
			if err := rows.Scan(&name, &cols, &del); err != nil {
				t.Fatal(err)
			}
			if cols != 3 || del != "a" { // 'a' = NO ACTION
				t.Errorf("%s: %d columns, delete action %q", name, cols, del)
			}
			n++
		}
		if n != 2 {
			t.Fatalf("composite work item foreign keys: %d", n)
		}
		for _, idx := range []string{"work_item_dependencies_pkey", "work_item_dependencies_reverse_idx"} {
			var c int
			if err := st.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&c); err != nil || c != 1 {
				t.Errorf("index %s: %d %v", idx, c, err)
			}
		}
	})

	t.Run("composite_foreign_keys_enforce_endpoint_ownership", func(t *testing.T) {
		insert := func(tenant, project, dependent, prerequisite string) error {
			_, err := st.Owner.Exec(ctx, `INSERT INTO work_item_dependencies (tenant_id, project_id, dependent_id, prerequisite_id, history_id, created_by_type, created_by_id, created_at)
				VALUES ($1, $2, $3, $4, 'wih_x', 'user', 'u', now())`, tenant, project, dependent, prerequisite)
			return err
		}
		// Same project: accepted by the database (the tuple is unique, not forbidden).
		if err := insert(tenantA, projectA, string(a1[2].ID), string(a1[1].ID)); err != nil {
			t.Fatalf("same-project link: %v", err)
		}
		for name, tc := range map[string]struct{ tenant, project, dep, pre string }{
			"prerequisite in another project": {tenantA, projectA, string(a1[0].ID), string(a2[0].ID)},
			"dependent in another project":    {tenantA, projectA, string(a2[0].ID), string(a1[0].ID)},
			"prerequisite in another tenant":  {tenantA, projectA, string(a1[0].ID), string(b[0].ID)},
			"dependent in another tenant":     {tenantA, projectA, string(b[0].ID), string(a1[0].ID)},
			"both in another project":         {tenantA, projectA, string(a2[0].ID), string(a2[0].ID)},
			"a missing prerequisite":          {tenantA, projectA, string(a1[0].ID), "wi_missing"},
			"a project of another tenant":     {tenantA, "p_billing", string(a1[0].ID), string(a1[1].ID)},
		} {
			err := insert(tc.tenant, tc.project, tc.dep, tc.pre)
			if code := pgCode(err); code != "23503" && code != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Self links and duplicates are rejected structurally.
		if code := pgCode(insert(tenantA, projectA, string(a1[0].ID), string(a1[0].ID))); code != "23514" {
			t.Errorf("self link code %q", code)
		}
		if code := pgCode(insert(tenantA, projectA, string(a1[0].ID), string(a1[1].ID))); code != "23505" {
			t.Errorf("duplicate link code %q", code)
		}
		// Work items and their history cannot be deleted out from under a link.
		if _, err := st.Owner.Exec(ctx, `DELETE FROM work_items WHERE tenant_id = $1 AND id = $2`, tenantA, string(a1[1].ID)); pgCode(err) != "23503" {
			t.Errorf("deleting a linked work item: %v", err)
		}
		st.OwnerExec(`DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND dependent_id = $2 AND prerequisite_id = $3`, tenantA, string(a1[2].ID), string(a1[1].ID))
	})

	t.Run("the_application_path_reports_foreign_endpoints_as_not_found", func(t *testing.T) {
		for name, pre := range map[string]work.Item{"another project": a2[0], "another tenant": b[0]} {
			if _, err := st.Link(a1[0], pre); !is(err, work.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := st.Link(a1[0], work.Item{Scope: scopeA, ID: "wi_missing"}); !is(err, work.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
		// The dependent itself in another project or tenant is not found either.
		if _, err := st.Deps.Add(ctx, addCmd(scopeA, a2[0], a1[0], 1)); !is(err, work.ErrNotFound) {
			t.Errorf("foreign dependent: %v", err)
		}
	})

	t.Run("row_level_security_isolates_tenants_without_predicates", func(t *testing.T) {
		count := func(tenant domain.TenantID, sql string, args ...any) (int, error) {
			var n int
			err := withTx(st, tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) })
			return n, err
		}
		if n, err := count(tenantA, `SELECT count(*) FROM work_item_dependencies`); err != nil || n != 1 {
			t.Fatalf("tenant A sees %d links, %v", n, err)
		}
		if n, err := count(tenantB, `SELECT count(*) FROM work_item_dependencies`); err != nil || n != 1 {
			t.Fatalf("tenant B sees %d links, %v", n, err)
		}
		if n, err := count(tenantB, `SELECT count(*) FROM work_item_dependencies WHERE dependent_id = $1`, string(a1[0].ID)); err != nil || n != 0 {
			t.Fatalf("tenant B reads tenant A's link: %d %v", n, err)
		}
		// Writing a row of another tenant is refused by the policy, and deleting finds nothing.
		err := withTx(st, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO work_item_dependencies (tenant_id, project_id, dependent_id, prerequisite_id, history_id, created_by_type, created_by_id, created_at)
				VALUES ($1, $2, $3, $4, 'wih_y', 'user', 'u', now())`, tenantA, projectA, string(a1[2].ID), string(a1[1].ID))
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v", err)
		}
		var deleted int64
		_ = withTx(st, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM work_item_dependencies WHERE dependent_id = $1`, string(a1[0].ID))
			deleted = tag.RowsAffected()
			return err
		})
		if deleted != 0 || st.Links(tenantA) != 1 {
			t.Fatalf("tenant B deleted %d of tenant A's links", deleted)
		}
		// No tenant context at all sees nothing.
		var n int
		if err := st.Runtime.QueryRow(ctx, `SELECT count(*) FROM work_item_dependencies`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("without a tenant context: %d %v", n, err)
		}
		// The runtime role has no UPDATE: links are inserted and deleted only.
		err = withTx(st, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE work_item_dependencies SET history_id = 'wih_z'`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("runtime update: %v", err)
		}
	})

	t.Run("pooled_connections_do_not_keep_the_tenant_context", func(t *testing.T) {
		// One pooled connection serves every call: tenant A's transaction must not
		// leave its context behind for the next caller.
		if _, err := st.Deps.List(ctx, listCmd(scopeA, a1[0])); err != nil {
			t.Fatal(err)
		}
		var setting *string
		if err := st.Runtime.QueryRow(ctx, `SELECT nullif(current_setting($1, true), '')`, storage.TenantSetting).Scan(&setting); err != nil || setting != nil {
			t.Fatalf("leaked tenant context %v %v", setting, err)
		}
		// Failed mutations clean up as well.
		if _, err := st.Deps.Add(ctx, addCmd(scopeA, a1[0], a1[1], 999)); !is(err, work.ErrVersionConflict) {
			t.Fatalf("stale add: %v", err)
		}
		if err := st.Runtime.QueryRow(ctx, `SELECT nullif(current_setting($1, true), '')`, storage.TenantSetting).Scan(&setting); err != nil || setting != nil {
			t.Fatalf("leaked tenant context after a failure %v %v", setting, err)
		}
		// A list for tenant B right after tenant A cannot see A's data.
		page, err := st.Deps.List(ctx, listCmd(scopeB, b[0]))
		if err != nil || len(page.Items) != 1 || page.Items[0].Link.PrerequisiteID != b[1].ID {
			t.Fatalf("tenant B list: %+v %v", page, err)
		}
		if _, err := st.Deps.List(ctx, listCmd(scopeB, a1[0])); !is(err, work.ErrNotFound) {
			t.Fatalf("tenant B reading tenant A's item: %v", err)
		}
	})
}
