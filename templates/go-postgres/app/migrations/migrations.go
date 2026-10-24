// Package migrations holds the application's ordered, forward-only SQL
// migrations and the small runner that applies them.
//
// Rules:
//   - Files are named T<number>_<name>.sql and applied in lexical order.
//   - Each file runs in one transaction together with its ledger row.
//   - A recorded checksum must keep matching: an edited, applied file is refused.
//   - A migrator serializes on a PostgreSQL advisory lock, so concurrent
//     deployments apply each file exactly once.
//   - A database containing a version this binary does not know was migrated by
//     a newer release; the older binary refuses to proceed (see docs/operations.md).
//   - There are no automatic down-migrations. Recovery is a forward migration or
//     a restore, as documented in docs/operations.md.
package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed T*.sql
var files embed.FS

// Errors.
var (
	ErrChecksum      = errors.New("migrations: an applied migration changed")
	ErrDatabaseNewer = errors.New("migrations: the database has migrations this release does not know")
	ErrOutOfOrder    = errors.New("migrations: a new migration is ordered before an applied one")
	ErrRole          = errors.New("migrations: a valid runtime role is required")
)

// lockKey is the advisory lock serializing migrators of this application.
const lockKey int64 = 0x7430343300000001

var roleRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// Migration is one embedded file.
type Migration struct {
	Version  string
	SQL      string
	Checksum string
}

// Load returns the embedded migrations in order.
func Load() ([]Migration, error) { return load(files) }

func load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "T*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]Migration, 0, len(names))
	for _, n := range names {
		raw, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{Version: strings.TrimSuffix(n, ".sql"), SQL: string(raw), Checksum: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

// Options configures Apply.
type Options struct {
	// RuntimeRole receives the runtime grants. It is created by provisioning,
	// not by a migration.
	RuntimeRole string
	// Migrations overrides the embedded set (tests).
	Migrations []Migration
}

// Apply brings the database to the latest version and returns the versions it
// applied. conn must be the owner/migration identity.
func Apply(ctx context.Context, conn *pgx.Conn, opts Options) ([]string, error) {
	if !roleRE.MatchString(opts.RuntimeRole) {
		return nil, ErrRole
	}
	set := opts.Migrations
	if set == nil {
		var err error
		if set, err = Load(); err != nil {
			return nil, err
		}
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return nil, fmt.Errorf("migrations: lock: %w", err)
	}
	defer func() {
		// The lock is session-scoped; unlock with a context that survives cancellation.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockKey)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, checksum text NOT NULL, runtime_role text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrations: ledger: %w", err)
	}
	if _, err := conn.Exec(ctx, "GRANT SELECT ON schema_migrations TO "+pgx.Identifier{opts.RuntimeRole}.Sanitize()); err != nil {
		return nil, fmt.Errorf("migrations: ledger grant: %w", err)
	}
	applied, err := ledger(ctx, conn)
	if err != nil {
		return nil, err
	}
	known := map[string]Migration{}
	var newest string
	for _, m := range set {
		known[m.Version] = m
	}
	for v := range applied {
		if _, ok := known[v]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseNewer, v)
		}
		if v > newest {
			newest = v
		}
	}
	var done []string
	for _, m := range set {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("%w: %s", ErrChecksum, m.Version)
			}
			continue
		}
		if m.Version < newest {
			return done, fmt.Errorf("%w: %s", ErrOutOfOrder, m.Version)
		}
		if err := run(ctx, conn, m, opts.RuntimeRole); err != nil {
			return done, fmt.Errorf("migrations: %s: %w", m.Version, err)
		}
		done = append(done, m.Version)
		newest = m.Version
	}
	return done, nil
}

func ledger(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c
	}
	return out, rows.Err()
}

func run(ctx context.Context, conn *pgx.Conn, m Migration, role string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	body := strings.ReplaceAll(m.SQL, "{{runtime_role}}", pgx.Identifier{role}.Sanitize())
	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, checksum, runtime_role) VALUES ($1, $2, $3)", m.Version, m.Checksum, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Current returns the applied versions in order, for readiness and diagnostics.
func Current(ctx context.Context, conn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Latest is the newest version this binary knows.
func Latest() (string, error) {
	m, err := Load()
	if err != nil || len(m) == 0 {
		return "", err
	}
	return m[len(m)-1].Version, nil
}
