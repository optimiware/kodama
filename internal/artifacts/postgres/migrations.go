package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-015's contribution: T015_001_artifacts.sql (artifacts,
// object records, retention pins).
//
// Order in the manifest: storage.BaseContribution() first (tenants, projects,
// audit, kodama_id and the tenant-enumeration function the reconciler uses),
// then this one:
//
//	storage.NewManifest(storage.BaseContribution(), artifactspg.Migrations())
//
// T-020 adds the attempt ownership foreign key over
// (tenant_id, project_id, attempt_id) when it creates the run tables.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by TestMigrationReapply
	}
	return storage.Contribution{Module: "artifacts", Migrations: ms}
}
