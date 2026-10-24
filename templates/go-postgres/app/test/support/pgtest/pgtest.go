// Package pgtest provisions an isolated PostgreSQL database per test with a
// migration (owner) role and a separate non-owner, non-BYPASSRLS runtime role.
//
//	INVENTORY_TEST_ADMIN_URL      superuser URI of a disposable server (the password comes from your environment)
//	INVENTORY_REQUIRE_POSTGRES=1  fail instead of skip when the URL is unset
//	INVENTORY_TEST_PREFIX         resource prefix for database and role names (default "inv")
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database is one isolated test database.
type Database struct {
	Name          string
	MigrationRole string
	RuntimeRole   string
	MigrationURL  string
	RuntimeURL    string
	AdminDBURL    string

	t        testing.TB
	adminURL string
	mu       sync.Mutex
	pools    []*pgxpool.Pool
}

// New creates a fresh database, or skips (fails when required) without a server.
func New(t testing.TB) *Database {
	t.Helper()
	admin := os.Getenv("INVENTORY_TEST_ADMIN_URL")
	if admin == "" {
		if os.Getenv("INVENTORY_REQUIRE_POSTGRES") == "1" {
			t.Fatal("INVENTORY_TEST_ADMIN_URL is unset but INVENTORY_REQUIRE_POSTGRES=1")
		}
		t.Skip("PostgreSQL test skipped: set INVENTORY_TEST_ADMIN_URL")
	}
	prefix := os.Getenv("INVENTORY_TEST_PREFIX")
	if prefix == "" {
		prefix = "inv"
	}
	suffix := randHex(t, 6)
	d := &Database{t: t, adminURL: admin, Name: prefix + "_" + suffix}
	d.MigrationRole, d.RuntimeRole = d.Name+"_mig", d.Name+"_rt"
	migPW, rtPW := randHex(t, 16), randHex(t, 16)
	var err error
	if d.MigrationURL, err = identity(admin, d.MigrationRole, migPW, d.Name); err != nil {
		t.Fatal(err)
	}
	if d.RuntimeURL, err = identity(admin, d.RuntimeRole, rtPW, d.Name); err != nil {
		t.Fatal(err)
	}
	if d.AdminDBURL, err = identity(admin, "", "", d.Name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.drop)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect as administrator: %v", err)
	}
	defer conn.Close(ctx)
	for _, s := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD '%s'", id(d.MigrationRole), migPW),
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD '%s'", id(d.RuntimeRole), rtPW),
		fmt.Sprintf("CREATE DATABASE %s OWNER %s", id(d.Name), id(d.MigrationRole)),
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	in, err := pgx.Connect(ctx, d.AdminDBURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", d.Name, err)
	}
	defer in.Close(ctx)
	for _, s := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", id(d.RuntimeRole)),
	} {
		if _, err := in.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return d
}

// Pool opens a pool with the given URL; it is closed on cleanup.
func (d *Database) Pool(ctx context.Context, dsn string, max int32) *pgxpool.Pool {
	d.t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		d.t.Fatal(err)
	}
	if max > 0 {
		cfg.MaxConns = max
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	d.pools = append(d.pools, p)
	d.mu.Unlock()
	if err := p.Ping(ctx); err != nil {
		d.t.Fatalf("ping: %v", err)
	}
	return p
}

// OwnerPool is a pool as the migration/owner role.
func (d *Database) OwnerPool(ctx context.Context, max int32) *pgxpool.Pool {
	return d.Pool(ctx, d.MigrationURL, max)
}

// RuntimePool is a pool as the non-owner runtime role.
func (d *Database) RuntimePool(ctx context.Context, max int32) *pgxpool.Pool {
	return d.Pool(ctx, d.RuntimeURL, max)
}

func (d *Database) drop() {
	d.mu.Lock()
	for _, p := range d.pools {
		p.Close()
	}
	d.pools = nil
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, d.adminURL)
	if err != nil {
		d.t.Errorf("cleanup connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	for _, s := range []string{
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", id(d.Name)),
		fmt.Sprintf("DROP ROLE IF EXISTS %s", id(d.RuntimeRole)),
		fmt.Sprintf("DROP ROLE IF EXISTS %s", id(d.MigrationRole)),
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			d.t.Errorf("cleanup %q: %v", s, err)
		}
	}
}

func id(s string) string { return pgx.Identifier{s}.Sanitize() }

func randHex(t testing.TB, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func identity(base, user, password, db string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	u.Path = "/" + strings.TrimPrefix(db, "/")
	return u.String(), nil
}
