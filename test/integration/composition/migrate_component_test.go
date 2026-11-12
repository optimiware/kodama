//go:build component

// Package composition carries the composed-setup evidence that needs real
// infrastructure. Under the component tag the package holds three files: this
// one, migrate_command_component_test.go and test_profile_component_test.go.
// The integration-tagged composition suite is memory-backed apart from its live
// log test, and is never built together with these files.
//
// This file is the proof for `kodama migrate`'s administrator handover
// sequence against a real PostgreSQL server.
package composition

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/fault"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/app/migrate"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestMain installs the harness signal handler and orphan sweep for this
// component test binary.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}

func componentDSN(t *testing.T, shard *pgtest.Shard, role, database string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(shard.Addr)
	if err != nil {
		t.Fatalf("split postgres address %q: %v", shard.Addr, err)
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable&connect_timeout=10", role, host, port, database)
}

func componentDB(t *testing.T, shard *pgtest.Shard, role, database string) *pgdriver.DB {
	t.Helper()
	db, err := pgdriver.New(pgdriver.Config{ConnString: componentDSN(t, shard, role, database), MaxConns: 2})
	if err != nil {
		t.Fatalf("connect as %s to %s: %v", role, database, err)
	}
	t.Cleanup(db.Close)
	return db
}

// md5Verifier renders the exact pg_authid verifier PostgreSQL stores for a role
// when password_encryption is md5. It is a test-only way to prove the configured
// Secret value was set, without reconfiguring the container's authentication.
func md5Verifier(role, password string) string {
	sum := md5.Sum([]byte(password + role))
	return "md5" + hex.EncodeToString(sum[:])
}

func roleVerifier(t *testing.T, admin *pgdriver.DB, role string) string {
	t.Helper()
	value, err := pgtest.ScalarString(context.Background(), admin,
		`SELECT rolpassword FROM pg_authid WHERE rolname = $1`, role)
	if err != nil {
		t.Fatalf("read verifier for %s: %v", role, err)
	}
	return value
}

func migrationNames(t *testing.T, db *pgdriver.DB) []string {
	t.Helper()
	rows, err := pgtest.QueryStringsN(context.Background(), db, 1,
		`SELECT name FROM kodama_meta.schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row[0])
	}
	return names
}

func migrationChecksums(t *testing.T, db *pgdriver.DB) map[string]string {
	t.Helper()
	rows, err := pgtest.QueryStringsN(context.Background(), db, 2,
		`SELECT name, checksum FROM kodama_meta.schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	checksums := make(map[string]string, len(rows))
	for _, row := range rows {
		checksums[row[0]] = row[1]
	}
	return checksums
}

func databaseOwner(t *testing.T, admin *pgdriver.DB, database string) string {
	t.Helper()
	owner, err := pgtest.ScalarString(context.Background(), admin,
		`SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, database)
	if err != nil {
		t.Fatalf("read database owner: %v", err)
	}
	return owner
}

// TestMigrateRealPostgresNonSuperuserAdministrator is the real proof of the
// stage database setup: a CREATEROLE/CREATEDB, non-super user administrator
// creates the database, applies migration 0001 and the rest as kodama_owner,
// hands the database over, and a second run is a no-op that keeps the same
// passwords. After the handover the administrator can no longer create objects.
func TestMigrateRealPostgresNonSuperuserAdministrator(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	admin := shard.Admin()

	if _, err := pgtest.ExecSQL(ctx, admin, `CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatalf("create the non-superuser administrator: %v", err)
	}
	super, err := pgtest.ScalarString(ctx, admin, `SELECT rolsuper::text FROM pg_roles WHERE rolname = 'kodama_admin'`)
	if err != nil || super != "false" {
		t.Fatalf("administrator is superuser=%q (err=%v); the proof needs a non-superuser", super, err)
	}
	// Store deterministic MD5 verifiers so the test can prove which password
	// value was set. This is test-only server configuration. ALTER SYSTEM
	// cannot run inside a transaction, so it uses one raw session.
	raw, err := shard.RawSession(ctx, "postgres")
	if err != nil {
		t.Fatalf("open the configuration session: %v", err)
	}
	if _, err := raw.Exec(ctx, `ALTER SYSTEM SET password_encryption = 'md5'`); err != nil {
		t.Fatalf("set password_encryption: %v", err)
	}
	if _, err := raw.Exec(ctx, `SELECT pg_reload_conf()`); err != nil {
		t.Fatalf("reload server configuration: %v", err)
	}
	if err := raw.Close(ctx); err != nil {
		t.Fatalf("close the configuration session: %v", err)
	}

	const database = "kodama_admin_owned"
	if err := pgdriver.EnsureDatabase(ctx, componentDSN(t, shard, "kodama_admin", "postgres"), database); err != nil {
		t.Fatalf("create the application database as the administrator: %v", err)
	}
	appDB := componentDB(t, shard, "kodama_admin", database)
	superDB := componentDB(t, shard, "postgres", database)

	const (
		runtimePassword    = "runtime-p'w-SENTINEL"
		controllerPassword = `controller-"pw"-SENTINEL`
		publisherPassword  = "publisher-SENTINEL"
		authPassword       = "auth-SENTINEL"
	)
	passwords := map[string]string{
		"kodama_runtime":    runtimePassword,
		"kodama_controller": controllerPassword,
		"kodama_publisher":  publisherPassword,
		"kodama_auth":       authPassword,
	}
	config := migrate.Config{Database: database, Admin: "kodama_admin", Passwords: passwords}

	applied, err := migrate.Apply(ctx, appDB, config)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	want := storepg.Migrations()
	if len(applied) != len(want) {
		t.Fatalf("first run applied %d migrations; want %d", len(applied), len(want))
	}
	for index, name := range applied {
		if name != want[index].Name {
			t.Fatalf("applied[%d] = %q; want %q", index, name, want[index].Name)
		}
	}
	if owner := databaseOwner(t, superDB, database); owner != "kodama_owner" {
		t.Fatalf("database owner = %q; want kodama_owner after the handover", owner)
	}
	if rows := migrationNames(t, superDB); len(rows) != len(want) {
		t.Fatalf("ledger has %d rows; want %d", len(rows), len(want))
	}
	wantChecksums := make(map[string]string, len(want))
	for _, migration := range want {
		wantChecksums[migration.Name] = ledgerChecksum(migration.SQL)
	}
	for name, checksum := range migrationChecksums(t, superDB) {
		if wantChecksums[name] != checksum {
			t.Fatalf("ledger checksum for %s = %q; want %q", name, checksum, wantChecksums[name])
		}
	}

	for role, password := range passwords {
		if got, wantVerifier := roleVerifier(t, admin, role), md5Verifier(role, password); got != wantVerifier {
			t.Fatalf("role %s verifier = %q; want the configured password's verifier", role, got)
		}
	}
	owner, err := pgtest.ScalarString(ctx, admin, `SELECT concat_ws(',', rolcanlogin::text, rolsuper::text, rolbypassrls::text) FROM pg_roles WHERE rolname = 'kodama_owner'`)
	if err != nil || owner != "false,false,false" {
		t.Fatalf("kodama_owner attributes = %q (err=%v); want a NOLOGIN non-superuser NOBYPASSRLS owner", owner, err)
	}

	// A second run is a no-op and must not rotate the passwords: the Secret did
	// not change, so each role keeps the exact verifier the first run set.
	before := make(map[string]string, len(passwords))
	for role := range passwords {
		before[role] = roleVerifier(t, admin, role)
	}
	again, err := migrate.Apply(ctx, appDB, config)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run applied %v; want no migration", again)
	}
	if ledger := migrationNames(t, superDB); len(ledger) != len(want) {
		t.Fatalf("second run changed the ledger to %d rows; want %d", len(ledger), len(want))
	}
	for role := range passwords {
		if got := roleVerifier(t, admin, role); got != before[role] {
			t.Fatalf("second run rotated the password verifier of %s", role)
		}
	}

	// After the handover the administrator loses CREATE on the database and the
	// public schema, so it can no longer add objects the runtime relies on.
	if _, err := pgtest.ExecSQL(ctx, appDB, `CREATE TABLE public.post_migration_probe (id integer)`); err == nil {
		t.Fatal("the administrator created a table after the handover")
	} else {
		pgtest.RequirePermissionDenied(t, err, "administrator create table")
	}
	if _, err := pgtest.ExecSQL(ctx, appDB, `CREATE SCHEMA kodama_admin_probe`); err == nil {
		t.Fatal("the administrator created a schema after the handover")
	}
}

func ledgerChecksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestMigrateRealPostgresPartialFailureResumes proves a failed migration leaves
// the ledger exactly at the last committed migration and a retry resumes from
// the failed one, without applying anything twice.
func TestMigrateRealPostgresPartialFailureResumes(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	admin := shard.Admin()

	if _, err := pgtest.ExecSQL(ctx, admin, `CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatalf("create the non-superuser administrator: %v", err)
	}
	const database = "kodama_recovery"
	if err := pgdriver.EnsureDatabase(ctx, componentDSN(t, shard, "kodama_admin", "postgres"), database); err != nil {
		t.Fatalf("create the recovery database: %v", err)
	}
	superDB := componentDB(t, shard, "postgres", database)
	appDB := componentDB(t, shard, "kodama_admin", database)

	// 0002 creates the tenants table. A pre-existing table of the same name
	// makes exactly that migration fail after 0001 committed.
	if _, err := pgtest.ExecSQL(ctx, superDB, `CREATE TABLE public.tenants (placeholder integer)`); err != nil {
		t.Fatalf("pre-create the conflicting table: %v", err)
	}
	config := migrate.Config{
		Database: database, Admin: "kodama_admin",
		Passwords: map[string]string{"kodama_runtime": "SENTINEL-RECOVERY-PASSWORD"},
	}
	applied, err := migrate.Apply(ctx, appDB, config)
	if err == nil {
		t.Fatal("migrate succeeded despite the conflicting table")
	}
	if len(applied) != 1 || applied[0] != "0001_roles.sql" {
		t.Fatalf("partial run applied %v; want 0001 only", applied)
	}
	if strings.Contains(err.Error(), "SENTINEL-RECOVERY-PASSWORD") || strings.Contains(err.Error(), "CREATE TABLE") {
		t.Fatalf("partial failure rendered a statement or password: %v", err)
	}
	if names := migrationNames(t, superDB); len(names) != 1 {
		t.Fatalf("ledger after the failure = %v; want exactly the one committed migration", names)
	}

	if _, err := pgtest.ExecSQL(ctx, superDB, `DROP TABLE public.tenants`); err != nil {
		t.Fatalf("drop the conflicting table: %v", err)
	}
	resumed, err := migrate.Apply(ctx, appDB, config)
	if err != nil {
		t.Fatalf("resumed migrate: %v", err)
	}
	want := storepg.Migrations()
	if len(resumed) != len(want)-1 || resumed[0] != want[1].Name {
		t.Fatalf("resumed run applied %v; want %s..%s", resumed, want[1].Name, want[len(want)-1].Name)
	}
	if names := migrationNames(t, superDB); len(names) != len(want) {
		t.Fatalf("ledger after the retry = %d rows; want %d", len(names), len(want))
	}
}

// TestMigrateRealPostgresRefusesAdministratorWithoutCreateRole proves a
// configuration where the administrator cannot create roles is refused with a
// classified, secret-free error instead of half-applying the setup.
func TestMigrateRealPostgresRefusesAdministratorWithoutCreateRole(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	admin := shard.Admin()

	if _, err := pgtest.ExecSQL(ctx, admin, `CREATE ROLE kodama_weak LOGIN CREATEDB NOSUPERUSER NOCREATEROLE`); err != nil {
		t.Fatalf("create the weak administrator: %v", err)
	}
	const database = "kodama_weak_admin"
	weakURL := componentDSN(t, shard, "kodama_weak", "postgres")
	if err := pgdriver.EnsureDatabase(ctx, weakURL, database); err != nil {
		t.Fatalf("create the database as the weak administrator: %v", err)
	}
	appDB := componentDB(t, shard, "kodama_weak", database)

	const password = "SENTINEL-WEAK-ADMIN-PASSWORD"
	_, err := migrate.Apply(ctx, appDB, migrate.Config{
		Database: database, Admin: "kodama_weak",
		Passwords: map[string]string{"kodama_runtime": password},
	})
	if err == nil {
		t.Fatal("migrate succeeded as an administrator without CREATEROLE")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("refusal class = %v (found=%v); want forbidden (%v)", class, ok, err)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "CREATE ROLE") {
		t.Fatalf("refusal rendered a statement or password: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("refusal does not carry SQLSTATE 42501: %v", err)
	}
}
