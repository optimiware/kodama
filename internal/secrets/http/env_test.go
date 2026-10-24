package secretshttp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretshttp "github.com/optimiware/kodama/internal/secrets/http"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	installation = "inst-http"
	tenantA      = domain.TenantID("t_acme")
	tenantB      = domain.TenantID("t_globex")
	projectA1    = domain.ProjectID("p_inventory")
	projectA2    = domain.ProjectID("p_billing")
	projectB1    = domain.ProjectID("p_ops")
)

var scopeA1 = domain.Scope{TenantID: tenantA, ProjectID: projectA1}

const baseURL = "/api/v1/tenants/t_acme/projects/p_inventory"

// authority is the injected authentication and authorization fake. Principals
// map to the tenants they may use; individual principals can be revoked and
// individual actions denied, switchable during a test.
type authority struct {
	mu      sync.Mutex
	tenants map[string]map[domain.TenantID]bool
	revoked map[string]error
	denied  map[httpapi.Action]error
	calls   int
}

func newAuthority() *authority {
	return &authority{
		tenants: map[string]map[domain.TenantID]bool{
			"alice": {tenantA: true}, "bob": {tenantB: true}, "carol": {tenantA: true, tenantB: true},
		},
		revoked: map[string]error{}, denied: map[httpapi.Action]error{},
	}
}

func (a *authority) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, ok := a.tenants[tok]; !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

func (a *authority) Authorize(_ context.Context, p httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if err := a.revoked[p.ID]; err != nil {
		return err
	}
	if err := a.denied[action]; err != nil {
		return err
	}
	if !a.tenants[p.ID][t.TenantID] {
		return httpapi.ErrHidden
	}
	return nil
}

func (a *authority) revoke(principal string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		delete(a.revoked, principal)
		return
	}
	a.revoked[principal] = err
}

func (a *authority) deny(action httpapi.Action, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		delete(a.denied, action)
		return
	}
	a.denied[action] = err
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// flakyWrapper fails key-provider calls on demand around a real provider.
type flakyWrapper struct {
	mu    sync.Mutex
	inner secrets.KeyWrapper
	err   error
}

func (f *flakyWrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return secrets.WrappedKey{}, err
	}
	return f.inner.Wrap(ctx, wc, key)
}

func (f *flakyWrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, w secrets.WrappedKey) ([]byte, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.inner.Unwrap(ctx, wc, w)
}

func (f *flakyWrapper) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// commitDB wraps the runtime pool so tests can make commits fail, either
// before they reach the server (nothing committed) or after (committed but the
// client sees an error: an unknown outcome).
type commitDB struct {
	storage.TxBeginner
	mu   sync.Mutex
	mode string // "", "rollback", "lost"
}

func (d *commitDB) set(mode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mode = mode
}

func (d *commitDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.TxBeginner.BeginTx(ctx, opts)
	d.mu.Lock()
	mode := d.mode
	d.mu.Unlock()
	if err != nil || mode == "" || opts.AccessMode == pgx.ReadOnly {
		return tx, err
	}
	return &flakyTx{Tx: tx, mode: mode}, nil
}

type flakyTx struct {
	pgx.Tx
	mode string
}

func (t *flakyTx) Commit(ctx context.Context) error {
	if t.mode == "rollback" {
		_ = t.Tx.Rollback(ctx)
		return errors.New("connection reset before commit")
	}
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	return errors.New("connection reset after commit") // committed, but the client cannot know
}

type env struct {
	t     testing.TB
	base  *eventstest.Env
	auth  *authority
	logs  *lockedBuffer
	wrap  *flakyWrapper
	db    *commitDB
	store *secretspg.Store
	svc   *secrets.Service
	idem  *httppg.Idempotency
	h     *secretshttp.Handler
	rt    *httpapi.Router
	ev    *eventspg.Store
}

type envOption func(*envConfig)

type envConfig struct{ backlog int }

func withBacklog(n int) envOption { return func(c *envConfig) { c.backlog = n } }

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
	m, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), secretspg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, base.Migration, m, storage.MigrateOptions{RuntimeRole: base.DB.RuntimeRole}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	key := make([]byte, keyfile.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master-key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": keyPath}})
	if err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, base: base, auth: newAuthority(), logs: &lockedBuffer{}, wrap: &flakyWrapper{inner: provider}, db: &commitDB{TxBeginner: base.Runtime}}
	evOpts := eventstest.DefaultOptions()
	if cfg.backlog > 0 {
		evOpts.Backlog.MaxEvents = cfg.backlog
	}
	e.ev = base.NewStore(t, evOpts)
	e.store, err = secretspg.NewStore(secretspg.Config{DB: e.db, Events: secretspg.EventsFrom(e.ev), IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: installation, Store: e.store, Wrapper: e.wrap, IDs: secrets.RandomIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	e.idem, err = httppg.NewIdempotency(httppg.Config{DB: e.db})
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := secretspg.NewKeyed(e.idem, e.store)
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := httpapi.NewCursorCodec(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.h, err = secretshttp.NewHandler(secretshttp.Config{Service: e.svc, Keyed: keyed, Cursors: cursors, HashKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	e.rt, err = httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: e.auth, Authorizer: e.auth, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
		Logger: slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.h.RegisterRoutes(e.rt); err != nil {
		t.Fatal(err)
	}
	return e
}

// result is one recorded response.
type result struct {
	Code   int
	Header http.Header
	Body   string
}

func (r result) json(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.Body), v); err != nil {
		t.Fatalf("not JSON (%d): %v\n%s", r.Code, err, r.Body)
	}
}

func (r result) errorCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.json(t, &e)
	return e.Error.Code
}

func (r result) etag() string { return r.Header.Get("ETag") }

func (r result) dump() string {
	var b strings.Builder
	b.WriteString(r.Body)
	for k, vs := range r.Header {
		b.WriteString("\n" + k + ": " + strings.Join(vs, ","))
	}
	return b.String()
}

// transcript collects every response a test saw, for disclosure checks.
var (
	transcriptMu sync.Mutex
	transcripts  = map[testing.TB]*strings.Builder{}
)

func (e *env) record(r result) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	b := transcripts[e.t]
	if b == nil {
		b = &strings.Builder{}
		transcripts[e.t] = b
		if t, ok := e.t.(*testing.T); ok {
			t.Cleanup(func() {
				transcriptMu.Lock()
				delete(transcripts, e.t)
				transcriptMu.Unlock()
			})
		}
	}
	b.WriteString(r.dump())
	b.WriteString("\n")
}

func (e *env) seen() string {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	if b := transcripts[e.t]; b != nil {
		return b.String()
	}
	return ""
}

// call performs a request as token. headers are name, value pairs.
func (e *env) call(method, path, token, body string, headers ...string) result {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.rt.ServeHTTP(rec, req)
	res := result{Code: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
	e.record(res)
	return res
}

func (e *env) ownerExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.base.Migration.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.base.Migration.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

func (e *env) events(tenant domain.TenantID) int {
	e.t.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = 'configuration.changed'`, string(tenant)).Scan(&n)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// dumpAll returns everything the module persisted as text (ciphertext excluded).
func (e *env) dumpAll() string {
	e.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM variables) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM variable_versions) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT tenant_id, project_id, id, name, kind, current_version, version FROM secrets) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT tenant_id, project_id, secret_id, version, scope_key_id, created_by, revoked_by FROM secret_versions) t`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM audit t`,
		`SELECT coalesce(string_agg(convert_from(response_body, 'UTF8') || response_headers::text || principal || canonical_route, E'\n'), '') FROM idempotency`,
		`SELECT coalesce(string_agg(t::text, E'\n'), '') FROM (SELECT * FROM secret_consumer_handling) t`,
	} {
		var s string
		if err := e.base.Migration.QueryRow(context.Background(), q).Scan(&s); err != nil {
			e.t.Fatalf("dump: %v\n%s", err, q)
		}
		b.WriteString(s + "\n")
	}
	for _, tenant := range []domain.TenantID{tenantA, tenantB} {
		for _, q := range []string{
			`SELECT coalesce(string_agg(convert_from(payload, 'UTF8'), E'\n'), '') FROM outbox WHERE tenant_id = $1`,
			`SELECT coalesce(string_agg(convert_from(payload, 'UTF8'), E'\n'), '') FROM event_observations WHERE tenant_id = $1`,
		} {
			var s string
			err := storage.WithTenant(context.Background(), e.base.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, q, string(tenant)).Scan(&s)
			})
			if err != nil {
				e.t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
	}
	return b.String()
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func json_marshal(v any) ([]byte, error) { return json.Marshal(v) }
