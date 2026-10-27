package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/identity"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

func TestMain(m *testing.M) {
	// Resource prefix of this task's test resources.
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t100")
	}
	os.Exit(m.Run())
}

// Contributions is the manifest the environment migrates: the execution
// environment's own, then this module after everything it references.
func contributions() []storage.Contribution {
	return []storage.Contribution{storage.BaseContribution(), execpg.RunResourceMigrations()}
}

// env is the real execution module over PostgreSQL with this module's schema
// applied on top, so run creation is the core's own admission.
type env struct {
	t     testing.TB
	x     *executiontest.Env
	store *gwpg.Store
	led   *gwpg.Ledger
	defs  map[domain.Scope]pipeline.Definition
	seq   atomic.Int64
}

func newEnv(t testing.TB) *env {
	t.Helper()
	x := executiontest.New(t)
	x.GrantAll()
	manifest, err := storage.NewManifest(contributions()...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := &env{t: t, x: x, defs: map[domain.Scope]pipeline.Definition{}}
	if e.store, err = gwpg.NewStore(x.Runtime, gwpg.Options{}); err != nil {
		t.Fatal(err)
	}
	if e.led, err = gwpg.NewLedger(x.Runtime, x.Store); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) id(prefix string) string { return fmt.Sprintf("%s_t100_%d", prefix, e.seq.Add(1)) }

// owner runs SQL as the owner role (seeding and inspection).
func (e *env) owner(sql string, args ...any) {
	e.t.Helper()
	e.x.OwnerExec(sql, args...)
}

func (e *env) count(sql string, args ...any) int { e.t.Helper(); return e.x.Count(sql, args...) }

// def returns the scope's registered pipeline definition.
func (e *env) def(scope domain.Scope) pipeline.Definition {
	e.t.Helper()
	if d, ok := e.defs[scope]; ok {
		return d
	}
	d := e.x.Define(scope, pipelinetest.Doc(pipelinetest.Container("job", "workspace: {access: read, from: run}")))
	e.defs[scope] = d
	return d
}

// regrant gives the engineer the run authority in the scope's tenant afresh.
func (e *env) regrant(scope domain.Scope) {
	e.t.Helper()
	e.owner(`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, string(scope.TenantID), executiontest.Engineer)
	e.x.Grant(scope, executiontest.Engineer, identity.RoleEngineer)
}

// trigger returns a valid trigger of the scope referencing its registered
// definition; with item it also names the work item.
func (e *env) trigger(scope domain.Scope, item domain.WorkItemID) gw.Trigger {
	e.t.Helper()
	return gw.Trigger{
		ID: gw.TriggerID(e.id("trg")), Scope: scope, RepositoryID: "repo-main", DefinitionID: e.def(scope).ID, WorkItemID: item,
		Branches: []string{"main", "release/*"}, SecretRef: "secret://project/hook", InstalledBy: executiontest.Engineer, Enabled: true, Version: 1,
		CreatedAt: clock, UpdatedAt: clock,
	}
}

// plan prepares a real admission plan for a new work item of the scope, as the
// trigger's planner does through the core's admission use case.
func (e *env) plan(scope domain.Scope) *execution.AdmissionPlan {
	e.t.Helper()
	e.regrant(scope)
	p := e.x.NewProject(scope)
	plan, err := e.x.Svc.Prepare(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t100"}, execution.AdmitCommand{
		Scope: scope, WorkItemID: p.Item, DefinitionID: e.def(scope).ID, ContentRevision: e.x.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
	if err != nil {
		e.t.Fatalf("prepare: %v", err)
	}
	return plan
}

// newRun creates a run of the scope through the core's admission store.
func (e *env) newRun(scope domain.Scope) domain.RunID {
	e.t.Helper()
	run, _, err := e.x.Store.Admit(context.Background(), e.plan(scope))
	if err != nil {
		e.t.Fatalf("admit: %v", err)
	}
	return run.ID
}

func (e *env) runs(scope domain.Scope) int {
	return e.count(`SELECT count(*) FROM runs WHERE tenant_id = $1 AND project_id = $2`, string(scope.TenantID), string(scope.ProjectID))
}

func (e *env) audits(scope domain.Scope, trigger gw.TriggerID) int {
	return e.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'git_trigger' AND resource_id = $2`, string(scope.TenantID), string(trigger))
}

// restartedPool is a new runtime connection pool, as after a process restart.
func (e *env) restartedPool() *pgxpool.Pool {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.x.Base.DB.RuntimePool(ctx, 16)
}

// storeSubject adapts the environment to the Store contract.
func (e *env) storeSubject() storeSubject {
	return storeSubject{
		Store: e.store,
		Restart: func() gw.Store {
			s, err := gwpg.NewStore(e.restartedPool(), gwpg.Options{})
			if err != nil {
				e.t.Fatal(err)
			}
			return s
		},
		ScopeA: executiontest.ScopeA, ScopeB: executiontest.ScopeB,
		NewTrigger: func(_ testing.TB, scope domain.Scope) gw.Trigger { return e.trigger(scope, "") },
		NewRun:     func(_ testing.TB, scope domain.Scope) domain.RunID { return e.newRun(scope) },
		Audits:     func(_ testing.TB, scope domain.Scope, id gw.TriggerID) int { return e.audits(scope, id) },
	}
}

// inTenant runs fn in a runtime-role transaction scoped to the tenant.
func (e *env) inTenant(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return e.x.RuntimeTx(tenant, fn)
}

// lostCommit wraps a TxBeginner so the next commit lands but its
// acknowledgement is lost.
type lostCommit struct {
	inner storage.TxBeginner
	armed atomic.Bool
}

var errLost = errors.New("connection reset while committing")

func (l *lostCommit) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := l.inner.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &lostTx{Tx: tx, l: l}, nil
}

type lostTx struct {
	pgx.Tx
	l *lostCommit
}

func (t *lostTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.l.armed.CompareAndSwap(true, false) {
		return errLost
	}
	return nil
}
