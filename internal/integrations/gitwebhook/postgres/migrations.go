package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-100's contribution: T100_001_git_triggers.sql (triggers,
// the delivery claim, the admission ledger and the trigger locator).
//
// Order in the manifest: it references tenants and projects (base), pipeline
// definitions, work items and runs, so the composition root lists it after the
// execution tables and, because the migration ledger refuses a pending migration
// ordered before an applied one, at the end of every contribution it has already
// applied:
//
//	storage.NewManifest(append(existingContributions, gitwebhookpg.Migrations())...)
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "gitwebhook", Migrations: ms}
}
