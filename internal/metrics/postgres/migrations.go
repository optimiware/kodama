package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-042's contribution: history-derived flow views. The
// composition root lists it after storage.BaseContribution() and the work
// contribution, whose work_item_history table the views read.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_metrics_migration_reapply
	}
	return storage.Contribution{Module: "metrics", Migrations: ms}
}
