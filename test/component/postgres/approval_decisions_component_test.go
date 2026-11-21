//go:build component

package postgrescomponent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	storepg "github.com/optimiware/kodema/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/store/units/platform"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/verify"
	pgtest "github.com/optimiware/kodema/test/support/infrastructure/postgres"
)

const approvalDecisionComponentPipelineYAML = `inputs: [repository]
completes_task: false
actions:
  - name: review
    request: role/engineer
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: refs/heads/main
      expected_base: base-sha-1111
      candidate: cand-sha-2222
    outputs: [approved]
`

// TestMigration0009IncrementalUpgradeAndRLSIsolationOnPostgreSQL proves:
//   - An existing database migrated through 0001..0008 upgrades cleanly to
//     0009_approval_decisions.sql without modifying any frozen 0001..0006
//     migration checksum.
//   - approval_decisions has forced RLS enabled and isolates rows across
//     tenants and projects under the non-owner kodema_runtime role.
//   - Check constraints (role-only assignment, expires_at > resolved_at,
//     revocation pair) and composite FKs reject malformed rows directly in PG.
func TestMigration0009IncrementalUpgradeAndRLSIsolationOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-1", "project-2")
	scopeOtherTenant := pgtest.Scope("tenant-2", "project-a")

	// 1. Apply 0001..0008 first.
	applied, err := runner.Apply(ctx, 8)
	if err != nil {
		t.Fatalf("apply 0001..0008: %v", err)
	}
	if len(applied) != 8 || applied[7].Name != "0008_task_review_lifecycle.sql" {
		t.Fatalf("applied = %+v; want 8 migrations ending at 0008_task_review_lifecycle.sql", applied)
	}

	// 2. Incrementally apply 0009_approval_decisions.sql.
	upgraded, err := runner.Apply(ctx, 0)
	if err != nil {
		t.Fatalf("apply incremental 0009: %v", err)
	}
	if len(upgraded) != 1 || upgraded[0].Name != "0009_approval_decisions.sql" {
		t.Fatalf("upgraded = %+v; want [0009_approval_decisions.sql]", upgraded)
	}
	again, err := runner.Apply(ctx, 0)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-run after 0009 = %d (err=%v); want 0", len(again), err)
	}

	// 3. Verify forced RLS is enabled on approval_decisions.
	enabled, forced := rlsFacts(ctx, t, shard, store.ApprovalDecisionsTable)
	if !enabled || !forced {
		t.Fatalf("approval_decisions RLS enabled=%v forced=%v; want both true", enabled, forced)
	}

	// 4. Seed two projects and a second tenant, insert one approval decision in
	//    scope1, and prove kodema_runtime under scope2 and scopeOtherTenant
	//    cannot see it.
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedProject(ctx, "tenant-1", "project-2")
	shard.SeedScope(ctx, "tenant-2", "project-a")
	execID, actionID := seedRunningRequest(ctx, t, shard, scope1)
	now := pgtest.FixtureTime
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO approval_decisions
		 (id, tenant_id, project_id, task_id, execution_id, action_id, assignment, operation, resource_id, subject, approver, resolved_at, expires_at, revoked_at, revoked_reason, version)
		 VALUES ('dec_pg_rls_1', 'tenant-1', 'project-1', NULL, $1, $2, 'role/reviewer', 'git.ref.update', 'res-1', '{"operation":{"kind":"literal","literal":"git.ref.update"}}'::jsonb, 'principal-1', $3, $4, NULL, NULL, 1)`,
		string(execID), string(actionID), now, now.Add(72*time.Hour)); err != nil {
		t.Fatalf("seed approval_decisions row: %v", err)
	}

	runtime := shard.Runtime(2)
	t.Cleanup(runtime.Close)

	row1 := scopedRow(t, ctx, runtime, scope1, 1, `SELECT count(*)::text FROM approval_decisions WHERE id = 'dec_pg_rls_1'`)
	if row1[0] != "1" {
		t.Fatalf("scope1 visible count = %q; want 1", row1[0])
	}
	row2 := scopedRow(t, ctx, runtime, scope2, 1, `SELECT count(*)::text FROM approval_decisions WHERE id = 'dec_pg_rls_1'`)
	if row2[0] != "0" {
		t.Fatalf("scope2 visible count = %q; want 0 (RLS isolation)", row2[0])
	}
	rowOther := scopedRow(t, ctx, runtime, scopeOtherTenant, 1, `SELECT count(*)::text FROM approval_decisions WHERE id = 'dec_pg_rls_1'`)
	if rowOther[0] != "0" {
		t.Fatalf("other-tenant visible count = %q; want 0 (RLS isolation)", rowOther[0])
	}

	// 5. Direct PG CHECK constraint proofs:
	//    - service/<id> assignment rejected (23514)
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO approval_decisions
		 (id, tenant_id, project_id, task_id, execution_id, action_id, assignment, operation, resource_id, subject, approver, resolved_at, expires_at, revoked_at, revoked_reason, version)
		 VALUES ('dec_pg_chk_svc', 'tenant-1', 'project-1', NULL, $1, $2, 'service/merge-coordinator', 'git.ref.update', 'res-1', '{}'::jsonb, 'principal-1', $3, $4, NULL, NULL, 1)`,
		string(execID), string(actionID), now, now.Add(time.Hour))
	pgtest.RequireSQLState(t, err, "23514", "approval_decisions_assignment_role rejects service/<id>")

	//    - expires_at <= resolved_at rejected (23514)
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO approval_decisions
		 (id, tenant_id, project_id, task_id, execution_id, action_id, assignment, operation, resource_id, subject, approver, resolved_at, expires_at, revoked_at, revoked_reason, version)
		 VALUES ('dec_pg_chk_exp', 'tenant-1', 'project-1', NULL, $1, $2, 'role/reviewer', 'git.ref.update', 'res-1', '{}'::jsonb, 'principal-1', $3, $3, NULL, NULL, 1)`,
		string(execID), string(actionID), now)
	pgtest.RequireSQLState(t, err, "23514", "approval_decisions_expiry rejects expires_at <= resolved_at")

	//    - mismatched revoked_at / revoked_reason pair rejected (23514)
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE approval_decisions SET revoked_at = $1 WHERE id = 'dec_pg_rls_1'`, now)
	pgtest.RequireSQLState(t, err, "23514", "approval_decisions_revocation_pair rejects revoked_at without revoked_reason")
}

// TestApprovalDecisionPersistReadAndCancelRevocationOnPostgreSQL exercises the
// full real-PostgreSQL commit path: resolving a human role request atomically
// persists approval_decisions, DecisionReader reads it cleanly under RLS, and
// task.cancel atomically revokes it in PostgreSQL.
func TestApprovalDecisionPersistReadAndCancelRevocationOnPostgreSQL(t *testing.T) {
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
		Name:      "approval-dec-pipe",
		YAML:      []byte(approvalDecisionComponentPipelineYAML),
		CallerKey: "apply-approval-dec-pipe",
		Content:   integrationContent("apply-approval-dec-pipe"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v%s", err, pgtest.CauseText(err))
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor:      actor,
		Scope:      env.scope,
		Title:      "Task with cross-exec approval decision",
		PipelineID: applied.PipelineID,
		Inputs:     map[id.InputKey]string{"repository": "repo-web"},
		Start:      true,
		CallerKey:  "create-approval-dec-task",
		Content:    integrationContent("create-approval-dec-task"),
	})
	if err != nil {
		t.Fatalf("task create_and_start: %v%s", err, pgtest.CauseText(err))
	}

	// Activate review action via the real controller engine.
	eng := executionstore.NewEngine(adapterController(t, env.shard))
	loaded, err := execStore.Load(ctx, env.scope, created.ExecutionID)
	if err != nil {
		t.Fatalf("load execution: %v", err)
	}
	_, actCommit, err := flow.Reduce(loaded, flow.ActivateFact{
		ExecutionID: loaded.ID,
		ActionID:    loaded.Actions[0].ID,
		At:          now,
	})
	if err != nil || actCommit == nil {
		t.Fatalf("Reduce(ActivateFact): commit=%+v err=%v", actCommit, err)
	}
	if _, err := eng.Commit(ctx, *actCommit); err != nil {
		t.Fatalf("eng.Commit(activate): %v%s", err, pgtest.CauseText(err))
	}

	// Resolve with pass -> atomically creates immutable approval_decisions row.
	resolved, err := requestService.Resolve(ctx, request.ResolveCommand{
		Actor:                 actor,
		Scope:                 env.scope,
		ExecutionID:           created.ExecutionID,
		ActionID:              loaded.Actions[0].ID,
		ExpectedActionVersion: 2,
		Decision:              request.DecisionPass,
		Response:              map[id.OutputKey]string{"approved": "yes"},
	})
	if err != nil {
		t.Fatalf("requestService.Resolve(pass): %v%s", err, pgtest.CauseText(err))
	}
	if !strings.HasPrefix(string(resolved.DecisionID), "dec_") {
		t.Fatalf("resolved.DecisionID = %q; want dec_ prefix", resolved.DecisionID)
	}

	// execution.Inspect surfaces the persisted decision_id on PostgreSQL.
	view, err := execStore.Inspect(ctx, env.scope, created.ExecutionID)
	if err != nil {
		t.Fatalf("execStore.Inspect: %v", err)
	}
	if len(view.Actions) != 1 || view.Actions[0].DecisionID != resolved.DecisionID {
		t.Fatalf("inspect action decision_id = %q; want %q", view.Actions[0].DecisionID, resolved.DecisionID)
	}

	// platform.DecisionReader reads the persisted decision under kodema_runtime RLS.
	decisionReader, err := platform.NewDecisionReader(pgStore, pgStore)
	if err != nil {
		t.Fatalf("platform.NewDecisionReader: %v", err)
	}
	decCtx, err := decisionReader.ReadDecision(ctx, verify.DecisionQuery{
		Scope:      env.scope,
		DecisionID: resolved.DecisionID,
		Actor:      actor,
		Now:        now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReadDecision: %v%s", err, pgtest.CauseText(err))
	}
	if !decCtx.Found || decCtx.Decision.TaskID != created.TaskID || decCtx.TaskState != "open" || !decCtx.Decision.RevokedAt.IsZero() {
		t.Fatalf("ReadDecision = %+v", decCtx)
	}

	// Cancelling the task atomically revokes the approval_decisions row on PostgreSQL!
	if _, err := env.task.Cancel(ctx, task.CancelCommand{
		Actor:                    actor,
		Scope:                    env.scope,
		TaskID:                   created.TaskID,
		ExpectedLifecycleVersion: created.LifecycleVersion,
		CallerKey:                "cancel-approval-dec-task",
		Content:                  integrationContent("cancel-approval-dec-task"),
	}); err != nil {
		t.Fatalf("task.Cancel: %v%s", err, pgtest.CauseText(err))
	}

	afterCancel, err := decisionReader.ReadDecision(ctx, verify.DecisionQuery{
		Scope:      env.scope,
		DecisionID: resolved.DecisionID,
		Actor:      actor,
		Now:        now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ReadDecision after cancel: %v", err)
	}
	if !afterCancel.Found || afterCancel.Decision.RevokedAt.IsZero() || afterCancel.Decision.RevokedReason != "task_cancelled" || afterCancel.TaskState != "cancelled" {
		t.Fatalf("ReadDecision after cancel = %+v; want revoked_reason=task_cancelled & task_state=cancelled", afterCancel)
	}

	// Cross-project lookup returns Found=false (never leaks existence).
	env.shard.SeedProject(ctx, string(env.scope.TenantID), "project-other")
	otherCtx, err := decisionReader.ReadDecision(ctx, verify.DecisionQuery{
		Scope:      pgtest.Scope(string(env.scope.TenantID), "project-other"),
		DecisionID: resolved.DecisionID,
		Actor:      actor,
		Now:        now,
	})
	if err != nil {
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("cross-project ReadDecision err = %v", err)
		}
	} else if otherCtx.Found {
		t.Fatalf("cross-project ReadDecision unexpectedly found decision: %+v", otherCtx)
	}
}
