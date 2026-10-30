package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
	"github.com/optimiware/kodama/internal/gitops/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Test_migration_reapply: fresh application after the other contributions,
// repeated application (a no-op verified by checksum), a refused edit and the
// constraints, forced row level security and tenant policies of every table.
func Test_migration_reapply(t *testing.T) {
	ctx := context.Background()
	if got := postgres.Migrations().Migrations; len(got) != 1 || got[0].ID != postgres.MigrationID || postgres.MigrationID != "016_git_recovery.sql" {
		t.Fatalf("the contribution is one migration, 016_git_recovery.sql: %+v", got)
	}
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
	first, err := storage.Migrate(ctx, owner, manifest(t, postgres.Migrations()), opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Migrate(ctx, owner, manifest(t, postgres.Migrations()), opts)
	if err != nil || len(second.Applied) != 0 || second.Skipped != len(first.Applied) {
		t.Fatalf("reapply: %+v %v", second, err)
	}
	changed := postgres.Migrations()
	changed.Migrations[0].SQL += "\n-- edited after merge"
	if _, err := storage.Migrate(ctx, owner, manifest(t, changed), opts); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("a modified migration: %v", err)
	}
	for table, names := range map[string][]string{
		"git_backup_runs":          {"git_backup_runs_pkey", "git_backup_runs_state_known", "git_backup_runs_tenant_id_fkey"},
		"git_recoveries":           {"git_recoveries_pkey", "git_recoveries_status_known", "git_recoveries_source", "git_recoveries_completed_matches", "git_recoveries_tenant_id_fkey"},
		"git_recovery_replays":     {"git_recovery_replays_pkey", "git_recovery_replays_tenant_id_recovery_id_fkey"},
		"git_recovery_resolutions": {"git_recovery_resolutions_pkey", "git_recovery_resolutions_kind_known", "git_recovery_resolutions_shape", "git_recovery_resolutions_tenant_id_recovery_id_fkey"},
		"git_prune_intents":        {"git_prune_intents_pkey", "git_prune_intents_tip_syntax", "git_prune_intents_tenant_id_project_id_repository_id_fkey"},
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
	var fk string
	if err := owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'git_prune_intents_tenant_id_project_id_repository_id_fkey'`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != "FOREIGN KEY (tenant_id, project_id, repository_id) REFERENCES repositories(tenant_id, project_id, id)" {
		t.Errorf("foreign key = %s", fk)
	}
	for _, idx := range []string{"git_backup_runs_one_active", "git_recoveries_one_active"} {
		var def string
		if err := owner.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, idx).Scan(&def); err != nil || !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "WHERE") {
			t.Errorf("index %s: %q %v", idx, def, err)
		}
	}
}

// Test_unit_migration_least_privilege: every grant names the runtime role through
// the placeholder, none deletes or grants everything, and the migration never
// touches the other contributions' tables.
func Test_unit_migration_least_privilege(t *testing.T) {
	sql := postgres.Migrations().Migrations[0].SQL
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
	for _, forbidden := range []string{"ALTER TABLE projects", "ALTER TABLE repositories", "ALTER TABLE git_push", "ALTER TABLE git_grants", "DROP ", "001_schema"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("the migration touches another contribution: %s", forbidden)
		}
	}
}

func run(ctx context.Context, e *pgEnv, tenant domain.TenantID, sql string, args ...any) error {
	return storage.WithTenant(ctx, e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func count(t *testing.T, e *pgEnv, tenant domain.TenantID, sql string, args ...any) int {
	t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Test_postgres_gitops_guards: row level security, the composite ownership
// keys, the runtime role's least privilege and the database-enforced invariants
// of leases, recoveries, resolutions and intents, against rows the stores wrote.
func Test_postgres_gitops_guards(t *testing.T) {
	ctx := context.Background()
	layerSkip(t)
	e := newPGEnv(t)
	A, B := gitopstest.ContractTenantA, gitopstest.ContractTenantB
	sa, sb := gitopstest.ContractScopeA(), gitopstest.ContractScopeB()
	e.Seed(t, sa, gitopstest.ContractRepo1)
	e.Seed(t, sb, gitopstest.ContractRepo1)
	now := e.clock.Now()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.Leases().Acquire(ctx, A, "bk_g1", time.Hour))
	must(e.Leases().Complete(ctx, A, "bk_g1"))
	must(e.Leases().Acquire(ctx, A, "bk_g2", time.Hour))
	rec, err := e.Recoveries().Begin(ctx, A, "rcv_g1", gitops.RestoreSource{SnapshotID: "snap-g", BackupID: "bk_g1"}, now)
	must(err)
	must(e.Recoveries().MarkVolumeRestored(ctx, A, rec.ID, now))
	must(e.Recoveries().MarkReplayed(ctx, A, rec.ID, gitopstest.ContractRepo1, gitops.ReplayResult{Created: 1}, now))
	d := gitops.Discrepancy{Repository: gitopstest.ContractRepo1, Ref: "refs/heads/main", Kind: gitops.DiscrepancyExtra, Actual: strings.Repeat("a", 40)}
	must(e.Recoveries().Resolve(ctx, A, rec.ID, gitops.DiscrepancyResolution{Discrepancy: d, By: "usr_operator", Reason: "kept", At: now}))
	must(e.Intents().Declare(ctx, sa, gitopstest.ContractRepo1, "refs/heads/kodama/work/wi_1", strings.Repeat("b", 40), "pruner_g", time.Hour))

	tables := []string{"git_backup_runs", "git_recoveries", "git_recovery_replays", "git_recovery_resolutions", "git_prune_intents"}
	t.Run("RLS hides every row from other tenants and from no tenant", func(t *testing.T) {
		for _, table := range tables {
			var n int
			if err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s without tenant context: %d rows, %v", table, n, err)
			}
			if got := count(t, e, B, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, string(A)); got != 0 {
				t.Fatalf("tenant B sees %d %s rows of tenant A", got, table)
			}
			if got := count(t, e, A, `SELECT count(*) FROM `+table); got == 0 {
				t.Fatalf("tenant A sees no %s rows", table)
			}
		}
	})

	t.Run("a tenant cannot write another tenant's rows", func(t *testing.T) {
		if err := run(ctx, e, B, `INSERT INTO git_backup_runs (tenant_id, backup_id, lease_expires_at, started_at, updated_at) VALUES ($1, 'bk_x', now(), now(), now())`, string(A)); sqlState(err) != "42501" {
			t.Fatalf("a foreign insert: %v", err)
		}
		if err := run(ctx, e, B, `UPDATE git_backup_runs SET state = 'failed' WHERE tenant_id = $1`, string(A)); err != nil {
			t.Fatal(err)
		}
		if state, known, _ := e.Leases().State(ctx, A, "bk_g2"); !known || state != gitops.BackupActive {
			t.Fatalf("another tenant changed the run: %v", state)
		}
	})

	t.Run("the runtime role never deletes and never edits identity", func(t *testing.T) {
		for _, table := range tables {
			if err := run(ctx, e, A, `DELETE FROM `+table+` WHERE tenant_id = $1`, string(A)); sqlState(err) != "42501" {
				t.Fatalf("delete from %s: %v", table, err)
			}
		}
		for _, stmt := range []string{
			`UPDATE git_backup_runs SET backup_id = 'bk_other' WHERE tenant_id = $1`,
			`UPDATE git_recoveries SET snapshot_id = 'snap-other' WHERE tenant_id = $1`,
			`UPDATE git_recovery_replays SET created = 9 WHERE tenant_id = $1`,
			`UPDATE git_recovery_resolutions SET reason = 'edited' WHERE tenant_id = $1`,
			`UPDATE git_prune_intents SET ref = 'refs/heads/kodama/work/other' WHERE tenant_id = $1`,
		} {
			if err := run(ctx, e, A, stmt, string(A)); sqlState(err) != "42501" {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	})

	t.Run("a backup run is final once complete or failed", func(t *testing.T) {
		for _, id := range []string{"bk_g1"} {
			if err := run(ctx, e, A, `UPDATE git_backup_runs SET state = 'active' WHERE tenant_id = $1 AND backup_id = $2`, string(A), id); sqlState(err) != "23514" {
				t.Fatalf("reviving %s: %v", id, err)
			}
			if err := run(ctx, e, A, `UPDATE git_backup_runs SET state = 'failed' WHERE tenant_id = $1 AND backup_id = $2`, string(A), id); sqlState(err) != "23514" {
				t.Fatalf("failing a complete run %s: %v", id, err)
			}
		}
		if err := run(ctx, e, A, `UPDATE git_backup_runs SET state = 'bogus' WHERE tenant_id = $1 AND backup_id = 'bk_g2'`, string(A)); sqlState(err) == "" {
			t.Fatal("an unknown state was accepted")
		}
		// A second active run is refused by the index even through a direct insert.
		if err := run(ctx, e, A, `INSERT INTO git_backup_runs (tenant_id, backup_id, lease_expires_at, started_at, updated_at) VALUES ($1, 'bk_g3', now(), now(), now())`, string(A)); sqlState(err) != "23505" {
			t.Fatalf("a second active run: %v", err)
		}
	})

	t.Run("recovery source is immutable and a complete recovery is final", func(t *testing.T) {
		if _, err := e.Owner.Exec(ctx, `UPDATE git_recoveries SET snapshot_id = 'snap-other' WHERE tenant_id = $1`, string(A)); sqlState(err) != "23514" {
			t.Fatalf("changing the source as the owner: %v", err)
		}
		if _, err := e.Owner.Exec(ctx, `UPDATE git_recoveries SET volume_restored = false WHERE tenant_id = $1`, string(A)); sqlState(err) != "23514" {
			t.Fatalf("un-restoring the volume: %v", err)
		}
		// Rows that are evidence cannot be edited even by the owner.
		if _, err := e.Owner.Exec(ctx, `UPDATE git_recovery_resolutions SET reason = 'edited' WHERE tenant_id = $1`, string(A)); sqlState(err) != "23514" {
			t.Fatalf("editing a resolution: %v", err)
		}
		if _, err := e.Owner.Exec(ctx, `DELETE FROM git_recovery_replays WHERE tenant_id = $1`, string(A)); sqlState(err) != "23514" {
			t.Fatalf("deleting a replay: %v", err)
		}
		// A recovery without a restored volume does not complete.
		if _, err := e.Recoveries().Begin(ctx, B, "rcv_g2", gitops.RestoreSource{SnapshotID: "snap-b"}, now); err != nil {
			t.Fatal(err)
		}
		if err := run(ctx, e, B, `UPDATE git_recoveries SET status = 'complete', completed_at = now() WHERE tenant_id = $1`, string(B)); sqlState(err) != "23514" {
			t.Fatalf("completing before the volume restore: %v", err)
		}
		if err := e.Recoveries().Complete(ctx, A, rec.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := run(ctx, e, A, `UPDATE git_recoveries SET updated_at = now() WHERE tenant_id = $1`, string(A)); sqlState(err) != "23514" {
			t.Fatalf("changing a complete recovery: %v", err)
		}
		// The steps of a complete recovery are refused by the database, not only by the store.
		if err := run(ctx, e, A, `INSERT INTO git_recovery_replays (tenant_id, recovery_id, repository_id, created, unchanged, completed_at) VALUES ($1, $2, 'r_late', 0, 0, now())`, string(A), rec.ID); sqlState(err) != "23514" {
			t.Fatalf("a replay of a complete recovery: %v", err)
		}
		if err := run(ctx, e, A, `INSERT INTO git_recovery_resolutions (tenant_id, recovery_id, repository_id, ref, kind, expected, actual, resolved_by, reason, resolved_at)
			VALUES ($1, $2, 'r_late', 'refs/heads/x', 'extra', '', $3, 'usr', 'late', now())`, string(A), rec.ID, strings.Repeat("c", 40)); sqlState(err) != "23514" {
			t.Fatalf("a resolution of a complete recovery: %v", err)
		}
		// Another tenant's recovery is not a parent.
		if err := run(ctx, e, B, `INSERT INTO git_recovery_replays (tenant_id, recovery_id, repository_id, created, unchanged, completed_at) VALUES ($1, $2, 'r_x', 0, 0, now())`, string(B), rec.ID); sqlState(err) != "23503" && sqlState(err) != "23514" {
			t.Fatalf("a replay under another tenant's recovery: %v", err)
		}
	})

	t.Run("a resolution carries its audit row once", func(t *testing.T) {
		if n := count(t, e, A, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2 AND resource_type = 'git_recovery' AND resource_id = $3 AND actor_id = 'usr_operator' AND reason = 'kept'
			AND metadata ->> 'ref' = 'refs/heads/main' AND metadata ->> 'repositoryId' = $4`, string(A), postgres.AuditResolution, rec.ID, gitopstest.ContractRepo1); n != 1 {
			t.Fatalf("audit rows: %d", n)
		}
		// A repeated resolution (even by a second operator) is recorded and audited once.
		db := gitops.Discrepancy{Repository: gitopstest.ContractRepo1, Ref: "refs/heads/old", Kind: gitops.DiscrepancyMissing, Expected: strings.Repeat("f", 40)}
		for _, by := range []string{"usr_one", "usr_two"} {
			must(e.Recoveries().Resolve(ctx, B, "rcv_g2", gitops.DiscrepancyResolution{Discrepancy: db, By: by, Reason: "accepted", At: now}))
		}
		if n := count(t, e, B, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2 AND resource_id = 'rcv_g2'`, string(B), postgres.AuditResolution); n != 1 {
			t.Fatalf("a repeated resolution audited %d times", n)
		}
		if n := count(t, e, B, `SELECT count(*) FROM git_recovery_resolutions WHERE tenant_id = $1 AND resolved_by = 'usr_one'`, string(B)); n != 1 {
			t.Fatalf("the first resolution stands: %d", n)
		}
		// A resolution the database refuses (the recovery is complete) leaves neither
		// its row nor an audit row: they are one transaction.
		before := count(t, e, A, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'git_recovery'`, string(A))
		if err := e.Recoveries().Resolve(ctx, A, rec.ID, gitops.DiscrepancyResolution{Discrepancy: d, By: "usr_two", Reason: "again", At: now}); err == nil {
			t.Fatal("a resolution of a complete recovery was recorded")
		}
		if after := count(t, e, A, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'git_recovery'`, string(A)); after != before {
			t.Fatalf("a refused resolution left an audit row (%d -> %d)", before, after)
		}
	})

	t.Run("prune intents are owned by their repository's project", func(t *testing.T) {
		wrong := domain.Scope{TenantID: A, ProjectID: "p138c_other"}
		e.Seed(t, wrong)
		if err := e.Intents().Declare(ctx, wrong, gitopstest.ContractRepo1, "refs/heads/kodama/work/wi_9", strings.Repeat("d", 40), "pruner_g", time.Hour); sqlState(err) != "23503" {
			t.Fatalf("an intent under another project: %v", err)
		}
		if err := e.Intents().Declare(ctx, sa, gitopstest.ContractRepo1, "refs/heads/kodama/work/wi_9", "not-a-commit", "pruner_g", time.Hour); sqlState(err) != "23514" {
			t.Fatalf("a malformed tip: %v", err)
		}
		// Tenant B has a repository of the same ID: its intents are its own.
		must(e.Intents().Declare(ctx, sb, gitopstest.ContractRepo1, "refs/heads/kodama/work/wi_1", strings.Repeat("e", 40), "pruner_g", time.Hour))
		if c, err := e.Intents().Conflicts(ctx, sa, gitopstest.ContractRepo1, "refs/heads/kodama/work/wi_77", strings.Repeat("e", 40)); err != nil || c {
			t.Fatalf("tenant B's intent reserves tenant A's commit: %v %v", c, err)
		}
	})
}

// Test_postgres_gitops_concurrency: two instances racing for the backup lease
// and for the recovery source resolve to one winner, in the database.
func Test_postgres_gitops_concurrency(t *testing.T) {
	ctx := context.Background()
	layerSkip(t)
	e := newPGEnv(t)
	A := gitopstest.ContractTenantA

	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := postgres.NewStore(e.Runtime, postgres.Options{Now: e.clock.Now})
			if err != nil {
				results <- err
				return
			}
			results <- s.Leases().Acquire(ctx, A, "bk_race_"+string(rune('a'+i)), time.Hour)
		}()
	}
	wg.Wait()
	close(results)
	won, busy := 0, 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, gitops.ErrBackupInProgress):
			busy++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || busy != 7 {
		t.Fatalf("lease race: %d won, %d busy", won, busy)
	}

	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := e.Recoveries().Begin(ctx, A, "rcv_race_"+string(rune('a'+i)), gitops.RestoreSource{BackupID: "bk_1"}, e.clock.Now())
			if err != nil {
				errs <- err
				return
			}
			ids <- rec.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("an identical source resumes: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("racing identical begins produced %d recoveries: %v", len(seen), seen)
	}
	var n int
	if err := e.Owner.QueryRow(ctx, `SELECT count(*) FROM git_recoveries WHERE tenant_id = $1`, string(A)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("recovery rows: %d %v", n, err)
	}
}

// Test_postgres_gitops_database_clock: without an injected clock the database's
// now() decides when a lease and a prune intent expire, so instances need no
// synchronized clocks.
func Test_postgres_gitops_database_clock(t *testing.T) {
	ctx := context.Background()
	layerSkip(t)
	e := newPGEnv(t)
	A, sa := gitopstest.ContractTenantA, gitopstest.ContractScopeA()
	e.Seed(t, sa, gitopstest.ContractRepo1)
	s, err := postgres.NewStore(e.Runtime, postgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, in := s.Leases(), s.Intents()
	if err := l.Acquire(ctx, A, "bk_db1", 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(ctx, A, "bk_db2", time.Minute); !errors.Is(err, gitops.ErrBackupInProgress) {
		t.Fatalf("a live lease: %v", err)
	}
	ref, tip := "refs/heads/kodama/work/wi_db", strings.Repeat("a", 40)
	if err := in.Declare(ctx, sa, gitopstest.ContractRepo1, ref, tip, "pruner_db", 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if busy, err := in.Conflicts(ctx, sa, gitopstest.ContractRepo1, ref, tip); err != nil || !busy {
		t.Fatalf("a live intent: %v %v", busy, err)
	}
	time.Sleep(700 * time.Millisecond)
	if busy, err := in.Conflicts(ctx, sa, gitopstest.ContractRepo1, ref, tip); err != nil || busy {
		t.Fatalf("an expired intent: %v %v", busy, err)
	}
	if err := l.Acquire(ctx, A, "bk_db2", time.Minute); err != nil {
		t.Fatalf("an expired lease is abandoned: %v", err)
	}
	if st, _, _ := l.State(ctx, A, "bk_db1"); st != gitops.BackupFailed {
		t.Fatalf("the abandoned run is %s", st)
	}
	if err := l.Renew(ctx, A, "bk_db1", time.Minute); !errors.Is(err, gitops.ErrLeaseLost) {
		t.Fatalf("renewing an abandoned run: %v", err)
	}
}
