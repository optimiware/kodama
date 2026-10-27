package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var bg = context.Background()

func code(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// Test_migration_reapply: the baseline applies and reapplies as a no-op,
// rejects a modified applied migration and creates the constraints the module
// relies on.
func Test_migration_reapply(t *testing.T) {
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(bg, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Applied) != 2 {
		t.Fatalf("the baseline is two migrations, applied %v", first.Applied)
	}
	second, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := storage.BaseContribution()
	changed.Migrations[0].SQL += "\n-- edited"
	bad, _ := storage.NewManifest(changed)
	if _, err := storage.Migrate(bg, owner, bad, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}
	for _, table := range []string{"mcp_servers", "mcp_run_bindings", "mcp_sessions", "mcp_invocations", "mcp_run_calls"} {
		var rls, force bool
		if err := owner.QueryRow(bg, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls, &force); err != nil || !rls || !force {
			t.Errorf("%s: row level security %v forced %v err %v", table, rls, force, err)
		}
	}
}

// Test_tenant_isolation: RLS, composite ownership, non-owner grants and pooled
// context clearing.
func Test_tenant_isolation(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{})
	a := s.Setup([]string{"echo"}, []string{"echo"}, nil)
	scopeB := domain.Scope{TenantID: "t033_globex", ProjectID: "p033_globex"}
	bw := s.NewWorkload(mcptest.WorkloadSpec{Scope: &scopeB, Run: "run_t033_b"})
	secB := s.Credential(scopeB, "remote-token", s.Fixture.Origin(), s.Fixture.Token())
	regB := s.Register(scopeB, secB, "echo")
	_ = bw

	rt := s.RT.Runtime
	inTenant := func(tenant domain.TenantID, fn func(tx pgx.Tx) error) error {
		return storage.WithTenant(bg, rt, tenant, func(ctx context.Context, tx pgx.Tx) error { return fn(tx) })
	}

	t.Run("a tenant sees only its own rows in every relation", func(t *testing.T) {
		for _, table := range []string{"mcp_servers"} {
			var n int
			if err := inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(bg, `SELECT count(*) FROM `+table).Scan(&n)
			}); err != nil || n != 1 {
				t.Fatalf("%s visible to tenant A: %d %v", table, n, err)
			}
		}
		if _, err := s.Store.GetRegistration(bg, a.W.Scope, regB.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("a foreign registration is visible: %v", err)
		}
		wrong := domain.Scope{TenantID: a.W.Scope.TenantID, ProjectID: scopeB.ProjectID}
		if _, err := s.Store.GetRegistration(bg, wrong, regB.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("a registration is visible through another project: %v", err)
		}
	})

	t.Run("a tenant cannot write another tenant's rows", func(t *testing.T) {
		err := inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `INSERT INTO mcp_servers (tenant_id, project_id, id, endpoint, secret_name, secret_version, tools, version, created_by, updated_by, created_at, updated_at)
				VALUES ($1, $2, 'mcp_t033_x', 'https://x.example.com/', 'a-b', 1, '{}', 1, 'u', 'u', now(), now())`, string(scopeB.TenantID), string(scopeB.ProjectID))
			return err
		})
		if code(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v (code %s), want a row level security violation", err, code(err))
		}
	})

	t.Run("composite ownership refuses a registration under another project", func(t *testing.T) {
		err := inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `INSERT INTO mcp_servers (tenant_id, project_id, id, endpoint, secret_name, secret_version, tools, version, created_by, updated_by, created_at, updated_at)
				VALUES ($1, 'p033_nonexistent', 'mcp_t033_y', 'https://x.example.com/', 'a-b', 1, '{}', 1, 'u', 'u', now(), now())`, string(a.W.Scope.TenantID))
			return err
		})
		if code(err) != "23503" {
			t.Fatalf("unknown project: %v (code %s)", err, code(err))
		}
		// A session may not name a binding of another registration or run.
		err = inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, `INSERT INTO mcp_sessions (tenant_id, project_id, id, run_id, attempt_id, runtime_session, server_id, server_version, secret_name, secret_version,
				protocol, incarnation, state, created_at, updated_at, expires_at)
				VALUES ($1, $2, 'mcs_t033_0123456789abcdef', 'run_unbound', 'att', 'rts', $3, 1, 'a-b', 1, 'p', 'i', 'active', now(), now(), now() + interval '1 hour')`,
				string(a.W.Scope.TenantID), string(a.W.Scope.ProjectID), string(a.Reg.ID))
			return err
		})
		if code(err) != "23503" {
			t.Fatalf("a session without a run binding: %v (code %s)", err, code(err))
		}
	})

	t.Run("the runtime role cannot delete or rewrite immutable columns", func(t *testing.T) {
		for _, stmt := range []string{
			`DELETE FROM mcp_servers`, `DELETE FROM mcp_sessions`, `DELETE FROM mcp_invocations`, `DELETE FROM mcp_run_bindings`,
			`UPDATE mcp_servers SET tenant_id = tenant_id, project_id = 'p033_x'`, `UPDATE mcp_sessions SET expires_at = now()`,
			`UPDATE mcp_run_bindings SET server_version = 9`, `UPDATE mcp_invocations SET tool = 'x'`,
		} {
			err := inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error { _, err := tx.Exec(bg, stmt); return err })
			if code(err) != "42501" {
				t.Errorf("%s: %v (code %s), want permission denied", stmt, err, code(err))
			}
		}
	})

	t.Run("a deleted registration and its version rules are enforced by the database", func(t *testing.T) {
		reg := a.Reg
		next, err := reg.Delete(reg.Version, mcpproxy.Actor{ID: mcptest.Admin}, s.RT.Clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Store.ReplaceRegistration(bg, mcpproxy.Mutation{Actor: mcpproxy.Actor{ID: mcptest.Admin}, Now: s.RT.Clock.Now()}, next, reg.Version); err != nil {
			t.Fatal(err)
		}
		// Not even a stale replay can touch a tombstone.
		if err := s.Store.ReplaceRegistration(bg, mcpproxy.Mutation{Actor: mcpproxy.Actor{ID: mcptest.Admin}, Now: s.RT.Clock.Now()}, next, reg.Version); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("replay on a tombstone: %v", err)
		}
		if _, err := s.RT.Owner.Exec(bg, `UPDATE mcp_servers SET version = version + 1, endpoint = 'https://other.example.com/' WHERE id = $1`, string(reg.ID)); err == nil {
			t.Fatal("a tombstone was changed")
		}
		if _, err := s.RT.Owner.Exec(bg, `UPDATE mcp_servers SET version = version + 5 WHERE id = $1`, string(regB.ID)); err == nil {
			t.Fatal("the version skipped")
		}
	})

	t.Run("the tenant context does not leak across pooled connections", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			var cur *string
			if err := rt.QueryRow(bg, `SELECT kodama_tenant_id()`).Scan(&cur); err != nil {
				t.Fatal(err)
			}
			if cur != nil {
				t.Fatalf("a pooled connection kept tenant %q", *cur)
			}
			var n int
			if err := rt.QueryRow(bg, `SELECT count(*) FROM mcp_servers`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("an unscoped read saw %d rows (%v)", n, err)
			}
			_ = inTenant(a.W.Scope.TenantID, func(tx pgx.Tx) error { return nil })
		}
	})

	t.Run("no relation stores a credential, argument or remote session identifier", func(t *testing.T) {
		rows, err := s.RT.Owner.Query(bg, `SELECT table_name, column_name FROM information_schema.columns WHERE table_name LIKE 'mcp\_%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var tbl, col string
			if err := rows.Scan(&tbl, &col); err != nil {
				t.Fatal(err)
			}
			switch col {
			case "token", "bearer", "credential", "arguments", "args", "result", "payload", "remote_session", "remote_session_id", "value":
				t.Errorf("%s.%s may hold content that must not be stored", tbl, col)
			}
		}
	})
}
