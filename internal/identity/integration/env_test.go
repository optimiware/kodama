// Package integration holds the identity acceptance tests: a real oauthsonas
// issuer (isolated container, S256 PKCE authorization-code flow), real
// PostgreSQL under the non-owner runtime role, the real event store, and the
// merged work and HTTP-idempotency modules wired through the identity bridges.
// The tests need no NATS, S3 or Git. Skipped acceptance is not passing evidence:
// run with KODAMA_REQUIRE_POSTGRES=1.
package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	httpapipg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/consumer"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/identity/idtest"
	idoidc "github.com/optimiware/kodama/internal/identity/oidc"
	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

const (
	tenantA, projectA, projectA2 = "t010_acme", "p010_inventory", "p010_billing"
	tenantB, projectB            = "t010_globex", "p010_ledger"
	commit                       = "1111111111111111111111111111111111111111"
)

type env struct {
	t       testing.TB
	fx      *oidctest.Fixture
	stack   *idtest.Stack
	srv     *httptest.Server
	idem    *httpapipg.Idempotency
	work    *work.Service
	store   *workpg.Store
	tokens  map[string]string
	users   map[string]identity.UserID
	hook    atomic.Pointer[func(ctx context.Context)]
	widgets int
}

// guards stand in for the workspace and evidence modules, which are not part of
// this task; the permission guard is the real identity one.
type guards struct{ e *env }

func (guards) WriterSettled(context.Context, domain.Scope, domain.WorkItemID) (bool, error) {
	return true, nil
}

func (g guards) VerifySubject(ctx context.Context, _ domain.Scope, _ domain.WorkItemID, c work.ReviewClaim) (work.VerifiedSubject, error) {
	if h := g.e.hook.Load(); h != nil {
		(*h)(ctx)
	}
	return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
}

func (guards) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }

// widgetAction is the route of the replay test: a keyed create that stands in for
// any later idempotent resource (runs, decisions).
const widgetAction httpapi.Action = "t010.widget.create"

const widgetsDDL = `
CREATE TABLE t010_widgets (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    id         kodama_id NOT NULL,
    project_id kodama_id NOT NULL,
    name       text NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
ALTER TABLE t010_widgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE t010_widgets FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON t010_widgets TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON t010_widgets USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT ON t010_widgets TO "%s";
`

// newEnv starts the issuer and the stack.
func newEnv(t testing.TB, mutate ...func(*idoidc.Config)) *env {
	t.Helper()
	return newEnvOn(t, oidctest.Start(t), mutate...)
}

// newEnvOn builds a stack (its own database) verifying tokens of an existing issuer.
func newEnvOn(t testing.TB, fx *oidctest.Fixture, mutate ...func(*idoidc.Config)) *env {
	t.Helper()
	e := &env{t: t, fx: fx, stack: idtest.New(t, idtest.Options{}), tokens: map[string]string{}, users: map[string]identity.UserID{}}
	s := e.stack
	s.OwnerExec(fmt.Sprintf(widgetsDDL, s.DB.RuntimeRole))

	cfg := idoidc.Config{Issuer: fx.Issuer, Audience: fx.Audience, AllowInsecureHTTP: true}
	for _, m := range mutate {
		m(&cfg)
	}
	verifier, err := idoidc.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := identity.NewStandaloneAuthority(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: standalone})
	if err != nil {
		t.Fatal(err)
	}
	authn, err := identityhttp.NewAuthenticator(verifier, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	rules := identityhttp.DefaultRules()
	rules[widgetAction] = identityhttp.Rule{Level: identityhttp.LevelProject, Any: []identity.Permission{identity.PermWorkCreate}}
	authz, err := identityhttp.NewAuthorizer(authority, rules)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: authn, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ih, err := identityhttp.NewHandler(s.Service, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ih.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}

	g := guards{e}
	e.store, err = workpg.NewStore(s.Runtime, s.Events, consumer.BindPermissions(func(pgx.Tx) work.TxGuards {
		return work.TxGuards{Writers: g, Dependencies: work.NoDependencyGraph{}, Subjects: g}
	}))
	if err != nil {
		t.Fatal(err)
	}
	workAuth, err := consumer.NewWorkAuthority(authority, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	e.work, err = work.NewService(work.Config{Repository: e.store, Authority: workAuth})
	if err != nil {
		t.Fatal(err)
	}
	wh, err := workhttp.NewHandler(e.work, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := wh.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}

	e.idem, err = httpapipg.NewIdempotency(httpapipg.Config{DB: s.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle(httpapi.Route{Method: http.MethodPost, Path: "/tenants/{tenantId}/projects/{projectId}/widgets", Action: widgetAction, Handler: e.createWidget}); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(rt)
	t.Cleanup(e.srv.Close)

	s.SeedProject(tenantA, projectA, 5)
	s.SeedProject(tenantA, projectA2, 5)
	s.SeedProject(tenantB, projectB, 5)
	return e
}

// signIn logs the persona in at the issuer, presents the access token once so the
// first-login mapping creates the user (and nothing else), and remembers both.
func (e *env) signIn(persona string) string {
	e.t.Helper()
	tok := e.fx.Login(persona).Access
	e.tokens[persona] = tok
	r := e.do(tok, http.MethodGet, "/me", "")
	if r.code != 200 {
		e.t.Fatalf("/me for %s: %d %s", persona, r.code, r.body)
	}
	var me struct{ Id string }
	if err := json.Unmarshal([]byte(r.body), &me); err != nil {
		e.t.Fatal(err)
	}
	e.users[persona] = identity.UserID(me.Id)
	return tok
}

func (e *env) bind(persona string, tenant domain.TenantID, project domain.ProjectID, role identity.Role) identity.BindingID {
	e.t.Helper()
	if _, ok := e.users[persona]; !ok {
		e.signIn(persona)
	}
	return e.stack.Bind(tenant, e.users[persona], project, role)
}

type response struct {
	code   int
	header http.Header
	body   string
}

func (r response) json(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("not JSON: %v: %s", err, r.body)
	}
}

func (r response) errCode(t testing.TB) string {
	t.Helper()
	var env struct{ Error struct{ Code string } }
	r.json(t, &env)
	return env.Error.Code
}

func (e *env) do(token, method, path, body string, headers ...string) response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, header: resp.Header, body: string(data)}
}

func (e *env) createWidget(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	res, err := e.idem.Execute(r.Context(), httpapipg.Request{
		Identity: id, AuthorizeReplay: c.AuthorizeReference,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			wid, _ := identity.RandomID("wd_")
			if _, err := tx.Exec(ctx, `INSERT INTO t010_widgets (tenant_id, id, project_id, name) VALUES ($1, $2, $3, 'secret-widget-name')`,
				string(scope.TenantID), wid, string(scope.ProjectID)); err != nil {
				return httpapi.Outcome{}, err
			}
			resp, err := httpapi.NewStoredResponse(r, http.StatusCreated, map[string]string{"id": wid, "name": "secret-widget-name"}, map[string]string{"ETag": `"1"`})
			return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: "widget", ID: wid}}, err
		},
	})
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, res.Response, res.Replayed)
}

// ---- work helpers ---------------------------------------------------------

func workPath(tenant, project string) string {
	return "/tenants/" + tenant + "/projects/" + project + "/work-items"
}

// itemInReview creates an item and moves it to review with the engineer's token.
// It returns the item path and its ETag.
func (e *env) itemInReview(engineerToken, tenant, project, title string) (path, etag string) {
	e.t.Helper()
	r := e.do(engineerToken, http.MethodPost, workPath(tenant, project),
		fmt.Sprintf(`{"kind":"story","title":%q,"spec":"# Plan\nDo it.","acceptanceCriteria":[{"id":"AC-1","text":"works"}]}`, title))
	if r.code != 201 {
		e.t.Fatalf("create: %d %s", r.code, r.body)
	}
	var it struct{ Id string }
	r.json(e.t, &it)
	path = workPath(tenant, project) + "/" + it.Id
	etag = r.header.Get("ETag")
	for _, body := range []string{`{"to":"ready"}`, `{"to":"active"}`, `{"to":"review","review":{"candidateCommit":"` + commit + `","evidence":[{"type":"commit","id":"` + commit + `"}]}}`} {
		r = e.do(engineerToken, http.MethodPost, path+"/transitions", body, "If-Match", etag)
		if r.code != 200 {
			e.t.Fatalf("transition %s: %d %s", body, r.code, r.body)
		}
		etag = r.header.Get("ETag")
	}
	return path, etag
}

const completion = `{"to":"done","reason":"accepted","completion":{"contentRevision":1,"candidateCommit":"` + commit + `","acceptedCriteria":["AC-1"]}}`

func (e *env) complete(token, path, etag string) response {
	return e.do(token, http.MethodPost, path+"/transitions", completion, "If-Match", etag)
}

func (e *env) itemState(path string) string {
	e.t.Helper()
	var state string
	err := e.stack.Owner.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, path[strings.LastIndex(path, "/")+1:]).Scan(&state)
	if err != nil {
		e.t.Fatal(err)
	}
	return state
}

func domainTenant(s string) domain.TenantID { return domain.TenantID(s) }

func decodeSegment(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
