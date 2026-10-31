package storage_test

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// This file is the infrastructure-free evidence for migration 006. It proves what
// the migration text declares and cross-checks it against the kernel's model; it
// does not prove what PostgreSQL enforces. Test_wait_decision_ordering_postgres,
// Test_step_outputs_immutable_postgres and Test_materialization_records_postgres
// execute the constraints, guards, grants and forced row security as a non-owner
// role against a real service and are the evidence for those.

func waitStatements(t testing.TB) []string { return sqlStatements(migrationSQL(t, waitMigration)) }

// constraintClause returns the text of `ADD CONSTRAINT name ...` inside the ALTER
// TABLE statement that adds it, up to the comma that ends the clause.
func constraintClause(t testing.TB, stmts []string, name string) string {
	t.Helper()
	for _, s := range stmts {
		if !strings.HasPrefix(s, "ALTER TABLE ") {
			continue
		}
		for _, clause := range topLevelCommas(s) {
			if i := strings.Index(clause, "ADD CONSTRAINT "+name+" "); i >= 0 {
				return clause[i:]
			}
		}
	}
	t.Fatalf("constraint %s is not added", name)
	return ""
}

var (
	statusTest = regexp.MustCompile(`\bstatus (= '[a-z]+'|IN \([^)]*\))`)
	statusSet  = regexp.MustCompile(`status IN \(([^)]*)\)`)
)

// withoutStatus replaces every test of the step status, so two constraints can be
// compared on everything but the statuses they allow.
func withoutStatus(s string) string { return statusTest.ReplaceAllString(s, "STATUS") }

// allowedStatuses returns the step statuses a constraint clause allows, reading the
// `status IN (...)` set that follows marker (or the first one when marker is empty).
func allowedStatuses(t testing.TB, clause, marker string) []string {
	t.Helper()
	if marker != "" {
		i := strings.Index(clause, marker)
		if i < 0 {
			t.Fatalf("no %q in %s", marker, clause)
		}
		clause = clause[i:]
	}
	m := statusSet.FindStringSubmatch(clause)
	if m == nil {
		t.Fatalf("no status set in %s", clause)
	}
	var out []string
	for _, v := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(v), "'"))
	}
	sort.Strings(out)
	return out
}

// priorClause is the constraint as 004 (resolved, expired) or 005 (cancelled) defined it.
func priorClause(t testing.TB, name string) string {
	t.Helper()
	for _, id := range []string{durableMigration, expandMigration} {
		for _, s := range sqlStatements(migrationSQL(t, id)) {
			if !strings.HasPrefix(s, "ALTER TABLE run_steps ") {
				continue
			}
			for _, clause := range topLevelCommas(s) {
				if i := strings.Index(clause, "ADD CONSTRAINT "+name+" "); i >= 0 {
					return clause[i:]
				}
			}
		}
	}
	t.Fatalf("no earlier definition of %s", name)
	return ""
}

// Test_unit_storage_wait_decision_ordering: a complete wait decision can be stored
// while its step has not settled, and nothing else about a decision is relaxed.
func Test_unit_storage_wait_decision_ordering(t *testing.T) {
	stmts := waitStatements(t)

	t.Run("registered_last_and_expand_only", func(t *testing.T) {
		var ids []string
		for _, m := range storage.BaseContribution().Migrations {
			ids = append(ids, m.ID)
		}
		if i := slices.Index(ids, waitMigration); i < 0 || ids[i-1] != durableMigration {
			t.Fatalf("%s is not appended directly after %s: %v", waitMigration, durableMigration, ids)
		}
		text := strings.Join(stmts, ";\n")
		if regexp.MustCompile(`(?i)\bDROP (TABLE|COLUMN|SCHEMA|POLICY|TRIGGER|INDEX|DOMAIN|FUNCTION)\b|\bTRUNCATE\b|\bDELETE\b|\bINSERT INTO\b|\bUPDATE run|\bUPDATE tasks|\bALTER TABLE \w+ (ADD|DROP|RENAME) COLUMN`).MatchString(text) {
			t.Error("a destructive, data-writing or column statement in the expand migration")
		}
		// Exactly the three constraints whose status agreement forbade the decision are replaced,
		// and each is re-added under its own name. The wait_waiting constraint is untouched.
		var dropped []string
		for _, s := range stmts {
			for _, m := range regexp.MustCompile(`DROP CONSTRAINT (\w+)`).FindAllStringSubmatch(s, -1) {
				dropped = append(dropped, m[1])
			}
		}
		want := []string{"run_steps_wait_resolved", "run_steps_wait_expired", "run_steps_wait_cancelled"}
		if !slices.Equal(dropped, want) {
			t.Errorf("dropped constraints = %v, want %v", dropped, want)
		}
		added := constraintNames(stmts)
		for _, name := range want {
			if !slices.Contains(added, name) {
				t.Errorf("%s is not re-added", name)
			}
		}
		if slices.Contains(dropped, "run_steps_wait_waiting") || slices.Contains(added, "run_steps_wait_waiting") ||
			slices.Contains(dropped, "run_steps_wait_unopened") || slices.Contains(added, "run_steps_wait_unopened") ||
			slices.Contains(dropped, "run_steps_wait_open") || slices.Contains(added, "run_steps_wait_open") {
			t.Error("the open-wait and unopened-wait constraints stay as they are")
		}
		for _, legacy := range []string{"task_workflow", "parked_at", "resume_state", "blocked_by_id", "resolution", "resolved_at"} {
			if regexp.MustCompile(`\b` + legacy + `\b`).MatchString(text) {
				t.Errorf("legacy column %s is touched", legacy)
			}
		}
	})

	t.Run("only_the_step_status_is_relaxed", func(t *testing.T) {
		// Each replacement equals the constraint it replaces once the status tests are masked:
		// the decision still needs its outcome, actor, signal, time, reason and its absences.
		for name, prior := range map[string]string{
			"run_steps_wait_resolved":  expandMigration,
			"run_steps_wait_expired":   expandMigration,
			"run_steps_wait_cancelled": durableMigration,
		} {
			old, now := priorClause(t, name), constraintClause(t, stmts, name)
			// The last clause of an ALTER ends at the statement; the clause lists differ in what follows.
			trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), ",") }
			if a, b := withoutStatus(trim(old)), withoutStatus(trim(now)); a != b {
				t.Errorf("%s (defined in %s) changed beyond the step status:\n old %s\n new %s", name, prior, a, b)
			}
		}
	})

	t.Run("the_statuses_the_decision_allows", func(t *testing.T) {
		resolved := constraintClause(t, stmts, "run_steps_wait_resolved")
		expired := constraintClause(t, stmts, "run_steps_wait_expired")
		cancelled := constraintClause(t, stmts, "run_steps_wait_cancelled")
		sets := map[string][]string{
			"resolved":         allowedStatuses(t, resolved, ""),
			"expired continue": allowedStatuses(t, expired, "wait_on_expiry = 'continue'"),
			"expired fail":     allowedStatuses(t, expired, "wait_on_expiry = 'fail'"),
			"cancelled":        allowedStatuses(t, cancelled, ""),
		}
		for what, want := range map[string][]string{
			"resolved":         {"succeeded", "waiting"},
			"expired continue": {"succeeded", "waiting"},
			"expired fail":     {"failed", "waiting"},
			"cancelled":        {"cancelled", "failed", "pending", "skipped", "waiting"},
		} {
			if !slices.Equal(sets[what], want) {
				t.Errorf("%s allows %v, want %v", what, sets[what], want)
			}
		}

		// Cross-check with the kernel: every pairing of a step and a wait that the kernel accepts
		// (CheckStepWait) is storable, and so is every decision the contract suite stores before
		// the step settles.
		spec := runtime.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"}, Input: "approval",
			Outcomes: []string{"approved", "rejected"}, Timeout: time.Hour, OnExpiry: runtime.ExpiryFail}
		cont := spec
		cont.OnExpiry = runtime.ExpiryContinue
		scope := durableScope
		now := t0
		build := func(sp runtime.WaitSpec, decide func(w *runtime.Wait) error) runtime.Wait {
			w, err := runtime.NewWait(scope, "w1", "run1", "review", sp)
			must(t, err)
			if decide == nil {
				return w
			}
			must(t, decide(&w))
			return w
		}
		open := func(w *runtime.Wait) error { return w.Open(now, map[string]string{"candidate": "c1"}, "") }
		after := func(then func(w *runtime.Wait) error) func(w *runtime.Wait) error {
			return func(w *runtime.Wait) error {
				if err := open(w); err != nil {
					return err
				}
				return then(w)
			}
		}
		type pair struct {
			wait   runtime.Wait
			column string // the wait_state column value
			kind   string // key into sets
		}
		pairs := []pair{
			{build(spec, after(func(w *runtime.Wait) error {
				_, err := w.Resolve(runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope, Caller: "alice", Key: "k"},
					WaitID: "w1", Outcome: "approved", Actor: "user:alice"}, now.Add(time.Minute))
				return err
			})), "resolved", "resolved"},
			{build(cont, after(func(w *runtime.Wait) error { _, err := w.Expire(now.Add(2 * time.Hour)); return err })), "expired", "expired continue"},
			{build(spec, after(func(w *runtime.Wait) error { _, err := w.Expire(now.Add(2 * time.Hour)); return err })), "expired", "expired fail"},
			{build(spec, after(func(w *runtime.Wait) error { _, err := w.Cancel(now, runtime.ReasonRunCancelled); return err })), "cancelled", "cancelled"},
			{build(spec, func(w *runtime.Wait) error { _, err := w.Cancel(now, runtime.ReasonRunFailed); return err }), "cancelled", "cancelled"},
		}
		steps := []runtime.StepState{runtime.StepPending, runtime.StepRunning, runtime.StepWaiting, runtime.StepSucceeded,
			runtime.StepFailed, runtime.StepSkipped, runtime.StepCancelled}
		for _, p := range pairs {
			for _, st := range steps {
				step := runtime.Step{Key: "review", Kind: runtime.StepWait, State: st, Version: 1}
				if st.Terminal() {
					step.Reason = runtime.ReasonRunFailed
				}
				// CheckStepWait is looser than 004 in one pairing, which this migration does not change: it
				// accepts a failed step for any expiry, while a `continue` expiry has always been stored only
				// with a succeeded or (now) a waiting step. Nothing settles an expired `continue` wait as failed.
				if p.kind == "expired continue" && st == runtime.StepFailed {
					continue
				}
				if runtime.CheckStepWait(step, p.wait) == nil && !slices.Contains(sets[p.kind], string(st)) {
					t.Errorf("the kernel accepts a %s wait (%s) on a %s step, the constraint does not", p.wait.State, p.kind, st)
				}
			}
		}
		// Stored before the step settles: the signal and expiry leave the step waiting for the
		// engine; a cancellation leaves a waiting step, or a pending one for a wait never opened.
		for _, c := range []struct{ kind, status string }{
			{"resolved", "waiting"}, {"expired continue", "waiting"}, {"expired fail", "waiting"},
			{"cancelled", "waiting"}, {"cancelled", "pending"},
		} {
			if !slices.Contains(sets[c.kind], c.status) {
				t.Errorf("a %s decision cannot be stored on a %s step", c.kind, c.status)
			}
		}
		// A decision never lands on a step that could not have it: no running step, and no settled
		// state that contradicts the decision.
		for _, c := range []struct{ kind, status string }{
			{"resolved", "pending"}, {"resolved", "running"}, {"resolved", "failed"}, {"resolved", "skipped"}, {"resolved", "cancelled"},
			{"expired continue", "pending"}, {"expired continue", "running"}, {"expired continue", "failed"}, {"expired continue", "cancelled"},
			{"expired fail", "pending"}, {"expired fail", "running"}, {"expired fail", "succeeded"}, {"expired fail", "cancelled"},
			{"cancelled", "running"}, {"cancelled", "succeeded"},
		} {
			if slices.Contains(sets[c.kind], c.status) {
				t.Errorf("a %s decision is allowed on a %s step", c.kind, c.status)
			}
		}
	})

	t.Run("a_decided_wait_stays_complete_immutable_and_closed", func(t *testing.T) {
		sql := strings.Join(stmts, ";\n")
		clauses := map[string][]string{
			"run_steps_wait_resolved": {
				"wait_outcome IS NOT NULL AND jsonb_exists(wait_outcomes, wait_outcome)",
				"wait_actor_ref IS NOT NULL AND wait_decided_at IS NOT NULL AND wait_consumed_signal_id IS NOT NULL"},
			"run_steps_wait_expired": {
				"wait_decided_at IS NOT NULL", "num_nonnulls(wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0",
				"wait_outcome IS NOT DISTINCT FROM 'expired'", "wait_outcome IS NULL"},
			"run_steps_wait_cancelled": {
				"wait_decided_at IS NOT NULL", "wait_reason IN ('run_cancelled', 'run_failed')",
				"num_nonnulls(wait_outcome, wait_answer, wait_actor_ref, wait_consumed_signal_id) = 0"},
		}
		for name, needs := range clauses {
			body := constraintClause(t, stmts, name)
			for _, need := range needs {
				if !strings.Contains(body, need) {
					t.Errorf("%s lacks %q", name, need)
				}
			}
			// The constraint applies exactly to its own state.
			state := strings.TrimPrefix(name, "run_steps_wait_")
			if !strings.Contains(body, "wait_state IS DISTINCT FROM '"+state+"'") {
				t.Errorf("%s does not apply to state %s alone", name, state)
			}
		}
		// The immutability and no-reopen rules live in 004/005's guard function, which this
		// migration must not replace, and which still holds them.
		if strings.Contains(sql, "kodama_run_step_guard") {
			t.Error("the run-step guard of 004/005 is replaced")
		}
		guard := ""
		for _, s := range sqlStatements(migrationSQL(t, durableMigration)) {
			if strings.HasPrefix(s, "CREATE OR REPLACE FUNCTION kodama_run_step_guard()") {
				guard = s
			}
		}
		for _, need := range []string{
			"OLD.wait_state = 'waiting' AND NEW.wait_state IN ('resolved', 'expired', 'cancelled')",
			"IF OLD.wait_state IN ('resolved', 'expired', 'cancelled') AND (NEW.wait_assignment, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref, NEW.wait_decided_at, NEW.wait_consumed_signal_id, NEW.wait_version, NEW.wait_reason)",
			"a decided wait is immutable, including its version",
			"a wait change must increase the wait version",
		} {
			if !strings.Contains(guard, need) {
				t.Errorf("the guard no longer holds %q", need)
			}
		}
	})

	t.Run("a_decided_step_never_goes_back", func(t *testing.T) {
		var fn string
		for _, s := range stmts {
			if strings.HasPrefix(s, "CREATE FUNCTION kodama_run_step_wait_order_guard()") {
				fn = s
			}
		}
		for _, need := range []string{
			"OLD.step_kind = 'wait' AND NEW.status IS DISTINCT FROM OLD.status",
			"OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled')",
			"OLD.status = 'waiting' AND NEW.status = 'pending'",
		} {
			if !strings.Contains(fn, need) {
				t.Errorf("kodama_run_step_wait_order_guard lacks %q", need)
			}
		}
		if !slices.Contains(stmts, "CREATE TRIGGER run_steps_wait_order_guard BEFORE UPDATE ON run_steps FOR EACH ROW EXECUTE FUNCTION kodama_run_step_wait_order_guard()") {
			t.Error("the wait-order guard is not a BEFORE UPDATE row trigger on run_steps")
		}
	})

	t.Run("step_outputs_are_recorded_once_with_the_settlement", func(t *testing.T) {
		var fn string
		for _, s := range stmts {
			if strings.HasPrefix(s, "CREATE FUNCTION kodama_run_step_outputs_guard()") {
				fn = s
			}
		}
		for _, need := range []string{
			"IF NEW.outputs IS DISTINCT FROM OLD.outputs THEN",
			"IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled') THEN",
			"IF NEW.status <> 'succeeded' THEN",
		} {
			if !strings.Contains(fn, need) {
				t.Errorf("kodama_run_step_outputs_guard lacks %q", need)
			}
		}
		if !slices.Contains(stmts, "CREATE TRIGGER run_steps_outputs_guard BEFORE UPDATE ON run_steps FOR EACH ROW EXECUTE FUNCTION kodama_run_step_outputs_guard()") {
			t.Error("the outputs guard is not a BEFORE UPDATE row trigger on run_steps")
		}
		// The settled states the guard names are the kernel's terminal step states.
		for _, s := range []runtime.StepState{runtime.StepSucceeded, runtime.StepFailed, runtime.StepSkipped, runtime.StepCancelled} {
			if !s.Terminal() || !strings.Contains(fn, "'"+string(s)+"'") {
				t.Errorf("settled state %s is not guarded", s)
			}
		}
		// The recorded outputs column was already writable by the runtime role; the guard, not the
		// grant, bounds it, so no grant changes.
		if len(statementsMatching(stmts, `^GRANT .* ON run_steps `)) != 0 {
			t.Error("run_steps grants are unchanged by this migration")
		}
	})
}

// Test_unit_storage_wait_decision_ordering's outbox part: one partial index, no
// table or row change.
func Test_unit_storage_outbox_index(t *testing.T) {
	stmts := waitStatements(t)
	if !slices.Contains(stmts, "CREATE INDEX audit_events_unpublished ON audit_events (tenant_id, created_at, id) WHERE published_at IS NULL") {
		t.Fatal("the partial index for unpublished outbox pages is missing or differs from the specified one")
	}
	if n := len(statementsMatching(stmts, `^CREATE (UNIQUE )?INDEX `)); n != 2 {
		t.Errorf("%d indexes created, want the outbox index and the materialization scan index", n)
	}
	if len(statementsMatching(stmts, `audit_events`)) != 1 {
		t.Error("audit_events is touched only to add the index")
	}
	// The retry-schedule index of the baseline stays; this one serves the creation order.
	if !strings.Contains(migrationSQL(t, "001_schema.sql"), "CREATE INDEX audit_events_pending ON audit_events (tenant_id, next_attempt_at, id) WHERE published_at IS NULL") {
		t.Error("the baseline's retry-schedule index changed")
	}
}

// Test_unit_storage_materialization_records: the table records the identity,
// issue time and revocation of a step attempt's secret material, tenant-isolated,
// with narrow grants and a guard that makes `revoked` final.
func Test_unit_storage_materialization_records(t *testing.T) {
	stmts := waitStatements(t)
	table := statementsMatching(stmts, `^CREATE TABLE run_materializations `)

	t.Run("one_table_keyed_by_scope_run_step_and_attempt", func(t *testing.T) {
		if len(table) != 1 || len(statementsMatching(stmts, `^CREATE TABLE `)) != 1 {
			t.Fatalf("migration 006 creates exactly the one table, got %d", len(statementsMatching(stmts, `^CREATE TABLE `)))
		}
		for _, need := range []string{
			"tenant_id kodama_id NOT NULL", "project_id kodama_id NOT NULL", "run_id kodama_id NOT NULL",
			"step_key text NOT NULL", "attempt integer NOT NULL CHECK (attempt >= 1)",
			"materialization_id text NOT NULL", "state text NOT NULL DEFAULT 'issued'", "issued_at timestamptz NOT NULL",
			"revoked_at timestamptz", "revocation_evidence text",
			"PRIMARY KEY (tenant_id, project_id, run_id, step_key, attempt)",
			"FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id)",
			"FOREIGN KEY (tenant_id, project_id, run_id, step_key) REFERENCES run_steps (tenant_id, project_id, run_id, step_key)",
		} {
			if !strings.Contains(table[0], need) {
				t.Errorf("run_materializations lacks %q", need)
			}
		}
		if regexp.MustCompile(`(?i)ON DELETE CASCADE`).MatchString(table[0]) {
			t.Error("run_materializations must not cascade: retention removes rows explicitly")
		}
		if strings.Contains(table[0], "issued_at timestamptz NOT NULL DEFAULT") {
			t.Error("the issue time is the kernel's, never defaulted by the store")
		}
	})

	t.Run("matches_the_kernels_record", func(t *testing.T) {
		// The kernel's states are the table's.
		for _, s := range []runtime.MaterializationState{runtime.MaterializationIssued, runtime.MaterializationRevoked} {
			if !strings.Contains(table[0], "'"+string(s)+"'") {
				t.Errorf("materialization state %s is not a value of the state column", s)
			}
		}
		if !strings.Contains(table[0], "CHECK (state IN ('issued', 'revoked'))") {
			t.Error("the state check is exactly the kernel's two states")
		}
		// The table enforces the identity the kernel derives, in the kernel's order.
		ref, err := runtime.NewMaterializationRef(durableScope, "run1", "build", 3)
		must(t, err)
		parts := strings.Split(ref.ID, ":")
		if len(parts) != 6 || parts[0] != "mat" || parts[1] != string(durableScope.TenantID) || parts[2] != string(durableScope.ProjectID) ||
			parts[3] != "run1" || parts[4] != "build" || parts[5] != "3" {
			t.Fatalf("the kernel's identity changed form: %q", ref.ID)
		}
		want := "materialization_id = 'mat:' || tenant_id || ':' || project_id || ':' || run_id || ':' || step_key || ':' || attempt::text"
		if !strings.Contains(table[0], "CONSTRAINT run_materializations_identity CHECK ( "+want) {
			t.Errorf("the identity constraint is not the kernel's derivation %q", want)
		}
		// Revocation time and evidence exist exactly when revoked, and never precede the issue.
		for _, need := range []string{
			"(state = 'revoked') = (revoked_at IS NOT NULL)",
			"state = 'revoked' OR revocation_evidence IS NULL",
			"revoked_at IS NULL OR revoked_at >= issued_at",
		} {
			if !strings.Contains(table[0], need) {
				t.Errorf("the revocation shape lacks %q", need)
			}
		}
	})

	t.Run("revoked_is_final_and_identity_is_immutable", func(t *testing.T) {
		var fn string
		for _, s := range stmts {
			if strings.HasPrefix(s, "CREATE FUNCTION kodama_run_materialization_guard()") {
				fn = s
			}
		}
		for _, need := range []string{
			"IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.step_key, NEW.attempt, NEW.materialization_id, NEW.issued_at) IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.step_key, OLD.attempt, OLD.materialization_id, OLD.issued_at) THEN",
			"IF OLD.state = 'revoked' AND (NEW.state, NEW.revoked_at, NEW.revocation_evidence) IS DISTINCT FROM (OLD.state, OLD.revoked_at, OLD.revocation_evidence) THEN",
		} {
			if !strings.Contains(fn, need) {
				t.Errorf("kodama_run_materialization_guard lacks %q", need)
			}
		}
		if strings.Count(fn, "ERRCODE = '23514'") != 2 {
			t.Error("both refusals are check violations")
		}
		if !slices.Contains(stmts, "CREATE TRIGGER run_materializations_guard BEFORE UPDATE ON run_materializations FOR EACH ROW EXECUTE FUNCTION kodama_run_materialization_guard()") {
			t.Error("the guard is not a BEFORE UPDATE row trigger")
		}
	})

	t.Run("tenant_isolation_and_narrow_grants", func(t *testing.T) {
		for _, need := range []string{
			"ALTER TABLE run_materializations ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE run_materializations FORCE ROW LEVEL SECURITY",
			"CREATE POLICY tenant_isolation ON run_materializations USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())",
			"GRANT SELECT, INSERT ON run_materializations TO {{runtime_role}}",
			"GRANT UPDATE (state, revoked_at, revocation_evidence) ON run_materializations TO {{runtime_role}}",
		} {
			if !slices.Contains(stmts, need) {
				t.Errorf("missing %q", need)
			}
		}
		if !strings.Contains(strings.Join(stmts, "\n"), "CREATE POLICY owner_all ON run_materializations TO %I") {
			t.Error("the owner policy is missing")
		}
		for _, g := range statementsMatching(stmts, `^GRANT `) {
			if regexp.MustCompile(`(?i)\b(DELETE|TRUNCATE|ALL)\b`).MatchString(g) || strings.Contains(g, "TO PUBLIC") {
				t.Errorf("over-broad grant %q", g)
			}
		}
		got := slices.Clone(updateGrants(t)["run_materializations"])
		sort.Strings(got)
		if !slices.Equal(got, []string{"revocation_evidence", "revoked_at", "state"}) {
			t.Errorf("update grant = %v", got)
		}
		for _, c := range []string{"tenant_id", "project_id", "run_id", "step_key", "attempt", "materialization_id", "issued_at"} {
			if slices.Contains(updateGrants(t)["run_materializations"], c) {
				t.Errorf("the runtime role can update %s", c)
			}
		}
	})
}
