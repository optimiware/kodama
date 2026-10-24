package logshttp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/logs"
	logshttp "github.com/optimiware/kodama/internal/logs/http"
	logskube "github.com/optimiware/kodama/internal/logs/kubernetes"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/internal/logs/logstest/kubefake"
)

func TestMain(m *testing.M) {
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t023")
	}
	os.Exit(m.Run())
}

func bg() context.Context { return context.Background() }

// rig is the whole stack under test: real identity, execution and artifact
// persistence, the real Kubernetes reader over the API fake, the real service
// and handler behind the shared protected router, served by net/http.
type rig struct {
	t    *testing.T
	env  *logstest.Env
	kube *kubefake.API
	svc  *logs.Service
	srv  *httptest.Server
}

type rigOptions struct {
	limits logs.Limits
	live   logs.LiveReader // replaces the real reader
	auth   logs.Authority  // replaces the module authority
	// routeAuth replaces the router's authorizer authority (identity outage).
	writeTimeout time.Duration
	// identity replaces the authority behind the router's authorizer.
	identity identity.Authority
}

func newRig(t *testing.T, a logstest.Attempt, env *logstest.Env, o rigOptions) *rig {
	t.Helper()
	r := &rig{t: t, env: env}
	r.kube = kubefake.New(t, logstest.Namespace, a.Job, logstest.JobUID, logstest.Container)
	var live logs.LiveReader = o.live
	if live == nil {
		reader, err := logskube.NewReader(logskube.Config{Endpoint: r.kube.URL(), Client: r.kube.Client(), AllowInsecure: true})
		if err != nil {
			t.Fatal(err)
		}
		live = reader
	}
	lim := o.limits
	if lim.MaxLines == 0 {
		lim = logstest.Limits()
	}
	var authority logs.Authority = env.Authority
	if o.auth != nil {
		authority = o.auth
	}
	svc, err := logs.NewService(logs.Config{Authority: authority, Attempts: env.Attempts, Workloads: env.Workloads, Live: live, Retained: env.Retained,
		Store: env.Logs, Redactions: env.Redact, Limits: lim, Clock: env.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	wt := o.writeTimeout
	if wt == 0 {
		wt = svc.Limits().WriteTimeout
	}
	h, err := logshttp.NewHandler(svc, wt)
	if err != nil {
		t.Fatal(err)
	}
	rules := identityhttp.DefaultRules()
	rules[logshttp.ActionLogs] = identityhttp.Rule{Level: identityhttp.LevelProject, Any: logshttp.RoutePermissions()[logshttp.ActionLogs]}
	idAuth := env.Identity
	if o.identity != nil {
		idAuth = o.identity
	}
	authz, err := identityhttp.NewAuthorizer(idAuth, rules)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearer{}, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(rt)
	t.Cleanup(r.srv.Close)
	r.svc = svc
	return r
}

// bearer authenticates "Authorization: Bearer <user id>": the fixture's
// stand-in for the OIDC authenticator, which is not under test here.
type bearer struct{}

func (bearer) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

func (r *rig) url(scope domain.Scope, run domain.RunID, query string) string {
	u := r.srv.URL + "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/runs/" + string(run) + "/logs"
	if query != "" {
		u += "?" + query
	}
	return u
}

func (r *rig) get(user string, scope domain.Scope, run domain.RunID, query string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.url(scope, run, query), nil)
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		r.t.Fatalf("GET %s: %v", query, err)
	}
	return resp
}

// result is a fully read response.
type result struct {
	status  int
	header  http.Header
	raw     string
	records []map[string]any
}

func (r *rig) read(user string, scope domain.Scope, run domain.RunID, query string) result {
	r.t.Helper()
	resp := r.get(user, scope, run, query)
	defer resp.Body.Close()
	return readAll(r.t, resp)
}

func readAll(t *testing.T, resp *http.Response) result {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	res := result{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	if err != nil {
		// A truncated body is a legitimate outcome of an aborted stream.
		res.raw = string(b)
	}
	if resp.StatusCode == http.StatusOK {
		sc := bufio.NewScanner(strings.NewReader(res.raw))
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		for sc.Scan() {
			var rec map[string]any
			if jerr := json.Unmarshal(sc.Bytes(), &rec); jerr != nil {
				t.Fatalf("a response line is not JSON: %q", sc.Text())
			}
			res.records = append(res.records, rec)
		}
	}
	return res
}

func (r result) meta(t *testing.T) map[string]any {
	t.Helper()
	if len(r.records) == 0 || r.records[0]["type"] != "meta" {
		t.Fatalf("the first record must be meta: %s", trunc(r.raw))
	}
	return r.records[0]
}

func (r result) summary(t *testing.T) map[string]any {
	t.Helper()
	if len(r.records) == 0 || r.records[len(r.records)-1]["type"] != "summary" {
		t.Fatalf("the last record must be the summary (its absence means interruption): %s", trunc(r.raw))
	}
	return r.records[len(r.records)-1]
}

func (r result) lines() []string {
	var out []string
	for _, rec := range r.records {
		if rec["type"] == "chunk" {
			out = append(out, rec["text"].(string))
		}
	}
	return out
}

func trunc(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// errorCode extracts the standard envelope's code and message.
func (r result) errorCode(t *testing.T) (string, string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.raw), &env); err != nil || env.Error.Code == "" {
		t.Fatalf("not an error envelope (status %d): %s", r.status, trunc(r.raw))
	}
	return env.Error.Code, env.Error.Message
}

// newRigWithEndpoint is a rig whose Kubernetes reader targets another endpoint
// (an unreachable one).
func newRigWithEndpoint(t *testing.T, a logstest.Attempt, env *logstest.Env, endpoint string) *rig {
	t.Helper()
	reader, err := logskube.NewReader(logskube.Config{Endpoint: endpoint, Client: http.DefaultClient, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return newRig(t, a, env, rigOptions{live: reader})
}
