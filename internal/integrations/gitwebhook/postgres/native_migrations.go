package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var nativeMigrationsFS embed.FS

// NativeMigrationID is the reserved slot 012 (Git triggers) of
// docs/implementation/internal-git-service.md, "Migration numbering".
const NativeMigrationID = "012_git_triggers.sql"

// NativeMigrations returns the native trigger tables as their own
// contribution. They reference repositories (007), projects and pipeline
// definitions (baseline), so a composition lists it after 007 and 009. The
// ledger refuses a pending migration ordered before an applied one. This
// package does not wire it into any composition: T-132 registers it in
// internal/app/migrate.go.
func NativeMigrations() storage.Contribution {
	ms, err := storage.LoadFS(nativeMigrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	for _, m := range ms {
		if m.ID == NativeMigrationID {
			return storage.Contribution{Module: "gitwebhook-native", Migrations: []storage.Migration{m}}
		}
	}
	panic("gitwebhook/postgres: embedded migration " + NativeMigrationID + " is missing")
}
