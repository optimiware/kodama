package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/optimiware/kodama/internal/storage"
)

// Contributions is the ADR 0013 fresh-database baseline. Legacy execution,
// runtime-grant and artifact migrations refer to removed tables and must not be
// appended. Phase 3 ports the server adapters to this schema.
func Contributions() []storage.Contribution {
	return []storage.Contribution{storage.BaseContribution()}
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
