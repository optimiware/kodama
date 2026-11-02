package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Real-SQL coverage of the runtime-issued Git grant records (T-150, migration
// 014_attempt_git.sql): the migration, tenant isolation, the constraints that keep
// the columns equal to the stored claims, and the append-only rule. These tests
// skip in the unit layer and without PostgreSQL.

func Test_attempt_git_migration_reapply(t *testing.T) {
	manifest, err := storage.NewManifest(storage.BaseContribution(), rtpg.ResourceMigrations(), rtpg.AttemptGitMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if got := rtpg.AttemptGitMigrations().Migrations; len(got) != 1 || got[0].ID != rtpg.AttemptGitMigrationID {
		t.Fatalf("the contribution is exactly migration 014: %+v", got)
	}
	if got := rtpg.ResourceMigrations().Migrations; len(got) != 1 || got[0].ID != rtpg.ResourceGrantsMigrationID {
		t.Fatalf("the earlier contribution is unchanged: %+v", got)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(bg, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Migrate(bg, owner, manifest, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	var n int
	if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'runtime_git_grants'::regclass AND conname = ANY($1)`,
		[]string{"runtime_git_grants_pkey", "runtime_git_grants_kind_known", "runtime_git_grants_claims_match", "runtime_git_grants_tenant_id_project_id_fkey"}).Scan(&n); err != nil || n != 4 {
		t.Fatalf("constraints: %d %v", n, err)
	}
	if err := owner.QueryRow(bg, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'runtime_git_grants'::regclass AND tgname = 'runtime_git_grants_immutable' AND NOT tgisinternal`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("guard trigger: %d %v", n, err)
	}
	// Applying the contribution does not change the earlier one's checksum.
	changed := rtpg.AttemptGitMigrations()
	changed.Migrations[0].SQL += "\n-- edited"
	bad, _ := storage.NewManifest(storage.BaseContribution(), rtpg.ResourceMigrations(), changed)
	if _, err := storage.Migrate(bg, owner, bad, opts); err == nil {
		t.Fatal("a modified applied migration was accepted")
	}
}

func Test_attempt_git_isolation_and_guards(t *testing.T) {
	s := rtest.New(t, rtest.Options{AttemptGit: true})
	a := domain.Scope{TenantID: "t150_acme", ProjectID: "p150_inventory"}
	b := domain.Scope{TenantID: "t150_globex", ProjectID: "p150_ledger"}
	s.SeedProject(a)
	s.SeedProject(b)
	store, err := rtpg.NewIssuedGrants(s.Runtime, s.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []runtimeauth.IssuedGrant{
		{Scope: a, RunID: "run_1", AttemptID: "att_1", Claims: rtest.IssuedClaims(gitv1.GrantSeed, a, "run_1", "att_1", "gg_a1", "repo_1")},
		{Scope: b, RunID: "run_1", AttemptID: "att_1", Claims: rtest.IssuedClaims(gitv1.GrantSeed, b, "run_1", "att_1", "gg_b1", "repo_1")},
	} {
		if err := store.RecordIssued(bg, g); err != nil {
			t.Fatal(err)
		}
	}
	inTenant := func(tenant domain.TenantID, fn func(tx pgx.Tx) error) error {
		return storage.WithTenant(bg, s.Runtime, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
	}

	t.Run("RLS hides other tenants and fails closed", func(t *testing.T) {
		for _, tenant := range []domain.TenantID{a.TenantID, b.TenantID} {
			if err := inTenant(tenant, func(tx pgx.Tx) error {
				var n int
				if err := tx.QueryRow(bg, `SELECT count(*) FROM runtime_git_grants`).Scan(&n); err != nil || n != 1 {
					t.Errorf("%s sees %d records (%v)", tenant, n, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := s.Runtime.QueryRow(bg, `SELECT count(*) FROM runtime_git_grants`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("no tenant context sees %d records (%v)", n, err)
		}
	})

	t.Run("the runtime role can only record and read", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE runtime_git_grants SET repository_id = 'repo_2'`,
			`DELETE FROM runtime_git_grants`,
			`TRUNCATE runtime_git_grants`,
		} {
			err := inTenant(a.TenantID, func(tx pgx.Tx) error { _, err := tx.Exec(bg, stmt); return err })
			if pgCode(err) != "42501" {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	})

	t.Run("a record is immutable for every role", func(t *testing.T) {
		for _, stmt := range []string{`UPDATE runtime_git_grants SET repository_id = repository_id`, `DELETE FROM runtime_git_grants`} {
			if _, err := s.Owner.Exec(bg, stmt); pgCode(err) != "23514" {
				t.Errorf("owner %s: %v", stmt, err)
			}
		}
	})

	t.Run("composite ownership and the claims match", func(t *testing.T) {
		claims := func(tenant domain.TenantID, project domain.ProjectID, repo string) string {
			return `{"grantId":"gg_x","audience":"kodama-git","tenantId":"` + string(tenant) + `","projectId":"` + string(project) + `","kind":"seed",` +
				`"actor":{"type":"workload","id":"att_x","runId":"run_x","attemptId":"att_x"},"repositories":[{"id":"` + repo + `","read":true,"update":["refs/heads/main"],"seed":true,"integration":null}],` +
				`"issuedAt":"2026-10-30T10:00:00Z","expiresAt":"2026-10-30T10:10:00Z"}`
		}
		insert := `INSERT INTO runtime_git_grants (tenant_id, grant_id, project_id, run_id, attempt_id, kind, repository_id, claims, recorded_at)
			VALUES ($1, 'gg_x', $2, 'run_x', 'att_x', 'seed', $3, $4::jsonb, now())`
		try := func(ctxTenant, tenant domain.TenantID, project domain.ProjectID, repo, claimsRepo string) error {
			return inTenant(ctxTenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(bg, insert, string(tenant), string(project), repo, claims(tenant, project, claimsRepo))
				return err
			})
		}
		if code := pgCode(try(a.TenantID, a.TenantID, b.ProjectID, "repo_x", "repo_x")); code != "23503" {
			t.Errorf("another tenant's project: %s", code)
		}
		if code := pgCode(try(a.TenantID, b.TenantID, b.ProjectID, "repo_x", "repo_x")); code != "42501" {
			t.Errorf("another tenant's row: %s", code)
		}
		if code := pgCode(try(a.TenantID, a.TenantID, a.ProjectID, "repo_other", "repo_x")); code != "23514" {
			t.Errorf("a column that differs from the claims: %s", code)
		}
		if err := try(a.TenantID, a.TenantID, a.ProjectID, "repo_x", "repo_x"); err != nil {
			t.Errorf("a consistent row: %v", err)
		}
	})
}
