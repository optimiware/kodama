package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-030's contribution. The composition root lists it after
// storage.BaseContribution(): it references tenants and projects, the kodama_id
// domain and the audit table that registration appends to. Nothing references
// it, so its position among later contributions does not matter. It emits no
// event, so it does not depend on the event contribution.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by TestAgentMigrations
	}
	return storage.Contribution{Module: "agents", Migrations: ms}
}
