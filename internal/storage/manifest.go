package storage

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Migration errors. They are wrapped with the offending migration ID.
var (
	ErrInvalidMigrationID  = errors.New("invalid migration ID")
	ErrDuplicateMigration  = errors.New("duplicate migration ID")
	ErrMigrationOrder      = errors.New("migration order conflict")
	ErrChecksumMismatch    = errors.New("applied migration was modified")
	ErrUnknownMigration    = errors.New("applied migration is not in the manifest")
	ErrRuntimeRoleRequired = errors.New("migration uses {{runtime_role}} but no runtime role was configured")
)

// migrationIDPattern requires a task prefix and sequence, such as
// T011_001_work.sql, so IDs are globally unique by construction.
var migrationIDPattern = regexp.MustCompile(`^T[0-9]{3}_[0-9]{3}_[a-z0-9][a-z0-9_]*\.sql$`)

// RuntimeRolePlaceholder in migration SQL is replaced by the quoted runtime
// role name given in MigrateOptions, so grants follow the deployment's role.
const RuntimeRolePlaceholder = "{{runtime_role}}"

// Migration is one immutable SQL unit. Once merged its ID and SQL never
// change; schema fixes are new migrations.
type Migration struct {
	ID  string
	SQL string
}

// Checksum is the hex SHA-256 of the migration SQL exactly as embedded
// (before placeholder expansion).
func (m Migration) Checksum() string {
	sum := sha256.Sum256([]byte(m.SQL))
	return hex.EncodeToString(sum[:])
}

// Contribution is the ordered migration list of one module.
type Contribution struct {
	Module     string
	Migrations []Migration
}

// LoadFS reads every *.sql file directly under dir as a migration, ordered by
// ID. Files with other extensions are ignored; invalid IDs are rejected.
func LoadFS(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("storage: read migrations %q: %w", dir, err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if !migrationIDPattern.MatchString(e.Name()) {
			return nil, fmt.Errorf("storage: %w: %q must match %s", ErrInvalidMigrationID, e.Name(), migrationIDPattern)
		}
		data, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("storage: read migration %q: %w", e.Name(), err)
		}
		out = append(out, Migration{ID: e.Name(), SQL: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Manifest is the validated, explicitly ordered list of all module
// contributions. Order across contributions is the order given to
// NewManifest, not lexical task order: the composition root lists base
// identity/tenant migrations first and each module after what it references.
type Manifest struct {
	migrations []Migration
}

// NewManifest validates and flattens contributions. It rejects invalid IDs,
// an ID appearing twice anywhere (across or within contributions), and a
// contribution whose own migrations are not in ascending ID order. Nothing
// touches a database.
func NewManifest(contributions ...Contribution) (Manifest, error) {
	seen := make(map[string]string)
	var all []Migration
	for _, c := range contributions {
		prev := ""
		for _, m := range c.Migrations {
			if !migrationIDPattern.MatchString(m.ID) {
				return Manifest{}, fmt.Errorf("storage: %w: %q (module %q) must match %s", ErrInvalidMigrationID, m.ID, c.Module, migrationIDPattern)
			}
			if first, dup := seen[m.ID]; dup {
				return Manifest{}, fmt.Errorf("storage: %w: %q in module %q already contributed by module %q", ErrDuplicateMigration, m.ID, c.Module, first)
			}
			if prev != "" && m.ID < prev {
				return Manifest{}, fmt.Errorf("storage: %w: %q follows %q in module %q", ErrMigrationOrder, m.ID, prev, c.Module)
			}
			if strings.TrimSpace(m.SQL) == "" {
				return Manifest{}, fmt.Errorf("storage: %w: %q has no SQL", ErrInvalidMigrationID, m.ID)
			}
			seen[m.ID] = c.Module
			prev = m.ID
			all = append(all, m)
		}
	}
	return Manifest{migrations: all}, nil
}

// Migrations returns a copy of the ordered migrations.
func (m Manifest) Migrations() []Migration {
	return append([]Migration(nil), m.migrations...)
}

//go:embed migrations/*.sql
var baseFS embed.FS

// BaseContribution returns T-002's tenancy, audit and discovery migrations.
// They must precede every contribution holding tenant-owned foreign keys.
func BaseContribution() Contribution {
	ms, err := LoadFS(baseFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by tests
	}
	return Contribution{Module: "storage", Migrations: ms}
}
