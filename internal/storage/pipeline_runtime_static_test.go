package storage_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/storage"
)

// expandSQL returns the embedded expand migration.
func expandSQL(t testing.TB) string {
	t.Helper()
	for _, m := range storage.BaseContribution().Migrations {
		if m.ID == expandMigration {
			return m.SQL
		}
	}
	t.Fatalf("%s is not in the base contribution", expandMigration)
	return ""
}

// sqlStatements removes line comments and splits SQL into statements, treating
// a $$ ... $$ body as part of one statement. Whitespace is collapsed. It is a
// reading aid for tests over the migration text, not a SQL parser.
func sqlStatements(sql string) []string {
	var code strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code.WriteString(line)
		code.WriteByte('\n')
	}
	var out []string
	var cur strings.Builder
	in := false
	text := code.String()
	for i := 0; i < len(text); i++ {
		if strings.HasPrefix(text[i:], "$$") {
			in = !in
			cur.WriteString("$$")
			i++
			continue
		}
		if text[i] == ';' && !in {
			out = append(out, strings.Join(strings.Fields(cur.String()), " "))
			cur.Reset()
			continue
		}
		cur.WriteByte(text[i])
	}
	if rest := strings.Join(strings.Fields(cur.String()), " "); rest != "" {
		out = append(out, rest)
	}
	return out
}

func statementsMatching(stmts []string, pattern string) []string {
	re := regexp.MustCompile(pattern)
	var out []string
	for _, s := range stmts {
		if re.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

// addedColumns returns the column definitions of every ALTER TABLE <table> ADD
// COLUMN in the statements, by column name.
func addedColumns(stmts []string, table string) map[string]string {
	cols := map[string]string{}
	re := regexp.MustCompile(`ADD COLUMN (\w+) ([^,]+)`)
	for _, s := range statementsMatching(stmts, `^ALTER TABLE `+table+` `) {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			cols[m[1]] = m[2]
		}
	}
	return cols
}

func constraintNames(stmts []string) []string {
	var names []string
	for _, s := range stmts {
		for _, m := range regexp.MustCompile(`ADD CONSTRAINT (\w+)`).FindAllStringSubmatch(s, -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// expandStatementShape checks the statement-level shape of the expand migration.
func expandStatementShape(t *testing.T) {
	stmts := sqlStatements(expandSQL(t))

	// Columns: the full execution representation, all nullable or defaulted so the
	// current composition keeps working.
	want := map[string][]string{
		"runs": {"lease_owner", "lease_expires_at", "lease_fence", "resource_identity", "resource_spec_digest",
			"cleanup_state", "cleanup_attempts", "cleanup_next_at", "retained_resources"},
		"run_steps": {"step_kind", "workload_id", "workload_spec_digest", "command_intent", "command_intent_fence",
			"command_intent_at", "launch_evidence", "workspace_held", "wait_id", "wait_assignment", "wait_correlation",
			"wait_request", "wait_input", "wait_outcomes", "wait_approving", "wait_on_expiry", "wait_deadline_at",
			"wait_state", "wait_outcome", "wait_answer", "wait_actor_ref", "wait_decided_at",
			"wait_consumed_signal_id", "wait_version"},
		"tasks": {"pipeline_id", "current_run_id", "status", "current_step_key", "waiting_for", "result",
			"progress_version", "progress_run_version"},
	}
	for table, names := range want {
		cols := addedColumns(stmts, table)
		for _, name := range names {
			def, ok := cols[name]
			if !ok {
				t.Errorf("%s.%s is not added", table, name)
				continue
			}
			if strings.Contains(def, "NOT NULL") && !strings.Contains(def, "DEFAULT") {
				t.Errorf("%s.%s is NOT NULL without a default: %s", table, name, def)
			}
			if table == "tasks" && strings.Contains(def, "NOT NULL") {
				t.Errorf("tasks.%s must be nullable at the expand stage: %s", name, def)
			}
			if strings.HasPrefix(name, "wait_") && strings.Contains(def, "NOT NULL") {
				t.Errorf("run_steps.%s must be nullable (a container step has no wait): %s", name, def)
			}
		}
	}
	if def := addedColumns(stmts, "run_steps")["step_kind"]; !strings.Contains(def, "DEFAULT 'container'") {
		t.Errorf("step_kind must default to container so existing rows stay valid: %q", def)
	}
	if def := addedColumns(stmts, "run_steps")["launch_evidence"]; !strings.Contains(def, "DEFAULT 'none'") {
		t.Errorf("launch_evidence must default to none: %q", def)
	}
	if !slices.ContainsFunc(stmts, func(s string) bool { return s == "ALTER TABLE run_steps ALTER COLUMN image DROP NOT NULL" }) {
		t.Error("a wait step has no image: image must stop being NOT NULL")
	}

	// Constraints and indexes the contract names.
	names := constraintNames(stmts)
	for _, c := range []string{
		"pipelines_name_unique", "runs_lease_pair", "runs_task_key", "runs_cleanup_state_known", "runs_retained_resources_shape",
		"run_steps_kind_known", "run_steps_image_shape", "run_steps_waiting_is_wait", "run_steps_wait_open",
		"run_steps_wait_resolved", "run_steps_wait_expired", "run_steps_wait_cancelled", "run_steps_wait_identity",
		"run_steps_wait_signal_fk", "run_steps_workspace_held_shape", "run_steps_intent_shape",
		"tasks_pipeline_fk", "tasks_current_run_fk", "tasks_projection_shape", "tasks_progress_pair",
	} {
		if !slices.Contains(names, c) {
			t.Errorf("constraint %s is missing", c)
		}
	}
	text := strings.Join(stmts, ";\n")
	for _, ix := range []string{
		"runs_lease_expiry", "runs_cleanup_due", "runs_task", "signal_records_one_winner", "run_steps_workspace_exclusive",
		"run_steps_wait_due", "run_steps_wait_inbox", "run_steps_wait_correlation", "tasks_current_run", "tasks_pipeline",
	} {
		if !regexp.MustCompile(`CREATE (UNIQUE )?INDEX ` + ix + ` `).MatchString(text) {
			t.Errorf("index %s is missing", ix)
		}
	}
	if !regexp.MustCompile(`CREATE INDEX run_steps_wait_due ON run_steps \(tenant_id, wait_deadline_at, [^)]*\) WHERE wait_state = 'waiting'`).MatchString(text) {
		t.Error("due waits are indexed by deadline over open waits only")
	}
	if !regexp.MustCompile(`CREATE INDEX tasks_current_run ON tasks \(tenant_id, project_id, current_run_id\) WHERE current_run_id IS NOT NULL`).MatchString(text) {
		t.Error("task current-run lookup index is missing")
	}
	if !regexp.MustCompile(`CREATE UNIQUE INDEX run_steps_workspace_exclusive ON run_steps \(tenant_id, project_id, run_id\) WHERE workspace_held`).MatchString(text) {
		t.Error("workspace exclusion must be one holder per run")
	}

	// current_run_id is proven against the task and project, not only the project.
	if !regexp.MustCompile(`tasks_current_run_fk FOREIGN KEY \(tenant_id, project_id, id, current_run_id\) REFERENCES runs \(tenant_id, project_id, task_id, id\)`).MatchString(text) {
		t.Error("current_run_id must reference (tenant, project, task, run)")
	}
	if !regexp.MustCompile(`runs_task_key UNIQUE \(tenant_id, project_id, task_id, id\)`).MatchString(text) {
		t.Error("runs must expose the (tenant, project, task, run) key the task references")
	}

	// Forced RLS, tenant policy and a runtime grant for every table created.
	for _, s := range statementsMatching(stmts, `^CREATE TABLE `) {
		table := regexp.MustCompile(`^CREATE TABLE (\w+)`).FindStringSubmatch(s)[1]
		for _, need := range []string{
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY", "ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY",
			"CREATE POLICY tenant_isolation ON " + table + " USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())",
		} {
			if !slices.Contains(stmts, need) {
				t.Errorf("%s: missing %q", table, need)
			}
		}
		if len(statementsMatching(stmts, `format\('CREATE POLICY owner_all ON `+table)) != 1 {
			t.Errorf("%s: owner policy is missing", table)
		}
		if !slices.Contains(stmts, "GRANT SELECT, INSERT ON "+table+" TO {{runtime_role}}") {
			t.Errorf("%s: runtime grant must be exactly SELECT, INSERT", table)
		}
		for _, g := range statementsMatching(stmts, `^GRANT .* ON `+table+` `) {
			if g != "GRANT SELECT, INSERT ON "+table+" TO {{runtime_role}}" {
				t.Errorf("%s: unexpected grant %q (the table is append-only)", table, g)
			}
		}
	}

	// Runtime column grants never reach admitted inputs, snapshots or ownership.
	immutable := map[string][]string{
		"runs":      {"tenant_id", "project_id", "id", "pipeline_id", "task_id", "definition_snapshot", "inputs", "sources", "bindings", "trigger_key", "request_key", "request_principal", "request_hash"},
		"run_steps": {"tenant_id", "project_id", "run_id", "id", "step_key", "ordinal", "image", "step_kind"},
		"tasks":     {"tenant_id", "project_id", "id", "kind", "created_at"},
	}
	for _, g := range statementsMatching(stmts, `^GRANT UPDATE \(`) {
		m := regexp.MustCompile(`^GRANT UPDATE \(([^)]*)\) ON (\w+) TO {{runtime_role}}$`).FindStringSubmatch(g)
		if m == nil {
			t.Errorf("unexpected grant %q", g)
			continue
		}
		cols := strings.Split(strings.ReplaceAll(m[1], " ", ""), ",")
		for _, c := range immutable[m[2]] {
			if slices.Contains(cols, c) {
				t.Errorf("runtime can update %s.%s", m[2], c)
			}
		}
	}
	for table, cols := range map[string][]string{
		"runs":      {"lease_owner", "lease_expires_at", "lease_fence", "cleanup_state", "retained_resources"},
		"run_steps": {"wait_state", "wait_outcome", "wait_consumed_signal_id", "wait_version", "workspace_held", "command_intent"},
		"tasks":     {"current_run_id", "status", "progress_version", "progress_run_version", "pipeline_id"},
	} {
		grant := statementsMatching(stmts, `^GRANT UPDATE \(.*\) ON `+table+` TO {{runtime_role}}$`)
		if len(grant) != 1 {
			t.Fatalf("%s: want one column grant, got %d", table, len(grant))
		}
		for _, c := range cols {
			if !strings.Contains(grant[0], c) {
				t.Errorf("runtime cannot update %s.%s", table, c)
			}
		}
	}

	// Guard triggers: the lease fence, wait lifecycle and task projection are
	// enforced by the database, whoever writes.
	for _, tr := range []string{
		"CREATE TRIGGER runs_guard BEFORE UPDATE ON runs",
		"CREATE TRIGGER run_steps_guard BEFORE UPDATE ON run_steps",
		"CREATE TRIGGER tasks_guard BEFORE UPDATE ON tasks",
	} {
		if len(statementsMatching(stmts, "^"+regexp.QuoteMeta(tr)+" ")) != 1 {
			t.Errorf("missing %q", tr)
		}
	}
	// The guards compare versions with explicit NULL handling.
	guards := strings.Join(statementsMatching(stmts, `^CREATE FUNCTION kodama_(run_step|task)_guard`), "\n")
	for _, need := range []string{
		"OLD.wait_version IS NOT NULL AND (NEW.wait_version IS NULL OR NEW.wait_version < OLD.wait_version)",
		"NEW.wait_consumed_signal_id, NEW.wait_version)",
		"OLD.progress_version IS NOT NULL AND (NEW.progress_version IS NULL OR NEW.progress_version < OLD.progress_version)",
		"OLD.progress_run_version IS NOT NULL AND NEW.progress_run_version IS NULL",
	} {
		if !strings.Contains(guards, need) {
			t.Errorf("version guard lacks %q", need)
		}
	}
	// The signal record outlives its wait: no reference to run, step or wait, and
	// it is removed only with the project.
	signal := statementsMatching(stmts, `^CREATE TABLE signal_records`)
	if len(signal) != 1 {
		t.Fatal("signal_records is not created exactly once")
	}
	for _, need := range []string{
		"PRIMARY KEY (tenant_id, project_id, caller_principal, caller_key)",
		"FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id) ON DELETE CASCADE",
		"request_digest text NOT NULL", "target_run_id kodama_id", "target_step_id kodama_id", "target_wait_id kodama_id",
		"decision_kind text", "decision_outcome text", "decision_answer text", "decision_actor_ref text", "decision_version kodama_version",
		"decided_at timestamptz", "refusal_reason text", "signal_records_tombstone_shape",
	} {
		if !strings.Contains(signal[0], need) {
			t.Errorf("signal_records lacks %q", need)
		}
	}
	if regexp.MustCompile(`(?i)REFERENCES (runs|run_steps)`).MatchString(signal[0]) {
		t.Error("a signal record must not reference runs or steps: retention removes them")
	}
	if regexp.MustCompile(`(?i)expire|ttl|retention|delete_after`).MatchString(strings.ReplaceAll(strings.ReplaceAll(signal[0], "'expired'", ""), "expired", "")) {
		t.Error("signal records must not expire")
	}
}

// Test_unit_storage_pipeline_name_unique is the infrastructure-free check of the
// validate-and-map stage: the migration text refuses duplicate project-scoped
// pipeline names and ambiguous task mappings before it changes anything, adds the
// project-scoped key only after validating, and never creates, merges or chooses
// a pipeline. It proves the declared order and content; real refusal, rollback and
// enforcement are executed by Test_pipeline_name_unique_postgres.
func Test_unit_storage_pipeline_name_unique(t *testing.T) {
	stmts := sqlStatements(expandSQL(t))
	if len(stmts) < 5 {
		t.Fatalf("migration has %d statements", len(stmts))
	}

	t.Run("validation_precedes_every_change", func(t *testing.T) {
		// Writers are excluded first, so nothing can commit between a check and the
		// mapping that relies on it; the lock is held to the end of the transaction.
		if stmts[0] != "LOCK TABLE pipelines, runs, tasks IN SHARE ROW EXCLUSIVE MODE" {
			t.Errorf("statement 0 = %q, want the write-excluding lock on every table validated or mapped", stmts[0])
		}
		for _, release := range statementsMatching(stmts, `(?i)^(COMMIT|ROLLBACK|RESET|SET LOCAL lock_timeout)`) {
			t.Errorf("the lock must be held through mapping: %q", release)
		}
		for i, want := range []struct{ errcode, probe string }{
			{"23505", "GROUP BY tenant_id, project_id, name HAVING count(*) > 1"},
			{"23514", "GROUP BY tenant_id, project_id, task_id HAVING count(DISTINCT pipeline_id) > 1"},
		} {
			s := stmts[i+1]
			if !strings.HasPrefix(s, "DO $$") || !strings.Contains(s, want.probe) ||
				!strings.Contains(s, "RAISE EXCEPTION") || !strings.Contains(s, "USING ERRCODE = '"+want.errcode+"'") ||
				!strings.Contains(s, "nothing was changed") {
				t.Errorf("statement %d is not the expected refusing validation (%s): %.160s", i+1, want.errcode, s)
			}
		}
		if !strings.Contains(stmts[1], "FROM pipelines") || !strings.Contains(stmts[2], "FROM runs WHERE task_id IS NOT NULL") {
			t.Error("validation must read pipelines and runs")
		}
		// Nothing before the validations mutates.
		for i := 3; i < len(stmts); i++ {
			if regexp.MustCompile(`^(ALTER|CREATE|UPDATE|INSERT|GRANT|DROP)`).MatchString(stmts[i]) {
				if i != 3 || !strings.Contains(stmts[i], "pipelines_name_unique") {
					t.Errorf("statement %d precedes the unique key: %.100s", i, stmts[i])
				}
				break
			}
		}
	})

	t.Run("project_scoped_key_follows_validation", func(t *testing.T) {
		const key = "ALTER TABLE pipelines ADD CONSTRAINT pipelines_name_unique UNIQUE (tenant_id, project_id, name)"
		if stmts[3] != key {
			t.Errorf("statement 3 = %q, want %q", stmts[3], key)
		}
		// The baseline repository-scoped key is left alone (retired at contract).
		if statementsMatching(stmts, `(?i)DROP .*pipelines|pipelines.* DROP |ALTER TABLE pipelines DROP`) != nil {
			t.Error("nothing about pipelines may be dropped at the expand stage")
		}
	})

	t.Run("no_pipeline_is_invented_or_chosen", func(t *testing.T) {
		for _, s := range stmts {
			switch {
			case regexp.MustCompile(`(?i)^(INSERT INTO|DELETE FROM|TRUNCATE) (pipelines|repositories)`).MatchString(s),
				regexp.MustCompile(`(?i)^UPDATE (pipelines|repositories|runs)\b`).MatchString(s):
				t.Errorf("the migration must not create, remove or rewrite pipelines, repositories or runs: %.100s", s)
			case regexp.MustCompile(`(?i)\bLIMIT 1\b`).MatchString(s) && !strings.HasPrefix(s, "DO $$"):
				t.Errorf("an arbitrary pick (LIMIT 1) outside the refusal reports: %.100s", s)
			case regexp.MustCompile(`(?i)ORDER BY[^;]*repo_id`).MatchString(s):
				t.Errorf("a repository is never chosen by ordering: %.100s", s)
			}
		}
		lock := slices.Index(stmts, "LOCK TABLE pipelines, runs, tasks IN SHARE ROW EXCLUSIVE MODE")
		if mapAt := slices.IndexFunc(stmts, func(s string) bool { return strings.HasPrefix(s, "UPDATE tasks ") }); lock != 0 || mapAt < 3 {
			t.Errorf("the mapping (statement %d) must follow the lock (statement %d) and both validations", mapAt, lock)
		}
		maps := statementsMatching(stmts, `^UPDATE tasks `)
		if len(maps) != 1 {
			t.Fatalf("want exactly one task mapping statement, got %d", len(maps))
		}
		m := maps[0]
		for _, need := range []string{
			"SET pipeline_id = m.pipeline_id", "FROM runs WHERE task_id IS NOT NULL GROUP BY tenant_id, project_id, task_id",
			"t.tenant_id = m.tenant_id AND t.project_id = m.project_id AND t.id = m.task_id", "t.pipeline_id IS NULL",
		} {
			if !strings.Contains(m, need) {
				t.Errorf("mapping lacks %q: %s", need, m)
			}
		}
		// A task with no run, and a run that is current, are never invented.
		if strings.Contains(m, "current_run_id") || strings.Contains(m, "status") {
			t.Errorf("the mapping must not project execution state: %s", m)
		}
	})

	t.Run("nothing_is_dropped", func(t *testing.T) {
		code := strings.Join(stmts, ";\n")
		for _, legacy := range []string{"task_workflow", "parked_at", "resume_state", "blocked_by_id", "resolution", "resolved_at"} {
			if regexp.MustCompile(`(?i)(DROP|RENAME)[^;]*\b` + legacy + `\b`).MatchString(code) {
				t.Errorf("legacy column %s is touched at the expand stage", legacy)
			}
		}
		if regexp.MustCompile(`(?i)\bDROP (TABLE|COLUMN|SCHEMA|POLICY|TRIGGER|INDEX)\b|\bTRUNCATE\b|\bDELETE FROM\b`).MatchString(code) {
			t.Error("destructive statement in the expand migration")
		}
	})

	t.Run("earlier_migrations_are_immutable", func(t *testing.T) {
		// 001 to 003 are deployed checksums: a schema change is a new migration.
		want := map[string]string{
			"001_schema.sql":      "62db9a805ed7e08bc9cce0a34f0ebe2007b3e51993f4e8d04de86c240682df60",
			"002_grants.sql":      "e518df487102cc542b81508b78b1847519045975ea25b7d316b96dc72f24d8ae",
			"003_git_hosting.sql": "c4ed0d2405396a950ada3ed0efb07dbf82d8f02918135b395b753b710517b2fa",
		}
		for _, m := range storage.BaseContribution().Migrations {
			if sum, ok := want[m.ID]; ok && m.Checksum() != sum {
				t.Errorf("%s was modified: checksum %s", m.ID, m.Checksum())
			}
		}
	})
}
