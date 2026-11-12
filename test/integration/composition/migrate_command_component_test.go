//go:build component

// This file proves `kodama migrate` end to end through the real CLI binary and
// a real PostgreSQL server: the target database comes from
// KODAMA_DATABASE_URL, EnsureDatabase creates it, the reviewed migrations and
// role passwords are applied once, concurrent runs serialize, and no password
// or connection URL ever reaches stdout or stderr.
package composition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// kodamaCLI builds the real command binary once per test into its temporary
// directory. The component proof drives the same entrypoint the pod runs.
func kodamaCLI(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "kodama")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kodama")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kodama: %v\n%s", err, output)
	}
	return binary
}

func moduleRoot() (string, error) {
	working, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(working, "..", "..", ".."))
}

// commandEnvironment renders one isolated child environment: only the explicit
// map plus PATH, never the host's KODAMA_* values.
func commandEnvironment(values map[string]string) []string {
	environment := []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range values {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// runMigrateBinary runs the built binary with the supplied environment and
// captures stdout, stderr and the exit code. It is safe to call from a test
// goroutine: it reports failures as errors instead of calling t.Fatal.
func runMigrateBinary(binary string, values map[string]string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "migrate")
	command.Env = commandEnvironment(values)
	var parseOut, errOut bytes.Buffer
	command.Stdout = &parseOut
	command.Stderr = &errOut
	runErr := command.Run()
	if ctx.Err() != nil {
		return parseOut.String(), errOut.String(), -1, fmt.Errorf("kodama migrate timed out: %w", ctx.Err())
	}
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			return parseOut.String(), errOut.String(), -1, fmt.Errorf("run kodama migrate: %w\nstderr: %s", runErr, errOut.String())
		}
		return parseOut.String(), errOut.String(), exit.ExitCode(), nil
	}
	return parseOut.String(), errOut.String(), 0, nil
}

// runMigrateCommand builds one binary and runs it once.
func runMigrateCommand(t *testing.T, values map[string]string) (stdout, stderr string, code int) {
	t.Helper()
	stdout, stderr, code, err := runMigrateBinary(kodamaCLI(t), values)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return stdout, stderr, code
}

func hostPort(t *testing.T, address string) (string, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split %q: %v", address, err)
	}
	return host, port
}

// roleURL renders one connection URL whose password is URL-encoded, so a
// password with quotes, slashes or spaces is carried exactly.
func roleURL(t *testing.T, host, port, role, password, database string) string {
	t.Helper()
	parsed := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable&connect_timeout=10",
	}
	return parsed.String()
}

// migrateCommandEnvironment is one complete `kodama migrate` environment for a
// target database, with sentinel passwords so every output can be checked.
type migrateCommandEnv struct {
	values    map[string]string
	passwords map[string]string
	adminURL  string
	roleURLs  []string
	database  string
}

func migrateCommandFixture(t *testing.T, shard *pgtest.Shard, adminPassword, database string) migrateCommandEnv {
	t.Helper()
	ctx := context.Background()
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS PASSWORD '`+adminPassword+`'`); err != nil {
		t.Fatalf("create the non-superuser administrator: %v", err)
	}
	host, port := hostPort(t, shard.Addr)
	passwords := map[string]string{
		"kodama_runtime":    "runtime-SENTINEL-p'w",
		"kodama_controller": `controller-"pw"-SENTINEL`,
		"kodama_publisher":  "publisher-SENTINEL",
		"kodama_auth":       "auth-SENTINEL",
	}
	roles := []struct{ env, role string }{
		{"KODAMA_DATABASE_URL", "kodama_runtime"},
		{"KODAMA_CONTROLLER_DATABASE_URL", "kodama_controller"},
		{"KODAMA_PUBLISHER_DATABASE_URL", "kodama_publisher"},
		{"KODAMA_AUTH_DATABASE_URL", "kodama_auth"},
	}
	values := map[string]string{
		"KODAMA_ADMIN_DATABASE_URL": roleURL(t, host, port, "kodama_admin", adminPassword, "postgres"),
	}
	var urls []string
	for _, entry := range roles {
		value := roleURL(t, host, port, entry.role, passwords[entry.role], database)
		values[entry.env] = value
		urls = append(urls, value)
	}
	return migrateCommandEnv{
		values:    values,
		passwords: passwords,
		adminURL:  values["KODAMA_ADMIN_DATABASE_URL"],
		roleURLs:  urls,
		database:  database,
	}
}

// assertSecretFreeMigrateOutput proves no password and no connection URL
// reached either stream, on success and on failure.
func assertSecretFreeMigrateOutput(t *testing.T, environment migrateCommandEnv, stdout, stderr string) {
	t.Helper()
	combined := stdout + stderr
	for _, password := range environment.passwords {
		if strings.Contains(combined, password) {
			t.Fatalf("migrate output rendered a role password: %q", combined)
		}
	}
	for _, value := range append([]string{environment.adminURL}, environment.roleURLs...) {
		if strings.Contains(combined, value) {
			t.Fatalf("migrate output rendered a connection URL: %q", combined)
		}
	}
}

func ledgerRows(t *testing.T, shard *pgtest.Shard, database string) int {
	t.Helper()
	super := componentDB(t, shard, "postgres", database)
	rows, err := pgtest.QueryStringsN(context.Background(), super, 1,
		`SELECT name FROM kodama_meta.schema_migrations`)
	if err != nil {
		t.Fatalf("read the migration ledger: %v", err)
	}
	return len(rows)
}

// TestMigrateCommandRealPostgres runs the real command end to end: the database
// named by KODAMA_DATABASE_URL is created and handed to kodama_owner, every
// reviewed migration is applied, the four role passwords are set, and a second
// run is a no-op with the same secret-free output class.
func TestMigrateCommandRealPostgres(t *testing.T) {
	shard := pgtest.Start(t)
	const database = "kodama_cli_owned"
	environment := migrateCommandFixture(t, shard, "admin-SENTINEL-pw", database)

	stdout, stderr, code := runMigrateCommand(t, environment.values)
	if code != 0 {
		t.Fatalf("first migrate exit code = %d; want 0 (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("successful migrate wrote stderr: %q", stderr)
	}
	want := len(storepg.Migrations())
	if !strings.Contains(stdout, fmt.Sprintf("%d migration(s) applied", want)) {
		t.Fatalf("stdout = %q; want %d applied migrations", stdout, want)
	}
	assertSecretFreeMigrateOutput(t, environment, stdout, stderr)

	// The database named by KODAMA_DATABASE_URL exists and the handover left
	// kodama_owner as its owner; the ledger holds every reviewed migration.
	if owner := databaseOwner(t, shard.Admin(), database); owner != "kodama_owner" {
		t.Fatalf("database owner = %q; want kodama_owner", owner)
	}
	if rows := ledgerRows(t, shard, database); rows != want {
		t.Fatalf("ledger rows = %d; want %d", rows, want)
	}
	for role := range environment.passwords {
		verifier := roleVerifier(t, shard.Admin(), role)
		if verifier == "" {
			t.Fatalf("role %s has no password after migrate", role)
		}
	}

	// A second run is a no-op: no migration applies, the owner and passwords
	// stay as they were, and the output keeps the same secret-free class.
	secondStdout, secondStderr, secondCode := runMigrateCommand(t, environment.values)
	if secondCode != 0 {
		t.Fatalf("second migrate exit code = %d; want 0 (stderr=%q)", secondCode, secondStderr)
	}
	if !strings.Contains(secondStdout, "0 migration(s) applied") {
		t.Fatalf("second stdout = %q; want a no-op run", secondStdout)
	}
	assertSecretFreeMigrateOutput(t, environment, secondStdout, secondStderr)
	if owner := databaseOwner(t, shard.Admin(), database); owner != "kodama_owner" {
		t.Fatalf("second run changed the database owner to %q", owner)
	}
	if rows := ledgerRows(t, shard, database); rows != want {
		t.Fatalf("second run changed the ledger to %d rows; want %d", rows, want)
	}

	// An administrator whose maintenance database IS the target database (a
	// legitimate re-run configuration) must not wait on its own setup lock.
	host, port := hostPort(t, shard.Addr)
	sameDatabase := copyEnvironment(environment.values)
	sameDatabase["KODAMA_ADMIN_DATABASE_URL"] = roleURL(t, host, port, "kodama_admin", "admin-SENTINEL-pw", database)
	sameStdout, sameStderr, sameCode := runMigrateCommand(t, sameDatabase)
	if sameCode != 0 {
		t.Fatalf("same-database migrate exit code = %d; want 0 (stderr=%q)", sameCode, sameStderr)
	}
	if !strings.Contains(sameStdout, "0 migration(s) applied") {
		t.Fatalf("same-database stdout = %q; want a no-op run", sameStdout)
	}
}

// TestMigrateCommandRefusesTamperedLedger proves an Apply failure through the
// real binary exits 1 (never 0 or 2) and stays secret-free. The ledger is
// valid on the first run; one checksum is then rewritten, so the second run
// reaches the ledger check and fails there.
func TestMigrateCommandRefusesTamperedLedger(t *testing.T) {
	shard := pgtest.Start(t)
	const database = "kodama_cli_tampered"
	environment := migrateCommandFixture(t, shard, "admin-SENTINEL-pw", database)

	if stdout, stderr, code := runMigrateCommand(t, environment.values); code != 0 {
		t.Fatalf("first migrate exit code = %d; want 0 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	super := componentDB(t, shard, "postgres", database)
	if _, err := pgtest.ExecSQL(context.Background(), super,
		`UPDATE kodama_meta.schema_migrations SET checksum = 'sha256:tampered' WHERE name = '0001_roles.sql'`); err != nil {
		t.Fatalf("tamper the migration ledger: %v", err)
	}

	stdout, stderr, code := runMigrateCommand(t, environment.values)
	if code != 1 {
		t.Fatalf("tampered-ledger exit code = %d; want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("a refused migration wrote stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "migrate:") || !strings.Contains(stderr, "was rewritten") {
		t.Fatalf("stderr = %q; want the rewritten-migration diagnostic", stderr)
	}
	assertSecretFreeMigrateOutput(t, environment, stdout, stderr)
	if strings.Contains(stderr, "postgres://") {
		t.Fatalf("stderr rendered a connection URL: %q", stderr)
	}
}

// TestMigrateCommandRealPostgresRuntimeFailures pins the failure contract: a
// runtime failure (unreachable host, wrong password, nonexistent role) exits 1
// and a configuration failure exits 2, with no password or URL in either
// stream.
func TestMigrateCommandRealPostgresRuntimeFailures(t *testing.T) {
	shard := pgtest.Start(t)
	environment := migrateCommandFixture(t, shard, "admin-SENTINEL-pw", "kodama_cli_failures")

	t.Run("unreachable maintenance database", func(t *testing.T) {
		values := copyEnvironment(environment.values)
		values["KODAMA_ADMIN_DATABASE_URL"] = roleURL(t, "127.0.0.1", "1", "kodama_admin", "admin-SENTINEL-pw", "postgres")
		stdout, stderr, code := runMigrateCommand(t, values)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 (stderr=%q)", code, stderr)
		}
		if !strings.Contains(stderr, "maintenance database is unreachable") {
			t.Fatalf("stderr = %q; want the fixed unreachable diagnostic", stderr)
		}
		assertSecretFreeMigrateOutput(t, environment, stdout, stderr)
	})

	t.Run("wrong administrator password", func(t *testing.T) {
		address, superuser := startPasswordShard(t)
		ctx := context.Background()
		connection, err := pgx.Connect(ctx, superuser)
		if err != nil {
			t.Fatalf("connect to the password shard: %v", err)
		}
		if _, err := connection.Exec(ctx,
			`CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS PASSWORD 'right-SENTINEL-pw'`); err != nil {
			connection.Close(ctx)
			t.Fatalf("create the administrator: %v", err)
		}
		connection.Close(ctx)

		host, port := hostPort(t, address)
		values := map[string]string{
			"KODAMA_ADMIN_DATABASE_URL":      roleURL(t, host, port, "kodama_admin", "wrong-SENTINEL-pw", "postgres"),
			"KODAMA_DATABASE_URL":            roleURL(t, host, port, "kodama_runtime", "runtime-SENTINEL", "kodama_password_failure"),
			"KODAMA_CONTROLLER_DATABASE_URL": roleURL(t, host, port, "kodama_controller", "controller-SENTINEL", "kodama_password_failure"),
			"KODAMA_PUBLISHER_DATABASE_URL":  roleURL(t, host, port, "kodama_publisher", "publisher-SENTINEL", "kodama_password_failure"),
			"KODAMA_AUTH_DATABASE_URL":       roleURL(t, host, port, "kodama_auth", "auth-SENTINEL", "kodama_password_failure"),
		}
		stdout, stderr, code := runMigrateCommand(t, values)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 for an authentication failure (stderr=%q)", code, stderr)
		}
		combined := stdout + stderr
		for _, secret := range []string{"wrong-SENTINEL-pw", "right-SENTINEL-pw", "postgres://"} {
			if strings.Contains(combined, secret) {
				t.Fatalf("wrong-password output rendered %q: %q", secret, combined)
			}
		}
	})

	t.Run("missing role URL is configuration", func(t *testing.T) {
		values := copyEnvironment(environment.values)
		values["KODAMA_AUTH_DATABASE_URL"] = ""
		stdout, stderr, code := runMigrateCommand(t, values)
		if code != 2 {
			t.Fatalf("exit code = %d; want 2 for a configuration failure (stderr=%q)", code, stderr)
		}
		if !strings.Contains(stderr, "KODAMA_AUTH_DATABASE_URL") {
			t.Fatalf("stderr = %q; want the missing setting named", stderr)
		}
		assertSecretFreeMigrateOutput(t, environment, stdout, stderr)
	})

	t.Run("administrator without CREATEDB", func(t *testing.T) {
		// The maintenance connection is reachable and takes the setup lock, so
		// this exercises the database-creation failure itself: a runtime
		// failure exits 1 and its diagnostic stays secret-free.
		if _, err := pgtest.ExecSQL(context.Background(), shard.Admin(),
			`CREATE ROLE kodama_nocreatedb LOGIN CREATEROLE NOCREATEDB NOSUPERUSER NOBYPASSRLS`); err != nil {
			t.Fatalf("create the weak administrator: %v", err)
		}
		values := copyEnvironment(environment.values)
		host, port := hostPort(t, shard.Addr)
		values["KODAMA_ADMIN_DATABASE_URL"] = roleURL(t, host, port, "kodama_nocreatedb", "weak-SENTINEL-pw", "postgres")
		stdout, stderr, code := runMigrateCommand(t, values)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 for a database-creation failure (stderr=%q)", code, stderr)
		}
		if !strings.Contains(stderr, "database creation failed") {
			t.Fatalf("stderr = %q; want the fixed creation diagnostic", stderr)
		}
		for _, secret := range []string{values["KODAMA_ADMIN_DATABASE_URL"], "weak-SENTINEL-pw"} {
			if strings.Contains(stdout+stderr, secret) {
				t.Fatalf("failure output rendered the weak administrator URL or password: %q", stdout+stderr)
			}
		}
		assertSecretFreeMigrateOutput(t, environment, stdout, stderr)
	})

	t.Run("nonexistent administrator role", func(t *testing.T) {
		values := copyEnvironment(environment.values)
		host, port := hostPort(t, shard.Addr)
		values["KODAMA_ADMIN_DATABASE_URL"] = roleURL(t, host, port, "kodama_absent", "absent-SENTINEL-pw", "postgres")
		stdout, stderr, code := runMigrateCommand(t, values)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 (stderr=%q)", code, stderr)
		}
		assertSecretFreeMigrateOutput(t, environment, stdout, stderr)
	})
}

// TestMigrateCommandParallelRunsConverge proves two simultaneous setup runs
// against one database both succeed: the session advisory lock serializes the
// ledger creation, so neither loses with a duplicate-schema/version conflict.
func TestMigrateCommandParallelRunsConverge(t *testing.T) {
	shard := pgtest.Start(t)
	const database = "kodama_cli_parallel"
	environment := migrateCommandFixture(t, shard, "admin-SENTINEL-pw", database)

	type result struct {
		stdout string
		stderr string
		code   int
	}
	binary := kodamaCLI(t)
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	var runners sync.WaitGroup
	for range 2 {
		runners.Go(func() {
			start.Wait()
			stdout, stderr, code, err := runMigrateBinary(binary, environment.values)
			if err != nil {
				results <- result{stdout: stdout, stderr: err.Error(), code: -1}
				return
			}
			results <- result{stdout: stdout, stderr: stderr, code: code}
		})
	}
	start.Done()
	runners.Wait()
	close(results)

	appliedTotal := 0
	for outcome := range results {
		if outcome.code != 0 {
			t.Fatalf("a parallel migrate exited %d: %q", outcome.code, outcome.stderr)
		}
		assertSecretFreeMigrateOutput(t, environment, outcome.stdout, outcome.stderr)
		if index := strings.Index(outcome.stdout, "database ready: "); index >= 0 {
			var count int
			_, _ = fmt.Sscanf(outcome.stdout[index+len("database ready: "):], "%d", &count)
			appliedTotal += count
		}
	}
	want := len(storepg.Migrations())
	if appliedTotal != want {
		t.Fatalf("parallel runs applied %d migrations in total; want %d", appliedTotal, want)
	}
	if rows := ledgerRows(t, shard, database); rows != want {
		t.Fatalf("ledger rows = %d; want %d", rows, want)
	}
	if owner := databaseOwner(t, shard.Admin(), database); owner != "kodama_owner" {
		t.Fatalf("database owner = %q; want kodama_owner", owner)
	}
}

// TestMigrateCommandPasswordLiteralIsNotInjectable proves a password containing
// the classic SQL-injection tail cannot create a role, even when the
// administrator's session runs with standard_conforming_strings=off. The E”
// literal doubles backslashes and quotes, so PostgreSQL stores exactly the
// configured bytes.
func TestMigrateCommandPasswordLiteralIsNotInjectable(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	admin := shard.Admin()

	// A deterministic verifier (md5) proves which exact bytes were stored.
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

	const adminPassword = "admin-SENTINEL-pw"
	if _, err := pgtest.ExecSQL(ctx, admin,
		`CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS PASSWORD '`+adminPassword+`'`); err != nil {
		t.Fatalf("create the non-superuser administrator: %v", err)
	}
	// The role-level setting makes every new administrator session use the
	// pre-9.1 literal rules, the exact condition under which a plain quoted
	// password is injectable.
	if _, err := pgtest.ExecSQL(ctx, admin, `ALTER ROLE kodama_admin SET standard_conforming_strings = off`); err != nil {
		t.Fatalf("disable standard_conforming_strings for the administrator: %v", err)
	}

	const database = "kodama_cli_injection"
	const injectedPassword = `\'; CREATE ROLE kodama_evil2; --`
	host, port := hostPort(t, shard.Addr)
	values := map[string]string{
		"KODAMA_ADMIN_DATABASE_URL":      roleURL(t, host, port, "kodama_admin", adminPassword, "postgres"),
		"KODAMA_DATABASE_URL":            roleURL(t, host, port, "kodama_runtime", injectedPassword, database),
		"KODAMA_CONTROLLER_DATABASE_URL": roleURL(t, host, port, "kodama_controller", "controller-SENTINEL", database),
		"KODAMA_PUBLISHER_DATABASE_URL":  roleURL(t, host, port, "kodama_publisher", "publisher-SENTINEL", database),
		"KODAMA_AUTH_DATABASE_URL":       roleURL(t, host, port, "kodama_auth", "auth-SENTINEL", database),
	}
	stdout, stderr, code := runMigrateCommand(t, values)
	if code != 0 {
		t.Fatalf("migrate exit code = %d; want 0 (stderr=%q)", code, stderr)
	}
	if strings.Contains(stdout+stderr, injectedPassword) {
		t.Fatalf("migrate output rendered the injected password: %q", stdout+stderr)
	}
	evil, err := pgtest.ScalarString(ctx, admin, `SELECT count(*)::text FROM pg_roles WHERE rolname = 'kodama_evil2'`)
	if err != nil {
		t.Fatalf("read the injected role: %v", err)
	}
	if evil != "0" {
		t.Fatalf("the injected password created %s kodama_evil2 role(s)", evil)
	}
	if got, want := roleVerifier(t, admin, "kodama_runtime"), md5Verifier("kodama_runtime", injectedPassword); got != want {
		t.Fatalf("kodama_runtime verifier = %q; want the exact configured password's verifier", got)
	}
}

// TestEnsureDatabaseIsIdempotentRealPostgres proves the existing-database
// branch: a second EnsureDatabase for the same name is a clean no-op.
func TestEnsureDatabaseIsIdempotentRealPostgres(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatalf("create the administrator: %v", err)
	}
	host, port := hostPort(t, shard.Addr)
	maintenance := roleURL(t, host, port, "kodama_admin", "unused", "postgres")
	const database = "kodama_ensure_twice"
	for run := 1; run <= 2; run++ {
		if err := pgdriver.EnsureDatabase(ctx, maintenance, database); err != nil {
			t.Fatalf("EnsureDatabase run %d: %v", run, err)
		}
	}
	if owner := databaseOwner(t, shard.Admin(), database); owner != "kodama_admin" {
		t.Fatalf("database owner = %q; want the connecting administrator", owner)
	}
}

// startPasswordShard starts one isolated PostgreSQL server that requires
// password authentication (scram-sha-256), so a wrong password is actually
// refused instead of being accepted by the trust shards. It returns the
// server address and a superuser connection URL.
func startPasswordShard(t *testing.T) (string, string) {
	t.Helper()
	hs := harness.NewShard(t)
	hs.RequireImage(hs.Context(), harness.ImagePostgres)
	container, err := hs.Run(hs.Context(), harness.RunSpec{
		Image: harness.ImagePostgres,
		Ports: []harness.Port{{Container: 5432}},
		Env: map[string]string{
			"POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
			"POSTGRES_PASSWORD":         "superuser-SENTINEL-pw",
			"POSTGRES_USER":             "postgres",
			"POSTGRES_DB":               "postgres",
		},
	})
	if err != nil {
		t.Fatalf("start the password-authenticated postgres: %v", err)
	}
	address, err := container.Addr(hs.Context(), 5432)
	if err != nil {
		t.Fatalf("password shard address: %v", err)
	}
	if err := hs.WaitReady(hs.Context(), "postgres", harness.PostgresProbe(address, "postgres")); err != nil {
		logs, _ := container.Logs(hs.Context())
		t.Fatalf("password shard readiness: %v\n%s", err, logs)
	}
	host, port := hostPort(t, address)
	return address, roleURL(t, host, port, "postgres", "superuser-SENTINEL-pw", "postgres")
}

func copyEnvironment(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}
