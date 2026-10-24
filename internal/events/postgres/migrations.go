package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-005's outbox, inbox, observation, quarantine and replay
// schema. The composition root lists it after storage.BaseContribution():
//
//	storage.NewManifest(storage.BaseContribution(), postgres.Migrations(), ...)
//
// It references tenants, projects and the kodama_id domain, and writes replay
// audit rows to the audit table, all contributed by T-002.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by TestMigrationReapply
	}
	return storage.Contribution{Module: "events", Migrations: ms}
}
