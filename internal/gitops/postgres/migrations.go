package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationID is the migration of the Git recovery state (T-138): the reserved
// slot 016 of docs/implementation/internal-git-service.md ("Migration
// numbering"; 015 is gitpush's operator actions).
const MigrationID = "016_git_recovery.sql"

// Migrations returns migration 016 as its own contribution. It references the
// tenants table (the baseline) and the repositories table (007), so a
// composition lists it after 013. The ledger refuses a pending migration ordered
// before an applied one. This package does not wire it into any composition:
// T-132 registers it in internal/app/migrate.go.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	for _, m := range ms {
		if m.ID == MigrationID {
			return storage.Contribution{Module: "gitops", Migrations: []storage.Migration{m}}
		}
	}
	panic("gitops/postgres: embedded migration " + MigrationID + " is missing")
}
