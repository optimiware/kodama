// Package worktest is the shared fixture of the work module's HTTP tests: an
// isolated, migrated PostgreSQL database with the runtime-role pool, the real
// work store over the real event store, a database-backed fixture authority
// (principals and grants stand in for the identity module) and permissive
// writer/subject guards (the workspace and evidence modules are not part of this
// task). It is imported only by tests.
package worktest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Stack is one migrated database with the work service wired over it.
type Stack struct {
	T       testing.TB
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	Events  *eventspg.Store
	Store   *workpg.Store
	Service *work.Service
}

// NewStack provisions the database, migrations and fixtures.
func NewStack(t testing.TB) *Stack {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := &Stack{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 16)}
	manifest, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workpg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, s.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rt := pgx.Identifier{db.RuntimeRole}.Sanitize()
	for _, ddl := range []string{
		`CREATE TABLE t011_principals (tenant_id kodama_id NOT NULL, principal_id text NOT NULL, actor_type text NOT NULL, PRIMARY KEY (tenant_id, principal_id))`,
		`CREATE TABLE t011_grants (tenant_id kodama_id NOT NULL, principal_id text NOT NULL, perm text NOT NULL, PRIMARY KEY (tenant_id, principal_id, perm))`,
	} {
		s.OwnerExec(ddl)
		table := ddl[len("CREATE TABLE "):]
		table = table[:indexSpace(table)]
		s.OwnerExec(fmt.Sprintf(`ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY; ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;
			CREATE POLICY tenant_all ON %[1]s USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
			CREATE POLICY owner_all ON %[1]s TO CURRENT_USER USING (true) WITH CHECK (true);
			GRANT SELECT, INSERT, UPDATE, DELETE ON %[1]s TO %[2]s`, table, rt))
	}
	opts := eventstest.DefaultOptions()
	s.Events, err = eventspg.NewStore(s.Runtime, opts)
	if err != nil {
		t.Fatal(err)
	}
	s.Store, err = workpg.NewStore(s.Runtime, s.Events, func(tx pgx.Tx) work.TxGuards {
		g := txGuards{tx}
		return work.TxGuards{Permissions: g, Writers: g, Dependencies: work.NoDependencyGraph{}, Subjects: g}
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Service, err = work.NewService(work.Config{Repository: s.Store, Authority: authority{s}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func indexSpace(s string) int {
	for i := range s {
		if s[i] == ' ' {
			return i
		}
	}
	return len(s)
}

// OwnerExec runs SQL as the migration (owner) role.
func (s *Stack) OwnerExec(sql string, args ...any) {
	s.T.Helper()
	if _, err := s.Owner.Exec(context.Background(), sql, args...); err != nil {
		s.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// SeedProject creates the tenant (if new) and a project with the WIP limit.
func (s *Stack) SeedProject(tenant, project string, wip int) {
	s.T.Helper()
	s.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, tenant)
	s.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, $3)`, tenant, project, wip)
}

// Principal registers a principal with an actor type and grants.
func (s *Stack) Principal(tenant, id string, typ work.ActorType, perms ...work.Permission) {
	s.T.Helper()
	s.OwnerExec(`INSERT INTO t011_principals (tenant_id, principal_id, actor_type) VALUES ($1, $2, $3)`, tenant, id, string(typ))
	for _, p := range perms {
		s.OwnerExec(`INSERT INTO t011_grants (tenant_id, principal_id, perm) VALUES ($1, $2, $3)`, tenant, id, string(p))
	}
}

// Revoke removes one grant.
func (s *Stack) Revoke(tenant, id string, perm work.Permission) {
	s.T.Helper()
	s.OwnerExec(`DELETE FROM t011_grants WHERE tenant_id = $1 AND principal_id = $2 AND perm = $3`, tenant, id, string(perm))
}

type authority struct{ s *Stack }

func (a authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (work.Actor, error) {
	var typ string
	err := storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_type FROM t011_principals WHERE tenant_id = $1 AND principal_id = $2`, string(scope.TenantID), principalID).Scan(&typ)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return work.Actor{}, work.ErrHidden
	}
	return work.Actor{ID: principalID, Type: work.ActorType(typ)}, err
}

func (a authority) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	return storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return txGuards{tx}.Authorize(ctx, actor, scope, perm)
	})
}

type txGuards struct{ tx pgx.Tx }

func (g txGuards) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	var one int
	err := g.tx.QueryRow(ctx, `SELECT 1 FROM t011_grants WHERE tenant_id = $1 AND principal_id = $2 AND perm = $3`,
		string(scope.TenantID), actor.ID, string(perm)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return work.ErrDenied
	}
	return err
}

// WriterSettled: no workspace module here, so no writer is ever unsettled.
func (txGuards) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, nil
}

// VerifySubject accepts the claim as the current subject.
func (txGuards) VerifySubject(_ context.Context, _ domain.Scope, _ domain.WorkItemID, c work.ReviewClaim) (work.VerifiedSubject, error) {
	return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
}

// VerifyEvidence accepts every reference.
func (txGuards) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }
