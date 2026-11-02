package productmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// world is one in-memory installation behind every port. Its authority rules
// mirror the role table: they decide from the Caller the port receives and
// from nothing ambient, so a test proves what the tool layer passes down.
type world struct {
	mu sync.Mutex

	roles map[string]map[domain.ProjectID]string // principal -> project ("*" tenant-wide) -> role
	// failAuthority makes every authorization an outage.
	failAuthority bool

	projects map[string]Project // tenant/project
	repos    map[string]*repoRec
	items    map[string]WorkItem
	runs     map[string]ProvisioningRun
	runDecl  map[string]declaration // the whole declaration of each run
	runBy    map[string]string      // the creating principal of each run
	repoRun  map[string]string      // repository id -> creating run (attempt creates only)
	repoAtt  map[string]string      // repository id -> creating attempt
	keys     map[string]string      // port idempotency: scope/principal/key -> resource id
	seq      int

	seeds    int
	sawScope []domain.Scope
	sawCalls []string
	sawKeys  []string
	startReq []StartProvisioning
	lastCall Caller
	// gate, when set, runs at the start of every work item create, before the
	// world's lock.
	gate func()
	// listExtra makes list ports return that many entries beyond the request.
	listExtra int
}

// declaration is everything a provisioning run's admission pins from the request.
type declaration struct {
	Definition, Repository string
	Inputs                 string
}

func declOf(r StartProvisioning) declaration {
	b, _ := json.Marshal(r.Inputs) // map keys are sorted
	return declaration{string(r.DefinitionID), r.Repository, string(b)}
}

type repoRec struct {
	scope domain.Scope
	repo  gitv1.Repository
}

func newWorld() *world {
	return &world{
		roles: map[string]map[domain.ProjectID]string{}, projects: map[string]Project{}, repos: map[string]*repoRec{},
		items: map[string]WorkItem{}, runs: map[string]ProvisioningRun{}, runDecl: map[string]declaration{}, runBy: map[string]string{},
		repoRun: map[string]string{}, repoAtt: map[string]string{}, keys: map[string]string{},
	}
}

func (w *world) grant(c Caller, project domain.ProjectID, role string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := principalOf(c)
	if w.roles[p] == nil {
		w.roles[p] = map[domain.ProjectID]string{}
	}
	if role == "" {
		delete(w.roles[p], project)
		return
	}
	w.roles[p][project] = role
}

func (w *world) addProject(tenant domain.TenantID, id domain.ProjectID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.projects[string(tenant)+"/"+string(id)] = Project{ID: id, Key: string(id), DisplayName: string(id), ActiveWIPLimit: 3, Version: 1}
}

var rolePerms = map[string][]string{
	"admin":    {"project.create", "project.read", "repo.read", "work.read", "run.read"},
	"engineer": {"project.read", "repo.create", "repo.read", "repo.seed", "work.create", "work.read", "run.manage"},
	"analyst":  {"project.read", "repo.read", "work.create", "work.read"},
	"viewer":   {"project.read", "repo.read", "work.read"},
}

// allow answers like an authority: unknown scope or no binding is not_found, a
// binding without the permission is denied.
func (w *world) allow(c Caller, scope domain.Scope, perm string) error {
	w.sawScope = append(w.sawScope, scope)
	w.lastCall = c
	if w.failAuthority {
		return fmt.Errorf("authority down")
	}
	if scope.TenantID != "" && scope.TenantID != c.Tenant {
		return ErrNotFound
	}
	if scope.ProjectID != "*" {
		if _, ok := w.projects[string(scope.TenantID)+"/"+string(scope.ProjectID)]; !ok {
			return ErrNotFound
		}
	}
	if c.IsWorkload() {
		if scope.ProjectID != c.Project {
			return ErrNotFound
		}
		switch perm {
		case "repo.create", "repo.read", "work.create", "work.read":
			return nil
		}
		return ErrDenied
	}
	role, ok := w.roles[principalOf(c)][scope.ProjectID]
	if !ok {
		role, ok = w.roles[principalOf(c)]["*"]
	}
	if !ok && scope.ProjectID == "*" && len(w.roles[principalOf(c)]) > 0 {
		return ErrDenied // a member of the tenant without the tenant-level role
	}
	if !ok {
		return ErrNotFound
	}
	for _, p := range rolePerms[role] {
		if p == perm {
			return nil
		}
	}
	return ErrDenied
}

func (w *world) note(call string) { w.sawCalls = append(w.sawCalls, call) }

func key(scope domain.Scope) string { return string(scope.TenantID) + "/" + string(scope.ProjectID) }

// ---- Repositories ----

type fakeRepos struct{ w *world }

func (f fakeRepos) Create(_ context.Context, c Caller, scope domain.Scope, idem string, req gitv1.CreateRepositoryRequest) (gitv1.Repository, error) {
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.note("repos.Create")
	w.sawKeys = append(w.sawKeys, idem)
	if err := w.allow(c, scope, "repo.create"); err != nil {
		return gitv1.Repository{}, err
	}
	kk := key(scope) + "|" + principalOf(c) + "|" + idem
	if id, ok := w.keys[kk]; ok {
		return w.repos[id].repo, nil
	}
	for _, r := range w.repos {
		if r.scope == scope && r.repo.Name == req.Name {
			return gitv1.Repository{}, Refuse(ErrConflict, "repository_name_taken", "the name is used by another repository")
		}
	}
	w.seq++
	id := fmt.Sprintf("repo_%04d", w.seq)
	branch := req.EffectiveDefaultBranch()
	r := gitv1.Repository{
		ID: id, Name: req.Name, DefaultBranch: branch, Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady, Empty: true, Version: 1,
		CloneURL:  fmt.Sprintf("https://git.example.invalid/git/%s/%s/%s.git", scope.TenantID, scope.ProjectID, req.Name),
		Policy:    gitv1.EffectivePolicy(req.Policy),
		CreatedAt: gitv1.Timestamp("2026-10-30T10:00:00Z"),
	}
	w.repos[id] = &repoRec{scope: scope, repo: r}
	if c.IsWorkload() {
		w.repoRun[id], w.repoAtt[id] = *c.Actor.RunID, *c.Actor.AttemptID
	}
	w.keys[kk] = id
	return r, nil
}

func (f fakeRepos) rec(c Caller, scope domain.Scope, id, perm string) (*repoRec, error) {
	if err := f.w.allow(c, scope, perm); err != nil {
		return nil, err
	}
	r, ok := f.w.repos[id]
	if !ok || r.scope != scope {
		return nil, ErrNotFound
	}
	return r, nil
}

func (f fakeRepos) Get(_ context.Context, c Caller, scope domain.Scope, id string) (gitv1.Repository, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("repos.Get")
	r, err := f.rec(c, scope, id, "repo.read")
	if err != nil {
		return gitv1.Repository{}, err
	}
	return r.repo, nil
}

func (f fakeRepos) List(_ context.Context, c Caller, scope domain.Scope, _ *string) (gitv1.RepositoryList, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("repos.List")
	if err := f.w.allow(c, scope, "repo.read"); err != nil {
		return gitv1.RepositoryList{}, err
	}
	out := gitv1.RepositoryList{Items: []gitv1.Repository{}}
	for i := 1; i <= f.w.seq; i++ {
		if r, ok := f.w.repos[fmt.Sprintf("repo_%04d", i)]; ok && r.scope == scope {
			out.Items = append(out.Items, r.repo)
		}
	}
	for i := 0; i < f.w.listExtra && len(out.Items) > 0; i++ {
		out.Items = append(out.Items, out.Items[0])
	}
	return out, nil
}

const seedPassword = "kgc_seed_0123456789abcdefghijklmnop"

func (f fakeRepos) RequestSeed(_ context.Context, c Caller, scope domain.Scope, id string) (gitv1.CredentialResponse, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("repos.RequestSeed")
	if _, err := f.rec(c, scope, id, "repo.seed"); err != nil {
		return gitv1.CredentialResponse{}, err
	}
	f.w.seeds++
	return gitv1.CredentialResponse{Username: "kodama", Password: seedPassword, GrantID: fmt.Sprintf("gg_%d", f.w.seeds), ExpiresAt: "2026-10-30T10:15:00Z"}, nil
}

// ---- Projects ----

type fakeProjects struct{ w *world }

func (f fakeProjects) Create(_ context.Context, c Caller, idem string, req CreateProjectRequest) (Project, error) {
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.note("projects.Create")
	if err := w.allow(c, domain.Scope{TenantID: c.Tenant, ProjectID: "*"}, "project.create"); err != nil {
		return Project{}, err
	}
	kk := string(c.Tenant) + "|" + principalOf(c) + "|" + idem
	if id, ok := w.keys[kk]; ok {
		return w.projects[string(c.Tenant)+"/"+id], nil
	}
	for _, p := range w.projects {
		if p.Key == req.Key {
			return Project{}, Refuse(ErrConflict, "project_key_taken", "the project key is in use")
		}
	}
	w.seq++
	p := Project{ID: domain.ProjectID(fmt.Sprintf("p_%04d", w.seq)), Key: req.Key, DisplayName: req.DisplayName, ActiveWIPLimit: 3, Version: 1}
	w.projects[string(c.Tenant)+"/"+string(p.ID)] = p
	w.keys[kk] = string(p.ID)
	return p, nil
}

func (f fakeProjects) Get(_ context.Context, c Caller, id domain.ProjectID) (Project, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("projects.Get")
	if err := f.w.allow(c, domain.Scope{TenantID: c.Tenant, ProjectID: id}, "project.read"); err != nil {
		return Project{}, err
	}
	p, ok := f.w.projects[string(c.Tenant)+"/"+string(id)]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (f fakeProjects) List(_ context.Context, c Caller, _ *string) (ProjectList, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("projects.List")
	var out ProjectList
	for k, p := range f.w.projects {
		if strings.HasPrefix(k, string(c.Tenant)+"/") && f.w.allow(c, domain.Scope{TenantID: c.Tenant, ProjectID: p.ID}, "project.read") == nil {
			out.Items = append(out.Items, p)
		}
	}
	for i := 0; i < f.w.listExtra && len(out.Items) > 0; i++ {
		out.Items = append(out.Items, out.Items[0])
	}
	return out, nil
}

// ---- Work items ----

type fakeItems struct{ w *world }

func (f fakeItems) Create(_ context.Context, c Caller, scope domain.Scope, idem string, req CreateWorkItemRequest) (WorkItem, error) {
	w := f.w
	if w.gate != nil {
		w.gate() // lets a test hold every concurrent caller before the create
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.note("items.Create")
	w.sawKeys = append(w.sawKeys, idem)
	if err := w.allow(c, scope, "work.create"); err != nil {
		return WorkItem{}, err
	}
	kk := key(scope) + "|" + principalOf(c) + "|" + idem
	if id, ok := w.keys[kk]; ok {
		return w.items[id], nil
	}
	w.seq++
	it := WorkItem{ID: domain.WorkItemID(fmt.Sprintf("wi_%04d", w.seq)), Kind: req.Kind, Title: req.Title, State: "backlog", Version: 1, ContentRevision: 1, CreatedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	w.items[key(scope)+"/"+string(it.ID)] = it
	w.keys[kk] = key(scope) + "/" + string(it.ID)
	return it, nil
}

func (f fakeItems) Get(_ context.Context, c Caller, scope domain.Scope, id domain.WorkItemID) (WorkItem, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("items.Get")
	if err := f.w.allow(c, scope, "work.read"); err != nil {
		return WorkItem{}, err
	}
	it, ok := f.w.items[key(scope)+"/"+string(id)]
	if !ok {
		return WorkItem{}, ErrNotFound
	}
	return it, nil
}

func (f fakeItems) List(_ context.Context, c Caller, scope domain.Scope, q WorkItemQuery) (WorkItemList, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("items.List")
	if err := f.w.allow(c, scope, "work.read"); err != nil {
		return WorkItemList{}, err
	}
	var out WorkItemList
	for k, it := range f.w.items {
		if strings.HasPrefix(k, key(scope)+"/") {
			out.Items = append(out.Items, it)
		}
	}
	for i := 0; i < f.w.listExtra; i++ {
		out.Items = append(out.Items, WorkItem{ID: "wi_extra", Kind: "task", Title: "x", State: "backlog", Version: 1})
	}
	if f.w.listExtra > 0 {
		for len(out.Items) <= q.Limit {
			out.Items = append(out.Items, WorkItem{ID: "wi_pad", Kind: "task", Title: "x", State: "backlog", Version: 1})
		}
	}
	return out, nil
}

// ---- Provisioning ----

type fakeProv struct{ w *world }

// Start follows the Provisioning contract: an existing run is compared on the
// whole declaration and replayed before the budget; otherwise the count and the
// admission are one step under the world's lock.
func (f fakeProv) Start(_ context.Context, c Caller, scope domain.Scope, req StartProvisioning) (ProvisioningRun, error) {
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.note("prov.Start")
	w.startReq = append(w.startReq, req)
	if err := w.allow(c, scope, "run.manage"); err != nil {
		return ProvisioningRun{}, err
	}
	rk := key(scope) + "/" + string(req.RunID)
	if r, ok := w.runs[rk]; ok {
		if w.runBy[rk] != principalOf(c) {
			return ProvisioningRun{}, ErrNotFound // another creator's run: not disclosed, never touched
		}
		if w.runDecl[rk] != declOf(req) {
			return ProvisioningRun{}, ErrIdempotencyConflict
		}
		return r, nil
	}
	active := 0
	for k, r := range w.runs {
		if strings.HasPrefix(k, key(scope)+"/") && !terminal(r.Status) { // project-wide
			active++
		}
	}
	if active >= req.MaxActive {
		return ProvisioningRun{}, ErrBudget
	}
	r := ProvisioningRun{ID: req.RunID, DefinitionID: req.DefinitionID, Status: "queued", Reason: "created"}
	w.runs[rk] = r
	w.runDecl[rk] = declOf(req)
	w.runBy[rk] = principalOf(c)
	return r, nil
}

func terminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

func (f fakeProv) Get(_ context.Context, c Caller, scope domain.Scope, id domain.RunID) (ProvisioningRun, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.note("prov.Get")
	if err := f.w.allow(c, scope, "run.manage"); err != nil {
		return ProvisioningRun{}, err
	}
	r, ok := f.w.runs[key(scope)+"/"+string(id)]
	if !ok {
		return ProvisioningRun{}, ErrNotFound
	}
	return r, nil
}

// ---- fixtures ----

const (
	tenantA domain.TenantID  = "t_acme"
	tenantB domain.TenantID  = "t_other"
	projP   domain.ProjectID = "p_inventory"
	projQ   domain.ProjectID = "p_billing"
)

func personCaller(tenant domain.TenantID, id string) Caller {
	return Caller{Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: id}, Tenant: tenant, RequestID: "req_" + id}
}

func attemptCaller(tenant domain.TenantID, project domain.ProjectID, attempt string) Caller {
	return attemptOfRun(tenant, project, "run_"+attempt, attempt)
}

func attemptOfRun(tenant domain.TenantID, project domain.ProjectID, run, attempt string) Caller {
	att := attempt
	return Caller{Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: attempt, RunID: &run, AttemptID: &att}, Tenant: tenant, Project: project, RunOrigin: OriginProvision}
}

type fixture struct {
	t   *testing.T
	w   *world
	svc *Service
	res *MemoryResults
}

func newFixture(t *testing.T, limits ...Limits) *fixture {
	t.Helper()
	w := newWorld()
	w.addProject(tenantA, projP)
	w.addProject(tenantA, projQ)
	w.addProject(tenantB, "p_foreign")
	res := NewMemoryResults()
	cfg := Config{Repositories: fakeRepos{w}, Projects: fakeProjects{w}, WorkItems: fakeItems{w}, Provisioning: fakeProv{w}, Results: res}
	if len(limits) > 0 {
		cfg.Limits = limits[0]
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, w: w, svc: svc, res: res}
}

func mustCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error code %q, got success", want)
	}
	if code, _ := classify(err); code != want {
		t.Fatalf("want code %q, got %q (%v)", want, code, err)
	}
}

func ptr[T any](v T) *T { return &v }
