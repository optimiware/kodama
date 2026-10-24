package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-032's contribution: T032_001_gateway.sql (model settings
// and revisions, run bindings, requests, usage).
//
// Order in the manifest: after the contributions of everything it references
// through composite foreign keys (the base tenancy tables, and the execution
// tables for runs and step attempts) and after the dependencies of its runtime
// checks (identity, secrets, runtime authority, events):
//
//	storage.NewManifest(append([]storage.Contribution{
//	    storage.BaseContribution(), eventspg.Migrations(), identitypg.Migrations(), secretspg.Migrations(),
//	    /* work, pipeline, workspace, artifacts, runtimeauth */
//	}, append(execpg.Migrations(), gatewaypg.Migrations())...)...)
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "gateway", Migrations: ms}
}
