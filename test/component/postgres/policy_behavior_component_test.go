//go:build component

package postgrescomponent

import (
	"context"
	"strconv"
	"testing"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestSystemRoleRLSPoliciesOnPostgreSQL is a CATALOG-POLICY PROOF ONLY: every
// statement is raw SQL inside a transaction that sets the transaction-local
// settings, through the real non-owner logins. It proves the declared
// controller/auth policies and the publisher's scoped-shape protections, but it
// does NOT prove the publisher's production shape (the outbox runs unscoped).
// TestPublisherProductionShapeOnPostgreSQL is the production-shape proof; the
// publisher block below drops the mirroring CHECK and the permissive
// events_scope policy in its isolated shard specifically to expose the declared
// WITH CHECK in isolation, so it must not be read as a real-schema guarantee.
func TestSystemRoleRLSPoliciesOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	// scope2 is the same tenant but a different project, so a policy that
	// checks only tenant_id would wrongly admit the tenant-1/project-1 rows.
	scope2 := pgtest.Scope("tenant-1", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedProject(ctx, "tenant-1", "project-2")
	shard.SeedExecution(ctx, scope1, "execution-1", "queued", 1, "", "")
	shard.SeedAction(ctx, scope1, "action-1", "execution-1", 0, "running", 1)
	shard.SeedCleanup(ctx, scope1, "cleanup-1", "execution-1", "pending")
	shard.SeedEvent(ctx, scope1, "event-1", "execution.queued", "execution-1", 1)
	shard.SeedService(ctx, scope1, "service-1", "enforcing")

	controller := shard.Role(store.ControllerRole, 2)
	defer controller.Close()
	publisher := shard.Role(store.PublisherRole, 2)
	defer publisher.Close()
	auth := shard.Role(store.AuthRole, 2)
	defer auth.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controller, store.ControllerRole)
	pgtest.RequireNonOwnerSession(t, ctx, publisher, store.PublisherRole)
	pgtest.RequireNonOwnerSession(t, ctx, auth, store.AuthRole)

	// Controller: the UPDATE grants exist on all three tables, so a zero row
	// count for a foreign transaction scope is the USING policy hiding the row.
	for _, statement := range []string{
		`UPDATE executions SET state = 'running' WHERE id = 'execution-1'`,
		`UPDATE actions SET state = 'running' WHERE id = 'action-1'`,
		`UPDATE cleanup SET state = 'pending' WHERE id = 'cleanup-1'`,
	} {
		affected, err := pgtest.ScopedExec(ctx, controller, scope2, statement)
		if err != nil {
			t.Fatalf("controller foreign-scope update %q: %v%s", statement, err, pgtest.CauseText(err))
		}
		if affected != 0 {
			t.Fatalf("controller foreign-scope update %q affected %d rows; want 0", statement, affected)
		}
	}
	// The INSERT grants exist on events and audit, so a foreign-scope row must
	// be refused by the WITH CHECK policy, not by a missing privilege.
	_, err := pgtest.ScopedExec(ctx, controller, scope2,
		`INSERT INTO events (id, tenant_id, project_id, type, subject_id, source_kind, source_principal, at, publish_state)
		 VALUES ('event-foreign','tenant-1','project-1','execution.queued','execution-1','shell','principal-1',now(),'pending')`)
	pgtest.RequireRLSViolation(t, err, "controller foreign-scope event insert")
	_, err = pgtest.ScopedExec(ctx, controller, scope2,
		`INSERT INTO audit (id, tenant_id, project_id, actor, operation, at)
		 VALUES ('audit-foreign','tenant-1','project-1','principal-1','execution.start',now())`)
	pgtest.RequireRLSViolation(t, err, "controller foreign-scope audit insert")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-foreign'`, 0, "foreign-scope event rows")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM audit WHERE id = 'audit-foreign'`, 0, "foreign-scope audit rows")

	// The same statements in the owning transaction scope commit.
	for _, statement := range []string{
		`UPDATE executions SET state = 'running' WHERE id = 'execution-1'`,
		`UPDATE actions SET state = 'running' WHERE id = 'action-1'`,
		`UPDATE cleanup SET state = 'pending' WHERE id = 'cleanup-1'`,
	} {
		affected, err := pgtest.ScopedExec(ctx, controller, scope1, statement)
		if err != nil || affected != 1 {
			t.Fatalf("controller owning-scope update %q = %d rows, err=%v; want 1", statement, affected, err)
		}
	}
	if _, err := pgtest.ScopedExec(ctx, controller, scope1,
		`INSERT INTO events (id, tenant_id, project_id, type, subject_id, source_kind, source_principal, at, publish_state)
		 VALUES ('event-owned','tenant-1','project-1','execution.started','execution-1','shell','principal-1',now(),'pending')`); err != nil {
		t.Fatalf("controller owning-scope event insert: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := pgtest.ScopedExec(ctx, controller, scope1,
		`INSERT INTO audit (id, tenant_id, project_id, actor, operation, at)
		 VALUES ('audit-owned','tenant-1','project-1','principal-1','execution.start',now())`); err != nil {
		t.Fatalf("controller owning-scope audit insert: %v%s", err, pgtest.CauseText(err))
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-owned'`, 1, "owning-scope event rows")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM audit WHERE id = 'audit-owned'`, 1, "owning-scope audit rows")

	// Publisher: two other declared layers would mask the declared WITH CHECK.
	// events.publish_state has a CHECK constraint with the same value set, and
	// the PUBLIC events_scope FOR ALL permissive policy's implicit WITH CHECK
	// is true for an in-scope row, so PostgreSQL ORs it with the publisher
	// policy. Drop both in this isolated shard so the declared publisher WITH
	// CHECK is the only thing that can refuse the out-of-set value. The
	// schema-facts test separately proves both remain in the declared catalog.
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(), `ALTER TABLE events DROP CONSTRAINT events_publish_state`); err != nil {
		t.Fatalf("drop mirroring publish_state check for RLS isolation: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(), `DROP POLICY events_scope ON events`); err != nil {
		t.Fatalf("drop permissive events scope policy for RLS isolation: %v%s", err, pgtest.CauseText(err))
	}
	_, err = pgtest.ScopedExec(ctx, publisher, scope1, `UPDATE events SET publish_state = 'archived' WHERE id = 'event-1'`)
	pgtest.RequireRLSViolation(t, err, "publisher out-of-set publish_state")
	publishState, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT publish_state FROM events WHERE id = 'event-1'`)
	if err != nil || publishState != "pending" {
		t.Fatalf("event publish_state after refused update = %q, err=%v; want unchanged pending", publishState, err)
	}
	affected, err := pgtest.ScopedExec(ctx, publisher, scope1, `UPDATE events SET publish_state = 'published' WHERE id = 'event-1'`)
	if err != nil || affected != 1 {
		t.Fatalf("publisher in-set publish_state update = %d rows, err=%v; want 1", affected, err)
	}
	publishState, err = pgtest.ScalarString(ctx, shard.Admin(), `SELECT publish_state FROM events WHERE id = 'event-1'`)
	if err != nil || publishState != "published" {
		t.Fatalf("event publish_state after in-set update = %q, err=%v; want published", publishState, err)
	}

	// Auth: the exact-row SELECT policy admits only the row named by the
	// transaction-local credential. The role is read-only by contract, so the
	// applicable write check is the missing UPDATE grant.
	requireAuthRow(t, ctx, auth, "execution-2", `SELECT id, principal_id FROM executions`, 0, "auth wrong-credential execution rows")
	requireAuthRow(t, ctx, auth, "execution-1", `SELECT id, principal_id FROM executions`, 1, "auth exact-credential execution rows")
	requireAuthRow(t, ctx, auth, "execution-1", `SELECT id FROM services`, 0, "auth wrong-credential service rows")
	requireAuthRow(t, ctx, auth, "service-1", `SELECT id FROM services`, 1, "auth exact-credential service rows")
	_, err = pgtest.ExecSQL(ctx, auth, `UPDATE executions SET state = 'running' WHERE id = 'execution-1'`)
	pgtest.RequirePermissionDenied(t, err, "auth read-only execution update")
}

// requireAuthRow runs one auth SELECT in a transaction that sets the named
// credential and asserts the visible row count.
func requireAuthRow(t *testing.T, ctx context.Context, auth *pgdriver.DB, credential, query string, want int, label string) {
	t.Helper()
	conn, err := auth.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin: %v", label, err)
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, `SELECT set_config('kodama.credential_id', $1, true)`, credential); err != nil {
		t.Fatalf("%s: set credential: %v", label, err)
	}
	rows, err := pgtest.QueryRows(ctx, conn, 1, `SELECT count(*) FROM (`+query+`) AS visible`)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if len(rows) != 1 || rows[0][0] != strconv.Itoa(want) {
		t.Fatalf("%s = %v; want %d", label, rows, want)
	}
}
