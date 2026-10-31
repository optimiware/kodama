package storage_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// Migration 007 stores the decision reason on a signal record so a refused
// signal that lost to a cancellation replays on its own. This file holds its
// infrastructure-free evidence (Test_unit_storage_signal_decision_reason: what
// the text declares, cross-checked against the kernel's vocabulary) and its
// real-database evidence (Test_signal_decision_reason_postgres: every write is
// made as the non-owner runtime role against a real service).

const (
	// insertCancelledSignal and cancelledValues bracket the optional decision_reason column
	// and value of a refused signal that lost to a cancelled decision.
	insertCancelledSignal = `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest,
  applied, refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_version, decided_at`
	cancelledValues = `) VALUES ('tenant-a', 'pa1', 'user-alice', $1, $2, 'decision', repeat('a', 64), false, 'decided', 'r1', 's2', 'wid-review',
  'cancelled', 2, now()`
)

func signalReasonStatements(t testing.TB) []string {
	return sqlStatements(migrationSQL(t, reasonMigration))
}

// Test_unit_storage_signal_decision_reason: the migration is appended last and is
// expand-only, adds exactly one nullable column limited to the kernel's cancelled
// reasons, requires a reason for every new cancelled record without validating
// history, and leaves retention, grants and row security untouched.
func Test_unit_storage_signal_decision_reason(t *testing.T) {
	stmts := signalReasonStatements(t)

	t.Run("registered_last_and_expand_only", func(t *testing.T) {
		var ids []string
		for _, m := range storage.BaseContribution().Migrations {
			ids = append(ids, m.ID)
		}
		if i := slices.Index(ids, reasonMigration); i != len(ids)-1 || ids[i-1] != waitMigration {
			t.Fatalf("%s is not appended directly after %s: %v", reasonMigration, waitMigration, ids)
		}
		text := strings.Join(stmts, ";\n")
		if regexp.MustCompile(`(?i)\bDROP\b|\bTRUNCATE\b|\bDELETE\b|\bINSERT\b|\bUPDATE\b|\bRENAME\b|\bCREATE (TABLE|TRIGGER|FUNCTION|POLICY|INDEX)\b`).MatchString(text) {
			t.Error("a destructive, data-writing or object-creating statement in the expand migration")
		}
		if len(stmts) != 2 {
			t.Fatalf("migration 007 has %d statements, want the column and its constraint: %v", len(stmts), stmts)
		}
		for _, s := range stmts {
			if !strings.HasPrefix(s, "ALTER TABLE signal_records") {
				t.Errorf("a statement outside signal_records: %s", s)
			}
		}
	})

	t.Run("one_nullable_column_limited_to_the_kernel_reasons", func(t *testing.T) {
		cols := addedColumns(stmts, "signal_records")
		if len(cols) != 1 {
			t.Fatalf("added columns = %v, want exactly decision_reason", cols)
		}
		if _, ok := cols["decision_reason"]; !ok {
			t.Fatalf("decision_reason is not added: %v", cols)
		}
		// The definition runs to the end of the ADD COLUMN clause, parentheses included.
		def := regexp.MustCompile(`ADD COLUMN decision_reason (.*)$`).FindStringSubmatch(strings.Join(strings.Fields(stmts[0]), " "))[1]
		if strings.Contains(strings.ToUpper(def), "NOT NULL") || strings.Contains(strings.ToUpper(def), "DEFAULT") {
			t.Errorf("the column must be nullable with no default so existing rows are not rewritten: %s", def)
		}
		// The value set is exactly the kernel's cancelled-decision reasons.
		m := regexp.MustCompile(`IN \(([^)]*)\)`).FindStringSubmatch(def)
		if m == nil {
			t.Fatalf("the column has no closed value set: %s", def)
		}
		var got []string
		for _, v := range strings.Split(m[1], ",") {
			got = append(got, strings.Trim(strings.TrimSpace(v), "'"))
		}
		slices.Sort(got)
		want := []string{string(runtime.ReasonRunCancelled), string(runtime.ReasonRunFailed)}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("reasons = %v, want the kernel's %v", got, want)
		}
		// Every reason the kernel accepts on a cancelled decision is storable.
		for _, r := range want {
			d := runtime.Decision{Kind: runtime.DecisionCancelled, Reason: runtime.Reason(r), Version: 2, At: time.Now()}
			if err := d.Validate(); err != nil {
				t.Errorf("the kernel rejects a cancelled decision with %q: %v", r, err)
			}
		}
	})

	t.Run("a_new_cancelled_record_requires_its_reason_and_history_is_not_validated", func(t *testing.T) {
		var constraint string
		for _, s := range statementsMatching(stmts, `ADD CONSTRAINT signal_records_decision_reason_shape`) {
			constraint = s
		}
		if constraint == "" {
			t.Fatal("signal_records_decision_reason_shape is not added")
		}
		compact := strings.Join(strings.Fields(constraint), " ")
		for _, need := range []string{
			"decision_kind IS NOT DISTINCT FROM 'cancelled'", "decision_reason IS NOT NULL", ") = (", "NOT VALID",
		} {
			if !strings.Contains(compact, need) {
				t.Errorf("the constraint lacks %q: %s", need, compact)
			}
		}
		// A NOT VALID constraint never scans or rewrites the existing rows and is not validated.
		if strings.Contains(strings.ToUpper(strings.Join(stmts, " ")), "VALIDATE CONSTRAINT") {
			t.Error("history must not be validated by the migration")
		}
	})

	t.Run("retention_grants_and_row_security_are_unchanged", func(t *testing.T) {
		text := strings.Join(stmts, ";\n")
		if regexp.MustCompile(`(?i)\b(GRANT|REVOKE|POLICY|ROW LEVEL SECURITY|expires_at|ON DELETE)\b`).MatchString(text) {
			t.Errorf("migration 007 changes grants, policies, retention or removal: %s", text)
		}
		// 004's table-level append-only grant covers the new column and nothing grants an update.
		sql004 := migrationSQL(t, expandMigration)
		if !strings.Contains(sql004, "GRANT SELECT, INSERT ON signal_records TO {{runtime_role}}") {
			t.Error("004 no longer grants the runtime role SELECT and INSERT on signal_records")
		}
		for _, id := range []string{expandMigration, durableMigration, waitMigration, reasonMigration} {
			if regexp.MustCompile(`(?i)(UPDATE|DELETE)[^;]*ON signal_records[^;]*TO`).MatchString(migrationSQL(t, id)) {
				t.Errorf("%s grants the runtime role a write to signal_records beyond insert", id)
			}
		}
		if regexp.MustCompile(`(?i)ADD COLUMN[^;]*expires_at`).MatchString(text) {
			t.Error("a signal record must never expire")
		}
	})

	t.Run("earlier_migrations_are_untouched", func(t *testing.T) {
		// 007 is a new file: the checksums of the applied migrations are those the earlier tests pin.
		sums := map[string]string{}
		for _, m := range storage.BaseContribution().Migrations {
			sums[m.ID] = m.Checksum()
		}
		if sums[waitMigration] == sums[reasonMigration] || sums[reasonMigration] == "" {
			t.Errorf("migration 007 shares a checksum or has none: %v", sums)
		}
	})
}

// Test_signal_decision_reason_postgres records a cancelled decision as the
// runtime role, removes the wait, step and run the way ordinary retention does,
// and reads the reason back from signal_records alone. It needs PostgreSQL: it
// skips in the unit layer and fails under KODAMA_REQUIRE_POSTGRES=1 without a service.
func Test_signal_decision_reason_postgres(t *testing.T) {
	t.Run("a_cancelled_decision_replays_its_reason_after_the_wait_is_removed", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")

		// The run is cancelled: the wait is decided cancelled (its reason lives on the row), and a
		// signal that arrives afterwards is refused as decided, recording the decision it lost to.
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE run_steps SET wait_state = 'cancelled', wait_version = 2, wait_decided_at = now(), wait_reason = 'run_cancelled' WHERE id = 's2'`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, insertCancelledSignal+`, decision_reason`+cancelledValues+`, 'run_cancelled')`, "late-approval", "sig-late")
			return err
		}))
		if got := scalar[string](t, pool, tenantA, `SELECT wait_reason FROM run_steps WHERE id = 's2'`); got != "run_cancelled" {
			t.Fatalf("wait reason before retention = %q", got)
		}

		// Ordinary retention removes the wait row, its step and the run: the signal record has no
		// reference to any of them and survives.
		e.exec(t, `DELETE FROM run_steps WHERE tenant_id = 'tenant-a' AND run_id = 'r1'`)
		if n := count(t, e.mig, `SELECT count(*) FROM run_steps WHERE run_id = 'r1' AND id = 's2'`); n != 0 {
			t.Fatalf("the wait row survived the removal: %d", n)
		}
		e.exec(t, `UPDATE tasks SET current_run_id = NULL WHERE tenant_id = 'tenant-a' AND current_run_id = 'r1'`)
		e.exec(t, `DELETE FROM runs WHERE tenant_id = 'tenant-a' AND id = 'r1'`)
		if n := count(t, e.mig, `SELECT count(*) FROM runs WHERE id = 'r1'`); n != 0 {
			t.Fatalf("the run survived the removal: %d", n)
		}

		// The reason is read from signal_records alone, as the runtime role bound to the tenant.
		type replay struct {
			kind, reason, refusal string
			applied               bool
			version               int64
		}
		var got replay
		must(t, asTenant(t, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT decision_kind, decision_reason, refusal_reason, applied, decision_version
FROM signal_records WHERE caller_principal = 'user-alice' AND caller_key = 'late-approval'`).Scan(&got.kind, &got.reason, &got.refusal, &got.applied, &got.version)
		}))
		if got != (replay{"cancelled", "run_cancelled", "decided", false, 2}) {
			t.Fatalf("replayed record = %+v", got)
		}
		// Another tenant, bound as itself, never sees the record or its reason.
		if n := scalar[int](t, e.runtime(t, 2), tenantB, `SELECT count(*) FROM signal_records WHERE decision_reason IS NOT NULL`); n != 0 {
			t.Errorf("tenant B reads %d reasons of tenant A", n)
		}
	})

	t.Run("both_cancellation_reasons_are_stored_and_a_resolved_record_has_none", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")
		for i, reason := range []string{"run_cancelled", "run_failed"} {
			key := []string{"k-cancelled", "k-failed"}[i]
			must(t, runtimeExec(t, pool, tenantA, insertCancelledSignal+`, decision_reason`+cancelledValues+`, $3)`, key, "sig-"+key, reason))
			if got := scalar[string](t, pool, tenantA, `SELECT decision_reason FROM signal_records WHERE caller_key = $1`, key); got != reason {
				t.Errorf("%s: stored reason %q", key, got)
			}
		}
		// A winning resolved signal carries no reason; a record of it is stored without one.
		must(t, runtimeExec(t, pool, tenantA, `UPDATE run_steps SET wait_version = wait_version WHERE id = 's2'`))
		must(t, runtimeExec(t, pool, tenantA, insertSignal, "s2", "wid-review"))
		if n := scalar[int](t, pool, tenantA, `SELECT count(*) FROM signal_records WHERE decision_kind = 'resolved' AND decision_reason IS NULL`); n != 1 {
			t.Errorf("resolved records without a reason = %d, want 1", n)
		}
	})

	t.Run("a_new_record_without_or_with_a_wrong_reason_is_refused", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")

		requireCode(t, runtimeExec(t, pool, tenantA, insertCancelledSignal+cancelledValues+`)`, "k1", "sig-1"),
			"23514", "a cancelled record without its reason")
		requireCode(t, runtimeExec(t, pool, tenantA, insertCancelledSignal+`, decision_reason`+cancelledValues+`, 'run_complete')`, "k2", "sig-2"),
			"23514", "a cancelled record with an unknown reason")
		requireCode(t, runtimeExec(t, pool, tenantA, insertCancelledSignal+`, decision_reason`+cancelledValues+`, '')`, "k3", "sig-3"),
			"23514", "a cancelled record with an empty reason")
		// A reason on a record that is not a cancellation is refused: a resolved winner, an expiry,
		// a refusal with no decision, and a tombstone.
		requireCode(t, runtimeExec(t, pool, tenantA, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_answer, decision_actor_ref, decision_version, decided_at, decision_reason)
VALUES ('tenant-a', 'pa1', 'user-alice', 'k4', 'sig-4', 'decision', repeat('a', 64), true, 'r1', 's2', 'wid-review', 'resolved', 'approved', 'because',
  'user:alice', 2, now(), 'run_cancelled')`), "23514", "a resolved record with a cancellation reason")
		requireCode(t, runtimeExec(t, pool, tenantA, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_version, decided_at, decision_reason)
VALUES ('tenant-a', 'pa1', 'user-alice', 'k5', 'sig-5', 'decision', repeat('a', 64), false, 'decided', 'r1', 's2', 'wid-review', 'expired', 'expired', 2, now(), 'run_cancelled')`),
			"23514", "an expired record with a reason")
		requireCode(t, runtimeExec(t, pool, tenantA, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  refusal_reason, target_run_id, target_step_id, target_wait_id, decision_reason)
VALUES ('tenant-a', 'pa1', 'user-alice', 'k6', 'sig-6', 'decision', repeat('a', 64), false, 'version_conflict', 'r1', 's2', 'wid-review', 'run_cancelled')`),
			"23514", "a decision-less refusal with a reason")
		requireCode(t, runtimeExec(t, pool, tenantA, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  refusal_reason, decision_reason)
VALUES ('tenant-a', 'pa1', 'user-alice', 'k7', 'sig-7', 'tombstone', repeat('a', 64), false, 'unknown_wait', 'run_failed')`),
			"23514", "a tombstone with a reason")
		if n := count(t, e.mig, `SELECT count(*) FROM signal_records`); n != 0 {
			t.Errorf("%d record(s) stored from refused statements", n)
		}
	})

	t.Run("the_record_stays_append_only_and_leaves_with_its_project", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		openedWaitStep(t, e, "s2", "wid-review", "continue")
		must(t, runtimeExec(t, pool, tenantA, insertCancelledSignal+`, decision_reason`+cancelledValues+`, 'run_cancelled')`, "k1", "sig-1"))

		for what, sql := range map[string]string{
			"rewrite the reason": `UPDATE signal_records SET decision_reason = 'run_failed'`,
			"clear the reason":   `UPDATE signal_records SET decision_reason = NULL`,
			"delete the record":  `DELETE FROM signal_records`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "42501", what)
		}
		// Even the owner cannot clear the reason of a cancelled record: the constraint holds for every writer.
		requireCode(t, tryExec(t, e, `UPDATE signal_records SET decision_reason = NULL`), "23514", "owner clearing the reason")
		if got := count(t, e.mig, `SELECT count(*) FROM signal_records WHERE decision_reason = 'run_cancelled'`); got != 1 {
			t.Fatalf("the reason did not survive the refused writes: %d", got)
		}
		// The record is removed only with its project.
		e.exec(t, `INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ('tenant-a', 'pa9', 'pa9', '{}');
INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_version, decided_at, decision_reason)
VALUES ('tenant-a', 'pa9', 'user-alice', 'gone', 'sig-gone', 'decision', repeat('b', 64), false, 'decided', 'r9', 's9', 'w9', 'cancelled', 2, now(), 'run_failed');
DELETE FROM projects WHERE tenant_id = 'tenant-a' AND id = 'pa9'`)
		var gone, kept int
		must(t, e.mig.QueryRow(testCtx(t), `SELECT count(*) FILTER (WHERE project_id = 'pa9'), count(*) FILTER (WHERE project_id = 'pa1') FROM signal_records`).Scan(&gone, &kept))
		if gone != 0 || kept != 1 {
			t.Errorf("after the project's removal: its records = %d, the other project's = %d", gone, kept)
		}
	})

	t.Run("an_existing_database_upgrades_without_rewriting_history", func(t *testing.T) {
		_, pool, opts := priorSchema(t)
		ctx := testCtx(t)
		base := storage.BaseContribution().Migrations
		upTo006, err := storage.NewManifest(storage.Contribution{Module: "storage", Migrations: base[:6]})
		must(t, err)
		if _, err := storage.Migrate(ctx, pool, upTo006, opts); err != nil {
			t.Fatalf("migrate to 006: %v", err)
		}
		// A cancelled record written before the column existed.
		_, err = pool.Exec(ctx, `
INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest,
  applied, refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-old', 'k-old', 'sig-old', 'decision', repeat('b', 64), false, 'decided', 'r-old', 's-old', 'w-old', 'cancelled', 2, now())`)
		must(t, err)
		if applied(t, pool) != 6 || columnExists(t, pool, "signal_records", "decision_reason") {
			t.Fatal("the prior schema already has the column")
		}

		res, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != reasonMigration || res.Skipped != 6 {
			t.Fatalf("migrate 007: %+v %v", res, err)
		}
		// History is untouched: the old record is present with a null reason.
		var reason *string
		must(t, pool.QueryRow(ctx, `SELECT decision_reason FROM signal_records WHERE caller_key = 'k-old'`).Scan(&reason))
		if reason != nil {
			t.Errorf("the old record was given reason %q", *reason)
		}
		// Every new cancelled record requires its reason.
		_, err = pool.Exec(ctx, `
INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest,
  applied, refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-new', 'k-new', 'sig-new', 'decision', repeat('c', 64), false, 'decided', 'r-old', 's-old', 'w-old2', 'cancelled', 2, now())`)
		requireCode(t, err, "23514", "a new cancelled record without a reason after the upgrade")
		// The constraint is recorded as not validated: the migration did not scan history.
		var validated bool
		must(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'signal_records_decision_reason_shape'`).Scan(&validated))
		if validated {
			t.Error("the reason constraint was validated against history")
		}
		// Reapplying changes nothing.
		again, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		if err != nil || len(again.Applied) != 0 || again.Skipped != 7 {
			t.Fatalf("reapply: %+v %v", again, err)
		}
	})
}
