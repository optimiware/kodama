package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-010's contribution. The composition root lists it after
// storage.BaseContribution() (it reuses users, tenants, projects and memberships)
// and after the events contribution when events are appended in the same
// database.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by TestMigrationReapply
	}
	return storage.Contribution{Module: "identity", Migrations: ms}
}
