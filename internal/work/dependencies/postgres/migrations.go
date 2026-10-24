// Package postgres is the PostgreSQL adapter of the business dependency graph:
// the table, serialized link mutations, listing and the transaction-bound
// admission guard of the work module.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is further
// constrained by forced row level security.
//
// Lock order (identical to internal/work/postgres, no new lock scheme):
//
//  1. the project row, FOR NO KEY UPDATE: every link insertion and removal takes
//     it, so graph changes serialize with work transitions and with each other;
//  2. the dependent work item row, FOR UPDATE;
//  3. link and prerequisite rows, read without locking;
//  4. the event backlog and scope rows inside the event store's Append, last.
//
// The admission guard runs inside a work transition, which already holds 1 and
// 2; it only reads, and it never locks the project after the item.
package postgres

import (
	"embed"

	"github.com/optimiware/kodama/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns T-045's contribution. The composition root lists it after
// storage.BaseContribution(), the events contribution and T-011's work
// contribution: it references work_items, tenants and the kodama_id domain, and
// its mutations append to work_item_history, audit and the event outbox.
func Migrations() storage.Contribution {
	ms, err := storage.LoadFS(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; covered by Test_dependency_isolation_and_migrations
	}
	return storage.Contribution{Module: "work_dependencies", Migrations: ms}
}
