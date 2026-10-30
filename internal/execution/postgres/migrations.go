package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func loadOwn() []storage.Migration {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return ms
}

// RunResourcesMigrationID is the migration of the run_shared resource lifecycle
// and persistence (T-112). StepArtifactRefsMigrationID pins verified descendant
// artifact descriptors on the producing step.
const (
	RunResourcesMigrationID     = "003_run_resources.sql"
	StepArtifactRefsMigrationID = "004_step_artifact_refs.sql"
	ProjectRunsMigrationID      = "005_project_runs.sql"
)

// RunResourceMigrations returns the additive run_shared tables and columns as
// their own contribution, after the squashed baseline. A composition that has
// already deployed lists it after every contribution it has applied (the
// ledger refuses a pending migration ordered before an applied one); this
// package does not wire it into any composition (T-116 owns the ordering).
func RunResourceMigrations() storage.Contribution {
	var out []storage.Migration
	for _, m := range loadOwn() {
		if m.ID == RunResourcesMigrationID || m.ID == StepArtifactRefsMigrationID || m.ID == ProjectRunsMigrationID {
			out = append(out, m)
		}
	}
	return storage.Contribution{Module: "execution-run-resources", Migrations: out}
}
