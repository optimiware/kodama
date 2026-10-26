package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:generate go run ./gen

//go:embed migrations/*.sql
var migrationsFS embed.FS

// WorkspaceFKMigrationID is the migration of the workspace module's deferred
// constraints, authored at internal/workspace/migrations.
const WorkspaceFKMigrationID = "T020_001_attempt_fk.sql"

func loadOwn() []storage.Migration {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by the migration tests
	}
	return ms
}

// WorkspaceFKSQL returns the SQL of T020_001_attempt_fk.sql as embedded.
func WorkspaceFKSQL() string { return workspaceAttemptFKSQL }

// Migrations returns the execution schema for a fresh database, in the order
// the composition root must list it:
//
//	storage.NewManifest(append([]storage.Contribution{
//	    storage.BaseContribution(), eventspg.Migrations(), /* work, pipeline, secrets, workspace, artifacts ... */
//	}, execpg.Migrations()...)...)
//
// It is Schema followed by CleanupMigrations and RunResourceMigrations. A composition that has already
// been deployed lists Schema where it always did and appends CleanupMigrations
// after everything it has applied (see CleanupMigrations).
func Migrations() []storage.Contribution {
	return append(Schema(), CleanupMigrations(), RunResourceMigrations())
}

// Schema returns the execution tables and their dependent constraints as
// separately registered contributions, in their frozen order.
//
// The tables (T020_002) come first. The dependent foreign keys follow as their
// own contributions: the workspace module's T020_001, then the artifact producer
// (T020_003) and secret pin (T020_004) constraints. The manifest orders
// contributions as given rather than by task number, and the migration ledger
// refuses to apply a pending migration ordered before an applied one, so this
// order must not change once deployed. The contributions need the work item,
// pipeline definition, secret, workspace and artifact tables to exist; list
// them earlier.
func Schema() []storage.Contribution {
	var tables, links []storage.Migration
	for _, m := range loadOwn() {
		switch m.ID {
		case "T020_002_runs.sql":
			tables = append(tables, m)
		case CleanupMigrationID, RunResourcesMigrationID:
		default:
			links = append(links, m)
		}
	}
	return []storage.Contribution{
		{Module: "execution", Migrations: tables},
		{Module: "execution-workspace-links", Migrations: []storage.Migration{{ID: WorkspaceFKMigrationID, SQL: workspaceAttemptFKSQL}}},
		{Module: "execution-links", Migrations: links},
	}
}

// CleanupMigrations returns the workload removal bookkeeping (T086_001) as its
// own final contribution. The ledger refuses a pending migration ordered
// before an applied one, so a composition that deployed execution before this
// existed must list it after every contribution it has already applied. The
// application manifest lists it after the runtime authority schema.
func CleanupMigrations() storage.Contribution {
	var cleanup []storage.Migration
	for _, m := range loadOwn() {
		if m.ID == CleanupMigrationID {
			cleanup = append(cleanup, m)
		}
	}
	return storage.Contribution{Module: "execution-cleanup", Migrations: cleanup}
}

// CleanupMigrationID is the migration of workload removal bookkeeping.
const CleanupMigrationID = "T086_001_workload_cleanup.sql"

// RunResourcesMigrationID is the migration of the run_shared resource lifecycle
// and persistence (T-112).
const RunResourcesMigrationID = "T112_001_run_resources.sql"

// RunResourceMigrations returns the additive run_shared tables and columns
// (T112_001) as their own final contribution. A composition that has already
// deployed execution lists it after every contribution it has applied (the
// ledger refuses a pending migration ordered before an applied one); this
// package does not wire it into any composition (T-116 owns the ordering).
func RunResourceMigrations() storage.Contribution {
	var out []storage.Migration
	for _, m := range loadOwn() {
		if m.ID == RunResourcesMigrationID {
			out = append(out, m)
		}
	}
	return storage.Contribution{Module: "execution-run-resources", Migrations: out}
}
