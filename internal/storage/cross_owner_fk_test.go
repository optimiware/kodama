package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/storage"
)

// Test_cross_owner_fk covers IT-02, IT-04 and IT-A02: select it with
//
//	go test -run cross_owner_fk ./internal/storage/
func Test_cross_owner_fk(t *testing.T) {
	e := newEnv(t)

	// Composite ownership constraints hold even for the table owner, which
	// bypasses nothing it is not allowed to: these run as the migration role.
	t.Run("owner_cannot_violate_composite_keys", func(t *testing.T) {
		ctx := testCtx(t)
		cases := []struct{ name, sql, code string }{
			{"project_in_unknown_tenant", `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('ghost', 'px', 'px', 1)`, "23503"},
			{"membership_project_of_other_tenant", `INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('tenant-a', 'mx', 'u2', 'pb1', 'viewer')`, "23503"},
			{"membership_unknown_project", `INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('tenant-a', 'mx', 'u2', 'nope', 'viewer')`, "23503"},
			{"membership_unknown_tenant", `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('ghost', 'mx', 'u2', 'viewer')`, "23503"},
			{"membership_unknown_user", `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('tenant-a', 'mx', 'nobody', 'viewer')`, "23503"},
			{"audit_project_of_other_tenant", `INSERT INTO audit (tenant_id, id, project_id, actor_type, actor_id, action, resource_type, resource_id) VALUES ('tenant-a', 'ax', 'pb1', 'system', 's', 'a', 'project', 'pb1')`, "23503"},
			{"audit_unknown_tenant", `INSERT INTO audit (tenant_id, id, actor_type, actor_id, action, resource_type, resource_id) VALUES ('ghost', 'ax', 'system', 's', 'a', 'project', 'p')`, "23503"},
			{"move_project_under_other_tenant_membership", `UPDATE projects SET tenant_id = 'tenant-b' WHERE tenant_id = 'tenant-a' AND id = 'pa1'`, "23503"},
			{"delete_project_referenced_by_membership", `DELETE FROM projects WHERE tenant_id = 'tenant-a' AND id = 'pa1'`, "23503"},
			{"unknown_role", `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('tenant-a', 'mx', 'u2', 'superadmin')`, "23514"},
			{"bad_id_syntax", `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-a', 'bad id', 'k', 1)`, "23514"},
			{"wip_limit_positive", `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-a', 'pz', 'pz', 0)`, "23514"},
			{"version_positive", `INSERT INTO projects (tenant_id, id, key, active_wip_limit, version) VALUES ('tenant-a', 'pz', 'pz', 1, 0)`, "23514"},
			{"settings_must_be_object", `INSERT INTO projects (tenant_id, id, key, active_wip_limit, settings) VALUES ('tenant-a', 'pz', 'pz', 1, '[]')`, "23514"},
			{"audit_actor_type", `INSERT INTO audit (tenant_id, id, actor_type, actor_id, action, resource_type, resource_id) VALUES ('tenant-a', 'ax', 'robot', 's', 'a', 'project', 'p')`, "23514"},
			{"audit_metadata_must_be_object", `INSERT INTO audit (tenant_id, id, actor_type, actor_id, action, resource_type, resource_id, metadata) VALUES ('tenant-a', 'ax', 'system', 's', 'a', 'project', 'p', '"x"')`, "23514"},
			// Uniqueness (IT-02 keys, database-schema.md).
			{"duplicate_tenant_key", `INSERT INTO tenants (id, key) VALUES ('tenant-c', 'a')`, "23505"},
			{"duplicate_project_key_in_tenant", `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-a', 'pz', 'pa1', 1)`, "23505"},
			{"duplicate_issuer_subject", `INSERT INTO users (id, issuer, subject) VALUES ('u9', 'https://issuer.example', 'subject-1')`, "23505"},
			{"duplicate_tenant_binding", `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('tenant-a', 'mx', 'u1', 'admin')`, "23505"},
			{"duplicate_project_binding", `INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('tenant-a', 'mx', 'u1', 'pa1', 'engineer')`, "23505"},
		}
		for _, c := range cases {
			_, err := e.mig.Exec(ctx, c.sql)
			requireCode(t, err, c.code, c.name)
		}
		// Same key in another tenant, and different roles for the same user, are fine.
		e.exec(t, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-a', 'pk', 'pb1', 1)`)
		e.exec(t, `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('tenant-a', 'mok', 'u1', 'reviewer')`)
	})

	// The runtime role is additionally fenced by RLS WITH CHECK: it cannot
	// write a row for a tenant other than its transaction context.
	t.Run("runtime_cannot_write_foreign_tenant_rows", func(t *testing.T) {
		pool := e.runtime(t, 2)
		cases := []struct{ name, sql, code string }{
			{"project_for_other_tenant", `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-b', 'px', 'px', 1)`, "42501"},
			{"membership_for_other_tenant", `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('tenant-b', 'mx', 'u2', 'viewer')`, "42501"},
			{"audit_for_other_tenant", `INSERT INTO audit (tenant_id, id, actor_type, actor_id, action, resource_type, resource_id) VALUES ('tenant-b', 'ax', 'system', 's', 'a', 'project', 'p')`, "42501"},
			{"reassign_own_project_to_other_tenant", `UPDATE projects SET tenant_id = 'tenant-b' WHERE id = 'pa3'`, "42501"},
			{"reassign_own_membership_to_other_tenant", `UPDATE memberships SET tenant_id = 'tenant-b' WHERE id = 'm1'`, "42501"},
			// RI checks run without RLS, so ownership mismatches still fail
			// on the composite key for the runtime role too.
			{"membership_project_of_other_tenant", `INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('tenant-a', 'mx', 'u2', 'pb1', 'viewer')`, "23503"},
			{"audit_project_of_other_tenant", `INSERT INTO audit (tenant_id, id, project_id, actor_type, actor_id, action, resource_type, resource_id) VALUES ('tenant-a', 'ax', 'pb1', 'system', 's', 'a', 'project', 'pb1')`, "23503"},
			{"tenant_row_insert_not_granted", `INSERT INTO tenants (id, key) VALUES ('tenant-a', 'zz')`, "42501"},
		}
		for _, c := range cases {
			err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, c.sql)
				return err
			})
			requireCode(t, err, c.code, c.name)
		}

		// Positive control: the same statements are valid for the context tenant.
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			for _, sql := range []string{
				`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('tenant-a', 'pnew', 'pnew', 2)`,
				`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('tenant-a', 'mnew', 'u2', 'pnew', 'viewer')`,
				`INSERT INTO audit (tenant_id, id, project_id, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, metadata)
				 VALUES ('tenant-a', 'anew', 'pnew', 'user', 'u1', 'project.created', 'project', 'pnew', 1, 'test', '{"k":"v"}')`,
			} {
				if _, err := tx.Exec(ctx, sql); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("valid writes for the context tenant failed: %v", err)
		}
		// And tenant B still sees none of them.
		_ = storage.WithTenant(testCtx(t), pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM projects WHERE id = 'pnew') + (SELECT count(*) FROM memberships WHERE id = 'mnew') + (SELECT count(*) FROM audit WHERE id = 'anew')").Scan(&n); err != nil {
				t.Error(err)
			} else if n != 0 {
				t.Errorf("tenant B sees %d of tenant A's new rows", n)
			}
			return nil
		})
	})
}
