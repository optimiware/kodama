package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-035's contribution: T035_001_delivery.sql (environments,
// immutable snapshots, deployments, append-only history).
//
// Order in the manifest: delivery references tenants and projects (base), the
// attempts the deployments originate from (execution tables, T020_002) and the
// image artifact (T015_001), so it is listed after them (deliverytest.Contributions
// is the executable form of this order):
//
//	storage.NewManifest(append([]storage.Contribution{
//	    storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(),
//	    workpg.Migrations(), secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(),
//	    artifactspg.Migrations(),
//	}, append(execpg.Migrations(), rtpg.Migrations(), deliverypg.Migrations())...)...)
//
// Once deployed the order must not change: the ledger refuses a pending
// migration ordered before an applied one.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "delivery", Migrations: ms}
}
