package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed migrations_releases/*.sql
var releaseMigrationsFS embed.FS

// WorkspaceProvenanceMigrationID is the one later change this package contributes
// to the squashed baseline (internal/storage/migrations/001_schema.sql).
const WorkspaceProvenanceMigrationID = "005_artifact_workspace_provenance.sql"

// ProvenanceMigrations returns the additive table of workspace provenance as its
// own contribution, after the run-resource grants' (004): the ledger refuses a
// pending migration ordered before an applied one, so a composition lists it last
// (internal/app.Contributions). A store over a database without it keeps serving
// attempt_private artifacts exactly as before and refuses to record workspace
// provenance it cannot keep.
func ProvenanceMigrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "artifacts-workspace-provenance", Migrations: ms}
}

// ReleaseMigrationsMigrationID is the additive migration for ADR-0012 release
// manifests and publication receipts.
const ReleaseMigrationsMigrationID = "006_artifact_releases_and_publications.sql"

// ReleaseMigrations returns the artifact release and publication tables as an
// additive contribution after the workspace provenance migration (005).
func ReleaseMigrations() storage.Contribution {
	ms, err := storage.LoadFS(releaseMigrationsFS, "migrations_releases")
	if err != nil {
		panic(err)
	}
	return storage.Contribution{Module: "artifacts-releases", Migrations: ms}
}

//go:embed migrations_source/*.sql
var sourceMigrationsFS embed.FS

// SourceProvenanceMigrationID is the migration of source-set provenance (T-149).
// Reserved IDs of the internal Git service are listed in
// docs/implementation/internal-git-service.md.
const SourceProvenanceMigrationID = "013_source_provenance.sql"

// SourceProvenanceMigrations returns the source-set provenance columns of
// artifacts, deployments and gates as one contribution (slot 013). It references
// the repositories of the repository contribution (007), so a composition lists
// it in numeric order after 007 and the run sources (011) and before the attempt
// Git state (014); the ledger refuses a pending migration ordered before an
// applied one. This package does not wire it into any composition: T-132 owns
// the ordering beside the one-line join in internal/app.
func SourceProvenanceMigrations() storage.Contribution {
	ms, err := storage.LoadFS(sourceMigrationsFS, "migrations_source")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "source-provenance", Migrations: ms}
}
