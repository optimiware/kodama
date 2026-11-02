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

func only(id string) []storage.Migration {
	var out []storage.Migration
	for _, m := range loadOwn() {
		if m.ID == id {
			out = append(out, m)
		}
	}
	return out
}

// ResourceGrantsMigrationID is the migration of the run-resource runtime grants
// (T-113). The attempt grants are in the squashed baseline
// (internal/storage/migrations/001_schema.sql); this is the one earlier change the
// package contributes.
const ResourceGrantsMigrationID = "004_runtime_resource_grants.sql"

// ResourceMigrations returns the run-resource grant table as its own
// contribution, after the run-resource lifecycle's (003): the ledger refuses a
// pending migration ordered before an applied one, so a composition lists it
// last. This package does not wire it into any composition (T-116 owns the
// ordering beside the one-line join in internal/app).
func ResourceMigrations() storage.Contribution {
	return storage.Contribution{Module: "runtimeauth-resource-grants", Migrations: only(ResourceGrantsMigrationID)}
}

// AttemptGitMigrationID is the migration of the runtime-issued Git grant records
// (T-150): the reserved slot 014 of the internal Git service
// (docs/implementation/internal-git-service.md, "Migration numbering").
const AttemptGitMigrationID = "014_attempt_git.sql"

// AttemptGitMigrations returns the table of runtime-issued Git grants as its own
// contribution. It references only the projects table of the baseline, so it
// applies after any contribution that is already deployed, but the ledger refuses
// a pending migration ordered before an applied one: a composition lists it in
// numeric order, after the run sources (011) and the source provenance (013) and
// before the push-record operations (015). This package does not wire it into
// any composition (T-132 registers it in internal/app/migrate.go).
func AttemptGitMigrations() storage.Contribution {
	return storage.Contribution{Module: "runtimeauth-attempt-git", Migrations: only(AttemptGitMigrationID)}
}
