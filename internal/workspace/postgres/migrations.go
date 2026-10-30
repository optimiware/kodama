package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationID is the migration of the workspace repository associations and
// internal checkpoints (T-147). The baseline workspace tables are part of
// 001_schema.sql; 007 is the repositories table, 008 the Git grants and 009 the
// push records (docs/implementation/internal-git-service.md, "Migration
// numbering").
const MigrationID = "010_workspace_repositories.sql"

// Migrations returns the internal-hosting tables as their own contribution. It
// extends the baseline workspaces table, so a composition lists it after the
// baseline; the ledger refuses a pending migration ordered before an applied
// one. This package does not wire it into any composition: T-132 registers it
// in internal/app/migrate.go.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "workspace", Migrations: ms}
}
