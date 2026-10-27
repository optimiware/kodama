package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

const (
	installation = "inst-test"
	tenantA      = domain.TenantID("t_acme")
	tenantB      = domain.TenantID("t_globex")
	projectA1    = domain.ProjectID("p_inventory")
	projectA2    = domain.ProjectID("p_billing")
	projectB1    = domain.ProjectID("p_ops")
)

var (
	scopeA1 = domain.Scope{TenantID: tenantA, ProjectID: projectA1}
	scopeA2 = domain.Scope{TenantID: tenantA, ProjectID: projectA2}
	scopeB1 = domain.Scope{TenantID: tenantB, ProjectID: projectB1}
)

// env is a migrated database with two tenants, the event bus, the module's
// store and service over a development keyfile provider.
type env struct {
	t    testing.TB
	base *eventstest.Env
	ev   *eventspg.Store

	store *secretspg.Store
	svc   *secrets.Service
	idem  *httppg.Idempotency
	keyed *secretspg.Keyed

	keys *keyring
	// wrapper is what the service uses; tests wrap it to inject failures.
	wrapper *faultyWrapper
	clock   func() time.Time
}

// keyring manages development key files in a temp dir.
type keyring struct {
	t     testing.TB
	dir   string
	files map[string]string
}

func newKeyring(t testing.TB) *keyring {
	return &keyring{t: t, dir: t.TempDir(), files: map[string]string{}}
}

func (k *keyring) add(version string) string {
	k.t.Helper()
	key := make([]byte, keyfile.KeySize)
	if _, err := rand.Read(key); err != nil {
		k.t.Fatal(err)
	}
	p := filepath.Join(k.dir, "master-key-"+version)
	if err := os.WriteFile(p, key, 0o600); err != nil {
		k.t.Fatal(err)
	}
	k.files[version] = p
	return p
}

func (k *keyring) provider(current string, versions ...string) *keyfile.Provider {
	k.t.Helper()
	files := map[string]string{}
	for _, v := range versions {
		files[v] = k.files[v]
	}
	p, err := keyfile.New(keyfile.Config{Development: true, Current: current, Files: files})
	if err != nil {
		k.t.Fatal(err)
	}
	return p
}

// faultyWrapper delegates to a real wrapper and can inject failures and hooks.
type faultyWrapper struct {
	mu       sync.Mutex
	inner    secrets.KeyWrapper
	wrapErr  error
	unwrapEr error
	// beforeUnwrap runs inside Unwrap, before delegating (used to race a
	// revocation against decryption).
	beforeUnwrap func(ctx context.Context)
	wraps        int
	unwraps      int
}

func (f *faultyWrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	f.mu.Lock()
	f.wraps++
	err, inner := f.wrapErr, f.inner
	f.mu.Unlock()
	if err != nil {
		return secrets.WrappedKey{}, err
	}
	return inner.Wrap(ctx, wc, key)
}

func (f *faultyWrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, w secrets.WrappedKey) ([]byte, error) {
	f.mu.Lock()
	f.unwraps++
	err, inner, hook := f.unwrapEr, f.inner, f.beforeUnwrap
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if err != nil {
		return nil, err
	}
	return inner.Unwrap(ctx, wc, w)
}

func (f *faultyWrapper) set(fn func(*faultyWrapper)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faultyWrapper) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wraps, f.unwraps
}

type envOption func(*envConfig)

type envConfig struct {
	backlog int
}

func withBacklog(n int) envOption { return func(c *envConfig) { c.backlog = n } }

func fullManifest(t testing.TB) storage.Manifest {
	t.Helper()
	// Applied order: base, events (by eventstest), then HTTP idempotency and the module.
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newEnv(t testing.TB, opts ...envOption) *env {
	t.Helper()
	var cfg envConfig
	for _, o := range opts {
		o(&cfg)
	}
	base := eventstest.NewDB(t, 40)
	base.SeedTenant(t, string(tenantA), string(projectA1), string(projectA2))
	base.SeedTenant(t, string(tenantB), string(projectB1))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, base.Migration, fullManifest(t), storage.MigrateOptions{RuntimeRole: base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A pin names the consuming run; these tests exercise the secrets module
	// without execution, so the run link is dropped. The execution composition
	// tests cover it against real runs.
	pgtest.DropExecutionLinks(t, base.Migration, "secret_pins", "secret_pins_run_fk")

	e := &env{t: t, base: base, keys: newKeyring(t)}
	e.keys.add("v1")
	e.wrapper = &faultyWrapper{inner: e.keys.provider("v1", "v1")}
	e.clock = func() time.Time { return time.Now().UTC() }

	evOpts := eventstest.DefaultOptions()
	if cfg.backlog > 0 {
		evOpts.Backlog.MaxEvents = cfg.backlog
	}
	e.ev = base.NewStore(t, evOpts)
	store, err := secretspg.NewStore(secretspg.Config{DB: base.Runtime, Events: secretspg.EventsFrom(e.ev), IDs: secrets.RandomIDs{}, Clock: func() time.Time { return e.clock() }})
	if err != nil {
		t.Fatal(err)
	}
	e.store = store
	e.svc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: installation, Store: store, Wrapper: e.wrapper, IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	e.idem, err = httppg.NewIdempotency(httppg.Config{DB: base.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	e.keyed, err = secretspg.NewKeyed(e.idem, store)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) ctx() context.Context {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.t.Cleanup(cancel)
	return ctx
}

func (e *env) mutation(actor string) secrets.Mutation {
	return secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: actor}, RequestID: "req_test", Now: e.clock()}
}

// createSecret creates a secret with its first value through the service.
func (e *env) createSecret(scope domain.Scope, name string, value string) secrets.Secret {
	e.t.Helper()
	ctx := e.ctx()
	m := e.mutation("alice")
	p, err := e.svc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte(value))
	if err != nil {
		e.t.Fatalf("prepare secret: %v", err)
	}
	var out secrets.Secret
	if err := e.svc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.svc.CreateSecret(ctx, w, m, p)
		return err
	}); err != nil {
		e.t.Fatalf("create secret: %v", err)
	}
	return out
}

// rotate adds a new value version, preparing against the secret's current resource version.
func (e *env) rotate(scope domain.Scope, id string, value string) secrets.Secret {
	e.t.Helper()
	s, err := e.rotateErr(scope, id, value)
	if err != nil {
		e.t.Fatalf("rotate: %v", err)
	}
	return s
}

func (e *env) rotateErr(scope domain.Scope, id string, value string) (secrets.Secret, error) {
	ctx := e.ctx()
	cur, err := e.store.GetSecret(ctx, scope, id)
	if err != nil {
		return secrets.Secret{}, err
	}
	m := e.mutation("alice")
	p, err := e.svc.PrepareVersion(ctx, m, scope, id, cur.Version(), []byte(value))
	if err != nil {
		return secrets.Secret{}, err
	}
	var out secrets.Secret
	err = e.svc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.svc.AddVersion(ctx, w, m, p)
		return err
	})
	return out, err
}

func (e *env) revoke(scope domain.Scope, id string, version int64) secrets.RevocationResult {
	e.t.Helper()
	res, err := e.revokeErr(scope, id, version)
	if err != nil {
		e.t.Fatalf("revoke: %v", err)
	}
	return res
}

func (e *env) revokeErr(scope domain.Scope, id string, version int64) (secrets.RevocationResult, error) {
	var res secrets.RevocationResult
	err := e.svc.Atomically(e.ctx(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		res, err = e.svc.RevokeVersion(ctx, w, e.mutation("alice"), scope, id, version)
		return err
	})
	return res, err
}

func (e *env) createVariable(scope domain.Scope, name string, value string) secrets.Variable {
	e.t.Helper()
	var out secrets.Variable
	err := e.svc.Atomically(e.ctx(), scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.svc.CreateVariable(ctx, w, e.mutation("alice"), secrets.CreateVariableInput{Scope: scope, Name: name, Value: strValue(value)})
		return err
	})
	if err != nil {
		e.t.Fatalf("create variable: %v", err)
	}
	return out
}

// register pins the secret's version for a run through the exported pin API in
// its own tenant transaction.
func (e *env) pin(scope domain.Scope, runID string, name string, version int64) error {
	return storage.WithTenant(e.ctx(), e.base.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return e.store.RegisterPins(ctx, tx, secretspg.PinRequest{
			Scope: scope, Consumer: secrets.ConsumerRef{Kind: "run", ID: runID},
			Bindings: []runtimeBinding{{Ref: "secret://project/" + name, Version: version, Name: name, Delivery: "file"}},
		})
	})
}

func (e *env) releasePins(scope domain.Scope, runID string) int64 {
	e.t.Helper()
	var n int64
	err := storage.WithTenant(e.ctx(), e.base.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = e.store.ReleasePins(ctx, tx, scope, secrets.ConsumerRef{Kind: "run", ID: runID})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ownerExec / count / scalar use the migration role (the owner policy lets it
// see every tenant), for assertions about what is physically stored.
func (e *env) ownerExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.base.Migration.Exec(e.ctx(), sql, args...); err != nil {
		e.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.base.Migration.QueryRow(e.ctx(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// dumpAll returns every byte of text the module persisted or enqueued, so tests
// can assert that a sentinel is absent. Ciphertext columns are excluded: they
// are expected to be opaque and are checked separately.
func (e *env) dumpAll() string {
	e.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT tenant_id, project_id, id, name, type, description, value_text FROM variables) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT tenant_id, project_id, id, name, kind, current_version, version FROM secrets) t`,
		`SELECT coalesce(string_agg(metadata::text || action || coalesce(reason, '') || coalesce(request_id, ''), E'\n'), '') FROM audit`,
		`SELECT coalesce(string_agg(convert_from(response_body, 'UTF8') || response_headers::text, E'\n'), '') FROM idempotency`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM secret_consumer_handling) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM secret_pins) t`,
	} {
		var s string
		if err := e.base.Migration.QueryRow(e.ctx(), q).Scan(&s); err != nil {
			e.t.Fatalf("dump: %v\n%s", err, q)
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	// The event tables only admit tenant-scoped reads, so read them through the
	// runtime role once per tenant.
	for _, tenant := range []domain.TenantID{tenantA, tenantB} {
		for _, q := range []string{
			`SELECT coalesce(string_agg(convert_from(payload, 'UTF8'), E'\n'), '') FROM outbox WHERE tenant_id = $1`,
			`SELECT coalesce(string_agg(convert_from(payload, 'UTF8'), E'\n'), '') FROM event_observations WHERE tenant_id = $1`,
		} {
			var s string
			err := storage.WithTenant(e.ctx(), e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, q, string(tenant)).Scan(&s)
			})
			if err != nil {
				e.t.Fatalf("dump events: %v\n%s", err, q)
			}
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (e *env) auditActions() []string {
	e.t.Helper()
	rows, err := e.base.Migration.Query(e.ctx(), `SELECT action FROM audit ORDER BY seq`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// outboxTypes counts the committed configuration.changed events of both tenants.
func (e *env) outboxTypes() int {
	e.t.Helper()
	total := 0
	for _, tenant := range []domain.TenantID{tenantA, tenantB} {
		var n int
		err := storage.WithTenant(e.ctx(), e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = 'configuration.changed'`, string(tenant)).Scan(&n)
		})
		if err != nil {
			e.t.Fatal(err)
		}
		total += n
	}
	return total
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func isErr(err error, target error) bool { return errors.Is(err, target) }

func notContains(t testing.TB, what, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if n != "" && bytes.Contains([]byte(haystack), []byte(n)) {
			t.Errorf("%s contains the sentinel %q", what, n)
		}
	}
}
