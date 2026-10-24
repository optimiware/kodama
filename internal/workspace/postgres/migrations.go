package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-013's workspace, publication-receipt and retained-
// reference schema. The composition root lists it after the base contribution
// and the event bus contribution (it writes audit rows and event outbox rows
// through events/postgres.Store.Append) and after the project tables:
//
//	storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workspacepg.Migrations(), ...)
//
// Cross-module foreign keys to work items, step attempts and artifacts are
// added by those modules' own contributions.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by TestMigrationReapply
	}
	return storage.Contribution{Module: "workspace", Migrations: ms}
}
