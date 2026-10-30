package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ResourceGrantsMigrationID is the migration of the run-resource runtime grants
// (T-113). The attempt grants are in the squashed baseline
// (internal/storage/migrations/001_schema.sql); this is the one later change the
// package contributes.
const ResourceGrantsMigrationID = "004_runtime_resource_grants.sql"

// ResourceMigrations returns the run-resource grant table as its own
// contribution, after the run-resource lifecycle's (003): the ledger refuses a
// pending migration ordered before an applied one, so a composition lists it
// last. This package does not wire it into any composition (T-116 owns the
// ordering beside the one-line join in internal/app).
func ResourceMigrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "runtimeauth-resource-grants", Migrations: ms}
}
