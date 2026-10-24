package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-012's contribution: T012_001_configuration.sql
// (variables, secrets, scope keys) and T012_002_references.sql (run pins and
// consumer handling).
//
// Order in the manifest: storage.BaseContribution() first (tenants, projects,
// audit, kodama_id), then the HTTP idempotency and event bus contributions the
// module's writes depend on, then this one:
//
//	storage.NewManifest(storage.BaseContribution(), httppg.Migrations(), eventspg.Migrations(), secretspg.Migrations())
//
// T-020 adds the run ownership foreign key for secret_pins.consumer_id.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_migration_reapply
	}
	return storage.Contribution{Module: "secrets", Migrations: ms}
}
