package stage

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// DB inspects the session's database through the probe relay, with the owner role (inspection and fixture damage
// only) and the non-owner runtime role (as the application sees data, forced row level security applying).
type DB struct {
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	fwd     *PortForward
}

// DB connects (once per Installation) through a port forward to the probe relay.
func (i *Installation) DB() *DB {
	i.T.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.db != nil {
		return i.db
	}
	ctx := i.Context(2 * time.Minute)
	fwd, err := i.Kube.Forward(ctx, i.S.Namespace, "svc/"+i.S.ProbeService, 5432)
	if err != nil {
		i.T.Fatalf("database probe: %v", err)
	}
	pool := func(key string, conns int32) *pgxpool.Pool {
		dsn, err := i.Kube.SecretValue(ctx, i.S.Namespace, i.S.ProbeSecret, key)
		if err != nil {
			i.T.Fatalf("probe Secret: %v", err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			i.T.Fatal(err)
		}
		u.Host = fmt.Sprintf("127.0.0.1:%d", fwd.Port)
		cfg, err := pgxpool.ParseConfig(u.String())
		if err != nil {
			i.T.Fatal(err)
		}
		cfg.MaxConns = conns
		p, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			i.T.Fatal(err)
		}
		return p
	}
	i.db = &DB{Owner: pool("owner-url", 4), Runtime: pool("runtime-url", 4), fwd: fwd}
	return i.db
}

// Close releases the pools and the port forward.
func (d *DB) Close() {
	d.Owner.Close()
	d.Runtime.Close()
	d.fwd.Close()
}

// OwnerCount runs a count as the owner role.
func (i *Installation) OwnerCount(sql string, args ...any) int {
	i.T.Helper()
	var n int
	if err := i.DB().Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		i.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// TryOwnerCount is OwnerCount for polling: a transient failure is reported, not fatal.
func (i *Installation) TryOwnerCount(sql string, args ...any) (int, error) {
	var n int
	err := i.DB().Owner.QueryRow(context.Background(), sql, args...).Scan(&n)
	return n, err
}

// MustScan scans one owner-role row.
func (i *Installation) MustScan(dst any, sql string, args ...any) {
	i.T.Helper()
	if err := i.DB().Owner.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		i.T.Fatalf("query: %v\n%s", err, sql)
	}
}

// OwnerExec runs a statement as the owner role.
func (i *Installation) OwnerExec(sql string, args ...any) {
	i.T.Helper()
	if _, err := i.DB().Owner.Exec(context.Background(), sql, args...); err != nil {
		i.T.Fatalf("exec: %v\n%s", err, sql)
	}
}

// TenantCount counts through the runtime role inside the tenant's context.
func (i *Installation) TenantCount(tenant string, sql string, args ...any) int {
	i.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), i.DB().Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		i.T.Fatalf("tenant count: %v\n%s", err, sql)
	}
	return n
}

// TenantStrings returns the first column of every row, read through the runtime role in the tenant's context.
func (i *Installation) TenantStrings(tenant string, sql string, args ...any) []string {
	i.T.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), i.DB().Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		i.T.Fatalf("tenant strings: %v\n%s", err, sql)
	}
	return out
}
