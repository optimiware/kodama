package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/storage"
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

// Test_delivery_migration_reapply proves the squashed baseline applies once and
// reapplies as a no-op that changes neither the ledger, the retained records nor
// the runtime role's privileges (also under concurrent migrators), and that an
// applied migration is immutable: a modified migration is refused. Real
// PostgreSQL.
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

	t.Run("the ledger records the baseline's two migrations once", func(t *testing.T) {
		var ids []string
		for _, line := range ledger(t, s) {
			ids = append(ids, strings.SplitN(line, "|", 2)[0])
		}
		want := make([]string, 0, len(manifest.Migrations()))
		for _, m := range manifest.Migrations() {
			want = append(want, m.ID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Fatalf("the ledger order is not the manifest order:\n%v\n%v", ids, want)
		}
	})

	t.Run("reapplication is a no-op for the ledger, the records and the privileges", func(t *testing.T) {
		led, rec, priv := ledger(t, s), retained(t, s), privileges(t, s)
		for range 2 {
			res, err := storage.Migrate(ctx, s.X.Migration, manifest, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) != 0 || res.Skipped != len(manifest.Migrations()) {
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

	t.Run("a modified applied migration is refused", func(t *testing.T) {
		led := ledger(t, s)
		// The ledger lists every applied migration in order: the manifest is the
		// fixture's whole manifest with the baseline altered.
		contribs := deliverytest.Contributions()
		altered := storage.Contribution{Module: contribs[0].Module, Migrations: append([]storage.Migration(nil), contribs[0].Migrations...)}
		altered.Migrations[0].SQL += "\nALTER TABLE deployments ADD COLUMN t035_tamper int;\n"
		tampered, err := storage.NewManifest(append([]storage.Contribution{altered}, contribs[1:]...)...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, s.X.Migration, tampered, opts); !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		var n int
		s.X.Scalar(&n, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'deployments' AND column_name = 't035_tamper'`)
		if n != 0 || fmt.Sprint(ledger(t, s)) != fmt.Sprint(led) {
			t.Fatal("a tampered migration changed the schema")
		}
	})

}
