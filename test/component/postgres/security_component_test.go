//go:build component

package postgrescomponent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func migratedShard(t *testing.T) *pgtest.Shard {
	t.Helper()
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(context.Background(), 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return shard
}

// TestNonOwnerForcedRLS proves the non-owner runtime role is confined to its
// transaction-local scope, cannot bypass RLS as the owner, and cannot write a
// row outside its scope.
func TestNonOwnerForcedRLS(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedTask(ctx, scope1, "task-1", "open", 1, "")
	shard.SeedTask(ctx, scope2, "task-2", "open", 1, "")

	runtime := shard.Runtime(4)
	defer runtime.Close()
	// A superuser or a BYPASSRLS role would make every refusal below pass for
	// the wrong reason, so prove the session identity first.
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")

	// Scope is established by the product adapter, not by a test helper, so a
	// broken establishScopeSQL is observable here.
	count, err := pgtest.AdapterScopedCount(ctx, runtime, scope1, "tasks")
	if err != nil || count != 1 {
		t.Fatalf("scope1 task count = %d, err=%v; want 1", count, err)
	}
	count, err = pgtest.AdapterScopedCount(ctx, runtime, scope2, "tasks")
	if err != nil || count != 1 {
		t.Fatalf("scope2 task count = %d, err=%v; want 1", count, err)
	}

	// A transaction with no scope sees nothing: the policy reads a NULL setting
	// and is not true.
	conn, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unscoped: %v", err)
	}
	unscoped, err := pgtest.QueryRowsCount(ctx, conn, `SELECT count(*) FROM tasks`)
	if err != nil {
		t.Fatalf("unscoped count: %v", err)
	}
	if unscoped != 0 {
		t.Fatalf("unscoped runtime saw %d task rows; want 0", unscoped)
	}
	if err := conn.Rollback(ctx); err != nil {
		t.Fatalf("rollback unscoped: %v", err)
	}

	// The app role cannot become the owner or otherwise bypass RLS.
	_, err = pgtest.ExecSQL(ctx, runtime, `SET ROLE kodama_owner`)
	pgtest.RequirePermissionDenied(t, err, "runtime SET ROLE kodama_owner")
	attrs := roleAttributesValue(ctx, t, shard, "kodama_runtime")
	if attrs.Super || attrs.BypassRLS {
		t.Fatalf("runtime role attributes = %+v; want non-super, NOBYPASSRLS", attrs)
	}

	// A write whose row scope differs from the transaction scope is refused by
	// the policy's WITH CHECK.
	_, err = pgtest.AdapterScopedExec(ctx, runtime, scope1,
		`INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, initiator_id, title, created_at)
		 VALUES ('task-cross','tenant-2','project-2',1,1,'open','principal-1','cross',now())`)
	pgtest.RequireRLSViolation(t, err, "cross-scope insert")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM tasks WHERE id = 'task-cross'`, 0, "cross-scope insert rows")
	// An update of another tenant's row affects zero rows.
	affected, err := pgtest.AdapterScopedExec(ctx, runtime, scope1, `UPDATE tasks SET state = 'done' WHERE id = 'task-2'`)
	if err != nil {
		t.Fatalf("cross-scope update: %v", err)
	}
	if affected != 0 {
		t.Fatalf("cross-scope update affected %d rows; want 0", affected)
	}
	state, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT state FROM tasks WHERE id = 'task-2'`)
	if err != nil || state != "open" {
		t.Fatalf("task-2 state = %q, err=%v; want unchanged open", state, err)
	}
}

// TestCompositeOwnershipConstraints proves the composite ownership foreign keys
// reject cross-tenant and cross-project references. Every referenced parent row
// really exists (in another project of the same tenant, or in another tenant),
// so only the composite tenant/project mismatch can reject the child. The admin
// superuser deliberately bypasses RLS: this test isolates the constraint, and
// the runtime role's non-owner posture is asserted separately. A pipeline
// selection is the one cross-project reference no constraint covers; the real
// task service refuses it under the runtime role instead.
func TestCompositeOwnershipConstraints(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-3")
	// project-2 is a real project of tenant-1: pipeline-2 and task-2 live there,
	// so a tenant-1/project-1 row pointing at them fails only the composite
	// (tenant_id, project_id) reference, never a dangling single-column id.
	shard.SeedProject(ctx, "tenant-1", "project-2")
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope12 := pgtest.Scope("tenant-1", "project-2")
	shard.SeedPipeline(ctx, scope1, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	shard.SeedPipeline(ctx, scope12, "pipeline-2", "other", []byte("actions: []\n"), 1, "[]")
	shard.SeedTask(ctx, scope1, "task-1", "open", 1, "")
	shard.SeedTask(ctx, scope12, "task-2", "open", 1, "")

	runtime := shard.Runtime(1)
	defer runtime.Close()
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")

	// A task selecting a pipeline that exists under the same tenant but in
	// another project. tasks.pipeline_id is an evidence identity and no
	// foreign key references pipelines; the Shell is the guard. The real task
	// service reads the selection in the task's own scope and its creation
	// unit pins that row, so the cross-project selection is not_found and
	// writes nothing.
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f' AND confrelid = 'pipelines'::regclass`,
		0, "foreign keys referencing pipelines")
	shard.SeedMembership(ctx, scope1, "membership-1", "principal-1", 1, string(tenancy.RoleEngineer))
	env := newServiceEnvOnShard(t, shard, nil)
	// task-1 is already seeded above; keep the service's generated ids clear.
	env.ids.restore(integrationIDsSnapshot{task: 100})
	const committedRows = `SELECT ((SELECT count(*) FROM tasks) + (SELECT count(*) FROM receipts) +
		(SELECT count(*) FROM audit) + (SELECT count(*) FROM events))::text`
	committed, err := pgtest.ScalarInt(ctx, shard.Admin(), committedRows)
	if err != nil {
		t.Fatalf("count rows before the cross-project selection: %v", err)
	}
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: scope1, Title: "cross", PipelineID: "pipeline-2",
		CallerKey: "task-cross", Content: integrationContent("task-cross"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("cross-project pipeline selection error = %v; want not_found", err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(), committedRows, committed, "rows after the refused cross-project selection")
	// The same service selecting the pipeline of its own project is the control.
	if _, err := env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: scope1, Title: "same", PipelineID: "pipeline-1",
		CallerKey: "task-own-project", Content: integrationContent("task-own-project"),
	}); err != nil {
		t.Fatalf("same-project pipeline selection: %v%s", err, pgtest.CauseText(err))
	}

	// Same-scope controls succeed, so a refusal below cannot be caused by an
	// unrelated missing parent or a malformed row.
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
		 VALUES ('membership-same','tenant-1','project-2','principal-same','[]'::jsonb,1,now())`); err != nil {
		t.Fatalf("same-scope membership: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, pipeline_id, initiator_id, title, created_at)
		 VALUES ('task-same','tenant-1','project-1',1,1,'open','pipeline-1','principal-1','same',now())`); err != nil {
		t.Fatalf("same-scope task: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO executions (id, tenant_id, project_id, task_id, principal_id, state, version, yaml_sha256, snapshot, created_at)
		 VALUES ('execution-same','tenant-1','project-1','task-1','principal-1','queued',1,'\\x00'::bytea,'{}'::jsonb,now())`); err != nil {
		t.Fatalf("same-scope execution: %v%s", err, pgtest.CauseText(err))
	}

	// A membership whose project exists but belongs to another tenant.
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
		 VALUES ('membership-cross','tenant-1','project-3','principal-cross','[]'::jsonb,1,now())`)
	pgtest.RequireForeignKeyViolation(t, err, "memberships_project", "cross-tenant membership")

	// An execution referencing a task that exists under the same tenant but in
	// another project.
	_, err = pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO executions (id, tenant_id, project_id, task_id, principal_id, state, version, yaml_sha256, snapshot, created_at)
		 VALUES ('execution-cross','tenant-1','project-1','task-2','principal-1','queued',1,'\\x00'::bytea,'{}'::jsonb,now())`)
	pgtest.RequireForeignKeyViolation(t, err, "executions_task", "cross-project task reference")

	for _, table := range []string{"memberships", "tasks", "executions"} {
		pgtest.RequireRowCount(t, ctx, shard.Admin(),
			`SELECT count(*)::text FROM `+table+` WHERE id LIKE '%cross%'`, 0, table+" cross rows")
	}
}

// TestSystemRoleGrantsAreLeastPrivilege exercises the declared controller,
// publisher and auth grants against real PostgreSQL. Attempts outside a grant
// are refused by the server, not merely by adapter validation.
func TestSystemRoleGrantsAreLeastPrivilege(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	shard.SeedEvent(ctx, scope, "event-1", "execution.queued", "execution-1", 1)
	shard.SeedService(ctx, scope, "service-1", "enforcing")
	shard.SeedTask(ctx, scope, "task-1", "open", 1, "")
	for _, privilege := range []string{"SELECT", "DELETE"} {
		grant, err := pgtest.ScalarString(ctx, shard.Admin(),
			`SELECT has_table_privilege($1, 'tasks', $2)::text`, store.SchemaDesign().RuntimeRole, privilege)
		if err != nil || grant != "true" {
			t.Fatalf("runtime role %s grant on tasks = %q, err=%v; want true", privilege, grant, err)
		}
	}

	controller := shard.Role(store.ControllerRole, 2)
	defer controller.Close()
	publisher := shard.Role(store.PublisherRole, 2)
	defer publisher.Close()
	auth := shard.Role(store.AuthRole, 2)
	defer auth.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controller, store.ControllerRole)
	pgtest.RequireNonOwnerSession(t, ctx, publisher, store.PublisherRole)
	pgtest.RequireNonOwnerSession(t, ctx, auth, store.AuthRole)

	// Controller reads executions and updates them only in its derived scope.
	count, err := pgtest.AdapterScopedCount(ctx, controller, scope, "executions")
	if err != nil || count != 1 {
		t.Fatalf("controller executions = %d, err=%v; want 1", count, err)
	}
	_, err = pgtest.AdapterScopedExec(ctx, controller, scope, `SELECT count(*) FROM tasks`)
	pgtest.RequirePermissionDenied(t, err, "controller read tasks")
	// The controller has execution-bound INSERT on events and audit, but
	// no UPDATE/DELETE on either and no SELECT on audit.
	if _, err := pgtest.AdapterScopedExec(ctx, controller, scope,
		`INSERT INTO events (id, tenant_id, project_id, type, subject_id, source_kind, source_principal, at, publish_state)
		 VALUES ('event-2','tenant-1','project-1','execution.queued','execution-1','shell','principal-1',now(),'pending')`); err != nil {
		t.Fatalf("controller event insert: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := pgtest.AdapterScopedExec(ctx, controller, scope,
		`INSERT INTO audit (id, tenant_id, project_id, actor, operation, at) VALUES ('audit-2','tenant-1','project-1','principal-1','execution.start',now())`); err != nil {
		t.Fatalf("controller audit insert: %v%s", err, pgtest.CauseText(err))
	}
	// Each denied statement is otherwise valid: it names real rows and columns
	// and would succeed if the grant existed.
	_, err = pgtest.AdapterScopedExec(ctx, controller, scope, `UPDATE events SET publish_state = 'published' WHERE id = 'event-2'`)
	pgtest.RequirePermissionDenied(t, err, "controller update event")
	_, err = pgtest.AdapterScopedExec(ctx, controller, scope, `DELETE FROM events WHERE id = 'event-2'`)
	pgtest.RequirePermissionDenied(t, err, "controller delete event")
	_, err = pgtest.AdapterScopedExec(ctx, controller, scope, `SELECT count(*) FROM audit`)
	pgtest.RequirePermissionDenied(t, err, "controller read audit")

	// Publisher reads and updates only events.publish_state.
	count, err = pgtest.AdapterScopedCount(ctx, publisher, scope, "events")
	if err != nil || count != 2 {
		t.Fatalf("publisher events = %d, err=%v; want 2 (seeded plus controller insert)", count, err)
	}
	_, err = pgtest.AdapterScopedExec(ctx, publisher, scope, `SELECT count(*) FROM tasks`)
	pgtest.RequirePermissionDenied(t, err, "publisher read tasks")
	if _, err := pgtest.AdapterScopedExec(ctx, publisher, scope,
		`UPDATE events SET publish_state = 'published' WHERE id = 'event-1'`); err != nil {
		t.Fatalf("publisher publish_state update: %v%s", err, pgtest.CauseText(err))
	}
	_, err = pgtest.AdapterScopedExec(ctx, publisher, scope,
		`UPDATE events SET type = 'changed' WHERE id = 'event-1'`)
	pgtest.RequirePermissionDenied(t, err, "publisher update non-granted event column")

	// Auth reads only the exact credential row and only the granted columns.
	rows, err := pgtest.QueryStringsN(ctx, auth, 1, `SELECT count(*)::text FROM executions`)
	if err != nil || rows[0][0] != "0" {
		t.Fatalf("auth unscoped executions = %v, err=%v; want 0", rows, err)
	}
	_, err = pgtest.ExecSQL(ctx, auth, `SELECT token_nonce FROM services`)
	pgtest.RequirePermissionDenied(t, err, "auth read services.token_nonce")
	// A fully valid execution row: only the missing INSERT grant can deny it.
	// The message assertion distinguishes the grant from the auth role's
	// row-level-security posture.
	_, err = pgtest.ExecSQL(ctx, auth,
		`INSERT INTO executions (id, tenant_id, project_id, pipeline_id, task_id, principal_id, state, version,
		 yaml_sha256, snapshot, token_nonce, token_expires_at, workspace_id, created_at)
		 VALUES ('execution-auth','tenant-1','project-1',NULL,NULL,'principal-1','queued',1,
		 '\x00'::bytea,'{}'::jsonb,NULL,NULL,'workspace-auth',now())`)
	pgtest.RequirePermissionDenied(t, err, "auth insert execution")
	conn, err := auth.Begin(ctx)
	if err != nil {
		t.Fatalf("auth begin: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('kodama.credential_id', $1, true)`, "execution-1"); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	rows, err = pgtest.QueryRows(ctx, conn, 2, `SELECT id, principal_id FROM executions`)
	if err != nil {
		t.Fatalf("auth exact-row read: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "execution-1" {
		t.Fatalf("auth exact-row read = %v; want execution-1 only", rows)
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("auth commit: %v", err)
	}
}

// TestPooledConnectionScopeReset interleaves tenants over a pool of one and
// proves no transaction or scope survives completion. Every scope is established
// by the product adapter, and after each adapter-driven commit or rollback a
// fresh raw transaction on the same pooled connection must read empty settings
// and zero visible rows.
func TestPooledConnectionScopeReset(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedTask(ctx, scope1, "task-1", "open", 1, "")
	shard.SeedTask(ctx, scope2, "task-2", "open", 1, "")

	runtime := shard.Runtime(1)
	defer runtime.Close()
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")
	scoped := storepg.New(runtime)

	for iteration := range 3 {
		count, err := pgtest.AdapterScopedCount(ctx, runtime, scope1, "tasks")
		if err != nil || count != 1 {
			t.Fatalf("iteration %d scope1 count = %d, err=%v; want 1", iteration, count, err)
		}
		requirePooledScopeReset(ctx, t, runtime)
		count, err = pgtest.AdapterScopedCount(ctx, runtime, scope2, "tasks")
		if err != nil || count != 1 {
			t.Fatalf("iteration %d scope2 count = %d, err=%v; want 1", iteration, count, err)
		}
		requirePooledScopeReset(ctx, t, runtime)
	}

	// A rolled-back adapter transaction resets the scope too.
	first, err := scoped.Begin(ctx, scope1)
	if err != nil {
		t.Fatalf("store begin scope1: %v", err)
	}
	firstRows, err := first.Query(ctx, store.Query{SQL: `SELECT count(*) FROM tasks`})
	if err != nil {
		t.Fatalf("store scope1 query: %v", err)
	}
	values, err := pgtest.ScanRows(firstRows, 1)
	if err != nil || values[0][0] != "1" {
		t.Fatalf("store scope1 count = %v, err=%v; want 1", values, err)
	}
	if err := first.Rollback(ctx); err != nil {
		t.Fatalf("store scope1 rollback: %v", err)
	}
	requirePooledScopeReset(ctx, t, runtime)

	// The same pooled connection accepts a different tenant immediately, then
	// resets again.
	second, err := scoped.Begin(ctx, scope2)
	if err != nil {
		t.Fatalf("store begin scope2: %v", err)
	}
	result, err := second.Query(ctx, store.Query{SQL: `SELECT count(*) FROM tasks`})
	if err != nil {
		t.Fatalf("store scope2 query: %v", err)
	}
	values, err = pgtest.ScanRows(result, 1)
	if err != nil || values[0][0] != "1" {
		t.Fatalf("store scope2 count = %v, err=%v; want 1", values, err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("store scope2 commit: %v", err)
	}
	requirePooledScopeReset(ctx, t, runtime)

	// A completed wrapper transaction refuses further statements.
	conn, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := conn.Query(ctx, `SELECT 1`); err == nil {
		t.Fatal("completed connection accepted another query; want ErrNoTransaction")
	} else if err != storepg.ErrNoTransaction && !strings.Contains(err.Error(), "not active") {
		t.Fatalf("completed connection error = %v; want ErrNoTransaction", err)
	}
}

// requirePooledScopeReset opens a raw transaction on the same pool and proves it
// inherited neither setting and cannot see any row without a scope. It is the
// observation the previous test made only after a test-helper transaction.
func requirePooledScopeReset(ctx context.Context, t *testing.T, runtime *pgdriver.DB) {
	t.Helper()
	conn, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatalf("raw pooled begin: %v", err)
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := pgtest.QueryRows(ctx, conn, 3,
		`SELECT COALESCE(current_setting('kodama.tenant_id', true), ''),
		        COALESCE(current_setting('kodama.project_id', true), ''),
		        count(*)::text
		 FROM tasks`)
	if err != nil {
		t.Fatalf("read pooled scope: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read pooled scope: %d rows; want 1", len(rows))
	}
	if rows[0][0] != "" || rows[0][1] != "" {
		t.Fatalf("pooled connection retained scope tenant=%q project=%q; want both empty", rows[0][0], rows[0][1])
	}
	if rows[0][2] != "0" {
		t.Fatalf("unscoped pooled transaction saw %s tasks; want 0", rows[0][2])
	}
	if err := conn.Commit(ctx); err != nil {
		t.Fatalf("raw pooled commit: %v", err)
	}
}

// TestProjectScopeExcludesTenantWideRowsOnPostgreSQL proves the adapter's SQL
// project predicate, not only RLS, excludes NULL-project rows from a project
// scope: RLS deliberately admits tenant-wide rows to a project transaction, so
// the adapter's own predicate is the primary layer.
func TestProjectScopeExcludesTenantWideRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	tenant := pgtest.TenantScope("tenant-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedService(ctx, tenant, "service-wide", "wide")
	shard.SeedMembership(ctx, tenant, "membership-wide", "principal-wide", 1, "engineer")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	for _, table := range []struct{ name, id string }{
		{"services", "service-wide"},
		{"memberships", "membership-wide"},
	} {
		if _, err := executor.Read(ctx, scope, table.name, table.id); !adapterClass(err, fault.NotFound) {
			t.Fatalf("project-scope read of tenant-wide %s error = %v; want not_found", table.name, err)
		}
		row, err := executor.Read(ctx, tenant, table.name, table.id)
		if err != nil || row.Values["id"] != table.id {
			t.Fatalf("tenant-scope read of %s = %v, err=%v; want the tenant-wide row", table.name, row.Values, err)
		}
		page, err := runtimeScanner(runtime).Query(ctx, scope, store.ScanQuery{Table: table.name, Limit: 10})
		if err != nil {
			t.Fatalf("project-scope scan of %s: %v", table.name, err)
		}
		for _, row := range page.Rows {
			if row.Values["id"] == table.id {
				t.Fatalf("project-scope scan of %s returned the tenant-wide row", table.name)
			}
		}
		tenantPage, err := runtimeScanner(runtime).Query(ctx, tenant, store.ScanQuery{Table: table.name, Limit: 10})
		if err != nil {
			t.Fatalf("tenant-scope scan of %s: %v", table.name, err)
		}
		found := false
		for _, row := range tenantPage.Rows {
			if row.Values["id"] == table.id {
				found = true
			}
		}
		if !found {
			t.Fatalf("tenant-scope scan of %s did not return the tenant-wide row", table.name)
		}
	}
}

// runtimeScanner returns the scanner seam over the runtime role.
func runtimeScanner(runtime *pgdriver.DB) *storepg.Store { return storepg.New(runtime) }

// TestDeadlineGuardOnRealPostgreSQL proves the transaction-time DeadlineGuard
// refuses a past deadline for both a guarded Write and a guarded Lock, and
// commits a future deadline. The strict-after boundary (deadline == transaction
// now is expired) cannot be constructed deterministically from outside the
// transaction, so the past/future pair pins the comparison direction.
func TestDeadlineGuardOnRealPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 1, "", "")
	shard.SeedExecution(ctx, scope, "execution-2", "running", 1, "", "")
	shard.SeedRunningAction(ctx, scope, "action-past", "execution-1", 0, time.Now().Add(-time.Hour))
	shard.SeedRunningAction(ctx, scope, "action-future", "execution-2", 0, time.Now().Add(time.Hour))
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)
	guard := func() *store.DeadlineGuard { return &store.DeadlineGuard{Column: "deadline"} }

	// A past-deadline guarded write is expired and leaves the row unchanged.
	_, err := executor.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
		Table: "actions", ID: "action-past", ExpectVersion: 1,
		Values: map[string]any{"state": "running"}, Deadline: guard(),
	}}})
	if class, ok := fault.ClassOf(err); !ok || class != fault.DeadlineExpired {
		t.Fatalf("past-deadline write error = %v; want deadline_expired", err)
	}
	unchanged := scopedRow(t, ctx, runtime, scope, 2,
		`SELECT state, version::text FROM actions WHERE id = 'action-past'`)
	if unchanged[0] != "running" || unchanged[1] != "1" {
		t.Fatalf("past-deadline action = %v; want running/1 unchanged", unchanged)
	}

	// A future-deadline guarded write commits and advances the version.
	if _, err := executor.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
		Table: "actions", ID: "action-future", ExpectVersion: 1,
		Values: map[string]any{"state": "running"}, Deadline: guard(),
	}}}); err != nil {
		t.Fatalf("future-deadline write: %v%s", err, pgtest.CauseText(err))
	}
	written := scopedRow(t, ctx, runtime, scope, 1,
		`SELECT version::text FROM actions WHERE id = 'action-future'`)
	if written[0] != "2" {
		t.Fatalf("future-deadline action version = %s; want 2", written[0])
	}

	// The same strict-after rule applies to a guarded lock.
	_, err = executor.Execute(ctx, scope, store.Unit{Locks: []store.Lock{{
		Table: "actions", IDs: []string{"action-past"}, ExpectVersion: 1, Deadline: guard(),
	}}})
	if class, ok := fault.ClassOf(err); !ok || class != fault.DeadlineExpired {
		t.Fatalf("past-deadline lock error = %v; want deadline_expired", err)
	}
	if _, err := executor.Execute(ctx, scope, store.Unit{Locks: []store.Lock{{
		Table: "actions", IDs: []string{"action-future"}, ExpectVersion: 2, Deadline: guard(),
	}}}); err != nil {
		t.Fatalf("future-deadline lock: %v%s", err, pgtest.CauseText(err))
	}
}
