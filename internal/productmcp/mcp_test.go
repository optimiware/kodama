package productmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/domain"
)

// tokenAuth is a table authenticator: bearer -> caller.
type tokenAuth struct {
	m    map[string]Caller
	down bool
}

func (a *tokenAuth) Authenticate(_ context.Context, bearer string) (Caller, error) {
	if a.down {
		return Caller{}, errors.New("identity authority down")
	}
	c, ok := a.m[bearer]
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	return c, nil
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func serve(t *testing.T, f *fixture, auth Authenticator) *httptest.Server {
	t.Helper()
	h, err := NewHandler(HandlerConfig{Service: f.svc, Authenticator: auth, MaxRequestBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func connect(t *testing.T, srv *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cl := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerTransport{token}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and returns the structured result or the error text.
func call(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) (errText string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "rpc: " + err.Error()
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}
	if out != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	return ""
}

func toolNames(t *testing.T, cs *mcp.ClientSession) ([]string, map[string]*mcp.Tool) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	by := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		by[tl.Name] = tl
	}
	sort.Strings(names)
	return names, by
}

func Test_unit_productmcp_tools_listed(t *testing.T) {
	f := newFixture(t)
	engineer := personCaller(tenantA, "erin")
	attempt := attemptCaller(tenantA, projP, "att1")
	auth := &tokenAuth{m: map[string]Caller{"erin-token": engineer, "att-token": attempt}}
	srv := serve(t, f, auth)

	want := []string{
		ToolProjectCreate, ToolProjectGet, ToolProjectList, ToolProvisioningStart, ToolRepositoryCreate, ToolRepositoryGet,
		ToolRepositoryList, ToolRepositorySeed, ToolWorkItemCreate, ToolWorkItemGet, ToolWorkItemList,
	}
	sort.Strings(want)

	names, tools := toolNames(t, connect(t, srv, "erin-token"))
	if !slices.Equal(names, want) {
		t.Fatalf("tools listed for a person:\n got %v\nwant %v", names, want)
	}
	listed := ToolNames(engineer)
	sort.Strings(listed)
	if !slices.Equal(listed, want) {
		t.Fatalf("ToolNames disagrees with the server: %v", listed)
	}
	for name, tl := range tools {
		if !strings.HasPrefix(name, "kodama_") || tl.Description == "" || tl.InputSchema == nil || tl.OutputSchema == nil || tl.Annotations == nil {
			t.Errorf("%s: incomplete definition: %+v", name, tl)
		}
		if tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint || tl.Annotations.OpenWorldHint == nil || *tl.Annotations.OpenWorldHint {
			t.Errorf("%s: tools never destroy and never reach an open world", name)
		}
		schema := schemaOf(t, tl)
		creates := strings.HasSuffix(name, "_create") || name == ToolProvisioningStart
		_, hasKey := schema.Properties["idempotencyKey"]
		if creates != hasKey || (creates && !slices.Contains(schema.Required, "idempotencyKey")) {
			t.Errorf("%s: creating tools, and only they, require an idempotencyKey (creates=%v)", name, creates)
		}
		if strings.HasSuffix(name, "_get") || strings.HasSuffix(name, "_list") {
			if !tl.Annotations.ReadOnlyHint {
				t.Errorf("%s: a read tool must be read-only", name)
			}
		} else if tl.Annotations.ReadOnlyHint {
			t.Errorf("%s: a mutating tool is not read-only", name)
		}
		for _, banned := range []string{"tenantId", "tenant", "actor", "principal", "userId", "token", "credential", "seed"} {
			if _, bad := schema.Properties[banned]; bad {
				t.Errorf("%s: argument %q would let the caller choose authority", name, banned)
			}
		}
	}

	// A run's attempt is offered only the tools of its own project.
	names, _ = toolNames(t, connect(t, srv, "att-token"))
	wantAttempt := []string{ToolRepositoryCreate, ToolRepositoryGet, ToolRepositoryList, ToolWorkItemCreate, ToolWorkItemGet, ToolWorkItemList}
	sort.Strings(wantAttempt)
	if !slices.Equal(names, wantAttempt) {
		t.Fatalf("tools listed for an attempt: %v", names)
	}
	// Offering is not authorization, and hiding is not the only defence: a
	// hidden tool called anyway is refused.
	if msg := call(t, connect(t, srv, "att-token"), ToolProjectCreate, map[string]any{"idempotencyKey": "k", "key": "x"}, nil); msg == "" {
		t.Fatal("an attempt called a tool it is not offered")
	}

	// Authentication happens on every request.
	for name, tok := range map[string]string{"missing": "", "unknown": "nope"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s bearer: status %d", name, resp.StatusCode)
		}
	}
	auth.down = true
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer erin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an authentication outage is unavailable, got %d", resp.StatusCode)
	}
}

type schemaView struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

func schemaOf(t *testing.T, tl *mcp.Tool) schemaView {
	t.Helper()
	b, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var v schemaView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func Test_unit_productmcp_create_project_repository_task(t *testing.T) {
	f := newFixture(t)
	admin := personCaller(tenantA, "ada")
	eng := personCaller(tenantA, "erin")
	f.w.grant(admin, "*", "admin")
	auth := &tokenAuth{m: map[string]Caller{"ada": admin, "erin": eng}}
	srv := serve(t, f, auth)
	ada, erin := connect(t, srv, "ada"), connect(t, srv, "erin")

	// AC-A12: an agent creates a project, a repository and tasks through tools.
	var project ProjectOut
	if msg := call(t, ada, ToolProjectCreate, ProjectCreateIn{IdempotencyKey: "proj-1", Key: "shop", DisplayName: "Shop"}, &project); msg != "" {
		t.Fatal(msg)
	}
	if project.ID == "" || project.Key != "shop" || project.Replayed {
		t.Fatalf("project: %+v", project)
	}
	// The same call fails without the role; and succeeds once it is granted.
	var repo RepositoryOut
	args := RepositoryCreateIn{ProjectID: project.ID, IdempotencyKey: "repo-1", Name: "backend"}
	if msg := call(t, erin, ToolRepositoryCreate, args, &repo); !strings.Contains(msg, "not_found") {
		t.Fatalf("a caller with no binding must not see the project: %q", msg)
	}
	f.w.grant(eng, domain.ProjectID(project.ID), "engineer")
	if msg := call(t, erin, ToolRepositoryCreate, args, &repo); msg != "" {
		t.Fatal(msg)
	}
	if repo.Name != "backend" || repo.DefaultBranch != "main" || repo.Hosting != "internal" || repo.Status != "ready" || !repo.Empty || repo.DirectPushToDefault {
		t.Fatalf("repository (omitted policy denies direct pushes): %+v", repo)
	}
	if strings.Contains(repo.CloneURL, "@") {
		t.Fatalf("the clone URL carries no credential: %s", repo.CloneURL)
	}

	var got RepositoryOut
	if msg := call(t, erin, ToolRepositoryGet, RepositoryGetIn{ProjectID: project.ID, RepositoryID: repo.ID}, &got); msg != "" || got.ID != repo.ID {
		t.Fatalf("get: %q %+v", msg, got)
	}
	var list RepositoryListOut
	if msg := call(t, erin, ToolRepositoryList, RepositoryListIn{ProjectID: project.ID}, &list); msg != "" || len(list.Items) != 1 {
		t.Fatalf("list: %q %+v", msg, list)
	}

	// Seed guidance: how to push, with the contract's one-time credential.
	var seed SeedOut
	if msg := call(t, erin, ToolRepositorySeed, SeedIn{ProjectID: project.ID, RepositoryID: repo.ID}, &seed); msg != "" {
		t.Fatal(msg)
	}
	if seed.Password != seedPassword || seed.Username != "kodama" || seed.GrantID == "" || seed.ExpiresAt == "" || seed.CloneURL != repo.CloneURL || seed.DefaultBranch != "main" {
		t.Fatalf("seed: %+v", seed)
	}
	text := strings.Join(seed.Instructions, "\n")
	for _, must := range []string{"git remote add kodama " + repo.CloneURL, "refs/heads/main", "seed_only", "never put the password"} {
		if !strings.Contains(text, must) {
			t.Errorf("instructions lack %q:\n%s", must, text)
		}
	}
	if strings.Contains(text, seed.Password) {
		t.Fatal("the instructions must not repeat the credential")
	}

	// Tasks: a task by default, listed and read back.
	var item WorkItemOut
	if msg := call(t, erin, ToolWorkItemCreate, WorkItemCreateIn{ProjectID: project.ID, IdempotencyKey: "task-1", Title: "Add search", Spec: "Search the catalogue.",
		AcceptanceCriteria: []WorkItemCriterion{{ID: "c1", Text: "finds items"}}}, &item); msg != "" {
		t.Fatal(msg)
	}
	if item.Kind != "task" || item.State != "backlog" || item.Version != 1 {
		t.Fatalf("work item: %+v", item)
	}
	var items WorkItemListOut
	if msg := call(t, erin, ToolWorkItemList, WorkItemListIn{ProjectID: project.ID}, &items); msg != "" || len(items.Items) != 1 {
		t.Fatalf("list: %q %+v", msg, items)
	}
	var one WorkItemOut
	if msg := call(t, erin, ToolWorkItemGet, WorkItemGetIn{ProjectID: project.ID, WorkItemID: item.ID}, &one); msg != "" || one.ID != item.ID {
		t.Fatalf("get: %q %+v", msg, one)
	}
	var plist ProjectListOut
	if msg := call(t, erin, ToolProjectList, ProjectListIn{}, &plist); msg != "" || len(plist.Items) != 1 || plist.Items[0].ID != project.ID {
		t.Fatalf("a person sees only the projects bound to them: %q %+v", msg, plist)
	}

	// The seed is refused where the contract refuses it, before any grant.
	f.w.mu.Lock()
	f.w.repos[repo.ID].repo.Empty = false
	f.w.mu.Unlock()
	before := f.w.seeds
	if msg := call(t, erin, ToolRepositorySeed, SeedIn{ProjectID: project.ID, RepositoryID: repo.ID}, nil); !strings.Contains(msg, "repository_not_empty") {
		t.Fatalf("a repository with refs cannot be seeded: %q", msg)
	}
	f.w.mu.Lock()
	f.w.repos[repo.ID].repo.Empty = true
	f.w.repos[repo.ID].repo.Status = "provisioning"
	f.w.mu.Unlock()
	if msg := call(t, erin, ToolRepositorySeed, SeedIn{ProjectID: project.ID, RepositoryID: repo.ID}, nil); !strings.Contains(msg, "repository_not_ready") {
		t.Fatalf("a provisioning repository cannot be seeded: %q", msg)
	}
	if f.w.seeds != before {
		t.Fatal("a refused seed request minted a grant")
	}

	// A conflicting name surfaces the contract's stable code.
	if msg := call(t, erin, ToolRepositoryCreate, RepositoryCreateIn{ProjectID: project.ID, IdempotencyKey: "repo-2", Name: "backend"}, nil); !strings.Contains(msg, "repository_name_taken") {
		t.Fatalf("name reuse: %q", msg)
	}
	// Invalid arguments never reach a port.
	calls := len(f.w.sawCalls)
	for _, bad := range []RepositoryCreateIn{
		{ProjectID: project.ID, IdempotencyKey: "k", Name: "Bad Name"},
		{ProjectID: project.ID, IdempotencyKey: "k", Name: "ok", DefaultBranch: ptr("refs/heads/main")},
		{ProjectID: project.ID, IdempotencyKey: "bad key!", Name: "ok"},
		{IdempotencyKey: "k", Name: "ok"},
	} {
		if msg := call(t, erin, ToolRepositoryCreate, bad, nil); msg == "" {
			t.Errorf("accepted %+v", bad)
		}
	}
	if len(f.w.sawCalls) != calls {
		t.Fatal("an invalid request reached a port")
	}
}

func Test_unit_productmcp_handler_binds_caller(t *testing.T) {
	f := newFixture(t)
	eng, viewer := personCaller(tenantA, "erin"), personCaller(tenantA, "vic")
	f.w.grant(eng, projP, "engineer")
	f.w.grant(viewer, projP, "viewer")
	srv := serve(t, f, &tokenAuth{m: map[string]Caller{"eng": eng, "viewer": viewer}})
	e, v := connect(t, srv, "eng"), connect(t, srv, "viewer")
	create := RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "svc"}

	// The engineer's connection does not lend its authority to the viewer's.
	if msg := call(t, e, ToolRepositoryCreate, create, nil); msg != "" {
		t.Fatal(msg)
	}
	create.IdempotencyKey, create.Name = "k2", "svc2"
	if msg := call(t, v, ToolRepositoryCreate, create, nil); !strings.Contains(msg, "forbidden") {
		t.Fatalf("viewer: %q", msg)
	}
	if len(f.w.repos) != 1 {
		t.Fatal("a viewer created a repository")
	}
	// An argument cannot choose the tenant, the actor or an origin.
	for _, extra := range []string{"tenantId", "actor", "origin"} {
		args := map[string]any{"projectId": string(projP), "idempotencyKey": "k3", "name": "svc3", extra: "x"}
		if msg := call(t, e, ToolRepositoryCreate, args, nil); msg == "" {
			t.Errorf("accepted an extra %q argument", extra)
		}
	}
	// Failures carry a stable code and constant text only.
	f.w.failAuthority = true
	msg := call(t, e, ToolRepositoryGet, RepositoryGetIn{ProjectID: string(projP), RepositoryID: "repo_0001"}, nil)
	if !strings.HasPrefix(msg, "unavailable:") || strings.Contains(msg, "authority down") {
		t.Fatalf("%q", msg)
	}
}
