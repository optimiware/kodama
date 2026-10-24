package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the gate schema (T034_001_gates.sql). The composition root
// lists it after the identity, event and HTTP foundations, the work, pipeline,
// workspace and artifact modules and the execution contributions:
//
//	storage.NewManifest(append(append([]storage.Contribution{
//	    storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
//	    secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
//	}, execpg.Migrations()...), gatespg.Migrations()...)...)
//
// Once deployed its ID and SQL never change.
func Migrations() []storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return []storage.Contribution{{Module: "gates", Migrations: ms}}
}
