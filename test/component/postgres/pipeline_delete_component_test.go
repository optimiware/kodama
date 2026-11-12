//go:build component

package postgrescomponent

// Deleting a pipeline with history on real PostgreSQL.
//
// Execution and task rows keep their pipeline_id as evidence and hold no
// reference to the pipeline row, so a pipeline that executions and tasks name
// can be deleted. The history rows survive unchanged and the deleted pipeline
// admits nothing new.

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// fullRow reads one entire row as jsonb text, so any changed column value after
// the pipeline deletion is attributable.
func fullRow(ctx context.Context, t *testing.T, shard *pgtest.Shard, table, id string) string {
	t.Helper()
	row, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT to_jsonb(r)::text FROM `+table+` r WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("read %s row %s: %v", table, id, err)
	}
	return row
}

// rowPipelineID reads the durable pipeline identity of one history row.
func rowPipelineID(ctx context.Context, t *testing.T, shard *pgtest.Shard, table, id string) string {
	t.Helper()
	value, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT COALESCE(pipeline_id, '<null>') FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("read %s.pipeline_id of %s: %v", table, id, err)
	}
	return value
}

// deleteRowCount counts the rows of one table.
func deleteRowCount(ctx context.Context, t *testing.T, shard *pgtest.Shard, table string) int64 {
	t.Helper()
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*)::text FROM `+table)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// deleteCommittedRows counts every row family a refused task operation could
// leave behind.
func deleteCommittedRows(ctx context.Context, t *testing.T, shard *pgtest.Shard) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64)
	for _, table := range []string{"tasks", "task_messages", "executions", "actions", "receipts", "audit", "events"} {
		counts[table] = deleteRowCount(ctx, t, shard, table)
	}
	return counts
}

func requireRowsUnchanged(ctx context.Context, t *testing.T, shard *pgtest.Shard, before map[string]int64, label string) {
	t.Helper()
	after := deleteCommittedRows(ctx, t, shard)
	for table, want := range before {
		if after[table] != want {
			t.Fatalf("%s changed %s rows: before %d, after %d", label, table, want, after[table])
		}
	}
}

// seedPipelineDeleteScope seeds the scope and membership the real services run in.
func seedPipelineDeleteScope(ctx context.Context, shard *pgtest.Shard) {
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, pgtest.Scope("tenant-1", "project-1"), "membership-1", "principal-1", 1,
		string(tenancy.RoleEngineer), string(tenancy.RoleAdmin))
}

// pipelineHistory is one applied pipeline with a queued execution and an open task
// that both name it, committed through the real services.
type pipelineHistory struct {
	pipelineID  pipeline.PipelineID
	started     task.CreateResult
	selected    task.CreateResult
	selectedCmd task.CreateCommand
}

func seedPipelineHistory(ctx context.Context, t *testing.T, env *serviceEnv) pipelineHistory {
	t.Helper()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: env.actor(), Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v%s", err, pgtest.CauseText(err))
	}
	started, err := env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Started", PipelineID: applied.PipelineID, Start: true,
		CallerKey: "task-create-and-start", Content: integrationContent("task-create-and-start"),
	})
	if err != nil {
		t.Fatalf("task create-and-start: %v%s", err, pgtest.CauseText(err))
	}
	selectedCmd := task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Selected", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	}
	selected, err := env.task.Create(ctx, selectedCmd)
	if err != nil {
		t.Fatalf("plain task create: %v%s", err, pgtest.CauseText(err))
	}
	return pipelineHistory{pipelineID: applied.PipelineID, started: started, selected: selected, selectedCmd: selectedCmd}
}

// deletePipelineWithHistory deletes the applied pipeline under its recorded version
// through the real management service.
func deletePipelineWithHistory(ctx context.Context, env *serviceEnv, pipelineID pipeline.PipelineID) (pipeline.ChangeResult, error) {
	return env.management.Delete(ctx, pipeline.DeleteCommand{
		Actor: env.actor(), Scope: env.scope, PipelineID: pipelineID, ExpectedVersion: 1,
		CallerKey: "pipeline-delete", Content: integrationContent("pipeline-delete"),
	})
}

// requireHistorySurvives proves the delete removed only the pipeline row:
// the task and execution rows are byte-identical and still name the pipeline.
func requireHistorySurvives(ctx context.Context, t *testing.T, env *serviceEnv, history pipelineHistory, before map[string]string) {
	t.Helper()
	count, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*)::text FROM pipelines WHERE id = $1`, string(history.pipelineID))
	if err != nil || count != 0 {
		t.Fatalf("pipeline rows after delete = %d, err=%v; want 0", count, err)
	}
	for key, want := range before {
		table, identity, _ := strings.Cut(key, "/")
		if got := fullRow(ctx, t, env.shard, table, identity); got != want {
			t.Fatalf("%s row changed across the pipeline deletion:\nbefore %s\nafter  %s", key, want, got)
		}
		if got := rowPipelineID(ctx, t, env.shard, table, identity); got != string(history.pipelineID) {
			t.Fatalf("%s durable pipeline identity = %q; want the deleted pipeline id %q", key, got, history.pipelineID)
		}
	}
}

func pipelineHistoryRows(ctx context.Context, t *testing.T, env *serviceEnv, history pipelineHistory) map[string]string {
	t.Helper()
	rows := make(map[string]string)
	for _, key := range []string{
		"tasks/" + string(history.started.TaskID),
		"tasks/" + string(history.selected.TaskID),
		"executions/" + string(history.started.ExecutionID),
	} {
		table, identity, _ := strings.Cut(key, "/")
		rows[key] = fullRow(ctx, t, env.shard, table, identity)
	}
	return rows
}

// requireDeletedPipelineAdmitsNothing proves the admitted execution still
// loads from its own snapshot and that every new admission from the deleted
// pipeline is refused not_found with nothing written.
func requireDeletedPipelineAdmitsNothing(ctx context.Context, t *testing.T, env *serviceEnv, history pipelineHistory) {
	t.Helper()
	aggregate, err := env.exec.Load(ctx, env.scope, history.started.ExecutionID)
	if err != nil {
		t.Fatalf("load the admitted execution after the delete: %v%s", err, pgtest.CauseText(err))
	}
	if aggregate.State != kernel.ExecutionQueued || len(aggregate.Actions) != 1 {
		t.Fatalf("admitted aggregate = state %q with %d actions; want queued with its one action", aggregate.State, len(aggregate.Actions))
	}
	view, err := env.exec.Inspect(ctx, env.scope, history.started.ExecutionID)
	if err != nil {
		t.Fatalf("inspect the admitted execution after the delete: %v%s", err, pgtest.CauseText(err))
	}
	if view.PipelineID != history.pipelineID {
		t.Fatalf("inspected pipeline id = %q; want %q", view.PipelineID, history.pipelineID)
	}
	read, err := env.reader.Get(ctx, env.scope, history.selected.TaskID)
	if err != nil {
		t.Fatalf("read the task after the delete: %v%s", err, pgtest.CauseText(err))
	}
	if read.SelectedPipeline != history.pipelineID {
		t.Fatalf("read task selected pipeline = %q; want %q", read.SelectedPipeline, history.pipelineID)
	}

	before := deleteCommittedRows(ctx, t, env.shard)
	_, err = env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: history.selected.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-deleted", Content: integrationContent("task-start-deleted"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("start against the deleted pipeline error = %v; want not_found", err)
	}
	requireRowsUnchanged(ctx, t, env.shard, before, "a refused start")
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Fresh", PipelineID: history.pipelineID,
		CallerKey: "task-create-deleted", Content: integrationContent("task-create-deleted"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("plain create selecting the deleted pipeline error = %v; want not_found", err)
	}
	requireRowsUnchanged(ctx, t, env.shard, before, "a refused plain create")

	// The lost-response retry of the create that committed before the delete
	// still converges on its receipt.
	saved := env.ids.snapshot()
	replayed, err := env.task.Create(ctx, history.selectedCmd)
	env.ids.restore(saved)
	if err != nil {
		t.Fatalf("retried create after the pipeline delete: %v%s", err, pgtest.CauseText(err))
	}
	if !replayed.Replayed || replayed.TaskID != history.selected.TaskID {
		t.Fatalf("retried create = %+v; want the replayed original task %q", replayed, history.selected.TaskID)
	}
	requireRowsUnchanged(ctx, t, env.shard, before, "a replayed create")
}

// TestDeletingAPipelineKeepsItsHistory proves no foreign key references
// pipelines, a pipeline with history is deletable, its task and execution rows
// survive unchanged, and the deleted pipeline admits nothing new.
func TestDeletingAPipelineKeepsItsHistory(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(ctx, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	catalog := foreignKeyCatalog(ctx, t, shard)
	for _, name := range []string{"executions_pipeline", "tasks_pipeline"} {
		if _, ok := catalog[name]; ok {
			t.Fatalf("the schema has the %s reference", name)
		}
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f' AND confrelid = 'pipelines'::regclass`,
		0, "foreign keys referencing pipelines")

	seedPipelineDeleteScope(ctx, shard)
	env := newServiceEnvOnShard(t, shard, nil)
	history := seedPipelineHistory(ctx, t, env)
	rowsBefore := pipelineHistoryRows(ctx, t, env, history)
	deleted, err := deletePipelineWithHistory(ctx, env, history.pipelineID)
	if err != nil {
		t.Fatalf("pipeline.delete: %v%s", err, pgtest.CauseText(err))
	}
	if deleted.Version != 2 || deleted.Replayed {
		t.Fatalf("delete result = %+v; want version 2, not replayed", deleted)
	}
	requireHistorySurvives(ctx, t, env, history, rowsBefore)
	requireDeletedPipelineAdmitsNothing(ctx, t, env, history)
}

// TestStartRacingPipelineDeleteWritesNothingOnPostgreSQL commits a pipeline
// delete after the start's admission read and before its commit transaction.
// The start's guarded pipeline lock finds no row, so the unit is refused whole.
func TestStartRacingPipelineDeleteWritesNothingOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	armed := false
	var env *serviceEnv
	var before map[string]int64
	env = newServiceEnvWithHook(t, func() {
		if !armed {
			return
		}
		armed = false
		if rows, err := pgtest.ExecSQL(ctx, env.shard.Admin(), `DELETE FROM pipelines WHERE id = 'pipeline-1'`); err != nil || rows != 1 {
			t.Fatalf("delete pipeline after admission affected %d rows, err=%v; want 1", rows, err)
		}
		before = deleteCommittedRows(ctx, t, env.shard)
	})
	created := applyPipelineAndCreateTask(t, env)
	armed = true
	_, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-racing-delete", Content: integrationContent("task-start-racing-delete"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("start with the pipeline deleted after admission error = %v; want version_conflict", err)
	}
	if before == nil {
		t.Fatal("the start never reached its commit unit")
	}
	requireRowsUnchanged(ctx, t, env.shard, before, "a start that lost the race with the delete")
	requireNoExecutionRows(t, ctx, env)
	row, err := pgtest.QueryOne(ctx, env.shard.Admin(), 2,
		`SELECT lifecycle_version::text, COALESCE(current_execution_id, '<null>') FROM tasks WHERE id = $1`, string(created))
	if err != nil || row[0] != "1" || row[1] != "<null>" {
		t.Fatalf("task after the refused start = %v, err=%v; want lifecycle 1 and no current execution", row, err)
	}
}

// TestCreateRacingPipelineDeleteWritesNothingOnPostgreSQL commits a pipeline
// delete after the plain create's scoped pipeline read and before its commit
// transaction. The creation unit pins the selected row, so it is refused whole.
func TestCreateRacingPipelineDeleteWritesNothingOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	armed := false
	var env *serviceEnv
	var before map[string]int64
	env = newServiceEnvWithHook(t, func() {
		if !armed {
			return
		}
		armed = false
		if rows, err := pgtest.ExecSQL(ctx, env.shard.Admin(), `DELETE FROM pipelines WHERE id = 'pipeline-1'`); err != nil || rows != 1 {
			t.Fatalf("delete pipeline after the scoped read affected %d rows, err=%v; want 1", rows, err)
		}
		before = deleteCommittedRows(ctx, t, env.shard)
	})
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: env.actor(), Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v%s", err, pgtest.CauseText(err))
	}
	armed = true
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Racing", PipelineID: applied.PipelineID,
		CallerKey: "task-create-racing-delete", Content: integrationContent("task-create-racing-delete"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("create with the pipeline deleted after the scoped read error = %v; want version_conflict", err)
	}
	if before == nil {
		t.Fatal("the create never reached its commit unit")
	}
	requireRowsUnchanged(ctx, t, env.shard, before, "a create that lost the race with the delete")
	if count := deleteRowCount(ctx, t, env.shard, "tasks"); count != 0 {
		t.Fatalf("tasks after the refused create = %d; want 0", count)
	}
}
