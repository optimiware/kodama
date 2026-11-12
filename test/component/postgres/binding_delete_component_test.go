//go:build component

package postgrescomponent

// Deleting a verified resource binding on real PostgreSQL.
//
// A verification row keeps its binding_id as evidence and holds no reference
// to the binding row, so a verified binding can be deleted and the complete
// verification row survives unchanged.

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/resource"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// verificationRow reads the entire verification row in a stable text shape,
// so any changed column value after the deletion is attributable.
func verificationRow(ctx context.Context, t *testing.T, shard *pgtest.Shard, id string) []string {
	t.Helper()
	row, err := pgtest.QueryOne(ctx, shard.Admin(), 15,
		`SELECT id, tenant_id, project_id, service_id, actor_id,
		        COALESCE(execution_id, '<null>'), COALESCE(action_id, '<null>'),
		        binding_id, resource_id, operation, subject::text,
		        COALESCE(requested_approval::text, '<null>'), result,
		        approved::text, created_at::text
		   FROM verifications WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("read verification row %s: %v", id, err)
	}
	return row
}

// foreignKeyCatalog returns every foreign key by name and referencing table.
func foreignKeyCatalog(ctx context.Context, t *testing.T, shard *pgtest.Shard) map[string]string {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 2,
		`SELECT c.conname, t.relname
		   FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
		  WHERE c.contype = 'f'
		  ORDER BY 1`)
	if err != nil {
		t.Fatalf("read foreign key catalog: %v", err)
	}
	catalog := make(map[string]string, len(rows))
	for _, row := range rows {
		catalog[row[0]] = row[1]
	}
	return catalog
}

// seedVerifiedBinding installs one scope, service, resource, binding, membership
// and verification through the shared seed helpers.
func seedVerifiedBinding(ctx context.Context, t *testing.T, shard *pgtest.Shard) tenancy.Scope {
	t.Helper()
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, scope, "membership-1", "principal-1", 1, string(tenancy.RoleAdmin))
	shard.SeedService(ctx, scope, "service-1", "service-1", string(tenancy.RoleEngineer))
	shard.SeedResource(ctx, scope, "resource-1")
	shard.SeedBinding(ctx, scope, "binding-1", "resource-1", "service-1")
	shard.SeedVerification(ctx, scope, "verification-1", "binding-1", "resource-1")
	return scope
}

// bindingResourceService builds the real binding service over the shard's runtime
// role, so deletion runs under the production scope and version guard.
func bindingResourceService(t *testing.T, shard *pgtest.Shard) *resource.Service {
	t.Helper()
	runtime := shard.Runtime(4)
	t.Cleanup(runtime.Close)
	scoped := storepg.New(runtime)
	adapter, err := identityunits.New(identityunits.Config{
		Executor: scoped, Scanner: scoped,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	svc, err := resource.NewService(resource.Config{
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		Bindings: adapter, Deletes: adapter, Reads: adapter, Queries: adapter,
	})
	if err != nil {
		t.Fatalf("resource.NewService: %v", err)
	}
	return svc
}

// deleteVerifiedBinding deletes the seeded binding under its recorded version.
func deleteVerifiedBinding(ctx context.Context, t *testing.T, svc *resource.Service, scope tenancy.Scope) resource.BindingDeleteResult {
	t.Helper()
	deleted, err := svc.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor: humanAdmin(scope, "principal-1"), Scope: scope, BindingID: "binding-1",
		ExpectedVersion: 1, CallerKey: "delete-1", Content: bindingContent("delete-1"),
	})
	if err != nil {
		t.Fatalf("resource.binding.delete: %v%s", err, pgtest.CauseText(err))
	}
	return deleted
}

// TestDeletingAVerifiedBindingKeepsTheVerification proves the schema holds no
// verifications_binding reference, the verified binding is deletable, and the
// verification row is unchanged afterwards.
func TestDeletingAVerifiedBindingKeepsTheVerification(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(ctx, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	catalog := foreignKeyCatalog(ctx, t, shard)
	if len(catalog) != 49 {
		t.Fatalf("foreign keys = %d; want 49", len(catalog))
	}
	if _, ok := catalog["verifications_binding"]; ok {
		t.Fatal("the schema has a verifications_binding reference")
	}

	scope := seedVerifiedBinding(ctx, t, shard)
	svc := bindingResourceService(t, shard)
	verificationBefore := verificationRow(ctx, t, shard, "verification-1")
	if deleted := deleteVerifiedBinding(ctx, t, svc, scope); deleted.Version != 2 {
		t.Fatalf("delete result = %+v; want version 2", deleted)
	}
	verificationAfter := verificationRow(ctx, t, shard, "verification-1")
	if strings.Join(verificationAfter, "\x00") != strings.Join(verificationBefore, "\x00") {
		t.Fatalf("verification row changed:\nbefore %v\nafter  %v", verificationBefore, verificationAfter)
	}
}
