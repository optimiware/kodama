package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-004's contribution. The composition root lists it after
// storage.BaseContribution() because the idempotency table references tenants
// and projects.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by tests
	}
	return storage.Contribution{Module: "httpapi", Migrations: ms}
}
