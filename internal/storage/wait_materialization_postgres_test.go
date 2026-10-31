package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// The tests in this file are the real-database evidence for migration 006. They
// need PostgreSQL: in the unit layer (-tags unit) they skip, and with
// KODAMA_REQUIRE_POSTGRES=1 they fail when no service is configured. Every
// write the runtime role would make is made as that role, which is not the
// owner: no superuser, no BYPASSRLS, only the column grants the migrations give
// it. A few statements run as the owner to show a guard holds for every writer.
//
// The contractual unit tests, free of a database, are
// Test_unit_storage_wait_decision_ordering and Test_unit_storage_materialization_records.

const (
	decideResolved = `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved', wait_answer = 'because',
  wait_actor_ref = 'user:alice', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = $1`
	insertSignal = `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_answer, decision_actor_ref, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-alice', 'k1', 'sig-1', 'decision', repeat('a', 64), true, 'r1', $1, $2, 'resolved', 'approved', 'because',
  'user:alice', 2, now())`
)

// inTenant runs fn in one transaction as the runtime role bound to tenant.
func asTenant(t testing.TB, pool *pgxpool.Pool, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return storage.WithTenant(testCtx(t), pool, tenant, fn)
}

func scalar[T any](t testing.TB, pool *pgxpool.Pool, tenant domain.TenantID, sql string, args ...any) T {
	t.Helper()
	var v T
	must(t, asTenant(t, pool, tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&v) }))
	return v
}

// openedWaitStep records and opens the wait of step id (a wait step of run r1),
// as the migration owner, on a step that is `waiting`.
func openedWaitStep(t testing.TB, e *env, id, waitID, onExpiry string) {
	t.Helper()
	e.exec(t, openWaitSQL(id, waitID, onExpiry))
}

func Test_wait_decision_ordering_postgres(t *testing.T) {
	t.Run("a_decision_is_stored_while_the_step_is_still_waiting", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")

		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, insertSignal, "s2", "wid-review"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, decideResolved, "s2")
			return err
		}))
		type row struct {
			status, state, outcome string
			version                int64
		}
		got := func() (r row) {
			must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, wait_state, wait_outcome, wait_version FROM run_steps WHERE id = 's2'`).Scan(&r.status, &r.state, &r.outcome, &r.version)
			}))
			return r
		}
		if r := got(); r != (row{"waiting", "resolved", "approved", 2}) {
			t.Fatalf("decided wait on a waiting step: %+v", r)
		}

		// The decision is complete and immutable, and a decided wait never reopens.
		for what, sql := range map[string]string{
			"reopened":                      `UPDATE run_steps SET wait_state = 'waiting', wait_version = 3 WHERE id = 's2'`,
			"another outcome":               `UPDATE run_steps SET wait_outcome = 'rejected', wait_version = 3 WHERE id = 's2'`,
			"another outcome, same version": `UPDATE run_steps SET wait_outcome = 'rejected' WHERE id = 's2'`,
			"a bumped version":              `UPDATE run_steps SET wait_version = 3 WHERE id = 's2'`,
			"cleared decision":              `UPDATE run_steps SET wait_state = NULL, wait_version = 3 WHERE id = 's2'`,
			"expired after resolved":        `UPDATE run_steps SET wait_state = 'expired', wait_version = 3, wait_outcome = 'expired', wait_actor_ref = NULL, wait_answer = NULL, wait_consumed_signal_id = NULL WHERE id = 's2'`,
			"cancelled after resolved":      `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 3, wait_reason = 'run_cancelled' WHERE id = 's2'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", what)
			if r := got(); r != (row{"waiting", "resolved", "approved", 2}) {
				t.Fatalf("%s changed the decided wait: %+v", what, r)
			}
		}
		// The owner is bound by the same guard.
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET wait_state = 'waiting', wait_version = 3 WHERE id = 's2'`), "23514", "owner reopening")

		// The step never goes back, and only then settles; a settled step never changes.
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'pending' WHERE id = 's2'`), "23514", "step back to pending")
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'succeeded', finished_at = now() WHERE id = 's2'`))
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'waiting', finished_at = NULL WHERE id = 's2'`), "23514", "settled step set back to waiting")
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'failed' WHERE id = 's2'`), "23514", "settled step changing state")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'waiting', finished_at = NULL WHERE id = 's2'`), "23514", "owner reopening the step")
		if r := got(); r.status != "succeeded" || r.state != "resolved" {
			t.Fatalf("settled: %+v", r)
		}
	})

	t.Run("a_decision_keeps_its_required_content_and_status", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		for i, id := range []string{"w3", "w4", "w5"} {
			addWaitStep(t, e, id, 2+i)
		}
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")
		openedWaitStep(t, e, "w3", "wid-3", "fail")
		openedWaitStep(t, e, "w4", "wid-4", "continue")
		openedWaitStep(t, e, "w5", "wid-5", "fail")

		bad := map[string]struct{ id, sql string }{
			"resolved without an outcome":                   {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_actor_ref = 'user:a', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"resolved with an undeclared outcome":           {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'maybe', wait_actor_ref = 'user:a', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"resolved without an actor":                     {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"resolved without a time":                       {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved', wait_actor_ref = 'user:a', wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"resolved without a signal":                     {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved', wait_actor_ref = 'user:a', wait_decided_at = now() WHERE id = 's2'`},
			"resolved on a failed step":                     {"s2", `UPDATE run_steps SET status = 'failed', finished_at = now(), wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved', wait_actor_ref = 'user:a', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"without raising the version":                   {"s2", `UPDATE run_steps SET wait_state = 'resolved', wait_outcome = 'approved', wait_actor_ref = 'user:a', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`},
			"expired fail with an outcome":                  {"w3", `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_outcome = 'expired', wait_decided_at = now() WHERE id = 'w3'`},
			"expired fail on a succeeded step":              {"w3", `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'expired', wait_version = 2, wait_decided_at = now() WHERE id = 'w3'`},
			"expired fail with an actor":                    {"w3", `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_actor_ref = 'user:a', wait_decided_at = now() WHERE id = 'w3'`},
			"expired continue without the reserved outcome": {"w4", `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_decided_at = now() WHERE id = 'w4'`},
			"expired continue on a failed step":             {"w4", `UPDATE run_steps SET status = 'failed', finished_at = now(), wait_state = 'expired', wait_version = 2, wait_outcome = 'expired', wait_decided_at = now() WHERE id = 'w4'`},
			"expired without a time":                        {"w4", `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_outcome = 'expired' WHERE id = 'w4'`},
			"cancelled with an outcome":                     {"w4", `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 2, wait_outcome = 'approved', wait_decided_at = now(), wait_reason = 'run_cancelled' WHERE id = 'w4'`},
			"cancelled without a reason":                    {"w4", `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 2, wait_decided_at = now() WHERE id = 'w4'`},
			"cancelled on a succeeded step":                 {"w4", `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'cancelled', wait_version = 2, wait_decided_at = now(), wait_reason = 'run_cancelled' WHERE id = 'w4'`},
			"cancelled on a running step":                   {"w4", `UPDATE run_steps SET status = 'running', wait_state = 'cancelled', wait_version = 2, wait_decided_at = now(), wait_reason = 'run_cancelled' WHERE id = 'w4'`},
		}
		for what, c := range bad {
			requireCode(t, runtimeExec(t, pool, tenantA, c.sql), "23514", what)
			if st := scalar[string](t, pool, tenantA, `SELECT coalesce(wait_state, '') FROM run_steps WHERE id = $1`, c.id); st != "waiting" {
				t.Errorf("%s: the refused statement left the wait %q", what, st)
			}
		}

		// The decisions the kernel takes before the step settles are stored.
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_decided_at = now() WHERE id = 'w3'`))
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_outcome = 'expired', wait_decided_at = now() WHERE id = 'w4'`))
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 2, wait_decided_at = now(), wait_reason = 'run_failed' WHERE id = 'w5'`))
		for id, want := range map[string]string{"w3": "expired", "w4": "expired", "w5": "cancelled"} {
			if got := scalar[string](t, pool, tenantA, `SELECT wait_state || '/' || status FROM run_steps WHERE id = $1`, id); got != want+"/waiting" {
				t.Errorf("%s = %s, want %s on a waiting step", id, got, want)
			}
		}
	})

	t.Run("a_wait_cancelled_before_it_opened_leaves_its_step_pending", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		// Identity recorded at admission, never opened; the run's cancellation decides it.
		e.exec(t, `UPDATE run_steps SET wait_id = 'wid-review', wait_version = 1, wait_assignment = 'reviewer' WHERE id = 's2'`)
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 2, wait_decided_at = now(), wait_reason = 'run_cancelled' WHERE id = 's2'`))
		if st := scalar[string](t, pool, tenantA, `SELECT status FROM run_steps WHERE id = 's2'`); st != "pending" {
			t.Fatalf("step = %s, want pending", st)
		}
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'waiting', wait_version = 3 WHERE id = 's2'`), "23514", "reopening a cancelled wait")
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'cancelled', finished_at = now() WHERE id = 's2'`))
	})

	t.Run("the_unopened_wait_has_no_decision_and_no_waiting_status", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		e.exec(t, `UPDATE run_steps SET wait_id = 'wid-review', wait_version = 1 WHERE id = 's2'`)
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'waiting' WHERE id = 's2'`), "23514", "waiting without being opened")
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_outcome = 'approved',
  wait_actor_ref = 'user:a', wait_decided_at = now(), wait_consumed_signal_id = 'sig-1' WHERE id = 's2'`), "23514", "resolving a wait that never opened")
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_decided_at = now() WHERE id = 's2'`), "23514", "a decision time on an unopened wait")
		// An open wait still carries no decision.
		e.exec(t, openWaitSQL("s2", "wid-review", "fail", "wait_version", "2"))
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_outcome = 'approved' WHERE id = 's2'`), "23514", "an open wait with an outcome")
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'pending' WHERE id = 's2'`), "23514", "an open wait on a pending step")
	})

	t.Run("a_refused_write_leaves_no_partial_row", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		addWaitStep(t, e, "w3", 2)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "fail")
		openedWaitStep(t, e, "w3", "wid-3", "fail")

		err := asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			// A valid decision, then an invalid one in the same transaction.
			if _, err := tx.Exec(ctx, `UPDATE run_steps SET wait_state = 'expired', wait_version = 2, wait_decided_at = now() WHERE id = 's2'`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE run_steps SET wait_state = 'resolved', wait_version = 2, wait_decided_at = now() WHERE id = 'w3'`)
			return err
		})
		requireCode(t, err, "23514", "the second decision")
		for _, id := range []string{"s2", "w3"} {
			if st := scalar[string](t, pool, tenantA, `SELECT wait_state FROM run_steps WHERE id = $1`, id); st != "waiting" {
				t.Errorf("%s kept %q from the refused transaction", id, st)
			}
		}

		// A decision whose signal record is missing is refused at commit and leaves nothing.
		err = asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, decideResolved, "s2")
			return err
		})
		requireCode(t, err, "23503", "a decision without its signal record")
		if st := scalar[string](t, pool, tenantA, `SELECT wait_state FROM run_steps WHERE id = 's2'`); st != "waiting" {
			t.Errorf("a refused commit left the wait %q", st)
		}
	})
}

func Test_outbox_index_postgres(t *testing.T) {
	e := newEnv(t)
	pool := e.runtime(t, 2)
	def := scalar[string](t, pool, tenantA, `SELECT indexdef FROM pg_indexes WHERE indexname = 'audit_events_unpublished'`)
	if !strings.Contains(def, "(tenant_id, created_at, id)") || !strings.Contains(def, "WHERE (published_at IS NULL)") {
		t.Fatalf("index = %s", def)
	}
	// The index changes no row: the seeded events are untouched, and a page in creation order
	// is served by it (the planner may choose a scan on a table this small, so the choice is forced).
	e.exec(t, `INSERT INTO audit_events (tenant_id, id, actor_type, actor_id, event_type, resource_type, resource_id)
SELECT 'tenant-a', 'ev-' || n, 'system', 'system', 'seed', 'project', 'pa1' FROM generate_series(1, 50) n`)
	var plan string
	must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `EXPLAIN SELECT id FROM audit_events WHERE tenant_id = 'tenant-a' AND published_at IS NULL ORDER BY created_at, id LIMIT 10`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan += line + "\n"
		}
		return rows.Err()
	}))
	if !strings.Contains(plan, "audit_events_unpublished") || strings.Contains(plan, "Sort") {
		t.Fatalf("an unpublished page is not served in order by the index:\n%s", plan)
	}
}

func Test_step_outputs_immutable_postgres(t *testing.T) {
	e := newEnv(t)
	seedPipelineRuntime(t, e)
	e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind) VALUES
  ('tenant-a', 'pa1', 'r1', 's3', 'lint', 5, 'registry.example/lint@sha256:abc', 'container'),
  ('tenant-a', 'pa1', 'r1', 's4', 'scan', 6, 'registry.example/scan@sha256:abc', 'container')`)
	pool := e.runtime(t, 2)
	outputs := func(id string) string {
		return scalar[string](t, pool, tenantA, `SELECT outputs::text FROM run_steps WHERE id = $1`, id)
	}

	// Outputs are not recorded before the step settles, nor on a step that does not succeed.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'running' WHERE id IN ('s1', 's3', 's4')`))
	requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET outputs = '{"commit": "abc"}' WHERE id = 's1'`), "23514", "outputs on a running step")
	requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'failed', finished_at = now(), outputs = '{"commit": "abc"}' WHERE id = 's3'`), "23514", "outputs on a failed step")
	if got := outputs("s1"); got != "{}" {
		t.Fatalf("a refused write left outputs %s", got)
	}

	// The settling commit records them.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), outputs = '{"commit": "abc"}' WHERE id = 's1'`))
	if got := outputs("s1"); got != `{"commit": "abc"}` {
		t.Fatalf("outputs = %s", got)
	}
	// A replay of the settling commit leaves them as recorded; any other change is refused.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET outputs = '{"commit": "abc"}' WHERE id = 's1'`))
	for what, sql := range map[string]string{
		"rewritten": `UPDATE run_steps SET outputs = '{"commit": "other"}' WHERE id = 's1'`,
		"extended":  `UPDATE run_steps SET outputs = '{"commit": "abc", "extra": "x"}' WHERE id = 's1'`,
		"cleared":   `UPDATE run_steps SET outputs = '{}' WHERE id = 's1'`,
	} {
		requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", "outputs "+what)
	}
	if got := outputs("s1"); got != `{"commit": "abc"}` {
		t.Fatalf("outputs after refusals = %s", got)
	}
	// The migration owner is held to the same guard.
	requireCode(t, tryExec(t, e, `UPDATE run_steps SET outputs = '{"commit": "owner"}' WHERE id = 's1'`), "23514", "owner rewriting outputs")
	// A failed step gains none later either.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'failed', finished_at = now() WHERE id = 's3'`))
	requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET outputs = '{"late": "x"}' WHERE id = 's3'`), "23514", "outputs on a settled failed step")
	// An unrelated update of a settled step does not touch outputs and is allowed.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET log_uri = 's3://logs/s1' WHERE id = 's1'`))
	if got := outputs("s1"); got != `{"commit": "abc"}` {
		t.Fatalf("outputs after an unrelated update = %s", got)
	}
}

// Test_step_outputs_reopen_bypass_postgres: outputs cannot be rewritten by first
// reopening the settled step. The bypass is two updates (back to an unsettled state,
// then settle again with other outputs); the first must be refused for every role,
// whether or not the step recorded a result code or any outputs.
func Test_step_outputs_reopen_bypass_postgres(t *testing.T) {
	e := newEnv(t)
	seedPipelineRuntime(t, e)
	e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind) VALUES
  ('tenant-a', 'pa1', 'r1', 's3', 'lint', 5, 'registry.example/lint@sha256:abc', 'container'),
  ('tenant-a', 'pa1', 'r1', 's4', 'scan', 6, 'registry.example/scan@sha256:abc', 'container'),
  ('tenant-a', 'pa1', 'r1', 's5', 'pack', 7, 'registry.example/pack@sha256:abc', 'container'),
  ('tenant-a', 'pa1', 'r1', 's6', 'sign', 8, 'registry.example/sign@sha256:abc', 'container')`)
	pool := e.runtime(t, 2)
	// s1 settles with outputs, s3 without any, s4 with a result code and outputs; neither s1 nor
	// s3 records a result_code, which is what once left them reopenable.
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'running' WHERE id IN ('s1', 's3', 's4', 's5', 's6')`))
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), outputs = '{"commit": "abc"}' WHERE id = 's1'`))
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'succeeded', finished_at = now() WHERE id = 's3'`))
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), attempt = 1, exit_code = 0, result_code = 'succeeded', outputs = '{"commit": "def"}' WHERE id = 's4'`))
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'failed', finished_at = now() WHERE id = 's5'`))
	must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET status = 'skipped', finished_at = now() WHERE id = 's6'`))
	read := func(id string) string {
		return scalar[string](t, pool, tenantA, `SELECT status || ' ' || outputs::text FROM run_steps WHERE id = $1`, id)
	}
	want := map[string]string{"s1": `succeeded {"commit": "abc"}`, "s3": "succeeded {}", "s4": `succeeded {"commit": "def"}`, "s5": "failed {}", "s6": "skipped {}"}
	for id, w := range want {
		if got := read(id); got != w {
			t.Fatalf("%s = %s, want %s", id, got, w)
		}
	}

	roles := map[string]func(sql string) error{
		"runtime": func(sql string) error { return runtimeExec(t, pool, tenantA, sql) },
		"owner":   func(sql string) error { return tryExec(t, e, sql) },
	}
	for role, exec := range roles {
		for id := range want {
			for _, back := range []string{"running", "pending", "waiting"} {
				// The first update of the bypass: reopen the settled step.
				err := exec(`UPDATE run_steps SET status = '` + back + `', finished_at = NULL WHERE id = '` + id + `'`)
				if err == nil {
					t.Errorf("%s reopened settled step %s as %s", role, id, back)
					continue
				}
				if code := pgCode(err); code != "23514" {
					t.Errorf("%s reopening %s as %s: SQLSTATE %q (%v), want 23514", role, id, back, code, err)
				}
			}
			// Another settled state is no way around it either.
			requireCode(t, exec(`UPDATE run_steps SET status = 'cancelled' WHERE id = '`+id+`'`), "23514", role+" re-settling "+id+" differently")
			// The two statements together, and the second alone, cannot rewrite the outputs.
			requireCode(t, exec(`UPDATE run_steps SET status = 'running', finished_at = NULL, outputs = '{"commit": "rewritten"}' WHERE id = '`+id+`'`), "23514", role+" reopening with new outputs "+id)
			requireCode(t, exec(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), outputs = '{"commit": "rewritten"}' WHERE id = '`+id+`'`), "23514", role+" settling again with new outputs "+id)
			requireCode(t, exec(`UPDATE run_steps SET outputs = '{"commit": "rewritten"}' WHERE id = '`+id+`'`), "23514", role+" rewriting "+id)
		}
		for id, w := range want {
			if got := read(id); got != w {
				t.Fatalf("after the %s refusals %s = %s, want %s", role, id, got, w)
			}
		}
	}

	// Identical replay of the settling commit is not a change, for either role.
	for role, exec := range roles {
		for id, w := range map[string]string{"s1": `{"commit": "abc"}`, "s3": `{}`, "s4": `{"commit": "def"}`} {
			if err := exec(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), outputs = '` + w + `' WHERE id = '` + id + `'`); err != nil {
				t.Errorf("%s replaying the settlement of %s: %v", role, id, err)
			}
		}
	}
	for id, w := range want {
		if got := read(id); got != w {
			t.Fatalf("after the replays %s = %s, want %s", id, got, w)
		}
	}
}

const (
	matInsert = `INSERT INTO run_materializations (tenant_id, project_id, run_id, step_key, attempt, materialization_id, issued_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`
	matRevoke = `UPDATE run_materializations SET state = 'revoked', revoked_at = $2, revocation_evidence = $3 WHERE materialization_id = $1`
)

func matRef(t testing.TB, scope domain.Scope, run domain.RunID, step domain.StepID, attempt int) runtime.MaterializationRef {
	t.Helper()
	ref, err := runtime.NewMaterializationRef(scope, run, step, attempt)
	must(t, err)
	return ref
}

func issue(ctx context.Context, tx pgx.Tx, ref runtime.MaterializationRef, at time.Time) error {
	_, err := tx.Exec(ctx, matInsert, ref.Scope.TenantID, ref.Scope.ProjectID, ref.RunID, ref.StepKey, ref.Attempt, ref.ID, at)
	return err
}

func Test_materialization_records_postgres(t *testing.T) {
	scopeA := domain.Scope{TenantID: tenantA, ProjectID: "pa1"}
	issuedAt := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	t.Run("issue_and_revoke_once_with_revoked_final", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		ref := matRef(t, scopeA, "r1", "build", 1)

		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return issue(ctx, tx, ref, issuedAt) }))
		state := func() (s string, revoked *time.Time, evidence *string) {
			must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT state, revoked_at, revocation_evidence FROM run_materializations WHERE materialization_id = $1`, ref.ID).Scan(&s, &revoked, &evidence)
			}))
			return
		}
		if s, r, ev := state(); s != "issued" || r != nil || ev != nil {
			t.Fatalf("issued record: %s %v %v", s, r, ev)
		}
		// The identity is issued once.
		err := asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return issue(ctx, tx, ref, issuedAt.Add(time.Minute)) })
		requireCode(t, err, "23505", "issuing the same attempt twice")

		revokedAt := issuedAt.Add(time.Hour)
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, matRevoke, ref.ID, revokedAt, "resolver:ok")
			return err
		}))
		if s, r, ev := state(); s != "revoked" || r == nil || !r.Equal(revokedAt) || ev == nil || *ev != "resolver:ok" {
			t.Fatalf("revoked record: %s %v %v", s, r, ev)
		}
		// Revoked is final: nothing about the revocation changes, it never returns to issued,
		// and the identity and issue time never move.
		for what, sql := range map[string]string{
			"another time":     `UPDATE run_materializations SET revoked_at = revoked_at + interval '1 minute' WHERE materialization_id = '` + ref.ID + `'`,
			"another evidence": `UPDATE run_materializations SET revocation_evidence = 'resolver:other' WHERE materialization_id = '` + ref.ID + `'`,
			"cleared evidence": `UPDATE run_materializations SET revocation_evidence = NULL WHERE materialization_id = '` + ref.ID + `'`,
			"back to issued":   `UPDATE run_materializations SET state = 'issued', revoked_at = NULL, revocation_evidence = NULL WHERE materialization_id = '` + ref.ID + `'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", "revoked record "+what)
			requireCode(t, tryExec(t, e, sql), "23514", "owner, revoked record "+what)
		}
		// Repeating the recorded revocation is not a change.
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, matRevoke, ref.ID, revokedAt, "resolver:ok")
			return err
		}))
		if s, _, _ := state(); s != "revoked" {
			t.Fatalf("state = %s", s)
		}
		// The runtime role cannot update the identity or the issue time (no privilege), and the
		// owner is stopped by the guard.
		for _, col := range []string{"issued_at = issued_at + interval '1 second'", "attempt = 2", "step_key = 'review'", "materialization_id = 'mat:other'", "tenant_id = 'tenant-b'"} {
			requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE run_materializations SET `+col), "42501", "runtime update of "+col)
			requireCode(t, tryExec(t, e, `UPDATE run_materializations SET `+col), "23514", "owner update of "+col)
		}
		// Nothing is deletable by the runtime role.
		requireCode(t, runtimeExec(t, pool, tenantA, `DELETE FROM run_materializations`), "42501", "runtime delete")
	})

	t.Run("the_record_is_what_the_kernel_would_store", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		plan := runtime.Plan{Digest: "sha256:mat", Steps: []runtime.PlanStep{
			{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}}}}
		run, err := runtime.NewRun(scopeA, "r1", "pl1", plan, 2)
		must(t, err)
		must(t, run.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone))

		// Every transition of an existing record is taken by the kernel and by the table alike.
		type rec = runtime.MaterializationRecord
		base := func(attempt int, state runtime.MaterializationState) rec {
			return rec{Ref: matRef(t, scopeA, "r1", "build", attempt), State: state, IssuedAt: issuedAt}
		}
		for i, tc := range []struct {
			name       string
			prev, next runtime.MaterializationState
			issuedAt   time.Duration
			ok         bool
		}{
			{"issued stays issued", runtime.MaterializationIssued, runtime.MaterializationIssued, 0, true},
			{"issued is revoked", runtime.MaterializationIssued, runtime.MaterializationRevoked, 0, true},
			{"revoked repeats", runtime.MaterializationRevoked, runtime.MaterializationRevoked, 0, true},
			{"revoked never returns", runtime.MaterializationRevoked, runtime.MaterializationIssued, 0, false},
			{"the issue time moves", runtime.MaterializationIssued, runtime.MaterializationIssued, time.Second, false},
			{"the issue time moves with revocation", runtime.MaterializationIssued, runtime.MaterializationRevoked, time.Second, false},
		} {
			attempt := i + 1
			prev, next := base(attempt, tc.prev), base(attempt, tc.next)
			next.IssuedAt = issuedAt.Add(tc.issuedAt)
			run.Steps[0].Attempt = attempt
			_, kernelErr := runtime.CheckMaterializations(run, []rec{prev}, []rec{next})
			if (kernelErr == nil) != tc.ok {
				t.Fatalf("%s: kernel refusal = %v, want ok=%v", tc.name, kernelErr, tc.ok)
			}
			// The table: issue (and, if the previous state is revoked, revoke), then apply the next record.
			must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				if err := issue(ctx, tx, prev.Ref, prev.IssuedAt); err != nil {
					return err
				}
				if tc.prev == runtime.MaterializationRevoked {
					_, err := tx.Exec(ctx, matRevoke, prev.Ref.ID, issuedAt.Add(time.Hour), "resolver:ok")
					return err
				}
				return nil
			}))
			// The issue time is not updatable by the runtime role, so the kernel's "moves the issue time" is
			// refused by the privilege as the runtime role and by the guard as the owner.
			var dbErr error
			switch {
			case tc.issuedAt != 0:
				dbErr = tryExec(t, e, `UPDATE run_materializations SET issued_at = $2 WHERE materialization_id = $1`, next.Ref.ID, next.IssuedAt)
			case tc.next == runtime.MaterializationRevoked && tc.prev == runtime.MaterializationRevoked:
				dbErr = asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, matRevoke, next.Ref.ID, issuedAt.Add(time.Hour), "resolver:ok")
					return err
				})
			case tc.next == runtime.MaterializationRevoked:
				dbErr = asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, matRevoke, next.Ref.ID, issuedAt.Add(time.Hour), "resolver:ok")
					return err
				})
			default:
				dbErr = runtimeExec(t, pool, tenantA, `UPDATE run_materializations SET state = $2 WHERE materialization_id = $1`, next.Ref.ID, string(tc.next))
			}
			if (dbErr == nil) != tc.ok {
				t.Errorf("%s: the table's refusal = %v, the kernel's = %v", tc.name, dbErr, kernelErr)
			}
			if dbErr != nil {
				requireCode(t, dbErr, "23514", tc.name)
			}
		}
	})

	t.Run("the_key_identity_and_references_are_enforced", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		ref := matRef(t, scopeA, "r1", "build", 1)
		insert := func(mut func(args []any)) error {
			args := []any{string(ref.Scope.TenantID), string(ref.Scope.ProjectID), string(ref.RunID), string(ref.StepKey), ref.Attempt, ref.ID, issuedAt}
			mut(args)
			return asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, matInsert, args...)
				return err
			})
		}
		requireCode(t, insert(func(a []any) { a[5] = "mat:tenant-a:pa1:r1:build:2" }), "23514", "an identity of another attempt")
		requireCode(t, insert(func(a []any) { a[5] = "mat:tenant-a:pa1:r1:review:1" }), "23514", "an identity of another step")
		requireCode(t, insert(func(a []any) { a[4] = 0; a[5] = "mat:tenant-a:pa1:r1:build:0" }), "23514", "attempt zero")
		requireCode(t, insert(func(a []any) { a[3] = "nostep"; a[5] = "mat:tenant-a:pa1:r1:nostep:1" }), "23503", "a step the run does not have")
		requireCode(t, insert(func(a []any) { a[2] = "nope"; a[5] = "mat:tenant-a:pa1:nope:build:1" }), "23503", "a run that does not exist")
		requireCode(t, insert(func(a []any) { a[1] = "pa2"; a[5] = "mat:tenant-a:pa2:r1:build:1" }), "23503", "another project's run")
		requireCode(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO run_materializations (tenant_id, project_id, run_id, step_key, attempt, materialization_id, state, issued_at)
VALUES ('tenant-a', 'pa1', 'r1', 'build', 1, $1, 'revoked', now())`, ref.ID)
			return err
		}), "23514", "a record born revoked without a revocation time")
		requireCode(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO run_materializations (tenant_id, project_id, run_id, step_key, attempt, materialization_id, issued_at, revocation_evidence)
VALUES ('tenant-a', 'pa1', 'r1', 'build', 1, $1, now(), 'resolver:ok')`, ref.ID)
			return err
		}), "23514", "evidence on an issued record")
		if n := scalar[int](t, pool, tenantA, `SELECT count(*) FROM run_materializations`); n != 0 {
			t.Fatalf("%d rows after seven refusals", n)
		}
		must(t, insert(func([]any) {}))
		requireCode(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, matRevoke, ref.ID, issuedAt.Add(-time.Second), "resolver:ok")
			return err
		}), "23514", "a revocation before the issue")
		requireCode(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, matRevoke, ref.ID, issuedAt.Add(time.Second), "")
			return err
		}), "23514", "empty evidence")
	})

	t.Run("tenants_are_isolated", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		e.exec(t, `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot) VALUES ('tenant-b', 'pb1', 'rb1', 'plb', '{}');
INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind) VALUES
  ('tenant-b', 'pb1', 'rb1', 'sb1', 'build', 0, 'registry.example/build@sha256:abc', 'container')`)
		pool := e.runtime(t, 2)
		scopeB := domain.Scope{TenantID: tenantB, ProjectID: "pb1"}
		refA, refB := matRef(t, scopeA, "r1", "build", 1), matRef(t, scopeB, "rb1", "build", 1)
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return issue(ctx, tx, refA, issuedAt) }))
		must(t, asTenant(t, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error { return issue(ctx, tx, refB, issuedAt) }))

		ids := func(tenant domain.TenantID) []string {
			var out []string
			must(t, asTenant(t, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT materialization_id FROM run_materializations ORDER BY 1`)
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
			}))
			return out
		}
		if got := ids(tenantA); len(got) != 1 || got[0] != refA.ID {
			t.Fatalf("tenant A sees %v", got)
		}
		if got := ids(tenantB); len(got) != 1 || got[0] != refB.ID {
			t.Fatalf("tenant B sees %v", got)
		}
		// Tenant B can neither write a row for tenant A nor change or remove one.
		err := asTenant(t, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			return issue(ctx, tx, matRef(t, scopeA, "r1", "build", 2), issuedAt)
		})
		requireCode(t, err, "42501", "tenant B writing a tenant A row")
		var touched int64
		must(t, asTenant(t, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, matRevoke, refA.ID, issuedAt.Add(time.Hour), "resolver:ok")
			touched = tag.RowsAffected()
			return err
		}))
		if touched != 0 || scalar[string](t, pool, tenantA, `SELECT state FROM run_materializations WHERE materialization_id = $1`, refA.ID) != "issued" {
			t.Fatalf("tenant B revoked tenant A's material (%d rows)", touched)
		}
		// The owner's own reads are not what bounds the runtime: the runtime role has neither bypass nor ownership.
		if super := scalar[bool](t, pool, tenantA, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`); super {
			t.Fatal("the runtime role bypasses row security")
		}
		if owner := scalar[bool](t, pool, tenantA, `SELECT pg_has_role(current_user, relowner, 'USAGE') FROM pg_class WHERE relname = 'run_materializations'`); owner {
			t.Fatal("the runtime role owns the table")
		}
	})

	t.Run("a_refused_write_leaves_no_partial_row", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		first, second := matRef(t, scopeA, "r1", "build", 1), matRef(t, scopeA, "r1", "build", 2)
		err := asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := issue(ctx, tx, first, issuedAt); err != nil {
				return err
			}
			if err := issue(ctx, tx, second, issuedAt); err != nil {
				return err
			}
			// The same identity again: refused, and the transaction with it.
			return issue(ctx, tx, second, issuedAt)
		})
		requireCode(t, err, "23505", "a duplicate inside a transaction")
		if n := scalar[int](t, pool, tenantA, `SELECT count(*) FROM run_materializations`); n != 0 {
			t.Fatalf("%d rows survived a refused transaction", n)
		}
		// A failed revocation leaves the record issued.
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return issue(ctx, tx, first, issuedAt) }))
		err = asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, matRevoke, first.ID, issuedAt.Add(time.Hour), "resolver:ok"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE run_materializations SET revocation_evidence = 'x' WHERE materialization_id = $1`, first.ID)
			return err
		})
		requireCode(t, err, "23514", "changing a revocation just recorded")
		if s := scalar[string](t, pool, tenantA, `SELECT state FROM run_materializations WHERE materialization_id = $1`, first.ID); s != "issued" {
			t.Fatalf("state = %s after a refused transaction", s)
		}
	})
}
