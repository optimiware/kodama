package secretstest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// MemEnv is the secrets module in memory: one memdb kernel shared by the secrets
// store and the event bus, with the ephemeral key wrapper.
type MemEnv struct {
	DB     *memdb.Store
	Events *eventstest.Mem
	Store_ *MemStore
	Wrap   *EphemeralWrapper
	mu     sync.Mutex
	fault  error
}

var _ Env = (*MemEnv)(nil)

type memFaultAppender struct{ env *MemEnv }

func (f memFaultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := f.env.Events.Append(tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewMemEnv wires the memory secrets store over a fresh kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	e := &MemEnv{DB: memdb.New(opts...)}
	var err error
	if e.Events, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	if e.Wrap, err = NewEphemeralWrapper(); err != nil {
		t.Fatal(err)
	}
	if e.Store_, err = NewMemStore(MemConfig{Store: e.DB, Events: memFaultAppender{env: e}, IDs: secrets.RandomIDs{}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenMem is the Open of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

func (e *MemEnv) Store() secrets.Store               { return e.Store_ }
func (e *MemEnv) Variables() secrets.VariableCatalog { return e.Store_ }
func (e *MemEnv) Secrets() secrets.SecretCatalog     { return e.Store_ }
func (e *MemEnv) Wrapper() secrets.KeyWrapper        { return e.Wrap }

func (e *MemEnv) Service(t testing.TB, w secrets.KeyWrapper) *secrets.Service {
	t.Helper()
	svc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: Installation, Store: e.Store_, Wrapper: w, IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (e *MemEnv) Source(t testing.TB, w secrets.KeyWrapper) secrets.VersionSource {
	t.Helper()
	src, err := NewMemVersionSource(e.Store_, w, Installation)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func (e *MemEnv) RegisterPins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, bindings []runtimev1.SecretBinding) error {
	return e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return e.Store_.RegisterPins(tx, PinRequest{Scope: scope, Consumer: consumer, Bindings: bindings})
	})
}

func (e *MemEnv) ReleasePins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef) (n int64, err error) {
	err = e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		n, err = e.Store_.ReleasePins(tx, scope, consumer)
		return err
	})
	return n, err
}

func (e *MemEnv) HasPin(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, secretID string, version int64) (bool, bool, error) {
	return e.Store_.HasPin(ctx, scope, consumer, secretID, version)
}

func (e *MemEnv) AuditActions(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	out, err := e.Store_.AuditActions(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *MemEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	ids, err := e.Events.EventIDs(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *MemEnv) WrapRevisions(t testing.TB, tenant domain.TenantID, keyID string) []int64 {
	t.Helper()
	revs, err := e.Store_.WrapRevisions(context.Background(), tenant, keyID)
	if err != nil {
		t.Fatal(err)
	}
	return revs
}

func (e *MemEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

// PGEnv is the PostgreSQL environment: the migrated database with the fixture
// tenants, the events store with an appender that can fail after the real
// append, and the development keyfile provider.
type PGEnv struct {
	Base   *eventstest.Env
	Events *eventspg.Store
	Pg     *secretspg.Store
	Wrap   secrets.KeyWrapper

	mu    sync.Mutex
	fault error
}

var _ Env = (*PGEnv)(nil)

// OpenPostgres is the Open of the PostgreSQL adapter. It skips without
// PostgreSQL, and always in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	base := eventstest.NewDB(t, 40)
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		if _, err := base.Migration.Exec(context.Background(), `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(s.TenantID)); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		if _, err := base.Migration.Exec(context.Background(), `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(s.TenantID), string(s.ProjectID)); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(context.Background(), base.Migration, manifest, storage.MigrateOptions{RuntimeRole: base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A pin names the run that consumed the version. This fixture exercises the
	// secrets module without execution, so the run link is dropped; the
	// execution composition tests cover it against real runs.
	pgtest.DropExecutionLinks(t, base.Migration, "secret_pins", "secret_pins_run_fk")
	p := &PGEnv{Base: base, Wrap: OpenKeyfile(t).Wrapper}
	p.Events = base.NewStore(t, eventstest.DefaultOptions())
	p.Pg, err = secretspg.NewStore(secretspg.Config{DB: base.Runtime, Events: p.appender(), IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *PGEnv) appender() secretspg.EventAppender {
	inner := secretspg.EventsFrom(e.Events)
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		if err := inner(ctx, tx, env); err != nil {
			return err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.fault
	}
}

func (e *PGEnv) Store() secrets.Store               { return e.Pg }
func (e *PGEnv) Variables() secrets.VariableCatalog { return e.Pg }
func (e *PGEnv) Secrets() secrets.SecretCatalog     { return e.Pg }
func (e *PGEnv) Wrapper() secrets.KeyWrapper        { return e.Wrap }

func (e *PGEnv) Service(t testing.TB, w secrets.KeyWrapper) *secrets.Service {
	t.Helper()
	svc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: Installation, Store: e.Pg, Wrapper: w, IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (e *PGEnv) Source(t testing.TB, w secrets.KeyWrapper) secrets.VersionSource {
	t.Helper()
	src, err := secretspg.NewVersionSource(e.Pg, w, Installation)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func (e *PGEnv) RegisterPins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, bindings []runtimev1.SecretBinding) error {
	return storage.WithTenant(ctx, e.Base.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return e.Pg.RegisterPins(ctx, tx, secretspg.PinRequest{Scope: scope, Consumer: consumer, Bindings: bindings})
	})
}

func (e *PGEnv) ReleasePins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef) (n int64, err error) {
	err = storage.WithTenant(ctx, e.Base.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		n, err = e.Pg.ReleasePins(ctx, tx, scope, consumer)
		return err
	})
	return n, err
}

func (e *PGEnv) HasPin(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, secretID string, version int64) (bool, bool, error) {
	return e.Pg.HasPin(ctx, scope, consumer, secretID, version)
}

func (e *PGEnv) column(t testing.TB, tenant domain.TenantID, sql string, args ...any) []string {
	t.Helper()
	var out []string
	// The outbox and audit policies are tenant-only: read in the tenant's context.
	err := storage.WithTenant(context.Background(), e.Base.Migration, tenant, func(ctx context.Context, tx pgx.Tx) error {
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

func (e *PGEnv) AuditActions(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	out := e.column(t, tenant, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type IN ('variable', 'secret', 'secret_scope_key')`, string(tenant))
	slices.Sort(out)
	return out
}

func (e *PGEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	return e.column(t, tenant, `SELECT event_id FROM outbox WHERE tenant_id = $1 ORDER BY event_id`, string(tenant))
}

func (e *PGEnv) WrapRevisions(t testing.TB, tenant domain.TenantID, keyID string) []int64 {
	t.Helper()
	rows, err := e.Base.Migration.Query(context.Background(), `SELECT revision FROM secret_scope_key_wraps WHERE tenant_id = $1 AND key_id = $2 ORDER BY revision`, string(tenant), keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *PGEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}
