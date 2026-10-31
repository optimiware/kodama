package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
	"github.com/optimiware/kodama/test/support/layer"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// env is one migrated database, shared by the cases of a test. The store runs
// on a pool of the non-owner runtime role (no superuser, no BYPASSRLS, only the
// column grants of the migrations); the owner pool seeds and resets.
type env struct {
	t     *testing.T
	db    *pgtest.Database
	owner *pgxpool.Pool
	rt    *pgxpool.Pool
}

// The scopes of the shared suite: t1/p1 is under test, t2/p1 is another tenant
// with the same project name and t1/p2 another project of the first tenant. Each
// has the pipeline the runs name.
var seeded = []struct{ tenant, project string }{{"t1", "p1"}, {"t1", "p2"}, {"t2", "p1"}}

// withDatabase runs the database checks as the "postgres" subtest. The unit
// layer (-tags unit) cannot reach a service, and a skipped subtest would
// invalidate the test that holds it, so there they are not registered at all
// and only the infrastructure-free checks of the test run. Everywhere else
// newEnv requires PostgreSQL: it skips only when KODAMA_REQUIRE_POSTGRES=1 is
// unset and no service is configured, and fails when it is set. The database
// evidence of the task is the run with the variable set and without the tag.
func withDatabase(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	if layer.Unit {
		t.Log("the PostgreSQL checks run outside the unit layer: go test without -tags unit, with KODAMA_REQUIRE_POSTGRES=1")
		return
	}
	t.Run("postgres", fn)
}

// newEnv provisions a database and applies the whole schema as the owner. It
// skips in the unit layer and, without KODAMA_REQUIRE_POSTGRES=1, when no
// PostgreSQL is configured; with it a missing database fails the test.
func newEnv(t *testing.T) *env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := &env{t: t, db: db, owner: db.MigrationPool(ctx, 4), rt: db.RuntimePool(ctx, 40)}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e.ownerExec(`INSERT INTO tenants (id, key) VALUES ('t1', 't1'), ('t2', 't2')`)
	for _, s := range seeded {
		e.ownerExec(`INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ($1, $2::text, $2::text, '{}')`, s.tenant, s.project)
		e.ownerExec(`INSERT INTO pipelines (tenant_id, project_id, id, name, definition, source) VALUES ($1, $2, 'pipe1', 'pipe1', '{}', 'api')`,
			s.tenant, s.project)
	}
	return e
}

func (e *env) ownerExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// reset removes every run and what hangs off it, as the owner. Tenants,
// projects and pipelines stay.
func (e *env) reset() {
	e.t.Helper()
	e.ownerExec(`TRUNCATE runs, signal_records, audit_events CASCADE`)
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// store returns a store over the runtime pool around clock.
func (e *env) store(clock runtime.Clock) *Store {
	e.t.Helper()
	s, err := New(e.rt, clock)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// factory builds a fresh, empty subject for each case of the shared suite: it
// resets the database, then returns a store whose clock is the case's clock.
func (e *env) factory(tb kernelcontracts.TB, clock runtime.Clock) kernelcontracts.Subject {
	e.reset()
	s, err := New(e.rt, clock)
	if err != nil {
		tb.Fatalf("new store: %v", err)
	}
	return s
}
