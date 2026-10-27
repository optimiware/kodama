package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// Resource prefix of this task's test fixtures (databases are provisioned by the
// shared helper with its own random names; fixture tables carry the prefix).
const prefix = "t011"

const (
	tenantA, projectA = "t_acme", "p_inventory"
	tenantB, projectB = "t_globex", "p_billing"
)

var (
	scopeA = domain.Scope{TenantID: tenantA, ProjectID: projectA}
	scopeB = domain.Scope{TenantID: tenantB, ProjectID: projectB}
)

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
)

// Principals of the fixture authority; roles are grants in t011_grants.
const (
	analyst  = "u_analyst"
	engineer = "u_engineer"
	reviewer = "u_reviewer"
	admin    = "u_admin"
	pipeline = "svc_pipeline"
)

var (
	analystPerms  = []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermRefine}
	engineerPerms = []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermCancel, work.PermReopen}
	reviewerPerms = []work.Permission{work.PermRead, work.PermComplete, work.PermReopen, work.PermCancel}
	adminPerms    = []work.Permission{work.PermRead, work.PermCreate, work.PermCancel} // admin is not a reviewer
)

type env struct {
	t       testing.TB
	db      *pgtest.Database
	owner   *pgxpool.Pool
	rt      *pgxpool.Pool
	events  *eventspg.Store
	store   *workpg.Store
	service *work.Service
	clock   *eventstest.Clock
}

type envOptions struct {
	bindings []eventspg.Binding
	mutate   func(*eventspg.Options)
	conns    int32
}

func newEnv(t testing.TB) *env { return newEnvWith(t, envOptions{}) }

func newEnvWith(t testing.TB, o envOptions) *env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conns := o.conns
	if conns == 0 {
		conns = 24
	}
	e := &env{t: t, db: db, owner: db.MigrationPool(ctx, 4), rt: db.RuntimePool(ctx, conns), clock: &eventstest.Clock{}}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e.fixtureTables()
	opts := eventstest.DefaultOptions()
	opts.Clock = e.clock.Now
	if o.mutate != nil {
		o.mutate(&opts)
	}
	e.events, err = eventspg.NewStore(e.rt, opts, o.bindings...)
	if err != nil {
		t.Fatal(err)
	}
	e.store, err = workpg.NewStore(e.rt, e.events, e.bindGuards)
	if err != nil {
		t.Fatal(err)
	}
	e.service, err = work.NewService(work.Config{Repository: e.store, Authority: dbAuthority{e}})
	if err != nil {
		t.Fatal(err)
	}
	e.seedTenant(tenantA, projectA, 5)
	e.seedTenant(tenantB, projectB, 5)
	for _, tenant := range []string{tenantA, tenantB} {
		e.principal(tenant, analyst, work.ActorUser, analystPerms...)
		e.principal(tenant, engineer, work.ActorUser, engineerPerms...)
		e.principal(tenant, reviewer, work.ActorUser, reviewerPerms...)
		e.principal(tenant, admin, work.ActorUser, adminPerms...)
		e.principal(tenant, pipeline, work.ActorService, append(append([]work.Permission{}, engineerPerms...), work.PermComplete)...)
	}
	return e
}

func (e *env) ownerExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// fixtureTables are the stand-ins for the identity, workspace and evidence
// modules, which this task must not implement: they follow the module table
// conventions (forced RLS on kodama_tenant_id, minimal grants).
func (e *env) fixtureTables() {
	rt := pgx.Identifier{e.db.RuntimeRole}.Sanitize()
	for _, ddl := range []string{
		`CREATE TABLE t011_principals (tenant_id kodama_id NOT NULL, principal_id text NOT NULL, actor_type text NOT NULL, PRIMARY KEY (tenant_id, principal_id))`,
		`CREATE TABLE t011_grants (tenant_id kodama_id NOT NULL, principal_id text NOT NULL, perm text NOT NULL, PRIMARY KEY (tenant_id, principal_id, perm))`,
		`CREATE TABLE t011_writers (tenant_id kodama_id NOT NULL, work_item_id kodama_id NOT NULL, attempt_id text NOT NULL, unsettled boolean NOT NULL, PRIMARY KEY (tenant_id, work_item_id, attempt_id))`,
		`CREATE TABLE t011_workspaces (tenant_id kodama_id NOT NULL, project_id kodama_id NOT NULL, work_item_id kodama_id NOT NULL, accepted_commit text, PRIMARY KEY (tenant_id, work_item_id))`,
		`CREATE TABLE t011_artifacts (tenant_id kodama_id NOT NULL, project_id kodama_id NOT NULL, id kodama_id NOT NULL, PRIMARY KEY (tenant_id, id))`,
		`CREATE TABLE t011_findings (tenant_id kodama_id NOT NULL, work_item_id kodama_id NOT NULL, id text NOT NULL, summary text NOT NULL, PRIMARY KEY (tenant_id, work_item_id, id))`,
		`CREATE TABLE t011_cancel_requests (tenant_id kodama_id NOT NULL, project_id kodama_id NOT NULL, work_item_id kodama_id NOT NULL, version bigint NOT NULL, requests int NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id, work_item_id))`,
		`CREATE TABLE t011_reservations (tenant_id kodama_id NOT NULL, work_item_id kodama_id NOT NULL, owner text NOT NULL, PRIMARY KEY (tenant_id, work_item_id))`,
	} {
		e.ownerExec(ddl)
		var table string
		for i := len("CREATE TABLE "); ddl[i] != ' '; i++ {
			table += string(ddl[i])
		}
		e.ownerExec(fmt.Sprintf(`ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY; ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;
			CREATE POLICY tenant_all ON %[1]s USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
			CREATE POLICY owner_all ON %[1]s TO CURRENT_USER USING (true) WITH CHECK (true);
			GRANT SELECT, INSERT, UPDATE, DELETE ON %[1]s TO %[2]s`, table, rt))
	}
}

func (e *env) seedTenant(tenant, project string, wip int) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, tenant)
	e.ownerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, $3)`, tenant, project, wip)
}

func (e *env) principal(tenant, id string, typ work.ActorType, perms ...work.Permission) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO t011_principals (tenant_id, principal_id, actor_type) VALUES ($1, $2, $3)`, tenant, id, string(typ))
	for _, p := range perms {
		e.ownerExec(`INSERT INTO t011_grants (tenant_id, principal_id, perm) VALUES ($1, $2, $3)`, tenant, id, string(p))
	}
}

func (e *env) revoke(tenant, principal string, perm work.Permission) {
	e.t.Helper()
	e.ownerExec(`DELETE FROM t011_grants WHERE tenant_id = $1 AND principal_id = $2 AND perm = $3`, tenant, principal, string(perm))
}

func (e *env) setActorType(tenant, principal string, typ work.ActorType) {
	e.t.Helper()
	e.ownerExec(`UPDATE t011_principals SET actor_type = $3 WHERE tenant_id = $1 AND principal_id = $2`, tenant, principal, string(typ))
}

// dbAuthority is the pre-transaction identity authority over the fixture tables.
type dbAuthority struct{ e *env }

func (a dbAuthority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (work.Actor, error) {
	var typ string
	err := storage.WithTenant(ctx, a.e.rt, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_type FROM t011_principals WHERE tenant_id = $1 AND principal_id = $2`, string(scope.TenantID), principalID).Scan(&typ)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return work.Actor{}, work.ErrHidden
	}
	return work.Actor{ID: principalID, Type: work.ActorType(typ)}, err
}

func (a dbAuthority) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	return storage.WithTenant(ctx, a.e.rt, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return txGuards{tx}.Authorize(ctx, actor, scope, perm)
	})
}

// txGuards is the transaction-bound fixture implementation of every guard.
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

func (g txGuards) WriterSettled(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (bool, error) {
	var n int
	err := g.tx.QueryRow(ctx, `SELECT count(*) FROM t011_writers WHERE tenant_id = $1 AND work_item_id = $2 AND unsettled`, string(scope.TenantID), string(id)).Scan(&n)
	return n == 0, err
}

func (g txGuards) VerifySubject(ctx context.Context, scope domain.Scope, id domain.WorkItemID, claim work.ReviewClaim) (work.VerifiedSubject, error) {
	var accepted *string
	err := g.tx.QueryRow(ctx, `SELECT accepted_commit FROM t011_workspaces WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&accepted)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && accepted == nil) {
		return work.VerifiedSubject{}, fmt.Errorf("%w: no accepted commit", work.ErrNotFound)
	} else if err != nil {
		return work.VerifiedSubject{}, err
	}
	if *accepted != claim.CandidateCommit {
		return work.VerifiedSubject{}, fmt.Errorf("%w: the workspace moved past the candidate", work.ErrStaleSubject)
	}
	if err := g.VerifyEvidence(ctx, scope, claim.Evidence); err != nil {
		return work.VerifiedSubject{}, err
	}
	rows, err := g.tx.Query(ctx, `SELECT id, summary FROM t011_findings WHERE tenant_id = $1 AND work_item_id = $2 ORDER BY id`, string(scope.TenantID), string(id))
	if err != nil {
		return work.VerifiedSubject{}, err
	}
	defer rows.Close()
	v := work.VerifiedSubject{CandidateCommit: claim.CandidateCommit, Evidence: claim.Evidence}
	for rows.Next() {
		var f work.Finding
		if err := rows.Scan(&f.ID, &f.Summary); err != nil {
			return work.VerifiedSubject{}, err
		}
		v.Findings = append(v.Findings, f)
	}
	return v, rows.Err()
}

func (g txGuards) VerifyEvidence(ctx context.Context, scope domain.Scope, refs []work.EvidenceRef) error {
	for _, r := range refs {
		if r.Type != work.EvidenceArtifact {
			continue
		}
		var one int
		err := g.tx.QueryRow(ctx, `SELECT 1 FROM t011_artifacts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), r.ID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: evidence not found", work.ErrNotFound)
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (e *env) bindGuards(tx pgx.Tx) work.TxGuards {
	g := txGuards{tx}
	return work.TxGuards{Permissions: g, Writers: writers{g}, Dependencies: work.NoDependencyGraph{}, Subjects: g}
}

type writers struct{ g txGuards }

func (w writers) WriterSettled(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (bool, error) {
	return w.g.WriterSettled(ctx, scope, id)
}

func (e *env) artifact(scope domain.Scope, id string) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO t011_artifacts (tenant_id, project_id, id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, string(scope.TenantID), string(scope.ProjectID), id)
}

func (e *env) acceptCommit(scope domain.Scope, id domain.WorkItemID, commit string) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO t011_workspaces (tenant_id, project_id, work_item_id, accepted_commit) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, work_item_id) DO UPDATE SET accepted_commit = EXCLUDED.accepted_commit`, string(scope.TenantID), string(scope.ProjectID), string(id), commit)
}

func (e *env) caller(principal string) work.Caller {
	return work.Caller{PrincipalID: principal, RequestID: "req_" + prefix}
}

// create makes a story with a specification and one criterion.
func (e *env) create(scope domain.Scope, kind work.Kind, title string) work.Item {
	e.t.Helper()
	it, err := e.service.Create(context.Background(), work.CreateCommand{
		Scope: scope, Caller: e.caller(engineer), Kind: kind, Title: title, Spec: "# Plan\nDo it.",
		Criteria: []work.Criterion{{ID: "AC-1", Text: "An exception is resolved with an audit record."}},
	})
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return it
}

func (e *env) move(it work.Item, principal string, to work.State, mutate ...func(*work.TransitionRequest)) (work.Item, error) {
	req := work.TransitionRequest{To: to, Reason: "because"}
	for _, m := range mutate {
		m(&req)
	}
	return e.service.Transition(context.Background(), work.TransitionCommand{
		Scope: it.Scope, Caller: e.caller(principal), ID: it.ID, Expected: it.Version, Request: req,
	})
}

func (e *env) mustMove(it work.Item, principal string, to work.State, mutate ...func(*work.TransitionRequest)) work.Item {
	e.t.Helper()
	out, err := e.move(it, principal, to, mutate...)
	if err != nil {
		e.t.Fatalf("%s -> %s: %v", it.State, to, err)
	}
	return out
}

// activate moves a new item to active.
func (e *env) activate(scope domain.Scope, title string) work.Item {
	e.t.Helper()
	it := e.create(scope, work.KindStory, title)
	it = e.mustMove(it, engineer, work.StateReady)
	return e.mustMove(it, engineer, work.StateActive)
}

func (e *env) toReview(it work.Item) work.Item {
	e.t.Helper()
	e.artifact(it.Scope, "art_report")
	e.acceptCommit(it.Scope, it.ID, commitA)
	return e.mustMove(it, engineer, work.StateReview, func(r *work.TransitionRequest) {
		r.Review = &work.ReviewClaim{CandidateCommit: commitA, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_report"}}}
	})
}

func completion(it work.Item) func(*work.TransitionRequest) {
	return func(r *work.TransitionRequest) {
		r.Reason = "accepted"
		r.Completion = &work.CompletionClaim{ContentRevision: it.ContentRevision, CandidateCommit: commitA, AcceptedCriteria: []string{"AC-1"}}
	}
}

func (e *env) count(tenant, sql string, args ...any) int {
	e.t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.rt, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// records counts the history, audit and outbox rows of one work item.
func (e *env) records(it work.Item) (history, audit, outbox int) {
	e.t.Helper()
	tenant := string(it.Scope.TenantID)
	history = e.count(tenant, `SELECT count(*) FROM work_item_history WHERE tenant_id = $1 AND work_item_id = $2`, tenant, string(it.ID))
	audit = e.count(tenant, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'work_item' AND resource_id = $2`, tenant, string(it.ID))
	outbox = e.count(tenant, `SELECT count(*) FROM outbox o JOIN event_observations b ON b.tenant_id = o.tenant_id AND b.event_id = o.event_id
		WHERE o.tenant_id = $1 AND b.resource_type = 'work_item' AND b.resource_id = $2`, tenant, string(it.ID))
	return
}

// parallel runs fns concurrently, releasing them together, and returns their errors.
func parallel(fns ...func() error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}
