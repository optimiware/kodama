package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	// MigrationID is the migration of the push records (T-130). The numbers 001 and
	// 002 are the squashed baseline; 007 is the repositories table, 008 the Git
	// grants (docs/implementation/internal-git-service.md, "Migration numbering").
	MigrationID = "009_git_pushes.sql"
	// OperationsMigrationID is the migration of the operator actions (T-138): the
	// resolution of unknown entries and the acknowledgement of a restored
	// journal. It is the reserved slot 015 (Git operations); this module owns it
	// because it relaxes this module's own triggers, and internal/gitops owns no
	// tables.
	OperationsMigrationID = "015_git_operations.sql"
)

func load(module, id string) storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	for _, m := range ms {
		if m.ID == id {
			return storage.Contribution{Module: module, Migrations: []storage.Migration{m}}
		}
	}
	panic("gitpush/postgres: embedded migration " + id + " is missing")
}

// Migrations returns the push-record tables as their own contribution. It
// references the repositories table, so a composition lists it after 005. The
// ledger refuses a pending migration ordered before an applied one. This package
// does not wire it into any composition: T-132 registers it in
// internal/app/migrate.go.
func Migrations() storage.Contribution { return load("gitpush", MigrationID) }

// OperationsMigrations returns migration 015 as its own contribution, to be
// registered by T-132 at slot 015 (after 014), always after Migrations().
func OperationsMigrations() storage.Contribution {
	return load("gitpush-operations", OperationsMigrationID)
}
