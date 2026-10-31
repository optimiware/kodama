package pipelines_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/pipelines"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

// pgEnv is a migrated database with two tenants. owner is the migration role
// (it seeds and inspects); the registry runs as the runtime role, so row-level
// security and grants apply to it exactly as in the product. A skipped database
// is not evidence: the unit layer skips these tests, and KODAMA_REQUIRE_POSTGRES=1
// turns a missing database into a failure.
type pgEnv struct {
	fixture
	owner *pgxpool.Pool
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	ctx := t.Context()
	db := postgres.New(t)
	owner := db.MigrationPool(ctx, 2)
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, m, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%.80s: %v", sql, err)
		}
	}
	exec(t, `INSERT INTO tenants (id, key) VALUES ($1, 'a'), ($2, 'b')`, string(tenantA), string(tenantB))
	e := &pgEnv{owner: owner}
	e.fixture = fixture{
		svc: pipelines.New(pipelines.NewPostgres(db.RuntimePool(ctx, 8))),
		project: func(t *testing.T, tenant domain.TenantID) domain.Scope {
			t.Helper()
			scope := domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(fresh("p"))}
			exec(t, `INSERT INTO projects (tenant_id, id, key, task_workflow) VALUES ($1, $2, $3, '{}')`, string(scope.TenantID), string(scope.ProjectID), string(scope.ProjectID))
			return scope
		},
		repo: func(t *testing.T, scope domain.Scope) string {
			t.Helper()
			id := fresh("repo")
			exec(t, `INSERT INTO repositories (tenant_id, id, project_id, name) VALUES ($1, $2, $3, $4)`, string(scope.TenantID), id, string(scope.ProjectID), id)
			return id
		},
	}
	return e
}

func (e *pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// Test_pipelines_project_unique_postgres runs the project-unique contract on
// real PostgreSQL, then proves the database constraint is only the backstop.
func Test_pipelines_project_unique_postgres(t *testing.T) {
	e := newPGEnv(t)
	runProjectUnique(t, e.fixture)

	t.Run("the_database_enforces_it_too", func(t *testing.T) {
		scope := e.project(t, tenantA)
		repo := e.repo(t, scope)
		if _, err := e.svc.Register(t.Context(), pipelines.RegisterRequest{Scope: scope, Document: doc("deploy", "api")}); err != nil {
			t.Fatal(err)
		}
		// A write that bypasses the registry (a second repository's row under the
		// same name) is refused by the project-unique constraint.
		_, err := e.owner.Exec(t.Context(), `INSERT INTO pipelines (tenant_id,project_id,id,repo_id,name,definition,source,source_commit)
			VALUES ($1,$2,'pl_direct',$3,'deploy','{}','git',$4)`, string(scope.TenantID), string(scope.ProjectID), repo, commit(1))
		if err == nil || !strings.Contains(err.Error(), "pipelines_name_unique") {
			t.Fatalf("direct duplicate insert = %v, want a pipelines_name_unique violation", err)
		}
	})

	t.Run("the_registry_refuses_without_the_constraint", func(t *testing.T) {
		// Drop the project-unique constraint (in this database only): the
		// registry's own check must still refuse a duplicate name from another
		// repository, so uniqueness never depends on the database error alone.
		if _, err := e.owner.Exec(t.Context(), `ALTER TABLE pipelines DROP CONSTRAINT pipelines_name_unique`); err != nil {
			t.Fatal(err)
		}
		scope := e.project(t, tenantA)
		r1, r2 := e.repo(t, scope), e.repo(t, scope)
		if _, err := e.svc.Index(t.Context(), index(e.fixture, scope, r1, "e1", 1, doc("ci", "one"))); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Index(t.Context(), index(e.fixture, scope, r2, "e2", 2, doc("ci", "two"))); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("index without the constraint = %v", err)
		}
		if _, err := e.svc.Register(t.Context(), pipelines.RegisterRequest{Scope: scope, Document: doc("ci", "three")}); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatalf("register without the constraint = %v", err)
		}
		if n := e.count(t, `SELECT count(*) FROM pipelines WHERE tenant_id=$1 AND project_id=$2 AND name='ci'`, string(scope.TenantID), string(scope.ProjectID)); n != 1 {
			t.Fatalf("%d rows hold the name, want 1", n)
		}
	})
}

// Test_pipelines_source_conflict_postgres runs the source-ownership contract on
// real PostgreSQL and checks what a refusal and a replay leave in the database.
func Test_pipelines_source_conflict_postgres(t *testing.T) {
	e := newPGEnv(t)
	runSourceConflict(t, e.fixture)

	t.Run("records_hold_identifiers_not_documents", func(t *testing.T) {
		scope := e.project(t, tenantA)
		r1, r2 := e.repo(t, scope), e.repo(t, scope)
		e.svc.Index(t.Context(), index(e.fixture, scope, r1, "e1", 1, doc("ci", "owner-marker")))
		if _, err := e.svc.Index(t.Context(), index(e.fixture, scope, r2, "e2", 2, doc("ci", "intruder-marker"))); !errors.Is(err, pipelines.ErrSourceConflict) {
			t.Fatal(err)
		}
		for range 2 { // a replay writes no second row
			e.svc.Index(t.Context(), index(e.fixture, scope, r2, "e2", 2, doc("ci", "intruder-marker")))
		}
		q := `SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND project_id=$2 AND event_type='pipeline.indexed' AND payload::text LIKE $3`
		if n := e.count(t, q, string(scope.TenantID), string(scope.ProjectID), `%"refusalCode": "source_conflict"%`); n != 1 {
			t.Fatalf("%d conflict rows, want 1", n)
		}
		if n := e.count(t, q, string(scope.TenantID), string(scope.ProjectID), `%marker%`); n != 0 {
			t.Fatalf("%d audit rows carry document content", n)
		}
		if n := e.count(t, `SELECT count(*) FROM pipelines WHERE tenant_id=$1 AND project_id=$2`, string(scope.TenantID), string(scope.ProjectID)); n != 1 {
			t.Fatalf("%d pipelines, want only the owner", n)
		}
	})

	t.Run("a_failed_transaction_leaves_no_record", func(t *testing.T) {
		// A cancelled context aborts before commit: neither the definition nor
		// its attempt record may remain, so the retry creates it.
		scope := e.project(t, tenantA)
		repo := e.repo(t, scope)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := e.svc.Index(ctx, index(e.fixture, scope, repo, "e1", 1, doc("ci", "v1"))); err == nil {
			t.Fatal("a cancelled index must fail")
		}
		res, err := e.svc.Index(t.Context(), index(e.fixture, scope, repo, "e1", 1, doc("ci", "v1")))
		if err != nil || !res.Created || res.Replayed {
			t.Fatalf("retry = %+v, %v", res, err)
		}
	})
}
