package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-023's contribution: T023_001_log_records.sql.
//
// Order in the manifest: after the artifact and execution contributions it
// references (the composition root lists it; this task does not edit the
// central manifest):
//
//	storage.NewManifest(append([]storage.Contribution{
//	    storage.BaseContribution(), /* ... */ artifactspg.Migrations(),
//	}, append(execpg.Migrations(), logspg.Migrations())...)...)
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "logs", Migrations: ms}
}
