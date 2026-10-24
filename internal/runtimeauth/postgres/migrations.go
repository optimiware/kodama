package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-019's contribution. The composition root lists it after
// storage.BaseContribution() (it references projects) and, once T-020 exists,
// after the execution contribution.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_migration_reapply
	}
	return storage.Contribution{Module: "runtimeauth", Migrations: ms}
}
