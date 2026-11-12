package pgdriver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
)

// fakeTag is a statement result.
type fakeTag struct{ affected int64 }

func (tag fakeTag) RowsAffected() int64 { return tag.affected }

// fakeRows is a deterministic row iterator.
type fakeRows struct {
	rows   [][]any
	index  int
	err    error
	closed bool
	mu     sync.Mutex
}

func (rows *fakeRows) Next() bool {
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if rows.index >= len(rows.rows) {
		return false
	}
	rows.index++
	return true
}

func (rows *fakeRows) Scan(dest ...any) error {
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if rows.index == 0 || rows.index > len(rows.rows) {
		return errors.New("scan without a row")
	}
	row := rows.rows[rows.index-1]
	if len(dest) != len(row) {
		return errors.New("scan arity mismatch")
	}
	for position := range dest {
		target, ok := dest[position].(*any)
		if !ok {
			return errors.New("unsupported scan target")
		}
		*target = row[position]
	}
	return nil
}

func (rows *fakeRows) Err() error { return rows.err }

func (rows *fakeRows) Close() error {
	rows.mu.Lock()
	defer rows.mu.Unlock()
	rows.closed = true
	return nil
}

// fakeTransaction records calls and injects failures.
type fakeTransaction struct {
	mu            sync.Mutex
	execResult    fakeTag
	execErr       error
	queryRows     rows
	queryErr      error
	commitErr     error
	rollbackErr   error
	commitCalls   int
	rollbackCalls int
	execCalls     int
	lastSQL       string
	lastArgs      []any
	lastCtx       context.Context
}

func (tx *fakeTransaction) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.execCalls++
	tx.lastSQL = sql
	tx.lastArgs = args
	tx.lastCtx = ctx
	if tx.execErr != nil {
		return nil, tx.execErr
	}
	return tx.execResult, nil
}

func (tx *fakeTransaction) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.lastSQL = sql
	tx.lastArgs = args
	tx.lastCtx = ctx
	if tx.queryErr != nil {
		return nil, tx.queryErr
	}
	return tx.queryRows, nil
}

func (tx *fakeTransaction) Commit(context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.commitCalls++
	return tx.commitErr
}

func (tx *fakeTransaction) Rollback(context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.rollbackCalls++
	return tx.rollbackErr
}

func (tx *fakeTransaction) calls() (commit, rollback int) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.commitCalls, tx.rollbackCalls
}

// fakeDriver opens fake transactions.
type fakeDriver struct {
	mu      sync.Mutex
	tx      *fakeTransaction
	err     error
	begins  int
	lastCtx context.Context
}

func (driver *fakeDriver) Begin(ctx context.Context) (transaction, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.begins++
	driver.lastCtx = ctx
	if driver.err != nil {
		return nil, driver.err
	}
	return driver.tx, nil
}

// newWithDriver builds a wrapper over a supplied driver. It is the fake-boundary
// constructor used by the unit suite.
func newWithDriver(driver beginner) (*DB, error) {
	if driver == nil {
		return nil, fault.New(fault.InvalidInput, "database driver is nil")
	}
	return &DB{driver: driver}, nil
}

func newFakeDB(t *testing.T) (*DB, *fakeTransaction, *fakeDriver) {
	t.Helper()
	tx := &fakeTransaction{}
	driver := &fakeDriver{tx: tx}
	db, err := newWithDriver(driver)
	if err != nil {
		t.Fatalf("newWithDriver: %v", err)
	}
	return db, tx, driver
}

func TestBeginExecQueryAndCommit(t *testing.T) {
	db, tx, _ := newFakeDB(t)
	tx.execResult = fakeTag{affected: 3}
	tx.queryRows = &fakeRows{rows: [][]any{{"value", int64(7)}}}

	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result, err := conn.Exec(context.Background(), "UPDATE t SET a = $1", "a")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.RowsAffected != 3 {
		t.Fatalf("RowsAffected = %d; want 3", result.RowsAffected)
	}
	if tx.lastSQL != "UPDATE t SET a = $1" || len(tx.lastArgs) != 1 || tx.lastArgs[0] != "a" {
		t.Fatalf("statement did not travel as parameters: %q %#v", tx.lastSQL, tx.lastArgs)
	}

	rows, err := conn.Query(context.Background(), "SELECT a, b")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !rows.Next() {
		t.Fatal("Next returned false")
	}
	var name any
	var number any
	if err := rows.Scan(&name, &number); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if name != "value" || number != int64(7) {
		t.Fatalf("scan = %#v/%#v", name, number)
	}
	if rows.Next() {
		t.Fatal("Next returned a second row")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := conn.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	commit, rollback := tx.calls()
	if commit != 1 || rollback != 0 {
		t.Fatalf("commit/rollback = %d/%d; want 1/0", commit, rollback)
	}
	if _, err := conn.Exec(context.Background(), "UPDATE t SET a = $1", "b"); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("Exec after commit = %v; want ErrNoTransaction", err)
	}
	if _, err := conn.Query(context.Background(), "SELECT 1"); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("Query after commit = %v; want ErrNoTransaction", err)
	}
	if err := conn.Commit(context.Background()); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("second Commit = %v; want ErrNoTransaction", err)
	}
}

func TestCancellationClosesTransaction(t *testing.T) {
	db, tx, _ := newFakeDB(t)
	tx.execErr = context.Canceled
	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, err = conn.Exec(context.Background(), "UPDATE t SET a = $1", "a")
	if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
		t.Fatalf("fault class = %q; want deadline_expired", class)
	}
	commit, rollback := tx.calls()
	if commit != 0 || rollback != 1 {
		t.Fatalf("commit/rollback = %d/%d; want 0/1", commit, rollback)
	}
	if _, err := conn.Exec(context.Background(), "UPDATE t SET a = $1", "b"); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("Exec after cancellation = %v; want ErrNoTransaction", err)
	}
}

func TestCanceledContextPerformsNoStatement(t *testing.T) {
	db, tx, _ := newFakeDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := db.Begin(ctx)
	if err == nil {
		t.Fatal("Begin with a canceled context succeeded")
	}
	_ = conn
	if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
		t.Fatalf("fault class = %q; want deadline_expired", class)
	}
	if tx.execCalls != 0 {
		t.Fatalf("canceled Begin issued %d statements", tx.execCalls)
	}
}

func TestCommitFailureRollsBack(t *testing.T) {
	db, tx, _ := newFakeDB(t)
	tx.commitErr = errors.New("injected commit failure")
	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = conn.Commit(context.Background())
	if class, _ := fault.ClassOf(err); class != fault.DependencyUnavailable {
		t.Fatalf("fault class = %q; want dependency_unavailable", class)
	}
	commit, rollback := tx.calls()
	if commit != 1 || rollback != 1 {
		t.Fatalf("commit/rollback = %d/%d; want 1/1", commit, rollback)
	}
	if _, err := conn.Exec(context.Background(), "UPDATE t", nil); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("Exec after failed commit = %v; want ErrNoTransaction", err)
	}
}

func TestRollbackClosesConnection(t *testing.T) {
	db, tx, _ := newFakeDB(t)
	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := conn.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	commit, rollback := tx.calls()
	if commit != 0 || rollback != 1 {
		t.Fatalf("commit/rollback = %d/%d; want 0/1", commit, rollback)
	}
	if err := conn.Rollback(context.Background()); !errors.Is(err, postgres.ErrNoTransaction) {
		t.Fatalf("second Rollback = %v; want ErrNoTransaction", err)
	}
}

func TestSQLStateMapping(t *testing.T) {
	cases := []struct {
		code  string
		class fault.Class
	}{
		{"23505", fault.VersionConflict},
		{"40001", fault.VersionConflict},
		{"40P01", fault.VersionConflict},
		{"42501", fault.Forbidden},
		{"57014", fault.DeadlineExpired},
		{"23514", fault.InvalidInput},
		{"42P01", fault.InternalFailure},
		{"08006", fault.DependencyUnavailable},
	}
	for _, test := range cases {
		err := mapError(&pgconn.PgError{Code: test.code})
		class, ok := fault.ClassOf(err)
		if !ok || class != test.class {
			t.Fatalf("SQLSTATE %s mapped to %q; want %q", test.code, class, test.class)
		}
	}
}

func TestSecretsNeverAppearInErrors(t *testing.T) {
	if _, err := New(Config{ConnString: "postgres://user:supersecret@bad host/db"}); err == nil {
		t.Fatal("New accepted a malformed connection string")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("constructor error leaked the password: %v", err)
	}

	db, _, driver := newFakeDB(t)
	driver.err = errors.New("dial failed for postgres://user:supersecret@db/app")
	if _, err := db.Begin(context.Background()); err == nil {
		t.Fatal("Begin succeeded with an injected failure")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("Begin error leaked the password: %v", err)
	}

	tx := &fakeTransaction{execErr: errors.New("statement failed with token=supersecret")}
	driver = &fakeDriver{tx: tx}
	db, err := newWithDriver(driver)
	if err != nil {
		t.Fatalf("newWithDriver: %v", err)
	}
	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := conn.Exec(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("Exec succeeded with an injected failure")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("Exec error leaked the token: %v", err)
	}
}

func TestConstructorRefusesInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted an empty connection string")
	}
	if _, err := New(Config{ConnString: "postgres://host/db", MaxConns: -1}); err == nil {
		t.Fatal("New accepted a negative pool bound")
	}
	if _, err := New(Config{ConnString: "postgres://host/db", MaxConns: 1, MinConns: 2}); err == nil {
		t.Fatal("New accepted a minimum above the maximum")
	}
	if _, err := New(Config{ConnString: "postgres://host/db", ConnectTimeout: -1}); err == nil {
		t.Fatal("New accepted a negative connect timeout")
	}
	if _, err := newWithDriver(nil); err == nil {
		t.Fatal("newWithDriver accepted a nil driver")
	}
	var nilDB *DB
	if _, err := nilDB.Begin(context.Background()); err == nil {
		t.Fatal("nil DB Begin succeeded")
	}
}

func TestDBClosesLazilyOpenedDriver(t *testing.T) {
	tx := &fakeTransaction{}
	closes := 0
	db := newWithOpener(func(context.Context) (beginner, func(), error) {
		return &fakeDriver{tx: tx}, func() { closes++ }, nil
	})
	conn, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := conn.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	db.Close()
	db.Close()
	if closes != 1 {
		t.Fatalf("pool closes = %d; want 1", closes)
	}
	if _, err := db.Begin(context.Background()); err == nil {
		t.Fatal("Begin after Close succeeded")
	}
}

func TestDriverErrorIsClassified(t *testing.T) {
	driver := &fakeDriver{err: errors.New("connection reset")}
	db, err := newWithDriver(driver)
	if err != nil {
		t.Fatalf("newWithDriver: %v", err)
	}
	_, err = db.Begin(context.Background())
	if class, _ := fault.ClassOf(err); class != fault.DependencyUnavailable {
		t.Fatalf("fault class = %q; want dependency_unavailable", class)
	}
}

// TestEnsureDatabaseValidatesTheNameBeforeConnecting proves a database name with
// a double quote, control byte or no content is refused as invalid input before
// any connection is attempted (the connection string is unreachable, so a dial
// would be reported instead).
func TestEnsureDatabaseValidatesTheNameBeforeConnecting(t *testing.T) {
	for _, name := range []string{"", "   ", `bad"name`, "bad\x00name"} {
		t.Run(name, func(t *testing.T) {
			err := EnsureDatabase(context.Background(),
				"postgres://admin:SENTINEL-PW@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1", name)
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("EnsureDatabase(%q) class = %v (found=%v); want invalid input", name, class, ok)
			}
		})
	}
}

// TestEnsureDatabaseConnectionErrorIsSecretFree proves an unreachable
// maintenance database is a classified, fixed diagnostic: neither the
// connection URL nor its password reaches the error.
func TestEnsureDatabaseConnectionErrorIsSecretFree(t *testing.T) {
	const password = "SENTINEL-MAINTENANCE-PASS"
	connString := "postgres://kodama_admin:" + password + "@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1"
	err := EnsureDatabase(context.Background(), connString, "kodama")
	if err == nil {
		t.Fatal("an unreachable maintenance database was accepted")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("error class = %v (found=%v); want dependency unavailable", class, ok)
	}
	if !strings.Contains(err.Error(), "maintenance database is unreachable") {
		t.Fatalf("error = %v; want the fixed diagnostic", err)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error rendered the connection string: %v", err)
	}
}

// TestParsePoolConfigHonoursTheDSNTLSQueryOptions proves the wrapper hands the
// configured DSN's TLS options to pgx unchanged. The `kodama migrate` and
// `kodama bootstrap-admin` one-shot commands connect through this constructor
// with exactly the DSN their Secret supplies, so an ignored, trimmed or
// downgraded sslmode/sslrootcert would silently weaken their connection.
func TestParsePoolConfigHonoursTheDSNTLSQueryOptions(t *testing.T) {
	const password = "SENTINEL-PG-PASSWORD"
	caPEM := selfSignedCAPEM(t)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write the test CA: %v", err)
	}
	expectedPool := x509.NewCertPool()
	if !expectedPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the test CA is not usable")
	}

	t.Run("verify-full loads the configured root and verifies the host", func(t *testing.T) {
		parsed, err := parsePoolConfig("postgres://kodama_admin:" + password + "@database:5432/kodama?sslmode=verify-full&sslrootcert=" + caPath)
		if err != nil {
			t.Fatalf("parsePoolConfig: %v", err)
		}
		tlsConfig := parsed.ConnConfig.TLSConfig
		if tlsConfig == nil {
			t.Fatal("sslmode=verify-full parsed without a TLS configuration")
		}
		if tlsConfig.InsecureSkipVerify {
			t.Fatal("sslmode=verify-full parsed without certificate verification")
		}
		if tlsConfig.RootCAs == nil || !tlsConfig.RootCAs.Equal(expectedPool) {
			t.Fatal("the configured sslrootcert was not loaded as the TLS root pool")
		}
	})

	t.Run("verify-ca keeps chain verification", func(t *testing.T) {
		parsed, err := parsePoolConfig("postgres://kodama_admin:" + password + "@database:5432/kodama?sslmode=verify-ca&sslrootcert=" + caPath)
		if err != nil {
			t.Fatalf("parsePoolConfig: %v", err)
		}
		tlsConfig := parsed.ConnConfig.TLSConfig
		if tlsConfig == nil || tlsConfig.RootCAs == nil {
			t.Fatal("sslmode=verify-ca parsed without the configured root")
		}
		if tlsConfig.VerifyPeerCertificate == nil {
			t.Fatal("sslmode=verify-ca parsed without chain verification")
		}
	})

	t.Run("disable is passed through, not upgraded", func(t *testing.T) {
		parsed, err := parsePoolConfig("postgres://kodama_admin:" + password + "@database:5432/kodama?sslmode=disable")
		if err != nil {
			t.Fatalf("parsePoolConfig: %v", err)
		}
		if parsed.ConnConfig.TLSConfig != nil {
			t.Fatal("sslmode=disable parsed as a TLS connection; the wrapper rewrote the DSN")
		}
	})

	t.Run("unreadable root is a secret-free invalid input", func(t *testing.T) {
		dsn := "postgres://kodama_admin:" + password + "@database:5432/kodama?sslmode=verify-full&sslrootcert=/nonexistent/ca.crt"
		_, err := parsePoolConfig(dsn)
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("class = %v (found=%v); want invalid_input (%v)", class, ok, err)
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), dsn) {
			t.Fatalf("refusal rendered the connection string: %v", err)
		}
		if _, err := New(Config{ConnString: dsn}); err == nil {
			t.Fatal("New accepted a DSN whose sslrootcert cannot be read")
		}
	})
}

// TestParsePoolConfigNeverRendersTheParserCause proves a malformed DSN whose
// parser diagnostic quotes the connection string (and, for shapes the parser
// cannot redact, its password) is refused with only the fixed message. The
// password sentinel and the parser's own text both stay private.
func TestParsePoolConfigNeverRendersTheParserCause(t *testing.T) {
	const sentinel = "SENTINEL-PARSE-PASSWORD"
	for _, dsn := range []string{
		"postgres://kodama_admin:" + sentinel + "@database:notaport/kodama",
		"postgres://kodama_admin:" + sentinel + "@database:5432/kodama?sslmode=bogus-mode",
	} {
		t.Run(dsn, func(t *testing.T) {
			_, err := parsePoolConfig(dsn)
			if err == nil {
				t.Fatalf("parsePoolConfig accepted %q", dsn)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v (found=%v); want invalid_input (%v)", class, ok, err)
			}
			if !strings.Contains(err.Error(), "database connection string is invalid") {
				t.Fatalf("error = %v; want the fixed message", err)
			}
			for _, leaked := range []string{sentinel, dsn, "cannot parse", "invalid port", "bogus-mode"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("refusal rendered %q: %v", leaked, err)
				}
			}
			if _, err := New(Config{ConnString: dsn}); err == nil {
				t.Fatalf("New accepted %q", dsn)
			}
		})
	}
}

// selfSignedCAPEM returns one self-signed CA certificate as PEM so the pgx
// parser can load it as an sslrootcert.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the test CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-pg-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the test CA: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
