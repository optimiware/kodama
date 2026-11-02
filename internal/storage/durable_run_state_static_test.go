package storage_test

import (
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// This file is the infrastructure-free evidence for the durable run-state
// migration (005). It proves what the migration text declares and that the schema
// has a home for every field of the kernel's runtime.RunRecord. It does not prove
// what PostgreSQL enforces: Test_durable_run_state_postgres executes the
// constraints, guards, grants, forced row security and rollback against a real
// service under a non-owner role, and is the evidence for those.

func migrationSQL(t testing.TB, id string) string {
	t.Helper()
	for _, m := range storage.BaseContribution().Migrations {
		if m.ID == id {
			return m.SQL
		}
	}
	t.Fatalf("%s is not in the base contribution", id)
	return ""
}

func durableStatements(t testing.TB) []string {
	return sqlStatements(migrationSQL(t, durableMigration))
}

// topLevelCommas splits s at commas outside parentheses and quotes.
func topLevelCommas(s string) []string {
	var parts []string
	depth, start := 0, 0
	quoted := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// schemaColumns returns every column of every table the base migrations create
// or extend, as "table.column".
func schemaColumns(t testing.TB) map[string]bool {
	t.Helper()
	cols := map[string]bool{}
	create := regexp.MustCompile(`^CREATE TABLE (\w+) \((.*)\)$`)
	add := regexp.MustCompile(`ADD COLUMN (\w+) `)
	for _, m := range storage.BaseContribution().Migrations {
		for _, s := range sqlStatements(m.SQL) {
			if c := create.FindStringSubmatch(s); c != nil {
				for _, part := range topLevelCommas(c[2]) {
					first, _, _ := strings.Cut(part, " ")
					switch first {
					case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT", "EXCLUDE", "":
						continue
					}
					cols[c[1]+"."+first] = true
				}
			}
			if a := regexp.MustCompile(`^ALTER TABLE (\w+) `).FindStringSubmatch(s); a != nil {
				for _, m := range add.FindAllStringSubmatch(s, -1) {
					cols[a[1]+"."+m[1]] = true
				}
			}
		}
	}
	return cols
}

// updateGrants returns the columns the runtime role may update per table,
// accumulated over every migration.
func updateGrants(t testing.TB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	re := regexp.MustCompile(`^GRANT UPDATE \(([^)]*)\) ON (\w+) TO {{runtime_role}}$`)
	for _, m := range storage.BaseContribution().Migrations {
		for _, s := range sqlStatements(m.SQL) {
			if g := re.FindStringSubmatch(s); g != nil {
				for _, c := range strings.Split(strings.ReplaceAll(g[1], " ", ""), ",") {
					out[g[2]] = append(out[g[2]], c)
				}
			}
		}
	}
	return out
}

// recordHomes states where each field of the kernel record is stored. A leaf is
// "table.column"; "@Type" says the field is a nested record that has its own row
// in this table; a field marked with "plan" lives in the immutable plan, which is
// the admitted run's definition_snapshot. The test below fails when the kernel
// gains or drops a field without this table (and the schema) being updated.
var recordHomes = map[string]map[string][]string{
	"RunRecord": {"run": {"@Run"}, "plan": {"runs.definition_snapshot"}, "task": {"@TaskRef"}, "claim": {"@Claim"},
		"waits": {"@Wait"}, "commands": {"@Command"}, "workloads": {"@WorkloadObservation"}, "resources": {"@ResourceObservation"},
		"outputs": {"@StepOutputs"}, "materializations": {"@MaterializationRecord"}},
	// The owning task is written by the run's insert and never updated; its project is the run's.
	"TaskRef": {"scope": {"runs.tenant_id", "runs.project_id"}, "id": {"runs.task_id"}},
	"Run": {"scope": {"runs.tenant_id", "runs.project_id"}, "id": {"runs.id"}, "pipelineId": {"runs.pipeline_id"},
		"version": {"runs.version"}, "outcome": {"runs.status"}, "reason": {"runs.reason"},
		"cancelRequested": {"runs.cancel_requested_at"}, "cleanup": {"@Cleanup"}, "steps": {"@Step"}},
	// The retained resources and workloads are tagged entries of one bounded array.
	"Cleanup": {"state": {"runs.cleanup_state"}, "attempts": {"runs.cleanup_attempts"}, "maxAttempts": {"runs.cleanup_max_attempts"},
		"retained": {"runs.retained_resources"}, "retainedWorkloads": {"runs.retained_resources"}, "reason": {"runs.cleanup_reason"}},
	"Claim": {"scope": {"runs.tenant_id", "runs.project_id"}, "runId": {"runs.id"}, "owner": {"runs.lease_owner"},
		"purpose": {"runs.lease_purpose"}, "fence": {"runs.lease_fence"}, "expires": {"runs.lease_expires_at"}},
	"Step": {"key": {"run_steps.step_key"}, "kind": {"run_steps.step_kind"}, "state": {"run_steps.status"},
		"reason": {"run_steps.reason"}, "version": {"run_steps.version"}, "attempt": {"run_steps.attempt"},
		"result": {"@Result"}, "predicate": {"@PredicateEvaluation"}},
	// A step's result code and reason are their own columns; its exit code is the existing exit_code.
	"Result": {"code": {"run_steps.result_code", "run_commands.result_code", "run_workload_observations.result_code"},
		"exitCode": {"run_steps.exit_code", "run_commands.result_exit_code", "run_workload_observations.result_exit_code"},
		"reason":   {"run_steps.result_reason", "run_commands.result_reason", "run_workload_observations.result_reason"}},
	"PredicateEvaluation": {"evaluated": {"run_steps.when_evaluated"}, "result": {"run_steps.when_result"},
		"inputDigest": {"run_steps.when_input_digest"}, "at": {"run_steps.when_evaluated_at"}},
	// Spec and the opening time come from the plan and the deadline: deadline = opened + the plan's timeout.
	"Wait": {"scope": {"run_steps.tenant_id", "run_steps.project_id"}, "id": {"run_steps.wait_id"}, "runId": {"run_steps.run_id"},
		"stepKey": {"run_steps.step_key"}, "spec": {"runs.definition_snapshot"}, "assignment": {"run_steps.wait_assignment"},
		"correlation": {"run_steps.wait_correlation"}, "request": {"run_steps.wait_request"}, "state": {"run_steps.wait_state", "run_steps.status"},
		"version": {"run_steps.wait_version"}, "openedAt": {"run_steps.wait_deadline_at", "runs.definition_snapshot"},
		"deadline": {"run_steps.wait_deadline_at"}, "decision": {"@Decision"}},
	"Decision": {"kind": {"run_steps.wait_state"}, "signalId": {"run_steps.wait_consumed_signal_id"}, "outcome": {"run_steps.wait_outcome"},
		"answer": {"run_steps.wait_answer"}, "actor": {"run_steps.wait_actor_ref"}, "reason": {"run_steps.wait_reason"},
		"version": {"run_steps.wait_version"}, "at": {"run_steps.wait_decided_at"}},
	"Command": {"id": {"run_commands.command_id"}, "scope": {"run_commands.tenant_id", "run_commands.project_id"},
		"runId": {"run_commands.run_id"}, "stepKey": {"run_commands.step_key"}, "kind": {"run_commands.kind"},
		"target": {"run_commands.target"}, "specDigest": {"run_commands.spec_digest"}, "fence": {"run_commands.fence"},
		"binding": {"@RuntimeBinding"}, "state": {"run_commands.state"}, "reason": {"run_commands.reason"}, "result": {"@Result"},
		"recordedAt": {"run_commands.recorded_at"}},
	// A command's binding repeats its scope, run, step, target, specification and fence; only the
	// attempt and the backend are separate columns (the kernel requires the rest to be equal).
	"RuntimeBinding": {"scope": {"run_commands.tenant_id", "run_workload_observations.tenant_id"},
		"runId":      {"run_commands.run_id", "run_workload_observations.run_id"},
		"stepKey":    {"run_commands.step_key", "run_workload_observations.step_key"},
		"attempt":    {"run_commands.binding_attempt", "run_workload_observations.attempt"},
		"workloadId": {"run_commands.target", "run_workload_observations.workload_id"},
		"specDigest": {"run_commands.spec_digest", "run_workload_observations.spec_digest"},
		"backend":    {"run_commands.binding_backend", "run_workload_observations.backend"},
		"fence":      {"run_commands.fence", "run_workload_observations.fence"}},
	"WorkloadObservation": {"binding": {"@RuntimeBinding"}, "phase": {"run_workload_observations.phase"},
		"result": {"@Result"}, "observedAt": {"run_workload_observations.observed_at"}},
	"ResourceObservation": {"handle": {"@ResourceHandle"}, "state": {"run_resource_observations.state"},
		"reason": {"run_resource_observations.reason"}, "observedAt": {"run_resource_observations.observed_at"}},
	// The outputs of a succeeded container step are the step's outputs column, recorded with its
	// settlement (006 guards it); the attempt is the step's.
	"StepOutputs": {"stepKey": {"run_steps.step_key"}, "attempt": {"run_steps.attempt"}, "values": {"run_steps.outputs"}},
	// A materialization has its own row (006): the identity is a pure function of the key.
	"MaterializationRecord": {"ref": {"@MaterializationRef"}, "state": {"run_materializations.state"},
		"issuedAt": {"run_materializations.issued_at"}},
	"MaterializationRef": {"scope": {"run_materializations.tenant_id", "run_materializations.project_id"},
		"runId": {"run_materializations.run_id"}, "stepKey": {"run_materializations.step_key"},
		"attempt": {"run_materializations.attempt"}, "id": {"run_materializations.materialization_id"}},
	"ResourceHandle": {"scope": {"run_resource_observations.tenant_id", "run_resource_observations.project_id"},
		"owner":    {"run_resource_observations.tenant_id", "run_resource_observations.owner_project_id"},
		"lifetime": {"run_resource_observations.lifetime"}, "runId": {"run_resource_observations.handle_run_id"},
		"kind": {"run_resource_observations.kind"}, "name": {"run_resource_observations.name"},
		"identity": {"run_resource_observations.identity"}, "fence": {"run_resource_observations.fence"}},
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Test_unit_storage_durable_run_state: the durable run-state migration is
// registered after the earlier migrations, is expand-only, adds exactly the
// columns and tables the kernel record needs, gives every field of RunRecord a
// column, matches the kernel's vocabulary, and keeps tenant authority and narrow
// grants on every new table.
func Test_unit_storage_durable_run_state(t *testing.T) {
	t.Run("registered_after_the_earlier_migrations", func(t *testing.T) {
		var ids []string
		for _, m := range storage.BaseContribution().Migrations {
			ids = append(ids, m.ID)
		}
		want := []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", expandMigration, durableMigration, waitMigration, reasonMigration}
		if !slices.Equal(ids, want) {
			t.Fatalf("manifest = %v, want %v", ids, want)
		}
		// 001 to 004 are deployed checksums: a schema change is a new migration.
		for id, sum := range map[string]string{
			"001_schema.sql":      "62db9a805ed7e08bc9cce0a34f0ebe2007b3e51993f4e8d04de86c240682df60",
			"002_grants.sql":      "e518df487102cc542b81508b78b1847519045975ea25b7d316b96dc72f24d8ae",
			"003_git_hosting.sql": "c4ed0d2405396a950ada3ed0efb07dbf82d8f02918135b395b753b710517b2fa",
			expandMigration:       "aca8523f764eff8be472f4be773385e7f29bfc3925e5b1418d92f0d011d05df6",
		} {
			for _, m := range storage.BaseContribution().Migrations {
				if m.ID == id && m.Checksum() != sum {
					t.Errorf("%s was modified: checksum %s", id, m.Checksum())
				}
			}
		}
	})

	t.Run("expand_only", func(t *testing.T) {
		stmts := durableStatements(t)
		text := strings.Join(stmts, ";\n")
		if regexp.MustCompile(`(?i)\bDROP (TABLE|COLUMN|SCHEMA|POLICY|TRIGGER|INDEX|DOMAIN|FUNCTION)\b|\bTRUNCATE\b|\bDELETE FROM\b|\bINSERT INTO\b`).MatchString(text) {
			t.Error("destructive or data-inventing statement in the expand migration")
		}
		// Only four constraints of 004 are replaced, because their old form forbade a wait
		// that has an identity but has not opened. Nothing else is dropped.
		var dropped []string
		for _, s := range stmts {
			for _, m := range regexp.MustCompile(`DROP CONSTRAINT (\w+)`).FindAllStringSubmatch(s, -1) {
				dropped = append(dropped, m[1])
			}
		}
		wantDropped := []string{"run_steps_container_no_wait", "run_steps_wait_unopened", "run_steps_wait_open", "run_steps_wait_cancelled"}
		if !slices.Equal(dropped, wantDropped) {
			t.Errorf("dropped constraints = %v, want %v", dropped, wantDropped)
		}
		for _, name := range wantDropped {
			if !slices.Contains(constraintNames(stmts), name) {
				t.Errorf("replaced constraint %s is not re-added", name)
			}
		}
		// Legacy objects stay readable: the project workflow and the task workflow columns.
		for _, legacy := range []string{"task_workflow", "parked_at", "resume_state", "blocked_by_id", "resolution", "resolved_at"} {
			if regexp.MustCompile(`\b` + legacy + `\b`).MatchString(text) {
				t.Errorf("legacy column %s is mentioned at the expand stage", legacy)
			}
		}
		if len(statementsMatching(stmts, `^ALTER TABLE (projects|tasks|pipelines|signal_records) `)) != 0 {
			t.Error("only runs and run_steps are extended")
		}
		// Data statements touch only the rows that would violate a new constraint.
		for _, s := range statementsMatching(stmts, `^UPDATE `) {
			if !regexp.MustCompile(` WHERE `).MatchString(s) {
				t.Errorf("an unconditional update: %s", s)
			}
		}
		if n := len(statementsMatching(stmts, `^UPDATE `)); n != 4 {
			t.Errorf("%d data statements, want the four narrow backfills", n)
		}
		// Every new column is nullable or defaulted, so the current composition keeps working.
		for _, table := range []string{"runs", "run_steps"} {
			for name, def := range addedColumns(stmts, table) {
				if strings.Contains(def, "NOT NULL") && !strings.Contains(def, "DEFAULT") {
					t.Errorf("%s.%s is NOT NULL without a default: %s", table, name, def)
				}
			}
		}
	})

	t.Run("runs_columns", func(t *testing.T) {
		cols := addedColumns(durableStatements(t), "runs")
		var names []string
		for n := range cols {
			names = append(names, n)
		}
		sort.Strings(names)
		if !slices.Equal(names, []string{"cleanup_max_attempts", "cleanup_reason", "lease_purpose"}) {
			t.Errorf("runs gains %v", names)
		}
		if !strings.Contains(cols["cleanup_max_attempts"], "integer NOT NULL DEFAULT 1") {
			t.Errorf("cleanup_max_attempts: %q", cols["cleanup_max_attempts"])
		}
		sql := strings.Join(durableStatements(t), ";\n")
		for _, need := range []string{
			"CHECK (lease_purpose IN ('execute', 'reconcile', 'cleanup'))",
			"CHECK ((lease_owner IS NULL) = (lease_purpose IS NULL))",
			"CHECK (cleanup_max_attempts >= 1 AND cleanup_attempts <= cleanup_max_attempts)",
			"cleanup_state <> 'retained' OR cleanup_reason IS NOT NULL",
		} {
			if !strings.Contains(sql, need) {
				t.Errorf("runs lacks %q", need)
			}
		}
	})

	t.Run("tables_and_keys", func(t *testing.T) {
		stmts := durableStatements(t)
		keys := map[string]string{
			"run_commands":              "PRIMARY KEY (tenant_id, project_id, run_id, command_id)",
			"run_workload_observations": "PRIMARY KEY (tenant_id, project_id, run_id, step_key, attempt)",
			"run_resource_observations": "PRIMARY KEY (tenant_id, project_id, run_id, kind, name, identity)",
		}
		created := statementsMatching(stmts, `^CREATE TABLE `)
		if len(created) != len(keys) {
			t.Fatalf("%d tables created, want %d", len(created), len(keys))
		}
		for table, key := range keys {
			s := statementsMatching(stmts, `^CREATE TABLE `+table+` `)
			if len(s) != 1 {
				t.Errorf("%s is not created exactly once", table)
				continue
			}
			for _, need := range []string{
				key, "tenant_id kodama_id NOT NULL", "project_id kodama_id NOT NULL", "run_id kodama_id NOT NULL",
				"FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id)",
			} {
				if !strings.Contains(s[0], need) {
					t.Errorf("%s lacks %q", table, need)
				}
			}
			if regexp.MustCompile(`(?i)ON DELETE CASCADE`).MatchString(s[0]) {
				t.Errorf("%s must not cascade: retention removes rows explicitly", table)
			}
		}
		// A workload command and observation name their step in the same run.
		for _, table := range []string{"run_commands", "run_workload_observations"} {
			if !strings.Contains(statementsMatching(stmts, `^CREATE TABLE `+table+` `)[0],
				"FOREIGN KEY (tenant_id, project_id, run_id, step_key) REFERENCES run_steps (tenant_id, project_id, run_id, step_key)") {
				t.Errorf("%s must reference its step within the run", table)
			}
		}
		// Commands: kind, target, digest, fence, state, reason, result, binding and time.
		cmd := statementsMatching(stmts, `^CREATE TABLE run_commands `)[0]
		for _, need := range []string{
			"kind text NOT NULL", "target text NOT NULL", "spec_digest text", "fence bigint NOT NULL", "state text NOT NULL DEFAULT 'intent'",
			"reason text", "result_code text", "result_exit_code integer", "result_reason text", "binding_attempt integer",
			"binding_backend text", "recorded_at timestamptz NOT NULL", "run_commands_binding_shape", "run_commands_state_reason",
			"run_commands_result_shape",
		} {
			if !strings.Contains(cmd, need) {
				t.Errorf("run_commands lacks %q", need)
			}
		}
		// Resource commands belong to no step: step_key is nullable on commands only.
		if strings.Contains(cmd, "step_key text NOT NULL") {
			t.Error("a resource command belongs to no step")
		}
		// Resource observations: workspaces are representable, not only services.
		res := statementsMatching(stmts, `^CREATE TABLE run_resource_observations `)[0]
		for _, need := range []string{"CHECK (kind IN ('workspace', 'service'))", "owner_project_id kodama_id", "lifetime text NOT NULL",
			"handle_run_id kodama_id", "reason text", "observed_at timestamptz NOT NULL", "run_resource_observations_owner"} {
			if !strings.Contains(res, need) {
				t.Errorf("run_resource_observations lacks %q", need)
			}
		}
	})

	t.Run("step_columns", func(t *testing.T) {
		cols := addedColumns(durableStatements(t), "run_steps")
		var names []string
		for n := range cols {
			names = append(names, n)
		}
		sort.Strings(names)
		want := []string{"attempt", "result_code", "result_reason", "wait_reason", "when_evaluated", "when_evaluated_at", "when_input_digest", "when_result"}
		if !slices.Equal(names, want) {
			t.Errorf("run_steps gains %v, want %v", names, want)
		}
		if !strings.Contains(cols["when_evaluated"], "boolean NOT NULL DEFAULT false") || !strings.Contains(cols["attempt"], "integer NOT NULL DEFAULT 0") {
			t.Errorf("defaults: when_evaluated %q attempt %q", cols["when_evaluated"], cols["attempt"])
		}
		// Pre-open waits: the choice is relaxed constraints on the step row, with one
		// exception for a wait cancelled before it opened. An opened wait still needs
		// its correlation, expected input and deadline.
		sql := strings.Join(durableStatements(t), ";\n")
		for _, need := range []string{
			"AND (wait_id IS NULL) = (wait_version IS NULL)",
			"num_nonnulls(wait_id, wait_version) = 2",
			"num_nonnulls(wait_correlation, wait_input, wait_outcomes, wait_on_expiry, wait_deadline_at) = 5",
			"OR (wait_state = 'cancelled' AND num_nonnulls(wait_correlation, wait_request, wait_input, wait_outcomes, wait_approving, wait_on_expiry, wait_deadline_at) = 0)",
			"wait_reason IS NOT NULL AND wait_reason IN ('run_cancelled', 'run_failed')",
		} {
			if !strings.Contains(sql, need) {
				t.Errorf("pre-open wait shape lacks %q", need)
			}
		}
		if regexp.MustCompile(`CREATE TABLE \w*wait`).MatchString(sql) {
			t.Error("the pre-open wait uses relaxed constraints, not a waits table")
		}
	})

	t.Run("every_record_field_has_a_column", func(t *testing.T) {
		known := schemaColumns(t)
		types := map[string]reflect.Type{}
		for _, v := range []any{runtime.RunRecord{}, runtime.Run{}, runtime.Cleanup{}, runtime.Claim{}, runtime.Step{}, runtime.Result{},
			runtime.PredicateEvaluation{}, runtime.Wait{}, runtime.Decision{}, runtime.Command{}, runtime.RuntimeBinding{},
			runtime.WorkloadObservation{}, runtime.ResourceObservation{}, runtime.ResourceHandle{},
			runtime.StepOutputs{}, runtime.MaterializationRecord{}, runtime.MaterializationRef{}, runtime.TaskRef{}} {
			types[reflect.TypeOf(v).Name()] = reflect.TypeOf(v)
		}
		if len(types) != len(recordHomes) {
			t.Fatalf("%d record types, %d homes", len(types), len(recordHomes))
		}
		for name, typ := range types {
			homes, ok := recordHomes[name]
			if !ok {
				t.Errorf("%s has no home table", name)
				continue
			}
			var mapped []string
			for f := range homes {
				mapped = append(mapped, f)
			}
			sort.Strings(mapped)
			if got := jsonFields(typ); !slices.Equal(got, mapped) {
				t.Errorf("%s fields %v are not the mapped fields %v: update the schema and this table together", name, got, mapped)
			}
			for field, targets := range homes {
				for _, target := range targets {
					if nested, ok := strings.CutPrefix(target, "@"); ok {
						if _, ok := types[nested]; !ok {
							t.Errorf("%s.%s: unknown nested record %s", name, field, nested)
						}
						continue
					}
					if !known[target] {
						t.Errorf("%s.%s is stored in %s, which is not a column", name, field, target)
					}
				}
			}
		}
	})

	t.Run("kernel_vocabulary_matches_the_checks", func(t *testing.T) {
		sql := strings.Join(durableStatements(t), ";\n") + ";\n" + strings.Join(sqlStatements(migrationSQL(t, expandMigration)), ";\n")
		quote := func(vs ...string) []string {
			var out []string
			for _, v := range vs {
				out = append(out, "'"+v+"'")
			}
			return out
		}
		for what, vs := range map[string][]string{
			"command kind": quote(string(runtime.CommandLaunch), string(runtime.CommandStop), string(runtime.CommandRemove),
				string(runtime.CommandEnsureResource), string(runtime.CommandStopResource), string(runtime.CommandRemoveResource)),
			"command state": quote(string(runtime.CommandIntent), string(runtime.CommandConfirmed), string(runtime.CommandRejected), string(runtime.CommandUncertain)),
			"result code": quote(string(runtime.ResultSucceeded), string(runtime.ResultFailed), string(runtime.ResultCancelled),
				string(runtime.ResultTimedOut), string(runtime.ResultLost)),
			"workload phase": quote(string(runtime.PhaseRunning), string(runtime.PhaseExited), string(runtime.PhaseAbsent), string(runtime.PhaseUnknown)),
			"resource state": quote(string(runtime.ResourceStarting), string(runtime.ResourceReady), string(runtime.ResourceFailed),
				string(runtime.ResourceStopped), string(runtime.ResourceRemoved), string(runtime.ResourceUnknown)),
			"resource kind":     quote(string(runtime.ResourceWorkspace), string(runtime.ResourceService)),
			"lifetime":          quote(string(runtime.LifetimeTenant), string(runtime.LifetimeProject), string(runtime.LifetimePipeline)),
			"claim purpose":     quote(string(runtime.ClaimExecute), string(runtime.ClaimReconcile), string(runtime.ClaimCleanup)),
			"cancel reason":     quote(string(runtime.ReasonRunCancelled), string(runtime.ReasonRunFailed)),
			"cleanup exhausted": quote(string(runtime.ReasonCleanupExhausted)),
		} {
			for _, v := range vs {
				if !strings.Contains(sql, v) {
					t.Errorf("%s %s is not in any check", what, v)
				}
			}
		}
		// The kernel's cleanup states map one to one onto the runs column.
		for kernel, column := range map[runtime.CleanupState]string{runtime.CleanupNotStarted: "none", runtime.CleanupPending: "pending",
			runtime.CleanupComplete: "done", runtime.CleanupExhausted: "retained"} {
			if !strings.Contains(migrationSQL(t, expandMigration), "'"+column+"'") {
				t.Errorf("cleanup state %s has no runs.cleanup_state value %s", kernel, column)
			}
		}
		// The kernel's step states are the status column's, including waiting.
		for _, s := range []runtime.StepState{runtime.StepPending, runtime.StepRunning, runtime.StepWaiting, runtime.StepSucceeded,
			runtime.StepFailed, runtime.StepSkipped, runtime.StepCancelled} {
			if !strings.Contains(migrationSQL(t, expandMigration), "'"+string(s)+"'") {
				t.Errorf("step state %s is not valid for run_steps.status", s)
			}
		}
	})

	t.Run("tenant_authority_and_narrow_grants", func(t *testing.T) {
		stmts := durableStatements(t)
		tables := []string{"run_commands", "run_workload_observations", "run_resource_observations"}
		for _, table := range tables {
			for _, need := range []string{
				"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY", "ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY",
				"CREATE POLICY tenant_isolation ON " + table + " USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())",
			} {
				if !slices.Contains(stmts, need) {
					t.Errorf("%s: missing %q", table, need)
				}
			}
			if !strings.Contains(strings.Join(stmts, "\n"), "CREATE POLICY owner_all ON "+table+" TO %I") {
				t.Errorf("%s: owner policy is missing", table)
			}
		}
		if !slices.Contains(stmts, "GRANT SELECT, INSERT ON run_commands, run_workload_observations, run_resource_observations TO {{runtime_role}}") {
			t.Error("the runtime role gets exactly SELECT and INSERT on the new tables")
		}
		for _, g := range statementsMatching(stmts, `^GRANT `) {
			if regexp.MustCompile(`(?i)\b(DELETE|TRUNCATE|ALL)\b`).MatchString(g) || strings.Contains(g, "TO PUBLIC") {
				t.Errorf("over-broad grant %q", g)
			}
		}
		// Updates are limited to state columns; identity, target, specification, binding,
		// admitted inputs and the cleanup bound are never updatable.
		grants := updateGrants(t)
		for table, cols := range map[string][]string{
			"run_commands":              {"state", "reason", "result_code", "result_exit_code", "result_reason"},
			"run_workload_observations": {"fence", "phase", "result_code", "result_exit_code", "result_reason", "observed_at"},
			"run_resource_observations": {"fence", "state", "reason", "observed_at"},
		} {
			got := slices.Clone(grants[table])
			sort.Strings(got)
			want := slices.Clone(cols)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s update grant = %v, want %v", table, got, want)
			}
		}
		for table, never := range map[string][]string{
			"run_commands":              {"tenant_id", "project_id", "run_id", "command_id", "step_key", "kind", "target", "spec_digest", "fence", "binding_attempt", "binding_backend", "recorded_at"},
			"run_workload_observations": {"tenant_id", "project_id", "run_id", "step_key", "attempt", "workload_id", "spec_digest", "backend"},
			"run_resource_observations": {"tenant_id", "project_id", "run_id", "kind", "name", "identity", "owner_project_id", "lifetime", "handle_run_id"},
			"runs":                      {"tenant_id", "project_id", "id", "pipeline_id", "definition_snapshot", "inputs", "sources", "bindings", "cleanup_max_attempts"},
			"run_steps":                 {"tenant_id", "project_id", "run_id", "id", "step_key", "ordinal", "image", "step_kind"},
		} {
			for _, c := range never {
				if slices.Contains(grants[table], c) {
					t.Errorf("the runtime role can update %s.%s", table, c)
				}
			}
		}
		// What the runtime role does write on the existing tables.
		for table, cols := range map[string][]string{
			"runs":      {"lease_purpose", "cleanup_reason"},
			"run_steps": {"attempt", "when_evaluated", "when_result", "when_input_digest", "when_evaluated_at", "result_code", "result_reason", "wait_reason"},
		} {
			for _, c := range cols {
				if !slices.Contains(grants[table], c) {
					t.Errorf("the runtime role cannot update %s.%s", table, c)
				}
			}
		}
	})
}

// Test_unit_storage_run_state_guards: every regression the kernel refuses has a
// declared database guard (trigger, function body or constraint) that holds for
// every writer, including the owner. Text only: the guards are executed against
// PostgreSQL by Test_durable_run_state_postgres.
func Test_unit_storage_run_state_guards(t *testing.T) {
	stmts := durableStatements(t)
	function := func(name string) string {
		s := statementsMatching(stmts, `^CREATE (OR REPLACE )?FUNCTION `+name+`\(\)`)
		if len(s) != 1 {
			t.Fatalf("function %s is not defined exactly once", name)
		}
		return s[0]
	}
	contains := func(what, body string, needs ...string) {
		t.Helper()
		for _, need := range needs {
			if !strings.Contains(body, need) {
				t.Errorf("%s lacks %q", what, need)
			}
		}
	}

	t.Run("triggers_fire_before_every_update", func(t *testing.T) {
		for _, tr := range []string{
			"CREATE TRIGGER run_commands_guard BEFORE UPDATE ON run_commands FOR EACH ROW EXECUTE FUNCTION kodama_run_command_guard()",
			"CREATE TRIGGER run_workload_observations_guard BEFORE UPDATE ON run_workload_observations FOR EACH ROW EXECUTE FUNCTION kodama_run_workload_observation_guard()",
			"CREATE TRIGGER run_resource_observations_guard BEFORE UPDATE ON run_resource_observations FOR EACH ROW EXECUTE FUNCTION kodama_run_resource_observation_guard()",
		} {
			if !slices.Contains(stmts, tr) {
				t.Errorf("missing %q", tr)
			}
		}
		// The run-step guard keeps the trigger 004 created; only its function is replaced.
		if len(statementsMatching(stmts, `^CREATE TRIGGER run_steps_guard`)) != 0 {
			t.Error("the run_steps_guard trigger is created by 004 and must not be created again")
		}
		for _, s := range statementsMatching(stmts, `^CREATE TRIGGER `) {
			if strings.Contains(s, "DISABLE") || strings.Contains(s, "AFTER") || !strings.Contains(s, "FOR EACH ROW") {
				t.Errorf("a guard must be a BEFORE row trigger: %s", s)
			}
		}
	})

	t.Run("command_guard", func(t *testing.T) {
		contains("kodama_run_command_guard", function("kodama_run_command_guard"),
			"IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.command_id, NEW.step_key, NEW.kind, NEW.target, NEW.spec_digest, NEW.fence, NEW.binding_attempt, NEW.binding_backend, NEW.recorded_at) IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.command_id, OLD.step_key, OLD.kind, OLD.target, OLD.spec_digest, OLD.fence, OLD.binding_attempt, OLD.binding_backend, OLD.recorded_at) THEN",
			"IF NOT ((OLD.state = 'intent' AND NEW.state IN ('confirmed', 'rejected', 'uncertain')) OR (OLD.state = 'uncertain' AND NEW.state IN ('confirmed', 'rejected'))) THEN",
			"ELSIF NEW.reason IS DISTINCT FROM OLD.reason THEN",
			"IF OLD.result_code IS NOT NULL AND (NEW.result_code, NEW.result_exit_code, NEW.result_reason) IS DISTINCT FROM (OLD.result_code, OLD.result_exit_code, OLD.result_reason) THEN")
		// Confirmed and rejected are final: no transition leaves them.
		if strings.Contains(function("kodama_run_command_guard"), "OLD.state = 'confirmed'") || strings.Contains(function("kodama_run_command_guard"), "OLD.state = 'rejected'") {
			t.Error("confirmed and rejected commands are final")
		}
		sql := strings.Join(stmts, "\n")
		contains("command constraints", sql,
			"(state IN ('intent', 'confirmed') AND reason IS NULL) OR (state IN ('rejected', 'uncertain') AND reason IS NOT NULL)",
			"result_code IS NOT NULL AND state = 'confirmed' AND kind IN ('launch_workload', 'stop_workload', 'remove_workload')",
			"fence bigint NOT NULL CHECK (fence BETWEEN 1 AND 9007199254740991)")
	})

	t.Run("workload_observation_guard", func(t *testing.T) {
		contains("kodama_run_workload_observation_guard", function("kodama_run_workload_observation_guard"),
			"IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.step_key, NEW.attempt, NEW.workload_id, NEW.backend) IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.step_key, OLD.attempt, OLD.workload_id, OLD.backend) THEN",
			"IF NEW.spec_digest IS DISTINCT FROM OLD.spec_digest THEN",
			"IF NEW.fence < OLD.fence THEN", "IF NEW.observed_at < OLD.observed_at THEN",
			"IF OLD.phase = 'exited' AND NOT (NEW.phase = 'absent' OR (NEW.phase = 'exited' AND (NEW.result_code, NEW.result_exit_code, NEW.result_reason) IS NOT DISTINCT FROM (OLD.result_code, OLD.result_exit_code, OLD.result_reason))) THEN",
			"an exited workload keeps its result")
		contains("run_workload_observations_result_shape", strings.Join(stmts, "\n"),
			"phase = 'exited' AND result_code IS NOT NULL", "phase <> 'exited' AND num_nonnulls(result_code, result_exit_code, result_reason) = 0")
	})

	t.Run("resource_observation_guard", func(t *testing.T) {
		contains("kodama_run_resource_observation_guard", function("kodama_run_resource_observation_guard"),
			"IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.kind, NEW.name, NEW.identity, NEW.owner_project_id, NEW.lifetime, NEW.handle_run_id) IS DISTINCT FROM (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.kind, OLD.name, OLD.identity, OLD.owner_project_id, OLD.lifetime, OLD.handle_run_id) THEN",
			"IF NEW.fence < OLD.fence THEN", "IF NEW.observed_at < OLD.observed_at THEN")
		contains("resource constraints", strings.Join(stmts, "\n"),
			"(lifetime = 'tenant') = (owner_project_id IS NULL)", "owner_project_id IS NULL OR owner_project_id = project_id",
			"kind <> 'workspace' OR lifetime = 'pipeline'")
	})

	t.Run("step_guard", func(t *testing.T) {
		body := function("kodama_run_step_guard")
		contains("kodama_run_step_guard", body,
			// Carried over from 004.
			"workload identity is immutable once set",
			"OLD.wait_version IS NOT NULL AND (NEW.wait_version IS NULL OR NEW.wait_version < OLD.wait_version)",
			"a decided wait is immutable, including its version",
			// Wait identity is fixed from the moment it is recorded, content once it has a state.
			"OLD.wait_id IS NOT NULL AND NEW.wait_id IS DISTINCT FROM OLD.wait_id",
			"OLD.wait_state IS NOT NULL AND (NEW.wait_correlation, NEW.wait_request, NEW.wait_input, NEW.wait_outcomes, NEW.wait_approving, NEW.wait_on_expiry, NEW.wait_deadline_at)",
			// A wait opens, or is cancelled before it opened without values; a decision follows only waiting.
			"IF NOT coalesce((OLD.wait_state IS NULL AND NEW.wait_state = 'waiting') OR (OLD.wait_state IS NULL AND NEW.wait_state = 'cancelled' AND NEW.wait_deadline_at IS NULL) OR (OLD.wait_state = 'waiting' AND NEW.wait_state IN ('resolved', 'expired', 'cancelled')), false) THEN",
			// Every change to a recorded wait raises the version, including before it opens.
			"OLD.wait_id IS NOT NULL AND (NEW.wait_assignment, NEW.wait_state, NEW.wait_outcome, NEW.wait_answer, NEW.wait_actor_ref, NEW.wait_decided_at, NEW.wait_consumed_signal_id, NEW.wait_reason)",
			"NEW.wait_version <= OLD.wait_version",
			"NEW.wait_consumed_signal_id, NEW.wait_version, NEW.wait_reason)",
			// New: attempt, write-once predicate evidence, final step result.
			"IF NEW.attempt < OLD.attempt THEN",
			"IF OLD.when_evaluated AND (NEW.when_evaluated, NEW.when_result, NEW.when_input_digest, NEW.when_evaluated_at) IS DISTINCT FROM (OLD.when_evaluated, OLD.when_result, OLD.when_input_digest, OLD.when_evaluated_at) THEN",
			"the when evaluation is recorded once and never changes",
			"IF OLD.status IN ('succeeded', 'failed', 'skipped', 'cancelled') AND (NEW.attempt, NEW.exit_code, NEW.result_code, NEW.result_reason) IS DISTINCT FROM (OLD.attempt, OLD.exit_code, OLD.result_code, OLD.result_reason) THEN",
			"a settled step keeps its attempt and result")
		// A decided wait can no longer change a value: the version rule must not be satisfiable by clearing.
		if strings.Contains(body, "OLD.wait_id IS NOT NULL AND NEW.wait_version") {
			t.Error("the version comparison must handle NULL explicitly")
		}
	})

	t.Run("constraints_hold_for_every_writer", func(t *testing.T) {
		names := constraintNames(stmts)
		for _, c := range []string{
			"runs_lease_purpose_known", "runs_lease_purpose_pair", "runs_cleanup_max_attempts_range", "runs_cleanup_reason_shape",
			"run_steps_wait_unopened", "run_steps_wait_open", "run_steps_wait_cancelled", "run_steps_wait_reason_shape",
			"run_steps_attempt_shape", "run_steps_when_evidence", "run_steps_when_result_state", "run_steps_result_shape",
			"run_steps_container_no_wait",
		} {
			if !slices.Contains(names, c) {
				t.Errorf("constraint %s is missing", c)
			}
		}
		sql := strings.Join(stmts, "\n")
		contains("step constraints", sql,
			// Evaluated false and unevaluated are different records.
			"(NOT when_evaluated AND num_nonnulls(when_result, when_input_digest, when_evaluated_at) = 0)",
			"when_evaluated AND when_result IS NOT NULL AND when_evaluated_at IS NOT NULL AND when_input_digest IS NOT NULL AND length(when_input_digest) BETWEEN 1 AND 128",
			"NOT when_evaluated OR when_result OR status IN ('pending', 'cancelled') OR (status = 'skipped' AND reason IS NOT DISTINCT FROM 'condition_false')",
			// A wait never carries a workload result.
			"step_kind = 'container' AND attempt >= 1",
			"attempt >= 0 AND (attempt = 0 OR step_kind = 'container') AND (status <> 'pending' OR attempt = 0)",
			// A cancelled-before-opening wait keeps the version and identity requirement.
			"num_nonnulls(wait_id, wait_version) = 2",
			"wait_state IS NOT DISTINCT FROM 'cancelled' OR wait_reason IS NULL")
		// An opened wait keeps its rule: correlation, expected input and deadline are all required
		// unless the wait is cancelled with none of the opened content.
		open := statementsMatching(stmts, `ADD CONSTRAINT run_steps_wait_open `)
		if len(open) != 1 || strings.Count(open[0], "wait_state = 'cancelled'") != 1 {
			t.Errorf("the cancelled exception must appear once in run_steps_wait_open")
		}
	})
}
