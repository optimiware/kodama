package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationID is the migration of the repository table (T-127). The numbers
// 001 and 002 are the squashed baseline; 003 and 004 belong to other
// capabilities and 007 is reserved for repositories (docs/implementation/
// internal-git-service.md, "Migration numbering").
const MigrationID = "007_git_repositories.sql"

// Migrations returns the repositories table as its own contribution. The ledger
// refuses a pending migration ordered before an applied one, so a composition
// lists it after 003 and 004. This package does not wire it into any
// composition: T-132 registers it in internal/app/migrate.go.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "repository", Migrations: ms}
}
