package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

const (
	tenantA domain.TenantID = "tenant-a"
	tenantB domain.TenantID = "tenant-b"
)

func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func baseManifest(t testing.TB, extra ...storage.Contribution) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(append([]storage.Contribution{storage.BaseContribution()}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// env is a migrated database with the standard seed data.
type env struct {
	db  *postgres.Database
	mig *pgxpool.Pool
}

// newEnv creates a fresh database, applies the base schema as the migration
// role and seeds two tenants with projects, users, memberships and audit rows
// through the owner role.
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := testCtx(t)
	db := postgres.New(t)
	mig := db.MigrationPool(ctx, 2)
	if _, err := storage.Migrate(ctx, mig, baseManifest(t), storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := &env{db: db, mig: mig}
	e.exec(t, `
INSERT INTO tenants (id, key, display_name) VALUES ('tenant-a', 'a', 'Tenant A'), ('tenant-b', 'b', 'Tenant B');
INSERT INTO projects (tenant_id, id, key, task_workflow)
  SELECT 'tenant-a', 'pa' || n, 'pa' || n, '{}' FROM generate_series(1, 3) n;
INSERT INTO projects (tenant_id, id, key, task_workflow)
  SELECT 'tenant-b', 'pb' || n, 'pb' || n, '{}' FROM generate_series(1, 5) n;
INSERT INTO users (id, issuer, subject) VALUES
  ('u1', 'https://issuer.example', 'subject-1'), ('u2', 'https://issuer.example', 'subject-2'),
  ('u3', 'https://other.example', 'subject-1');
INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES
  ('tenant-a', 'm1', 'u1', NULL, 'admin'),
  ('tenant-a', 'm2', 'u1', 'pa1', 'engineer'),
  ('tenant-b', 'm3', 'u1', NULL, 'viewer'),
  ('tenant-b', 'm4', 'u2', NULL, 'reviewer');
INSERT INTO audit_events (tenant_id, id, actor_type, actor_id, event_type, resource_type, resource_id)
  VALUES ('tenant-a', 'au-a', 'system', 'system', 'seed', 'project', 'pa1'),
         ('tenant-b', 'au-b', 'system', 'system', 'seed', 'project', 'pb1');`)
	return e
}

func (e *env) exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.mig.Exec(testCtx(t), sql, args...); err != nil {
		t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func (e *env) runtime(t testing.TB, maxConns int32) *pgxpool.Pool {
	t.Helper()
	return e.db.RuntimePool(testCtx(t), maxConns)
}

// pgCode returns the SQLSTATE of err, or "" if it is not a PostgreSQL error.
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func requireCode(t testing.TB, err error, want, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: succeeded, want SQLSTATE %s", what, want)
		return
	}
	if got := pgCode(err); got != want {
		t.Errorf("%s: SQLSTATE %q (%v), want %s", what, got, err, want)
	}
}
