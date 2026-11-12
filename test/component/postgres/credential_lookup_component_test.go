//go:build component

package postgrescomponent

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestCredentialLookupProductionShapeOnPostgreSQL drives the product
// CredentialLookup adapter in its real unauthenticated bootstrap shape: the
// transaction is unscoped and the exact-row auth policy is admitted only after
// set_config('kodama.credential_id'). It proves the execution and service
// lookups return the candidate scope and that a wrong kind and an unknown id
// are the same uniform refusal.
func TestCredentialLookupProductionShapeOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	shard.SeedService(ctx, scope, "service-1", "enforcing")
	authDB := shard.Role(store.AuthRole, 2)
	defer authDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, authDB, store.AuthRole)
	lookup := storepg.NewCredentialLookup(authDB)

	candidate, err := lookup.LookupCredential(ctx, store.CredentialExecution, "execution-1")
	if err != nil {
		t.Fatalf("execution credential lookup: %v%s", err, pgtest.CauseText(err))
	}
	if candidate.TenantID != scope.TenantID || candidate.ProjectID != scope.ProjectID || candidate.PrincipalID != "principal-1" {
		t.Fatalf("execution candidate = %+v; want tenant-1/project-1/principal-1", candidate)
	}
	candidate, err = lookup.LookupCredential(ctx, store.CredentialService, "service-1")
	if err != nil {
		t.Fatalf("service credential lookup: %v%s", err, pgtest.CauseText(err))
	}
	if candidate.TenantID != scope.TenantID || candidate.ProjectID != scope.ProjectID {
		t.Fatalf("service candidate = %+v; want tenant-1/project-1", candidate)
	}
	// A wrong kind and an unknown id are indistinguishable.
	for _, testCase := range []struct {
		kind store.CredentialKind
		id   string
	}{
		{store.CredentialService, "execution-1"},
		{store.CredentialExecution, "missing"},
	} {
		_, err := lookup.LookupCredential(ctx, testCase.kind, testCase.id)
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("lookup %s/%s error = %v; want uniform unauthenticated", testCase.kind, testCase.id, err)
		}
	}
}
