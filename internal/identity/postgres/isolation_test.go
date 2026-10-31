package postgres_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/idtest"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func TestIdentityIsolation(t *testing.T) {
	f := newFixture(t, idtestOptions{Conns: 1}) // a single pooled connection exposes any leaked context
	rt := f.DB.RuntimeRole
	b := must(f.grant(f.admin, f.engineer, projectA, identity.RoleEngineer))
	must(f.Service.Grant(ctx, identity.GrantCommand{Caller: idtest.Caller(f.admin), Tenant: tenantA, User: f.reviewer, Role: identity.RoleReviewer}))

	t.Run("runtime role flags and ownership", func(t *testing.T) {
		var super, bypass bool
		if err := f.Owner.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, rt).Scan(&super, &bypass); err != nil || super || bypass {
			t.Fatalf("super=%v bypass=%v %v", super, bypass, err)
		}
		for _, table := range []string{"identity_authz_revisions", "identity_bootstrap"} {
			var owner string
			var rls, forced bool
			if err := f.Owner.QueryRow(ctx, `SELECT pg_get_userbyid(c.relowner), c.relrowsecurity, c.relforcerowsecurity FROM pg_class c WHERE c.oid = $1::regclass`, "public."+table).Scan(&owner, &rls, &forced); err != nil {
				t.Fatal(err)
			}
			if owner == rt || !rls || !forced {
				t.Errorf("%s: owner=%s rls=%v forced=%v", table, owner, rls, forced)
			}
		}
	})

	t.Run("minimum table grants", func(t *testing.T) {
		grants := func(table string) map[string]bool {
			rows, err := f.Owner.Query(ctx, `SELECT privilege_type FROM information_schema.role_table_grants WHERE table_name = $1 AND grantee = $2`, table, rt)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			out := map[string]bool{}
			for rows.Next() {
				var p string
				_ = rows.Scan(&p)
				out[p] = true
			}
			return out
		}
		rev := grants("identity_authz_revisions")
		if !rev["SELECT"] || !rev["INSERT"] || !rev["UPDATE"] || rev["DELETE"] || rev["TRUNCATE"] || len(rev) != 3 {
			t.Errorf("revisions: %v", rev)
		}
		if got := grants("identity_bootstrap"); len(got) != 0 {
			t.Errorf("bootstrap: %v", got)
		}
		// Binding rows are never created or removed by anything but the identity use cases, and users never by the runtime.
		if got := grants("users"); len(got) != 1 || !got["SELECT"] {
			t.Errorf("users: %v", got)
		}
	})

	t.Run("definer functions are pinned and not executable by PUBLIC", func(t *testing.T) {
		rows, err := f.Owner.Query(ctx, `SELECT p.proname, p.prosecdef, COALESCE(array_to_string(p.proconfig, ','), ''),
			has_function_privilege('public', p.oid, 'EXECUTE'), has_function_privilege($1, p.oid, 'EXECUTE')
			FROM pg_proc p WHERE p.proname IN ('kodama_identity_map_principal', 'kodama_identity_user', 'kodama_identity_tenants')`, rt)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var name, cfg string
			var secdef, public, runtime bool
			if err := rows.Scan(&name, &secdef, &cfg, &public, &runtime); err != nil {
				t.Fatal(err)
			}
			n++
			if !secdef || !strings.Contains(cfg, "search_path=pg_catalog, pg_temp") || public || !runtime {
				t.Errorf("%s: secdef=%v config=%q public=%v runtime=%v", name, secdef, cfg, public, runtime)
			}
		}
		if n != 3 {
			t.Fatalf("%d functions", n)
		}
	})

	t.Run("tenant policies confine every identity relation", func(t *testing.T) {
		count := func(tenant string, sql string, args ...any) (int, error) {
			var n int
			err := storage.WithTenant(ctx, f.Runtime, domainTenant(tenant), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, args...).Scan(&n)
			})
			return n, err
		}
		for _, sql := range []string{
			`SELECT count(*) FROM identity_authz_revisions`, `SELECT count(*) FROM memberships`, `SELECT count(*) FROM audit`, `SELECT count(*) FROM projects`,
		} {
			a, errA := count(tenantA, sql)
			bn, errB := count(tenantB, sql)
			wantB := 0
			if strings.Contains(sql, "FROM projects") {
				wantB = 1 // tenantB owns exactly its own project
			}
			if errA != nil || errB != nil || a == 0 || bn != wantB || (wantB == 1 && a != 2) {
				t.Errorf("%s: tenantA=%d (%v) tenantB=%d (%v)", sql, a, errA, bn, errB)
			}
		}
		// Writes for another tenant fail the policy; ownership constraints reject cross-tenant references.
		err := storage.WithTenant(ctx, f.Runtime, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO identity_authz_revisions (tenant_id, user_id) VALUES ($1, $2)`, tenantA, string(f.outsider))
			return err
		})
		if err == nil {
			t.Error("a tenantB transaction wrote a tenantA revision")
		}
		err = storage.WithTenant(ctx, f.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ($1, 'mb_x', $2, $3, 'viewer')`, tenantA, string(f.outsider), projectB)
			return err
		})
		if err == nil {
			t.Error("a membership referenced another tenant's project")
		}
		// No tenant context, no rows.
		var n int
		if err := f.Runtime.QueryRow(ctx, `SELECT count(*) FROM identity_authz_revisions`).Scan(&n); err != nil || n != 0 {
			t.Errorf("no context: %d %v", n, err)
		}
		// Through the service, a project of another tenant is simply absent.
		if _, err := f.Service.Grant(ctx, identity.GrantCommand{Caller: idtest.Caller(f.admin), Tenant: tenantA, User: f.outsider, Project: projectB, Role: identity.RoleViewer}); !is(err, identity.ErrNotFound) {
			t.Errorf("cross-tenant project: %v", err)
		}
	})

	t.Run("pooled connection never leaks a tenant context", func(t *testing.T) {
		// Conns is 1: every statement below reuses the connection that just ran identity transactions.
		for i := 0; i < 3; i++ {
			must(f.Service.ListProjects(ctx, idtest.Caller(f.admin), tenantA, "", 10))
			var tenant *string
			if err := f.Runtime.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&tenant); err != nil || tenant != nil {
				t.Fatalf("leaked context %v %v", tenant, err)
			}
			var n int
			if err := f.Runtime.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rows visible without a context: %d %v", n, err)
			}
			// A failing transaction ends cleanly too.
			_, err := f.Service.Revoke(ctx, identity.RevokeCommand{Caller: idtest.Caller(f.outsider), Tenant: tenantA, ID: b.ID, Expected: 1})
			if !is(err, identity.ErrHidden) {
				t.Fatalf("outsider revoke: %v", err)
			}
			if err := f.Runtime.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&tenant); err != nil || tenant != nil {
				t.Fatalf("leaked context after failure %v %v", tenant, err)
			}
		}
	})

	t.Run("transaction-bound authority sees the committed state and no other tenant", func(t *testing.T) {
		err := storage.WithTenant(ctx, f.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			a := identitypg.NewTxAuthority(tx)
			if err := a.Authorize(ctx, identity.UserActor(f.reviewer), identity.Target{TenantID: tenantA, ProjectID: projectA}, identity.PermWorkComplete); err != nil {
				return err
			}
			if err := a.Authorize(ctx, identity.UserActor(f.reviewer), identity.Target{TenantID: tenantB, ProjectID: projectB}, identity.PermWorkComplete); !is(err, identity.ErrHidden) {
				return errors.New("foreign tenant not hidden")
			}
			if err := a.Authorize(ctx, identity.UserActor(f.outsider), identity.Target{TenantID: tenantA, ProjectID: projectA}, identity.PermWorkRead); !is(err, identity.ErrHidden) {
				return errors.New("outsider not hidden")
			}
			if err := a.Authorize(ctx, identity.UserActor(f.engineer), identity.Target{TenantID: tenantA, ProjectID: projectA}, identity.PermWorkComplete); !is(err, identity.ErrDenied) {
				return errors.New("engineer completed work")
			}
			if err := a.ResolveUser(ctx, string(tenantA), f.outsider); !is(err, identity.ErrHidden) {
				return errors.New("outsider resolved")
			}
			return a.ResolveUser(ctx, string(tenantA), f.reviewer)
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestMigrationReapply(t *testing.T) {
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 4)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}

	full := manifestOf(t)
	res, err := storage.Migrate(ctx, owner, full, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Applied, []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", "004_pipeline_runtime_expand.sql", "005_durable_run_state.sql", "006_wait_decision_and_materializations.sql", "007_signal_decision_reason.sql"}) {
		t.Fatalf("the baseline is seven migrations, applied %v", res.Applied)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ('t010_up', 'up'); INSERT INTO users (id, issuer, subject) VALUES ('u_up', 'https://i', 's')`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO memberships (tenant_id, id, user_id, role) VALUES ('t010_up', 'mb_up', 'u_up', 'viewer')`); err != nil {
		t.Fatal(err)
	}
	res, err = storage.Migrate(ctx, owner, full, opts)
	if err != nil || len(res.Applied) != 0 || res.Skipped != len(full.Migrations()) {
		t.Fatalf("reapply: %+v %v", res, err)
	}

	// Existing rows survive: the binding remains visible to its tenant.
	rt := db.RuntimePool(ctx, 4)
	users := rtStack{rt}
	if got := users.bindingCount("t010_up"); got != 1 {
		t.Fatalf("pre-existing binding lost: %d", got)
	}

	// A modified migration is refused rather than silently reapplied.
	tampered := storage.BaseContribution()
	tampered.Migrations[0].SQL += "\n-- edited after merge\n"
	m, err := storage.NewManifest(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, m, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("tampered migration: %v", err)
	}
}

type rtStack struct{ pool storage.TxBeginner }

func (r rtStack) bindingCount(tenant string) int {
	var n int
	if err := storage.WithTenant(ctx, r.pool, domainTenant(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n)
	}); err != nil {
		panic(err)
	}
	return n
}
