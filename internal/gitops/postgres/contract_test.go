package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
	"github.com/optimiware/kodama/internal/gitops/postgres"
	gitpushpg "github.com/optimiware/kodama/internal/gitpush/postgres"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func manifest(t testing.TB, ops storage.Contribution) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(storage.BaseContribution(), repopg.Migrations(), gitauthpg.Migrations(), gitpushpg.Migrations(), gitpushpg.OperationsMigrations(), ops)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// pgEnv is the Env of the PostgreSQL adapter: the stores over a migrated
// database, sharing the contract's clock.
type pgEnv struct {
	T       testing.TB
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	store   *postgres.Store
	clock   *gitopstest.Clock
}

// openPG skips without PostgreSQL, and always in the unit layer.
func openPG(t testing.TB) gitopstest.Env {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
	return newPGEnv(t)
}

func newPGEnv(t testing.TB) *pgEnv {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &pgEnv{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 30), clock: gitopstest.NewClock()}
	if _, err := storage.Migrate(ctx, e.Owner, manifest(t, postgres.Migrations()), storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var err error
	if e.store, err = postgres.NewStore(e.Runtime, postgres.Options{Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []domain.TenantID{gitopstest.ContractTenantA, gitopstest.ContractTenantB} {
		e.OwnerExec(t, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(tenant))
	}
	return e
}

func (e *pgEnv) Leases() gitops.BackupLeases      { return e.store.Leases() }
func (e *pgEnv) Recoveries() gitops.RecoveryStore { return e.store.Recoveries() }
func (e *pgEnv) Intents() gitops.PruneIntents     { return e.store.Intents() }
func (e *pgEnv) Clock() *gitopstest.Clock         { return e.clock }

// Reopen is another store instance over the same database and clock.
func (e *pgEnv) Reopen(t testing.TB) gitopstest.Env {
	c := *e
	s, err := postgres.NewStore(e.Runtime, postgres.Options{Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	c.store = s
	return &c
}

// OwnerExec runs a statement as the owner (outside row level security).
func (e *pgEnv) OwnerExec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.Owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Seed creates the tenant, the project and the repositories of the scope.
func (e *pgEnv) Seed(t testing.TB, scope domain.Scope, repositoryIDs ...string) {
	t.Helper()
	e.OwnerExec(t, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	e.OwnerExec(t, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5) ON CONFLICT DO NOTHING`,
		string(scope.TenantID), string(scope.ProjectID))
	for _, id := range repositoryIDs {
		e.OwnerExec(t, `INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'main', 'internal', 'ready', 'user', 'usr_fixture', now(), now()) ON CONFLICT DO NOTHING`,
			string(scope.TenantID), string(scope.ProjectID), id, strings.ReplaceAll(id, "_", "-"))
	}
}

// Test_postgres_gitops_state_contract runs the shared durable-state contract on
// real PostgreSQL. It skips in the unit layer and without PostgreSQL
// (test/support/postgres).
func Test_postgres_gitops_state_contract(t *testing.T) { gitopstest.Contract(t, openPG) }

// layerSkip skips a test that needs PostgreSQL in the unit layer.
func layerSkip(t testing.TB) {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
}
