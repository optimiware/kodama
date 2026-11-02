package repositorytest

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/repository"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// PGEnv is the repository persistence on real PostgreSQL: the baseline plus
// the repository contribution, the fixture tenants and projects, and an event
// appender that can fail after the real append.
type PGEnv struct {
	T testing.TB
	// DB, Migration and Runtime are the isolated database, its owner pool and
	// the non-owner runtime pool the store uses.
	DB        *pgtest.Database
	Migration *pgxpool.Pool
	Runtime   *pgxpool.Pool
	Events    *eventspg.Store
	PG        *repopg.Store

	mu    sync.Mutex
	fault error
}

var _ Env = (*PGEnv)(nil)

type pgFaultAppender struct{ env *PGEnv }

func (f pgFaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	res, err := f.env.Events.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewPGEnv builds the environment. It skips without PostgreSQL and always in
// the unit layer (test/support/postgres).
func NewPGEnv(t testing.TB) *PGEnv {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &PGEnv{T: t, DB: db, Migration: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 30)}
	manifest, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.Migration, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, s := range []domain.Scope{ScopeA, ScopeB} {
		if _, err := e.Migration.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, string(s.TenantID)); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		if _, err := e.Migration.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`,
			string(s.TenantID), string(s.ProjectID)); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}
	if e.Events, err = eventspg.NewStore(e.Runtime, eventstest.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	if e.PG, err = repopg.NewStore(e.Runtime, pgFaultAppender{env: e}, repopg.Options{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenPostgres is the OpenEnv of the PostgreSQL adapter.
func OpenPostgres(t testing.TB) Env { return NewPGEnv(t) }

// Store implements Env.
func (e *PGEnv) Store() repository.Store { return e.PG }

// FailEvents implements Env.
func (e *PGEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

// column reads one text column of the tenant's rows in the tenant's context: the
// outbox and audit policies are tenant-only.
func (e *PGEnv) column(t testing.TB, tenant domain.TenantID, sql string, args ...any) []string {
	t.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), e.Migration, tenant, func(ctx context.Context, tx pgx.Tx) error {
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
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// EventTypes implements Env.
func (e *PGEnv) EventTypes(t testing.TB, scope domain.Scope, id repository.ID) []string {
	t.Helper()
	return e.column(t, scope.TenantID, `SELECT event_type FROM outbox
		WHERE tenant_id = $1 AND convert_from(payload, 'UTF8')::jsonb -> 'resource' ->> 'id' = $2`, string(scope.TenantID), string(id))
}

// AuditActions implements Env.
func (e *PGEnv) AuditActions(t testing.TB, scope domain.Scope, id repository.ID) []string {
	t.Helper()
	return e.column(t, scope.TenantID, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type = 'repository' AND resource_id = $2`,
		string(scope.TenantID), string(id))
}
