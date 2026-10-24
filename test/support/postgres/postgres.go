// Package postgres provides isolated PostgreSQL databases for integration
// tests.
//
// The helper connects with an administrator URI taken from the environment
// (host-env.json of the scope that make dev-up built, either stack):
//
//	KODAMA_TEST_POSTGRES_ADMIN_URL  postgres://user:pass@host:port/postgres?sslmode=disable
//	                                (a superuser)
//	KODAMA_REQUIRE_POSTGRES=1       fail instead of skip when the URL is unset
//
// New creates one database per call, named t002_<random>, owned by a fresh
// migration role, plus a fresh runtime role (NOSUPERUSER NOBYPASSRLS), both
// named after the database so concurrent tests and packages never collide.
// The layout mirrors scripts/dev/environment.py: the migration role owns the
// database and public schema, PUBLIC cannot create in public, and the runtime
// role only has USAGE on it; schema grants for tables come from migrations.
// t.Cleanup closes every pool handed out, drops the database and both roles.
//
// NewFromTemplate (template.go) clones a database that was migrated once per
// manifest digest instead of migrating every test's database.
package postgres

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

	"github.com/optimiware/kodama/test/support/layer"
)

const (
	// AdminURLEnv names the environment variable holding the admin URI.
	AdminURLEnv = "KODAMA_TEST_POSTGRES_ADMIN_URL"
	// RequireEnv names the variable that turns a missing admin URI into a
	// test failure.
	RequireEnv = "KODAMA_REQUIRE_POSTGRES"

	// NamePrefix prefixes every database and role created by this helper.
	NamePrefix = "t002_"
)

// AdminURL returns the configured administrator URI, or "" when unset.
func AdminURL() string { return os.Getenv(AdminURLEnv) }

// Database is one isolated test database with its two roles.
type Database struct {
	Name          string
	MigrationRole string
	RuntimeRole   string
	// MigrationURL and RuntimeURL connect as the owner and runtime roles.
	MigrationURL string
	RuntimeURL   string

	adminURL string
	// shared marks a clone: its roles belong to the template and outlive it.
	shared bool
	t      testing.TB
	mu     sync.Mutex
	pools  []*pgxpool.Pool
}

// New creates a fresh database or skips the test when PostgreSQL is not
// configured (fails instead when KODAMA_REQUIRE_POSTGRES=1). Everything is
// removed when the test ends.
func New(t testing.TB) *Database {
	t.Helper()
	adminURL := requireAdminURL(t)

	suffix := randomHex(t, 6)
	d := &Database{
		Name:          NamePrefix + suffix,
		MigrationRole: NamePrefix + suffix + "_mig",
		RuntimeRole:   NamePrefix + suffix + "_rt",
		adminURL:      adminURL,
		t:             t,
	}
	migPassword, rtPassword := randomHex(t, 16), randomHex(t, 16)
	var err error
	if d.MigrationURL, err = withIdentity(adminURL, d.MigrationRole, migPassword, d.Name); err != nil {
		t.Fatalf("build migration URL: %v", err)
	}
	if d.RuntimeURL, err = withIdentity(adminURL, d.RuntimeRole, rtPassword, d.Name); err != nil {
		t.Fatalf("build runtime URL: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Register cleanup first so a partial setup is still removed.
	t.Cleanup(d.drop)
	d.create(ctx, migPassword, rtPassword)
	return d
}

// requireAdminURL skips (or fails with KODAMA_REQUIRE_POSTGRES=1) unless
// PostgreSQL is configured, and returns the administrator URI.
func requireAdminURL(t testing.TB) string {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
	adminURL := AdminURL()
	if adminURL == "" {
		if os.Getenv(RequireEnv) == "1" {
			t.Fatalf("%s is unset but %s=1: PostgreSQL is required", AdminURLEnv, RequireEnv)
		}
		t.Skipf("PostgreSQL integration test skipped: set %s to an admin URI (or %s=1 to require it)", AdminURLEnv, RequireEnv)
	}
	return adminURL
}

func (d *Database) create(ctx context.Context, migPassword, rtPassword string) {
	d.t.Helper()
	admin, err := pgx.Connect(ctx, d.adminURL)
	if err != nil {
		d.t.Fatalf("connect as PostgreSQL administrator (%s): %v", AdminURLEnv, err)
	}
	defer admin.Close(ctx)
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %s", ident(d.MigrationRole), literal(migPassword)),
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %s", ident(d.RuntimeRole), literal(rtPassword)),
		fmt.Sprintf("CREATE DATABASE %s OWNER %s", ident(d.Name), ident(d.MigrationRole)),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			d.t.Fatalf("setup %q: %v", strings.SplitN(stmt, " PASSWORD", 2)[0], err)
		}
	}
	inDB, err := pgx.Connect(ctx, mustReplaceDatabase(d.adminURL, d.Name))
	if err != nil {
		d.t.Fatalf("connect to %s: %v", d.Name, err)
	}
	defer inDB.Close(ctx)
	for _, stmt := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", ident(d.RuntimeRole)),
	} {
		if _, err := inDB.Exec(ctx, stmt); err != nil {
			d.t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}

// drop removes the database and roles. It runs with a fresh context so it
// still works after the test context ends.
func (d *Database) drop() {
	d.closePools()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, d.adminURL)
	if err != nil {
		d.t.Errorf("cleanup: connect as administrator: %v", err)
		return
	}
	defer admin.Close(ctx)
	stmts := []string{fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", ident(d.Name))}
	if !d.shared {
		stmts = append(stmts,
			fmt.Sprintf("DROP ROLE IF EXISTS %s", ident(d.RuntimeRole)),
			fmt.Sprintf("DROP ROLE IF EXISTS %s", ident(d.MigrationRole)))
	}
	for _, stmt := range stmts {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			d.t.Errorf("cleanup %q: %v", stmt, err)
		}
	}
}

func (d *Database) closePools() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.pools {
		p.Close()
	}
	d.pools = nil
}

// MigrationPool returns a pool connected as the owner role. Closed on cleanup.
func (d *Database) MigrationPool(ctx context.Context, maxConns int32) *pgxpool.Pool {
	d.t.Helper()
	return d.pool(ctx, d.MigrationURL, maxConns)
}

// RuntimePool returns a pool connected as the non-owner runtime role. Closed
// on cleanup. Use maxConns=1 to force connection reuse.
func (d *Database) RuntimePool(ctx context.Context, maxConns int32) *pgxpool.Pool {
	d.t.Helper()
	return d.pool(ctx, d.RuntimeURL, maxConns)
}

func (d *Database) pool(ctx context.Context, dsn string, maxConns int32) *pgxpool.Pool {
	d.t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		d.t.Fatalf("parse pool config: %v", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		d.t.Fatalf("open pool: %v", err)
	}
	d.mu.Lock()
	d.pools = append(d.pools, p)
	d.mu.Unlock()
	if err := p.Ping(ctx); err != nil {
		d.t.Fatalf("ping %s: %v", cfg.ConnConfig.User, err)
	}
	return p
}

func withIdentity(adminURL, user, password, database string) (string, error) {
	u, err := url.Parse(adminURL)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", AdminURLEnv, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("%s must be a postgres:// URI", AdminURLEnv)
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + database
	u.RawPath = ""
	return u.String(), nil
}

func mustReplaceDatabase(adminURL, database string) string {
	u, err := url.Parse(adminURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + database
	u.RawPath = ""
	return u.String()
}

func ident(s string) string   { return pgx.Identifier{s}.Sanitize() }
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}
