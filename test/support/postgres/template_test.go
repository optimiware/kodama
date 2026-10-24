package postgres_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/test/support/postgres"
)

const demoSchema = `
CREATE TABLE items (id int PRIMARY KEY, label text NOT NULL);
GRANT SELECT, INSERT ON items TO %s;
INSERT INTO items VALUES (1, 'from-template');`

func demoMigrator(calls *atomic.Int32) postgres.Migrator {
	return func(ctx context.Context, d *postgres.Database) error {
		calls.Add(1)
		_, err := d.MigrationPool(ctx, 1).Exec(ctx, fmt.Sprintf(demoSchema, d.RuntimeRole))
		return err
	}
}

func freshDigest(t *testing.T) string {
	t.Helper()
	return postgres.ManifestDigest(t.Name(), time.Now().String())
}

func admin(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, postgres.AdminURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func count(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTemplateRoundTrip migrates once, clones many times, proves each clone is
// isolated and owned like a New database, and checks that cleanup leaves
// nothing behind.
func TestTemplateRoundTrip(t *testing.T) {
	postgres.New(t) // skips without PostgreSQL; the lifecycle test covers New itself
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	digest := freshDigest(t)
	var calls atomic.Int32
	var cloneNames []string
	var roles []string

	t.Run("clones", func(t *testing.T) {
		a := postgres.NewFromTemplate(t, digest, demoMigrator(&calls))
		b := postgres.NewFromTemplate(t, digest, demoMigrator(&calls))
		cloneNames = []string{a.Name, b.Name}
		roles = []string{a.MigrationRole, a.RuntimeRole}
		if a.Name == b.Name || calls.Load() != 1 {
			t.Fatalf("names %q %q, migrations run %d (want 1)", a.Name, b.Name, calls.Load())
		}
		rtA, rtB := a.RuntimePool(ctx, 1), b.RuntimePool(ctx, 1)
		var label, db string
		if err := rtA.QueryRow(ctx, "SELECT label, current_database() FROM items WHERE id = 1").Scan(&label, &db); err != nil || label != "from-template" || db != a.Name {
			t.Fatalf("clone content = %q in %q (err %v)", label, db, err)
		}
		if _, err := rtA.Exec(ctx, "INSERT INTO items VALUES (2, 'only-in-a')"); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := rtB.QueryRow(ctx, "SELECT count(*) FROM items").Scan(&n); err != nil || n != 1 {
			t.Fatalf("clone B sees %d rows after writing to A (err %v); clones share state", n, err)
		}
		var super, bypass bool
		var owner string
		if err := rtA.QueryRow(ctx, `SELECT r.rolsuper, r.rolbypassrls, pg_get_userbyid(d.datdba)
			FROM pg_roles r, pg_database d WHERE r.rolname = current_user AND d.datname = current_database()`).Scan(&super, &bypass, &owner); err != nil {
			t.Fatal(err)
		}
		if super || bypass || owner != a.MigrationRole {
			t.Fatalf("runtime super=%v bypassrls=%v, database owner %q want %q", super, bypass, owner, a.MigrationRole)
		}
		if _, err := rtA.Exec(ctx, "CREATE TABLE rogue (n int)"); err == nil {
			t.Fatal("runtime role can create tables in a clone")
		}
		if _, err := rtA.Exec(ctx, "DELETE FROM items"); err == nil {
			t.Fatal("runtime role has privileges the migration never granted")
		}
		if _, err := a.MigrationPool(ctx, 1).Exec(ctx, "CREATE TABLE owned (n int)"); err != nil {
			t.Fatalf("migration role cannot create tables in a clone: %v", err)
		}
	})
	if len(cloneNames) == 0 {
		return // skipped or failed during setup
	}

	conn := admin(t, ctx)
	// The clones' cleanup has run; the template and its roles remain for reuse.
	if n := count(t, ctx, conn, "SELECT count(*) FROM pg_database WHERE datname = ANY($1)", cloneNames); n != 0 {
		t.Fatalf("%d cloned databases survived cleanup", n)
	}
	var tplName string
	if err := conn.QueryRow(ctx, "SELECT datname FROM pg_database WHERE datistemplate AND datname LIKE $1 AND datname = $2",
		postgres.TemplatePrefix+"%", roles[0][:len(roles[0])-len("_mig")]).Scan(&tplName); err != nil {
		t.Fatalf("template not retained: %v", err)
	}
	t.Run("reuse", func(t *testing.T) {
		c := postgres.NewFromTemplate(t, digest, demoMigrator(&calls))
		var n int
		if err := c.RuntimePool(ctx, 1).QueryRow(ctx, "SELECT count(*) FROM items").Scan(&n); err != nil || n != 1 || calls.Load() != 1 {
			t.Fatalf("reused clone rows=%d migrations=%d err=%v", n, calls.Load(), err)
		}
	})

	postgres.DropTemplate(t, digest)
	postgres.DropTemplate(t, digest) // idempotent
	if n := count(t, ctx, conn, "SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname = ANY($2))", tplName, roles); n != 0 {
		t.Fatalf("DropTemplate left %d database/role objects", n)
	}
}

func TestTemplateIsBuiltOnceUnderConcurrency(t *testing.T) {
	postgres.New(t)
	digest := freshDigest(t)
	t.Cleanup(func() { postgres.DropTemplate(t, digest) })
	var calls atomic.Int32
	t.Run("group", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				postgres.NewFromTemplate(t, digest, demoMigrator(&calls))
			})
		}
	})
	if calls.Load() != 1 {
		t.Fatalf("template migrated %d times, want 1", calls.Load())
	}
}
