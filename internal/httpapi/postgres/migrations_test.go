package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func Test_idempotency_tenant_isolation(t *testing.T) {
	e := newEnv(t)
	ctx := testCtx(t)

	// Seed one record per tenant through the real adapter.
	for _, s := range []struct{ token, tenant, project string }{{"alice", "tenant-a", "pa1"}, {"alice", "tenant-b", "pb1"}} {
		if r := e.create(s.token, s.tenant, s.project, "k", `{"name":"x"}`); r.Code != 201 {
			t.Fatalf("seed: %d %s", r.Code, r.Body)
		}
	}

	t.Run("migration reapplication is safe", func(t *testing.T) {
		res, err := storage.Migrate(ctx, e.mig, manifest(t), storage.MigrateOptions{RuntimeRole: e.db.RuntimeRole})
		if err != nil || len(res.Applied) != 0 || res.Skipped != 2 {
			t.Fatalf("reapply: %+v, %v", res, err)
		}
		if n := e.count(t, `SELECT count(*) FROM schema_migrations`); n != 2 {
			t.Fatalf("recorded %d migrations, want the baseline's two", n)
		}
	})

	t.Run("table is tenant owned with forced RLS and minimal grants", func(t *testing.T) {
		if n := e.count(t, `SELECT count(*) FROM pg_class WHERE relname = 'idempotency' AND relrowsecurity AND relforcerowsecurity`); n != 1 {
			t.Fatal("row level security is not enabled and forced")
		}
		if n := e.count(t, `SELECT count(*) FROM pg_policies WHERE tablename = 'idempotency' AND policyname = 'tenant_isolation' AND qual LIKE '%kodama_tenant_id()%' AND with_check LIKE '%kodama_tenant_id()%'`); n != 1 {
			t.Fatal("tenant policy missing")
		}
		rows, err := e.mig.Query(ctx, `SELECT privilege_type FROM information_schema.role_table_grants WHERE table_name = 'idempotency' AND grantee = $1 ORDER BY 1`, e.db.RuntimeRole)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			got = append(got, p)
		}
		if strings.Join(got, ",") != "DELETE,INSERT,SELECT" {
			t.Fatalf("runtime privileges = %v", got)
		}
		var super, bypass bool
		if err := e.rt.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
			t.Fatalf("runtime role is privileged: %v %v %v", super, bypass, err)
		}
	})

	t.Run("runtime role sees only the context tenant", func(t *testing.T) {
		count := func(tenant string) (n int, other int) {
			err := storage.WithTenant(ctx, e.rt, tenantFor(tenant), func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency`).Scan(&n); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `SELECT count(*) FROM idempotency WHERE tenant_id <> $1`, tenant).Scan(&other)
			})
			if err != nil {
				t.Fatal(err)
			}
			return n, other
		}
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			if n, other := count(tenant); n != 1 || other != 0 {
				t.Errorf("%s sees %d rows, %d foreign", tenant, n, other)
			}
		}
	})

	t.Run("absent context and pooled reuse see and write nothing", func(t *testing.T) {
		one := e.db.RuntimePool(ctx, 1) // every statement reuses the same connection
		if err := storage.WithTenant(ctx, one, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("in context: %d %v", n, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := one.QueryRow(ctx, `SELECT count(*) FROM idempotency`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("after the transaction the same connection sees %d rows (%v)", n, err)
		}
		_, err := one.Exec(ctx, `INSERT INTO idempotency (tenant_id, principal, method, canonical_route, key, request_hash, response_status, response_body, expires_at)
			VALUES ('tenant-a', 'p', 'POST', '/r', 'k2', decode(repeat('00', 32), 'hex'), 200, '{}', now() + interval '1 hour')`)
		if sqlState(err) != "42501" {
			t.Fatalf("insert without context: %v", err)
		}
		if _, err := one.Exec(ctx, `DELETE FROM idempotency`); err != nil {
			t.Fatal(err)
		}
		if got := e.records(t); got != 2 {
			t.Fatalf("a context-free delete removed records: %d left", got)
		}
	})

	t.Run("a tenant cannot write, update or delete another tenant's record", func(t *testing.T) {
		err := storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO idempotency (tenant_id, principal, method, canonical_route, key, request_hash, response_status, response_body, expires_at)
				VALUES ('tenant-b', 'p', 'POST', '/r', 'k3', decode(repeat('00', 32), 'hex'), 200, '{}', now() + interval '1 hour')`)
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v", err)
		}
		err = storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM idempotency WHERE tenant_id = 'tenant-b'`)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("deleted %d foreign rows", tag.RowsAffected())
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		err = storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE idempotency SET response_status = 200`)
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("update by runtime role: %v", err)
		}
		if e.records(t) != 2 {
			t.Fatalf("records = %d", e.records(t))
		}
	})

	t.Run("composite ownership rejects another tenant's project", func(t *testing.T) {
		// As the owner (bypassing policies is not possible: FORCE applies), the
		// foreign key alone must reject tenant-a pointing at tenant-b's project.
		_, err := e.mig.Exec(ctx, `INSERT INTO idempotency (tenant_id, principal, method, canonical_route, key, project_id, request_hash, response_status, response_body, expires_at)
			VALUES ('tenant-a', 'p', 'POST', '/r', 'k4', 'pb1', decode(repeat('00', 32), 'hex'), 200, '{}', now() + interval '1 hour')`)
		if sqlState(err) != "23503" {
			t.Fatalf("cross-owner project accepted: %v", err)
		}
		// Through the adapter, nothing the callback wrote survives.
		id := identity("alice", "tenant-a", "pb1", "k5", "b")
		_, err = e.idem.Execute(ctx, httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
				return okOutcome("w_x"), nil
			}})
		if err == nil {
			t.Fatal("record for another tenant's project was stored")
		}
		if e.records(t) != 2 {
			t.Fatalf("records = %d", e.records(t))
		}
	})

	t.Run("table constraints bound stored values", func(t *testing.T) {
		bad := map[string]string{
			"hash length":    `request_hash => decode('00', 'hex')`,
			"status":         `response_status => 500`,
			"key syntax":     `key => 'has space'`,
			"method":         `method => 'GET'`,
			"unpaired ref":   `target_kind => 'widget'`,
			"headers array":  `response_headers => '[]'::jsonb`,
			"expiry order":   `expires_at => now() - interval '1 hour'`,
			"body too large": `response_body => decode(repeat('00', 1048577), 'hex')`,
		}
		for name, override := range bad {
			cols := map[string]string{
				"tenant_id": `'tenant-a'`, "principal": `'p'`, "method": `'POST'`, "canonical_route": `'/r'`, "key": `'kc'`,
				"request_hash": `decode(repeat('00', 32), 'hex')`, "response_status": `200`, "response_body": `'{}'`,
				"response_headers": `'{}'::jsonb`, "expires_at": `now() + interval '1 hour'`,
			}
			col, val, _ := strings.Cut(override, " => ")
			cols[col] = val
			var names, vals []string
			for k, v := range cols {
				names = append(names, k)
				vals = append(vals, v)
			}
			_, err := e.mig.Exec(ctx, `INSERT INTO idempotency (`+strings.Join(names, ",")+`) VALUES (`+strings.Join(vals, ",")+`)`)
			if sqlState(err) != "23514" {
				t.Errorf("%s accepted or wrong error: %v", name, err)
			}
		}
	})
}
