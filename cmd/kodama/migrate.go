package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/app/migrate"
)

// newMaintenanceDB opens the administrator's maintenance database pool. Tests
// replace it with a failing connector to prove the unreachable-maintenance
// refusal never renders the pool's cause; a real installation uses the pgx
// driver.
var newMaintenanceDB = pgdriver.New

// roleURLEnv names the connection URLs whose user and password define the
// database roles' login credentials. The database setup step sets exactly these
// passwords, so the secret the server reads and the roles never drift apart.
var roleURLEnv = []string{
	"KODAMA_DATABASE_URL",
	"KODAMA_CONTROLLER_DATABASE_URL",
	"KODAMA_PUBLISHER_DATABASE_URL",
	"KODAMA_AUTH_DATABASE_URL",
}

// runMigrateEntry is `kodama migrate`: create the database if needed, apply the
// reviewed migrations and set the role passwords, as the database administrator.
// KODAMA_ADMIN_DATABASE_URL names a maintenance database; the target database is
// the one KODAMA_DATABASE_URL points at.
func runMigrateEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		usage(stderr, "migrate takes no arguments; it reads its configuration from the environment")
		return 2
	}
	adminURL := os.Getenv("KODAMA_ADMIN_DATABASE_URL")
	admin, err := url.Parse(adminURL)
	// Each refusal names the one setting it is about, so a caller can tell a
	// wrong scheme from a missing user, password or maintenance database. A
	// single combined message would let a query-string password stand in for a
	// missing user and hide a weakened check.
	if err != nil || admin.Scheme != "postgres" {
		fmt.Fprintln(stderr, "kodama: KODAMA_ADMIN_DATABASE_URL must be a postgres:// URL")
		return 2
	}
	if admin.User == nil || admin.User.Username() == "" {
		fmt.Fprintln(stderr, "kodama: KODAMA_ADMIN_DATABASE_URL must name a database user")
		return 2
	}
	if password, ok := admin.User.Password(); !ok || password == "" {
		fmt.Fprintln(stderr, "kodama: KODAMA_ADMIN_DATABASE_URL must carry a database password")
		return 2
	}
	if strings.Trim(admin.Path, "/") == "" {
		fmt.Fprintln(stderr, "kodama: KODAMA_ADMIN_DATABASE_URL must name a maintenance database")
		return 2
	}
	target, err := url.Parse(os.Getenv("KODAMA_DATABASE_URL"))
	if err != nil || strings.Trim(target.Path, "/") == "" {
		fmt.Fprintln(stderr, "kodama: KODAMA_DATABASE_URL must name the target database")
		return 2
	}
	database := strings.Trim(target.Path, "/")
	targetAdmin := *admin
	targetAdmin.Path = "/" + database
	passwords := map[string]string{}
	origins := map[string]string{}
	for _, name := range roleURLEnv {
		role, password, err := roleCredential(os.Getenv(name))
		if err != nil {
			fmt.Fprintf(stderr, "kodama: %s: %v\n", name, err)
			return 2
		}
		if !migrate.ValidRole(role) {
			fmt.Fprintf(stderr, "kodama: %s: role must be a lowercase kodama_ login role\n", name)
			return 2
		}
		if previous, seen := passwords[role]; seen && previous != password {
			fmt.Fprintf(stderr, "kodama: %s and %s set different passwords for role %s\n", origins[role], name, role)
			return 2
		}
		passwords[role] = password
		origins[role] = name
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// Serialize the whole one-shot setup with a session-level advisory lock on
	// the maintenance database, held by the administrator connection until the
	// run ends. Concurrent `kodama migrate` processes then queue before the
	// database creation and the migration apply instead of racing them: without
	// this lock, both see a missing database/ledger and one loses noisily with
	// a duplicate-database or version conflict.
	//
	// When the maintenance database IS the target database the command does not
	// take this lock: Apply's own lock on the target connection already
	// serializes the run, and a second session-level lock on the same database
	// would wait on itself forever. EnsureDatabase is then a plain existence
	// check on a database that must already exist.
	if strings.Trim(admin.Path, "/") != database {
		lockDB, err := newMaintenanceDB(pgdriver.Config{ConnString: adminURL, MaxConns: 1})
		if err != nil {
			fmt.Fprintln(stderr, "kodama: maintenance database is unreachable")
			return 1
		}
		defer lockDB.Close()
		release, err := lockDB.LockSession(ctx, migrate.SetupLockKey)
		if err != nil {
			fmt.Fprintln(stderr, "kodama: maintenance database is unreachable")
			return 1
		}
		defer func() { _ = release(context.WithoutCancel(ctx)) }()
	}
	if err := pgdriver.EnsureDatabase(ctx, adminURL, database); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	db, err := pgdriver.New(pgdriver.Config{ConnString: targetAdmin.String(), MaxConns: 2})
	if err != nil {
		fmt.Fprintf(stderr, "kodama: connect as administrator: %v\n", err)
		return 1
	}
	defer db.Close()
	applied, err := migrate.Apply(ctx, db, migrate.Config{Database: database, Admin: admin.User.Username(), Passwords: passwords})
	for _, name := range applied {
		fmt.Fprintf(stdout, "applied %s\n", name)
	}
	if err != nil {
		fmt.Fprintf(stderr, "kodama: migrate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "database ready: %d migration(s) applied, %d role password(s) set\n", len(applied), len(passwords))
	return 0
}

// roleCredential returns the role and password a connection URL carries. Each
// refusal names the part of the URL that is missing, so a query-string
// password can never stand in for userinfo credentials.
func roleCredential(raw string) (role, password string, err error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "postgres" {
		return "", "", fmt.Errorf("must be a postgres:// URL")
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		return "", "", fmt.Errorf("must name a database user")
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		return "", "", fmt.Errorf("has no password")
	}
	return parsed.User.Username(), password, nil
}
