package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/gitpush/postgres"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func manifest(t *testing.T, push storage.Contribution) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations(), gitauthpg.Migrations(), push, postgres.OperationsMigrations())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Test_migration_reapply: fresh application after 007 and 008, repeated
// application (a no-op verified by checksum), a refused edit and the constraints
// the module relies on.
func Test_migration_reapply(t *testing.T) {
	ctx := context.Background()
	if got := postgres.Migrations().Migrations; len(got) != 1 || got[0].ID != postgres.MigrationID || postgres.MigrationID != "009_git_pushes.sql" {
		t.Fatalf("the contribution is one migration, 009_git_pushes.sql: %+v", got)
	}
	if got := postgres.OperationsMigrations().Migrations; len(got) != 1 || got[0].ID != postgres.OperationsMigrationID || postgres.OperationsMigrationID != "015_git_operations.sql" {
		t.Fatalf("the operations contribution is one migration, 015_git_operations.sql: %+v", got)
	}
	m := manifest(t, postgres.Migrations())
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(ctx, owner, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Migrate(ctx, owner, m, opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := postgres.Migrations()
	changed.Migrations[0].SQL += "\n-- edited after merge"
	if _, err := storage.Migrate(ctx, owner, manifest(t, changed), opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}

	for table, names := range map[string][]string{
		"git_push_records": {"git_push_records_pkey", "git_push_records_receive_unique", "git_push_records_tenant_id_project_id_repository_id_fkey",
			"git_push_records_grant_kind_known", "git_push_records_status_known", "git_push_records_actor_shape", "git_push_records_digest_syntax"},
		"git_push_entries": {"git_push_entries_pkey", "git_push_entries_ref_unique", "git_push_entries_kind_matches", "git_push_entries_reason_matches_state",
			"git_push_entries_values_differ", "git_push_entries_state_known", "git_push_entries_ref_syntax"},
		"git_journal_heads":    {"git_journal_heads_pkey", "git_journal_heads_range"},
		"git_push_resolutions": {"git_push_resolutions_pkey", "git_push_resolutions_known", "git_push_resolutions_reason_length"},
		"git_journal_resets":   {"git_journal_resets_pkey", "git_journal_resets_lowers"},
	} {
		for _, name := range names {
			var n int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`, name, table).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: constraint %s missing (%d, %v)", table, name, n, err)
			}
		}
		var rls, force bool
		var policies int
		if err := owner.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity,
			(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid AND pg_get_expr(p.polqual, p.polrelid) LIKE '%kodama_tenant_id()%')
			FROM pg_class c WHERE c.relname = $1`, table).Scan(&rls, &force, &policies); err != nil {
			t.Fatal(err)
		}
		if !rls || !force || policies < 1 {
			t.Errorf("%s: rls=%v force=%v tenant policies=%d", table, rls, force, policies)
		}
	}
	var idx string
	if err := owner.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'git_push_entries_ref_idx'`).Scan(&idx); err != nil ||
		!strings.Contains(idx, "(tenant_id, repository_id, ref, state)") {
		t.Errorf("the per-ref read index of 015: %q %v", idx, err)
	}
	var fk string
	if err := owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'git_push_records_tenant_id_project_id_repository_id_fkey'`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != "FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories(tenant_id, project_id, id)" {
		t.Errorf("foreign key = %s", fk)
	}
}

// Test_unit_migration_least_privilege: every grant names the runtime role through
// the placeholder, none deletes or grants everything, and the migration never
// touches the baseline or the other contributions' tables.
func Test_unit_migration_least_privilege(t *testing.T) {
	for _, c := range []storage.Contribution{postgres.Migrations(), postgres.OperationsMigrations()} {
		checkLeastPrivilege(t, c.Migrations[0].SQL)
	}
}

func checkLeastPrivilege(t *testing.T, sql string) {
	t.Helper()
	if !strings.Contains(sql, storage.RuntimeRolePlaceholder) {
		t.Fatal("the migration grants nothing to the runtime role")
	}
	for _, m := range regexp.MustCompile(`(?m)^GRANT\b[^;]*;`).FindAllString(sql, -1) {
		if !strings.Contains(m, storage.RuntimeRolePlaceholder) {
			t.Errorf("a grant that does not name the runtime role: %s", m)
		}
		for _, bad := range []string{"DELETE", "ALL", "TRUNCATE", "PUBLIC", "WITH GRANT OPTION"} {
			if regexp.MustCompile(`(?i)\b` + bad + `\b`).MatchString(m) {
				t.Errorf("grant mentions %s: %s", bad, m)
			}
		}
	}
	for _, forbidden := range []string{"ALTER TABLE projects", "ALTER TABLE repositories", "ALTER TABLE git_grants", "DROP ", "INSERT INTO projects", "001_schema"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("the migration touches another contribution: %s", forbidden)
		}
	}
}

// Test_migration_015_backfills_apply_order: a database that applied 009 and
// recorded applied entries before 015 gets them numbered (by the record's last
// change, then push ID and ordinal), the constraint holds, and the sequence
// continues after them for the next entry that applies.
func Test_migration_015_backfills_apply_order(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	before, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations(), gitauthpg.Migrations(), postgres.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, before, opts); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO tenants (id, key) VALUES ('t013', 't013')`)
	exec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ('t013', 'p013', 'p013', 5)`)
	exec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
		VALUES ('t013', 'p013', 'r013', 'r013', 'main', 'internal', 'ready', 'user', 'usr_fixture', now(), now())`)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	record := func(id string, updated time.Duration, entries ...[3]string) {
		exec(`INSERT INTO git_push_records (tenant_id, project_id, id, repository_id, receive_id, grant_id, grant_kind, actor_type, actor_id, repository_empty,
			request_digest, deadline, status, version, created_at, updated_at)
			VALUES ('t013', 'p013', $1, 'r013', $1, 'gr1', 'human', 'user', 'usr_1', false, $2, $3, 'applied', 2, $4, $5)`,
			id, digest, base.Add(24*time.Hour), base, base.Add(updated))
		for i, e := range entries { // ref, state, reason
			var reason *string
			if e[2] != "" {
				reason = &e[2]
			}
			exec(`INSERT INTO git_push_entries (tenant_id, push_id, ordinal, project_id, repository_id, ref, old_value, new_value, kind, state, reason)
				VALUES ('t013', $1, $2, 'p013', 'r013', $3, $4, $5, 'create', 'authorized', NULL)`, id, i, e[0], strings.Repeat("0", 40), strings.Repeat("1", 40))
			if e[1] == "applied" {
				exec(`UPDATE git_push_entries SET state = 'committing' WHERE push_id = $1 AND ordinal = $2`, id, i)
				exec(`UPDATE git_push_entries SET state = 'applied' WHERE push_id = $1 AND ordinal = $2`, id, i)
			} else {
				exec(`UPDATE git_push_entries SET state = 'refused', reason = $3 WHERE push_id = $1 AND ordinal = $2`, id, i, *reason)
			}
		}
	}
	// Last changes: B before A before C; A has two applied entries and a refused one.
	record("push_b", time.Minute, [3]string{"refs/heads/b", "applied", ""})
	record("push_a", 2*time.Minute, [3]string{"refs/heads/a1", "applied", ""}, [3]string{"refs/heads/a2", "refused", "no_commit_intent"}, [3]string{"refs/heads/a3", "applied", ""})
	record("push_c", 3*time.Minute, [3]string{"refs/heads/c", "applied", ""})

	full, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations(), gitauthpg.Migrations(), postgres.Migrations(), postgres.OperationsMigrations())
	if err != nil {
		t.Fatal(err)
	}
	res, err := storage.Migrate(ctx, owner, full, opts)
	if err != nil || len(res.Applied) != 1 || res.Applied[0] != postgres.OperationsMigrationID {
		t.Fatalf("015 on a database with applied entries: %+v %v", res, err)
	}
	seqs := map[string]int64{}
	rows, err := owner.Query(ctx, `SELECT ref, applied_seq FROM git_push_entries WHERE state = 'applied' ORDER BY applied_seq`)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for rows.Next() {
		var ref string
		var seq *int64
		if err := rows.Scan(&ref, &seq); err != nil || seq == nil {
			t.Fatalf("an applied entry without an order: %v", err)
		}
		seqs[ref] = *seq
		order = append(order, ref)
	}
	rows.Close()
	if strings.Join(order, ",") != "refs/heads/b,refs/heads/a1,refs/heads/a3,refs/heads/c" || seqs["refs/heads/b"] != 1 || seqs["refs/heads/c"] != 4 {
		t.Fatalf("backfilled order %v %v", order, seqs)
	}
	var refusedOrder *int64
	if err := owner.QueryRow(ctx, `SELECT applied_seq FROM git_push_entries WHERE state = 'refused'`).Scan(&refusedOrder); err != nil || refusedOrder != nil {
		t.Fatalf("a refused entry has an order: %v %v", refusedOrder, err)
	}
	// The sequence continues after the backfill.
	exec(`INSERT INTO git_push_records (tenant_id, project_id, id, repository_id, receive_id, grant_id, grant_kind, actor_type, actor_id, repository_empty,
		request_digest, deadline, status, version, created_at, updated_at)
		VALUES ('t013', 'p013', 'push_d', 'r013', 'push_d', 'gr1', 'human', 'user', 'usr_1', false, $1, $2, 'authorized', 1, $3, $3)`, digest, base.Add(24*time.Hour), base)
	exec(`INSERT INTO git_push_entries (tenant_id, push_id, ordinal, project_id, repository_id, ref, old_value, new_value, kind, state)
		VALUES ('t013', 'push_d', 0, 'p013', 'r013', 'refs/heads/d', $1, $2, 'create', 'authorized')`, strings.Repeat("0", 40), strings.Repeat("1", 40))
	exec(`UPDATE git_push_entries SET state = 'committing' WHERE push_id = 'push_d'`)
	exec(`UPDATE git_push_entries SET state = 'applied' WHERE push_id = 'push_d'`)
	var next int64
	if err := owner.QueryRow(ctx, `SELECT applied_seq FROM git_push_entries WHERE push_id = 'push_d'`).Scan(&next); err != nil || next != 5 {
		t.Fatalf("the next entry to apply is numbered %d (%v), want 5", next, err)
	}
}
