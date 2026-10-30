package productmcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/domain"
)

// Only a provisioning attempt may be a workload caller.
func Test_unit_productmcp_workload_caller_only_provisioning(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, origin := range []string{"", "work_item", "push", "manual", "provisioning"} {
		a := attemptCaller(tenantA, projP, "att1")
		a.RunOrigin = origin
		if err := a.Validate(); err == nil {
			t.Errorf("origin %q accepted", origin)
		}
		before := len(f.w.sawCalls)
		_, err := f.svc.ListRepositories(ctx, a, RepositoryListIn{})
		mustCode(t, err, "invalid_credential")
		if len(f.w.sawCalls) != before {
			t.Fatalf("a %q attempt reached a port", origin)
		}
	}
	if err := attemptCaller(tenantA, projP, "att1").Validate(); err != nil {
		t.Fatal(err)
	}
	// Malformed identities are refused by the contract's own actor rules, so no
	// two workloads can share an idempotency principal such as "run:".
	empty, odd, longID := "", "run id/../x", strings.Repeat("a", 129)
	mutate := map[string]func(*Caller){
		"empty run":      func(c *Caller) { c.Actor.RunID = &empty },
		"odd run":        func(c *Caller) { c.Actor.RunID = &odd },
		"long run":       func(c *Caller) { c.Actor.RunID = &longID },
		"empty attempt":  func(c *Caller) { c.Actor.AttemptID = &empty },
		"odd attempt":    func(c *Caller) { c.Actor.AttemptID = &odd },
		"empty actor id": func(c *Caller) { c.Actor.ID = "" },
		"odd actor id":   func(c *Caller) { c.Actor.ID = "att 1;" },
		"unknown type":   func(c *Caller) { c.Actor.Type = "service" },
		"no run":         func(c *Caller) { c.Actor.RunID = nil },
		"no attempt":     func(c *Caller) { c.Actor.AttemptID = nil },
		"bad project":    func(c *Caller) { c.Project = "p q" },
		"no project":     func(c *Caller) { c.Project = "" },
	}
	auth := &tokenAuth{m: map[string]Caller{}}
	for name, mut := range mutate {
		c := attemptCaller(tenantA, projP, "att1")
		mut(&c)
		auth.m[name] = c
		before := len(f.w.sawCalls)
		_, err := f.svc.CreateWorkItem(ctx, c, WorkItemCreateIn{IdempotencyKey: "k", Title: "t"})
		mustCode(t, err, "invalid_credential")
		if len(f.w.sawCalls) != before {
			t.Fatalf("%s: reached a port", name)
		}
	}
	userMutate := map[string]func(*Caller){
		"empty user":   func(c *Caller) { c.Actor.ID = "" },
		"odd user":     func(c *Caller) { c.Actor.ID = "erin smith" },
		"user run":     func(c *Caller) { c.Actor.RunID = &odd },
		"user attempt": func(c *Caller) { c.Actor.AttemptID = &empty },
	}
	for name, mut := range userMutate {
		c := personCaller(tenantA, "erin")
		mut(&c)
		auth.m["u "+name] = c
		before := len(f.w.sawCalls)
		_, err := f.svc.ListRepositories(ctx, c, RepositoryListIn{ProjectID: string(projP)})
		mustCode(t, err, "invalid_credential")
		if len(f.w.sawCalls) != before {
			t.Fatalf("%s: reached a port", name)
		}
	}
	msrv := serve(t, f, auth)
	for name := range auth.m {
		req, _ := http.NewRequest(http.MethodPost, msrv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+name)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	if len(f.w.sawCalls) != 0 {
		t.Fatal("a malformed principal reached a port")
	}

	person := personCaller(tenantA, "erin")
	person.RunOrigin = OriginProvision
	if person.Validate() == nil {
		t.Error("a person carries no run origin")
	}

	// The handler refuses an authenticator that mints one: 401, no server built.
	bad := attemptCaller(tenantA, projP, "att2")
	bad.RunOrigin = "work_item"
	srv := serve(t, f, &tokenAuth{m: map[string]Caller{"bad": bad}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer bad")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The provisioning contract: replay before budget, whole-declaration compare,
// terminal runs free the budget, unknown or foreign runs are not found.
func Test_unit_productmcp_provisioning_replay_before_budget(t *testing.T) {
	ctx := context.Background()
	eng := personCaller(tenantA, "erin")
	f := newFixture(t)
	f.w.grant(eng, projP, "engineer")
	lim := DefaultLimits()
	lim.MaxActiveProvisioning = 2
	mk := func(res Results) *Service {
		s, err := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: res, Limits: lim})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	in := func(key string) ProvisioningStartIn {
		return ProvisioningStartIn{ProjectID: string(projP), IdempotencyKey: key, DefinitionID: "def_bootstrap", Repository: "backend", Inputs: map[string]string{"stack": "go"}}
	}
	svc := mk(f.res)
	r0, err := svc.StartProvisioningRun(ctx, eng, in("k0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartProvisioningRun(ctx, eng, in("k1")); err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartProvisioningRun(ctx, eng, in("k2"))
	mustCode(t, err, "budget_exhausted")

	// The idempotency record is lost: the retry at budget exhaustion is still a
	// replay, answered by the port from the whole stored declaration.
	lost := mk(NewMemoryResults())
	again, err := lost.StartProvisioningRun(ctx, eng, in("k0"))
	if err != nil || again.ID != r0.ID || len(f.w.runs) != 2 {
		t.Fatalf("replay at exhaustion: %v %+v runs=%d", err, again, len(f.w.runs))
	}
	// Another declaration under that run is a conflict, never a second run.
	changed := in("k0")
	changed.Inputs = map[string]string{"stack": "rust"}
	_, err = mk(NewMemoryResults()).StartProvisioningRun(ctx, eng, changed)
	mustCode(t, err, "idempotency_conflict")
	changed = in("k0")
	changed.Repository = "frontend"
	_, err = mk(NewMemoryResults()).StartProvisioningRun(ctx, eng, changed)
	mustCode(t, err, "idempotency_conflict")
	// The same code when the tool layer's record survived.
	changed.Inputs = nil
	_, err = svc.StartProvisioningRun(ctx, eng, changed)
	mustCode(t, err, "idempotency_conflict")
	if len(f.w.runs) != 2 {
		t.Fatalf("%d runs", len(f.w.runs))
	}

	// A terminal run is no longer active.
	f.w.mu.Lock()
	rk := key(domain.Scope{TenantID: tenantA, ProjectID: projP}) + "/" + string(r0.ID)
	r := f.w.runs[rk]
	r.Status = "succeeded"
	f.w.runs[rk] = r
	f.w.mu.Unlock()
	if _, err := svc.StartProvisioningRun(ctx, eng, in("k2")); err != nil {
		t.Fatalf("a terminal run frees its slot: %v", err)
	}

	// The cap is the project's, not the caller's: k1 and k2 are active, so
	// another engineer cannot start a third.
	other := personCaller(tenantA, "eli")
	f.w.grant(other, projP, "engineer")
	_, err = svc.StartProvisioningRun(ctx, other, in("k-other"))
	mustCode(t, err, "budget_exhausted")
	// ...but their own run identity is not another's: a RunID taken by someone
	// else's run is not found, never replayed, overwritten or disclosed.
	sc0 := domain.Scope{TenantID: tenantA, ProjectID: projP}
	k1 := string(runIdentity(eng, projP, "k1"))
	_, err = fakeProv{f.w}.Start(ctx, other, sc0, StartProvisioning{RunID: domain.RunID(k1), DefinitionID: "def_other", MaxActive: 99})
	if err != ErrNotFound {
		t.Fatalf("another creator's RunID: %v", err)
	}
	if got := f.w.runs[key(sc0)+"/"+k1]; got.DefinitionID != "def_bootstrap" || f.w.runBy[key(sc0)+"/"+k1] != principalOf(eng) {
		t.Fatalf("another creator's run was touched: %+v", got)
	}

	// Get: unknown, foreign-project and foreign-tenant runs are not found.
	p := fakeProv{f.w}
	sc := domain.Scope{TenantID: tenantA, ProjectID: projP}
	if _, err := p.Get(ctx, eng, sc, "run_nope"); err != ErrNotFound {
		t.Fatalf("unknown: %v", err)
	}
	f.w.grant(eng, projQ, "engineer")
	if _, err := p.Get(ctx, eng, domain.Scope{TenantID: tenantA, ProjectID: projQ}, domain.RunID(r0.ID)); err != ErrNotFound {
		t.Fatalf("foreign project: %v", err)
	}
	if _, err := p.Get(ctx, eng, domain.Scope{TenantID: tenantB, ProjectID: projP}, domain.RunID(r0.ID)); err != ErrNotFound {
		t.Fatalf("foreign tenant: %v", err)
	}
}

// A repository an attempt creates records its creating run; a replacement
// attempt of the same run converges on the original instead of colliding.
func Test_unit_productmcp_run_created_repositories_and_replacement_attempts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a1 := attemptOfRun(tenantA, projP, "run_1", "att_1")
	a2 := attemptOfRun(tenantA, projP, "run_1", "att_2") // replacement attempt of the same run
	other := attemptOfRun(tenantA, projP, "run_2", "att_3")
	in := RepositoryCreateIn{IdempotencyKey: "k", Name: "backend"}

	r1, err := f.svc.CreateRepository(ctx, a1, in)
	if err != nil {
		t.Fatal(err)
	}
	if f.w.repoRun[r1.ID] != "run_1" || f.w.repoAtt[r1.ID] != "att_1" {
		t.Fatalf("the creating run was not recorded: %q %q", f.w.repoRun[r1.ID], f.w.repoAtt[r1.ID])
	}
	// The replacement attempt retries the same create.
	r2, err := f.svc.CreateRepository(ctx, a2, in)
	if err != nil || r2.ID != r1.ID || !r2.Replayed {
		t.Fatalf("replacement attempt: %v %+v", err, r2)
	}
	// Even when the tool-layer record is lost, the use case's key (principal =
	// the run) converges instead of answering repository_name_taken.
	lost, _ := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: NewMemoryResults()})
	r3, err := lost.CreateRepository(ctx, a2, in)
	if err != nil || r3.ID != r1.ID {
		t.Fatalf("lost record: %v %+v", err, r3)
	}
	if f.w.repoAtt[r1.ID] != "att_1" || len(f.w.repos) != 1 {
		t.Fatal("a replay changed the creating attempt or created again")
	}
	// Another run is another principal: same name collides, same key does not replay.
	_, err = f.svc.CreateRepository(ctx, other, in)
	mustCode(t, err, "repository_name_taken")
	it1, err := f.svc.CreateWorkItem(ctx, a1, WorkItemCreateIn{IdempotencyKey: "w", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	it2, err := f.svc.CreateWorkItem(ctx, other, WorkItemCreateIn{IdempotencyKey: "w", Title: "t"})
	if err != nil || it2.ID == it1.ID || it2.Replayed {
		t.Fatalf("another run must not replay: %v %+v", err, it2)
	}
	// A person's create records no run.
	eng := personCaller(tenantA, "erin")
	f.w.grant(eng, projP, "engineer")
	pr, err := f.svc.CreateRepository(ctx, eng, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.w.repoRun[pr.ID]; ok {
		t.Fatal("a person-created repository carries a run")
	}
}

// capture records every response of the client: status and headers.
type capture struct {
	token string
	mu    sync.Mutex
	seen  []*http.Response
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		c.mu.Lock()
		c.seen = append(c.seen, &http.Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Close: resp.Close})
		c.mu.Unlock()
	}
	return resp, err
}

func Test_unit_productmcp_responses_are_not_cacheable(t *testing.T) {
	f := newFixture(t)
	eng := personCaller(tenantA, "erin")
	f.w.grant(eng, projP, "engineer")
	srv := serve(t, f, &tokenAuth{m: map[string]Caller{"erin": eng}})

	// Raw requests: success, unauthenticated and a method the server refuses.
	do := func(method, token, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	for name, resp := range map[string]*http.Response{
		"ok":           do(http.MethodPost, "erin", init),
		"unauthorized": do(http.MethodPost, "", init),
		"get":          do(http.MethodGet, "erin", ""),
	} {
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s (%d): headers %v", name, resp.StatusCode, resp.Header)
		}
	}

	// A real session: every response of create, get and the seed call, which
	// carries a live credential, is captured at the client's transport.
	rec := &capture{token: "erin"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL, HTTPClient: &http.Client{Transport: rec}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var r RepositoryOut
	if msg := call(t, cs, ToolRepositoryCreate, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "svc"}, &r); msg != "" {
		t.Fatal(msg)
	}
	before := len(rec.seen)
	var seed SeedOut
	if msg := call(t, cs, ToolRepositorySeed, SeedIn{ProjectID: string(projP), RepositoryID: r.ID}, &seed); msg != "" || seed.Password == "" {
		t.Fatalf("seed: %q", msg)
	}
	if len(rec.seen) <= before {
		t.Fatal("the seed call's response was not captured")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, resp := range rec.seen {
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("response %d (%d): headers %v", i, resp.StatusCode, resp.Header)
		}
	}
}

// An oversize body is refused, and the server is told to close the connection:
// MaxBytesReader must be handed the server's own writer, not the wrapper.
func Test_unit_productmcp_oversize_body_refused(t *testing.T) {
	f := newFixture(t)
	eng := personCaller(tenantA, "erin")
	h, err := NewHandler(HandlerConfig{Service: f.svc, Authenticator: &tokenAuth{m: map[string]Caller{"erin": eng}}, MaxRequestBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"pad":"` + strings.Repeat("a", 4096) + `"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer erin")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !resp.Close {
		t.Errorf("the connection is not marked for closing: %v", resp.Header)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", resp.Header)
	}
	if len(f.w.sawCalls) != 0 {
		t.Fatal("an oversize request reached a port")
	}
}

func Test_unit_productmcp_content_bounds_and_list_limits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	eng, ada := personCaller(tenantA, "erin"), personCaller(tenantA, "ada")
	f.w.grant(eng, projP, "engineer")
	f.w.grant(ada, "*", "admin")
	lim := DefaultLimits()
	item := func(mut func(*WorkItemCreateIn)) error {
		in := WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "t"}
		mut(&in)
		_, err := f.svc.CreateWorkItem(ctx, eng, in)
		return err
	}
	cases := map[string]func(*WorkItemCreateIn){
		"title":       func(in *WorkItemCreateIn) { in.Title = strings.Repeat("x", lim.MaxTitleChars+1) },
		"title bytes": func(in *WorkItemCreateIn) { in.Title = strings.Repeat("é", lim.MaxTitleChars*4/2+1) },
		"spec":        func(in *WorkItemCreateIn) { in.Spec = strings.Repeat("x", lim.MaxSpecBytes+1) },
		"criterion": func(in *WorkItemCreateIn) {
			in.AcceptanceCriteria = []WorkItemCriterion{{ID: "c", Text: strings.Repeat("x", lim.MaxCriterionChars+1)}}
		},
		"criterion id": func(in *WorkItemCreateIn) {
			in.AcceptanceCriteria = []WorkItemCriterion{{ID: strings.Repeat("c", lim.MaxCriterionIDChars+1), Text: "x"}}
		},
		"criteria": func(in *WorkItemCreateIn) { in.AcceptanceCriteria = make([]WorkItemCriterion, lim.MaxCriteria+1) },
		"control":  func(in *WorkItemCreateIn) { in.Title = "a\x00b" },
		"utf8":     func(in *WorkItemCreateIn) { in.Spec = "a\xffb" },
	}
	for name, mut := range cases {
		mustCode(t, item(mut), "invalid_argument")
		_ = name
	}
	mustCode(t, item(func(in *WorkItemCreateIn) { in.AcceptanceCriteria = []WorkItemCriterion{{ID: "", Text: "x"}} }), "invalid_request")
	if len(f.w.sawCalls) != 0 {
		t.Fatalf("an out-of-bounds item reached a port: %v", f.w.sawCalls)
	}
	// At the bound is accepted: characters, not bytes, count for the title.
	if err := item(func(in *WorkItemCreateIn) {
		in.Title = strings.Repeat("é", lim.MaxTitleChars)
		in.Spec = "line one\nline two\ttabbed"
	}); err != nil {
		t.Fatalf("a title at the bound: %v", err)
	}

	for name, in := range map[string]ProjectCreateIn{
		"key":         {IdempotencyKey: "k", Key: strings.Repeat("k", lim.MaxProjectKeyChars+1)},
		"displayName": {IdempotencyKey: "k", Key: "ok", DisplayName: strings.Repeat("d", lim.MaxDisplayNameChars+1)},
		"control":     {IdempotencyKey: "k", Key: "ok", DisplayName: "a\x07"},
	} {
		if _, err := f.svc.CreateProject(ctx, ada, in); err == nil {
			t.Errorf("%s accepted", name)
		} else {
			mustCode(t, err, "invalid_argument")
		}
	}
	for _, c := range f.w.sawCalls {
		if c == "projects.Create" {
			t.Fatal("an out-of-bounds project reached a port")
		}
	}

	// A port that returns more than was asked for is not trusted.
	if _, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "second", Title: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateRepository(ctx, eng, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "r", Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	f.w.listExtra = 1
	_, err := f.svc.ListWorkItems(ctx, eng, WorkItemListIn{ProjectID: string(projP), Limit: 2})
	mustCode(t, err, "unavailable")
	f.w.listExtra = lim.MaxPage + 1
	_, err = f.svc.ListRepositories(ctx, eng, RepositoryListIn{ProjectID: string(projP)})
	mustCode(t, err, "unavailable")
	_, err = f.svc.ListProjects(ctx, eng, ProjectListIn{})
	mustCode(t, err, "unavailable")
	f.w.listExtra = 0
	if l, err := f.svc.ListWorkItems(ctx, eng, WorkItemListIn{ProjectID: string(projP), Limit: 2}); err != nil || len(l.Items) != 2 {
		t.Fatalf("%v %+v", err, l)
	}
}

// An omitted default branch or policy is the same request as spelling it out.
func Test_unit_productmcp_idempotency_hash_is_normalized(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	eng := personCaller(tenantA, "erin")
	f.w.grant(eng, projP, "engineer")
	base := RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "svc"}
	r1, err := f.svc.CreateRepository(ctx, eng, base)
	if err != nil {
		t.Fatal(err)
	}
	explicit := base
	explicit.DefaultBranch, explicit.DirectPushToDefault = ptr("main"), ptr(false)
	r2, err := f.svc.CreateRepository(ctx, eng, explicit)
	if err != nil || r2.ID != r1.ID || !r2.Replayed {
		t.Fatalf("%v %+v", err, r2)
	}
	explicit.DirectPushToDefault = ptr(true)
	_, err = f.svc.CreateRepository(ctx, eng, explicit)
	mustCode(t, err, "idempotency_conflict")
}
