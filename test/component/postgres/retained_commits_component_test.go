//go:build component

package postgrescomponent

// Retained-commit discovery on real PostgreSQL. The retained-commit keeper
// consumes RetentionStore.RetainedCommits. This test seeds every evidence
// shape the query must distinguish and proves the exact commit set the Shell
// will keep: current and boundary evidence, active execution extension,
// expired evidence, an empty subject, a duplicate candidate, another project
// and a non-Git operation.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	retentiondriver "github.com/optimiware/kodama/internal/shell/app/drivers/retention"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func TestRetainedCommitsReturnsOnlyRetainedEvidence(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	now := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)

	own := pgtest.Scope("tenant-1", "project-1")
	foreign := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedService(ctx, own, "service-1", "enforcing")
	shard.SeedResource(ctx, own, "resource-1")
	shard.SeedService(ctx, foreign, "service-2", "enforcing")
	shard.SeedResource(ctx, foreign, "resource-2")
	shard.SeedExecution(ctx, own, "execution-running", "running", 1, "", "")
	shard.SeedExecution(ctx, own, "execution-failed", "failed", 1, "", "")

	live := strings.Repeat("a", 40)
	boundary := strings.Repeat("b", 40)
	expired := strings.Repeat("c", 40)
	active := strings.Repeat("d", 40)
	failedExecution := strings.Repeat("e", 40)
	foreignCommit := strings.Repeat("f", 40)
	nonGit := strings.Repeat("1", 40)

	seedVerification := func(scope struct {
		tenant  string
		project string
	}, service, resource, id, operation, candidate, execution string, created time.Time) {
		t.Helper()
		subject := "{}"
		if candidate != "" {
			subject = fmt.Sprintf(`{"fields":[{"name":"candidate","value":%q}]}`, candidate)
		}
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
			`INSERT INTO verifications (id, tenant_id, project_id, service_id, actor_id, execution_id, action_id,
			 binding_id, resource_id, operation, subject, requested_approval, result, approved, created_at)
			 VALUES ($1,$2,$3,$4,'principal-1',NULLIF($5, ''),NULL,'binding-1',$6,$7,$8::jsonb,NULL,'approved',true,$9)`,
			id, scope.tenant, scope.project, service, execution, resource, operation, subject, created); err != nil {
			t.Fatalf("seed verification %q: %v%s", id, err, pgtest.CauseText(err))
		}
	}
	ownScope := struct {
		tenant  string
		project string
	}{"tenant-1", "project-1"}
	foreignScope := struct {
		tenant  string
		project string
	}{"tenant-2", "project-2"}

	// Ordinary evidence is retained for 90 days: the boundary row stays, the
	// row one day past it expires.
	seedVerification(ownScope, "service-1", "resource-1", "evidence-live", "git.ref.update", live, "", now.Add(-89*24*time.Hour))
	seedVerification(ownScope, "service-1", "resource-1", "evidence-boundary", "git.ref.update", boundary, "", now.Add(-90*24*time.Hour))
	seedVerification(ownScope, "service-1", "resource-1", "evidence-expired", "git.ref.update", expired, "", now.Add(-91*24*time.Hour))
	// An active execution extends otherwise ancient evidence.
	seedVerification(ownScope, "service-1", "resource-1", "evidence-active", "git.ref.update", active, "execution-running", now.Add(-400*24*time.Hour))
	seedVerification(ownScope, "service-1", "resource-1", "evidence-failed", "git.ref.update", failedExecution, "execution-failed", now.Add(-400*24*time.Hour))
	// A row with no candidate field carries no commit.
	seedVerification(ownScope, "service-1", "resource-1", "evidence-empty", "git.ref.update", "", "", now.Add(-time.Hour))
	// A duplicate candidate is returned once.
	seedVerification(ownScope, "service-1", "resource-1", "evidence-live-duplicate", "git.ref.update", live, "", now.Add(-48*time.Hour))
	// Only git.ref.update evidence is a commit source.
	seedVerification(ownScope, "service-1", "resource-1", "evidence-non-git", "resource.update", nonGit, "", now.Add(-time.Hour))
	// Another project's evidence never leaks into this scope.
	seedVerification(foreignScope, "service-2", "resource-2", "evidence-foreign", "git.ref.update", foreignCommit, "", now.Add(-time.Hour))

	runtime := shard.Runtime(4)
	defer runtime.Close()
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")
	store := retentiondriver.NewRetentionStore(runtime)

	commits, err := store.RetainedCommits(ctx, installation.RetentionScope{TenantID: "tenant-1", ProjectID: "project-1"}, now)
	if err != nil {
		t.Fatalf("RetainedCommits: %v", err)
	}
	want := []string{live, boundary, active}
	if !reflect.DeepEqual(commits, want) {
		t.Fatalf("retained commits = %v; want %v", commits, want)
	}

	// A tenant-wide scope carries audit rows only and has no commit evidence.
	if tenantWide, err := store.RetainedCommits(ctx, installation.RetentionScope{TenantID: "tenant-1"}, now); err != nil || tenantWide != nil {
		t.Fatalf("tenant-wide RetainedCommits = %v, %v; want nil, nil", tenantWide, err)
	}
	// A project with no retained evidence returns an empty set, not an error.
	if empty, err := store.RetainedCommits(ctx, installation.RetentionScope{TenantID: "tenant-1", ProjectID: "project-empty"}, now); err != nil || len(empty) != 0 {
		t.Fatalf("empty project RetainedCommits = %v, %v; want an empty set", empty, err)
	}
	// The boundary time and context are required; neither is defaulted.
	if _, err := store.RetainedCommits(ctx, installation.RetentionScope{TenantID: "tenant-1", ProjectID: "project-1"}, time.Time{}); err == nil {
		t.Fatal("RetainedCommits accepted a zero boundary time")
	}
	var noContext context.Context
	if _, err := store.RetainedCommits(noContext, installation.RetentionScope{TenantID: "tenant-1", ProjectID: "project-1"}, now); err == nil {
		t.Fatal("RetainedCommits accepted a nil context")
	}
}
