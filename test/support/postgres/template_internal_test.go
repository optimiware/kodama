package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A failed migration must leave no database that a later caller could mistake
// for a finished template, and the next caller must rebuild it.
func TestFailedBuildLeavesNoTemplateAndIsRebuilt(t *testing.T) {
	adminURL := requireAdminURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tpl, err := newTemplate(adminURL, ManifestDigest(t.Name(), time.Now().String()))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	defer func() {
		for _, role := range []string{tpl.rtRole, tpl.migRole} {
			if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(role)); err != nil {
				t.Error(err)
			}
		}
	}()
	exists := func() (n int) {
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", tpl.name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	boom := errors.New("boom")
	err = tpl.ensure(ctx, t, func(ctx context.Context, d *Database) error {
		if _, err := d.MigrationPool(ctx, 1).Exec(ctx, "CREATE TABLE half (n int)"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ensure error = %v, want the migration failure", err)
	}
	if exists() != 0 {
		t.Fatal("failed build left its database behind")
	}

	// An interrupted build (a crash between CREATE DATABASE and the template
	// mark) leaves a plain database; it is dropped and rebuilt.
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident(tpl.name)+" OWNER "+ident(tpl.migRole)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	if err := tpl.ensure(ctx, t, func(ctx context.Context, d *Database) error {
		calls.Add(1)
		_, err := d.MigrationPool(ctx, 1).Exec(ctx, "CREATE TABLE whole (n int)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ready bool
	if err := conn.QueryRow(ctx, "SELECT datistemplate FROM pg_database WHERE datname = $1", tpl.name).Scan(&ready); err != nil || !ready || calls.Load() != 1 {
		t.Fatalf("rebuilt template ready=%v migrations=%d err=%v", ready, calls.Load(), err)
	}
	if err := tpl.ensure(ctx, t, func(context.Context, *Database) error { t.Error("complete template migrated again"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := tpl.drop(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if exists() != 0 {
		t.Fatal("drop left the template")
	}
}
