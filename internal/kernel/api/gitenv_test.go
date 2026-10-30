package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/store"
)

const (
	tenantID  = domain.TenantID("t_acme")
	otherID   = domain.TenantID("t_other")
	projectID = domain.ProjectID("p_erp")

	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333"
	z0 = "0000000000000000000000000000000000000000"
)

// mapAuth authenticates a fixed set of bearer tokens.
type mapAuth map[string]api.Principal

func (m mapAuth) Authenticate(_ context.Context, r *http.Request) (api.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if p, found := m[token]; ok && found {
		return p, nil
	}
	return api.Principal{}, api.ErrUnauthenticated
}

// fakeWorkers stands in for the tenant's worker: it records the forwarded Git
// requests and answers the RPC with configurable behavior.
type fakeWorkers struct {
	mu          sync.Mutex
	forwarded   []*http.Request
	initErr     error
	ancestor    bool
	ancestorErr error
	handlerErr  error
	// onGit, when set, answers the forwarded request.
	onGit func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeWorkers) GitHandler(context.Context, domain.TenantID) (http.Handler, error) {
	if f.handlerErr != nil {
		return nil, f.handlerErr
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.forwarded = append(f.forwarded, r)
		f.mu.Unlock()
		if f.onGit != nil {
			f.onGit(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "worker")
	}), nil
}

func (f *fakeWorkers) Initialize(_ context.Context, _ domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error) {
	if f.initErr != nil {
		return gitv1.InitializeRepositoryResponse{}, f.initErr
	}
	return gitv1.InitializeRepositoryResponse{RepositoryID: req.RepositoryID, DefaultBranch: req.DefaultBranch, Empty: true, Created: true}, nil
}

func (f *fakeWorkers) ReadRefs(context.Context, domain.TenantID, gitv1.ReadRefsRequest) (gitv1.RefsResponse, error) {
	return gitv1.RefsResponse{Refs: []gitv1.RefTip{{Ref: "refs/heads/main", Commit: c1}}, Empty: false}, nil
}

func (f *fakeWorkers) IsAncestor(context.Context, domain.TenantID, gitv1.IsAncestorRequest) (gitv1.IsAncestorResponse, error) {
	return gitv1.IsAncestorResponse{IsAncestor: f.ancestor}, f.ancestorErr
}

func (f *fakeWorkers) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.forwarded...)
}

// env is one kernel Git hosting over the in-memory backend.
type env struct {
	t        *testing.T
	mem      *store.MemoryGit
	git      *api.GitServer
	srv      *httptest.Server
	workers  *fakeWorkers
	signer   *gitjwt.Signer
	verifier *gitjwt.Verifier
	now      time.Time
	nowMu    sync.Mutex
}

func (e *env) clock() time.Time {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.nowMu.Lock()
	e.now = e.now.Add(d)
	e.nowMu.Unlock()
}

func newSigner(t testing.TB, clock func() time.Time) *gitjwt.Signer {
	t.Helper()
	ring := func() *gitjwt.KeyRing {
		k, err := gitjwt.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		r, err := gitjwt.NewKeyRing(1, k)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	s, err := gitjwt.NewSigner(gitjwt.SignerConfig{Issuer: "kodama-git", Forwarding: ring(), Identity: ring(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Users of the fixture: an engineer, a viewer, a tenant-wide administrator, a
// person without a role and an engineer of another tenant.
var tokens = mapAuth{
	"tok-eng":     {UserID: "u_eng"},
	"tok-viewer":  {UserID: "u_viewer"},
	"tok-admin":   {UserID: "u_admin"},
	"tok-outside": {UserID: "u_outside"},
	"tok-foreign": {UserID: "u_foreign"},
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, mem: store.NewMemoryGit(), workers: &fakeWorkers{}, now: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	e.signer = newSigner(t, e.clock)
	e.verifier = e.signer.Verifier()
	e.mem.AddTenant(tenantID, "acme")
	e.mem.AddProject(tenantID, projectID, "erp")
	e.mem.AddProject(tenantID, "p_web", "web")
	e.mem.AddTenant(otherID, "other")
	e.mem.AddProject(otherID, "p_other", "erp")
	e.mem.Grant(tenantID, "u_eng", string(projectID), "engineer")
	e.mem.Grant(tenantID, "u_viewer", string(projectID), "viewer")
	e.mem.Grant(tenantID, "u_admin", "", "admin")
	e.mem.Grant(otherID, "u_foreign", "", "engineer")
	e.mem.SetClock(e.clock)
	git, err := api.NewGit(api.GitConfig{Store: e.mem, Auth: tokens, Workers: e.workers, Signer: e.signer, Identities: e.verifier,
		PublicURL: "https://kodama.example.invalid", Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	e.git = git
	e.srv = httptest.NewServer(git.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

type reply struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (e *env) do(method, path, token string, body any, headers ...string) reply {
	e.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: string(raw), header: resp.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func errCode(r reply) string {
	if m, ok := r.body["error"].(map[string]any); ok {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

func str(v any) string { s, _ := v.(string); return s }

// createRepo creates a repository through the API and returns its resource.
func (e *env) createRepo(token, projectSeg, name string, extra map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"name": name}
	for k, v := range extra {
		body[k] = v
	}
	path := "/api/v1/tenants/acme/repositories"
	if projectSeg != "" {
		path = "/api/v1/tenants/acme/projects/" + projectSeg + "/repositories"
	}
	r := e.do(http.MethodPost, path, token, body)
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		e.t.Fatalf("create %s: %d %s", name, r.status, r.raw)
	}
	return r.body
}

// credential exchanges a token for a Git credential.
func (e *env) credential(token string, repo map[string]any, seed bool) reply {
	e.t.Helper()
	body := map[string]any{"repositoryId": repo["id"], "seed": seed}
	if p, ok := repo["projectId"].(string); ok {
		body["projectId"] = p
	}
	return e.do(http.MethodPost, "/api/v1/tenants/acme/git-credentials", token, body)
}

func (e *env) mustCredential(token string, repo map[string]any, seed bool) (grantID, password string) {
	e.t.Helper()
	r := e.credential(token, repo, seed)
	if r.status != http.StatusCreated {
		e.t.Fatalf("credential: %d %s", r.status, r.raw)
	}
	return str(r.body["grantId"]), str(r.body["password"])
}

// gitGet sends a smart-HTTP request with a Basic credential.
func (e *env) gitGet(path, password string, method string) reply {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if password != "" {
		req.SetBasicAuth("whoever", password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: string(raw), header: resp.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (e *env) control() *api.GitControl { return e.git.Control() }

// authorize drives pre-receive's call for one receive made under a grant.
func (e *env) authorize(grantID, repoID string, updates []gitv1.RefUpdate, facts []gitv1.GitFact) (gitv1.PushAuthorizeResponse, error) {
	receive := gitv1.NewReceiveID(grantID)
	return e.authorizeAs(receive, grantID, repoID, updates, facts)
}

func (e *env) authorizeAs(receive, grantID, repoID string, updates []gitv1.RefUpdate, facts []gitv1.GitFact) (gitv1.PushAuthorizeResponse, error) {
	return e.control().Authorize(e.t.Context(), tenantID, gitv1.PushAuthorizeRequest{
		RepositoryID: repoID, GrantID: grantID, ReceiveID: receive, Updates: updates, PushOptions: []string{}, GitFacts: facts})
}

func create(ref, to string) gitv1.RefUpdate { return gitv1.RefUpdate{Ref: ref, Old: z0, New: to} }

func factsFor(empty bool, updates ...gitv1.RefUpdate) []gitv1.GitFact {
	yes := true
	out := make([]gitv1.GitFact, len(updates))
	for i, u := range updates {
		out[i] = gitv1.GitFact{Ref: u.Ref, RepositoryEmpty: empty}
		if !gitv1.IsZeroObjectID(u.Old) && !gitv1.IsZeroObjectID(u.New) {
			out[i].FastForward = &yes
		}
	}
	return out
}

func reasons(resp gitv1.PushAuthorizeResponse) []string {
	out := make([]string, len(resp.Updates))
	for i, v := range resp.Updates {
		out[i] = "allow"
		if v.Verdict == gitv1.VerdictDeny {
			out[i] = *v.Reason
		}
	}
	return out
}

func contractCode(err error) domain.ErrorCode {
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}
