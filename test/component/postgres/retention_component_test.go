//go:build component

package postgrescomponent

// Retention enforcement on real PostgreSQL. The test seeds rows for every
// S10 §5 family, including boundary, live, actively referenced and
// tenant-wide rows, then runs the production retention sweeper twice. It proves
// the exact intended set is removed by the first sweep and that the second
// sweep is a no-op.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	retentiondriver "github.com/optimiware/kodama/internal/shell/app/drivers/retention"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func TestRetentionSweepDeletesExactlyExpiredRowsAcrossTwoSweeps(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	now := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)

	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedService(ctx, scope1, "service-1", "enforcing")
	shard.SeedResource(ctx, scope1, "resource-1")
	shard.SeedService(ctx, scope2, "service-2", "enforcing")
	shard.SeedResource(ctx, scope2, "resource-2")
	shard.SeedExecution(ctx, scope1, "execution-running", "running", 1, "", "")
	shard.SeedExecution(ctx, scope1, "execution-failed", "failed", 1, "", "")
	shard.SeedExecution(ctx, scope2, "execution-running-2", "running", 1, "", "")

	seedVerification := func(scope struct {
		tenant  string
		project string
	}, id, execution string, created time.Time) {
		t.Helper()
		pgScope := pgtest.Scope(scope.tenant, scope.project)
		shard.SeedVerification(ctx, pgScope, id, "", "resource-1")
		if execution == "" {
			if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
				`UPDATE verifications SET created_at = $1 WHERE id = $2`, created, id); err != nil {
				t.Fatalf("age verification %q: %v%s", id, err, pgtest.CauseText(err))
			}
			return
		}
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
			`UPDATE verifications SET created_at = $1, execution_id = $2 WHERE id = $3`, created, execution, id); err != nil {
			t.Fatalf("age active verification %q: %v%s", id, err, pgtest.CauseText(err))
		}
	}
	own := struct {
		tenant  string
		project string
	}{"tenant-1", "project-1"}
	foreign := struct {
		tenant  string
		project string
	}{"tenant-2", "project-2"}

	// Ordinary evidence: 90 days. The boundary row and the row created after
	// the boundary stay; a row referenced by a running execution stays even
	// when it is far older; a row referenced only by a failed execution has no
	// active reference and expires.
	seedVerification(own, "evidence-expired", "", now.Add(-91*24*time.Hour))
	seedVerification(own, "evidence-boundary", "", now.Add(-90*24*time.Hour))
	seedVerification(own, "evidence-live", "", now.Add(-89*24*time.Hour))
	seedVerification(own, "evidence-active", "execution-running", now.Add(-400*24*time.Hour))
	seedVerification(own, "evidence-failed-execution", "execution-failed", now.Add(-400*24*time.Hour))
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO verifications (id, tenant_id, project_id, service_id, actor_id, execution_id, action_id,
		 binding_id, resource_id, operation, subject, requested_approval, result, approved, created_at)
		 VALUES ('evidence-foreign-expired','tenant-2','project-2','service-2','principal-1',NULL,NULL,
		 'binding-2','resource-2','git.ref.update','{}'::jsonb,NULL,'approved',true,$1)`,
		now.Add(-400*24*time.Hour)); err != nil {
		t.Fatalf("seed foreign verification: %v%s", err, pgtest.CauseText(err))
	}

	seedAudit := func(scope struct {
		tenant  string
		project string
	}, id, project string, at time.Time) {
		t.Helper()
		pgScope := pgtest.Scope(scope.tenant, scope.project)
		if project == "" {
			if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
				`INSERT INTO audit (id, tenant_id, project_id, actor, operation, at)
				 VALUES ($1, $2, NULL, 'principal-1', 'execution.start', $3)`,
				id, pgScope.TenantID, at); err != nil {
				t.Fatalf("seed tenant-wide audit %q: %v%s", id, err, pgtest.CauseText(err))
			}
			return
		}
		shard.SeedAudit(ctx, pgScope, id, "execution.start")
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(), `UPDATE audit SET at = $1 WHERE id = $2`, at, id); err != nil {
			t.Fatalf("age audit %q: %v%s", id, err, pgtest.CauseText(err))
		}
	}
	// Audit metadata: one year, including tenant-wide rows (project_id NULL)
	// swept through a tenant scope.
	seedAudit(own, "audit-expired", "project-1", now.Add(-366*24*time.Hour))
	seedAudit(own, "audit-boundary", "project-1", now.Add(-365*24*time.Hour))
	seedAudit(own, "audit-live", "project-1", now.Add(-364*24*time.Hour))
	seedAudit(own, "audit-tenant-wide-expired", "", now.Add(-366*24*time.Hour))
	seedAudit(own, "audit-tenant-wide-live", "", now.Add(-364*24*time.Hour))
	seedAudit(foreign, "audit-foreign-expired", "project-2", now.Add(-366*24*time.Hour))

	// Operation receipts never expire before project deletion. A ten-year-old
	// receipt must survive both sweeps.
	shard.SeedReceipt(ctx, scope1, "receipt-ancient", "principal-1", "task.create", "caller-1", `{"a":1}`, `{"task":"task-1"}`)
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE receipts SET created_at = $1, completed_at = $1 WHERE id = 'receipt-ancient'`,
		now.Add(-10*365*24*time.Hour)); err != nil {
		t.Fatalf("age receipt: %v%s", err, pgtest.CauseText(err))
	}

	// Raw model content is off by default; even an expired row must survive
	// while the policy has no opt-in.
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO content_records (id, tenant_id, project_id, principal_id, execution_id, model,
		 request_body, response_body, recorded_at, retain_until, version)
		 VALUES ('content-expired','tenant-1','project-1','principal-1',NULL,'model-1',
		 '\x00','\x00',$1,$2,1)`,
		now.Add(-400*24*time.Hour), now.Add(-399*24*time.Hour)); err != nil {
		t.Fatalf("seed expired content record: %v%s", err, pgtest.CauseText(err))
	}

	runtime := shard.Runtime(4)
	defer runtime.Close()
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")

	store := retentiondriver.NewRetentionStore(runtime)
	sweeper, err := installation.NewRetentionSweeper(installation.RetentionPolicy{}, store)
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}
	scopes := []installation.RetentionScope{
		{TenantID: "tenant-1", ProjectID: "project-1"},
		{TenantID: "tenant-1"},
	}

	result, err := sweeper.Sweep(ctx, scopes, now)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if got, want := result.DeletedByClass[installation.RetentionEvidence], 2; got != want {
		t.Errorf("first sweep deleted %d evidence rows; want %d", got, want)
	}
	if got, want := result.DeletedByClass[installation.RetentionAudit], 2; got != want {
		t.Errorf("first sweep deleted %d audit rows; want %d", got, want)
	}
	if got := result.DeletedByClass[installation.RetentionReceipts]; got != 0 {
		t.Errorf("first sweep deleted %d receipts; want 0", got)
	}

	for _, id := range []string{
		"evidence-expired",
		"evidence-failed-execution",
		"audit-expired",
		"audit-tenant-wide-expired",
	} {
		requireRetentionAbsent(t, ctx, shard, id)
	}
	for _, id := range []string{
		"evidence-boundary",
		"evidence-live",
		"evidence-active",
		"evidence-foreign-expired",
		"audit-boundary",
		"audit-live",
		"audit-tenant-wide-live",
		"audit-foreign-expired",
		"receipt-ancient",
		"content-expired",
	} {
		requireRetentionPresent(t, ctx, shard, id)
	}

	// The second sweep proves idempotence: exactly the same clock and scopes
	// must delete nothing, and every remaining row must still be present.
	second, err := sweeper.Sweep(ctx, scopes, now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, class := range installation.RetentionClasses() {
		if got := second.DeletedByClass[class]; got != 0 {
			t.Errorf("second sweep deleted %d %s rows; want 0", got, class)
		}
	}
	for _, id := range []string{
		"evidence-boundary", "evidence-live", "evidence-active", "evidence-foreign-expired",
		"audit-boundary", "audit-live", "audit-tenant-wide-live", "audit-foreign-expired",
		"receipt-ancient", "content-expired",
	} {
		requireRetentionPresent(t, ctx, shard, id)
	}
}

func requireRetentionPresent(t *testing.T, ctx context.Context, shard *pgtest.Shard, id string) {
	t.Helper()
	table := retentionTableFor(id)
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("count %s %q: %v%s", table, id, err, pgtest.CauseText(err))
	}
	if count != 1 {
		t.Fatalf("%s %q count = %d after the sweep; want 1", table, id, count)
	}
}

func requireRetentionAbsent(t *testing.T, ctx context.Context, shard *pgtest.Shard, id string) {
	t.Helper()
	table := retentionTableFor(id)
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("count %s %q: %v%s", table, id, err, pgtest.CauseText(err))
	}
	if count != 0 {
		t.Fatalf("%s %q still exists after the sweep; want it deleted", table, id)
	}
}

func retentionTableFor(id string) string {
	switch {
	case len(id) >= len("evidence-") && id[:len("evidence-")] == "evidence-":
		return "verifications"
	case len(id) >= len("audit-") && id[:len("audit-")] == "audit-":
		return "audit"
	case len(id) >= len("receipt-") && id[:len("receipt-")] == "receipt-":
		return "receipts"
	case len(id) >= len("content-") && id[:len("content-")] == "content-":
		return "content_records"
	default:
		return ""
	}
}
