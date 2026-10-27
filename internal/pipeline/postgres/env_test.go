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
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Resource prefix of this task's test fixtures (databases are provisioned by the
// shared helper with its own random names).
const prefix = "t014"

const (
	tenantA, projectA = "t_acme", "p_inventory"
	projectA2         = "p_ops"
	tenantB, projectB = "t_globex", "p_billing"
)

var (
	scopeA  = domain.Scope{TenantID: tenantA, ProjectID: projectA}
	scopeA2 = domain.Scope{TenantID: tenantA, ProjectID: projectA2}
	scopeB  = domain.Scope{TenantID: tenantB, ProjectID: projectB}
)

const (
	engineer = "u_engineer"
	pipeSvc  = "svc_pipeline"
)

type env struct {
	t     testing.TB
	db    *pgtest.Database
	owner *pgxpool.Pool
	rt    *pgxpool.Pool
	store *pipelinepg.Store
	auth  *pipelinetest.Authority
	svc   *pipeline.Service
	v     *pipeline.Validator
	mu    sync.Mutex // guards clock and n, which concurrent registrations share
	clock time.Time
	n     int
}

type envOptions struct {
	conns int32
	store pipelinepg.Options
	// wrap lets a test interpose on the runtime database handle.
	wrap func(storage.TxBeginner) storage.TxBeginner
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
	e := &env{t: t, db: db, owner: db.MigrationPool(ctx, 4), rt: db.RuntimePool(ctx, conns), clock: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var handle storage.TxBeginner = e.rt
	if o.wrap != nil {
		handle = o.wrap(handle)
	}
	if e.store, err = pipelinepg.NewStore(handle, o.store); err != nil {
		t.Fatal(err)
	}
	e.v = pipelinetest.NewValidator(t)
	e.auth = pipelinetest.NewAuthority()
	e.svc, err = pipeline.NewService(pipeline.ServiceConfig{
		Repository: e.store, Authority: e.auth, Validator: e.v,
		Clock: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.clock = e.clock.Add(time.Second)
			return e.clock
		},
		NewID: func() (string, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.n++
			return fmt.Sprintf("pd_%s_%04d", prefix, e.n), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.seed(tenantA, projectA, projectA2)
	e.seed(tenantB, projectB)
	for _, s := range []domain.Scope{scopeA, scopeA2, scopeB} {
		e.auth.Grant(s, engineer, pipeline.ActorUser, pipeline.PermList, pipeline.PermRegister, pipeline.PermRead)
		e.auth.Grant(s, pipeSvc, pipeline.ActorService, pipeline.PermRegister, pipeline.PermRead)
	}
	return e
}

func (e *env) ownerExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

func (e *env) seed(tenant string, projects ...string) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, tenant)
	for _, p := range projects {
		e.ownerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, tenant, p)
	}
}

func (e *env) register(scope domain.Scope, principal, src string) (pipeline.RegisterResult, error) {
	return e.svc.Register(context.Background(), pipeline.RegisterCommand{
		Scope: scope, Caller: pipeline.Caller{PrincipalID: principal, RequestID: "req_" + prefix}, Format: pipeline.FormatYAML, Body: []byte(src),
	})
}

func (e *env) mustRegister(scope domain.Scope, principal, src string) pipeline.RegisterResult {
	e.t.Helper()
	r, err := e.register(scope, principal, src)
	if err != nil {
		e.t.Fatalf("register: %v", err)
	}
	return r
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

func (e *env) definitions(scope domain.Scope) int {
	return e.count(`SELECT count(*) FROM pipeline_definitions WHERE tenant_id = $1 AND project_id = $2`, string(scope.TenantID), string(scope.ProjectID))
}

func (e *env) audits(scope domain.Scope) int {
	return e.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND project_id = $2 AND resource_type = 'pipeline_definition'`, string(scope.TenantID), string(scope.ProjectID))
}

// withTenant runs fn in a runtime-role transaction scoped to tenant.
func (e *env) withTenant(tenant string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.rt, domain.TenantID(tenant), fn)
}

// commitMode controls what a wrapped transaction's Commit does.
type commitMode int

const (
	commitNormal     commitMode = iota
	commitLostReply             // the commit happens, the caller sees an error
	commitNeverLands            // the commit is rolled back, the caller sees an error
)

// flakyDB wraps a TxBeginner so a test can lose a commit's acknowledgement.
type flakyDB struct {
	inner storage.TxBeginner
	mode  commitMode
}

func (f *flakyDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := f.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &flakyTx{Tx: tx, mode: &f.mode}, nil
}

type flakyTx struct {
	pgx.Tx
	mode *commitMode
}

var errLostCommit = errors.New("connection reset while committing")

func (t *flakyTx) Commit(ctx context.Context) error {
	switch *t.mode {
	case commitLostReply:
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return errLostCommit
	case commitNeverLands:
		_ = t.Tx.Rollback(ctx)
		return errLostCommit
	}
	return t.Tx.Commit(ctx)
}
