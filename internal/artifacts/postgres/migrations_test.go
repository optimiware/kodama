package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var tables = []string{"artifacts", "artifact_objects", "artifact_pins"}

// TestMigrationReapply: fresh application of the baseline, reapply, constraints,
// immutability guards and runtime grants on a real PostgreSQL.
func TestMigrationReapply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgtest.New(t)
	mig := db.MigrationPool(ctx, 4)

	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Applied, []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql"}) {
		t.Fatalf("the baseline is three migrations, applied %v", first.Applied)
	}
	second, err := storage.Migrate(ctx, mig, m, opts)
	if err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply applied %v skipped %d", second.Applied, second.Skipped)
	}

	// The baseline installs the execution link; this module's test creates no
	// attempts, so it asserts the constraint and then isolates the artifact
	// schema. The execution composition tests exercise the link itself.
	var linked bool
	if err := mig.QueryRow(ctx, `SELECT count(*) = 1 FROM pg_constraint WHERE conname = 'artifacts_producer_attempt_fk'`).Scan(&linked); err != nil || !linked {
		t.Fatalf("the producer attempt link is not installed: %v %v", linked, err)
	}
	if _, err := mig.Exec(ctx, `ALTER TABLE artifacts DROP CONSTRAINT artifacts_producer_attempt_fk`); err != nil {
		t.Fatal(err)
	}

	one := func(sql string, args ...any) (v bool) {
		t.Helper()
		if err := mig.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
		return v
	}

	t.Run("row level security is enabled and forced", func(t *testing.T) {
		for _, tbl := range tables {
			if !one(`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tbl) {
				t.Errorf("%s: RLS not enabled and forced", tbl)
			}
		}
	})

	t.Run("runtime grants are minimal", func(t *testing.T) {
		for _, tbl := range tables {
			if one(`SELECT has_table_privilege($1, $2, 'TRUNCATE')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role may TRUNCATE", tbl)
			}
			if !one(`SELECT has_table_privilege($1, $2, 'SELECT')`, db.RuntimeRole, tbl) || !one(`SELECT has_table_privilege($1, $2, 'INSERT')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role lacks SELECT/INSERT", tbl)
			}
			// Only pins can be released by deletion; metadata and object records
			// are retained.
			if tbl != "artifact_pins" && one(`SELECT has_table_privilege($1, $2, 'DELETE')`, db.RuntimeRole, tbl) {
				t.Errorf("%s: runtime role may DELETE", tbl)
			}
		}
		for tbl, cols := range map[string][]string{
			"artifacts": {"tenant_id", "project_id", "id", "run_id", "attempt_id", "step_id", "name", "kind", "role", "digest", "media_type", "size_bytes",
				"registry", "source_repository_url", "source_commit", "staging_key", "created_at"},
			"artifact_objects": {"key", "purpose", "generation", "artifact_id", "created_at"},
			"artifact_pins":    {"owner_kind", "owner_id", "created_at"},
		} {
			for _, c := range cols {
				if one(`SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, tbl, c) {
					t.Errorf("%s.%s is updatable by the runtime role", tbl, c)
				}
			}
		}
		for tbl, cols := range map[string][]string{
			"artifacts":        {"state", "disposition", "sealed_key", "transfer_expires_at", "claim_token", "claim_generation", "claim_until", "version"},
			"artifact_objects": {"state", "winner", "writable_until", "delete_token", "deleted_at"},
		} {
			for _, c := range cols {
				if !one(`SELECT has_column_privilege($1, $2, $3, 'UPDATE')`, db.RuntimeRole, tbl, c) {
					t.Errorf("%s.%s is not updatable by the runtime role", tbl, c)
				}
			}
		}
	})

	seed := `INSERT INTO tenants (id, key) VALUES ('t_a', 'a'), ('t_b', 'b');
INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t_a', 'p_1', 'p1', 3), ('t_a', 'p_2', 'p2', 3), ('t_b', 'p_9', 'p9', 3);`
	if _, err := mig.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	digest := `sha256:` + strings.Repeat("ab", 32)
	file := func(id, name string) string {
		return `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, staging_key,
 transfer_expires_at, state, created_at, updated_at, version)
VALUES ('t_a', 'p_1', '` + id + `', 'run_1', 'att_1', 'build', '` + name + `', 'file', 'output', '` + digest + `', 'text/plain', 5, 'artifacts/t_a/p_1/` + id + `/staging/n',
 now(), 'reserved', now(), now(), 1)`
	}
	if _, err := mig.Exec(ctx, file("art_1", "report")); err != nil {
		t.Fatalf("a valid reservation: %v", err)
	}
	expectFail := func(name, sql string) {
		t.Helper()
		if _, err := mig.Exec(ctx, sql); err == nil {
			t.Errorf("%s: statement succeeded:\n%s", name, sql)
		}
	}
	ociRow := func(id, name, commit string) string {
		return `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, registry, source_repository_url, source_commit,
 state, verified_at, verified_size_bytes, created_at, updated_at, version)
VALUES ('t_a', 'p_1', '` + id + `', 'run_1', 'att_1', 'build', '` + name + `', 'oci', 'output', '` + digest + `', 'registry.example.test/team/app', 'https://git.example.test/a.git', ` + commit + `,
 'registered', now(), 10, now(), now(), 1)`
	}

	t.Run("constraints reject invalid rows", func(t *testing.T) {
		expectFail("duplicate (attempt, name)", file("art_dup", "report"))
		expectFail("same name in another attempt is fine but unknown project is not", strings.Replace(file("art_x", "other"), `'p_1'`, `'p_404'`, 1))
		expectFail("project of another tenant", strings.Replace(file("art_x", "other"), `'t_a', 'p_1'`, `'t_a', 'p_9'`, 1))
		expectFail("bad name", file("art_x", "../evil"))
		expectFail("bad digest", strings.Replace(file("art_x", "n"), digest, "sha256:xyz", 1))
		expectFail("negative size", strings.Replace(file("art_x", "n"), "'text/plain', 5", "'text/plain', -1", 1))
		expectFail("oversized", strings.Replace(file("art_x", "n"), "'text/plain', 5", "'text/plain', 1099511627777", 1))
		expectFail("unknown kind", strings.Replace(file("art_x", "n"), `'file', 'output'`, `'dir', 'output'`, 1))
		expectFail("unknown role", strings.Replace(file("art_x", "n"), `'file', 'output'`, `'file', 'secret'`, 1))
		expectFail("unknown state", strings.Replace(file("art_x", "n"), `'reserved'`, `'finished'`, 1))
		expectFail("usable without a seal", strings.Replace(file("art_x", "n"), `'reserved'`, `'usable'`, 1))
		expectFail("file without media type", strings.Replace(file("art_x", "n"), "'text/plain'", "NULL", 1))
		expectFail("file without staging key", strings.Replace(file("art_x", "n"), `'artifacts/t_a/p_1/art_x/staging/n'`, "NULL", 1))
		expectFail("staging key escaping the namespace", strings.Replace(file("art_x", "n"), `'artifacts/t_a/p_1/art_x/staging/n'`, `'../../etc/passwd'`, 1))
		expectFail("file with a registry", strings.Replace(file("art_x", "n"), `'text/plain', 5`, `'text/plain', 5, 'r.example/x'`, 1))
		expectFail("oci without provenance", ociRow("art_o", "img", "NULL"))
		expectFail("half provenance", strings.Replace(file("art_x", "n"), `staging_key,`, `source_commit, staging_key,`, 1))
		expectFail("provenance with credentials", strings.Replace(ociRow("art_o", "img", `'`+strings.Repeat("a", 40)+`'`), "https://git.example.test/a.git", "https://u:p@git.example.test/a.git", 1))
		expectFail("branch name as commit", ociRow("art_o", "img", `'main'`))
		expectFail("oci diagnostic", strings.Replace(ociRow("art_o", "img", `'`+strings.Repeat("a", 40)+`'`), `'oci', 'output'`, `'oci', 'diagnostic'`, 1))
		if _, err := mig.Exec(ctx, ociRow("art_oci", "img", `'`+strings.Repeat("a", 40)+`'`)); err != nil {
			t.Fatalf("a valid verified registration: %v", err)
		}
		expectFail("object of another project", `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, writable_until, created_at)
VALUES ('t_a', 'p_2', 'art_1', 'artifacts/t_a/p_2/art_1/staging/n', 'staging', now(), now())`)
		expectFail("object with an unknown purpose", `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, writable_until, created_at)
VALUES ('t_a', 'p_1', 'art_1', 'artifacts/t_a/p_1/art_1/other/n', 'other', now(), now())`)
		expectFail("staging winner", `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, winner, writable_until, created_at)
VALUES ('t_a', 'p_1', 'art_1', 'artifacts/t_a/p_1/art_1/staging/w', 'staging', true, now(), now())`)
		expectFail("pin of an unknown artifact", `INSERT INTO artifact_pins (tenant_id, project_id, artifact_id, owner_kind, owner_id, created_at)
VALUES ('t_a', 'p_1', 'art_nope', 'gate', 'g_1', now())`)
	})

	if _, err := mig.Exec(ctx, `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, writable_until, created_at)
VALUES ('t_a', 'p_1', 'art_1', 'artifacts/t_a/p_1/art_1/staging/n', 'staging', now(), now())`); err != nil {
		t.Fatal(err)
	}

	t.Run("immutability guards hold even for the owner", func(t *testing.T) {
		expectFail("rename", `UPDATE artifacts SET name = 'x' WHERE id = 'art_1'`)
		expectFail("re-point digest", `UPDATE artifacts SET digest = 'sha256:`+strings.Repeat("cd", 32)+`' WHERE id = 'art_1'`)
		expectFail("move attempt", `UPDATE artifacts SET attempt_id = 'att_2' WHERE id = 'art_1'`)
		expectFail("move run", `UPDATE artifacts SET run_id = 'run_2' WHERE id = 'art_1'`)
		expectFail("move project", `UPDATE artifacts SET project_id = 'p_2' WHERE id = 'art_1'`)
		expectFail("rewrite size", `UPDATE artifacts SET size_bytes = 6 WHERE id = 'art_1'`)
		expectFail("rewrite provenance", `UPDATE artifacts SET source_repository_url = 'https://x.example/y', source_commit = '`+strings.Repeat("a", 40)+`' WHERE id = 'art_1'`)
		expectFail("rewrite staging key", `UPDATE artifacts SET staging_key = 'artifacts/other' WHERE id = 'art_1'`)
		expectFail("delete", `DELETE FROM artifacts WHERE id = 'art_1'`)
		expectFail("state skips verification", `UPDATE artifacts SET state = 'usable', sealed_key = 'artifacts/s', version = 2 WHERE id = 'art_1'`)
		expectFail("lifecycle change without a version bump", `UPDATE artifacts SET disposition = 'abandoned' WHERE id = 'art_1'`)
		expectFail("version change without lifecycle", `UPDATE artifacts SET version = 2 WHERE id = 'art_1'`)
		expectFail("transfer expiry moving back", `UPDATE artifacts SET transfer_expires_at = now() - interval '1 day' WHERE id = 'art_1'`)
		expectFail("skipping a claim generation", `UPDATE artifacts SET claim_generation = 5, claim_token = 't', claim_until = now() WHERE id = 'art_1'`)
		expectFail("claim without expiry", `UPDATE artifacts SET claim_generation = 1, claim_token = 't' WHERE id = 'art_1'`)

		// A claim, then the legal completion.
		if _, err := mig.Exec(ctx, `UPDATE artifacts SET claim_generation = 1, claim_token = 't', claim_until = now() + interval '1 minute' WHERE id = 'art_1'`); err != nil {
			t.Fatalf("claim: %v", err)
		}
		expectFail("lowering a claim generation", `UPDATE artifacts SET claim_generation = 0 WHERE id = 'art_1'`)
		if _, err := mig.Exec(ctx, `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, generation, writable_until, created_at)
VALUES ('t_a', 'p_1', 'art_1', 'artifacts/t_a/p_1/art_1/sealed/c1', 'candidate', 1, now(), now())`); err != nil {
			t.Fatal(err)
		}
		expectFail("usable at a size other than declared", `UPDATE artifacts SET state = 'usable', sealed_key = 'artifacts/t_a/p_1/art_1/sealed/c1', verified_at = now(), verified_size_bytes = 4, version = 2 WHERE id = 'art_1'`)
		expectFail("usable at the staging key", `UPDATE artifacts SET state = 'usable', sealed_key = staging_key, verified_at = now(), verified_size_bytes = 5, version = 2 WHERE id = 'art_1'`)
		if _, err := mig.Exec(ctx, `UPDATE artifacts SET state = 'usable', sealed_key = 'artifacts/t_a/p_1/art_1/sealed/c1', verified_at = now(), verified_size_bytes = 5, claim_token = NULL, claim_until = NULL, version = 2 WHERE id = 'art_1'`); err != nil {
			t.Fatalf("a proper completion: %v", err)
		}
		expectFail("change the seal", `UPDATE artifacts SET sealed_key = 'artifacts/t_a/p_1/art_1/sealed/other' WHERE id = 'art_1'`)
		expectFail("back to reserved", `UPDATE artifacts SET state = 'reserved', sealed_key = NULL, verified_at = NULL, verified_size_bytes = NULL, version = 3 WHERE id = 'art_1'`)
		expectFail("abandon usable content", `UPDATE artifacts SET disposition = 'abandoned', version = 3 WHERE id = 'art_1'`)
		expectFail("rewrite verification evidence", `UPDATE artifacts SET verified_at = now() - interval '1 day' WHERE id = 'art_1'`)

		// Object records.
		expectFail("delete an object record", `DELETE FROM artifact_objects WHERE artifact_id = 'art_1'`)
		expectFail("move an object to another artifact", `UPDATE artifact_objects SET artifact_id = 'art_oci' WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`)
		expectFail("rewrite an object key", `UPDATE artifact_objects SET key = 'artifacts/x' WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`)
		expectFail("delete a live object directly", `UPDATE artifact_objects SET state = 'deleted', deleted_at = now() WHERE key = 'artifacts/t_a/p_1/art_1/staging/n'`)
	})

	t.Run("the winner is protected until retention removes the artifact", func(t *testing.T) {
		if _, err := mig.Exec(ctx, `UPDATE artifact_objects SET winner = true WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`); err != nil {
			// Setting the winner flag on a live candidate is legal (commit does it).
			t.Fatalf("set winner: %v", err)
		}
		expectFail("second winner", `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, generation, winner, writable_until, created_at)
VALUES ('t_a', 'p_1', 'art_1', 'artifacts/t_a/p_1/art_1/sealed/c2', 'candidate', 2, true, now(), now())`)
		expectFail("unset the winner", `UPDATE artifact_objects SET winner = false WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`)
		expectFail("schedule the winner for deletion while the artifact is active", `UPDATE artifact_objects SET state = 'delete_pending', delete_requested_at = now() WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`)
		if _, err := mig.Exec(ctx, `UPDATE artifacts SET disposition = 'removed', version = 3 WHERE id = 'art_1'`); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if _, err := mig.Exec(ctx, `UPDATE artifact_objects SET state = 'delete_pending', delete_requested_at = now() WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`); err != nil {
			t.Fatalf("after retention removed the artifact: %v", err)
		}
		expectFail("revive a candidate", `UPDATE artifact_objects SET state = 'live' WHERE key = 'artifacts/t_a/p_1/art_1/sealed/c1'`)
		expectFail("reopen a removed artifact", `UPDATE artifacts SET disposition = 'active', version = 4 WHERE id = 'art_1'`)
		// A staging record may be reopened by a renewed transfer.
		if _, err := mig.Exec(ctx, `UPDATE artifact_objects SET state = 'delete_pending', delete_requested_at = now() WHERE key = 'artifacts/t_a/p_1/art_1/staging/n'`); err != nil {
			t.Fatal(err)
		}
		if _, err := mig.Exec(ctx, `UPDATE artifact_objects SET state = 'live' WHERE key = 'artifacts/t_a/p_1/art_1/staging/n'`); err != nil {
			t.Fatalf("a renewed transfer reopens staging: %v", err)
		}
		expectFail("writable horizon moving back", `UPDATE artifact_objects SET writable_until = now() - interval '1 day' WHERE key = 'artifacts/t_a/p_1/art_1/staging/n'`)
	})

	t.Run("owner can read and repeat does not change data", func(t *testing.T) {
		if _, err := storage.Migrate(ctx, mig, m, opts); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := mig.QueryRow(ctx, `SELECT count(*) FROM artifacts`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("artifacts after reapply: %d %v", n, err)
		}
	})
}
