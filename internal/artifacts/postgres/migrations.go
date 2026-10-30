package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

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
