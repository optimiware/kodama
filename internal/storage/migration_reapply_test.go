package storage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

func contribution(module string, ms ...storage.Migration) storage.Contribution {
	return storage.Contribution{Module: module, Migrations: ms}
}

func mig(id, sql string) storage.Migration { return storage.Migration{ID: id, SQL: sql} }

func manifest(t testing.TB, cs ...storage.Contribution) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(cs...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func appliedIDs(t testing.TB, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(testCtx(t), "SELECT id FROM schema_migrations ORDER BY seq")
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func tableExists(t testing.TB, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(testCtx(t), "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// Test_migration_reapply covers ordered, idempotent, serialized application,
// duplicate-ID rejection, checksum immutability and atomic failure. Select it
// with: go test -run migration_reapply ./internal/storage/
func Test_migration_reapply(t *testing.T) {
	db := postgres.New(t) // shared database, subtests build on each other in order
	ctx := testCtx(t)
	pool := db.MigrationPool(ctx, 4)
	opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}

	base := storage.BaseContribution()
	modA := contribution("modA",
		mig("T900_001_a_one.sql", "CREATE TABLE a_one (tenant_id kodama_id NOT NULL REFERENCES tenants (id), n int)"),
		mig("T900_002_a_two.sql", "CREATE TABLE a_two (n int); GRANT SELECT ON a_two TO {{runtime_role}}"),
	)
	// A module listed after base but with a lexically smaller task ID: the
	// manifest order, not the name, decides.
	modB := contribution("modB", mig("T001_001_b_first.sql", "CREATE TABLE b_first (n int)"))
	modC := contribution("modC", mig("T901_001_c.sql", "CREATE TABLE c_one (n int)"))
	full := manifest(t, base, modA, modB)
	wantOrder := func() []string {
		var ids []string
		for _, m := range full.Migrations() {
			ids = append(ids, m.ID)
		}
		return ids
	}()

	t.Run("ordered_application", func(t *testing.T) {
		res, err := storage.Migrate(ctx, pool, full, opts)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(res.Applied) != fmt.Sprint(wantOrder) || res.Skipped != 0 {
			t.Fatalf("applied %v skipped %d, want %v", res.Applied, res.Skipped, wantOrder)
		}
		if got := appliedIDs(t, pool); fmt.Sprint(got) != fmt.Sprint(wantOrder) {
			t.Fatalf("recorded %v, want %v", got, wantOrder)
		}
		var role string
		err = pool.QueryRow(ctx, `SELECT grantee FROM information_schema.table_privileges WHERE table_name = 'a_two' AND privilege_type = 'SELECT' AND grantee = $1`, db.RuntimeRole).Scan(&role)
		if err != nil {
			t.Fatalf("runtime role placeholder grant missing: %v", err)
		}
	})

	t.Run("reapply_is_noop", func(t *testing.T) {
		var before []string
		rows, err := pool.Query(ctx, "SELECT id || checksum || applied_at::text FROM schema_migrations ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			before = append(before, s)
		}
		rows.Close()
		res, err := storage.Migrate(ctx, pool, full, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 0 || res.Skipped != len(wantOrder) {
			t.Fatalf("reapply result = %+v", res)
		}
		var after []string
		rows, err = pool.Query(ctx, "SELECT id || checksum || applied_at::text FROM schema_migrations ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			after = append(after, s)
		}
		rows.Close()
		if fmt.Sprint(before) != fmt.Sprint(after) {
			t.Fatal("reapply changed schema_migrations")
		}
	})

	t.Run("incremental_contribution_applies_only_new", func(t *testing.T) {
		res, err := storage.Migrate(ctx, pool, manifest(t, base, modA, modB, modC), opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 1 || res.Applied[0] != "T901_001_c.sql" {
			t.Fatalf("applied %v", res.Applied)
		}
	})

	t.Run("checksum_mismatch_rejected", func(t *testing.T) {
		tampered := contribution("modA",
			mig("T900_001_a_one.sql", "CREATE TABLE a_one (tenant_id kodama_id NOT NULL REFERENCES tenants (id), n int, extra int)"),
			modA.Migrations[1],
		)
		next := contribution("modZ", mig("T902_001_z.sql", "CREATE TABLE z_one (n int)"))
		before := appliedIDs(t, pool)
		_, err := storage.Migrate(ctx, pool, manifest(t, base, tampered, modB, modC, next), opts)
		if !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		if tableExists(t, pool, "z_one") || fmt.Sprint(appliedIDs(t, pool)) != fmt.Sprint(before) {
			t.Fatal("a later migration was applied despite the tampered history")
		}
	})

	t.Run("applied_migration_missing_from_manifest_rejected", func(t *testing.T) {
		_, err := storage.Migrate(ctx, pool, manifest(t, base, modA), opts)
		if !errors.Is(err, storage.ErrUnknownMigration) {
			t.Fatalf("err = %v, want ErrUnknownMigration", err)
		}
	})

	t.Run("pending_before_applied_rejected", func(t *testing.T) {
		gap := contribution("gap", mig("T899_001_gap.sql", "CREATE TABLE gap_one (n int)"))
		_, err := storage.Migrate(ctx, pool, manifest(t, base, gap, modA, modB, modC), opts)
		if !errors.Is(err, storage.ErrMigrationOrder) {
			t.Fatalf("err = %v, want ErrMigrationOrder", err)
		}
		if tableExists(t, pool, "gap_one") {
			t.Fatal("out-of-order migration was applied")
		}
	})

	t.Run("duplicate_id_rejected", func(t *testing.T) {
		before := appliedIDs(t, pool)
		dup := contribution("dup", mig("T900_001_a_one.sql", "CREATE TABLE dup_table (n int)"))
		_, err := storage.NewManifest(base, modA, dup)
		if !errors.Is(err, storage.ErrDuplicateMigration) {
			t.Fatalf("err = %v, want ErrDuplicateMigration", err)
		}
		// Rejection happens before any database work.
		if tableExists(t, pool, "dup_table") || fmt.Sprint(appliedIDs(t, pool)) != fmt.Sprint(before) {
			t.Fatal("duplicate contribution reached the database")
		}
	})

	t.Run("failed_migration_leaves_no_partial_state", func(t *testing.T) {
		d := postgres.New(t)
		p := d.MigrationPool(ctx, 2)
		good := contribution("good", mig("T910_001_good.sql", "CREATE TABLE good_one (n int)"))
		if _, err := storage.Migrate(ctx, p, manifest(t, good), storage.MigrateOptions{}); err != nil {
			t.Fatal(err)
		}
		bad := contribution("bad",
			mig("T911_001_bad.sql", "CREATE TABLE half_done (n int); INSERT INTO half_done VALUES (1); SELECT 1/0"),
			mig("T911_002_after.sql", "CREATE TABLE after_bad (n int)"),
		)
		res, err := storage.Migrate(ctx, p, manifest(t, good, bad), storage.MigrateOptions{})
		if err == nil || pgCode(err) != "22012" {
			t.Fatalf("err = %v, want division by zero", err)
		}
		if len(res.Applied) != 0 {
			t.Fatalf("result claims %v applied", res.Applied)
		}
		if tableExists(t, p, "half_done") || tableExists(t, p, "after_bad") {
			t.Fatal("partial DDL from the failed migration survived")
		}
		if got := appliedIDs(t, p); fmt.Sprint(got) != "[T910_001_good.sql]" {
			t.Fatalf("recorded %v", got)
		}
		// The lock was released: a corrected migration under a new ID applies.
		fixed := contribution("bad", mig("T911_001_bad.sql", "CREATE TABLE half_done (n int)"))
		_, err = storage.Migrate(ctx, p, manifest(t, good, fixed), storage.MigrateOptions{})
		if err != nil {
			t.Fatalf("retry after failure: %v", err)
		}
	})

	t.Run("runtime_role_required_when_placeholder_used", func(t *testing.T) {
		d := postgres.New(t)
		p := d.MigrationPool(ctx, 1)
		_, err := storage.Migrate(ctx, p, manifest(t, base), storage.MigrateOptions{})
		if !errors.Is(err, storage.ErrRuntimeRoleRequired) {
			t.Fatalf("err = %v, want ErrRuntimeRoleRequired", err)
		}
		if tableExists(t, p, "schema_migrations") {
			t.Fatal("validation must happen before touching the database")
		}
	})

	t.Run("concurrent_runners_serialize", func(t *testing.T) {
		d := postgres.New(t)
		slow := contribution("slow",
			mig("T920_001_slow.sql", "SELECT pg_sleep(0.3); CREATE TABLE slow_one (n int)"),
			mig("T920_002_slow.sql", "SELECT pg_sleep(0.3); CREATE TABLE slow_two (n int); INSERT INTO slow_two VALUES (1)"),
		)
		m := manifest(t, base, slow)
		const runners = 6
		pools := make([]*pgxpool.Pool, runners)
		for i := range pools {
			// Separate pools: runners are independent processes in production.
			pools[i] = d.MigrationPool(ctx, 1)
		}
		var wg sync.WaitGroup
		results := make([]storage.MigrateResult, runners)
		errs := make([]error, runners)
		for i := 0; i < runners; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = storage.Migrate(ctx, pools[i], m, storage.MigrateOptions{RuntimeRole: d.RuntimeRole})
			}(i)
		}
		wg.Wait()
		total := 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("runner %d: %v", i, errs[i])
			}
			total += len(results[i].Applied)
		}
		if total != len(m.Migrations()) {
			t.Fatalf("migrations applied %d times in total, want exactly %d", total, len(m.Migrations()))
		}
		p := d.MigrationPool(ctx, 1)
		if got := appliedIDs(t, p); len(got) != len(m.Migrations()) {
			t.Fatalf("recorded %v", got)
		}
		// The lock is database-scoped: it does not block another database.
		other := postgres.New(t)
		if _, err := storage.Migrate(ctx, other.MigrationPool(ctx, 1), manifest(t, contribution("x", mig("T921_001_x.sql", "SELECT 1"))), storage.MigrateOptions{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancelled_context_releases_lock", func(t *testing.T) {
		d := postgres.New(t)
		p := d.MigrationPool(ctx, 1)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := storage.Migrate(cctx, p, manifest(t, contribution("x", mig("T930_001_x.sql", "SELECT 1"))), storage.MigrateOptions{}); err == nil {
			t.Fatal("cancelled migrate succeeded")
		}
		if _, err := storage.Migrate(ctx, p, manifest(t, contribution("x", mig("T930_001_x.sql", "SELECT 1"))), storage.MigrateOptions{}); err != nil {
			t.Fatalf("pool unusable after cancelled migrate: %v", err)
		}
	})
}
