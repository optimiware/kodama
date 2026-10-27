package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// Contributions lists the schema contributions of the core assembly in the
// order they must be applied.
//
// It is the squashed baseline first: 001_schema.sql holds every capability's
// tables, views, functions, indexes and policies in dependency order, and
// 002_grants.sql holds every GRANT and REVOKE that names the runtime role. A
// PUBLIC revocation stays in the schema migration beside the definition it
// protects, so an interrupted run is fail-closed.
//
// Later schema changes append one contribution per change after it; the
// migration ledger refuses a pending migration ordered before an applied one,
// so the list is part of the deployed contract and only ever grows at the end.
// The additive run_shared resource tables and columns (T-112) follow the last
// contribution an earlier release applied. The execution store reads their
// columns unconditionally, so they must exist before the store serves a run;
// the migration enables nothing: no route, backend, policy or admission of the
// new format is wired by it.
func Contributions() []storage.Contribution {
	return []storage.Contribution{storage.BaseContribution(), execpg.RunResourceMigrations()}
}

// Manifest validates the contribution list.
func Manifest() (storage.Manifest, error) { return storage.NewManifest(Contributions()...) }

// MigrateConfig names the two database identities of a migration: the owner
// role that applies the schema and the runtime role that is granted access to
// it. They are never the same role.
type MigrateConfig struct {
	// MigrationURL connects as the owner (database-migration-url).
	MigrationURL string
	// RuntimeURL identifies the runtime role by its user name
	// (database-runtime-url); the migrations grant it its minimal privileges.
	// RuntimeRole names the role directly instead, for a migration Pod that
	// must not hold the runtime credential. Exactly one of them is set.
	RuntimeURL  string
	RuntimeRole string
	MaxConns    int32
}

// Migrate applies the manifest in order through the owner identity.
func Migrate(ctx context.Context, cfg MigrateConfig) (storage.MigrateResult, error) {
	if cfg.MigrationURL == "" || (cfg.RuntimeURL == "") == (cfg.RuntimeRole == "") {
		return storage.MigrateResult{}, errors.New("migration requires the owner database URL and exactly one of the runtime database URL or the runtime role name")
	}
	role := cfg.RuntimeRole
	if cfg.RuntimeURL != "" {
		var err error
		if role, err = roleOf(cfg.RuntimeURL); err != nil {
			return storage.MigrateResult{}, err
		}
	}
	if owner, err := roleOf(cfg.MigrationURL); err != nil {
		return storage.MigrateResult{}, err
	} else if owner == role {
		return storage.MigrateResult{}, errors.New("the migration and runtime identities must differ: the serving role must not own the schema")
	}
	manifest, err := Manifest()
	if err != nil {
		return storage.MigrateResult{}, err
	}
	conns := cfg.MaxConns
	if conns == 0 {
		conns = 2
	}
	pool, err := storage.Open(ctx, storage.Config{URL: cfg.MigrationURL, MaxConns: conns, ApplicationName: "kodama-migrate"})
	if err != nil {
		return storage.MigrateResult{}, fmt.Errorf("open migration pool: %w", err)
	}
	defer pool.Close()
	return storage.Migrate(ctx, pool, manifest, storage.MigrateOptions{RuntimeRole: role})
}

func roleOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("a database URL must name its role (postgres://role:password@host/db)")
	}
	return u.User.Username(), nil
}
