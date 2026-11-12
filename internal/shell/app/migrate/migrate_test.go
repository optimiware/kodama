package migrate_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/app/migrate"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
)

// fakeDB is an in-memory ledger plus a statement log. It stands in for the
// administrator connection: the real SQL runs in the component suites.
type fakeDB struct {
	ledgerCreated bool
	ledger        map[string]string
	log           []string
	failOn        string
	failErr       error
	begins        int
	commits       int
}

type fakeConn struct {
	db      *fakeDB
	pending map[string]string
}

func (db *fakeDB) Begin(context.Context) (storepg.Conn, error) {
	db.begins++
	return &fakeConn{db: db, pending: map[string]string{}}, nil
}

func (c *fakeConn) Exec(_ context.Context, sql string, args ...any) (store.Result, error) {
	short, _, _ := strings.Cut(strings.TrimSpace(sql), "\n")
	c.db.log = append(c.db.log, short)
	if strings.HasPrefix(sql, "CREATE SCHEMA kodama_meta") {
		if c.db.ledgerCreated {
			return store.Result{}, errors.New("permission denied for database: the ledger is created once")
		}
		c.db.ledgerCreated = true
	}
	if c.db.failOn != "" && strings.Contains(sql, c.db.failOn) {
		if c.db.failErr != nil {
			return store.Result{}, c.db.failErr
		}
		return store.Result{}, errors.New("boom")
	}
	if strings.HasPrefix(sql, "INSERT INTO") {
		c.pending[args[0].(string)] = args[1].(string)
	}
	return store.Result{}, nil
}

func (c *fakeConn) Query(_ context.Context, _ string, args ...any) (store.Rows, error) {
	if len(args) == 0 {
		return &fakeBool{value: c.db.ledgerCreated}, nil
	}
	sum, found := c.db.ledger[args[0].(string)]
	return &fakeRows{sum: sum, has: found}, nil
}

func (c *fakeConn) Commit(context.Context) error {
	maps.Copy(c.db.ledger, c.pending)
	c.pending = map[string]string{}
	c.db.commits++
	return nil
}
func (c *fakeConn) Rollback(context.Context) error { return nil }

type fakeRows struct {
	sum  string
	has  bool
	read bool
}

func (r *fakeRows) Next() bool {
	if !r.has || r.read {
		return false
	}
	r.read = true
	return true
}
func (r *fakeRows) Scan(dest ...any) error { *(dest[0].(*string)) = r.sum; return nil }
func (r *fakeRows) Err() error             { return nil }
func (r *fakeRows) Close() error           { return nil }

func TestApplyRunsEveryMigrationOnceThenOnlyPasswords(t *testing.T) {
	db := &fakeDB{ledger: map[string]string{}}
	cfg := migrate.Config{Database: "kodama", Admin: "admin", Passwords: map[string]string{"kodama_runtime": "p'w"}}
	applied, err := migrate.Apply(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != len(storepg.Migrations()) || applied[0] != "0001_roles.sql" {
		t.Fatalf("applied %d migrations, first %v", len(applied), applied)
	}
	joined := strings.Join(db.log, "\n")
	for _, want := range []string{
		`GRANT kodama_owner TO "admin" WITH INHERIT FALSE, SET TRUE`,
		`ALTER DATABASE "kodama" OWNER TO kodama_owner`,
		`ALTER ROLE "kodama_runtime" PASSWORD E'p''w'`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing statement %q in:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "SET LOCAL ROLE kodama_owner") != len(applied)-1 {
		t.Fatalf("only the role-creating migration may run as the administrator:\n%s", joined)
	}

	again, err := migrate.Apply(context.Background(), db, cfg)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run applied %v, err %v", again, err)
	}
}

func TestApplyRefusesRewrittenMigrationAndBadRoles(t *testing.T) {
	db := &fakeDB{ledger: map[string]string{"0001_roles.sql": "sha256:old"}}
	if _, err := migrate.Apply(context.Background(), db, migrate.Config{Database: "kodama", Admin: "admin"}); err == nil {
		t.Fatal("a rewritten applied migration was accepted")
	}
	for _, role := range []string{"postgres", "kodama_", "kodama_x; DROP", "kodama_Runtime"} {
		db := &fakeDB{ledger: map[string]string{}}
		_, err := migrate.Apply(context.Background(), db, migrate.Config{Database: "kodama", Admin: "admin", Passwords: map[string]string{role: "x"}})
		if err == nil {
			t.Fatalf("role %q was accepted", role)
		}
	}
	// The driver cause of a password failure carries the statement (and thus
	// the password); the returned error must not render it.
	const leaked = "SENTINEL-SETPW-CAUSE"
	db = &fakeDB{
		ledger:  map[string]string{},
		failOn:  "ALTER ROLE",
		failErr: errors.New(`syntax error at or near "ALTER ROLE kodama_runtime PASSWORD '` + leaked + `'"`),
	}
	_, err := migrate.Apply(context.Background(), db, migrate.Config{Database: "kodama", Admin: "admin", Passwords: map[string]string{"kodama_runtime": "secret-pw"}})
	if err == nil || strings.Contains(err.Error(), "secret-pw") {
		t.Fatalf("password failure must not echo the password: %v", err)
	}
	if strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), "ALTER ROLE") {
		t.Fatalf("password failure rendered the driver cause: %v", err)
	}
	if !strings.Contains(err.Error(), "set password of role kodama_runtime failed") {
		t.Fatalf("error = %v; want it to name only the failed step", err)
	}
}

// TestApplyNeverRendersADriverStatementOrPassword proves a private driver
// diagnostic (which may quote the failing statement and its arguments) never
// reaches the returned error. The class stays reachable for the caller.
func TestApplyNeverRendersADriverStatementOrPassword(t *testing.T) {
	const sentinel = "SENTINEL-STATEMENT-AND-PASSWORD"
	db := &fakeDB{
		ledger:  map[string]string{},
		failOn:  "CREATE ROLE",
		failErr: errors.New(`syntax error at or near "ALTER ROLE kodama_runtime PASSWORD '` + sentinel + `'"`),
	}
	_, err := migrate.Apply(context.Background(), db, migrate.Config{
		Database: "kodama", Admin: "admin",
		Passwords: map[string]string{"kodama_runtime": sentinel},
	})
	if err == nil {
		t.Fatal("Apply accepted a failing driver statement")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "ALTER ROLE") {
		t.Fatalf("Apply rendered a statement or password: %v", err)
	}
	if !strings.Contains(err.Error(), "apply 0001_roles.sql failed") {
		t.Fatalf("error = %v; want it to name the setup step only", err)
	}
}

// TestApplyValidatesRolesBeforeOpeningTheLedger proves a role-name typo is
// refused before any database work, so a bad configuration cannot run the
// migration set and fail at the password step.
func TestApplyValidatesRolesBeforeOpeningTheLedger(t *testing.T) {
	for _, tc := range []struct {
		name      string
		passwords map[string]string
	}{
		{"non-kodama role", map[string]string{"postgres": "x"}},
		{"empty password", map[string]string{"kodama_runtime": ""}},
		{"nul password", map[string]string{"kodama_runtime": "a\x00b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDB{ledger: map[string]string{}}
			_, err := migrate.Apply(context.Background(), db, migrate.Config{
				Database: "kodama", Admin: "admin",
				Passwords: tc.passwords,
			})
			if err == nil {
				t.Fatalf("Apply accepted %s", tc.name)
			}
			if db.begins != 0 || len(db.log) != 0 {
				t.Fatalf("Apply opened %d connections and logged %v before refusing the password; want zero", db.begins, db.log)
			}
		})
	}
}

// TestApplyUsesTheConfiguredPasswordOnEveryRun proves the role password is the
// Secret's value, never a freshly generated one: both runs set exactly the
// configured literal, and no other role is altered.
func TestApplyUsesTheConfiguredPasswordOnEveryRun(t *testing.T) {
	db := &fakeDB{ledger: map[string]string{}}
	const password = "SENTINEL-STABLE-PASSWORD"
	cfg := migrate.Config{Database: "kodama", Admin: "admin", Passwords: map[string]string{"kodama_runtime": password}}

	if _, err := migrate.Apply(context.Background(), db, cfg); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := migrate.Apply(context.Background(), db, cfg); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	wantStatement := `ALTER ROLE "kodama_runtime" PASSWORD E'SENTINEL-STABLE-PASSWORD'`
	seen := 0
	for _, statement := range db.log {
		if strings.HasPrefix(statement, "ALTER ROLE") {
			seen++
			if statement != wantStatement {
				t.Fatalf("password statement = %q; want %q", statement, wantStatement)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("ALTER ROLE statements = %d; want one per run", seen)
	}
}

type fakeBool struct {
	value bool
	read  bool
}

func (r *fakeBool) Next() bool {
	if r.read {
		return false
	}
	r.read = true
	return true
}
func (r *fakeBool) Scan(dest ...any) error { *(dest[0].(*bool)) = r.value; return nil }
func (r *fakeBool) Err() error             { return nil }
func (r *fakeBool) Close() error           { return nil }

// lockingFakeDB is the administrator connection plus the session-lock seam.
type lockingFakeDB struct {
	*fakeDB
	acquires int
	releases int
	key      int64
}

func (db *lockingFakeDB) LockSession(_ context.Context, key int64) (func(context.Context) error, error) {
	db.acquires++
	db.key = key
	return func(context.Context) error { db.releases++; return nil }, nil
}

type failingLockDB struct{ *fakeDB }

func (db *failingLockDB) LockSession(context.Context, int64) (func(context.Context) error, error) {
	return nil, errors.New("lock unavailable")
}

// TestApplyTakesTheSetupLockBeforeTheLedger proves the administrator connection
// takes the advisory session lock exactly once, before any transaction, and
// releases it when the run ends. Each concurrent migrate process then observes
// a complete ledger instead of racing the ledger creation.
func TestApplyTakesTheSetupLockBeforeTheLedger(t *testing.T) {
	db := &lockingFakeDB{fakeDB: &fakeDB{ledger: map[string]string{}}}
	if _, err := migrate.Apply(context.Background(), db, migrate.Config{
		Database: "kodama", Admin: "admin", Passwords: map[string]string{"kodama_runtime": "pw"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if db.acquires != 1 || db.releases != 1 {
		t.Fatalf("lock acquisitions/releases = %d/%d; want 1/1", db.acquires, db.releases)
	}
	if db.key == 0 {
		t.Fatal("the setup lock key is zero")
	}
	if db.begins == 0 {
		t.Fatal("the run did no database work after taking the lock")
	}
}

// TestApplyLockFailureStopsBeforeAnyMigration proves a lock that cannot be held
// (another setup run owns it forever) fails the run before any migration
// statement, not halfway through the set.
func TestApplyLockFailureStopsBeforeAnyMigration(t *testing.T) {
	db := &failingLockDB{fakeDB: &fakeDB{ledger: map[string]string{}}}
	if _, err := migrate.Apply(context.Background(), db, migrate.Config{
		Database: "kodama", Admin: "admin", Passwords: map[string]string{"kodama_runtime": "pw"},
	}); err == nil {
		t.Fatal("Apply ignored a failed setup lock")
	}
	if db.begins != 0 {
		t.Fatalf("the failed lock still opened %d transactions", db.begins)
	}
}

// TestQuotePasswordIsInjectionSafeUnderBothStringSettings proves the emitted
// literal doubles backslashes and quotes inside an E” string, so it denotes
// exactly the configured bytes whether standard_conforming_strings is on or off.
// TestQuoteIdentDoublesEmbeddedQuotes proves an identifier containing a double
// quote is emitted as one quoted identifier, never as two tokens: the doubled
// form is what ALTER DATABASE/GRANT use for the operator-configured names.
func TestQuoteIdentDoublesEmbeddedQuotes(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"kodama", `"kodama"`},
		{`a"b`, `"a""b"`},
		{`"`, `""""`},
		{`a"; DROP DATABASE x; --`, `"a""; DROP DATABASE x; --"`},
	} {
		if got := migrate.QuoteIdentForTest(tc.name); got != tc.want {
			t.Fatalf("quoteIdent(%q) = %s; want %s", tc.name, got, tc.want)
		}
	}
}

func TestQuotePasswordIsInjectionSafeUnderBothStringSettings(t *testing.T) {
	for _, tc := range []struct{ password, want string }{
		{"plain", `E'plain'`},
		{"p'w", `E'p''w'`},
		{`\'; CREATE ROLE kodama_evil; --`, `E'\\''; CREATE ROLE kodama_evil; --'`},
		{`back\slash`, `E'back\\slash'`},
	} {
		if got := migrate.QuotePasswordForTest(tc.password); got != tc.want {
			t.Fatalf("quotePassword(%q) = %s; want %s", tc.password, got, tc.want)
		}
	}
}
