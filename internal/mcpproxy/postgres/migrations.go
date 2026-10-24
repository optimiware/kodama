package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-033's contribution. The composition root lists it after
// the contributions it references: storage.BaseContribution() (tenants,
// projects, audit, kodama_id) and, for the secrets and runtime authority it
// bridges to, the secrets and runtimeauth contributions. Nothing references it,
// so its position after those does not matter. It emits no event.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_migration_reapply
	}
	return storage.Contribution{Module: "mcpproxy", Migrations: ms}
}
