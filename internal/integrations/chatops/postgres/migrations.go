package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-041's contribution: T041_001_chatops.sql (destinations,
// deliveries, attempt history, replay audit metadata and the restore fence).
//
// Order in the manifest: ChatOps references tenants and projects (base), and
// its authorization reads the identity tables, so it is listed after the event
// and identity contributions (chatopstest.Contributions is the executable form):
//
//	storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(),
//	    identitypg.Migrations() /* , other modules */, chatopspg.Migrations())
//
// Once deployed the order must not change: the ledger refuses a pending
// migration ordered before an applied one.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return storage.Contribution{Module: "chatops", Migrations: ms}
}
