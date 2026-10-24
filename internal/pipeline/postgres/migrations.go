package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-014's contribution. The composition root lists it after
// storage.BaseContribution(): it references tenants, projects and the kodama_id
// domain, and registration appends to the audit table. It does not depend on
// the event contribution because registration emits no event.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_pipeline_migrations
	}
	return storage.Contribution{Module: "pipeline", Migrations: ms}
}
