package storage_test

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	expandMigration  = "004_pipeline_runtime_expand.sql"
	durableMigration = "005_durable_run_state.sql"
)

// seedPipelineRuntime adds pipelines, tasks, runs and two steps to the standard
// environment: a container step (s1) and a wait step (s2) in run r1.
func seedPipelineRuntime(t testing.TB, e *env) {
	t.Helper()
	e.exec(t, `
INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES
  ('tenant-a', 'pa1', 'pl1', 'sdlc', 'api', '{}'),
  ('tenant-a', 'pa2', 'pl2', 'sdlc', 'api', '{}'),
  ('tenant-b', 'pb1', 'plb', 'sdlc', 'api', '{}');
INSERT INTO tasks (tenant_id, project_id, id, title, state) VALUES
  ('tenant-a', 'pa1', 't1', 'one', 'open'),
  ('tenant-a', 'pa1', 't2', 'two', 'open'),
  ('tenant-a', 'pa2', 't3', 'three', 'open');
INSERT INTO runs (tenant_id, project_id, id, pipeline_id, task_id, definition_snapshot) VALUES
  ('tenant-a', 'pa1', 'r1', 'pl1', 't1', '{}'),
  ('tenant-a', 'pa1', 'r2', 'pl1', 't2', '{}'),
  ('tenant-a', 'pa2', 'r3', 'pl2', 't3', '{}');
INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind) VALUES
  ('tenant-a', 'pa1', 'r1', 's1', 'build', 0, 'registry.example/build@sha256:abc', 'container'),
  ('tenant-a', 'pa1', 'r1', 's2', 'review', 1, NULL, 'wait');`)
}

// openWaitSQL opens the wait of a step as the migration owner. Overrides replace
// the default value (a SQL expression) of a column.
func openWaitSQL(step, waitID, onExpiry string, overrides ...string) string {
	cols := []struct{ name, value string }{
		{"status", "'waiting'"}, {"wait_id", "'" + waitID + "'"}, {"wait_assignment", "'reviewer'"},
		{"wait_correlation", `'{"candidate":"c0ffee"}'`}, {"wait_input", "'approval'"},
		{"wait_outcomes", `'["approved","rejected"]'`}, {"wait_approving", "'approved'"},
		{"wait_on_expiry", "'" + onExpiry + "'"}, {"wait_deadline_at", "now() + interval '1 hour'"},
		{"wait_state", "'waiting'"}, {"wait_version", "1"},
	}
	over := map[string]string{}
	for i := 0; i+1 < len(overrides); i += 2 {
		over[overrides[i]] = overrides[i+1]
	}
	var set []string
	for _, c := range cols {
		v := c.value
		if o, ok := over[c.name]; ok {
			v = o
			delete(over, c.name)
		}
		set = append(set, c.name+" = "+v)
	}
	for name, v := range over { // columns outside the default set, such as wait_request
		set = append(set, name+" = "+v)
	}
	return "UPDATE run_steps SET " + strings.Join(set, ", ") + " WHERE id = '" + step + "'"
}

func addWaitStep(t testing.TB, e *env, step string, ordinal int) {
	t.Helper()
	e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind)
VALUES ('tenant-a', 'pa1', 'r1', $1, $2, $3, NULL, 'wait')`, step, "wait"+step, ordinal)
}

func tryExec(t testing.TB, e *env, sql string, args ...any) error {
	t.Helper()
	_, err := e.mig.Exec(testCtx(t), sql, args...)
	return err
}

func runtimeExec(t testing.TB, pool *pgxpool.Pool, tenant domain.TenantID, sql string, args ...any) error {
	t.Helper()
	return storage.WithTenant(testCtx(t), pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// Test_unit_storage_pipeline_runtime_expand is the infrastructure-free check of
// the expand migration: the shape of its SQL (statement structure, constraint,
// index and grant inventory, forced RLS on every table it creates, append-only
// signal records, no destructive statement). It proves what the text declares,
// not what PostgreSQL enforces; Test_pipeline_runtime_expand_postgres executes
// the constraints, guards, forced RLS under a non-owner role, and rollback
// against a real service and is the evidence for those.
func Test_unit_storage_pipeline_runtime_expand(t *testing.T) {
	t.Run("manifest_and_text", func(t *testing.T) {
		ms := storage.BaseContribution().Migrations
		var ids []string
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		want := []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", expandMigration, durableMigration}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("manifest = %v, want %v", ids, want)
		}
		sql := ms[3].SQL
		code := regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")
		// Expand and validate only: nothing a deployed database holds is dropped.
		if m := regexp.MustCompile(`(?im)^\s*(DROP\s+(TABLE|COLUMN|SCHEMA|TRIGGER|POLICY|INDEX)|TRUNCATE|DELETE\s+FROM)\b`).FindString(sql); m != "" {
			t.Errorf("destructive statement %q in the expand migration", m)
		}
		if m := regexp.MustCompile(`(?i)DROP\s+COLUMN`).FindString(sql); m != "" {
			t.Errorf("column drop in the expand migration")
		}
		// Only the two constraints that required an image / listed step states are replaced.
		drops := regexp.MustCompile(`(?im)^ALTER TABLE \w+ DROP CONSTRAINT (\w+);`).FindAllStringSubmatch(sql, -1)
		var dropped []string
		for _, d := range drops {
			dropped = append(dropped, d[1])
		}
		if strings.Join(dropped, ",") != "run_steps_image_check,run_steps_status_check" {
			t.Errorf("dropped constraints = %v", dropped)
		}
		for _, legacy := range []string{"task_workflow", "parked_at", "resume_state", "blocked_by_id", "resolved_at"} {
			if regexp.MustCompile(`(?i)(DROP|RENAME)[^;]*` + legacy).MatchString(code) {
				t.Errorf("legacy column %s is touched", legacy)
			}
		}
		for _, want := range []string{
			"ALTER TABLE signal_records ENABLE ROW LEVEL SECURITY", "ALTER TABLE signal_records FORCE ROW LEVEL SECURITY",
			"GRANT SELECT, INSERT ON signal_records TO {{runtime_role}}", "tasks_current_run_fk",
			"pipelines_name_unique", "run_steps_wait_due",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("migration lacks %q", want)
			}
		}
		if strings.Contains(sql, "ON signal_records TO {{runtime_role}};\nGRANT UPDATE") || regexp.MustCompile(`(?i)(UPDATE|DELETE)[^;]*ON signal_records`).MatchString(sql) {
			t.Error("signal_records must stay append-only for the runtime role")
		}
		if strings.Contains(sql, "expires") && regexp.MustCompile(`(?i)signal_records[^;]*expires_at`).MatchString(sql) {
			t.Error("signal records must not expire")
		}
	})

	t.Run("statements", func(t *testing.T) { expandStatementShape(t) })
}

// Test_pipeline_runtime_expand_postgres executes the expand migration's
// constraints and guards against real PostgreSQL: new constraints, forced RLS
// under a non-owner runtime role, append-only signal records, and rollback. It
// skips in the unit layer (-tags unit); run it without the tag with
// KODAMA_TEST_POSTGRES_ADMIN_URL set.
func Test_pipeline_runtime_expand_postgres(t *testing.T) {

	t.Run("run_lease_workload_cleanup", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		ok := func(sql string, args ...any) {
			t.Helper()
			if err := tryExec(t, e, sql, args...); err != nil {
				t.Fatalf("%.80q: %v", sql, err)
			}
		}
		// Lease: owner and expiry travel together; the fence only grows, and a new
		// holder must take a higher one.
		requireCode(t, tryExec(t, e, `UPDATE runs SET lease_owner = 'w1' WHERE id = 'r1'`), "23514", "owner without expiry")
		ok(`UPDATE runs SET lease_owner = 'w1', lease_purpose = 'execute', lease_expires_at = now() + interval '1 minute', lease_fence = 1 WHERE id = 'r1'`)
		ok(`UPDATE runs SET lease_expires_at = now() + interval '2 minutes' WHERE id = 'r1'`) // renewal keeps the fence
		requireCode(t, tryExec(t, e, `UPDATE runs SET lease_fence = 0 WHERE id = 'r1'`), "23514", "fence decrease")
		requireCode(t, tryExec(t, e, `UPDATE runs SET lease_owner = 'w2' WHERE id = 'r1'`), "23514", "takeover without expiry")
		requireCode(t, tryExec(t, e, `UPDATE runs SET lease_owner = 'w2', lease_expires_at = now() WHERE id = 'r1'`), "23514", "takeover keeping the fence")
		ok(`UPDATE runs SET lease_owner = 'w2', lease_expires_at = now() + interval '1 minute', lease_fence = 2 WHERE id = 'r1'`)
		ok(`UPDATE runs SET lease_owner = NULL, lease_purpose = NULL, lease_expires_at = NULL WHERE id = 'r1'`) // release keeps the fence
		requireCode(t, tryExec(t, e, `UPDATE runs SET lease_owner = 'w2', lease_expires_at = now(), lease_fence = 2 WHERE id = 'r1'`), "23514", "reacquire without a higher fence")
		var fence int64
		if err := e.mig.QueryRow(testCtx(t), `SELECT lease_fence FROM runs WHERE id = 'r1'`).Scan(&fence); err != nil || fence != 2 {
			t.Fatalf("fence = %d (%v), want 2", fence, err)
		}

		// Resource identity is stable once set.
		ok(`UPDATE runs SET resource_identity = 'run-r1', resource_spec_digest = repeat('a', 64) WHERE id = 'r1'`)
		requireCode(t, tryExec(t, e, `UPDATE runs SET resource_identity = 'run-other' WHERE id = 'r1'`), "23514", "resource identity change")
		requireCode(t, tryExec(t, e, `UPDATE runs SET resource_spec_digest = repeat('b', 64) WHERE id = 'r1'`), "23514", "resource spec change")
		requireCode(t, tryExec(t, e, `UPDATE runs SET resource_spec_digest = 'nothex' WHERE id = 'r2'`), "23514", "bad spec digest")

		// Cleanup is separate from the outcome and keeps retained identities.
		requireCode(t, tryExec(t, e, `UPDATE runs SET cleanup_state = 'retained' WHERE id = 'r1'`), "23514", "retained without identities")
		requireCode(t, tryExec(t, e, `UPDATE runs SET cleanup_state = 'done', retained_resources = '["svc-1"]' WHERE id = 'r1'`), "23514", "done with identities")
		requireCode(t, tryExec(t, e, `UPDATE runs SET cleanup_state = 'sideways' WHERE id = 'r1'`), "23514", "unknown cleanup state")
		requireCode(t, tryExec(t, e, `UPDATE runs SET cleanup_state = 'done', cleanup_next_at = now() WHERE id = 'r1'`), "23514", "retry time after done")
		ok(`UPDATE runs SET cleanup_state = 'retained', cleanup_max_attempts = 5, cleanup_attempts = 5, cleanup_reason = 'cleanup_exhausted', retained_resources = '["svc-1","ws-r1"]' WHERE id = 'r1'`)
		ok(`UPDATE runs SET cleanup_state = 'pending', cleanup_next_at = now() + interval '1 minute' WHERE id = 'r1'`)
		ok(`UPDATE runs SET cleanup_state = 'done', cleanup_next_at = NULL, cleanup_reason = NULL, retained_resources = '[]' WHERE id = 'r1'`)

		// Workload identity, command intent and launch evidence.
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET command_intent = 'launch' WHERE id = 's1'`), "23514", "intent without fence and time")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET command_intent = 'launch', command_intent_fence = 2, command_intent_at = now() WHERE id = 's1'`), "23514", "intent without workload")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET launch_evidence = 'started' WHERE id = 's1'`), "23514", "evidence without workload")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workload_id = 'wl', command_intent = 'explode', command_intent_fence = 2, command_intent_at = now() WHERE id = 's1'`), "23514", "unknown intent")
		ok(`UPDATE run_steps SET workload_id = 'run-r1-step-s1', workload_spec_digest = repeat('c', 64),
  command_intent = 'launch', command_intent_fence = 2, command_intent_at = now(), launch_evidence = 'uncertain' WHERE id = 's1'`)
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workload_id = 'another' WHERE id = 's1'`), "23514", "workload identity change")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workload_spec_digest = repeat('d', 64) WHERE id = 's1'`), "23514", "workload spec change")

		// One step holds the workspace at a time, and only while running.
		e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image)
VALUES ('tenant-a', 'pa1', 'r1', 's5', 'test', 5, 'img')`)
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workspace_held = true WHERE id = 's1'`), "23514", "held while pending")
		ok(`UPDATE run_steps SET status = 'running', workspace_held = true WHERE id = 's1'`)
		ok(`UPDATE run_steps SET status = 'running' WHERE id = 's5'`)
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workspace_held = true WHERE id = 's5'`), "23505", "second holder")
		ok(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), workspace_held = false WHERE id = 's1'`)
		ok(`UPDATE run_steps SET workspace_held = true WHERE id = 's5'`)
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET workspace_held = true WHERE id = 's2'`), "23514", "a wait never holds the workspace")
		// Another run is independent.
		e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, status, workspace_held)
VALUES ('tenant-a', 'pa1', 'r2', 's6', 'build', 0, 'img', 'running', true)`)
	})

	t.Run("wait_shape_and_decision", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		ok := func(sql string, args ...any) {
			t.Helper()
			if err := tryExec(t, e, sql, args...); err != nil {
				t.Fatalf("%.100q: %v", sql, err)
			}
		}
		// Step kind decides what a step carries.
		requireCode(t, tryExec(t, e, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind)
VALUES ('tenant-a', 'pa1', 'r1', 'x1', 'bad', 9, 'img', 'wait')`), "23514", "wait with an image")
		requireCode(t, tryExec(t, e, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image)
VALUES ('tenant-a', 'pa1', 'r1', 'x2', 'bad', 9, NULL)`), "23514", "container without an image")
		requireCode(t, tryExec(t, e, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind)
VALUES ('tenant-a', 'pa1', 'r1', 'x3', 'bad', 9, '', 'container')`), "23514", "container with an empty image")
		requireCode(t, tryExec(t, e, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind)
VALUES ('tenant-a', 'pa1', 'r1', 'x4', 'bad', 9, NULL, 'sidecar')`), "23514", "unknown step kind")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET wait_state = 'waiting' WHERE id = 's1'`), "23514", "container carrying wait state")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET runtime_id = 'rt' WHERE id = 's2'`), "23514", "wait owning a workload")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'waiting' WHERE id = 's2'`), "23514", "waiting before it opened")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'waiting' WHERE id = 's1'`), "23514", "container waiting")

		// An opened wait is complete, bounded and addressable.
		for name, o := range map[string][]string{
			"reserved expired outcome":        {"wait_outcomes", `'["approved","expired"]'`},
			"reserved cancelled":              {"wait_outcomes", `'["cancelled"]'`},
			"approving not declared":          {"wait_approving", `'merged'`},
			"too many outcomes":               {"wait_outcomes", `'["a","b","c","d","e","f","g","h","i"]'`},
			"no outcomes":                     {"wait_outcomes", `'[]'`},
			"five correlation keys":           {"wait_correlation", `'{"a":"1","b":"2","c":"3","d":"4","e":"5"}'`},
			"bad input kind":                  {"wait_input", `'Approval!'`},
			"unknown expiry":                  {"wait_on_expiry", `'retry'`},
			"oversize request":                {"wait_request", `repeat('x', 4097)`},
			"empty assignment":                {"wait_assignment", `''`},
			"no deadline":                     {"wait_deadline_at", `NULL`},
			"neither assigned nor correlated": {"wait_assignment", `NULL`, "wait_correlation", `'{}'`},
		} {
			requireCode(t, tryExec(t, e, openWaitSQL("s2", "w1", "fail", o...)), "23514", name)
		}
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'waiting', wait_state = 'waiting', wait_id = 'w1' WHERE id = 's2'`), "23514", "incomplete open")
		ok(openWaitSQL("s2", "w1", "fail", "wait_request", `'Ship it?'`))

		// Identity is unique, and identity, correlation and expected input are immutable.
		addWaitStep(t, e, "s3", 2)
		requireCode(t, tryExec(t, e, openWaitSQL("s3", "w1", "fail")), "23505", "reused wait identity")
		for name, sql := range map[string]string{
			"identity":    `UPDATE run_steps SET wait_id = 'w9', wait_version = 2 WHERE id = 's2'`,
			"correlation": `UPDATE run_steps SET wait_correlation = '{"candidate":"deadbeef"}', wait_version = 2 WHERE id = 's2'`,
			"outcomes":    `UPDATE run_steps SET wait_outcomes = '["approved"]', wait_version = 2 WHERE id = 's2'`,
			"approving":   `UPDATE run_steps SET wait_approving = 'rejected', wait_version = 2 WHERE id = 's2'`,
			"deadline":    `UPDATE run_steps SET wait_deadline_at = now() + interval '9 days', wait_version = 2 WHERE id = 's2'`,
			"request":     `UPDATE run_steps SET wait_request = 'other', wait_version = 2 WHERE id = 's2'`,
		} {
			requireCode(t, tryExec(t, e, sql), "23514", "immutable "+name)
		}
		// Reassignment is one version-checked step; identity and correlation stay.
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET wait_assignment = 'operator' WHERE id = 's2'`), "23514", "reassignment without a version bump")
		ok(`UPDATE run_steps SET wait_assignment = 'operator', wait_version = 2 WHERE id = 's2'`)

		// Resolution needs a declared outcome, the actor and the consumed signal,
		// whose record exists by commit.
		resolve := func(outcome string, version int, signal string) string {
			return `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = '` + outcome + `',
  wait_actor_ref = 'user-1', wait_decided_at = now(), wait_consumed_signal_id = '` + signal + `', wait_version = ` + strconv.Itoa(version) + ` WHERE id = 's2'`
		}
		requireCode(t, tryExec(t, e, resolve("merged", 3, "sig1")), "23514", "outcome outside the declared set")
		requireCode(t, tryExec(t, e, resolve("approved", 3, "sig1")), "23503", "signal record missing at commit")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = 'approved',
  wait_actor_ref = 'user-1', wait_decided_at = now(), wait_version = 3 WHERE id = 's2'`), "23514", "resolved without a consumed signal")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'expired', wait_outcome = 'approved',
  wait_decided_at = now(), wait_version = 3 WHERE id = 's2'`), "23514", "expiry carrying a resolver outcome")
		ok(`INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_answer, decision_actor_ref, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-1', 'key-1', 'sig1', 'decision', repeat('a', 64), true, 'r1', 's2', 'w1', 'resolved', 'approved', 'ok', 'user-1', 3, now())`)
		ok(resolve("approved", 3, "sig1"))
		// A decided wait is immutable and takes no second decision.
		for name, sql := range map[string]string{
			"outcome change":   `UPDATE run_steps SET wait_outcome = 'rejected', wait_version = 4 WHERE id = 's2'`,
			"reassignment":     `UPDATE run_steps SET wait_assignment = 'reviewer', wait_version = 4 WHERE id = 's2'`,
			"second decision":  `UPDATE run_steps SET wait_state = 'expired', wait_version = 4 WHERE id = 's2'`,
			"reopen":           `UPDATE run_steps SET wait_state = 'waiting', status = 'waiting', wait_version = 4 WHERE id = 's2'`,
			"actor change":     `UPDATE run_steps SET wait_actor_ref = 'user-2', wait_version = 4 WHERE id = 's2'`,
			"unversioned noop": `UPDATE run_steps SET wait_answer = 'late' WHERE id = 's2'`,
		} {
			requireCode(t, tryExec(t, e, sql), "23514", name)
		}

		// Expiry: continue succeeds with the reserved outcome, fail fails the step.
		addWaitStep(t, e, "s4", 3)
		addWaitStep(t, e, "s7", 4)
		ok(openWaitSQL("s4", "w4", "continue"))
		ok(openWaitSQL("s7", "w7", "fail"))
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'failed', finished_at = now(), wait_state = 'expired', wait_decided_at = now(), wait_version = 2 WHERE id = 's4'`), "23514", "continue that fails")
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'expired', wait_outcome = 'expired', wait_decided_at = now(), wait_version = 2 WHERE id = 's7'`), "23514", "fail that succeeds")
		ok(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'expired', wait_outcome = 'expired', wait_decided_at = now(), wait_version = 2 WHERE id = 's4'`)
		ok(`UPDATE run_steps SET status = 'failed', finished_at = now(), wait_state = 'expired', wait_decided_at = now(), wait_version = 2 WHERE id = 's7'`)
		// Cancellation with the run carries no outcome, actor or signal.
		addWaitStep(t, e, "s8", 5)
		ok(openWaitSQL("s8", "w8", "fail"))
		requireCode(t, tryExec(t, e, `UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'cancelled', wait_actor_ref = 'user-1', wait_decided_at = now(), wait_version = 2 WHERE id = 's8'`), "23514", "cancellation naming an actor")
		ok(`UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'cancelled', wait_decided_at = now(), wait_reason = 'run_cancelled', wait_version = 2 WHERE id = 's8'`)

		// Indexes for due waits, the role inbox, correlation routing and the task's runs.
		for _, name := range []string{"run_steps_wait_due", "run_steps_wait_inbox", "run_steps_wait_correlation", "run_steps_workspace_exclusive", "tasks_current_run", "runs_task", "runs_lease_expiry"} {
			var def string
			if err := e.mig.QueryRow(testCtx(t), `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def); err != nil {
				t.Errorf("index %s: %v", name, err)
			}
		}
		var def string
		if err := e.mig.QueryRow(testCtx(t), `SELECT indexdef FROM pg_indexes WHERE indexname = 'run_steps_wait_due'`).Scan(&def); err != nil || !strings.Contains(def, "wait_state = 'waiting'") {
			t.Errorf("due waits index is not limited to open waits: %s (%v)", def, err)
		}
	})

	t.Run("task_projection_and_current_run", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		ok := func(sql string, args ...any) {
			t.Helper()
			if err := tryExec(t, e, sql, args...); err != nil {
				t.Fatalf("%.100q: %v", sql, err)
			}
		}
		// New fields are nullable: legacy rows and the legacy columns are untouched.
		var nullable int
		if err := e.mig.QueryRow(testCtx(t), `SELECT count(*) FROM information_schema.columns WHERE table_name = 'tasks' AND is_nullable = 'NO'
  AND column_name IN ('pipeline_id', 'current_run_id', 'status', 'current_step_key', 'waiting_for', 'assigned_role', 'result', 'progress_version', 'progress_run_version')`).Scan(&nullable); err != nil || nullable != 0 {
			t.Fatalf("projection columns not nullable: %d (%v)", nullable, err)
		}
		var title, state string
		if err := e.mig.QueryRow(testCtx(t), `SELECT title, state FROM tasks WHERE id = 't1'`).Scan(&title, &state); err != nil || state != "open" {
			t.Fatalf("legacy task row unreadable: %v", err)
		}

		// pipeline_id: same project, immutable once chosen.
		requireCode(t, tryExec(t, e, `UPDATE tasks SET pipeline_id = 'pl2' WHERE id = 't1'`), "23503", "pipeline of another project")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET pipeline_id = 'plb' WHERE id = 't1'`), "23503", "pipeline of another tenant")
		ok(`UPDATE tasks SET pipeline_id = 'pl1' WHERE id = 't1'`)
		requireCode(t, tryExec(t, e, `INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-a', 'pa1', 'pl1b', 'other', 'api', '{}');
UPDATE tasks SET pipeline_id = 'pl1b' WHERE id = 't1'`), "23514", "pipeline change")

		// current_run_id belongs to this task and this project.
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'r1', status = 'running' WHERE id = 't2'`), "23514", "current run without a pipeline")
		ok(`UPDATE tasks SET pipeline_id = 'pl1' WHERE id = 't2'`)
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'r1', status = 'running' WHERE id = 't2'`), "23503", "another task's run in the same project")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'r3', status = 'running' WHERE id = 't2'`), "23503", "another project's run")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'nope', status = 'running' WHERE id = 't2'`), "23503", "unknown run")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'r2' WHERE id = 't2'`), "23514", "current run without a status")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET status = 'running' WHERE id = 't2'`), "23514", "status of a run that does not exist")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET status = 'rejected' WHERE id = 't2'`), "23514", "unknown status")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET current_run_id = 'r2', status = 'running', waiting_for = 'reviewer' WHERE id = 't2'`), "23514", "assignment while not waiting")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET progress_version = 1 WHERE id = 't2'`), "23514", "progress version without a run version")
		ok(`UPDATE tasks SET current_run_id = 'r2', status = 'waiting', current_step_key = 'review', waiting_for = 'reviewer',
  progress_version = 5, progress_run_version = 4 WHERE id = 't2'`)
		// A projection never moves backwards for the same run.
		requireCode(t, tryExec(t, e, `UPDATE tasks SET progress_version = 4 WHERE id = 't2'`), "23514", "progress version regression")
		requireCode(t, tryExec(t, e, `UPDATE tasks SET progress_run_version = 3 WHERE id = 't2'`), "23514", "run version regression")
		ok(`UPDATE tasks SET status = 'succeeded', waiting_for = NULL, progress_version = 6, progress_run_version = 5, result = '{"ref":"artifact-1"}' WHERE id = 't2'`)
		requireCode(t, tryExec(t, e, `UPDATE tasks SET result = jsonb_build_object('x', repeat('y', 17000)) WHERE id = 't2'`), "23514", "unbounded result")
	})

	t.Run("signal_records", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		ok := func(sql string, args ...any) {
			t.Helper()
			if err := tryExec(t, e, sql, args...); err != nil {
				t.Fatalf("%.100q: %v", sql, err)
			}
		}
		decision := func(key, signal, applied, reason, wait, kind, outcome, actor, version string) string {
			return `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_actor_ref, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-1', '` + key + `', '` + signal + `', 'decision', repeat('a', 64), ` + applied + `, ` + reason + `, 'r1', 's2', '` + wait + `',
  ` + kind + `, ` + outcome + `, ` + actor + `, ` + version + `, CASE WHEN ` + version + ` IS NULL THEN NULL ELSE now() END)`
		}
		// A winning record is a complete decision; one wait has at most one winner.
		ok(decision("k1", "sig-k1", "true", "NULL", "w1", "'resolved'", "'approved'", "'user-1'", "3"))
		requireCode(t, tryExec(t, e, decision("k1", "sig-other", "true", "NULL", "w2", "'resolved'", "'approved'", "'user-1'", "3")), "23505", "reused identity")
		requireCode(t, tryExec(t, e, decision("k2", "sig-k1", "true", "NULL", "w2", "'resolved'", "'approved'", "'user-1'", "3")), "23505", "reused signal id")
		requireCode(t, tryExec(t, e, decision("k2", "sig-k2", "true", "NULL", "w1", "'resolved'", "'rejected'", "'user-1'", "3")), "23505", "second winner")
		requireCode(t, tryExec(t, e, decision("k3", "sig-k3", "true", "NULL", "w3", "'expired'", "NULL", "NULL", "3")), "23514", "winner that expired")
		requireCode(t, tryExec(t, e, decision("k3", "sig-k3", "true", "NULL", "w3", "'resolved'", "NULL", "'user-1'", "3")), "23514", "resolved without an outcome")
		requireCode(t, tryExec(t, e, decision("k3", "sig-k3", "true", "NULL", "w3", "'resolved'", "'approved'", "NULL", "3")), "23514", "resolved without an actor")
		requireCode(t, tryExec(t, e, decision("k3", "sig-k3", "true", "NULL", "w3", "NULL", "NULL", "NULL", "NULL")), "23514", "winner with no decision")
		// Refusals: a decided refusal replays the decision it lost to; a conflict or
		// invalid outcome has none; a refusal never counts as the winner.
		ok(decision("k4", "sig-k4", "false", "'decided'", "w1", "'resolved'", "'approved'", "'user-1'", "3"))
		ok(decision("k5", "sig-k5", "false", "'decided'", "w1", "'expired'", "'expired'", "NULL", "3"))
		ok(decision("k6", "sig-k6", "false", "'version_conflict'", "w1", "NULL", "NULL", "NULL", "NULL"))
		ok(decision("k7", "sig-k7", "false", "'invalid_outcome'", "w1", "NULL", "NULL", "NULL", "NULL"))
		requireCode(t, tryExec(t, e, decision("k8", "sig-k8", "false", "'decided'", "w1", "NULL", "NULL", "NULL", "NULL")), "23514", "decided refusal without a decision")
		requireCode(t, tryExec(t, e, decision("k8", "sig-k8", "false", "'version_conflict'", "w1", "'resolved'", "'approved'", "'user-1'", "3")), "23514", "conflict refusal with a decision")
		requireCode(t, tryExec(t, e, decision("k8", "sig-k8", "false", "'unknown_wait'", "w1", "NULL", "NULL", "NULL", "NULL")), "23514", "targetless reason on a targeted record")
		requireCode(t, tryExec(t, e, decision("k8", "sig-k8", "true", "'decided'", "w1", "'resolved'", "'approved'", "'user-1'", "3")), "23514", "applied refusal")
		requireCode(t, tryExec(t, e, decision("k8", "sig-k8", "false", "NULL", "w1", "'resolved'", "'approved'", "'user-1'", "3")), "23514", "unapplied with no reason")
		// A tombstone is smaller: identity, digest, reason and time only.
		tomb := func(key, signal, reason, extra string) string {
			return `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason` + extra + `)
VALUES ('tenant-a', 'pa1', 'user-1', '` + key + `', '` + signal + `', 'tombstone', repeat('b', 64), false, ` + reason + `)`
		}
		ok(tomb("t1", "sig-t1", "'unknown_wait'", ""))
		ok(tomb("t2", "sig-t2", "'ambiguous_correlation'", ""))
		requireCode(t, tryExec(t, e, tomb("t3", "sig-t3", "'decided'", "")), "23514", "tombstone with a targeted reason")
		requireCode(t, tryExec(t, e, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason, target_wait_id)
VALUES ('tenant-a', 'pa1', 'user-1', 't4', 'sig-t4', 'tombstone', repeat('b', 64), false, 'unknown_wait', 'w1')`), "23514", "tombstone with a target")
		requireCode(t, tryExec(t, e, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ('tenant-a', 'pa1', 'user-1', 't5', 'sig-t5', 'tombstone', 'short', false, 'unknown_wait')`), "23514", "bad digest")
		requireCode(t, tryExec(t, e, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ('tenant-a', 'pa1', 'user-1', repeat('k', 129), 'sig-t6', 'tombstone', repeat('b', 64), false, 'unknown_wait')`), "23514", "key over 128 characters")
		// The same key under another principal, project or tenant is another identity.
		ok(`INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ('tenant-a', 'pa1', 'user-2', 't1', 'sig-u2', 'tombstone', repeat('b', 64), false, 'unknown_wait'),
       ('tenant-a', 'pa2', 'user-1', 't1', 'sig-p2', 'tombstone', repeat('b', 64), false, 'unknown_wait'),
       ('tenant-b', 'pb1', 'user-1', 't1', 'sig-b', 'tombstone', repeat('b', 64), false, 'unknown_wait')`)
		requireCode(t, tryExec(t, e, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ('tenant-a', 'pb1', 'user-1', 'cross', 'sig-x', 'tombstone', repeat('b', 64), false, 'unknown_wait')`), "23503", "project of another tenant")

		// Ordinary retention removes the wait, step and run, never the record.
		ok(`DELETE FROM run_steps WHERE run_id = 'r1'`)
		ok(`UPDATE tasks SET current_run_id = NULL WHERE current_run_id = 'r1'`)
		ok(`DELETE FROM runs WHERE id = 'r1'`)
		var kind, outcome, actor string
		var version int
		if err := e.mig.QueryRow(testCtx(t), `SELECT decision_kind, decision_outcome, decision_actor_ref, decision_version FROM signal_records
WHERE caller_principal = 'user-1' AND caller_key = 'k1'`).Scan(&kind, &outcome, &actor, &version); err != nil ||
			kind != "resolved" || outcome != "approved" || actor != "user-1" || version != 3 {
			t.Fatalf("decision after retention: %s %s %s %d (%v)", kind, outcome, actor, version, err)
		}
		// Only the project's removal removes them.
		e.exec(t, `INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ('tenant-a', 'pa9', 'pa9', '{}');
INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ('tenant-a', 'pa9', 'user-1', 'gone', 'sig-gone', 'tombstone', repeat('b', 64), false, 'unknown_wait');
DELETE FROM projects WHERE id = 'pa9'`)
		var left, kept int
		if err := e.mig.QueryRow(testCtx(t), `SELECT count(*) FILTER (WHERE project_id = 'pa9'), count(*) FILTER (WHERE project_id = 'pa1') FROM signal_records`).Scan(&left, &kept); err != nil || left != 0 || kept == 0 {
			t.Fatalf("after project removal: removed project has %d, other has %d (%v)", left, kept, err)
		}
	})

	t.Run("forced_rls_non_owner", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)

		var enabled, forced bool
		if err := e.mig.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'signal_records'`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatalf("signal_records rls enabled=%v forced=%v (%v)", enabled, forced, err)
		}
		var super, bypass bool
		if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
			t.Fatalf("runtime role is privileged: super=%v bypass=%v (%v)", super, bypass, err)
		}

		insert := `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied, refusal_reason)
VALUES ($1, $2, 'user-1', $3, $4, 'tombstone', repeat('b', 64), false, 'unknown_wait')`
		// write inserts a row owned by rowTenant inside ctxTenant's transaction.
		write := func(ctxTenant, rowTenant domain.TenantID, project, key, id string) error {
			return storage.WithTenant(ctx, pool, ctxTenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, insert, string(rowTenant), project, key, id)
				return err
			})
		}
		if err := write(tenantA, tenantA, "pa1", "ka", "sig-a"); err != nil {
			t.Fatalf("tenant a insert: %v", err)
		}
		if err := write(tenantB, tenantB, "pb1", "kb", "sig-b"); err != nil {
			t.Fatalf("tenant b insert: %v", err)
		}
		// A row of another tenant is refused by the policy, whatever the caller writes.
		requireCode(t, write(tenantB, tenantA, "pa1", "kx", "sig-x"), "42501", "tenant b writing a tenant a row")
		requireCode(t, storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, insert, "tenant-b", "pb1", "ky", "sig-y")
			return err
		}), "42501", "tenant a writing a tenant b row")
		for _, tc := range []struct {
			tenant domain.TenantID
			want   string
		}{{tenantA, "sig-a"}, {tenantB, "sig-b"}} {
			var ids []string
			err := storage.WithTenant(ctx, pool, tc.tenant, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT signal_id FROM signal_records ORDER BY signal_id`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					ids = append(ids, id)
				}
				return rows.Err()
			})
			if err != nil || len(ids) != 1 || ids[0] != tc.want {
				t.Fatalf("%s sees %v (%v), want only %s", tc.tenant, ids, err, tc.want)
			}
		}
		// Without a tenant context nothing is visible.
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM signal_records`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("no tenant context sees %d rows (%v)", n, err)
		}
		// Append-only for the runtime role: no update, delete or truncate.
		for _, sql := range []string{
			`UPDATE signal_records SET decision_answer = 'changed'`,
			`UPDATE signal_records SET refusal_reason = 'decided'`,
			`DELETE FROM signal_records`,
			`TRUNCATE signal_records`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "42501", sql)
		}
		// Admitted inputs and snapshots stay immutable; the wait kind is fixed.
		for _, sql := range []string{
			`UPDATE run_steps SET step_kind = 'container' WHERE id = 's2'`,
			`UPDATE run_steps SET image = 'other' WHERE id = 's1'`,
			`UPDATE run_steps SET step_key = 'other' WHERE id = 's1'`,
			`UPDATE runs SET definition_snapshot = '{}' WHERE id = 'r1'`,
			`UPDATE tasks SET parent_id = NULL, kind = 'incident' WHERE id = 't1'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "42501", sql)
		}
		// Lifecycle columns are writable and tenant-scoped.
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE runs SET lease_owner = 'w1', lease_purpose = 'execute', lease_expires_at = now() + interval '1 minute', lease_fence = 1 WHERE id = 'r1'`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET pipeline_id = 'pl1' WHERE id = 't1'`); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE runs SET lease_owner = 'w1', lease_purpose = 'execute', lease_expires_at = now(), lease_fence = 1 WHERE tenant_id = 'tenant-b'`)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("tenant a touched %d tenant b runs", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("runtime lifecycle update: %v", err)
		}
		// Legacy columns stay readable by the runtime role.
		err = storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			var wf, state string
			var parked, resolved *string
			if err := tx.QueryRow(ctx, `SELECT p.task_workflow::text FROM projects p WHERE p.id = 'pa1'`).Scan(&wf); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT state, parked_at::text, resolved_at::text FROM tasks WHERE id = 't1' AND resume_state IS NULL AND blocked_by_id IS NULL AND resolution IS NULL`).
				Scan(&state, &parked, &resolved)
		})
		if err != nil {
			t.Fatalf("legacy columns unreadable: %v", err)
		}
	})

	t.Run("runtime_rollback", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)
		openWait := func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, openWaitSQL("s2", "w1", "fail"))
			return err
		}
		if err := storage.WithTenant(ctx, pool, tenantA, openWait); err != nil {
			t.Fatalf("open wait as runtime: %v", err)
		}
		insertWinner := func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_actor_ref, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-1', 'key-1', 'sig1', 'decision', repeat('a', 64), true, 'r1', 's2', 'w1', 'resolved', 'approved', 'user-1', 2, now())`)
			return err
		}
		decide := func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = 'approved',
  wait_actor_ref = 'user-1', wait_decided_at = now(), wait_consumed_signal_id = 'sig1', wait_version = 2 WHERE id = 's2'`)
			return err
		}
		state := func() (string, int) {
			var wait string
			var records int
			if err := e.mig.QueryRow(ctx, `SELECT wait_state FROM run_steps WHERE id = 's2'`).Scan(&wait); err != nil {
				t.Fatal(err)
			}
			if err := e.mig.QueryRow(ctx, `SELECT count(*) FROM signal_records`).Scan(&records); err != nil {
				t.Fatal(err)
			}
			return wait, records
		}

		// A failure after the decision rolls back the decision and its record.
		boom := errBoom{}
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := insertWinner(ctx, tx); err != nil {
				return err
			}
			if err := decide(ctx, tx); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the callback error", err)
		}
		if wait, records := state(); wait != "waiting" || records != 0 {
			t.Fatalf("after rollback: wait %s, %d records; want waiting, 0", wait, records)
		}
		// A decision whose signal record is missing at commit is refused whole.
		err = storage.WithTenant(ctx, pool, tenantA, decide)
		requireCode(t, err, "23503", "decision without its record")
		if wait, records := state(); wait != "waiting" || records != 0 {
			t.Fatalf("after refused commit: wait %s, %d records; want waiting, 0", wait, records)
		}
		// A record whose decision is refused is not left behind either.
		err = storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := insertWinner(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = 'merged',
  wait_actor_ref = 'user-1', wait_decided_at = now(), wait_consumed_signal_id = 'sig1', wait_version = 2 WHERE id = 's2'`)
			return err
		})
		requireCode(t, err, "23514", "undeclared outcome")
		if wait, records := state(); wait != "waiting" || records != 0 {
			t.Fatalf("after refused decision: wait %s, %d records; want waiting, 0", wait, records)
		}
		// Record and decision commit together, in either order.
		err = storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := decide(ctx, tx); err != nil {
				return err
			}
			return insertWinner(ctx, tx)
		})
		if err != nil {
			t.Fatalf("decision and record: %v", err)
		}
		if wait, records := state(); wait != "resolved" || records != 1 {
			t.Fatalf("after commit: wait %s, %d records; want resolved, 1", wait, records)
		}
	})

	// Versions are guarded against every route a non-owner writer has: a statement
	// that changes only a version, a NULL reset (SQL unknown passes a comparison and
	// a pair check), and a version of a decided wait.
	t.Run("version_guards_non_owner", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)
		must := func(sql string) {
			t.Helper()
			if err := runtimeExec(t, pool, tenantA, sql); err != nil {
				t.Fatalf("%.100q: %v", sql, err)
			}
		}
		version := func(sql string) (v *int64) {
			t.Helper()
			if err := e.mig.QueryRow(ctx, sql).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}

		// An open wait at version 3 cannot go back, whatever else is (not) changed.
		must(openWaitSQL("s2", "w1", "fail"))
		must(`UPDATE run_steps SET wait_version = 3 WHERE id = 's2'`)
		for name, sql := range map[string]string{
			"version-only regression":  `UPDATE run_steps SET wait_version = 2 WHERE id = 's2'`,
			"regression to the start":  `UPDATE run_steps SET wait_version = 1 WHERE id = 's2'`,
			"version cleared":          `UPDATE run_steps SET wait_version = NULL WHERE id = 's2'`,
			"regression with a change": `UPDATE run_steps SET wait_assignment = 'operator', wait_version = 2 WHERE id = 's2'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", name)
		}
		if v := version(`SELECT wait_version FROM run_steps WHERE id = 's2'`); v == nil || *v != 3 {
			t.Fatalf("wait version = %v after refused regressions, want 3", v)
		}
		must(`UPDATE run_steps SET wait_version = 4 WHERE id = 's2'`) // forward without a change is allowed

		// The decision commits its record and version together; afterwards the
		// version is frozen along with the decision.
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
  target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_actor_ref, decision_version, decided_at)
VALUES ('tenant-a', 'pa1', 'user-1', 'key-1', 'sig1', 'decision', repeat('a', 64), true, 'r1', 's2', 'w1', 'resolved', 'approved', 'user-1', 5, now())`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = 'approved',
  wait_actor_ref = 'user-1', wait_decided_at = now(), wait_consumed_signal_id = 'sig1', wait_version = 5 WHERE id = 's2'`)
			return err
		})
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		for name, sql := range map[string]string{
			"decided version raised":    `UPDATE run_steps SET wait_version = 99 WHERE id = 's2'`,
			"decided version raised +1": `UPDATE run_steps SET wait_version = 6 WHERE id = 's2'`,
			"decided version lowered":   `UPDATE run_steps SET wait_version = 4 WHERE id = 's2'`,
			"decided version cleared":   `UPDATE run_steps SET wait_version = NULL WHERE id = 's2'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", name)
		}
		must(`UPDATE run_steps SET wait_version = wait_version WHERE id = 's2'`) // a no-op write is not a mutation
		if v := version(`SELECT wait_version FROM run_steps WHERE id = 's2'`); v == nil || *v != 5 {
			t.Fatalf("decided wait version = %v, want 5", v)
		}

		// Task progress: once initialized it is never cleared and never regresses.
		must(`UPDATE tasks SET pipeline_id = 'pl1' WHERE id = 't2'`)
		must(`UPDATE tasks SET current_run_id = 'r2', status = 'running', progress_version = 5, progress_run_version = 4 WHERE id = 't2'`)
		for name, sql := range map[string]string{
			"both versions cleared":        `UPDATE tasks SET progress_version = NULL, progress_run_version = NULL WHERE id = 't2'`,
			"progress version cleared":     `UPDATE tasks SET progress_version = NULL WHERE id = 't2'`,
			"run version cleared":          `UPDATE tasks SET progress_run_version = NULL WHERE id = 't2'`,
			"progress version lowered":     `UPDATE tasks SET progress_version = 4 WHERE id = 't2'`,
			"run version lowered":          `UPDATE tasks SET progress_run_version = 3 WHERE id = 't2'`,
			"clear then restore, step 1":   `UPDATE tasks SET progress_version = NULL, progress_run_version = NULL, status = 'queued' WHERE id = 't2'`,
			"run changed, run version off": `UPDATE tasks SET current_run_id = NULL, status = 'pending', progress_run_version = NULL WHERE id = 't2'`,
		} {
			requireCode(t, runtimeExec(t, pool, tenantA, sql), "23514", name)
		}
		var pv, rv *int64
		if err := e.mig.QueryRow(ctx, `SELECT progress_version, progress_run_version FROM tasks WHERE id = 't2'`).Scan(&pv, &rv); err != nil ||
			pv == nil || rv == nil || *pv != 5 || *rv != 4 {
			t.Fatalf("progress = %v/%v (%v), want 5/4: a refused reset must change nothing", pv, rv, err)
		}
		// A different run may restart its own version; the overall version still only grows.
		e.exec(t, `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, task_id, definition_snapshot) VALUES ('tenant-a', 'pa1', 'r9', 'pl1', 't2', '{}')`)
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE tasks SET current_run_id = 'r9', progress_version = 4, progress_run_version = 1 WHERE id = 't2'`), "23514", "new run with a lower progress version")
		must(`UPDATE tasks SET current_run_id = 'r9', progress_version = 6, progress_run_version = 1 WHERE id = 't2'`)
		requireCode(t, runtimeExec(t, pool, tenantA, `UPDATE tasks SET progress_run_version = NULL WHERE id = 't2'`), "23514", "new run's version cleared")
		must(`UPDATE tasks SET progress_version = 6, progress_run_version = 2 WHERE id = 't2'`)
	})
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
