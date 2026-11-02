package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// GrantsMigrationID is the migration of the Git grants table (T-136). Reserved
// IDs of the internal Git service are listed in
// docs/implementation/internal-git-service.md.
const GrantsMigrationID = "008_git_grants.sql"

// Migrations returns the Git grant table as this package's contribution. The
// ledger refuses a pending migration ordered before an applied one, so a
// composition lists it in numeric order after the repository migration (007)
// and before the push records (009). This package does not wire it into any
// composition: T-132 owns the ordering beside the one-line join in internal/app.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "gitauth", Migrations: ms}
}
