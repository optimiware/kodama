//go:build component

package postgrescomponent

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/request"
	storepg "github.com/optimiware/kodema/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/task"
	pgtest "github.com/optimiware/kodema/test/support/infrastructure/postgres"
)

const reviewComponentPipelineYAML = `inputs: [repository]
actions:
  - name: review
    request: role/engineer
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: refs/heads/main
      candidate: sha-candidate-1
    outputs: [feedback]
`

// TestRequestResolveReviseAppendsTaskMessageAndReplaysIdempotentlyOnPostgreSQL
// proves that resolving a task-bound request action with decision="revise"
// satisfies actions_request_decision, locks tasks[taskID] before executions in
// lockOrder(), atomically inserts one task_messages row under forced RLS, and
// neither duplicates task_messages on idempotent replay nor mutates state on a
// conflicting retry.
func TestRequestResolveReviseAppendsTaskMessageAndReplaysIdempotentlyOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	now := time.Now().UTC().Truncate(time.Microsecond)

	pgStore := storepg.New(env.runtime)
	execStore := executionstore.New(pgStore, pgStore)
	refHeads, err := readers.NewRefHeadReader(pgStore)
	if err != nil {
		t.Fatalf("readers.NewRefHeadReader: %v", err)
	}
	requestService, err := request.NewService(request.Config{
		Clock:       integrationClock{now: now},
		IDs:         env.ids,
		Authorizer:  allowAuthorizer{},
		Authority:   confirmAuthority{},
		State:       execStore,
		Activations: execStore,
		Resolutions: execStore,
		RefHeads:    refHeads,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}

	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor:     actor,
		Scope:     env.scope,
		Name:      "review-pipe",
		YAML:      []byte(reviewComponentPipelineYAML),
		CallerKey: "apply-review-pipe",
		Content:   integrationContent("apply-review-pipe"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v%s", err, pgtest.CauseText(err))
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor:      actor,
		Scope:      env.scope,
		Title:      "PR needing revision",
		PipelineID: applied.PipelineID,
		Inputs:     map[id.InputKey]string{"repository": "shop/web"},
		Start:      true,
		CallerKey:  "create-revise-task",
		Content:    integrationContent("create-revise-task"),
	})
	if err != nil {
		t.Fatalf("task create_and_start: %v%s", err, pgtest.CauseText(err))
	}

	eng := executionstore.NewEngine(adapterController(t, env.shard))
	pre, err := env.exec.Load(ctx, env.scope, created.ExecutionID)
	if err != nil {
		t.Fatalf("load execution: %v%s", err, pgtest.CauseText(err))
	}
	_, actCommit, err := flow.Reduce(pre, flow.ActivateFact{
		ExecutionID: pre.ID,
		ActionID:    pre.Actions[0].ID,
		At:          now,
	})
	if err != nil {
		t.Fatalf("reduce activate: %v", err)
	}
	if _, err := eng.Commit(ctx, *actCommit); err != nil {
		t.Fatalf("commit activate: %v%s", err, pgtest.CauseText(err))
	}
	active, err := env.exec.Load(ctx, env.scope, created.ExecutionID)
	if err != nil {
		t.Fatalf("reload execution: %v%s", err, pgtest.CauseText(err))
	}

	resolveCmd := request.ResolveCommand{
		Actor:                    actor,
		Scope:                    env.scope,
		ExecutionID:              created.ExecutionID,
		ActionID:                 active.Actions[0].ID,
		ExpectedExecutionVersion: active.Version,
		ExpectedActionVersion:    active.Actions[0].Version,
		Decision:                 request.DecisionRevise,
		Note:                     "Please split the handler and add nil checks",
		Response:                 kernel.Outputs{"feedback": "split handler"},
	}
	first, err := requestService.Resolve(ctx, resolveCmd)
	if err != nil {
		t.Fatalf("resolve revise: %v%s", err, pgtest.CauseText(err))
	}
	if first.Decision != request.DecisionRevise || first.Replayed {
		t.Fatalf("first resolve = %+v; want non-replayed revise", first)
	}

	actionRow := scopedRow(t, ctx, env.runtime, env.scope, 3,
		`SELECT state, request_decision, (outputs IS NULL)::text FROM actions WHERE id = $1`,
		string(active.Actions[0].ID))
	if actionRow[0] != "failed" || actionRow[1] != "revise" || actionRow[2] != "true" {
		t.Fatalf("action row = %v; want failed/revise/null-outputs", actionRow)
	}
	execRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT state, failure->>'reason' FROM executions WHERE id = $1`,
		string(created.ExecutionID))
	if execRow[0] != "failed" || execRow[1] != kernel.FailureReasonNeedsRevision {
		t.Fatalf("execution row = %v; want failed/needs_revision", execRow)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM task_messages WHERE task_id = $1`, 1, "revise task_messages", string(created.TaskID))
	msgRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT author_id, body FROM task_messages WHERE task_id = $1`,
		string(created.TaskID))
	if msgRow[0] != string(actor.ID) || msgRow[1] != "Please split the handler and add nil checks" {
		t.Fatalf("task_messages row = %v; want author=%q feedback", msgRow, actor.ID)
	}
	gotTask, err := env.reader.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read task after revise: %v%s", err, pgtest.CauseText(err))
	}
	if gotTask.State != task.StateOpen || len(gotTask.Messages) != 1 {
		t.Fatalf("task after revise = state:%q messages:%d; want open/1", gotTask.State, len(gotTask.Messages))
	}

	// Idempotent replay of the identical revise request returns the original
	// result and does NOT append a duplicate task_messages row.
	replayed, err := requestService.Resolve(ctx, resolveCmd)
	if err != nil {
		t.Fatalf("resolve revise replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "resolve revise replay", first, replayed)
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM task_messages WHERE task_id = $1`, 1, "revise task_messages after replay", string(created.TaskID))

	// Conflicting retry with changed feedback note is refused and leaves task_messages untouched.
	conflictCmd := resolveCmd
	conflictCmd.Note = "Changed revision feedback"
	_, err = requestService.Resolve(ctx, conflictCmd)
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("conflicting revise retry err = %v; want changed_idempotency_content", err)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM task_messages WHERE task_id = $1`, 1, "revise task_messages after conflict", string(created.TaskID))
}

// TestTaskParentIDConstraintsAndRefusalsOnPostgreSQL proves cross-project,
// nonexistent, and self parent_id links are refused both through the real task
// service/adapter and directly by PostgreSQL's tasks_parent composite FK and
// tasks_no_self_parent CHECK constraint.
func TestTaskParentIDConstraintsAndRefusalsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()

	// Seed a second project and a foreign parent task inside it.
	env.shard.SeedProject(ctx, "tenant-1", "project-2")
	if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-foreign-proj', 'tenant-1', 'project-2', 1, 1, 'open', 'principal-1', 'Foreign project parent', $1)`,
		pgtest.FixtureTime); err != nil {
		t.Fatalf("seed foreign project task: %v", err)
	}

	// Valid parent/child creation within the same project succeeds.
	parent, err := env.task.Create(ctx, task.CreateCommand{
		Actor:     actor,
		Scope:     env.scope,
		Title:     "In-scope parent",
		CallerKey: "create-parent-ok",
		Content:   integrationContent("create-parent-ok"),
	})
	if err != nil {
		t.Fatalf("create parent: %v%s", err, pgtest.CauseText(err))
	}
	child, err := env.task.Create(ctx, task.CreateCommand{
		Actor:     actor,
		Scope:     env.scope,
		ParentID:  parent.TaskID,
		Title:     "In-scope child",
		CallerKey: "create-child-ok",
		Content:   integrationContent("create-child-ok"),
	})
	if err != nil {
		t.Fatalf("create child: %v%s", err, pgtest.CauseText(err))
	}
	childRow := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT parent_id FROM tasks WHERE id = $1`, string(child.TaskID))
	if childRow[0] != string(parent.TaskID) {
		t.Fatalf("child parent_id = %q; want %q", childRow[0], parent.TaskID)
	}

	// Nonexistent parent_id is refused through the task service.
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor:     actor,
		Scope:     env.scope,
		ParentID:  "task-does-not-exist",
		Title:     "Orphan child",
		CallerKey: "create-orphan",
		Content:   integrationContent("create-orphan"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("nonexistent parent_id err = %v; want not_found", err)
	}

	// Cross-project parent_id is refused through the task service.
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor:     actor,
		Scope:     env.scope,
		ParentID:  "task-foreign-proj",
		Title:     "Cross-project child",
		CallerKey: "create-cross-project",
		Content:   integrationContent("create-cross-project"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("cross-project parent_id err = %v; want not_found", err)
	}

	// Self-parent is refused at the adapter layer with invalid_input.
	_, err = env.reader.Create(ctx, task.CreateUnit{
		Owner:     env.scope,
		TaskID:    "task-self-unit",
		ParentID:  "task-self-unit",
		Title:     "Self parent",
		Initiator: actor.ID,
		At:        pgtest.FixtureTime,
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("self-parent CreateUnit err = %v; want invalid_input", err)
	}

	// Direct PostgreSQL constraint proofs: tasks_parent composite FK rejects
	// nonexistent and cross-project parents (SQLSTATE 23503), and
	// tasks_no_self_parent CHECK rejects self-parenting (SQLSTATE 23514).
	_, err = pgtest.ExecSQL(ctx, env.shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, parent_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-fk-missing', 'tenant-1', 'project-1', 'task-missing-fk', 1, 1, 'open', 'principal-1', 'Missing FK', $1)`,
		pgtest.FixtureTime)
	pgtest.RequireSQLState(t, err, "23503", "tasks_parent FK rejects nonexistent parent_id")

	_, err = pgtest.ExecSQL(ctx, env.shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, parent_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-fk-cross', 'tenant-1', 'project-1', 'task-foreign-proj', 1, 1, 'open', 'principal-1', 'Cross-project FK', $1)`,
		pgtest.FixtureTime)
	pgtest.RequireSQLState(t, err, "23503", "tasks_parent composite FK rejects cross-project parent_id")

	_, err = pgtest.ExecSQL(ctx, env.shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, parent_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-chk-self', 'tenant-1', 'project-1', 'task-chk-self', 1, 1, 'open', 'principal-1', 'Self parent CHECK', $1)`,
		pgtest.FixtureTime)
	pgtest.RequireSQLState(t, err, "23514", "tasks_no_self_parent CHECK rejects parent_id = id")
}

// TestMigration0007IncrementalUpgradeFromBaselineOnPostgreSQL proves an
// existing database migrated through frozen baseline 0001..0006 with live rows
// upgrades cleanly when 0007_task_review_lifecycle.sql is applied incrementally.
func TestMigration0007IncrementalUpgradeFromBaselineOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)
	scope := pgtest.Scope("tenant-1", "project-1")

	// 1. Apply frozen baseline 0001..0006 only.
	baseline, err := runner.Apply(ctx, 6)
	if err != nil {
		t.Fatalf("apply baseline 0001..0006: %v", err)
	}
	if len(baseline) != 6 || baseline[5].Name != "0006_privileges.sql" {
		t.Fatalf("baseline applied = %+v; want 6 migrations ending at 0006_privileges.sql", baseline)
	}

	// 2. Seed pre-0007 rows on the baseline schema.
	shard.SeedScope(ctx, "tenant-1", "project-1")
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-pre-0007', 'tenant-1', 'project-1', 1, 1, 'open', 'principal-1', 'Pre-0007 task', $1)`,
		pgtest.FixtureTime); err != nil {
		t.Fatalf("seed pre-0007 task: %v", err)
	}
	_, actionID := seedRunningRequest(ctx, t, shard, scope)

	// Confirm the baseline actions_request_decision constraint rejects 'revise' before 0007.
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET request_decision = 'revise', request_actor = 'principal-1', request_resolved_at = now() WHERE id = $1`,
		string(actionID))
	pgtest.RequireSQLState(t, err, "23514", "pre-0007 actions_request_decision rejects revise")

	// 3. Apply 0007 incrementally on top of the existing 0001..0006 database.
	upgraded, err := runner.Apply(ctx, 0)
	if err != nil {
		t.Fatalf("apply incremental 0007: %v", err)
	}
	if len(upgraded) != 1 || upgraded[0].Name != "0007_task_review_lifecycle.sql" {
		t.Fatalf("upgraded = %+v; want only 0007_task_review_lifecycle.sql", upgraded)
	}
	again, err := runner.Apply(ctx, 0)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-run after 0007 = %d (err=%v); want 0", len(again), err)
	}

	// 4. Verify existing rows remain intact and new 0007 capabilities work under RLS.
	runtime := shard.Runtime(2)
	t.Cleanup(runtime.Close)
	preRow := scopedRow(t, ctx, runtime, scope, 1,
		`SELECT (parent_id IS NULL)::text FROM tasks WHERE id = 'task-pre-0007'`)
	if preRow[0] != "true" {
		t.Fatalf("pre-0007 task parent_id IS NULL = %q; want true", preRow[0])
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, parent_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-post-0007', 'tenant-1', 'project-1', 'task-pre-0007', 1, 1, 'open', 'principal-1', 'Post-0007 child', $1)`,
		pgtest.FixtureTime); err != nil {
		t.Fatalf("insert post-0007 child task: %v", err)
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE actions SET request_decision = 'revise', request_actor = 'principal-1', request_resolved_at = now() WHERE id = $1`,
		string(actionID)); err != nil {
		t.Fatalf("post-0007 revise decision update: %v", err)
	}
	postAction := scopedRow(t, ctx, runtime, scope, 1,
		`SELECT request_decision FROM actions WHERE id = $1`, string(actionID))
	if postAction[0] != "revise" {
		t.Fatalf("post-0007 request_decision = %q; want revise", postAction[0])
	}
}
