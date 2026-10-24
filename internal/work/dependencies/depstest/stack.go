// Package depstest is the shared fixture of the dependency graph's tests: an
// isolated, migrated PostgreSQL database with the runtime-role pool, the real
// T-011 work store and service wired with the real T-045 dependency guard, the
// real dependency store and service, and a database-backed fixture authority
// (principals and grants stand in for the identity module). Writer and review
// subject guards are permissive because the workspace and evidence modules are
// not part of this task. It is imported only by tests.
//
// Fixture tables carry the task resource prefix t045.
package depstest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	deppg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Prefix is the resource prefix of this task's fixtures.
const Prefix = "t045"

const (
	TenantA, ProjectA = "t_acme", "p_inventory"
	TenantB, ProjectB = "t_globex", "p_billing"
	// ProjectA2 is a second project of tenant A.
	ProjectA2 = "p_payments"
)

var (
	ScopeA  = domain.Scope{TenantID: TenantA, ProjectID: ProjectA}
	ScopeA2 = domain.Scope{TenantID: TenantA, ProjectID: ProjectA2}
	ScopeB  = domain.Scope{TenantID: TenantB, ProjectID: ProjectB}
)

// Principals of the fixture authority, registered in every tenant.
const (
	Engineer = "u_engineer" // edits intent, works, cancels, reopens
	Reader   = "u_reader"   // work_item:read only
	Reviewer = "u_reviewer" // completes work
	Outsider = "u_outsider" // authenticated, holds no grants
)

const Commit = "1111111111111111111111111111111111111111"

var (
	EngineerPerms = []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermCancel, work.PermReopen}
	ReaderPerms   = []work.Permission{work.PermRead}
	ReviewerPerms = []work.Permission{work.PermRead, work.PermComplete, work.PermReopen}
)

// Options customize a Stack.
type Options struct {
	// Conns is the runtime pool size; zero selects 24.
	Conns int32
	// MutateEvents adjusts the event store policies.
	MutateEvents func(*eventspg.Options)
	// PermissionBinder replaces the transaction-bound permission binder of the
	// dependency store (nil selects the fixture authority).
	PermissionBinder deppg.PermissionBinder
}

// Stack is one migrated database with both services wired over it.
type Stack struct {
	T       testing.TB
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	Events  *eventspg.Store
	Clock   *eventstest.Clock

	WorkStore *workpg.Store
	Work      *work.Service
	DepStore  *deppg.Store
	Deps      *dependencies.Service
	Authority *Authority
}

// Authority is the pre-transaction fixture authority over the grants table. When
// Outage is set it fails like an unreachable identity service.
type Authority struct {
	s      *Stack
	Outage atomic.Bool
}

// NewStack provisions the database, the migrations and the fixtures, and seeds
// tenants A and B with one project each (WIP limit 5) plus a second project of
// tenant A.
func NewStack(t testing.TB, o ...Options) *Stack {
	t.Helper()
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	if opt.Conns == 0 {
		opt.Conns = 24
	}
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := &Stack{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, opt.Conns), Clock: &eventstest.Clock{}}
	s.Authority = &Authority{s: s}
	s.Migrate(ctx)
	s.fixtureTables()

	opts := eventstest.DefaultOptions()
	opts.Clock = s.Clock.Now
	if opt.MutateEvents != nil {
		opt.MutateEvents(&opts)
	}
	var err error
	if s.Events, err = eventspg.NewStore(s.Runtime, opts); err != nil {
		t.Fatal(err)
	}
	if s.WorkStore, err = workpg.NewStore(s.Runtime, s.Events, BindGuards); err != nil {
		t.Fatal(err)
	}
	if s.Work, err = work.NewService(work.Config{Repository: s.WorkStore, Authority: s.Authority}); err != nil {
		t.Fatal(err)
	}
	bind := opt.PermissionBinder
	if bind == nil {
		bind = func(tx pgx.Tx) work.PermissionChecker { return grants{tx} }
	}
	if s.DepStore, err = deppg.NewStore(s.Runtime, s.Events, bind); err != nil {
		t.Fatal(err)
	}
	if s.Deps, err = dependencies.NewService(dependencies.Config{Repository: s.DepStore, Authority: s.Authority}); err != nil {
		t.Fatal(err)
	}

	s.SeedProject(TenantA, ProjectA, 5)
	s.SeedProject(TenantA, ProjectA2, 5)
	s.SeedProject(TenantB, ProjectB, 5)
	for _, tenant := range []string{TenantA, TenantB} {
		s.Principal(tenant, Engineer, work.ActorUser, EngineerPerms...)
		s.Principal(tenant, Reader, work.ActorUser, ReaderPerms...)
		s.Principal(tenant, Reviewer, work.ActorUser, ReviewerPerms...)
		s.Principal(tenant, Outsider, work.ActorUser)
	}
	return s
}

// Manifest is the full migration order: base, events, work, dependencies.
func Manifest() (storage.Manifest, error) {
	return storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workpg.Migrations(), deppg.Migrations())
}

// Migrate applies the manifest (reapplication is a no-op).
func (s *Stack) Migrate(ctx context.Context) storage.MigrateResult {
	s.T.Helper()
	m, err := Manifest()
	if err != nil {
		s.T.Fatal(err)
	}
	res, err := storage.Migrate(ctx, s.Owner, m, storage.MigrateOptions{RuntimeRole: s.DB.RuntimeRole})
	if err != nil {
		s.T.Fatalf("migrate: %v", err)
	}
	return res
}

// BindGuards is the work store's guard binder with the real dependency guard:
// the wiring T-046 performs in production.
func BindGuards(tx pgx.Tx) work.TxGuards {
	return work.TxGuards{
		Permissions:  grants{tx},
		Writers:      permissive{},
		Dependencies: deppg.NewDependencyGuard(tx),
		Subjects:     permissive{},
	}
}

func (s *Stack) fixtureTables() {
	rt := pgx.Identifier{s.DB.RuntimeRole}.Sanitize()
	for table, cols := range map[string]string{
		Prefix + "_principals": `tenant_id kodama_id NOT NULL, principal_id text NOT NULL, actor_type text NOT NULL, PRIMARY KEY (tenant_id, principal_id)`,
		Prefix + "_grants":     `tenant_id kodama_id NOT NULL, principal_id text NOT NULL, perm text NOT NULL, PRIMARY KEY (tenant_id, principal_id, perm)`,
	} {
		s.OwnerExec(fmt.Sprintf(`CREATE TABLE %s (%s)`, table, cols))
		s.OwnerExec(fmt.Sprintf(`ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY; ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;
			CREATE POLICY tenant_all ON %[1]s USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
			CREATE POLICY owner_all ON %[1]s TO CURRENT_USER USING (true) WITH CHECK (true);
			GRANT SELECT, INSERT, UPDATE, DELETE ON %[1]s TO %[2]s`, table, rt))
	}
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
	s.OwnerExec(`INSERT INTO `+Prefix+`_principals (tenant_id, principal_id, actor_type) VALUES ($1, $2, $3)`, tenant, id, string(typ))
	for _, p := range perms {
		s.Grant(tenant, id, p)
	}
}

// Grant adds one grant.
func (s *Stack) Grant(tenant, id string, perm work.Permission) {
	s.T.Helper()
	s.OwnerExec(`INSERT INTO `+Prefix+`_grants (tenant_id, principal_id, perm) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, tenant, id, string(perm))
}

// Revoke removes one grant.
func (s *Stack) Revoke(tenant, id string, perm work.Permission) {
	s.T.Helper()
	s.OwnerExec(`DELETE FROM `+Prefix+`_grants WHERE tenant_id = $1 AND principal_id = $2 AND perm = $3`, tenant, id, string(perm))
}

// ResolveActor implements work.ActorResolver.
func (a *Authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (work.Actor, error) {
	if a.Outage.Load() {
		return work.Actor{}, errors.New("identity authority unreachable")
	}
	var typ string
	err := storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_type FROM `+Prefix+`_principals WHERE tenant_id = $1 AND principal_id = $2`,
			string(scope.TenantID), principalID).Scan(&typ)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return work.Actor{}, work.ErrHidden
	}
	return work.Actor{ID: principalID, Type: work.ActorType(typ)}, err
}

// Authorize implements work.PermissionChecker.
func (a *Authority) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	if a.Outage.Load() {
		return errors.New("identity authority unreachable")
	}
	return storage.WithTenant(ctx, a.s.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return grants{tx}.Authorize(ctx, actor, scope, perm)
	})
}

// grants is the transaction-bound permission checker over the fixture table.
type grants struct{ tx pgx.Tx }

func (g grants) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	var one int
	err := g.tx.QueryRow(ctx, `SELECT 1 FROM `+Prefix+`_grants WHERE tenant_id = $1 AND principal_id = $2 AND perm = $3`,
		string(scope.TenantID), actor.ID, string(perm)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return work.ErrDenied
	}
	return err
}

// permissive stands in for the workspace and evidence modules: no writer is
// ever unsettled and every review claim is current.
type permissive struct{}

func (permissive) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, nil
}

func (permissive) VerifySubject(_ context.Context, _ domain.Scope, _ domain.WorkItemID, c work.ReviewClaim) (work.VerifiedSubject, error) {
	return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
}

func (permissive) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }

// Caller returns a caller for a principal.
func Caller(principal string) work.Caller {
	return work.Caller{PrincipalID: principal, RequestID: "req_" + Prefix}
}

// Create makes a story, task, defect or epic with a specification and one
// criterion, in backlog.
func (s *Stack) Create(scope domain.Scope, kind work.Kind, title string) work.Item {
	s.T.Helper()
	it, err := s.Work.Create(context.Background(), work.CreateCommand{
		Scope: scope, Caller: Caller(Engineer), Kind: kind, Title: title, Spec: "# Plan\nDo it.",
		Criteria: []work.Criterion{{ID: "AC-1", Text: "An exception is resolved with an audit record."}},
	})
	if err != nil {
		s.T.Fatalf("create: %v", err)
	}
	return it
}

// Get reads an item through the work service.
func (s *Stack) Get(it work.Item) work.Item {
	s.T.Helper()
	got, err := s.Work.Get(context.Background(), it.Scope, Caller(Engineer), it.ID)
	if err != nil {
		s.T.Fatalf("get: %v", err)
	}
	return got
}

// Move requests a transition with the principal; mutate adjusts the request.
func (s *Stack) Move(it work.Item, principal string, to work.State, mutate ...func(*work.TransitionRequest)) (work.Item, error) {
	req := work.TransitionRequest{To: to, Reason: "because"}
	for _, m := range mutate {
		m(&req)
	}
	return s.Work.Transition(context.Background(), work.TransitionCommand{
		Scope: it.Scope, Caller: Caller(principal), ID: it.ID, Expected: it.Version, Request: req,
	})
}

// MustMove is Move that fails the test on error.
func (s *Stack) MustMove(it work.Item, principal string, to work.State, mutate ...func(*work.TransitionRequest)) work.Item {
	s.T.Helper()
	out, err := s.Move(it, principal, to, mutate...)
	if err != nil {
		s.T.Fatalf("%s -> %s: %v", it.State, to, err)
	}
	return out
}

// Ready moves a backlog item to ready.
func (s *Stack) Ready(it work.Item) work.Item { return s.MustMove(it, Engineer, work.StateReady) }

// Activate moves a fresh item through ready to active.
func (s *Stack) Activate(it work.Item) work.Item {
	return s.MustMove(s.Ready(it), Engineer, work.StateActive)
}

// Review moves an active item to review.
func (s *Stack) Review(it work.Item) work.Item {
	return s.MustMove(it, Engineer, work.StateReview, func(r *work.TransitionRequest) {
		r.Review = &work.ReviewClaim{CandidateCommit: Commit, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_report"}}}
	})
}

// Complete moves a review item to done as the reviewer.
func (s *Stack) Complete(it work.Item) work.Item {
	return s.MustMove(it, Reviewer, work.StateDone, func(r *work.TransitionRequest) {
		r.Reason = "accepted"
		r.Completion = &work.CompletionClaim{ContentRevision: it.ContentRevision, CandidateCommit: Commit, AcceptedCriteria: []string{"AC-1"}}
	})
}

// Done creates an item and drives it to done.
func (s *Stack) Done(scope domain.Scope, title string) work.Item {
	s.T.Helper()
	return s.Complete(s.Review(s.Activate(s.Create(scope, work.KindStory, title))))
}

// Link adds Dependent -> Prerequisite as the engineer at the dependent's
// current version and returns the updated dependent.
func (s *Stack) Link(dependent, prerequisite work.Item) (dependencies.Outcome, error) {
	cur := s.Get(dependent)
	return s.Deps.Add(context.Background(), dependencies.AddCommand{
		Scope: dependent.Scope, Caller: Caller(Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: cur.Version,
	})
}

// MustLink is Link that fails the test on error.
func (s *Stack) MustLink(dependent, prerequisite work.Item) dependencies.Outcome {
	s.T.Helper()
	out, err := s.Link(dependent, prerequisite)
	if err != nil {
		s.T.Fatalf("link: %v", err)
	}
	return out
}

// Unlink removes Dependent -> Prerequisite as the engineer at the dependent's
// current version.
func (s *Stack) Unlink(dependent, prerequisite work.Item) (dependencies.Outcome, error) {
	cur := s.Get(dependent)
	return s.Deps.Remove(context.Background(), dependencies.RemoveCommand{
		Scope: dependent.Scope, Caller: Caller(Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: cur.Version,
	})
}

// Count runs a count query in a tenant transaction as the runtime role.
func (s *Stack) Count(tenant, sql string, args ...any) int {
	s.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), s.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		s.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Records counts the history, audit and outbox rows of one work item.
func (s *Stack) Records(it work.Item) (history, audit, outbox int) {
	s.T.Helper()
	tenant := string(it.Scope.TenantID)
	history = s.Count(tenant, `SELECT count(*) FROM work_item_history WHERE tenant_id = $1 AND work_item_id = $2`, tenant, string(it.ID))
	audit = s.Count(tenant, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'work_item' AND resource_id = $2`, tenant, string(it.ID))
	outbox = s.Count(tenant, `SELECT count(*) FROM outbox o JOIN event_observations b ON b.tenant_id = o.tenant_id AND b.event_id = o.event_id
		WHERE o.tenant_id = $1 AND b.resource_type = 'work_item' AND b.resource_id = $2`, tenant, string(it.ID))
	return
}

// Links counts the stored links of a tenant (all projects).
func (s *Stack) Links(tenant string) int {
	s.T.Helper()
	return s.Count(tenant, `SELECT count(*) FROM work_item_dependencies WHERE tenant_id = $1`, tenant)
}
