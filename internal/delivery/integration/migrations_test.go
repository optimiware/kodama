package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

// ledger is the applied-migration history: id, checksum and application time.
func ledger(t *testing.T, s *stack) []string {
	t.Helper()
	rows, err := s.X.Migration.Query(context.Background(), `SELECT id || '|' || checksum || '|' || applied_at::text FROM schema_migrations ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

// retained fingerprints every retained delivery record, so a reapplication that
// touched data would be visible.
func retained(t *testing.T, s *stack) string {
	t.Helper()
	var parts []string
	for _, table := range deliveryTables {
		var sum string
		s.X.Scalar(&sum, `SELECT coalesce(md5(string_agg(t::text, E'\n' ORDER BY t::text)), '') || ':' || count(*) FROM `+table+` t`)
		parts = append(parts, table+"="+sum)
	}
	return strings.Join(parts, ";")
}

// privileges lists the runtime role's table and column privileges on the
// delivery tables.
func privileges(t *testing.T, s *stack) string {
	t.Helper()
	rows, err := s.X.Migration.Query(context.Background(), `
SELECT 't:' || table_name || ':' || privilege_type FROM information_schema.table_privileges
 WHERE grantee = $1 AND table_name = ANY($2)
UNION ALL
SELECT 'c:' || table_name || '.' || column_name || ':' || privilege_type FROM information_schema.column_privileges
 WHERE grantee = $1 AND table_name = ANY($2) AND privilege_type = 'UPDATE'
ORDER BY 1`, s.X.Base.DB.RuntimeRole, deliveryTables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatal("the runtime role holds no delivery privileges")
	}
	return strings.Join(out, "\n")
}

// Test_delivery_migration_reapply proves the delivery contribution is ordered
// after the migrations it references, applies once, reapplies as a no-op that
// changes neither the ledger, the retained records nor the runtime role's
// privileges (also under concurrent migrators), and that an applied migration is
// immutable: a modified or reordered delivery migration is refused before any
// later one runs. Real PostgreSQL.
func Test_delivery_migration_reapply(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
	// Retained records to preserve: environments, a snapshot, a deployment and history.
	id, _ := s.settled(t)
	if _, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id); err != nil {
		t.Fatal(err)
	}

	manifest, err := deliverytest.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	opts := storage.MigrateOptions{RuntimeRole: s.X.Base.DB.RuntimeRole}
	order := func() map[string]int {
		pos := map[string]int{}
		for i, m := range manifest.Migrations() {
			pos[m.ID] = i
		}
		return pos
	}()

	t.Run("the delivery migration follows everything it references and is recorded once", func(t *testing.T) {
		var ids []string
		for _, line := range ledger(t, s) {
			ids = append(ids, strings.SplitN(line, "|", 2)[0])
		}
		want := make([]string, 0, len(order))
		for _, m := range manifest.Migrations() {
			want = append(want, m.ID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Fatalf("the ledger order is not the manifest order:\n%v\n%v", ids, want)
		}
		own := deliverypgIDs()
		if len(own) == 0 || ids[len(ids)-1] != own[len(own)-1] {
			t.Fatalf("the delivery migration must follow every migration it references: %v", ids)
		}
		for _, id := range own {
			n := 0
			for _, got := range ids {
				if got == id {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%s is recorded %d times", id, n)
			}
		}
		for _, prefix := range []string{"T002_", "T015_", "T019_", "T020_"} {
			found := false
			for _, id := range ids {
				found = found || strings.HasPrefix(id, prefix)
			}
			if !found {
				t.Errorf("no applied %s migration: the delivery schema's foreign keys have no target", prefix)
			}
		}
	})

	t.Run("reapplication is a no-op for the ledger, the records and the privileges", func(t *testing.T) {
		led, rec, priv := ledger(t, s), retained(t, s), privileges(t, s)
		for range 2 {
			res, err := storage.Migrate(ctx, s.X.Migration, manifest, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) != 0 || res.Skipped != len(order) {
				t.Fatalf("reapplication = %+v", res)
			}
		}
		if got := ledger(t, s); fmt.Sprint(got) != fmt.Sprint(led) {
			t.Fatal("reapplication changed the migration ledger")
		}
		if got := retained(t, s); got != rec {
			t.Fatalf("reapplication changed retained records:\n%s\n%s", rec, got)
		}
		if got := privileges(t, s); got != priv {
			t.Fatalf("reapplication changed the runtime role's privileges:\n%s\n---\n%s", priv, got)
		}
	})

	t.Run("concurrent migrators serialize and change nothing", func(t *testing.T) {
		led, rec := ledger(t, s), retained(t, s)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = storage.Migrate(ctx, s.X.Migration, manifest, opts)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("migrator %d: %v", i, err)
			}
		}
		if fmt.Sprint(ledger(t, s)) != fmt.Sprint(led) || retained(t, s) != rec {
			t.Fatal("concurrent reapplication changed state")
		}
	})

	t.Run("a modified applied migration is refused before a later one runs", func(t *testing.T) {
		led := ledger(t, s)
		contribs := deliverytest.Contributions()
		for i, c := range contribs {
			if c.Module != "delivery" {
				continue
			}
			ms := append([]storage.Migration(nil), c.Migrations...)
			ms[0] = storage.Migration{ID: ms[0].ID, SQL: ms[0].SQL + "\nALTER TABLE deployments ADD COLUMN t035_tamper int;\n"}
			contribs[i] = storage.Contribution{Module: c.Module, Migrations: ms}
		}
		next := storage.Contribution{Module: "later", Migrations: []storage.Migration{{ID: "T999_001_later.sql", SQL: "CREATE TABLE t035_later (n int)"}}}
		tampered, err := storage.NewManifest(append(contribs, next)...)
		if err != nil {
			t.Fatal(err)
		}
		_, err = storage.Migrate(ctx, s.X.Migration, tampered, opts)
		if !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		var n int
		s.X.Scalar(&n, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'deployments' AND column_name = 't035_tamper'`)
		var later bool
		s.X.Scalar(&later, `SELECT to_regclass('t035_later') IS NOT NULL`)
		if n != 0 || later || fmt.Sprint(ledger(t, s)) != fmt.Sprint(led) {
			t.Fatal("a tampered delivery migration changed the schema or a later migration ran")
		}
	})

	t.Run("a delivery migration ordered before what it references fails atomically on a fresh database", func(t *testing.T) {
		db := postgres.New(t)
		pool := db.MigrationPool(ctx, 2)
		var reordered []storage.Contribution
		for _, c := range deliverytest.Contributions() {
			if c.Module == "delivery" {
				reordered = append([]storage.Contribution{c}, reordered...)
			} else {
				reordered = append(reordered, c)
			}
		}
		bad, err := storage.NewManifest(reordered...)
		if err == nil {
			_, err = storage.Migrate(ctx, pool, bad, storage.MigrateOptions{RuntimeRole: db.RuntimeRole})
		}
		if err == nil {
			t.Fatal("a delivery migration ordered before its references was applied")
		}
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('deployments') IS NOT NULL OR to_regclass('environments') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatalf("a failed migration left delivery tables: %v %v", exists, err)
		}
		// The correct order applies on the same database, then reapplies cleanly.
		good, err := deliverytest.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			res, err := storage.Migrate(ctx, pool, good, storage.MigrateOptions{RuntimeRole: db.RuntimeRole})
			if err != nil {
				t.Fatalf("apply %d: %v", i, err)
			}
			if (i == 0) != (len(res.Applied) > 0) {
				t.Fatalf("apply %d: %+v", i, res)
			}
		}
	})
}

// deliverypgIDs lists the delivery contribution's migration IDs.
func deliverypgIDs() []string {
	var out []string
	for _, m := range deliverypg.Migrations().Migrations {
		out = append(out, m.ID)
	}
	return out
}
