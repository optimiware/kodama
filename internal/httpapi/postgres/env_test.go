package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

const (
	tenantA domain.TenantID = "tenant-a"
	tenantB domain.TenantID = "tenant-b"
)

func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func manifest(t testing.TB) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(storage.BaseContribution(), httppg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// env is a migrated database with two tenants, fixture tables standing in for a
// consuming module's persistence, and an HTTP router around them.
type env struct {
	db  *pgtest.Database
	mig *pgxpool.Pool
	rt  *pgxpool.Pool

	auth  *authority
	idem  *httppg.Idempotency
	route *httpapi.Router

	// beforeInsert, when set, runs inside the create callback before the fixture
	// write; tests use it to hold a transaction open or to fail it.
	hookMu       sync.Mutex
	beforeInsert func(ctx context.Context, tx pgx.Tx) error
	doCalls      atomic.Int64
}

// fixtureSQL is the stand-in for a consuming module's schema: a versioned
// resource with tenant/project ownership and an append-only history, written by
// the same transaction as the idempotency record.
const fixtureSQL = `
CREATE TABLE t004_widgets (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    id         kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    name       text NOT NULL,
    version    bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE TABLE t004_widget_history (
    tenant_id kodama_id NOT NULL,
    seq       bigint GENERATED ALWAYS AS IDENTITY,
    widget_id kodama_id NOT NULL,
    version   bigint NOT NULL,
    name      text NOT NULL,
    PRIMARY KEY (tenant_id, seq),
    FOREIGN KEY (tenant_id, widget_id) REFERENCES t004_widgets (tenant_id, id)
);
ALTER TABLE t004_widgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE t004_widgets FORCE ROW LEVEL SECURITY;
ALTER TABLE t004_widget_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE t004_widget_history FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON t004_widgets TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON t004_widget_history TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON t004_widgets USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON t004_widget_history USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT, UPDATE ON t004_widgets TO {{runtime_role}};
GRANT SELECT, INSERT ON t004_widget_history TO {{runtime_role}};
`

type option func(*httppg.Config)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	ctx := testCtx(t)
	db := pgtest.New(t)
	mig := db.MigrationPool(ctx, 4)
	if _, err := storage.Migrate(ctx, mig, manifest(t), storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := &env{db: db, mig: mig, rt: db.RuntimePool(ctx, 30), auth: newAuthority()}
	e.ownerExec(t, `
INSERT INTO tenants (id, key) VALUES ('tenant-a', 'a'), ('tenant-b', 'b');
INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES
  ('tenant-a', 'pa1', 'pa1', 3), ('tenant-a', 'pa2', 'pa2', 3), ('tenant-b', 'pb1', 'pb1', 3);`)
	// Fixture DDL with the runtime role substituted like a module migration.
	e.ownerExec(t, strings.ReplaceAll(fixtureSQL, "{{runtime_role}}", `"`+db.RuntimeRole+`"`))

	cfg := httppg.Config{DB: e.rt}
	for _, o := range opts {
		o(&cfg)
	}
	e.setDB(t, cfg)
	return e
}

func (e *env) setDB(t *testing.T, cfg httppg.Config) {
	t.Helper()
	idem, err := httppg.NewIdempotency(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.idem = idem
	e.route = e.newRouter(t)
}

func (e *env) ownerExec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.mig.Exec(testCtx(t), sql, args...); err != nil {
		t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

func (e *env) count(t testing.TB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.mig.QueryRow(testCtx(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

func (e *env) widgets(t testing.TB) int { return e.count(t, `SELECT count(*) FROM t004_widgets`) }
func (e *env) history(t testing.TB) int {
	return e.count(t, `SELECT count(*) FROM t004_widget_history`)
}
func (e *env) records(t testing.TB) int { return e.count(t, `SELECT count(*) FROM idempotency`) }
func (e *env) setHook(f func(ctx context.Context, tx pgx.Tx) error) {
	e.hookMu.Lock()
	e.beforeInsert = f
	e.hookMu.Unlock()
}

func (e *env) hook() func(ctx context.Context, tx pgx.Tx) error {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	return e.beforeInsert
}

// authority is the injected authentication and authorization fake. Tokens map
// to principals; revocation is switchable per principal and per access kind.
type authority struct {
	mu            sync.Mutex
	revokedRoute  map[string]error // principal -> error for route-level access
	revokedTarget map[string]error // principal -> error for resource-level access
	deniedRes     map[string]error // "kind:id" -> error for that one resource, any principal
	calls         []string
	onTarget      func(ctx context.Context) // called for resource-level checks
}

func newAuthority() *authority {
	return &authority{revokedRoute: map[string]error{}, revokedTarget: map[string]error{}, deniedRes: map[string]error{}}
}

func (a *authority) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer alice":
		return httpapi.Principal{ID: "alice"}, nil
	case "Bearer bob":
		return httpapi.Principal{ID: "bob"}, nil
	}
	return httpapi.Principal{}, httpapi.ErrUnauthenticated
}

func (a *authority) Authorize(ctx context.Context, p httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, p.ID+" "+string(action)+" "+string(t.TenantID)+"/"+string(t.ProjectID)+"/"+t.Resource.Kind+":"+t.Resource.ID)
	if t.Resource.ID != "" {
		if a.onTarget != nil {
			a.onTarget(ctx)
		}
		if err := a.revokedTarget[p.ID]; err != nil {
			return err
		}
		if err := a.deniedRes[t.Resource.Kind+":"+t.Resource.ID]; err != nil {
			return err
		}
	}
	return a.revokedRoute[p.ID]
}

// denyResource refuses access to exactly one resource, whoever asks; a nil
// error lifts the refusal.
func (a *authority) denyResource(kind, id string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		delete(a.deniedRes, kind+":"+id)
		return
	}
	a.deniedRes[kind+":"+id] = err
}

func (a *authority) revoke(principal string, route, target error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if route != nil {
		a.revokedRoute[principal] = route
	}
	if target != nil {
		a.revokedTarget[principal] = target
	}
}

type widget struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "w_" + hex.EncodeToString(b[:])
}

func (e *env) newRouter(t *testing.T) *httpapi.Router {
	t.Helper()
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: e.auth, Authorizer: e.auth, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	const base = "/tenants/{tenantId}/projects/{projectId}/widgets"
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(rt.Handle(httpapi.Route{Method: "POST", Path: base, Action: "widget.create", Handler: e.createWidget}))
	must(rt.Handle(httpapi.Route{Method: "PATCH", Path: base + "/{id}", Action: "widget.edit", ResourceKind: "widget", ResourceParam: "id", Handler: e.editWidget}))
	// Keyed operations on an existing widget: the route is resource-scoped. Clone
	// names no target (route authorization is sufficient); derive names the
	// widget it creates.
	must(rt.Handle(httpapi.Route{Method: "POST", Path: base + "/{id}/clone", Action: "widget.clone", ResourceKind: "widget", ResourceParam: "id",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) { e.derive(w, r, c, false) }}))
	must(rt.Handle(httpapi.Route{Method: "POST", Path: base + "/{id}/derive", Action: "widget.derive", ResourceKind: "widget", ResourceParam: "id",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) { e.derive(w, r, c, true) }}))
	return rt
}

// derive is a keyed, resource-scoped create: it writes a new widget from an
// existing one's route. withTarget selects whether the outcome names the new
// widget for replay authorization.
func (e *env) derive(w http.ResponseWriter, r *http.Request, c *httpapi.Call, withTarget bool) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	id, err := c.Identity(r, body)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	res, err := e.idem.Execute(r.Context(), httppg.Request{
		Identity:        id,
		AuthorizeReplay: c.AuthorizeReference,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			e.doCalls.Add(1)
			if h := e.hook(); h != nil {
				if err := h(ctx, tx); err != nil {
					return httpapi.Outcome{}, err
				}
			}
			wid, err := insertWidget(ctx, tx, string(scope.TenantID), string(scope.ProjectID), "derived-secret")
			if err != nil {
				return httpapi.Outcome{}, err
			}
			resp, err := httpapi.NewStoredResponse(r, http.StatusCreated, widget{ID: wid, Name: "derived-secret", Version: 1}, map[string]string{"ETag": `"1"`})
			out := httpapi.Outcome{Response: resp}
			if withTarget {
				out.Target = httpapi.Reference{Kind: "widget", ID: wid}
			}
			return out, err
		},
	})
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, res.Response, res.Replayed)
}

// createWidget is the representative keyed create handler (never a product
// route): decode, identity, one transaction writing the fixture, its history
// and the idempotency record, then HTTP success only after commit.
func (e *env) createWidget(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	id, err := c.Identity(r, body)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	res, err := e.idem.Execute(r.Context(), httppg.Request{
		Identity:        id,
		AuthorizeReplay: c.AuthorizeReference,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			e.doCalls.Add(1)
			if h := e.hook(); h != nil {
				if err := h(ctx, tx); err != nil {
					return httpapi.Outcome{}, err
				}
			}
			wid := newID()
			if _, err := tx.Exec(ctx, `INSERT INTO t004_widgets (tenant_id, id, project_id, name) VALUES ($1, $2, $3, $4)`,
				string(scope.TenantID), wid, string(scope.ProjectID), in.Name); err != nil {
				return httpapi.Outcome{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO t004_widget_history (tenant_id, widget_id, version, name) VALUES ($1, $2, 1, $3)`,
				string(scope.TenantID), wid, in.Name); err != nil {
				return httpapi.Outcome{}, err
			}
			resp, err := httpapi.NewStoredResponse(r, http.StatusCreated, widget{ID: wid, Name: in.Name, Version: 1},
				map[string]string{"ETag": `"1"`, "Location": "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/widgets/" + wid})
			return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: "widget", ID: wid}}, err
		},
	})
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, res.Response, res.Replayed)
}

// editWidget is the representative edit: the expected version goes into one
// conditional UPDATE, so two writers holding the same ETag cannot both win.
func (e *env) editWidget(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := httpapi.DecodeJSON(w, r, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var out widget
	err = storage.WithTenant(r.Context(), e.rt, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
UPDATE t004_widgets SET name = $4, version = version + 1
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $5
RETURNING id, name, version`,
			string(scope.TenantID), string(scope.ProjectID), c.Target.Resource.ID, in.Name, int64(expected)).Scan(&out.ID, &out.Name, &out.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			var current int64
			err := tx.QueryRow(ctx, `SELECT version FROM t004_widgets WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
				string(scope.TenantID), string(scope.ProjectID), c.Target.Resource.ID).Scan(&current)
			if errors.Is(err, pgx.ErrNoRows) {
				return httpapi.NotFoundError()
			}
			if err != nil {
				return err
			}
			return httpapi.CheckVersion(expected, domain.Version(current)) // always a conflict here
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO t004_widget_history (tenant_id, widget_id, version, name) VALUES ($1, $2, $3, $4)`,
			string(scope.TenantID), out.ID, out.Version, out.Name)
		return err
	})
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	_ = httpapi.SetETag(w, domain.Version(out.Version))
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

type result struct {
	Code   int
	Header http.Header
	Body   string
}

func (r result) widget(t testing.TB) widget {
	t.Helper()
	var w widget
	if err := json.Unmarshal([]byte(r.Body), &w); err != nil {
		t.Fatalf("not a widget: %v: %s", err, r.Body)
	}
	return w
}

func (r result) errorCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	if err := json.Unmarshal([]byte(r.Body), &e); err != nil {
		t.Fatalf("not an error envelope: %v: %s", err, r.Body)
	}
	return e.Error.Code
}

func (r result) replayed() bool { return r.Header.Get("Idempotent-Replayed") == "true" }

// call performs a request against the router.
func (e *env) call(method, path, token, body string, headers ...string) result {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.route.ServeHTTP(rec, req)
	return result{Code: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
}

func (e *env) create(token, tenant, project, key, body string) result {
	return e.call("POST", "/api/v1/tenants/"+tenant+"/projects/"+project+"/widgets", token, body, "Idempotency-Key", key)
}

const createA1 = "/api/v1/tenants/tenant-a/projects/pa1/widgets"

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func headerDump(r result) string {
	var b strings.Builder
	for k, vs := range r.Header {
		b.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	return b.String()
}

func (e *env) authCalls() int {
	e.auth.mu.Lock()
	defer e.auth.mu.Unlock()
	return len(e.auth.calls)
}

func tenantFor(s string) domain.TenantID { return domain.TenantID(s) }
