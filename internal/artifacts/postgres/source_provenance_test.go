package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	repositorypg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// TestSourceProvenanceMigration (T-149, DQ-17): the additive migration 013 applies
// after the baseline and repeats without changing data, keeps every earlier
// artifact row valid, and the database refuses a malformed, mixed, reordered or
// rewritten source set.
func TestSourceProvenanceMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgtest.New(t)
	mig := db.MigrationPool(ctx, 4)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}

	base, err := storage.NewManifest(storage.BaseContribution(), repositorypg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, mig, base, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `ALTER TABLE artifacts DROP CONSTRAINT artifacts_producer_attempt_fk`); err != nil {
		t.Fatal(err)
	}
	digest := `sha256:` + strings.Repeat("ab", 32)
	seed := `INSERT INTO tenants (id, key) VALUES ('t_a', 'a'); INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t_a', 'p_1', 'p1', 3);`
	if _, err := mig.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"repo_api", "repo_web"} {
		if _, err := mig.Exec(ctx, `INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ('t_a','p_1',$1::text,replace($1::text,'_','-'),'main','internal','ready','user','u',now(),now())`, id); err != nil {
			t.Fatal(err)
		}
	}
	oci := func(id, extraCols, extraVals string) string {
		return `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, registry` + extraCols + `,
 state, verified_at, verified_size_bytes, created_at, updated_at, version)
VALUES ('t_a', 'p_1', '` + id + `', 'run_1', 'att_1', 'build', '` + id + `', 'oci', 'output', '` + digest + `', 'registry.example.test/team/app'` + extraVals + `,
 'registered', now(), 10, now(), now(), 1)`
	}
	// An artifact of the earlier schema, written before the migration.
	if _, err := mig.Exec(ctx, oci("art_old", `, source_repository_url, source_commit`, `, 'https://git.example.test/a.git', '`+strings.Repeat("a", 40)+`'`)); err != nil {
		t.Fatal(err)
	}

	m, err := storage.NewManifest(storage.BaseContribution(), repositorypg.Migrations(), artifactspg.SourceProvenanceMigrations())
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil || len(first.Applied) != 1 || first.Applied[0] != artifactspg.SourceProvenanceMigrationID {
		t.Fatalf("013 is the one pending migration: %+v %v", first, err)
	}
	second, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil || len(second.Applied) != 0 {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	var url, commit *string
	var setDigest *string
	if err := mig.QueryRow(ctx, `SELECT source_repository_url, source_commit, source_set_digest FROM artifacts WHERE id = 'art_old'`).Scan(&url, &commit, &setDigest); err != nil ||
		url == nil || commit == nil || setDigest != nil {
		t.Fatalf("an earlier row keeps its single source: %v %v %v %v", url, commit, setDigest, err)
	}

	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	commits := func(entries ...string) string { return `'[` + strings.Join(entries, ",") + `]'::jsonb` }
	ent := func(repo, c string) string { return `{"repositoryId":"` + repo + `","commit":"` + c + `"}` }
	setCols := `, source_set_digest, source_commits`
	good := commits(ent("repo_api", c1), ent("repo_web", c2))
	if _, err := mig.Exec(ctx, oci("art_set", setCols, `, '`+digest+`', `+good)); err != nil {
		t.Fatalf("a registration with a source set: %v", err)
	}
	expectFail := func(name, sql string) {
		t.Helper()
		if _, err := mig.Exec(ctx, sql); err == nil {
			t.Errorf("%s: statement succeeded:\n%s", name, sql)
		}
	}
	t.Run("the database refuses a malformed source set", func(t *testing.T) {
		expectFail("a digest without commits", oci("art_x1", `, source_set_digest`, `, '`+digest+`'`))
		expectFail("commits without a digest", oci("art_x2", `, source_commits`, `, `+good))
		expectFail("a set beside a single source", oci("art_x3", setCols+`, source_repository_url, source_commit`, `, '`+digest+`', `+good+`, 'https://git.example.test/a.git', '`+c1+`'`))
		expectFail("a bad digest", oci("art_x4", setCols, `, 'sha256:xyz', `+good))
		expectFail("an empty list", oci("art_x5", setCols, `, '`+digest+`', '[]'::jsonb`))
		expectFail("not an array", oci("art_x6", setCols, `, '`+digest+`', '{}'::jsonb`))
		expectFail("unordered repositories", oci("art_x7", setCols, `, '`+digest+`', `+commits(ent("repo_web", c2), ent("repo_api", c1))))
		expectFail("a repository twice", oci("art_x8", setCols, `, '`+digest+`', `+commits(ent("repo_api", c1), ent("repo_api", c2))))
		expectFail("a branch as commit", oci("art_x9", setCols, `, '`+digest+`', `+commits(ent("repo_api", "main"))))
		expectFail("the zero commit", oci("art_x10", setCols, `, '`+digest+`', `+commits(ent("repo_api", strings.Repeat("0", 40)))))
		expectFail("an unknown member", oci("art_x11", setCols, `, '`+digest+`', '[{"repositoryId":"repo_api","commit":"`+c1+`","url":"https://x"}]'::jsonb`))
		expectFail("a bad repository id", oci("art_x12", setCols, `, '`+digest+`', `+commits(ent("repo/api", c1))))
		var many []string
		for i := 0; i < 33; i++ {
			many = append(many, ent("repo_"+strings.Repeat("a", i+1), c1))
		}
		expectFail("more than 32 entries", oci("art_x13", setCols, `, '`+digest+`', `+commits(many...)))
		expectFail("a repository of another project", oci("art_x15", setCols, `, '`+digest+`', `+commits(ent("repo_api", c1), ent("repo_other", c2))))
		expectFail("an OCI registration without any provenance", oci("art_x14", ``, ``))
	})
	t.Run("provenance is immutable, whichever way it would change", func(t *testing.T) {
		expectFail("rewrite the set", `UPDATE artifacts SET source_commits = `+commits(ent("repo_api", c2), ent("repo_web", c2))+` WHERE id = 'art_set'`)
		expectFail("rewrite the digest", `UPDATE artifacts SET source_set_digest = 'sha256:`+strings.Repeat("cd", 32)+`' WHERE id = 'art_set'`)
		expectFail("turn a set into a single source", `UPDATE artifacts SET source_set_digest = NULL, source_commits = NULL, source_repository_url = 'https://git.example.test/a.git', source_commit = '`+c1+`' WHERE id = 'art_set'`)
		expectFail("turn a single source into a set", `UPDATE artifacts SET source_repository_url = NULL, source_commit = NULL, source_set_digest = '`+digest+`', source_commits = `+good+` WHERE id = 'art_old'`)
	})
}
