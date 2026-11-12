// Package migrate applies the reviewed PostgreSQL migration set and sets the
// login passwords of the database roles, as the database administrator.
//
// It is the one-shot database setup step of an installation: the runtime never
// holds administrator rights, and migration 0001 creates roles and
// needs CREATEROLE. Applying is idempotent: a ledger records each applied
// migration with its checksum, an applied migration is never run twice, and a
// migration whose reviewed body changed after it was applied is refused.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
)

const ledger = "kodama_meta.schema_migrations"

// The bootstrap migration creates roles and therefore runs as the
// administrator; every other migration runs as the migration owner role.
var bootstrap = map[string]bool{"0001_roles.sql": true}

// SetupLockKey is the fixed advisory-lock key that serializes concurrent
// one-shot setup runs. Two `kodama migrate` processes started together (a
// rollout, or a Helm hook retried) must not both create the database or the
// ledger, or apply the same migration; the second waits and then observes a
// complete ledger instead of failing with a duplicate/version conflict. The
// command takes it on the maintenance connection before EnsureDatabase; Apply
// takes the same key on its target connection for callers that bypass the
// command.
const SetupLockKey int64 = 0x6b6f64616d61 // "kodama"

// DB opens one transaction as the database administrator.
type DB interface {
	Begin(context.Context) (storepg.Conn, error)
}

// SessionLocker is implemented by an administrator connection that can hold a
// session-level advisory lock on one dedicated connection for the whole setup
// run. It is optional so a pure fake (and any read-only checkout) need not
// implement it; the production PostgreSQL wrapper does, so concurrent real runs
// serialize before the ledger is created.
type SessionLocker interface {
	LockSession(ctx context.Context, key int64) (release func(context.Context) error, err error)
}

// Config is one setup run.
type Config struct {
	// Database is the database name the owner role takes over (ALTER DATABASE).
	Database string
	// Admin is the administrator role the connection runs as. It is granted the
	// owner role so it can run the other migrations as that role.
	Admin string
	// Passwords are the login passwords to set, by role. A role that does not
	// exist after the migrations is an error: a typo must not pass silently.
	Passwords map[string]string
}

// Apply runs the migrations, then sets the role passwords. It returns the names
// of the migrations it applied in this run.
func Apply(ctx context.Context, db DB, config Config) ([]string, error) {
	if db == nil || config.Database == "" || config.Admin == "" {
		return nil, fault.New(fault.InvalidInput, "migration requires a database connection, database name and administrator role")
	}
	// Validate every role password before the first statement: a role-name
	// typo must not run the migration set and then fail at the password step.
	for role, password := range config.Passwords {
		if !ValidRole(role) || password == "" || strings.ContainsRune(password, 0) {
			return nil, fault.New(fault.InvalidInput, "role password setting needs a kodama_ role name and a non-empty password")
		}
	}
	// Serialize the whole run with a session-level advisory lock taken on the
	// administrator connection. The lock is held across the ledger check, the
	// migration set and the password statements, so a concurrent run waits for a
	// complete ledger instead of racing the CREATE SCHEMA and failing noisily.
	if locker, ok := db.(SessionLocker); ok {
		release, err := locker.LockSession(ctx, SetupLockKey)
		if err != nil {
			return nil, migrationFault("acquire the setup lock", err)
		}
		if release != nil {
			defer func() { _ = release(context.WithoutCancel(ctx)) }()
		}
	}
	// The ledger is created once, while the administrator still owns the
	// database. After 0001 hands the database to the owner role the administrator
	// may no longer create schemas, so even IF NOT EXISTS would be refused.
	exists, err := ledgerExists(ctx, db)
	if err != nil {
		return nil, migrationFault("check migration ledger", err)
	}
	if !exists {
		if err := execOne(ctx, db, `CREATE SCHEMA kodama_meta;
CREATE TABLE `+ledger+` (
	name text PRIMARY KEY,
	checksum text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
			return nil, migrationFault("create migration ledger", err)
		}
	}
	var applied []string
	for _, migration := range storepg.Migrations() {
		sum := checksum(migration.SQL)
		recorded, found, err := lookup(ctx, db, migration.Name)
		if err != nil {
			return applied, migrationFault("read migration ledger", err)
		}
		if found {
			if recorded != sum {
				return applied, fault.New(fault.InvalidInput, fmt.Sprintf("applied migration %s was rewritten (recorded %s, reviewed %s)", migration.Name, recorded, sum))
			}
			continue
		}
		if err := applyOne(ctx, db, config, migration, sum); err != nil {
			return applied, migrationFault("apply "+migration.Name, err)
		}
		applied = append(applied, migration.Name)
	}
	for role, password := range config.Passwords {
		if err := setPassword(ctx, db, role, password); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// migrationFault keeps a driver error's class reachable but never renders its
// text: a statement, a value or a connection string in a private cause must not
// reach the command output. The public message names only the setup step.
func migrationFault(operation string, err error) error {
	class := fault.InternalFailure
	if classified, ok := fault.ClassOf(err); ok {
		class = classified
	}
	return fault.Wrap(class, operation+" failed", err)
}

func applyOne(ctx context.Context, db DB, config Config, migration storepg.Migration, sum string) error {
	conn, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if !bootstrap[migration.Name] {
		if _, err := conn.Exec(ctx, `SET LOCAL ROLE `+store.DefaultOwnerRole); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, body(migration.SQL)); err != nil {
		return err
	}
	if migration.Name == "0001_roles.sql" {
		// The administrator may not be a superuser: let it assume the owner role
		// it just created, then hand the database to that role, which the
		// 0001 body cannot do.
		//
		// Administrator rights after the handover are ADMIN plus SET on
		// kodama_owner, never INHERIT:
		//   - PostgreSQL 16+ records the role's creator as a grantor and gives
		//     it ADMIN on the role it created, so the administrator keeps ADMIN
		//     without a second (and refused) explicit ADMIN grant;
		//   - this statement adds SET and denies INHERIT, so the administrator
		//     keeps no direct CREATE on the database or the public schema and
		//     no direct SELECT on tenants;
		//   - it does keep ownership of the kodama_meta schema and the
		//     schema_migrations ledger it created before the handover;
		//   - it can SET ROLE kodama_owner at will (and grant that role onward),
		//     which is why the one-shot setup credential must be removed or
		//     disabled after the installation completes.
		// This is the documented design: the setup administrator is not a
		// runtime credential, and only the owner role holds object privileges.
		for _, statement := range []string{
			`GRANT ` + store.DefaultOwnerRole + ` TO ` + quoteIdent(config.Admin) + ` WITH INHERIT FALSE, SET TRUE`,
			`ALTER DATABASE ` + quoteIdent(config.Database) + ` OWNER TO ` + store.DefaultOwnerRole,
		} {
			if _, err := conn.Exec(ctx, statement); err != nil {
				return err
			}
		}
	}
	if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `INSERT INTO `+ledger+` (name, checksum) VALUES ($1, $2)`, migration.Name, sum); err != nil {
		return err
	}
	return conn.Commit(ctx)
}

func setPassword(ctx context.Context, db DB, role, password string) error {
	if !ValidRole(role) || password == "" || strings.ContainsRune(password, 0) {
		return fault.New(fault.InvalidInput, "role password setting needs a kodama_ role name and a non-empty password")
	}
	// ALTER ROLE does not take bind parameters; the literal is quoted. It is
	// emitted as an E'' escape string with BOTH backslashes and single quotes
	// doubled, so the statement is a literal regardless of the session's
	// standard_conforming_strings setting. A plain '...' literal is injectable
	// with standard_conforming_strings=off: a password containing \' closes the
	// quote and appends SQL. The password never appears in an error: the
	// statement text is not returned.
	//
	// Note for operators: PostgreSQL logs ALTER ROLE ... PASSWORD 'literal' in
	// clear text when log_statement covers it; the chart generates the role
	// passwords as hex, and statement logging must not be enabled on the
	// one-shot setup connection.
	if err := execOne(ctx, db, `ALTER ROLE `+quoteIdent(role)+` PASSWORD `+quotePassword(password)); err != nil {
		return fault.New(fault.DependencyUnavailable, fmt.Sprintf("set password of role %s failed", role))
	}
	return nil
}

// ValidRole reports whether the name is one of the database's login roles.
// Only lowercase kodama_ names are accepted: a password is never set on an
// operator or superuser role by a typo.
func ValidRole(role string) bool {
	if !strings.HasPrefix(role, "kodama_") || len(role) == len("kodama_") {
		return false
	}
	for _, r := range role {
		if !(r >= 'a' && r <= 'z' || r == '_') {
			return false
		}
	}
	return true
}

func ledgerExists(ctx context.Context, db DB) (bool, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, `SELECT to_regclass('`+ledger+`') IS NOT NULL`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return false, rows.Err()
	}
	var exists bool
	if err := rows.Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func lookup(ctx context.Context, db DB, name string) (string, bool, error) {
	conn, err := db.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, `SELECT checksum FROM `+ledger+` WHERE name = $1`, name)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var sum string
	if err := rows.Scan(&sum); err != nil {
		return "", false, err
	}
	return sum, true, nil
}

func execOne(ctx context.Context, db DB, statement string) error {
	conn, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, statement); err != nil {
		return err
	}
	return conn.Commit(ctx)
}

// body strips standalone BEGIN/COMMIT lines so the runner's transaction owns
// atomicity when a migration brackets its statements in its own.
func body(sql string) string {
	lines := strings.Split(sql, "\n")
	kept := lines[:0:0]
	for _, line := range lines {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "BEGIN;", "BEGIN", "COMMIT;", "COMMIT":
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func checksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// quotePassword renders one SQL string literal that is safe under both
// standard_conforming_strings settings. In an E” string a backslash is an
// escape character, so every backslash and every single quote is doubled; the
// resulting literal denotes exactly the input bytes and cannot terminate the
// statement early.
func quotePassword(password string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `''`)
	return `E'` + replacer.Replace(password) + `'`
}
