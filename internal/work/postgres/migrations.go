package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-011's contribution. The composition root lists it after
// storage.BaseContribution() and the events contribution: it references tenants,
// projects and the kodama_id domain, and its mutations append to the audit table
// and the event outbox.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_migration_reapply
	}
	return storage.Contribution{Module: "work", Migrations: ms}
}
