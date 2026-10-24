package workspacetest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// MemStoreEnv is the workspace persistence in memory: one memdb kernel shared
// by the workspace store and the event bus.
type MemStoreEnv struct {
	DB     *memdb.Store
	Events *eventstest.Mem
	Mem    *MemStore

	mu    sync.Mutex
	fault error
}

var _ StoreEnv = (*MemStoreEnv)(nil)

type memFaultAppender struct{ env *MemStoreEnv }

func (f memFaultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := f.env.Events.Append(tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewMemStoreEnv wires the memory workspace store over a fresh kernel.
func NewMemStoreEnv(t testing.TB, opts ...memdb.Option) *MemStoreEnv {
	t.Helper()
	e := &MemStoreEnv{DB: memdb.New(opts...)}
	var err error
	if e.Events, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	if e.Mem, err = NewMemStore(e.DB, memFaultAppender{env: e}, MemOptions{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenMemStore is the OpenStore of the memory adapter.
func OpenMemStore(t testing.TB) StoreEnv { return NewMemStoreEnv(t) }

func (e *MemStoreEnv) Store() workspace.Store { return e.Mem }

func (e *MemStoreEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	ids, err := e.Events.EventIDs(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *MemStoreEnv) AuditActions(t testing.TB, scope domain.Scope, id workspace.ID) []string {
	t.Helper()
	out, err := e.Mem.AuditActions(context.Background(), scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *MemStoreEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

// PGStoreEnv adapts the PostgreSQL environment to the contract's StoreEnv, with
// an event appender that can fail after the real append.
type PGStoreEnv struct {
	E      *Env
	Store_ *workspacepg.Store

	mu    sync.Mutex
	fault error
}

var _ StoreEnv = (*PGStoreEnv)(nil)

type pgFaultAppender struct {
	inner *eventspg.Store
	env   *PGStoreEnv
}

func (f pgFaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	res, err := f.inner.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// OpenPostgresStore is the OpenStore of the PostgreSQL adapter: the fixture
// database with the fixture tenants. It skips without PostgreSQL, and always in
// the unit layer.
func OpenPostgresStore(t testing.TB) StoreEnv {
	t.Helper()
	e := New(t, Config{})
	p := &PGStoreEnv{E: e}
	var err error
	if p.Store_, err = workspacepg.NewStore(e.Runtime, pgFaultAppender{inner: e.Events, env: p}, workspacepg.Options{}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *PGStoreEnv) Store() workspace.Store { return e.Store_ }

// column reads one text column of the tenant's rows in the tenant's context: the
// outbox and audit policies are tenant-only.
func (e *PGStoreEnv) column(t testing.TB, tenant domain.TenantID, sql string, args ...any) []string {
	t.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), e.E.Migration, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *PGStoreEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	return e.column(t, tenant, `SELECT event_id FROM outbox WHERE tenant_id = $1 ORDER BY event_id`, string(tenant))
}

func (e *PGStoreEnv) AuditActions(t testing.TB, scope domain.Scope, id workspace.ID) []string {
	t.Helper()
	out := e.column(t, scope.TenantID, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type = 'workspace' AND resource_id = $2`, string(scope.TenantID), string(id))
	slices.Sort(out)
	return out
}

func (e *PGStoreEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}
