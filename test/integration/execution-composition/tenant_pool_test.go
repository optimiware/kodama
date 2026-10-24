package composition_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// poolContext reads the tenant setting and the visible runs through the pool
// outside any transaction of ours, as the next borrower of the connection would.
func poolContext(t testing.TB, e *Env) (setting string, runs int) {
	t.Helper()
	ctx := context.Background()
	if err := e.Pool.QueryRow(ctx, `SELECT coalesce(current_setting('`+storage.TenantSetting+`', true), '')`).Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if err := e.Pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	return setting, runs
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// Test_composition_tenant_pool: SD-04, SD-10, IT-03, IT-06, WS-11 (acceptance
// `composition_tenant_pool`). One runtime connection serves both tenants through
// success, rollback, cancellation and guard denial with no tenant context left
// behind; foreign tenant, project, run, attempt and session bindings are
// refused; row-level security is what limits an inspection query that omits
// every predicate; and the runtime role is neither owner, superuser nor
// BYPASSRLS. Real PostgreSQL; no NATS, S3 or Git.
func Test_composition_tenant_pool(t *testing.T) {
	ctx := context.Background()
	e := New(t, Options{SingleConn: true})
	if e.Pool.Stat().MaxConns() != 1 {
		t.Fatalf("the composition pool has %d connections", e.Pool.Stat().MaxConns())
	}
	a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
	b := e.Run(e.NewProject(ScopeB), Job("build", readRun, labelJob))
	a2 := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
	b2 := e.Run(e.NewProject(ScopeB), Job("build", readRun, labelJob))
	clean := func(step string) {
		t.Helper()
		if setting, runs := poolContext(t, e); setting != "" || runs != 0 {
			t.Fatalf("after %s the pooled connection carries tenant %q and shows %d runs", step, setting, runs)
		}
	}
	clean("setup")

	t.Run("one connection is reused across tenants after success, rollback, cancellation and denial", func(t *testing.T) {
		// success, tenant A
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("a")))); err != nil {
			t.Fatal(err)
		}
		clean("A's success")
		// rollback, tenant B
		e.Faults.Commit(2)
		if _, err := e.Submit(b, Request(t, Completed(outputsOf("b")))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatal(err)
		}
		e.Faults.Clear()
		clean("B's rollback")
		// guard denial: A's session on B's route and B's session on A's
		if code, _ := e.doToken(b.ID(), a.Token, "result", mustJSON(t, Request(t, Completed(outputsOf("x"))))); code != 401 {
			t.Fatalf("%d", code)
		}
		if _, _, err := e.Guarded.Update(rpgSession(ctx, a2), b2.Scope, b2.Run.ID, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
			t.Error("a foreign session reached the mutation")
			return execution.Effects{}, nil
		}); err == nil {
			t.Fatal("a foreign session mutated the run")
		}
		clean("the guard denial")
		// cancellation, tenant A then B
		if err := e.CancelErr(a2); err != nil {
			t.Fatal(err)
		}
		clean("A's cancellation")
		if err := e.CancelErr(b2); err != nil {
			t.Fatal(err)
		}
		clean("B's cancellation")
		// and B's retry succeeds on the same connection
		if _, err := e.Submit(b, Request(t, Completed(outputsOf("b")))); err != nil {
			t.Fatal(err)
		}
		clean("B's retry")
		for _, c := range []struct {
			at    *Attempt
			label string
		}{{a, "a"}, {b, "b"}} {
			if got := e.Aggregate(c.at).Step("build").Outputs["label"].String(); got != c.label {
				t.Fatalf("tenant %s accepted %q", c.at.Scope.TenantID, got)
			}
		}
	})

	t.Run("row-level security limits queries that omit every predicate", func(t *testing.T) {
		for _, tenant := range []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID} {
			err := e.inTenant(tenant, func(ctx context.Context, tx pgx.Tx) error {
				for _, table := range []string{"runs", "step_runs", "step_attempts", "attempt_controls", "run_history", "execution_reservations",
					"runtime_grants", "secret_pins", "workspaces", "outbox", "audit"} {
					var n, foreign int
					if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM `+table, string(tenant)).Scan(&n, &foreign); err != nil {
						return err
					}
					if foreign != 0 {
						t.Errorf("tenant %s sees %d foreign rows of %s", tenant, foreign, table)
					}
					if (table == "runs" || table == "runtime_grants") && n == 0 {
						t.Errorf("tenant %s sees no %s of its own", tenant, table)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// The owner sees both: the isolation is the policy, not the data.
		if n := e.Count(`SELECT count(DISTINCT tenant_id) FROM runtime_grants`); n != 2 {
			t.Fatalf("%d tenants hold grants", n)
		}
	})

	t.Run("foreign tenant, project, run, attempt and session bindings are refused", func(t *testing.T) {
		victim := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		before := e.Snap(ScopeB.TenantID)
		// Execution: another tenant's run under this scope, or this tenant's run under another's.
		for name, scope := range map[string]domain.Scope{
			"tenant B scope for A's run":  {TenantID: ScopeB.TenantID, ProjectID: ScopeA.ProjectID},
			"A tenant, B's project":       {TenantID: ScopeA.TenantID, ProjectID: ScopeB.ProjectID},
			"B tenant, A's project, pair": ScopeB,
		} {
			_, _, err := e.RT.Update(ctx, scope, a.Run.ID, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
				t.Errorf("%s reached the mutation", name)
				return execution.Effects{}, nil
			})
			if !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// Runtime authority: B's transaction cannot revoke A's grant, by scope or by identifier.
		err := e.inTenant(ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// Another tenant's scope inside B's transaction: row-level security
			// hides the grant, so nothing is revoked (or the call is refused).
			if n, err := rtpg.RevokeAttempt(ctx, tx, ScopeA, victim.ID(), e.X.Clock.Now()); err == nil && n != 0 {
				t.Errorf("a revocation under another tenant's scope revoked %d grants", n)
			}
			n, err := rtpg.RevokeAttempt(ctx, tx, ScopeB, victim.ID(), e.X.Clock.Now())
			if err != nil || n != 0 {
				t.Errorf("a foreign attempt id revoked %d grants (%v)", n, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if e.GrantRevoked(victim) {
			t.Fatal("another tenant's transaction revoked this tenant's grant")
		}
		if got := e.Snap(ScopeB.TenantID); got != before {
			t.Fatalf("a refused binding changed state:\n before %+v\n after  %+v", before, got)
		}
		// Sessions: another run's, another tenant's and another project's.
		other := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		for name, c := range map[string]struct {
			sess  *Attempt
			scope domain.Scope
			run   domain.RunID
		}{
			"another run of the tenant": {other, other.Scope, a2.Run.ID},
			"another tenant":            {b2, a2.Scope, a2.Run.ID},
			"another project":           {other, domain.Scope{TenantID: ScopeA.TenantID, ProjectID: ScopeB.ProjectID}, other.Run.ID},
		} {
			_, _, err := e.Guarded.Update(rpgSession(ctx, c.sess), c.scope, c.run, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
				t.Errorf("%s reached the mutation", name)
				return execution.Effects{}, nil
			})
			if err == nil {
				t.Fatalf("%s mutated the run", name)
			}
		}
	})

	t.Run("composite ownership is enforced by the database, not only by predicates", func(t *testing.T) {
		// A's attempt under B's project, and B's tenant id inside A's transaction.
		for name, c := range map[string]struct {
			tenant domain.TenantID
			row    [3]string // tenant, project, attempt
			want   string
		}{
			"foreign project for the attempt": {ScopeA.TenantID, [3]string{string(ScopeA.TenantID), string(ScopeB.ProjectID), string(a2.ID())}, "23503"},
			"foreign tenant row":              {ScopeA.TenantID, [3]string{string(ScopeB.TenantID), string(ScopeB.ProjectID), string(b2.ID())}, "42501"},
		} {
			err := e.inTenant(c.tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO execution_reservations (tenant_id, project_id, attempt_id, run_id, held_at) VALUES ($1, $2, $3, 'run_x', now())`,
					c.row[0], c.row[1], c.row[2])
				return err
			})
			if got := sqlState(err); got != c.want {
				t.Fatalf("%s: SQLSTATE %q want %q (%v)", name, got, c.want, err)
			}
		}
	})

	t.Run("the runtime role is neither owner, superuser nor BYPASSRLS", func(t *testing.T) {
		var super, bypass bool
		var owned int
		if err := e.Pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
			t.Fatal(err)
		}
		if err := e.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		if super || bypass || owned != 0 {
			t.Fatalf("the runtime role is superuser=%v bypassrls=%v and owns %d tables", super, bypass, owned)
		}
		var forced int
		e.X.Scalar(&forced, `SELECT count(*) FROM pg_class WHERE relname IN ('runs', 'step_attempts', 'attempt_controls', 'run_history', 'runtime_grants', 'execution_reservations') AND relrowsecurity AND relforcerowsecurity`)
		if forced != 6 {
			t.Fatalf("%d of 6 tables force row-level security", forced)
		}
	})
}
