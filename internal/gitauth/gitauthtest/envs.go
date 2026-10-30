package gitauthtest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/memory"
	gitpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// MemEnv is the grant store in memory: one memdb kernel.
type MemEnv struct {
	DB  *memdb.Store
	Mem *memory.Store
}

var _ Env = (*MemEnv)(nil)

// NewMemEnv wires the memory adapter over a fresh kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	db := memdb.New(opts...)
	s, err := memory.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return &MemEnv{DB: db, Mem: s}
}

// OpenMem is the Open of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

func (e *MemEnv) Store() gitauth.Store { return e.Mem }

func (e *MemEnv) SeedProject(testing.TB, domain.Scope) {}

func (e *MemEnv) GrantCount(t testing.TB, tenant domain.TenantID) int {
	t.Helper()
	n, err := e.Mem.Count(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *MemEnv) RevocationAudits(t testing.TB, tenant domain.TenantID) []AuditRow {
	t.Helper()
	recs, err := e.Mem.Audits(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditRow
	for _, r := range recs {
		out = append(out, AuditRow{Action: r.Action, ActorType: string(r.Actor.Type), ActorID: r.Actor.ID, Reason: r.Reason, Revoked: r.Revoked})
	}
	return out
}

// PGEnv is the PostgreSQL adapter over an isolated migrated database.
type PGEnv struct {
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	PG      *gitpg.Store
}

var _ Env = (*PGEnv)(nil)

// OpenPostgres is the Open of the PostgreSQL adapter: an isolated migrated
// database. It skips without PostgreSQL, and always in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
	return NewPGEnv(t)
}

// NewPGEnv provisions the database and applies the baseline and migration 006.
func NewPGEnv(t testing.TB) *PGEnv {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &PGEnv{DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 16)}
	manifest, err := storage.NewManifest(storage.BaseContribution(), gitpg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, e.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if e.PG, err = gitpg.NewStore(e.Runtime); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *PGEnv) Store() gitauth.Store { return e.PG }

// OwnerExec runs a statement as the owner (outside row-level security), for
// seeding and for inspecting stored rows.
func (e *PGEnv) OwnerExec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.Owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *PGEnv) SeedProject(t testing.TB, scope domain.Scope) {
	t.Helper()
	e.OwnerExec(t, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	e.OwnerExec(t, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5) ON CONFLICT DO NOTHING`,
		string(scope.TenantID), string(scope.ProjectID))
}

func (e *PGEnv) GrantCount(t testing.TB, tenant domain.TenantID) int {
	t.Helper()
	var n int
	if err := e.Owner.QueryRow(context.Background(), `SELECT count(*) FROM git_grants WHERE tenant_id = $1`, string(tenant)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *PGEnv) RevocationAudits(t testing.TB, tenant domain.TenantID) []AuditRow {
	t.Helper()
	rows, err := e.Owner.Query(context.Background(), `SELECT action, actor_type, actor_id, coalesce(reason, ''), (metadata ->> 'revoked')::int
		FROM audit WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = $1 ORDER BY seq`, string(tenant), gitauth.AuditResourceType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.Action, &r.ActorType, &r.ActorID, &r.Reason, &r.Revoked); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
