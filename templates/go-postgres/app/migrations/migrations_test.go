package migrations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"example.invalid/inventory/migrations"
	"example.invalid/inventory/test/support/pgtest"
)

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestMigrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("fresh install, then repeated application is a no-op", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		done, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole})
		if err != nil || len(done) != 2 {
			t.Fatalf("fresh: %v %v", done, err)
		}
		done, err = migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole})
		if err != nil || len(done) != 0 {
			t.Fatalf("repeat: %v %v", done, err)
		}
		latest, _ := migrations.Latest()
		cur, err := migrations.Current(ctx, conn)
		if err != nil || cur[len(cur)-1] != latest {
			t.Fatalf("current %v latest %s %v", cur, latest, err)
		}
		// The runtime role owns nothing and cannot alter the schema.
		rt := connect(t, db.RuntimeURL)
		if _, err := rt.Exec(ctx, "ALTER TABLE exceptions ADD COLUMN x int"); err == nil {
			t.Fatal("the runtime role must not own tables")
		}
		if _, err := rt.Exec(ctx, "CREATE TABLE x (a int)"); err == nil {
			t.Fatal("the runtime role must not create objects")
		}
	})

	t.Run("an applied migration that changed is refused", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		set, _ := migrations.Load()
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: set}); err != nil {
			t.Fatal(err)
		}
		tampered := append([]migrations.Migration{}, set...)
		tampered[0].Checksum = "00" + tampered[0].Checksum[2:]
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: tampered}); !errors.Is(err, migrations.ErrChecksum) {
			t.Fatalf("want ErrChecksum, got %v", err)
		}
	})

	t.Run("a database from a newer release is refused by an older one", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		set, _ := migrations.Load()
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: set}); err != nil {
			t.Fatal(err)
		}
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: set[:1]}); !errors.Is(err, migrations.ErrDatabaseNewer) {
			t.Fatalf("want ErrDatabaseNewer, got %v", err)
		}
	})

	t.Run("a migration ordered before an applied one is refused", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		independent := []migrations.Migration{{Version: "T900_b", SQL: "CREATE TABLE b (i int);", Checksum: "b"}}
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: independent}); err != nil {
			t.Fatal(err)
		}
		both := []migrations.Migration{{Version: "T800_a", SQL: "CREATE TABLE a (i int);", Checksum: "a"}, independent[0]}
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: both}); !errors.Is(err, migrations.ErrOutOfOrder) {
			t.Fatalf("want ErrOutOfOrder, got %v", err)
		}
	})

	t.Run("a failed migration rolls back completely and leaves no ledger row", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		bad := []migrations.Migration{{Version: "T001_bad", SQL: "CREATE TABLE half (i int); SELECT 1/0;", Checksum: "x"}}
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: db.RuntimeRole, Migrations: bad}); err == nil {
			t.Fatal("expected failure")
		}
		var exists bool
		_ = conn.QueryRow(ctx, "SELECT to_regclass('half') IS NOT NULL").Scan(&exists)
		var n int
		_ = conn.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n)
		if exists || n != 0 {
			t.Fatalf("partial migration survived: table=%v ledger=%d", exists, n)
		}
	})

	t.Run("concurrent migrators apply each file once", func(t *testing.T) {
		db := pgtest.New(t)
		var wg sync.WaitGroup
		results := make(chan int, 6)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := pgx.Connect(ctx, db.MigrationURL)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close(ctx)
				done, err := migrations.Apply(ctx, c, migrations.Options{RuntimeRole: db.RuntimeRole})
				if err != nil {
					t.Error(err)
				}
				results <- len(done)
			}()
		}
		wg.Wait()
		close(results)
		total := 0
		for n := range results {
			total += n
		}
		if total != 2 {
			t.Fatalf("each of the 2 files must be applied exactly once across migrators, got %d", total)
		}
	})

	t.Run("an invalid runtime role is refused before touching the database", func(t *testing.T) {
		db := pgtest.New(t)
		conn := connect(t, db.MigrationURL)
		if _, err := migrations.Apply(ctx, conn, migrations.Options{RuntimeRole: `x"; DROP TABLE y; --`}); !errors.Is(err, migrations.ErrRole) {
			t.Fatalf("want ErrRole, got %v", err)
		}
	})
}
