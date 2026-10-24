package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config describes a connection pool. The zero value of every field except URL
// selects the pgxpool default.
type Config struct {
	// URL is a PostgreSQL connection URI or key/value DSN. The runtime pool
	// uses the non-owner runtime role; the migration pool the owner role.
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// ApplicationName is reported to PostgreSQL for diagnostics.
	ApplicationName string
}

// Pool is a pgx connection pool. It embeds *pgxpool.Pool so adapters can use
// it directly; tenant-scoped work must go through WithTenant.
type Pool struct {
	*pgxpool.Pool
}

// Open creates a pool and verifies connectivity. Sessions run in UTC. There is
// no package-level state; callers own the returned pool and must Close it.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.URL == "" {
		return nil, errors.New("storage: database URL is required")
	}
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgconn redacts the password in parse errors.
		return nil, fmt.Errorf("storage: parse database URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	if cfg.ApplicationName != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}
	p, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("storage: open pool: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("storage: ping database: %w", err)
	}
	return &Pool{Pool: p}, nil
}
