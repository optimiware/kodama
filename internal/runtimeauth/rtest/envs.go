package rtest

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// MemEnv is the runtime-authority store in memory: one memdb kernel shared by
// the grant store and the execution owner's attempt rows.
type MemEnv struct {
	DB       *memdb.Store
	Attempts *MemAttempts
	Grants   *MemStore
}

var _ Env = (*MemEnv)(nil)

// NewMemEnv wires the grant store and its attempt authority over a fresh kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	db := memdb.New(opts...)
	attempts := NewMemAttempts(db)
	grants, err := NewMemStore(db, attempts)
	if err != nil {
		t.Fatal(err)
	}
	return &MemEnv{DB: db, Attempts: attempts, Grants: grants}
}

// OpenMem is the Open of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

func (e *MemEnv) Store() runtimeauth.Store { return e.Grants }

func (e *MemEnv) SeedProject(testing.TB, domain.Scope) {}

func (e *MemEnv) SeedAttempt(t testing.TB, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, job runtimeauth.ExpectedJob, digest runtimeauth.Digest, actions runtimeauth.ActionSet) {
	t.Helper()
	if err := e.Attempts.Seed(context.Background(), scope, run, attempt, job, digest, actions); err != nil {
		t.Fatal(err)
	}
}

func (e *MemEnv) Narrow(t testing.TB, scope domain.Scope, attempt domain.AttemptID, revision int64, actions runtimeauth.ActionSet) {
	t.Helper()
	if err := e.Attempts.Modify(context.Background(), scope, attempt, func(s *runtimeauth.AttemptState) { s.Revision, s.Actions = revision, actions }); err != nil {
		t.Fatal(err)
	}
}

func (e *MemEnv) Cancel(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, revoke bool, now time.Time) error {
	return e.Attempts.Cancel(ctx, scope, attempt, revoke, now)
}

// PGEnv adapts the PostgreSQL stack to the contract's Env.
type PGEnv struct{ S *Stack }

var _ Env = PGEnv{}

// OpenPostgres is the Open of the PostgreSQL adapter: an isolated migrated
// database in the `test` environment. It skips without PostgreSQL, and always
// in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	return PGEnv{S: New(t, Options{})}
}

func (e PGEnv) Store() runtimeauth.Store { return e.S.Store }

func (e PGEnv) SeedProject(t testing.TB, scope domain.Scope) {
	t.Helper()
	e.S.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	e.S.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(scope.TenantID), string(scope.ProjectID))
}

func (e PGEnv) SeedAttempt(t testing.TB, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, job runtimeauth.ExpectedJob, digest runtimeauth.Digest, actions runtimeauth.ActionSet) {
	t.Helper()
	e.S.Authority.Seed(scope, run, attempt, job, digest, actions)
}

func (e PGEnv) Narrow(t testing.TB, scope domain.Scope, attempt domain.AttemptID, revision int64, actions runtimeauth.ActionSet) {
	t.Helper()
	e.S.Authority.Update(`revision = $1, actions = $2 WHERE tenant_id = $3 AND id = $4`, revision, actions.Strings(), string(scope.TenantID), string(attempt))
}

func (e PGEnv) Cancel(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, revoke bool, now time.Time) error {
	// The fixture revokes with its own (test) clock, like the module's service.
	return e.S.Authority.Cancel(ctx, scope, attempt, revoke)
}
