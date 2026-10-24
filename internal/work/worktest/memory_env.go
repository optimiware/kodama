package worktest

import (
	"context"
	"sync"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// MemEnv is the whole work module in memory: one memdb kernel shared by the
// work store, the event bus and the fixture authority, so every cross-module
// join takes the same Tx.
type MemEnv struct {
	DB     *memdb.Store
	Events *eventstest.Mem
	Auth   *MemAuthority
	Store  *MemStore
	Svc    *work.Service

	mu    sync.Mutex
	fault error
}

var _ Env = (*MemEnv)(nil)

// faultAppender fails every append after the event was written in the Tx.
type faultAppender struct {
	inner EventAppender
	env   *MemEnv
}

func (f faultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := f.inner.Append(tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewMemEnv wires the work module over a fresh memdb kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	e := &MemEnv{DB: memdb.New(opts...)}
	var err error
	if e.Events, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	e.Auth = NewMemAuthority(e.DB)
	if e.Store, err = NewMemStore(e.DB, faultAppender{inner: e.Events, env: e}, e.Auth.Guards); err != nil {
		t.Fatal(err)
	}
	if e.Svc, err = work.NewService(work.Config{Repository: e.Store, Authority: e.Auth}); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenMem is the Open of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

func (e *MemEnv) Service() *work.Service                 { return e.Svc }
func (e *MemEnv) Repository() work.Repository            { return e.Store }
func (e *MemEnv) Cancellations() work.CancellationSource { return e.Store }

func (e *MemEnv) SeedProject(t testing.TB, scope domain.Scope, wip int) {
	t.Helper()
	if err := e.Store.SeedProject(context.Background(), scope, wip); err != nil {
		t.Fatal(err)
	}
	e.Events.AddTenant(scope.TenantID)
}

func (e *MemEnv) Principal(t testing.TB, tenant domain.TenantID, id string, typ work.ActorType, perms ...work.Permission) {
	t.Helper()
	if err := e.Auth.Principal(context.Background(), tenant, id, typ, perms...); err != nil {
		t.Fatal(err)
	}
}

func (e *MemEnv) Revoke(t testing.TB, tenant domain.TenantID, id string, perm work.Permission) {
	t.Helper()
	if err := e.Auth.Revoke(context.Background(), tenant, id, perm); err != nil {
		t.Fatal(err)
	}
}

func (e *MemEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

func (e *MemEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	ids, err := e.Events.EventIDs(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *MemEnv) Admit(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (workpg.Admission, error) {
	var a workpg.Admission
	err := e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		a, err = LockActionable(tx, scope, id)
		return err
	})
	return a, err
}
