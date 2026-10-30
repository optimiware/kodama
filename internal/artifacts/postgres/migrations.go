package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var releaseMigrationsFS embed.FS

// ReleaseMigrationsMigrationID is the additive migration for ADR-0012 release
// manifests and publication receipts.
const ReleaseMigrationsMigrationID = "005_artifact_releases_and_publications.sql"

// ReleaseMigrations returns the artifact release and publication tables as an
// additive contribution after the baseline and run-resource migrations.
func ReleaseMigrations() storage.Contribution {
	ms, err := storage.LoadFS(releaseMigrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return storage.Contribution{Module: "artifacts-releases", Migrations: ms}
}
