package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations is the additive state schema contribution. It follows the
// baseline and any earlier contribution in the composition manifest.
func Migrations() storage.Contribution {
	migrations, err := storage.LoadFS(migrationFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by migration tests
	}
	return storage.Contribution{Module: "state", Migrations: migrations}
}
