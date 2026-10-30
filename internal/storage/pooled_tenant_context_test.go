package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

type probeResult struct {
	pid      int
	setting  string // "<null>" when the setting was never created on this session
	projects int
}

// probe inspects the pooled connection outside any WithTenant transaction.
func probe(t testing.TB, pool *pgxpool.Pool) probeResult {
	t.Helper()
	var r probeResult
	err := pool.QueryRow(testCtx(t), `SELECT pg_backend_pid(),
		coalesce(current_setting('kodama.tenant_id', true), '<null>'),
		(SELECT count(*) FROM projects)`).Scan(&r.pid, &r.setting, &r.projects)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return r
}

func (r probeResult) clean() bool {
	return (r.setting == "" || r.setting == "<null>") && r.projects == 0
}

func inTenant(t testing.TB, pool *pgxpool.Pool, tenant domain.TenantID) (pid, visible int) {
	t.Helper()
	err := storage.WithTenant(testCtx(t), pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT pg_backend_pid(), (SELECT count(*) FROM projects)").Scan(&pid, &visible)
	})
	if err != nil {
		t.Fatalf("WithTenant(%s): %v", tenant, err)
	}
	return pid, visible
}

// Test_pooled_tenant_context covers IT-04, IT-05 and IT-A03: select it with
//
//	go test -run pooled_tenant_context ./internal/storage/
func Test_pooled_tenant_context(t *testing.T) {
	e := newEnv(t)
	pool := e.runtime(t, 1) // one connection: every call below reuses it

	t.Run("tenant_sees_only_own_rows", func(t *testing.T) {
		_, a := inTenant(t, pool, tenantA)
		_, b := inTenant(t, pool, tenantB)
		if a != 3 || b != 5 {
			t.Fatalf("visible projects: A=%d B=%d, want 3 and 5", a, b)
		}
	})

	assertClean := func(t *testing.T, pid int, what string) {
		t.Helper()
		r := probe(t, pool)
		if r.pid != pid {
			t.Fatalf("%s: probe ran on backend %d, not the reused connection %d; the check would be vacuous", what, r.pid, pid)
		}
		if !r.clean() {
			t.Fatalf("%s: tenant context survived on the pooled connection: %+v", what, r)
		}
	}

	t.Run("after_commit", func(t *testing.T) {
		pid, n := inTenant(t, pool, tenantA)
		if n != 3 {
			t.Fatalf("visible = %d", n)
		}
		assertClean(t, pid, "commit")
	})

	t.Run("after_rollback", func(t *testing.T) {
		var pid int
		sentinel := fmt.Errorf("business failure")
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "UPDATE projects SET display_name = 'changed'"); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			return sentinel
		})
		if err != sentinel {
			t.Fatalf("err = %v, want the callback's error unwrapped", err)
		}
		assertClean(t, pid, "rollback")
		var changed int
		if err := e.mig.QueryRow(testCtx(t), "SELECT count(*) FROM projects WHERE display_name = 'changed'").Scan(&changed); err != nil || changed != 0 {
			t.Fatalf("rolled-back update persisted: %d rows, err %v", changed, err)
		}
	})

	t.Run("after_sql_error", func(t *testing.T) {
		var pid int
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "SELECT 1/0") // aborts the transaction
			return err
		})
		requireCode(t, err, "22012", "division by zero")
		assertClean(t, pid, "sql error")
	})

	t.Run("after_panic", func(t *testing.T) {
		var pid int
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Fatalf("recovered %v, want the original panic value", r)
				}
			}()
			_ = storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				panic("boom")
			})
			t.Fatal("WithTenant returned after a panic")
		}()
		assertClean(t, pid, "panic")
	})

	t.Run("after_cancel_between_statements", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testCtx(t))
		defer cancel()
		var pid int
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			cancel()
			return ctx.Err()
		})
		if err == nil {
			t.Fatal("cancelled transaction reported success")
		}
		assertClean(t, pid, "cancel")
	})

	t.Run("after_cancel_mid_query", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(testCtx(t), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT pg_sleep(30)")
			return err
		})
		if err == nil || time.Since(start) > 10*time.Second {
			t.Fatalf("mid-query cancellation: err = %v after %s", err, time.Since(start))
		}
		if r := probe(t, pool); !r.clean() { // pool may have replaced the connection
			t.Fatalf("tenant context survived: %+v", r)
		}
	})

	t.Run("commit_failure_does_not_leak", func(t *testing.T) {
		// A deferred constraint violation fails at COMMIT, after fn succeeded.
		e.exec(t, `CREATE TABLE commit_probe_fk (id int PRIMARY KEY)`)
		e.exec(t, `CREATE TABLE commit_probe_ref (id int REFERENCES commit_probe_fk (id) DEFERRABLE INITIALLY DEFERRED)`)
		e.exec(t, fmt.Sprintf(`GRANT ALL ON commit_probe_ref, commit_probe_fk TO %q`, e.db.RuntimeRole))
		var pid int
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO commit_probe_ref VALUES (1)")
			return err
		})
		requireCode(t, err, "23503", "deferred FK failing at commit")
		assertClean(t, pid, "commit failure")
	})

	t.Run("tenant_b_cannot_touch_tenant_a", func(t *testing.T) {
		err := storage.WithTenant(testCtx(t), pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM projects WHERE tenant_id = 'tenant-a'").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("B reads %d of A's projects", n)
			}
			for _, q := range []string{
				"SELECT count(*) FROM memberships WHERE tenant_id = 'tenant-a'",
				"SELECT count(*) FROM audit_events WHERE tenant_id = 'tenant-a'",
				"SELECT count(*) FROM tenants WHERE id = 'tenant-a'",
			} {
				var visible int
				if err := tx.QueryRow(ctx, q).Scan(&visible); err != nil {
					return err
				}
				if visible != 0 {
					t.Errorf("B sees A's rows: %s = %d", q, visible)
				}
			}
			ct, err := tx.Exec(ctx, "UPDATE projects SET display_name = 'pwned' WHERE tenant_id = 'tenant-a'")
			if err != nil {
				return err
			}
			if ct.RowsAffected() != 0 {
				t.Errorf("B updated %d of A's projects", ct.RowsAffected())
			}
			ct, err = tx.Exec(ctx, "DELETE FROM memberships WHERE tenant_id = 'tenant-a'")
			if err != nil {
				return err
			}
			if ct.RowsAffected() != 0 {
				t.Errorf("B deleted %d of A's memberships", ct.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unset_context_fails_closed", func(t *testing.T) {
		if r := probe(t, pool); !r.clean() {
			t.Fatalf("unset context sees rows: %+v", r)
		}
		for _, table := range []string{"tenants", "projects", "users", "memberships", "audit_events"} {
			var n int
			if err := pool.QueryRow(testCtx(t), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s without context: count=%d err=%v, want 0", table, n, err)
			}
		}
		_, err := pool.Exec(testCtx(t), "INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ('tenant-a', 'px', 'px', '{}')")
		requireCode(t, err, "42501", "insert without tenant context")
		// An empty-string setting must also fail closed.
		err = storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if _, err := tx.Exec(ctx, "SELECT set_config('kodama.tenant_id', '', true)"); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("empty context sees %d rows", n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid_tenant_id_rejected", func(t *testing.T) {
		called := false
		err := storage.WithTenant(testCtx(t), pool, "a' OR '1'='1", func(context.Context, pgx.Tx) error { called = true; return nil })
		if err == nil || called {
			t.Fatalf("invalid tenant accepted: err=%v called=%v", err, called)
		}
	})

	t.Run("unknown_tenant_sees_nothing", func(t *testing.T) {
		if _, n := inTenant(t, pool, "no-such-tenant"); n != 0 {
			t.Fatalf("unknown tenant sees %d projects", n)
		}
	})

	t.Run("concurrent_tenants_never_leak", func(t *testing.T) {
		shared := e.runtime(t, 3) // fewer connections than goroutines
		const workers, iterations = 12, 30
		want := map[domain.TenantID]int{tenantA: 3, tenantB: 5}
		var wg sync.WaitGroup
		errs := make(chan error, workers*iterations*2)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					tenant := tenantA
					if (w+i)%2 == 1 {
						tenant = tenantB
					}
					err := storage.WithTenant(testCtx(t), shared, tenant, func(ctx context.Context, tx pgx.Tx) error {
						var setting string
						var total, foreign int
						if err := tx.QueryRow(ctx, `SELECT current_setting('kodama.tenant_id'), pg_sleep(0.002)::text IS NOT NULL, count(*),
							count(*) FILTER (WHERE tenant_id <> current_setting('kodama.tenant_id')) FROM projects`).Scan(&setting, new(bool), &total, &foreign); err != nil {
							return err
						}
						if setting != string(tenant) || total != want[tenant] || foreign != 0 {
							return fmt.Errorf("worker %d iteration %d: tenant %s saw setting=%q total=%d foreign=%d", w, i, tenant, setting, total, foreign)
						}
						return nil
					})
					if err != nil {
						errs <- err
						return
					}
					// Outside a transaction the shared pool must be empty-handed.
					var n int
					if err := shared.QueryRow(testCtx(t), "SELECT count(*) FROM projects").Scan(&n); err != nil {
						errs <- err
						return
					}
					if n != 0 {
						errs <- fmt.Errorf("worker %d: context leaked to a plain pool query (%d rows)", w, n)
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}
