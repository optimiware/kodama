package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/test/support/postgres"
)

func TestDatabaseLifecycle(t *testing.T) {
	var name, migRole, rtRole string
	var rtURL string
	t.Run("provisioned", func(t *testing.T) {
		d := postgres.New(t)
		name, migRole, rtRole, rtURL = d.Name, d.MigrationRole, d.RuntimeRole, d.RuntimeURL
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if !strings.HasPrefix(d.Name, postgres.NamePrefix) || d.MigrationRole == d.RuntimeRole {
			t.Fatalf("names = %q %q %q", d.Name, d.MigrationRole, d.RuntimeRole)
		}
		rt := d.RuntimePool(ctx, 1)
		var super, bypass bool
		var db, user string
		if err := rt.QueryRow(ctx, `SELECT current_database(), current_user, r.rolsuper, r.rolbypassrls
			FROM pg_roles r WHERE r.rolname = current_user`).Scan(&db, &user, &super, &bypass); err != nil {
			t.Fatal(err)
		}
		if db != d.Name || user != d.RuntimeRole || super || bypass {
			t.Fatalf("runtime identity = %s/%s super=%v bypassrls=%v", db, user, super, bypass)
		}
		mig := d.MigrationPool(ctx, 1)
		var owner string
		if err := mig.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()`).Scan(&owner); err != nil || owner != d.MigrationRole {
			t.Fatalf("database owner = %q (err %v), want %q", owner, err, d.MigrationRole)
		}
		if _, err := mig.Exec(ctx, "CREATE TABLE owned (n int)"); err != nil {
			t.Fatalf("migration role cannot create tables: %v", err)
		}
		if _, err := rt.Exec(ctx, "CREATE TABLE rogue (n int)"); err == nil {
			t.Fatal("runtime role can create tables")
		}
		// A second database is independent.
		other := postgres.New(t)
		if other.Name == d.Name || other.RuntimeRole == d.RuntimeRole {
			t.Fatal("databases are not uniquely named")
		}
	})

	if name == "" {
		return // skipped (no PostgreSQL configured) or failed during setup
	}
	// The subtest above has finished, so its cleanup has run.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, postgres.AdminURL())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var left int
	if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1)
		+ (SELECT count(*) FROM pg_roles WHERE rolname = ANY ($2))`, name, []string{migRole, rtRole}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("cleanup left %d database/role objects behind", left)
	}
	if _, err := pgx.Connect(ctx, rtURL); err == nil {
		t.Fatal("runtime role can still connect after cleanup")
	}
}
