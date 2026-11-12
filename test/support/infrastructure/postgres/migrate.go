package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
)

// ErrAppliedMigrationRewritten reports a ledger entry whose recorded checksum no
// longer matches the reviewed migration body. An applied migration is never
// rewritten, so this is refused instead of silently re-applied.
var ErrAppliedMigrationRewritten = errors.New("pgtest: an applied migration was rewritten")

// MigrationNamePattern is the migration naming rule.
var MigrationNamePattern = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)

// ledgerSQL creates the migration ledger. It is the test-side runner's durable
// record that an applied migration is never applied twice.
const ledgerSchema = "kodama_test_meta"

const ledgerSQL = `CREATE SCHEMA IF NOT EXISTS ` + ledgerSchema + `;
CREATE TABLE IF NOT EXISTS ` + ledgerSchema + `.kodama_schema_migrations (
	name text PRIMARY KEY,
	checksum text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
)`

// AppliedMigration is one ledger row.
type AppliedMigration struct {
	Name     string
	Checksum string
}

// MigrationRunner applies the reviewed migration set exactly once.
type MigrationRunner struct {
	shard *Shard
}

// NewMigrationRunner builds a runner over one shard.
func NewMigrationRunner(shard *Shard) *MigrationRunner {
	return &MigrationRunner{shard: shard}
}

// EnsureLedger creates the ledger table if absent.
func (runner *MigrationRunner) EnsureLedger(ctx context.Context) error {
	_, err := ExecSQL(ctx, runner.shard.Admin(), ledgerSQL)
	return err
}

// Apply applies every migration with an ordinal at or below through. through=0
// means all. It returns only the migrations it newly applied. Re-running an
// already-applied identical migration is a no-op; a changed body is refused.
func (runner *MigrationRunner) Apply(ctx context.Context, through int) ([]AppliedMigration, error) {
	if err := runner.EnsureLedger(ctx); err != nil {
		return nil, err
	}
	migrations := storepg.Migrations()
	if len(migrations) == 0 {
		return nil, errors.New("pgtest: the frozen migration set is empty")
	}
	var applied []AppliedMigration
	for index, migration := range migrations {
		ordinal := index + 1
		if through > 0 && ordinal > through {
			break
		}
		if !MigrationNamePattern.MatchString(migration.Name) {
			return nil, fmt.Errorf("pgtest: migration %q does not match %s", migration.Name, MigrationNamePattern)
		}
		checksum := migrationChecksum(migration.SQL)
		recorded, found, err := runner.lookup(ctx, migration.Name)
		if err != nil {
			return nil, err
		}
		if found {
			if recorded != checksum {
				return nil, fmt.Errorf("%w: %s recorded %s, reviewed %s", ErrAppliedMigrationRewritten, migration.Name, recorded, checksum)
			}
			continue
		}
		if err := runner.applyOne(ctx, migration, checksum); err != nil {
			return nil, fmt.Errorf("apply %s: %w%s", migration.Name, err, causeChain(err))
		}
		applied = append(applied, AppliedMigration{Name: migration.Name, Checksum: checksum})
	}
	return applied, nil
}

func (runner *MigrationRunner) lookup(ctx context.Context, name string) (string, bool, error) {
	rows, err := QueryStringsN(ctx, runner.shard.Admin(), 1,
		`SELECT checksum FROM `+ledgerSchema+`.kodama_schema_migrations WHERE name = $1`, name)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0][0], true, nil
}

// bootstrapMigrations is the migration that creates roles. PostgreSQL requires
// CREATEROLE (or superuser) for CREATE ROLE, and the migration owner is
// deliberately NOCREATEROLE, so it runs as the administrator exactly like a
// database administrator's bootstrap step. Every other migration runs as the
// owner role through SET LOCAL ROLE.
var bootstrapMigrations = map[string]bool{
	"0001_roles.sql": true,
}

func (runner *MigrationRunner) applyOne(ctx context.Context, migration storepg.Migration, checksum string) error {
	conn, err := runner.shard.Owner().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if !bootstrapMigrations[migration.Name] {
		if _, err := conn.Exec(ctx, `SET LOCAL ROLE `+store.DefaultOwnerRole); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, migrationBody(migration.SQL)); err != nil {
		return err
	}
	// The migration owner is the database owner, so it inherits the public
	// schema's CREATE privilege through pg_database_owner. PostgreSQL creates a
	// database owned by the connecting administrator; the 0001 migration
	// creates the owner role but cannot hand the database to it, so the
	// administrator does that once here. Without this the NOLOGIN owner can
	// never create the sequence, tables or indexes the later migrations define.
	if migration.Name == "0001_roles.sql" {
		if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `ALTER DATABASE `+quoteIdent(runner.shard.Database)+` OWNER TO `+store.DefaultOwnerRole); err != nil {
			return err
		}
	}
	// The ledger belongs to the administrator; reset the transaction role
	// before recording so an owner without a ledger grant cannot fail here.
	if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO `+ledgerSchema+`.kodama_schema_migrations (name, checksum) VALUES ($1, $2)`,
		migration.Name, checksum); err != nil {
		return err
	}
	return conn.Commit(ctx)
}

// migrationBody strips the standalone transaction-control statements a migration
// may carry so the runner's own transaction owns atomicity: executing BEGIN or
// COMMIT inside the runner's transaction would commit it early.
func migrationBody(sql string) string {
	lines := strings.Split(sql, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "BEGIN;") || strings.EqualFold(trimmed, "BEGIN") ||
			strings.EqualFold(trimmed, "COMMIT;") || strings.EqualFold(trimmed, "COMMIT") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func causeChain(err error) string {
	var parts []string
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
		parts = append(parts, cause.Error())
	}
	if len(parts) == 0 {
		return ""
	}
	return " (cause: " + strings.Join(parts, " <- ") + ")"
}

func migrationChecksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Applied lists the ledger in application order.
func (runner *MigrationRunner) Applied(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := QueryStringsN(ctx, runner.shard.Admin(), 2,
		`SELECT name, checksum FROM `+ledgerSchema+`.kodama_schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	applied := make([]AppliedMigration, len(rows))
	for index, row := range rows {
		applied[index] = AppliedMigration{Name: row[0], Checksum: row[1]}
	}
	return applied, nil
}
