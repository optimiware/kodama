package idtest

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
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/storage"
)

// failingEvents makes an event append fail after the event was written in the
// mutation's transaction, as a bus that breaks late would.
type failingEvents struct {
	mu  sync.Mutex
	err error
}

func (f *failingEvents) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *failingEvents) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

type memFaultAppender struct {
	inner EventAppender
	fail  *failingEvents
}

func (a memFaultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := a.inner.Append(tx, env)
	if err != nil {
		return res, err
	}
	return res, a.fail.get()
}

type pgFaultAppender struct {
	inner identitypg.EventAppender
	fail  *failingEvents
}

func (a pgFaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	res, err := a.inner.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	return res, a.fail.get()
}

// memEnv adapts Mem, over a kernel shared with the in-memory event bus, to the
// contract's Env.
type memEnv struct {
	m      *Mem
	events *eventstest.Mem
	fail   *failingEvents
}

var _ Env = memEnv{}

// OpenMem is the Open of the memory adapter: Mem, which holds the semantics the
// PostgreSQL adapter is held to, writing its events to the in-memory bus in the
// same transaction.
func OpenMem(t testing.TB) Env {
	t.Helper()
	kernel := memdb.New()
	bus, err := eventstest.NewMem(kernel, eventstest.DefaultMemOptions())
	if err != nil {
		t.Fatal(err)
	}
	fail := &failingEvents{}
	m, err := NewMemOn(kernel, memFaultAppender{inner: bus, fail: fail})
	if err != nil {
		t.Fatal(err)
	}
	return memEnv{m: m, events: bus, fail: fail}
}

func (e memEnv) Repository() identity.Repository  { return e.m }
func (e memEnv) Directory() identity.Directory    { return e.m }
func (e memEnv) Mapper() identity.PrincipalMapper { return e.m }
func (e memEnv) Access() identity.AccessReader    { return e.m }

func (e memEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	ids, err := e.events.EventIDs(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

func (e memEnv) AuditActions(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	out, err := e.m.AuditActions(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e memEnv) FailEvents(err error) { e.fail.set(err) }

func (e memEnv) SeedProject(t testing.TB, tenant domain.TenantID, project domain.ProjectID) {
	t.Helper()
	e.events.AddTenant(tenant)
	e.m.AddTenant(tenant, string(tenant))
	e.m.AddProject(tenant, project, string(project))
}

func (e memEnv) Bind(t testing.TB, tenant domain.TenantID, user identity.UserID, project domain.ProjectID, role identity.Role) identity.BindingID {
	t.Helper()
	id := identity.BindingID("mb_" + RandomHex(t, 8))
	e.m.AddBinding(tenant, id, user, project, role)
	return id
}

func (e memEnv) Revision(_ testing.TB, tenant domain.TenantID, user identity.UserID) int64 {
	return e.m.Revision(tenant, user)
}

// pgEnv adapts the PostgreSQL stack to the contract's Env. Its store appends
// events through a wrapper that can fail after the append.
type pgEnv struct {
	s     *Stack
	store *identitypg.Store
	fail  *failingEvents
}

var _ Env = pgEnv{}

// OpenPostgres is the Open of the PostgreSQL adapter: an isolated migrated
// database in the `test` environment. It skips without PostgreSQL, and always
// in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	s := New(t, Options{})
	fail := &failingEvents{}
	store, err := identitypg.NewStore(s.Runtime, pgFaultAppender{inner: s.Events, fail: fail})
	if err != nil {
		t.Fatal(err)
	}
	return pgEnv{s: s, store: store, fail: fail}
}

func (e pgEnv) Repository() identity.Repository  { return e.store }
func (e pgEnv) Directory() identity.Directory    { return e.store }
func (e pgEnv) Mapper() identity.PrincipalMapper { return e.store }
func (e pgEnv) Access() identity.AccessReader    { return e.store }

func (e pgEnv) FailEvents(err error) { e.fail.set(err) }

// column reads one text column of the tenant's rows in the tenant's context: the
// outbox and audit policies are tenant-only.
func (e pgEnv) column(t testing.TB, tenant domain.TenantID, sql string) []string {
	t.Helper()
	var out []string
	err := storage.WithTenant(context.Background(), e.s.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, string(tenant))
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
	slices.Sort(out)
	return out
}

func (e pgEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	return e.column(t, tenant, `SELECT event_id FROM outbox WHERE tenant_id = $1`)
}

func (e pgEnv) AuditActions(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	return e.column(t, tenant, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type IN ('membership', 'project')`)
}

func (e pgEnv) SeedProject(t testing.TB, tenant domain.TenantID, project domain.ProjectID) {
	t.Helper()
	e.s.SeedProject(tenant, project, 5)
}

func (e pgEnv) Bind(t testing.TB, tenant domain.TenantID, user identity.UserID, project domain.ProjectID, role identity.Role) identity.BindingID {
	t.Helper()
	return e.s.Bind(tenant, user, project, role)
}

func (e pgEnv) Revision(t testing.TB, tenant domain.TenantID, user identity.UserID) int64 {
	t.Helper()
	return e.s.Revision(tenant, user)
}
